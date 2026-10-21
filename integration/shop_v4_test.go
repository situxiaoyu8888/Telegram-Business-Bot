package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
)

func (f shopFixture) startPromo(t *testing.T, promoStock *int64) {
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	must(t, f.db.Model(&model.ShopProduct{}).Where("id = ?", f.prod.ID).Updates(map[string]any{"promo_starts_at": start, "promo_ends_at": end}).Error)
	must(t, f.db.Model(&model.ShopSKU{}).Where("id = ?", f.sku.ID).Updates(map[string]any{"promo_price": "8", "promo_stock": promoStock}).Error)
}

func (f shopFixture) promoStock(t *testing.T) int64 {
	var k model.ShopSKU
	must(t, f.db.First(&k, f.sku.ID).Error)
	return *k.PromoStock
}

func TestShopPromoStockSellsOutWithoutOverselling(t *testing.T) {
	f := newShopFixture(t, 50, "physical")
	two := int64(2)
	f.startPromo(t, &two)
	b, addr := f.buyer(t, 7001, "100")
	req := func(id string, qty int64) shop.CheckoutRequest {
		return shop.CheckoutRequest{RequestID: id, Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: qty}}, DeliveryType: shop.DeliveryExpress, AddressID: addr}
	}
	if _, err := f.svc.CreateOrder(b, req("too-many", 3)); err == nil || !strings.Contains(err.Error(), "活动价限量") {
		t.Fatal(err)
	}
	o, err := f.svc.CreateOrder(b, req("promo", 2))
	must(t, err)
	if o.PayAmount != "16.000000" || f.promoStock(t) != 0 {
		t.Fatal(o.PayAmount, f.promoStock(t))
	}
	// Sold out: the regular price applies again.
	o2, err := f.svc.CreateOrder(b, req("regular", 1))
	must(t, err)
	if o2.PayAmount != "10.000000" {
		t.Fatal(o2.PayAmount)
	}
	// Canceling a promo order returns its promo stock.
	must(t, f.svc.CancelByBuyer(b.UserID, o.ID))
	if f.promoStock(t) != 2 {
		t.Fatal(f.promoStock(t))
	}

	// Many buyers rush the two promo units; promo stock never goes below zero.
	var wg sync.WaitGroup
	var promoOrders int32
	for i := 0; i < 8; i++ {
		buyer, addr := f.buyer(t, int64(7100+i), "50")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			o, err := f.svc.CreateOrder(buyer, shop.CheckoutRequest{RequestID: fmt.Sprintf("rush-%d", i), Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
			if err == nil && o.PayAmount == "8.000000" {
				atomic.AddInt32(&promoOrders, 1)
			}
		}(i)
	}
	wg.Wait()
	var promoItems int64
	f.db.Model(&model.ShopOrderItem{}).Joins("JOIN shop_orders o ON o.id = shop_order_items.order_id").Where("shop_order_items.promo = ? AND o.status <> ?", true, shop.StatusCanceled).Select("COALESCE(SUM(quantity),0)").Scan(&promoItems)
	if promoOrders > 2 || promoItems != int64(promoOrders) || f.promoStock(t) != 2-int64(promoOrders) {
		t.Fatalf("promo orders=%d items=%d left=%d", promoOrders, promoItems, f.promoStock(t))
	}
}

func TestShopPurchaseLimitHoldsUnderConcurrency(t *testing.T) {
	f := newShopFixture(t, 50, "physical")
	must(t, f.db.Model(&model.ShopProduct{}).Where("id = ?", f.prod.ID).Update("limit_per_user", 2).Error)
	b, addr := f.buyer(t, 7201, "100")
	var wg sync.WaitGroup
	var ok int32
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: fmt.Sprintf("lim-%d", i), Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr}); err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}(i)
	}
	wg.Wait()
	if ok != 2 {
		t.Fatal("orders within the limit:", ok)
	}
	_, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "lim-more", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	if err == nil || !strings.Contains(err.Error(), "每人限购 2 件") {
		t.Fatal(err)
	}
	// A canceled order frees its units.
	var first model.ShopOrder
	must(t, f.db.Where("user_id = ?", b.UserID).First(&first).Error)
	must(t, f.svc.CancelByBuyer(b.UserID, first.ID))
	_, err = f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "lim-after-cancel", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
}

func TestShopReferralCommissionWaitsForAfterSaleWindow(t *testing.T) {
	f := newShopFixture(t, 50, "physical")
	must(t, f.db.Create(&model.SystemConfig{Key: "shop_referral_rate", Value: "0.1"}).Error)
	parent, _ := f.buyer(t, 7301, "0")
	child, addr := f.buyer(t, 7302, "100")
	must(t, f.db.Model(&model.TelegramUser{}).Where("id = ?", child.UserID).Update("parent_user_id", parent.UserID).Error)
	loner, laddr := f.buyer(t, 7303, "100")

	place := func(b shop.Buyer, addr uint64, id string, qty int64) *model.ShopOrder {
		o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: id, Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: qty}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
		must(t, err)
		must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", id, "", "admin"))
		must(t, f.svc.ConfirmReceipt(b.UserID, o.ID))
		return o
	}
	place(child, addr, "c1", 3)
	refunded := place(child, addr, "c2", 3)
	a, err := f.svc.ApplyAfterSale(child.UserID, shop.AfterSaleRequest{OrderID: refunded.ID, Type: shop.AfterSaleRefund, Reason: "坏", Amount: "10"})
	must(t, err)
	must(t, f.svc.RefundAfterSale(a.ID, shop.Scope{}, "10", "", "admin"))
	place(loner, laddr, "l1", 1)

	// Still inside the after-sale window: nothing is paid yet.
	must(t, f.svc.GrantCommissions(context.Background(), time.Now()))
	if f.balance(t, parent.UserID) != "0" {
		t.Fatal("paid before the window closed")
	}
	later := time.Now().AddDate(0, 0, 8)
	must(t, f.svc.GrantCommissions(context.Background(), later))
	must(t, f.svc.GrantCommissions(context.Background(), later))
	// 10% of 30 + 10% of (30 − 10) = 5
	if f.balance(t, parent.UserID) != "5" {
		t.Fatal("parent balance", f.balance(t, parent.UserID))
	}
	var rows int64
	f.db.Model(&model.ReferralCommission{}).Where("source_type = ?", "shop_order").Count(&rows)
	var pending int64
	f.db.Model(&model.ShopOrder{}).Where("commission_done = ?", false).Count(&pending)
	if rows != 2 || pending != 0 {
		t.Fatalf("commission rows=%d unprocessed orders=%d", rows, pending)
	}
}

func TestShopFavoritesAndHistory(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, _ := f.buyer(t, 7401, "0")
	must(t, f.svc.SetFavorite(b.UserID, f.prod.ID, true))
	must(t, f.svc.SetFavorite(b.UserID, f.prod.ID, true))
	if !f.svc.IsFavorite(b.UserID, f.prod.ID) {
		t.Fatal("not favorited")
	}
	if err := f.svc.SetFavorite(b.UserID, 99999, true); err == nil {
		t.Fatal("favorited a missing product")
	}
	must(t, f.svc.RecordView(b.UserID, f.prod.ID))
	must(t, f.svc.RecordView(b.UserID, f.prod.ID))
	favs, err := f.svc.Favorites(b.UserID)
	must(t, err)
	hist, err := f.svc.History(b.UserID)
	must(t, err)
	if len(favs) != 1 || len(hist) != 1 {
		t.Fatal(len(favs), len(hist))
	}
	// Products taken off sale drop out of both lists.
	must(t, f.db.Model(&model.ShopProduct{}).Where("id = ?", f.prod.ID).Update("enabled", false).Error)
	favs, _ = f.svc.Favorites(b.UserID)
	hist, _ = f.svc.History(b.UserID)
	if len(favs) != 0 || len(hist) != 0 {
		t.Fatal("disabled product listed")
	}
	must(t, f.svc.SetFavorite(b.UserID, f.prod.ID, false))
	must(t, f.svc.ClearHistory(b.UserID))
}
