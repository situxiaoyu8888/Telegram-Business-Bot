package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
)

// secondMerchant adds another merchant with one physical SKU priced 5 USDT.
func (f shopFixture) secondMerchant(t *testing.T, stock int64) model.ShopSKU {
	m := model.Merchant{Name: "第二商家", Address: "清迈 2 号", Enabled: true}
	must(t, f.db.Create(&m).Error)
	p := model.ShopProduct{MerchantID: m.ID, CategoryID: f.prod.CategoryID, Name: "另一个商品", Kind: "physical", AllowExpress: true, Enabled: true}
	must(t, f.db.Create(&p).Error)
	k := model.ShopSKU{ProductID: p.ID, Name: "默认", Price: "5.000000", Stock: stock, Enabled: true}
	must(t, f.db.Create(&k).Error)
	return k
}

func TestShopMultiMerchantCheckoutIsAllOrNothing(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	other := f.secondMerchant(t, 1)
	b, addr := f.buyer(t, 5001, "100")
	c := model.ShopCoupon{Name: "通用券", Threshold: "0", Discount: "1", ValidDays: 1, PerUserLimit: 1, Claimable: true, Enabled: true}
	must(t, f.db.Create(&c).Error)
	must(t, f.svc.Claim(b.UserID, c.ID))
	var held model.ShopUserCoupon
	must(t, f.db.Where("user_id = ?", b.UserID).First(&held).Error)
	must(t, f.svc.SetCart(b.UserID, f.sku.ID, 1, true))
	must(t, f.svc.SetCart(b.UserID, other.ID, 1, true))

	groups := func(otherQty int64, secondCoupon uint64) []shop.CheckoutGroup {
		return []shop.CheckoutGroup{
			{Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryPickup, UserCouponID: held.ID},
			{Items: []shop.CheckoutItem{{SKUID: other.ID, Quantity: otherQty}}, DeliveryType: shop.DeliveryExpress, UserCouponID: secondCoupon},
		}
	}
	// The second group lacks stock: nothing is charged and the first group's stock is untouched.
	if _, err := f.svc.CreateOrders(b, shop.CheckoutRequest{RequestID: "multi-fail", AddressID: addr, Groups: groups(2, 0)}); err == nil || !strings.Contains(err.Error(), "第 2 个商家") {
		t.Fatal(err)
	}
	// One coupon cannot pay for two orders.
	if _, err := f.svc.CreateOrders(b, shop.CheckoutRequest{RequestID: "multi-coupon", AddressID: addr, Groups: groups(1, held.ID)}); err == nil {
		t.Fatal("coupon used twice")
	}
	var count int64
	f.db.Model(&model.ShopOrder{}).Count(&count)
	if count != 0 || f.balance(t, b.UserID) != "100" || f.stock(t) != 5 {
		t.Fatalf("partial checkout committed: orders=%d balance=%s stock=%d", count, f.balance(t, b.UserID), f.stock(t))
	}

	req := shop.CheckoutRequest{RequestID: "multi-ok", AddressID: addr, FromCart: true, Groups: groups(1, 0)}
	orders, err := f.svc.CreateOrders(b, req)
	must(t, err)
	again, err := f.svc.CreateOrders(b, req)
	must(t, err)
	if len(orders) != 2 || len(again) != 2 || again[0].ID != orders[0].ID || orders[0].PayAmount != "9.000000" || orders[1].PayAmount != "5.000000" {
		t.Fatalf("orders=%+v again=%+v", orders, again)
	}
	if orders[0].MerchantID == orders[1].MerchantID || f.balance(t, b.UserID) != "86" {
		t.Fatal("split or charge wrong", f.balance(t, b.UserID))
	}
	if lines, _ := f.svc.Cart(b.UserID); len(lines) != 0 {
		t.Fatal("cart not cleared")
	}
}

func TestShopAfterSaleRefundsArePartialAndBounded(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 5101, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "as", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 3}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	if _, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: o.ID, Type: shop.AfterSaleRefund, Reason: "未发货", Amount: "5"}); err == nil {
		t.Fatal("after-sale on an unshipped order")
	}
	must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", "SF1", "", "admin"))

	if _, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: o.ID, Type: shop.AfterSaleRefund, Reason: "坏了", Amount: "31"}); err == nil {
		t.Fatal("refund above the paid amount")
	}
	a, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: o.ID, Type: shop.AfterSaleRefund, Reason: "一件坏了", Amount: "10"})
	must(t, err)
	if _, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: o.ID, Type: shop.AfterSaleRefund, Reason: "again", Amount: "1"}); err == nil {
		t.Fatal("two open requests")
	}
	if err := f.svc.CancelByAdmin(o.ID, shop.Scope{}, "x", "admin"); err == nil {
		t.Fatal("order canceled while an after-sale is open")
	}
	// Another merchant's account cannot touch it.
	if err := f.svc.RefundAfterSale(a.ID, shop.Scope{MerchantID: f.mer.ID + 99}, "8", "", "m"); err == nil {
		t.Fatal("foreign merchant refunded")
	}
	must(t, f.svc.RefundAfterSale(a.ID, shop.Scope{MerchantID: f.mer.ID}, "8", "部分退款", "m"))
	if f.balance(t, b.UserID) != "78" {
		t.Fatal(f.balance(t, b.UserID))
	}
	if err := f.svc.RefundAfterSale(a.ID, shop.Scope{}, "1", "", "admin"); err == nil {
		t.Fatal("refunded twice")
	}

	// Return flow on the remaining 22 USDT: approve, buyer ships back, refund.
	r, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: o.ID, Type: shop.AfterSaleReturn, Reason: "不想要了", Amount: "22"})
	must(t, err)
	if err := f.svc.RefundAfterSale(r.ID, shop.Scope{}, "22", "", "admin"); err == nil {
		t.Fatal("return refunded before approval")
	}
	must(t, f.svc.ApproveReturn(r.ID, shop.Scope{}, "", "", "admin"))
	must(t, f.svc.SubmitReturn(b.UserID, r.ID, "顺丰", "SF-BACK"))
	must(t, f.svc.RefundAfterSale(r.ID, shop.Scope{}, "22", "", "admin"))
	var stored model.ShopOrder
	must(t, f.db.First(&stored, o.ID).Error)
	if f.balance(t, b.UserID) != "100" || stored.RefundedAmount != "30.000000" || stored.AfterSaleOpen {
		t.Fatalf("balance=%s refunded=%s open=%v", f.balance(t, b.UserID), stored.RefundedAmount, stored.AfterSaleOpen)
	}
	if _, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: o.ID, Type: shop.AfterSaleRefund, Reason: "more", Amount: "1"}); err == nil {
		t.Fatal("after-sale on a fully refunded order")
	}
	var ret model.ShopAfterSale
	must(t, f.db.First(&ret, r.ID).Error)
	if ret.ReturnAddress != "曼谷 1 号" || ret.ReturnTracking != "SF-BACK" {
		t.Fatalf("%+v", ret)
	}
}

func TestShopCancelAfterPartialRefundReturnsOnlyTheRest(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 5201, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "cr", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", "SF2", "", "admin"))
	a, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: o.ID, Type: shop.AfterSaleRefund, Reason: "少发", Amount: "10"})
	must(t, err)
	must(t, f.svc.RefundAfterSale(a.ID, shop.Scope{}, "10", "", "admin"))
	must(t, f.svc.CancelByAdmin(o.ID, shop.Scope{}, "拦截退回", "admin"))
	if f.balance(t, b.UserID) != "100" || f.stock(t) != 5 {
		t.Fatalf("balance=%s stock=%d", f.balance(t, b.UserID), f.stock(t))
	}
}

func TestShopAutoCompleteSkipsOpenAfterSales(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 5301, "100")
	place := func(id string) *model.ShopOrder {
		o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: id, Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
		must(t, err)
		must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", id, "", "admin"))
		return o
	}
	old, disputed, recent := place("old"), place("disputed"), place("recent")
	_, err := f.svc.ApplyAfterSale(b.UserID, shop.AfterSaleRequest{OrderID: disputed.ID, Type: shop.AfterSaleRefund, Reason: "坏了", Amount: "1"})
	must(t, err)
	weekAgo := time.Now().AddDate(0, 0, -8)
	must(t, f.db.Model(&model.ShopOrder{}).Where("id IN ?", []uint64{old.ID, disputed.ID}).Update("shipped_at", weekAgo).Error)
	must(t, f.svc.AutoComplete(context.Background(), time.Now()))
	status := func(id uint64) string {
		var o model.ShopOrder
		must(t, f.db.First(&o, id).Error)
		return o.Status
	}
	if status(old.ID) != shop.StatusCompleted || status(disputed.ID) != shop.StatusShipped || status(recent.ID) != shop.StatusShipped {
		t.Fatal(status(old.ID), status(disputed.ID), status(recent.ID))
	}
}

func TestShopMerchantScopeIsolatesProductsAndOrders(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	other := f.secondMerchant(t, 5)
	var otherProduct model.ShopProduct
	must(t, f.db.First(&otherProduct, other.ProductID).Error)
	scope := f.mer.ID

	// Editing another merchant's product is refused; a new product is pinned to the account's merchant.
	edit := otherProduct
	edit.Name = "改名"
	if _, err := f.svc.SaveProduct(edit, []shop.SKUInput{{Name: "默认", Price: "1", Stock: 1}}, scope); err == nil {
		t.Fatal("edited another merchant's product")
	}
	id, err := f.svc.SaveProduct(model.ShopProduct{MerchantID: otherProduct.MerchantID, CategoryID: f.prod.CategoryID, Name: "新品", Kind: "physical", AllowExpress: true, Enabled: true}, []shop.SKUInput{{Name: "默认", Price: "2", Stock: 3}}, scope)
	must(t, err)
	var created model.ShopProduct
	must(t, f.db.First(&created, id).Error)
	if created.MerchantID != scope {
		t.Fatal("product not pinned to the merchant", created.MerchantID)
	}
	products, err := f.svc.AdminProducts(scope)
	must(t, err)
	for _, p := range products {
		if p.MerchantID != scope {
			t.Fatal("foreign product listed")
		}
	}

	b, addr := f.buyer(t, 5401, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "scoped", Items: []shop.CheckoutItem{{SKUID: other.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	if err := f.svc.Fulfil(o.ID, shop.Scope{MerchantID: scope}, "顺丰", "X", "", "m"); err == nil {
		t.Fatal("shipped another merchant's order")
	}
	if err := f.svc.CancelByAdmin(o.ID, shop.Scope{MerchantID: scope}, "x", "m"); err == nil {
		t.Fatal("refunded another merchant's order")
	}
	list, err := f.svc.AdminOrders(shop.AdminOrderQuery{MerchantID: scope})
	must(t, err)
	if len(list) != 0 {
		t.Fatal("foreign order listed")
	}
}

type capturedNotice struct {
	chats []int64
	html  string
}

type fakeNotifier struct{ staff chan capturedNotice }

func (fakeNotifier) Notify(context.Context, uint64, int64, string, uint64) error { return nil }
func (n fakeNotifier) NotifyStaff(_ context.Context, ids []int64, html string) error {
	n.staff <- capturedNotice{ids, html}
	return nil
}

func TestShopNewOrderNotifiesAdminsAndMerchantContact(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	must(t, f.db.Model(&model.Merchant{}).Where("id = ?", f.mer.ID).Update("notify_chat_id", 777).Error)
	n := fakeNotifier{staff: make(chan capturedNotice, 4)}
	svc := shop.New(&store.Store{DB: f.db}, f.w, n, nil).WithStaff([]int64{111, 777})
	b, addr := f.buyer(t, 5501, "100")
	_, err := svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "notify", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	select {
	case got := <-n.staff:
		if len(got.chats) != 2 || got.chats[0] != 111 || got.chats[1] != 777 || !strings.Contains(got.html, "商城新订单") {
			t.Fatalf("%+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no staff notification")
	}
}
