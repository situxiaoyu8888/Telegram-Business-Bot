package integration

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

type shopFixture struct {
	db   *gorm.DB
	w    *wallet.Service
	svc  *shop.Service
	sku  model.ShopSKU
	prod model.ShopProduct
	mer  model.Merchant
}

func newShopFixture(t *testing.T, stock int64, kind string) shopFixture {
	db := testDatabase(t)
	st := &store.Store{DB: db}
	w := wallet.New(st, "USDT", "1", "0")
	f := shopFixture{db: db, w: w, svc: shop.New(st, w, nil, nil)}
	f.mer = model.Merchant{Name: "测试商家", Address: "曼谷 1 号", Enabled: true}
	must(t, db.Create(&f.mer).Error)
	cat := model.ShopCategory{Name: "分类", Enabled: true}
	must(t, db.Create(&cat).Error)
	f.prod = model.ShopProduct{MerchantID: f.mer.ID, CategoryID: cat.ID, Name: "商品", Kind: kind, AllowExpress: kind == "physical", AllowPickup: kind == "physical", Enabled: true}
	must(t, db.Create(&f.prod).Error)
	f.sku = model.ShopSKU{ProductID: f.prod.ID, Name: "默认", Price: "10.000000", Stock: stock, Enabled: true}
	must(t, db.Create(&f.sku).Error)
	return f
}

// buyer creates a user with the given wallet balance and a default address.
func (f shopFixture) buyer(t *testing.T, tgID int64, balance string) (shop.Buyer, uint64) {
	u := model.TelegramUser{TelegramUserID: tgID, ReferralCode: fmt.Sprintf("shop%d", tgID), Status: 1}
	must(t, f.db.Create(&u).Error)
	if balance != "0" {
		must(t, f.w.CreditDeposit(model.DepositOrder{UserID: u.ID, Network: "TRC20", Address: "addr", Amount: balance, TxID: fmt.Sprintf("tx-%d", tgID)}))
	}
	addrID, err := f.svc.SaveAddress(u.ID, model.ShopAddress{Name: "张三", Phone: "123", Address: "某路 1 号"})
	must(t, err)
	return shop.Buyer{UserID: u.ID, TelegramUserID: tgID}, addrID
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f shopFixture) balance(t *testing.T, userID uint64) string {
	av, _, err := f.w.Balance(userID)
	must(t, err)
	return av.String()
}

func (f shopFixture) stock(t *testing.T) int64 {
	var k model.ShopSKU
	must(t, f.db.First(&k, f.sku.ID).Error)
	return k.Stock
}

func TestShopOrderIsChargedOnceAndStockNeverOversells(t *testing.T) {
	f := newShopFixture(t, 3, "physical")
	b, addr := f.buyer(t, 2001, "100")
	req := shop.CheckoutRequest{RequestID: "same-tap", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = f.svc.CreateOrder(b, req) }()
	}
	wg.Wait()
	var orders int64
	f.db.Model(&model.ShopOrder{}).Count(&orders)
	if orders != 1 || f.balance(t, b.UserID) != "90" || f.stock(t) != 2 {
		t.Fatalf("repeated request: orders=%d balance=%s stock=%d", orders, f.balance(t, b.UserID), f.stock(t))
	}

	// Ten buyers race for the remaining two units.
	var ok int32
	for i := 0; i < 10; i++ {
		buyer, addr := f.buyer(t, int64(3000+i), "50")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := shop.CheckoutRequest{RequestID: fmt.Sprintf("race-%d", i), Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryPickup, AddressID: addr}
			if _, err := f.svc.CreateOrder(buyer, r); err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}(i)
	}
	wg.Wait()
	var charged int64
	f.db.Model(&model.WalletTransaction{}).Where("type = ?", "shop_pay").Count(&charged)
	if ok != 2 || f.stock(t) != 0 || charged != 3 {
		t.Fatalf("race: succeeded=%d stock=%d charges=%d", ok, f.stock(t), charged)
	}
}

func TestShopInsufficientBalanceRollsBack(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 2101, "5")
	_, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "poor", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	if err == nil || !strings.Contains(err.Error(), "余额不足") {
		t.Fatal(err)
	}
	var orders int64
	f.db.Model(&model.ShopOrder{}).Count(&orders)
	if orders != 0 || f.stock(t) != 5 || f.balance(t, b.UserID) != "5" {
		t.Fatalf("not rolled back: orders=%d stock=%d balance=%s", orders, f.stock(t), f.balance(t, b.UserID))
	}
}

func TestShopCouponDiscountAndCancelRefund(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 2201, "100")
	c := model.ShopCoupon{Name: "满20减3", Threshold: "20", Discount: "3", ValidDays: 7, PerUserLimit: 1, Claimable: true, Enabled: true}
	must(t, f.db.Create(&c).Error)
	must(t, f.svc.Claim(b.UserID, c.ID))
	if err := f.svc.Claim(b.UserID, c.ID); err == nil {
		t.Fatal("per-user limit ignored")
	}
	var held model.ShopUserCoupon
	must(t, f.db.Where("user_id = ?", b.UserID).First(&held).Error)

	req := shop.CheckoutRequest{RequestID: "with-coupon", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr, UserCouponID: held.ID}
	if _, err := f.svc.CreateOrder(b, req); err == nil || !strings.Contains(err.Error(), "门槛") {
		t.Fatal("coupon accepted below threshold:", err)
	}
	req.Items[0].Quantity = 2
	o, err := f.svc.CreateOrder(b, req)
	must(t, err)
	if o.PayAmount != "17.000000" || f.balance(t, b.UserID) != "83" || f.stock(t) != 3 {
		t.Fatalf("pay=%s balance=%s stock=%d", o.PayAmount, f.balance(t, b.UserID), f.stock(t))
	}
	must(t, f.db.First(&held, held.ID).Error)
	if held.Status != shop.CouponUsed || held.OrderID != o.ID {
		t.Fatal("coupon not consumed", held)
	}

	must(t, f.svc.CancelByBuyer(b.UserID, o.ID))
	if err := f.svc.CancelByBuyer(b.UserID, o.ID); err == nil {
		t.Fatal("canceled twice")
	}
	must(t, f.db.First(&held, held.ID).Error)
	var p model.ShopProduct
	must(t, f.db.First(&p, f.prod.ID).Error)
	if f.balance(t, b.UserID) != "100" || f.stock(t) != 5 || held.Status != shop.CouponUnused || p.Sales != 0 {
		t.Fatalf("refund: balance=%s stock=%d coupon=%s sales=%d", f.balance(t, b.UserID), f.stock(t), held.Status, p.Sales)
	}
}

func TestShopBuyerCannotCancelShippedOrder(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 2301, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "ship", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", "SF123", "", "admin"))
	if err := f.svc.CancelByBuyer(b.UserID, o.ID); err == nil {
		t.Fatal("buyer canceled a shipped order")
	}
	must(t, f.svc.ConfirmReceipt(b.UserID, o.ID))
	if err := f.svc.CancelByAdmin(o.ID, shop.Scope{}, "test", "admin"); err == nil {
		t.Fatal("completed order refunded")
	}
	if f.balance(t, b.UserID) != "90" {
		t.Fatal(f.balance(t, b.UserID))
	}
}

func TestShopVirtualCodesAreDeliveredOnPayment(t *testing.T) {
	f := newShopFixture(t, 0, "virtual")
	n, err := f.svc.ImportCodes(f.sku.ID, "CODE-1\nCODE-2\n\n", 0)
	must(t, err)
	if n != 2 || f.stock(t) != 2 {
		t.Fatal(n, f.stock(t))
	}
	b, _ := f.buyer(t, 2401, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "v1", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryVirtual})
	must(t, err)
	if o.Status != shop.StatusCompleted || o.DeliveryNote != "CODE-1\nCODE-2" {
		t.Fatalf("status=%s note=%q", o.Status, o.DeliveryNote)
	}
	if _, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "v2", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress}); err == nil {
		t.Fatal("virtual product shipped by express")
	}
}

func TestShopCouponTotalQuantityUnderConcurrency(t *testing.T) {
	f := newShopFixture(t, 1, "physical")
	c := model.ShopCoupon{Name: "限量", Threshold: "0", Discount: "1", ValidDays: 1, TotalQty: 2, PerUserLimit: 1, Claimable: true, Enabled: true}
	must(t, f.db.Create(&c).Error)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		b, _ := f.buyer(t, int64(2500+i), "0")
		wg.Add(1)
		go func() { defer wg.Done(); _ = f.svc.Claim(b.UserID, c.ID) }()
	}
	wg.Wait()
	var held int64
	f.db.Model(&model.ShopUserCoupon{}).Count(&held)
	must(t, f.db.First(&c, c.ID).Error)
	if held != 2 || c.IssuedQty != 2 {
		t.Fatalf("held=%d issued=%d", held, c.IssuedQty)
	}
}

func TestShopCartAndAdminCodeCounts(t *testing.T) {
	f := newShopFixture(t, 0, "virtual")
	_, err := f.svc.ImportCodes(f.sku.ID, "A\nB\nC", 0)
	must(t, err)
	b, _ := f.buyer(t, 2601, "100")
	must(t, f.svc.SetCart(b.UserID, f.sku.ID, 2, true))
	must(t, f.svc.SetCart(b.UserID, f.sku.ID, 1, true))
	lines, err := f.svc.Cart(b.UserID)
	must(t, err)
	if len(lines) != 1 || lines[0].Quantity != 3 || !lines[0].Available {
		t.Fatalf("%+v", lines)
	}
	_, err = f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "cart", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryVirtual, FromCart: true})
	must(t, err)
	if lines, _ = f.svc.Cart(b.UserID); len(lines) != 0 {
		t.Fatal("cart not cleared after checkout")
	}
	products, err := f.svc.AdminProducts(0)
	must(t, err)
	if len(products) != 1 || products[0].Codes[f.sku.ID] != 2 {
		t.Fatalf("unused codes: %+v", products[0].Codes)
	}
	cards, err := f.svc.Products(shop.ProductQuery{Keyword: "商"})
	must(t, err)
	if len(cards) != 1 || cards[0].MinPrice != "10.000000" || !cards[0].InStock {
		t.Fatalf("%+v", cards)
	}
}
