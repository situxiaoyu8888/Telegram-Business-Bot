package integration

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
)

func TestShopPromoPriceAppliesOnlyInsideItsWindow(t *testing.T) {
	f := newShopFixture(t, 10, "physical")
	start, end := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	p := f.prod
	if _, err := f.svc.SaveProduct(p, []shop.SKUInput{{Name: "默认", Price: "10", Stock: 10, PromoPrice: "10"}}, 0); err == nil {
		t.Fatal("promo price not below the regular price accepted")
	}
	if _, err := f.svc.SaveProduct(p, []shop.SKUInput{{Name: "默认", Price: "10", Stock: 10, PromoPrice: "8"}}, 0); err == nil {
		t.Fatal("promo price without a window accepted")
	}
	p.PromoStartsAt, p.PromoEndsAt = &start, &end
	_, err := f.svc.SaveProduct(p, []shop.SKUInput{{Name: "默认", Price: "10", Stock: 10, PromoPrice: "8"}}, 0)
	must(t, err)

	cards, err := f.svc.Products(shop.ProductQuery{OnPromo: true})
	must(t, err)
	if len(cards) != 1 || cards[0].MinPrice != "8.000000" || cards[0].ListPrice != "10.000000" || !cards[0].OnPromo {
		t.Fatalf("%+v", cards)
	}
	b, addr := f.buyer(t, 6001, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "promo", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	if o.PayAmount != "16.000000" {
		t.Fatal(o.PayAmount)
	}
	// After the window the regular price is back without any change to the SKU.
	past := time.Now().Add(-time.Minute)
	must(t, f.db.Model(&model.ShopProduct{}).Where("id = ?", f.prod.ID).Update("promo_ends_at", past).Error)
	o, err = f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "regular", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	if o.PayAmount != "10.000000" {
		t.Fatal(o.PayAmount)
	}
}

func TestShopSettlementLedgerFollowsCompletionsAndRefunds(t *testing.T) {
	f := newShopFixture(t, 10, "physical")
	must(t, f.db.Model(&model.Merchant{}).Where("id = ?", f.mer.ID).Update("commission_rate", "0.1").Error)
	b, addr := f.buyer(t, 6101, "200")
	place := func(id string, qty int64) *model.ShopOrder {
		o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: id, Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: qty}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
		must(t, err)
		must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", id, "", "admin"))
		return o
	}
	// Refunded before completion: the sale entry is booked net of it.
	early := place("early", 2)
	a, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: early.ID, Type: shop.AfterSaleRefund, Reason: "少件", Amount: "5"})
	must(t, err)
	must(t, f.svc.RefundAfterSale(a.ID, shop.Scope{}, "5", "", "admin"))
	must(t, f.svc.ConfirmReceipt(b.UserID, early.ID))

	// Refunded after completion: a negative entry reverses part of the sale.
	late := place("late", 3)
	must(t, f.svc.ConfirmReceipt(b.UserID, late.ID))
	a, err = f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: late.ID, Type: shop.AfterSaleRefund, Reason: "质量问题", Amount: "10"})
	must(t, err)
	must(t, f.svc.RefundAfterSale(a.ID, shop.Scope{}, "10", "", "admin"))

	// Canceled orders never reach the ledger.
	canceled := place("canceled", 1)
	must(t, f.svc.CancelByAdmin(canceled.ID, shop.Scope{}, "缺货", "admin"))

	balances, err := f.svc.MerchantBalances(f.mer.ID)
	must(t, err)
	// sales 15 + 30, refund −10 → amount 35; commission 1.5 + 3 − 1 = 3.5; net 31.5
	if len(balances) != 1 || balances[0].Entries != 3 || balances[0].Amount != "35.000000" || balances[0].Commission != "3.500000" || balances[0].Net != "31.500000" {
		t.Fatalf("%+v", balances)
	}
	// Orders are settled only once their after-sale period is over, so a paid-out sale can no
	// longer be refunded.
	if _, err := f.svc.CreateSettlement(f.mer.ID, "", "admin"); err == nil {
		t.Fatal("settled orders still within their after-sale period")
	}
	must(t, f.svc.GrantCommissions(context.Background(), time.Now().AddDate(0, 0, 8)))
	if _, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: early.ID, Type: shop.AfterSaleRefund, Reason: "再退", Amount: "1"}); err == nil {
		t.Fatal("after-sale accepted on a finalised order")
	}
	st, err := f.svc.CreateSettlement(f.mer.ID, "", "admin")
	must(t, err)
	if st.Net != "31.500000" || st.EntryCount != 3 {
		t.Fatalf("%+v", st)
	}
	if _, err := f.svc.CreateSettlement(f.mer.ID, "", "admin"); err == nil {
		t.Fatal("settled the same entries twice")
	}
	if err := f.svc.MarkSettlementPaid(st.ID, "", "", "admin"); err == nil {
		t.Fatal("paid without a reference")
	}
	must(t, f.svc.MarkSettlementPaid(st.ID, "TX-123", "", "admin"))
	if err := f.svc.MarkSettlementPaid(st.ID, "TX-124", "", "admin"); err == nil {
		t.Fatal("paid twice")
	}
	balances, _ = f.svc.MerchantBalances(f.mer.ID)
	if balances[0].Net != "0.000000" || balances[0].SettledNet != "31.500000" {
		t.Fatalf("%+v", balances[0])
	}
}

func TestShopReviewsAreOncePerItemAndRespectHiding(t *testing.T) {
	f := newShopFixture(t, 10, "physical")
	b, addr := f.buyer(t, 6201, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "rv", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	detail, err := f.svc.Order(b.UserID, o.ID)
	must(t, err)
	item := detail.Items[0].ID
	if err := f.svc.Review(b.UserID, o.ID, []shop.ReviewInput{{OrderItemID: item, Rating: 5}}); err == nil {
		t.Fatal("reviewed an unfinished order")
	}
	must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", "1", "", "admin"))
	must(t, f.svc.ConfirmReceipt(b.UserID, o.ID))
	if err := f.svc.Review(b.UserID, o.ID, []shop.ReviewInput{{OrderItemID: item, Rating: 6}}); err == nil {
		t.Fatal("rating out of range")
	}
	must(t, f.svc.Review(b.UserID, o.ID, []shop.ReviewInput{{OrderItemID: item, Rating: 4, Content: "不错"}}))
	if err := f.svc.Review(b.UserID, o.ID, []shop.ReviewInput{{OrderItemID: item, Rating: 1}}); err == nil {
		t.Fatal("reviewed twice")
	}
	other, _ := f.buyer(t, 6202, "0")
	if err := f.svc.Review(other.UserID, o.ID, []shop.ReviewInput{{OrderItemID: item, Rating: 1}}); err == nil {
		t.Fatal("reviewed someone else's order")
	}
	p, err := f.svc.Product(f.prod.ID)
	must(t, err)
	if p.RatingCount != 1 || p.Rating != 4 {
		t.Fatal(p.Rating, p.RatingCount)
	}
	reviews, _ := f.svc.AdminReviews(f.mer.ID)
	if err := f.svc.ReplyReview(reviews[0].ID, shop.Scope{MerchantID: f.mer.ID + 1}, "x"); err == nil {
		t.Fatal("another merchant replied")
	}
	must(t, f.svc.ReplyReview(reviews[0].ID, shop.Scope{MerchantID: f.mer.ID}, "谢谢"))
	must(t, f.svc.SetReviewHidden(reviews[0].ID, true))
	if p, _ = f.svc.Product(f.prod.ID); p.RatingCount != 0 {
		t.Fatal("hidden review still counted")
	}
	if detail, _ = f.svc.Order(b.UserID, o.ID); detail.CanReview {
		t.Fatal("still offered a review")
	}
}

func TestShopReportAndExport(t *testing.T) {
	f := newShopFixture(t, 3, "physical")
	b, addr := f.buyer(t, 6301, "100")
	keep, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "keep", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryExpress, AddressID: addr, Remark: "=cmd()"})
	must(t, err)
	drop, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "drop", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryPickup})
	must(t, err)
	must(t, f.svc.CancelByBuyer(b.UserID, drop.ID))

	today := time.Now()
	from := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	r, err := f.svc.SalesReport(0, from.AddDate(0, 0, -6), from.AddDate(0, 0, 1))
	must(t, err)
	if r.Orders != 1 || r.Canceled != 1 || r.Sales != "20.000000" || len(r.Days) != 7 || len(r.Top) != 1 || r.Top[0].Quantity != 2 {
		t.Fatalf("%+v", r)
	}
	// Stock is 1 after the cancel restored one unit, below the default threshold of 5.
	if len(r.LowStock) != 1 || r.LowStock[0].Stock != 1 {
		t.Fatalf("low stock %+v", r.LowStock)
	}
	if other, _ := f.svc.SalesReport(f.mer.ID+1, from, from.AddDate(0, 0, 1)); other.Orders != 0 || len(other.LowStock) != 0 {
		t.Fatal("another merchant sees this merchant's report")
	}

	var buf bytes.Buffer
	must(t, f.svc.ExportOrders(&buf, 0, "", from, from.AddDate(0, 0, 1)))
	out := buf.String()
	if !strings.HasPrefix(out, "\xEF\xBB\xBF订单号") || !strings.Contains(out, keep.OrderNo) || !strings.Contains(out, "'=cmd()") || strings.Count(out, "\n") != 3 {
		t.Fatalf("csv:\n%s", out)
	}
}

func TestShopHomeShowsOnlyActiveBanners(t *testing.T) {
	f := newShopFixture(t, 3, "physical")
	if err := f.svc.SaveBanner(model.ShopBanner{ImageURL: "http://insecure/x.png", LinkType: "none", Enabled: true}); err == nil {
		t.Fatal("http image accepted")
	}
	if err := f.svc.SaveBanner(model.ShopBanner{ImageURL: "https://img/x.png", LinkType: "product", LinkValue: "999", Enabled: true}); err == nil {
		t.Fatal("link to a missing product accepted")
	}
	past := time.Now().Add(-time.Hour)
	must(t, f.svc.SaveBanner(model.ShopBanner{Title: "当前", ImageURL: "https://img/a.png", LinkType: "product", LinkValue: "1", Enabled: true}))
	must(t, f.svc.SaveBanner(model.ShopBanner{Title: "过期", ImageURL: "https://img/b.png", LinkType: "none", Enabled: true, EndsAt: &past}))
	must(t, f.svc.SaveBanner(model.ShopBanner{Title: "停用", ImageURL: "https://img/c.png", LinkType: "none", Enabled: false}))
	must(t, f.db.Model(&model.ShopProduct{}).Where("id = ?", f.prod.ID).Update("featured", true).Error)
	h, err := f.svc.Home()
	must(t, err)
	if len(h.Banners) != 1 || h.Banners[0].Title != "当前" || len(h.Featured) != 1 || len(h.Promos) != 0 {
		t.Fatalf("%+v", h)
	}
}

func TestPlatformDiscountsAreSettledByBearer(t *testing.T) {
	f := newShopFixture(t, 10, "physical")
	b, addr := f.buyer(t, 6111, "200")
	coupon := func(merchantID uint64) uint64 {
		c := model.ShopCoupon{Name: "减3", MerchantID: merchantID, Threshold: "0", Discount: "3", ValidDays: 7, PerUserLimit: 1, Claimable: true, Enabled: true}
		must(t, f.db.Create(&c).Error)
		must(t, f.svc.Claim(b.UserID, c.ID))
		var held model.ShopUserCoupon
		must(t, f.db.Where("user_id = ? AND coupon_id = ?", b.UserID, c.ID).First(&held).Error)
		return held.ID
	}
	bearer := func(v string) {
		must(t, f.db.Where("`key` = ?", "shop_discount_bearer").Delete(&model.SystemConfig{}).Error)
		must(t, f.db.Create(&model.SystemConfig{Key: "shop_discount_bearer", Value: v}).Error)
	}
	place := func(id string, held uint64) *model.ShopOrder {
		o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: id, Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryExpress, AddressID: addr, UserCouponID: held})
		must(t, err)
		must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", id, "", "admin"))
		must(t, f.svc.ConfirmReceipt(b.UserID, o.ID))
		return o
	}
	sale := func(o *model.ShopOrder) string {
		var e model.ShopMerchantEntry
		must(t, f.db.Where("entry_key = ?", fmt.Sprintf("sale:%d", o.ID)).First(&e).Error)
		return e.Amount
	}

	// Platform bears: a platform coupon is paid to the merchant, the merchant's own is not.
	bearer("platform")
	platform := place("platform", coupon(0))
	if platform.PayAmount != "17.000000" || platform.PlatformSubsidy != "3.000000" || sale(platform) != "20.000000" {
		t.Fatalf("platform-borne coupon: pay %s subsidy %s sale %s", platform.PayAmount, platform.PlatformSubsidy, sale(platform))
	}
	own := place("own", coupon(f.mer.ID))
	if own.PlatformSubsidy != "0.000000" || sale(own) != "17.000000" {
		t.Fatalf("merchant coupon: subsidy %s sale %s", own.PlatformSubsidy, sale(own))
	}
	// A refund of half the payment takes back half of the booked sale, subsidy included.
	a, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: platform.ID, Type: shop.AfterSaleRefund, Reason: "少件", Amount: "8.5"})
	must(t, err)
	must(t, f.svc.RefundAfterSale(a.ID, shop.Scope{}, "8.5", "", "admin"))
	var refund model.ShopMerchantEntry
	must(t, f.db.Where("entry_key = ?", fmt.Sprintf("refund:%d", a.ID)).First(&refund).Error)
	if refund.Amount != "-10.000000" {
		t.Fatal("refund entry", refund.Amount)
	}

	// Merchant bears (the default): settled on what the buyer paid.
	bearer("merchant")
	merchant := place("merchant", coupon(0))
	if merchant.PlatformSubsidy != "0.000000" || sale(merchant) != "17.000000" {
		t.Fatalf("merchant-borne coupon: subsidy %s sale %s", merchant.PlatformSubsidy, sale(merchant))
	}
}
