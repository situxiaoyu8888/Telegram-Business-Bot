package shop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

// StatusGrouping is an order paid into a group that has not filled up yet.
const StatusGrouping = "grouping"

// Group statuses.
const (
	GroupOpen    = "open"
	GroupSuccess = "success"
	GroupFailed  = "failed"
)

// groupPrice is a SKU's group-buying price (regular price × the product's group rate).
func groupPrice(l line) decimal.Decimal {
	rate := amount(l.GroupRate)
	if !rate.IsPositive() || rate.GreaterThan(decimal.NewFromInt(1)) {
		rate = decimal.NewFromInt(1)
	}
	return amount(l.ListPrice).Mul(rate).Truncate(6)
}

// applyGroupPrice checks a group checkout (one SKU of a group-buying product) and reprices it.
func applyGroupPrice(items []pricedItem) (decimal.Decimal, error) {
	if len(items) != 1 {
		return decimal.Zero, userErr("拼团每次只能购买一种商品")
	}
	l := &items[0].line
	if l.GroupSize < 2 {
		return decimal.Zero, userErr("该商品不支持拼团")
	}
	l.Price, l.OnPromo = groupPrice(*l).StringFixed(6), false
	return amount(l.Price).Mul(decimal.NewFromInt(items[0].Quantity)), nil
}

// joinGroup starts or joins a group for an order being created inside tx and returns the group.
// It reports whether this order filled the group.
func (s *Service) joinGroup(tx *gorm.DB, b Buyer, g CheckoutGroup, l line) (model.ShopGroup, bool, error) {
	now := time.Now()
	if g.GroupMode == "start" {
		hours := l.GroupHours
		if hours <= 0 {
			hours = 24
		}
		grp := model.ShopGroup{ProductID: l.ProductID, LeaderUserID: b.UserID, Size: l.GroupSize, Joined: 1, Status: GroupOpen, ExpiresAt: now.Add(time.Duration(hours) * time.Hour)}
		return grp, false, tx.Create(&grp).Error
	}
	var grp model.ShopGroup
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&grp, g.GroupID).Error; err != nil {
		return grp, false, userErr("拼团不存在")
	}
	if grp.ProductID != l.ProductID {
		return grp, false, userErr("拼团商品不一致")
	}
	if grp.Status != GroupOpen || !now.Before(grp.ExpiresAt) || grp.Joined >= grp.Size {
		return grp, false, userErr("该拼团已结束，可以自己发起一个新的拼团")
	}
	var mine int64
	if err := tx.Model(&model.ShopOrder{}).Where("group_id = ? AND user_id = ? AND status <> ?", grp.ID, b.UserID, StatusCanceled).Count(&mine).Error; err != nil {
		return grp, false, err
	}
	if mine > 0 {
		return grp, false, userErr("你已经在这个拼团里了")
	}
	grp.Joined++
	updates := map[string]any{"joined": grp.Joined}
	full := grp.Joined >= grp.Size
	if full {
		grp.Status, grp.ClosedAt = GroupSuccess, &now
		updates["status"], updates["closed_at"] = GroupSuccess, now
	}
	return grp, full, tx.Model(&model.ShopGroup{}).Where("id = ?", grp.ID).Updates(updates).Error
}

// completeGroup turns a filled group's waiting orders into normal paid orders inside tx.
func completeGroup(tx *gorm.DB, groupID uint64) error {
	return tx.Model(&model.ShopOrder{}).Where("group_id = ? AND status = ?", groupID, StatusGrouping).Update("status", StatusPaid).Error
}

// notifyGroupSuccess tells every member that their group filled up.
func (s *Service) notifyGroupSuccess(groupID uint64) {
	var orders []model.ShopOrder
	if s.st.DB.Where("group_id = ? AND status <> ?", groupID, StatusCanceled).Find(&orders).Error != nil {
		return
	}
	for _, o := range orders {
		items := s.itemsText(o.ID)
		s.notify(o.BotID, o.TelegramUserID, fmt.Sprintf("🎉 <b>拼团成功</b>\n\n订单号：<code>%s</code>%s\n\n商家会尽快为你处理。", o.OrderNo, items), o.ID)
		s.notifyStaff(o.MerchantID, fmt.Sprintf("🛒 <b>拼团成团，订单待处理</b>\n\n订单号：<code>%s</code>\n商家：%s%s\n配送：%s", o.OrderNo, htmlEscape(o.MerchantName), items, deliveryNames[o.DeliveryType]))
	}
}

// leaveGroup removes a canceled waiting order from its open group inside tx; an empty group fails.
func leaveGroup(tx *gorm.DB, groupID uint64) error {
	var grp model.ShopGroup
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&grp, groupID).Error; err != nil {
		return err
	}
	if grp.Status != GroupOpen {
		return nil
	}
	updates := map[string]any{"joined": gorm.Expr("joined - 1")}
	if grp.Joined <= 1 {
		updates["status"], updates["closed_at"] = GroupFailed, time.Now()
	}
	return tx.Model(&model.ShopGroup{}).Where("id = ?", grp.ID).Updates(updates).Error
}

// ExpireGroups fails open groups past their deadline and refunds every waiting order.
func (s *Service) ExpireGroups(ctx context.Context, now time.Time) error {
	// Orders of groups that already failed but were not refunded (an earlier pass stopped on an
	// error) are refunded first, so no buyer's money stays held.
	var stuck []uint64
	if err := s.st.DB.WithContext(ctx).Model(&model.ShopOrder{}).
		Where("status = ? AND group_id IN (?)", StatusGrouping, s.st.DB.Model(&model.ShopGroup{}).Select("id").Where("status = ?", GroupFailed)).
		Limit(200).Pluck("id", &stuck).Error; err != nil {
		return err
	}
	for _, oid := range stuck {
		if err := s.cancel(oid, Scope{}, "拼团未成团，已自动退款", autoOperator); err != nil && !IsUserError(err) {
			s.logger.Error("refund failed group order", zap.Uint64("order_id", oid), zap.Error(err))
		}
	}
	var ids []uint64
	if err := s.st.DB.WithContext(ctx).Model(&model.ShopGroup{}).Where("status = ? AND expires_at <= ?", GroupOpen, now).Limit(200).Pluck("id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var orderIDs []uint64
		err := s.st.DB.Transaction(func(tx *gorm.DB) error {
			var grp model.ShopGroup
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&grp, id).Error; err != nil {
				return err
			}
			if grp.Status != GroupOpen || grp.ExpiresAt.After(now) {
				return nil
			}
			if err := tx.Model(&model.ShopGroup{}).Where("id = ?", id).Updates(map[string]any{"status": GroupFailed, "closed_at": now}).Error; err != nil {
				return err
			}
			return tx.Model(&model.ShopOrder{}).Where("group_id = ? AND status = ?", id, StatusGrouping).Pluck("id", &orderIDs).Error
		})
		if err != nil {
			return err
		}
		for _, oid := range orderIDs {
			// One failing refund does not hold up the rest; the sweep above retries it next pass.
			if err := s.cancel(oid, Scope{}, "拼团未成团，已自动退款", autoOperator); err != nil && !IsUserError(err) {
				s.logger.Error("refund failed group order", zap.Uint64("order_id", oid), zap.Error(err))
			}
		}
	}
	return nil
}

// GroupView is a group as shown to buyers.
type GroupView struct {
	model.ShopGroup
	Product    ProductCard `json:"product"`
	GroupPrice string      `json:"group_price"`
	Members    []string    `json:"members"` // masked names, leader first
	Mine       bool        `json:"mine"`
	MyOrderID  uint64      `json:"my_order_id,omitempty"`
}

func (s *Service) Group(userID, id uint64) (*GroupView, error) {
	var grp model.ShopGroup
	if err := s.st.DB.First(&grp, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, userErr("拼团不存在")
		}
		return nil, err
	}
	return s.groupView(userID, grp)
}

func (s *Service) groupView(userID uint64, grp model.ShopGroup) (*GroupView, error) {
	v := &GroupView{ShopGroup: grp, Members: []string{}}
	cards, err := s.productsByIDs([]uint64{grp.ProductID})
	if err != nil {
		return nil, err
	}
	if len(cards) > 0 {
		v.Product = cards[0]
	}
	var p model.ShopProduct
	if err := s.st.DB.Select("group_rate").First(&p, grp.ProductID).Error; err == nil && v.Product.ID != 0 {
		rate := amount(p.GroupRate)
		if !rate.IsPositive() || rate.GreaterThan(decimal.NewFromInt(1)) {
			rate = decimal.NewFromInt(1)
		}
		v.GroupPrice = amount(v.Product.ListPrice).Mul(rate).Truncate(6).StringFixed(6)
	}
	var members []struct {
		ID     uint64
		UserID uint64
	}
	if err := s.st.DB.Model(&model.ShopOrder{}).Select("id, user_id").Where("group_id = ? AND status <> ?", grp.ID, StatusCanceled).Order("id asc").Scan(&members).Error; err != nil {
		return nil, err
	}
	for _, m := range members {
		var u model.TelegramUser
		if s.st.DB.Select("first_name", "username").First(&u, m.UserID).Error == nil {
			v.Members = append(v.Members, maskName(u))
		}
		if m.UserID == userID {
			v.Mine, v.MyOrderID = true, m.ID
		}
	}
	return v, nil
}

// OpenGroups lists a product's groups that can still be joined, closest to filling first.
func (s *Service) OpenGroups(userID, productID uint64) ([]GroupView, error) {
	var groups []model.ShopGroup
	if err := s.st.DB.Where("product_id = ? AND status = ? AND expires_at > ?", productID, GroupOpen, time.Now()).Order("(size - joined) asc, expires_at asc").Limit(10).Find(&groups).Error; err != nil {
		return nil, err
	}
	out := []GroupView{}
	for _, g := range groups {
		v, err := s.groupView(userID, g)
		if err != nil {
			return nil, err
		}
		out = append(out, *v)
	}
	return out, nil
}
