package integration

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
)

func TestShopProductExportImportRoundTrip(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	var buf bytes.Buffer
	must(t, f.svc.ExportProducts(&buf, 0))
	csvText := buf.String()
	if !strings.HasPrefix(csvText, "\xEF\xBB\xBF商品ID,商家,分类,商品名称") || !strings.Contains(csvText, "测试商家,分类,商品,实物") {
		t.Fatalf("export:\n%s", csvText)
	}
	lines := strings.Split(strings.TrimSpace(strings.TrimPrefix(csvText, "\xEF\xBB\xBF")), "\n")
	// Raise the price, add a second SKU to the product and a brand-new product.
	edited := strings.Replace(lines[1], ",默认,10,5,", ",默认,12.5,8,", 1)
	header := lines[0]
	extraSKU := strings.Replace(edited, ",默认,12.5,8,", ",大份,15,3,", 1)
	newProduct := ",测试商家,分类,新品,实物,快递,标准,7,20,,,0,是,否,0,,"
	file := strings.Join([]string{header, edited, extraSKU, newProduct}, "\n")

	res, err := f.svc.ImportProducts([]byte(file), 0, false)
	must(t, err)
	if res.Updates != 1 || res.Creates != 1 || res.Errors != 0 || res.Applied {
		t.Fatalf("preview %+v", res)
	}
	var k model.ShopSKU
	must(t, f.db.First(&k, f.sku.ID).Error)
	if k.Price != "10.000000" {
		t.Fatal("preview changed data")
	}
	res, err = f.svc.ImportProducts([]byte(file), 0, true)
	must(t, err)
	if !res.Applied {
		t.Fatalf("%+v", res)
	}
	must(t, f.db.First(&k, f.sku.ID).Error)
	var skus, products int64
	f.db.Model(&model.ShopSKU{}).Where("product_id = ? AND enabled = ?", f.prod.ID, true).Count(&skus)
	f.db.Model(&model.ShopProduct{}).Count(&products)
	if k.Price != "12.500000" || k.Stock != 8 || skus != 2 || products != 2 {
		t.Fatalf("price=%s stock=%d skus=%d products=%d", k.Price, k.Stock, skus, products)
	}
}

func TestShopProductImportRejectsBadRowsAsAWhole(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	file := strings.Join([]string{
		"商品ID,商家,分类,商品名称,类型,配送方式,规格,价格,库存",
		",测试商家,分类,好商品,实物,快递,标准,5,10",
		",测试商家,不存在的分类,坏商品,实物,快递,标准,5,10",
		",测试商家,分类,没库存数,实物,快递,标准,5,很多",
	}, "\n")
	res, err := f.svc.ImportProducts([]byte(file), 0, true)
	must(t, err)
	if res.Applied || res.Errors != 2 || !strings.Contains(res.Rows[1].Error, "第 3 行") || !strings.Contains(res.Rows[1].Error, "分类") || !strings.Contains(res.Rows[2].Error, "库存") {
		t.Fatalf("%+v", res)
	}
	var n int64
	f.db.Model(&model.ShopProduct{}).Count(&n)
	if n != 1 {
		t.Fatal("bad import saved products", n)
	}
	if _, err := f.svc.ImportProducts([]byte("商品名称\n x"), 0, false); err == nil {
		t.Fatal("missing columns accepted")
	}
}

func TestShopProductImportAcceptsGBKAndRespectsMerchantScope(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	other := f.secondMerchant(t, 5)
	utf := strings.Join([]string{"商品ID,分类,商品名称,配送方式,规格,价格,库存", ",分类,中文名商品,自提,份,3,4"}, "\n")
	gbk, err := simplifiedchinese.GBK.NewEncoder().String(utf)
	must(t, err)
	res, err := f.svc.ImportProducts([]byte(gbk), f.mer.ID, true)
	must(t, err)
	var p model.ShopProduct
	must(t, f.db.Where("name = ?", "中文名商品").First(&p).Error)
	if !res.Applied || p.MerchantID != f.mer.ID || !p.AllowPickup || p.AllowExpress {
		t.Fatalf("%+v %+v", res, p)
	}
	// A merchant account cannot update another merchant's product by ID.
	foreign := strings.Join([]string{"商品ID,分类,商品名称,规格,价格,库存", strconv.FormatUint(other.ProductID, 10) + ",分类,改名,默认,1,1"}, "\n")
	res, err = f.svc.ImportProducts([]byte(foreign), f.mer.ID, true)
	must(t, err)
	if res.Applied || res.Errors != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestShopMerchantPayoutRequest(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 9301, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "payout", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", "1", "", "admin"))
	must(t, f.svc.ConfirmReceipt(b.UserID, o.ID))
	const tron = "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"
	if _, err := f.svc.RequestPayout(f.mer.ID, "not-an-address", "", "shop1"); err == nil {
		t.Fatal("invalid address accepted")
	}
	st, err := f.svc.RequestPayout(f.mer.ID, tron, "月结", "shop1")
	must(t, err)
	if st.Net != "20.000000" || st.PayoutAddress != tron || st.RequestedBy != "shop1" || st.Status != shop.SettlementPending {
		t.Fatalf("%+v", st)
	}
	var m model.Merchant
	must(t, f.db.First(&m, f.mer.ID).Error)
	if m.PayoutAddress != tron {
		t.Fatal("address not remembered")
	}
	if _, err := f.svc.RequestPayout(f.mer.ID, tron, "", "shop1"); err == nil {
		t.Fatal("paid out an empty balance")
	}
	if err := f.svc.SetPayoutAddress(st.ID, f.mer.ID+1, tron, "x"); err == nil {
		t.Fatal("another merchant changed the address")
	}
	must(t, f.svc.MarkSettlementPaid(st.ID, "0xabc", "", "admin"))
	if err := f.svc.SetPayoutAddress(st.ID, f.mer.ID, tron, "shop1"); err == nil {
		t.Fatal("address changed after payment")
	}
}
