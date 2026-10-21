package shop

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/wallet"
)

// CheckoutItem is one SKU and quantity being bought.
type CheckoutItem struct {
	SKUID    uint64 `json:"sku_id"`
	Quantity int64  `json:"quantity"`
}

// CheckoutGroup is one merchant's part of a checkout; it becomes one order.
type CheckoutGroup struct {
	Items        []CheckoutItem `json:"items"`
	DeliveryType string         `json:"delivery_type"`
	UserCouponID uint64         `json:"user_coupon_id"`
	Remark       string         `json:"remark"`
	UsePoints    bool           `json:"use_points"`
	GroupMode    string         `json:"group_mode"` // "start" or "join" for group buying
	GroupID      uint64         `json:"group_id"`   // the group to join
}

// CheckoutRequest pays for one group (the top-level fields) or several (Groups), e.g. a cart
// spanning merchants. All orders of a request are charged in one transaction.
type CheckoutRequest struct {
	RequestID    string          `json:"request_id"`
	Items        []CheckoutItem  `json:"items"`
	DeliveryType string          `json:"delivery_type"`
	AddressID    uint64          `json:"address_id"`
	UserCouponID uint64          `json:"user_coupon_id"`
	Remark       string          `json:"remark"`
	FromCart     bool            `json:"from_cart"`
	UsePoints    bool            `json:"use_points"`
	GroupMode    string          `json:"group_mode"`
	GroupID      uint64          `json:"group_id"`
	Groups       []CheckoutGroup `json:"groups"`
}

const maxCheckoutGroups = 10

// Quote is the price of a checkout, with the coupons the buyer could use on it.
type Quote struct {
	MerchantID      uint64       `json:"merchant_id"`
	MerchantName    string       `json:"merchant_name"`
	MerchantAddress string       `json:"merchant_address"`
	DeliveryTypes   []string     `json:"delivery_types"`
	GoodsAmount     string       `json:"goods_amount"`
	Discount        string       `json:"discount"`
	PayAmount       string       `json:"pay_amount"`
	Coupons         []UserCoupon `json:"coupons"`
	Balance         string       `json:"balance"`
	MemberLevel     string       `json:"member_level"`
	MemberDiscount  string       `json:"member_discount"`
	Points          int64        `json:"points"`      // the buyer's balance
	PointsUsed      int64        `json:"points_used"` // points this checkout would spend
	PointsDiscount  string       `json:"points_discount"`
}

type pricedItem struct {
	CheckoutItem
	line line
}

// price validates a checkout against current data. It is used for previews and, inside the order
// transaction, for the real charge; db is the transaction in the second case.
func (s *Service) price(db *gorm.DB, items []CheckoutItem) ([]pricedItem, decimal.Decimal, []string, error) {
	if len(items) == 0 {
		return nil, decimal.Zero, nil, userErr("请选择要购买的商品")
	}
	if len(items) > 50 {
		return nil, decimal.Zero, nil, userErr("单次最多购买 50 种商品")
	}
	ids := []uint64{}
	seen := map[uint64]bool{}
	for _, it := range items {
		if it.Quantity < 1 || it.Quantity > maxQuantity {
			return nil, decimal.Zero, nil, userErr("购买数量需在 1–%d 之间", maxQuantity)
		}
		if seen[it.SKUID] {
			return nil, decimal.Zero, nil, userErr("商品重复")
		}
		seen[it.SKUID] = true
		ids = append(ids, it.SKUID)
	}
	ls, err := s.lines(db, ids)
	if err != nil {
		return nil, decimal.Zero, nil, err
	}
	out := []pricedItem{}
	total := decimal.Zero
	var merchantID uint64
	kinds := map[string]bool{}
	express, pickup := true, true
	for _, it := range items {
		l, ok := ls[it.SKUID]
		if !ok || !l.available() {
			return nil, decimal.Zero, nil, userErr("部分商品已下架，请返回购物车检查")
		}
		if l.Stock < it.Quantity {
			return nil, decimal.Zero, nil, userErr("「%s %s」库存不足，剩余 %d", l.ProductName, l.Name, l.Stock)
		}
		if l.OnPromo && l.PromoStock != nil && *l.PromoStock < it.Quantity {
			return nil, decimal.Zero, nil, userErr("「%s %s」活动价限量，仅剩 %d 件", l.ProductName, l.Name, *l.PromoStock)
		}
		if merchantID != 0 && l.MerchantID != merchantID {
			return nil, decimal.Zero, nil, userErr("不同商家的商品请分开下单")
		}
		merchantID = l.MerchantID
		kinds[l.Kind] = true
		express = express && l.AllowExpress
		pickup = pickup && l.AllowPickup
		total = total.Add(amount(l.Price).Mul(decimal.NewFromInt(it.Quantity)))
		out = append(out, pricedItem{CheckoutItem: it, line: l})
	}
	if len(kinds) > 1 {
		return nil, decimal.Zero, nil, userErr("虚拟商品和实物商品请分开下单")
	}
	types := []string{}
	if kinds["virtual"] {
		types = append(types, DeliveryVirtual)
	} else {
		if express {
			types = append(types, DeliveryExpress)
		}
		if pickup {
			types = append(types, DeliveryPickup)
		}
		if len(types) == 0 {
			return nil, decimal.Zero, nil, userErr("所选商品没有共同的配送方式，请分开下单")
		}
	}
	return out, total.Truncate(6), types, nil
}

// couponDiscount returns how much a held coupon takes off goodsAmount for this merchant,
// or a user error explaining why it cannot be used.
func couponDiscount(held model.ShopUserCoupon, c model.ShopCoupon, merchantID uint64, goods decimal.Decimal, now time.Time) (decimal.Decimal, error) {
	if held.Status != CouponUnused {
		return decimal.Zero, userErr("优惠券已使用")
	}
	if held.ExpiresAt != nil && !held.ExpiresAt.After(now) {
		return decimal.Zero, userErr("优惠券已过期")
	}
	if !c.Enabled {
		return decimal.Zero, userErr("优惠券已停用")
	}
	if c.MerchantID != 0 && c.MerchantID != merchantID {
		return decimal.Zero, userErr("该优惠券不适用于此商家")
	}
	if goods.LessThan(amount(c.Threshold)) {
		return decimal.Zero, userErr("未达到优惠券使用门槛 %s USDT", amount(c.Threshold).String())
	}
	return decimal.Min(amount(c.Discount), goods), nil
}

// Preview prices a checkout and lists usable coupons without charging anything.
func (s *Service) Preview(userID uint64, req CheckoutRequest) (*Quote, error) {
	items, goods, types, err := s.price(s.st.DB, req.Items)
	if err != nil {
		return nil, err
	}
	if req.GroupMode != "" {
		if goods, err = applyGroupPrice(items); err != nil {
			return nil, err
		}
	}
	merchant := items[0].line
	q := &Quote{MerchantID: merchant.MerchantID, MerchantName: merchant.MerchantName, MerchantAddress: merchant.MerchantAddress, DeliveryTypes: types, GoodsAmount: goods.StringFixed(6), Coupons: []UserCoupon{}}
	held, err := s.MyCoupons(userID)
	if err != nil {
		return nil, err
	}
	discount := decimal.Zero
	now := time.Now()
	for _, h := range held {
		c := model.ShopCoupon{Enabled: h.Enabled, MerchantID: h.MerchantID, Threshold: h.Threshold, Discount: h.Discount}
		d, err := couponDiscount(h.ShopUserCoupon, c, merchant.MerchantID, goods, now)
		if err != nil {
			continue
		}
		q.Coupons = append(q.Coupons, h)
		if h.ID == req.UserCouponID {
			discount = d
		}
	}
	if req.UserCouponID != 0 && discount.IsZero() {
		return nil, userErr("所选优惠券不可用")
	}
	m, err := s.member(s.st.DB, userID)
	if err != nil {
		return nil, err
	}
	level, member, pointsUsed, pointsDiscount := s.orderDiscounts(amount(m.Spent), m.Points, goods, discount, req.UsePoints)
	q.Discount, q.MemberLevel, q.MemberDiscount = discount.StringFixed(6), level, member.StringFixed(6)
	q.Points, q.PointsUsed, q.PointsDiscount = m.Points, pointsUsed, pointsDiscount.StringFixed(6)
	q.PayAmount = goods.Sub(discount).Sub(member).Sub(pointsDiscount).StringFixed(6)
	available, _, err := s.wallet.Balance(userID)
	if err != nil {
		return nil, err
	}
	q.Balance = available.StringFixed(6)
	return q, nil
}

// Buyer identifies who is placing an order and through which bot.
type Buyer struct {
	UserID         uint64
	TelegramUserID int64
	BotID          uint64
}

func newOrderNo(now time.Time) string {
	n, _ := rand.Int(rand.Reader, big.NewInt(1_000_000))
	return fmt.Sprintf("S%s%06d", now.Format("060102150405"), n.Int64())
}

// CreateOrder places a single-group checkout; see CreateOrders.
func (s *Service) CreateOrder(b Buyer, req CheckoutRequest) (*model.ShopOrder, error) {
	orders, err := s.CreateOrders(b, req)
	if err != nil {
		return nil, err
	}
	return &orders[0], nil
}

// CreateOrders charges the wallet and creates one paid order per group in a single transaction:
// either every order is placed or none is. Repeating a request ID returns the original orders,
// so a retried tap never charges twice.
func (s *Service) CreateOrders(b Buyer, req CheckoutRequest) ([]model.ShopOrder, error) {
	req.RequestID = strings.TrimSpace(req.RequestID)
	if req.RequestID == "" || len(req.RequestID) > 60 {
		return nil, userErr("请求无效，请刷新后重试")
	}
	groups := req.Groups
	if len(groups) == 0 {
		groups = []CheckoutGroup{{Items: req.Items, DeliveryType: req.DeliveryType, UserCouponID: req.UserCouponID, Remark: req.Remark, UsePoints: req.UsePoints, GroupMode: req.GroupMode, GroupID: req.GroupID}}
	}
	if len(groups) > maxCheckoutGroups {
		return nil, userErr("一次最多结算 %d 个商家", maxCheckoutGroups)
	}
	// A single group keeps the plain request ID, so earlier clients stay idempotent.
	requestIDs := []string{req.RequestID}
	if len(groups) > 1 {
		requestIDs = requestIDs[:0]
		for i := range groups {
			requestIDs = append(requestIDs, fmt.Sprintf("%s#%d", req.RequestID, i))
		}
	}
	var orders []model.ShopOrder
	var existing bool
	var filledGroups []uint64
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		// One checkout per buyer at a time, so purchase limits hold under concurrent taps. This
		// must be the first statement: under REPEATABLE READ the first plain read fixes the
		// snapshot, and it has to come after the lock to see orders committed while waiting.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&model.TelegramUser{}, b.UserID).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND request_id IN ?", b.UserID, requestIDs).Order("id asc").Find(&orders).Error; err != nil {
			return err
		}
		if len(orders) > 0 {
			existing = true
			return nil
		}
		var cartSKUs []uint64
		for i, g := range groups {
			o, skus, filled, err := s.createOrderTx(tx, b, requestIDs[i], g, req.AddressID)
			if err != nil {
				if len(groups) > 1 && IsUserError(err) {
					return userErr("第 %d 个商家：%s", i+1, err.Error())
				}
				return err
			}
			orders = append(orders, o)
			cartSKUs = append(cartSKUs, skus...)
			if filled {
				filledGroups = append(filledGroups, o.GroupID)
			}
		}
		if req.FromCart {
			if err := tx.Where("user_id = ? AND sku_id IN ?", b.UserID, cartSKUs).Delete(&model.ShopCartItem{}).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !existing {
		for _, id := range filledGroups {
			s.notifyGroupSuccess(id)
		}
		for _, o := range orders {
			if o.Status == StatusGrouping {
				s.notify(o.BotID, o.TelegramUserID, fmt.Sprintf("👥 <b>拼团已支付，等待成团</b>\n\n订单号：<code>%s</code>\n分享给好友一起拼，未成团将自动退款。", o.OrderNo), o.ID)
				continue
			}
			if o.GroupID != 0 {
				continue // a filled group is announced by notifyGroupSuccess
			}
			msg := fmt.Sprintf("🛒 <b>下单成功</b>\n\n订单号：<code>%s</code>\n商家：%s\n实付：<code>%s USDT</code>", o.OrderNo, htmlEscape(o.MerchantName), amount(o.PayAmount).String())
			if o.Status == StatusCompleted {
				msg += "\n\n🎁 卡密已发放，请在商城「订单详情」中查看。"
			}
			s.notify(o.BotID, o.TelegramUserID, msg, o.ID)
			s.notifyStaff(o.MerchantID, fmt.Sprintf("🛒 <b>商城新订单</b>\n\n订单号：<code>%s</code>\n商家：%s\n配送：%s\n实付：<code>%s USDT</code>\n买家 Telegram ID：<code>%d</code>%s", o.OrderNo, htmlEscape(o.MerchantName), deliveryNames[o.DeliveryType], amount(o.PayAmount).String(), o.TelegramUserID, map[bool]string{true: "\n\n卡密已自动发放。", false: ""}[o.Status == StatusCompleted]))
		}
	}
	return orders, nil
}

var deliveryNames = map[string]string{DeliveryExpress: "快递配送", DeliveryPickup: "到店自提", DeliveryVirtual: "虚拟商品"}

// createOrderTx prices, charges and records one group inside the caller's transaction and
// returns the order with the SKUs it bought.
// filled reports that this order completed a group-buying group.
func (s *Service) createOrderTx(tx *gorm.DB, b Buyer, requestID string, g CheckoutGroup, addressID uint64) (order model.ShopOrder, skus []uint64, filled bool, err error) {
	if err = checkLen(g.Remark, "备注", 255, false); err != nil {
		return
	}
	if g.GroupMode != "" && g.GroupMode != "start" && g.GroupMode != "join" {
		err = userErr("拼团参数无效")
		return
	}
	items, goods, types, err := s.price(tx, g.Items)
	if err != nil {
		return
	}
	if g.GroupMode != "" {
		if items[0].line.Kind == "virtual" {
			err = userErr("虚拟商品不支持拼团")
			return
		}
		if goods, err = applyGroupPrice(items); err != nil {
			return
		}
	}
	allowed := false
	for _, t := range types {
		allowed = allowed || t == g.DeliveryType
	}
	if !allowed {
		return order, nil, false, userErr("所选商品不支持该配送方式")
	}
	if err := s.checkLimits(tx, b.UserID, items); err != nil {
		return order, nil, false, err
	}
	first := items[0].line
	now := time.Now()
	order = model.ShopOrder{OrderNo: newOrderNo(now), RequestID: requestID, UserID: b.UserID, TelegramUserID: b.TelegramUserID, BotID: b.BotID, MerchantID: first.MerchantID, MerchantName: first.MerchantName, DeliveryType: g.DeliveryType, Status: StatusPaid, Remark: strings.TrimSpace(g.Remark), RefundedAmount: "0", PaidAt: now}
	switch g.DeliveryType {
	case DeliveryExpress:
		var addr model.ShopAddress
		if err := tx.Where("id = ? AND user_id = ?", addressID, b.UserID).First(&addr).Error; err != nil {
			return order, nil, false, userErr("请选择收货地址")
		}
		order.ContactName, order.ContactPhone, order.Address = addr.Name, addr.Phone, addr.Address
	case DeliveryPickup:
		order.Address = first.MerchantAddress
	}

	discount := decimal.Zero
	var held model.ShopUserCoupon
	if g.UserCouponID != 0 {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", g.UserCouponID, b.UserID).First(&held).Error; err != nil {
			return order, nil, false, userErr("优惠券不存在")
		}
		var c model.ShopCoupon
		if err := tx.First(&c, held.CouponID).Error; err != nil {
			return order, nil, false, userErr("优惠券不存在")
		}
		if discount, err = couponDiscount(held, c, first.MerchantID, goods, now); err != nil {
			return order, nil, false, err
		}
		order.UserCouponID = held.ID
	}
	// Tier and points discounts come after the coupon. Spending points locks the member row.
	var m model.ShopMember
	if g.UsePoints {
		m, err = lockMember(tx, b.UserID)
	} else {
		m, err = s.member(tx, b.UserID)
	}
	if err != nil {
		return order, nil, false, err
	}
	level, member, pointsUsed, pointsDiscount := s.orderDiscounts(amount(m.Spent), m.Points, goods, discount, g.UsePoints)
	pay := goods.Sub(discount).Sub(member).Sub(pointsDiscount)
	order.GoodsAmount, order.Discount, order.PayAmount = goods.StringFixed(6), discount.StringFixed(6), pay.StringFixed(6)
	order.MemberLevel, order.MemberDiscount, order.PointsUsed, order.PointsDiscount = level, member.StringFixed(6), pointsUsed, pointsDiscount.StringFixed(6)
	if g.GroupMode != "" {
		grp, full, err := s.joinGroup(tx, b, g, items[0].line)
		if err != nil {
			return order, nil, false, err
		}
		order.GroupID, filled = grp.ID, full
		if !full {
			order.Status = StatusGrouping
		}
	}
	if err := tx.Create(&order).Error; err != nil {
		return order, nil, false, err
	}
	if filled {
		if err := completeGroup(tx, order.GroupID); err != nil {
			return order, nil, false, err
		}
	}
	if pointsUsed > 0 {
		if err := addPoints(tx, b.UserID, -pointsUsed, PointsSpend, order.ID, "商城订单抵扣 "+order.OrderNo, ""); err != nil {
			return order, nil, false, err
		}
	}
	skus = []uint64{}
	for _, it := range items {
		// The conditional update is the stock check that holds under concurrent orders.
		res := tx.Model(&model.ShopSKU{}).Where("id = ? AND stock >= ?", it.SKUID, it.Quantity).Update("stock", gorm.Expr("stock - ?", it.Quantity))
		if res.Error != nil {
			return order, nil, false, res.Error
		}
		if res.RowsAffected != 1 {
			return order, nil, false, userErr("「%s %s」库存不足", it.line.ProductName, it.line.Name)
		}
		if it.line.OnPromo && it.line.PromoStock != nil {
			res := tx.Model(&model.ShopSKU{}).Where("id = ? AND promo_stock >= ?", it.SKUID, it.Quantity).Update("promo_stock", gorm.Expr("promo_stock - ?", it.Quantity))
			if res.Error != nil {
				return order, nil, false, res.Error
			}
			// Never fall back to the regular price silently: the buyer was shown the promo price.
			if res.RowsAffected != 1 {
				return order, nil, false, userErr("「%s %s」活动价已抢完，请返回刷新后按当前价格购买", it.line.ProductName, it.line.Name)
			}
		}
		if err := tx.Model(&model.ShopProduct{}).Where("id = ?", it.line.ProductID).Update("sales", gorm.Expr("sales + ?", it.Quantity)).Error; err != nil {
			return order, nil, false, err
		}
		if err := tx.Create(&model.ShopOrderItem{OrderID: order.ID, ProductID: it.line.ProductID, SKUID: it.SKUID, ProductName: it.line.ProductName, SKUName: it.line.Name, ImageURL: firstImage(it.line.Images), Price: it.line.Price, Quantity: it.Quantity, Promo: it.line.OnPromo}).Error; err != nil {
			return order, nil, false, err
		}
		skus = append(skus, it.SKUID)
	}
	if order.UserCouponID != 0 {
		// The coupon row is locked and re-checked above, so one coupon cannot pay for two groups.
		if err := tx.Model(&model.ShopUserCoupon{}).Where("id = ?", held.ID).Updates(map[string]any{"status": CouponUsed, "order_id": order.ID}).Error; err != nil {
			return order, nil, false, err
		}
	}
	if err := s.wallet.Spend(tx, b.UserID, pay, "shop_order", order.ID, "商城订单 "+order.OrderNo); err != nil {
		if errors.Is(err, wallet.ErrInsufficientBalance) {
			return order, nil, false, userErr("余额不足，请先充值")
		}
		return order, nil, false, err
	}
	if g.DeliveryType == DeliveryVirtual {
		if err := s.assignCodes(tx, &order, items); err != nil {
			return order, nil, false, err
		}
	}
	return order, skus, filled, nil
}

// assignCodes delivers pre-loaded codes for a virtual order. When any SKU lacks codes the order
// stays paid for an administrator to deliver by hand.
func (s *Service) assignCodes(tx *gorm.DB, order *model.ShopOrder, items []pricedItem) error {
	var picked []model.ShopVirtualCode
	for _, it := range items {
		var codes []model.ShopVirtualCode
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("sku_id = ? AND order_id = 0", it.SKUID).Order("id asc").Limit(int(it.Quantity)).Find(&codes).Error; err != nil {
			return err
		}
		if int64(len(codes)) < it.Quantity {
			return nil
		}
		picked = append(picked, codes...)
	}
	lines := []string{}
	ids := []uint64{}
	for _, c := range picked {
		ids = append(ids, c.ID)
		lines = append(lines, c.Code)
	}
	if err := tx.Model(&model.ShopVirtualCode{}).Where("id IN ?", ids).Update("order_id", order.ID).Error; err != nil {
		return err
	}
	now := time.Now()
	order.Status, order.DeliveryNote, order.CompletedAt, order.HandledBy = StatusCompleted, strings.Join(lines, "\n"), &now, "auto"
	if err := tx.Model(&model.ShopOrder{}).Where("id = ?", order.ID).Updates(map[string]any{"status": order.Status, "delivery_note": order.DeliveryNote, "completed_at": now, "handled_by": order.HandledBy}).Error; err != nil {
		return err
	}
	return s.recordSale(tx, order.ID)
}

// OrderDetail is an order with its items (and, for a single order, its after-sale requests).
type OrderDetail struct {
	model.ShopOrder
	Items      []model.ShopOrderItem `json:"items"`
	AfterSales []model.ShopAfterSale `json:"after_sales,omitempty"`
	// CanApplyAfterSale tells the buyer's page whether to offer an after-sale request.
	CanApplyAfterSale bool `json:"can_apply_after_sale"`
	// CanReview is set while the buyer can still review items not in ReviewedItems.
	CanReview     bool     `json:"can_review"`
	ReviewedItems []uint64 `json:"reviewed_items,omitempty"`
}

func (s *Service) withItems(orders []model.ShopOrder) ([]OrderDetail, error) {
	out := make([]OrderDetail, 0, len(orders))
	if len(orders) == 0 {
		return out, nil
	}
	ids := []uint64{}
	for _, o := range orders {
		ids = append(ids, o.ID)
	}
	var items []model.ShopOrderItem
	if err := s.st.DB.Where("order_id IN ?", ids).Order("id asc").Find(&items).Error; err != nil {
		return nil, err
	}
	byOrder := map[uint64][]model.ShopOrderItem{}
	for _, it := range items {
		byOrder[it.OrderID] = append(byOrder[it.OrderID], it)
	}
	for _, o := range orders {
		list := byOrder[o.ID]
		if list == nil {
			list = []model.ShopOrderItem{}
		}
		out = append(out, OrderDetail{ShopOrder: o, Items: list})
	}
	return out, nil
}

// Orders lists the buyer's orders, optionally by status, newest first.
func (s *Service) Orders(userID uint64, status string, page int) ([]OrderDetail, error) {
	db := s.st.DB.Where("user_id = ?", userID)
	if status != "" {
		db = db.Where("status = ?", status)
	}
	if page < 1 {
		page = 1
	}
	var orders []model.ShopOrder
	if err := db.Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&orders).Error; err != nil {
		return nil, err
	}
	return s.withItems(orders)
}

func (s *Service) Order(userID, id uint64) (*OrderDetail, error) {
	var o model.ShopOrder
	if err := s.st.DB.Where("id = ? AND user_id = ?", id, userID).First(&o).Error; err != nil {
		return nil, userErr("订单不存在")
	}
	list, err := s.withItems([]model.ShopOrder{o})
	if err != nil {
		return nil, err
	}
	d := &list[0]
	if err := s.st.DB.Where("order_id = ?", o.ID).Order("id desc").Find(&d.AfterSales).Error; err != nil {
		return nil, err
	}
	d.CanApplyAfterSale = s.afterSaleAllowed(o, time.Now()) == nil
	if o.Status == StatusCompleted && o.CompletedAt != nil && time.Since(*o.CompletedAt) <= reviewWindowDays*24*time.Hour {
		if d.ReviewedItems, err = s.reviewedItems(o.ID); err != nil {
			return nil, err
		}
		d.CanReview = len(d.ReviewedItems) < len(d.Items)
	}
	return d, nil
}

// Scope limits an order operation to one buyer (UserID) or one merchant's console account
// (MerchantID); zero values mean no restriction.
type Scope struct {
	UserID     uint64
	MerchantID uint64
}

// lockOrder loads an order for update within the scope.
func lockOrder(tx *gorm.DB, id uint64, scope Scope) (model.ShopOrder, error) {
	var o model.ShopOrder
	q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id)
	if scope.UserID != 0 {
		q = q.Where("user_id = ?", scope.UserID)
	}
	if scope.MerchantID != 0 {
		q = q.Where("merchant_id = ?", scope.MerchantID)
	}
	if err := q.First(&o).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return o, userErr("订单不存在")
		}
		return o, err
	}
	return o, nil
}

// CancelByBuyer cancels an order that has not been shipped or prepared yet.
func (s *Service) CancelByBuyer(userID, id uint64) error {
	return s.cancel(id, Scope{UserID: userID}, "买家取消", "")
}

// CancelByAdmin cancels any paid, shipped or ready order and refunds what has not been refunded yet.
func (s *Service) CancelByAdmin(id uint64, scope Scope, reason, operator string) error {
	if err := checkLen(reason, "取消原因", 255, true); err != nil {
		return err
	}
	return s.cancel(id, scope, strings.TrimSpace(reason), operator)
}

func (s *Service) cancel(id uint64, scope Scope, reason, operator string) error {
	var o model.ShopOrder
	var refund decimal.Decimal
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if o, err = lockOrder(tx, id, scope); err != nil {
			return err
		}
		if scope.UserID != 0 && o.Status != StatusPaid && o.Status != StatusGrouping {
			return userErr("订单已发货或已备货，请联系商家处理")
		}
		if o.Status != StatusPaid && o.Status != StatusShipped && o.Status != StatusReady && o.Status != StatusGrouping {
			return userErr("当前订单状态不能取消")
		}
		if o.Status == StatusGrouping && o.GroupID != 0 {
			if err := leaveGroup(tx, o.GroupID); err != nil {
				return err
			}
		}
		if o.PointsUsed > 0 {
			if err := addPoints(tx, o.UserID, o.PointsUsed, PointsRefund, o.ID, "商城订单取消退回 "+o.OrderNo, operator); err != nil {
				return err
			}
		}
		if o.AfterSaleOpen {
			return userErr("该订单有处理中的售后申请，请先处理售后")
		}
		var items []model.ShopOrderItem
		if err := tx.Where("order_id = ?", o.ID).Find(&items).Error; err != nil {
			return err
		}
		for _, it := range items {
			if err := tx.Model(&model.ShopSKU{}).Where("id = ?", it.SKUID).Update("stock", gorm.Expr("stock + ?", it.Quantity)).Error; err != nil {
				return err
			}
			if it.Promo {
				if err := tx.Model(&model.ShopSKU{}).Where("id = ? AND promo_stock IS NOT NULL", it.SKUID).Update("promo_stock", gorm.Expr("promo_stock + ?", it.Quantity)).Error; err != nil {
					return err
				}
			}
			if err := tx.Model(&model.ShopProduct{}).Where("id = ? AND sales >= ?", it.ProductID, it.Quantity).Update("sales", gorm.Expr("sales - ?", it.Quantity)).Error; err != nil {
				return err
			}
		}
		if o.UserCouponID != 0 {
			if err := tx.Model(&model.ShopUserCoupon{}).Where("id = ?", o.UserCouponID).Updates(map[string]any{"status": CouponUnused, "order_id": 0}).Error; err != nil {
				return err
			}
		}
		refund = amount(o.PayAmount).Sub(amount(o.RefundedAmount))
		if refund.IsNegative() {
			refund = decimal.Zero
		}
		if err := s.wallet.Refund(tx, o.UserID, refund, "shop_order", o.ID, "商城订单退款 "+o.OrderNo); err != nil {
			return err
		}
		now := time.Now()
		o.Status, o.CancelReason, o.CanceledAt = StatusCanceled, reason, &now
		return tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Updates(map[string]any{"status": StatusCanceled, "cancel_reason": reason, "canceled_at": now, "handled_by": operator, "refunded_amount": amount(o.PayAmount).StringFixed(6)}).Error
	})
	if err == nil {
		s.notify(o.BotID, o.TelegramUserID, fmt.Sprintf("↩️ <b>订单已取消</b>\n\n订单号：<code>%s</code>\n原因：%s\n<code>%s USDT</code> 已退回钱包余额。", o.OrderNo, htmlEscape(reason), refund.String()), o.ID)
		if scope.UserID != 0 {
			s.notifyStaff(o.MerchantID, fmt.Sprintf("↩️ <b>买家取消了订单</b>\n\n订单号：<code>%s</code>\n商家：%s\n已退款：<code>%s USDT</code>", o.OrderNo, htmlEscape(o.MerchantName), refund.String()))
		}
	}
	return err
}

// ConfirmReceipt lets the buyer complete a shipped or ready order.
func (s *Service) ConfirmReceipt(userID, id uint64) error {
	return s.complete(id, Scope{UserID: userID}, "")
}

// CompleteByAdmin completes a shipped or ready order, e.g. after pickup.
func (s *Service) CompleteByAdmin(id uint64, scope Scope, operator string) error {
	return s.complete(id, scope, operator)
}

func (s *Service) complete(id uint64, scope Scope, operator string) error {
	var o model.ShopOrder
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if o, err = lockOrder(tx, id, scope); err != nil {
			return err
		}
		if o.Status != StatusShipped && o.Status != StatusReady {
			return userErr("当前订单状态不能确认完成")
		}
		if operator == autoOperator && o.AfterSaleOpen {
			return userErr("售后处理中，暂不自动确认")
		}
		updates := map[string]any{"status": StatusCompleted, "completed_at": time.Now()}
		if operator != "" {
			updates["handled_by"] = operator
		}
		if err := tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
			return err
		}
		return s.recordSale(tx, o.ID)
	})
	if err == nil && scope.UserID == 0 {
		msg := fmt.Sprintf("✅ <b>订单已完成</b>\n\n订单号：<code>%s</code>\n感谢您的购买！", o.OrderNo)
		if operator == autoOperator {
			msg = fmt.Sprintf("✅ <b>订单已自动确认收货</b>\n\n订单号：<code>%s</code>\n如有问题，可在商城订单详情中申请售后。", o.OrderNo)
		}
		s.notify(o.BotID, o.TelegramUserID, msg, o.ID)
	}
	return err
}

// Fulfil moves a paid order forward: express orders ship with a carrier and tracking number,
// pickup orders become ready with an optional note, and virtual orders complete with the delivered content.
func (s *Service) Fulfil(id uint64, scope Scope, carrier, trackingNo, note, operator string) error {
	var o model.ShopOrder
	var msg string
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if o, err = lockOrder(tx, id, scope); err != nil {
			return err
		}
		if o.Status != StatusPaid {
			return userErr("只有待处理的订单可以发货")
		}
		now := time.Now()
		updates := map[string]any{"handled_by": operator}
		switch o.DeliveryType {
		case DeliveryExpress:
			if err := checkLen(carrier, "快递公司", 64, true); err != nil {
				return err
			}
			if err := checkLen(trackingNo, "快递单号", 128, true); err != nil {
				return err
			}
			updates["status"], updates["carrier"], updates["tracking_no"], updates["shipped_at"] = StatusShipped, strings.TrimSpace(carrier), strings.TrimSpace(trackingNo), now
			msg = fmt.Sprintf("🚚 <b>订单已发货</b>\n\n订单号：<code>%s</code>\n快递：%s\n单号：<code>%s</code>", o.OrderNo, htmlEscape(strings.TrimSpace(carrier)), htmlEscape(strings.TrimSpace(trackingNo)))
		case DeliveryPickup:
			if err := checkLen(note, "取货说明", 1000, false); err != nil {
				return err
			}
			updates["status"], updates["delivery_note"], updates["shipped_at"] = StatusReady, strings.TrimSpace(note), now
			msg = fmt.Sprintf("🏪 <b>商品已备好，可以到店自提</b>\n\n订单号：<code>%s</code>\n地址：%s", o.OrderNo, htmlEscape(o.Address))
		case DeliveryVirtual:
			if err := checkLen(note, "发货内容", 5000, true); err != nil {
				return err
			}
			updates["status"], updates["delivery_note"], updates["completed_at"] = StatusCompleted, strings.TrimSpace(note), now
			msg = fmt.Sprintf("🎁 <b>虚拟商品已发放</b>\n\n订单号：<code>%s</code>\n请在商城「订单详情」中查看。", o.OrderNo)
		}
		if err := tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Updates(updates).Error; err != nil {
			return err
		}
		if updates["status"] == StatusCompleted {
			return s.recordSale(tx, o.ID)
		}
		return nil
	})
	if err == nil {
		s.notify(o.BotID, o.TelegramUserID, msg, o.ID)
	}
	return err
}

// checkLimits enforces each product's per-buyer limit, counting units in the buyer's
// non-canceled orders (including ones created earlier in this checkout) plus this group.
func (s *Service) checkLimits(tx *gorm.DB, userID uint64, items []pricedItem) error {
	want := map[uint64]int64{}
	limits := map[uint64]pricedItem{}
	for _, it := range items {
		if it.line.LimitPerUser > 0 {
			want[it.line.ProductID] += it.Quantity
			limits[it.line.ProductID] = it
		}
	}
	for productID, qty := range want {
		var bought int64
		if err := tx.Raw(`SELECT COALESCE(SUM(i.quantity), 0) FROM shop_order_items i JOIN shop_orders o ON o.id = i.order_id
			WHERE o.user_id = ? AND o.status <> ? AND i.product_id = ?`, userID, StatusCanceled, productID).Scan(&bought).Error; err != nil {
			return err
		}
		it := limits[productID]
		if bought+qty > it.line.LimitPerUser {
			left := it.line.LimitPerUser - bought
			if left < 0 {
				left = 0
			}
			return userErr("「%s」每人限购 %d 件，你还可以购买 %d 件", it.line.ProductName, it.line.LimitPerUser, left)
		}
	}
	return nil
}

type AdminOrderQuery struct {
	Status     string
	Keyword    string
	MerchantID uint64 // 0 = every merchant
}

// AdminOrders lists recent orders for the console.
func (s *Service) AdminOrders(q AdminOrderQuery) ([]OrderDetail, error) {
	db := s.st.DB.Model(&model.ShopOrder{})
	if q.Status != "" {
		db = db.Where("status = ?", q.Status)
	}
	if q.MerchantID != 0 {
		db = db.Where("merchant_id = ?", q.MerchantID)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		db = db.Where("order_no = ? OR telegram_user_id = ?", kw, kw)
	}
	var orders []model.ShopOrder
	if err := db.Order("id desc").Limit(500).Find(&orders).Error; err != nil {
		return nil, err
	}
	return s.withItems(orders)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
