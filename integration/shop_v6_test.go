package integration

import (
	"testing"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
)

func TestShopMerchantApplicationsOnePendingAtATime(t *testing.T) {
	f := newShopFixture(t, 1, "physical")
	b, _ := f.buyer(t, 9101, "0")
	in := shop.ApplicationInput{ShopName: "阿明水果店", Contact: "@aming", Category: "水果"}
	a, err := f.svc.ApplyMerchant(b, in)
	must(t, err)
	if a.Status != shop.ApplicationPending {
		t.Fatal(a.Status)
	}
	if _, err := f.svc.ApplyMerchant(b, in); err == nil {
		t.Fatal("second pending application accepted")
	}
	if _, err := f.svc.ApplyMerchant(b, shop.ApplicationInput{ShopName: "", Contact: "x"}); err == nil {
		t.Fatal("missing shop name accepted")
	}
	must(t, f.db.Model(&model.ShopMerchantApplication{}).Where("id = ?", a.ID).Update("status", shop.ApplicationRejected).Error)
	_, err = f.svc.ApplyMerchant(b, in)
	must(t, err)
	mine, err := f.svc.MyApplication(b.UserID)
	must(t, err)
	if mine == nil || mine.ID == a.ID || mine.Status != shop.ApplicationPending {
		t.Fatalf("%+v", mine)
	}
}

func TestShopBuyerMerchantMessages(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	other := f.secondMerchant(t, 5)
	var otherProduct model.ShopProduct
	must(t, f.db.First(&otherProduct, other.ProductID).Error)
	b, addr := f.buyer(t, 9201, "100")
	foreign, _ := f.buyer(t, 9202, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "msg", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)

	msg, err := f.svc.SendToMerchant(b, "小明", f.mer.ID, shop.MessageInput{Content: "  什么时候发货？ ", OrderID: o.ID})
	must(t, err)
	if msg.Content != "什么时候发货？" || msg.OrderID != o.ID {
		t.Fatalf("%+v", msg)
	}
	// Someone else's order reference is dropped rather than exposed to the merchant.
	m2, err := f.svc.SendToMerchant(foreign, "路人", f.mer.ID, shop.MessageInput{Content: "我的单呢", OrderID: o.ID})
	must(t, err)
	if m2.OrderID != 0 {
		t.Fatal("foreign order attached")
	}
	if _, err := f.svc.SendToMerchant(b, "小明", f.mer.ID, shop.MessageInput{Content: "  "}); err == nil {
		t.Fatal("empty message accepted")
	}

	convs, err := f.svc.AdminConversations(f.mer.ID)
	must(t, err)
	if len(convs) != 2 {
		t.Fatal("conversations", len(convs))
	}
	var mine shop.ConversationRow
	for _, c := range convs {
		if c.UserID == b.UserID {
			mine = c
		}
	}
	if mine.MerchantUnread != 1 || mine.LastMessage != "什么时候发货？" {
		t.Fatalf("%+v", mine)
	}
	if others, _ := f.svc.AdminConversations(otherProduct.MerchantID); len(others) != 0 {
		t.Fatal("another merchant sees the threads")
	}
	if _, err := f.svc.AdminThread(mine.ID, shop.Scope{MerchantID: otherProduct.MerchantID}); err == nil {
		t.Fatal("another merchant opened the thread")
	}
	if err := f.svc.ReplyToBuyer(mine.ID, shop.Scope{MerchantID: otherProduct.MerchantID}, "hi", "x"); err == nil {
		t.Fatal("another merchant replied")
	}
	thread, err := f.svc.AdminThread(mine.ID, shop.Scope{MerchantID: f.mer.ID})
	must(t, err)
	if len(thread.Messages) != 1 || thread.Conversation.MerchantUnread != 0 {
		t.Fatalf("%+v", thread)
	}
	must(t, f.svc.ReplyToBuyer(mine.ID, shop.Scope{MerchantID: f.mer.ID}, "今天下午发", "shop1"))
	list, _ := f.svc.BuyerConversations(b.UserID)
	if len(list) != 1 || list[0].BuyerUnread != 1 || list[0].MerchantName != "测试商家" {
		t.Fatalf("%+v", list)
	}
	bt, err := f.svc.BuyerThread(b.UserID, f.mer.ID)
	must(t, err)
	if len(bt.Messages) != 2 || !bt.Messages[1].FromMerchant || bt.Conversation.BuyerUnread != 0 {
		t.Fatalf("%+v", bt)
	}
	if list, _ = f.svc.BuyerConversations(b.UserID); list[0].BuyerUnread != 0 {
		t.Fatal("not marked read")
	}
}
