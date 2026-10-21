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

func (f shopFixture) config(t *testing.T, key, value string) {
	must(t, f.db.Create(&model.SystemConfig{Key: key, Value: value}).Error)
}

func (f shopFixture) points(t *testing.T, userID uint64) int64 {
	var m model.ShopMember
	f.db.Where("user_id = ?", userID).First(&m)
	return m.Points
}

func TestShopPointsAndMemberTiers(t *testing.T) {
	f := newShopFixture(t, 100, "physical")
	f.config(t, "shop_points_per_usdt", "10")
	f.config(t, "shop_points_value", "0.01")
	f.config(t, "shop_points_max_ratio", "0.5")
	f.config(t, "shop_member_levels", "白银:50:0.9,黄金:500:0.8")
	b, addr := f.buyer(t, 8001, "500")
	order := func(id string, qty int64, usePoints bool) *model.ShopOrder {
		o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: id, Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: qty}}, DeliveryType: shop.DeliveryExpress, AddressID: addr, UsePoints: usePoints})
		must(t, err)
		return o
	}
	finish := func(o *model.ShopOrder) {
		must(t, f.svc.Fulfil(o.ID, shop.Scope{}, "顺丰", o.OrderNo, "", "admin"))
		must(t, f.svc.ConfirmReceipt(b.UserID, o.ID))
	}
	// No tier yet: 6 × 10 = 60 USDT, earns 600 points after the after-sale window.
	first := order("p1", 6, false)
	if first.PayAmount != "60.000000" || first.MemberLevel != "" {
		t.Fatal(first.PayAmount, first.MemberLevel)
	}
	finish(first)
	must(t, f.svc.GrantCommissions(context.Background(), time.Now().AddDate(0, 0, 8)))
	if f.points(t, b.UserID) != 600 {
		t.Fatal("points", f.points(t, b.UserID))
	}
	info, err := f.svc.Member(b.UserID)
	must(t, err)
	if info.Level == nil || info.Level.Name != "白银" || info.Next.Name != "黄金" {
		t.Fatalf("%+v", info)
	}
	// Silver: 10% off 20 → 18; points cover at most half of 18 = 9 USDT = 900 points, but only 600 are held.
	q, err := f.svc.Preview(b.UserID, shop.CheckoutRequest{Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, UsePoints: true})
	must(t, err)
	if q.MemberDiscount != "2.000000" || q.PointsUsed != 600 || q.PointsDiscount != "6.000000" || q.PayAmount != "12.000000" {
		t.Fatalf("%+v", q)
	}
	second := order("p2", 2, true)
	if second.PayAmount != "12.000000" || second.PointsUsed != 600 || f.points(t, b.UserID) != 0 {
		t.Fatal(second.PayAmount, second.PointsUsed, f.points(t, b.UserID))
	}
	// Canceling returns the points.
	must(t, f.svc.CancelByBuyer(b.UserID, second.ID))
	if f.points(t, b.UserID) != 600 {
		t.Fatal("points not returned", f.points(t, b.UserID))
	}
	// Admin adjustments cannot push the balance below zero.
	must(t, f.svc.AdjustPoints(8001, -100, "测试扣减", "admin"))
	if err := f.svc.AdjustPoints(8001, -1000, "超额", "admin"); err == nil {
		t.Fatal("balance went negative")
	}
	if f.points(t, b.UserID) != 500 {
		t.Fatal(f.points(t, b.UserID))
	}
	var logs int64
	f.db.Model(&model.ShopPointsLog{}).Where("user_id = ?", b.UserID).Count(&logs)
	if logs != 4 {
		t.Fatal("points log entries", logs)
	}
}

func (f shopFixture) enableGroups(t *testing.T, size int) {
	must(t, f.db.Model(&model.ShopProduct{}).Where("id = ?", f.prod.ID).Updates(map[string]any{"group_size": size, "group_hours": 2, "group_rate": "0.8"}).Error)
}

func TestShopGroupBuyingSucceedsWhenFull(t *testing.T) {
	f := newShopFixture(t, 50, "physical")
	f.enableGroups(t, 3)
	leader, laddr := f.buyer(t, 8101, "100")
	start, err := f.svc.CreateOrder(leader, shop.CheckoutRequest{RequestID: "g-start", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: laddr, GroupMode: "start"})
	must(t, err)
	if start.Status != shop.StatusGrouping || start.PayAmount != "8.000000" || start.GroupID == 0 {
		t.Fatalf("%+v", start)
	}
	if err := f.svc.Fulfil(start.ID, shop.Scope{}, "顺丰", "x", "", "admin"); err == nil {
		t.Fatal("shipped before the group filled")
	}
	if _, err := f.svc.CreateOrder(leader, shop.CheckoutRequest{RequestID: "g-again", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: laddr, GroupMode: "join", GroupID: start.GroupID}); err == nil {
		t.Fatal("joined own group twice")
	}
	// Five buyers race for the two open places.
	var wg sync.WaitGroup
	var joined int32
	for i := 0; i < 5; i++ {
		b, addr := f.buyer(t, int64(8110+i), "100")
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: fmt.Sprintf("g-join-%d", i), Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr, GroupMode: "join", GroupID: start.GroupID}); err == nil {
				atomic.AddInt32(&joined, 1)
			}
		}(i)
	}
	wg.Wait()
	var grp model.ShopGroup
	must(t, f.db.First(&grp, start.GroupID).Error)
	var paid, waiting int64
	f.db.Model(&model.ShopOrder{}).Where("group_id = ? AND status = ?", grp.ID, shop.StatusPaid).Count(&paid)
	f.db.Model(&model.ShopOrder{}).Where("group_id = ? AND status = ?", grp.ID, shop.StatusGrouping).Count(&waiting)
	if joined != 2 || grp.Joined != 3 || grp.Status != shop.GroupSuccess || paid != 3 || waiting != 0 {
		t.Fatalf("joined=%d group=%+v paid=%d waiting=%d", joined, grp, paid, waiting)
	}
	must(t, f.svc.Fulfil(start.ID, shop.Scope{}, "顺丰", "x", "", "admin"))
}

func TestShopGroupBuyingExpiresAndRefunds(t *testing.T) {
	f := newShopFixture(t, 50, "physical")
	f.enableGroups(t, 3)
	leader, laddr := f.buyer(t, 8201, "100")
	member, maddr := f.buyer(t, 8202, "100")
	quitter, qaddr := f.buyer(t, 8203, "100")
	start, err := f.svc.CreateOrder(leader, shop.CheckoutRequest{RequestID: "e-start", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryExpress, AddressID: laddr, GroupMode: "start"})
	must(t, err)
	_, err = f.svc.CreateOrder(member, shop.CheckoutRequest{RequestID: "e-join", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: maddr, GroupMode: "join", GroupID: start.GroupID})
	must(t, err)
	third, err := f.svc.CreateOrder(quitter, shop.CheckoutRequest{RequestID: "e-third", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: qaddr, GroupMode: "join", GroupID: start.GroupID})
	if err != nil || third.Status != shop.StatusPaid {
		t.Fatal("third member should fill the group", err)
	}

	// A second group that never fills.
	g2, err := f.svc.CreateOrder(leader, shop.CheckoutRequest{RequestID: "e2-start", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: laddr, GroupMode: "start"})
	must(t, err)
	j2, err := f.svc.CreateOrder(member, shop.CheckoutRequest{RequestID: "e2-join", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: maddr, GroupMode: "join", GroupID: g2.GroupID})
	must(t, err)
	// A member may leave while waiting; the group shrinks.
	must(t, f.svc.CancelByBuyer(member.UserID, j2.ID))
	var grp model.ShopGroup
	must(t, f.db.First(&grp, g2.GroupID).Error)
	if grp.Joined != 1 || grp.Status != shop.GroupOpen {
		t.Fatalf("%+v", grp)
	}
	leaderBefore, stockBefore := f.balance(t, leader.UserID), f.stock(t)
	must(t, f.svc.ExpireGroups(context.Background(), time.Now().Add(3*time.Hour)))
	must(t, f.db.First(&grp, g2.GroupID).Error)
	var o model.ShopOrder
	must(t, f.db.First(&o, g2.ID).Error)
	if grp.Status != shop.GroupFailed || o.Status != shop.StatusCanceled || !strings.Contains(o.CancelReason, "拼团未成团") {
		t.Fatalf("group=%s order=%s", grp.Status, o.Status)
	}
	if f.balance(t, leader.UserID) == leaderBefore || f.stock(t) != stockBefore+1 {
		t.Fatal("not refunded or restocked", f.balance(t, leader.UserID), leaderBefore, f.stock(t), stockBefore)
	}
	if _, err := f.svc.CreateOrder(member, shop.CheckoutRequest{RequestID: "late", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: maddr, GroupMode: "join", GroupID: g2.GroupID}); err == nil {
		t.Fatal("joined a failed group")
	}
}
