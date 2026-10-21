package shop

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
)

// GrantCommissions pays shop referral commissions to the direct referrer of each buyer, once an
// order has completed and its after-sale period has passed, so the amount (paid minus refunds)
// can no longer change. Orders are marked done whether or not a commission was due, so turning
// the rate on later does not pay for old orders.
func (s *Service) GrantCommissions(ctx context.Context, now time.Time) error {
	rate, err := decimal.NewFromString(s.configString("shop_referral_rate"))
	if err != nil || rate.IsNegative() || rate.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		rate = decimal.Zero
	}
	cutoff := now.AddDate(0, 0, -s.configInt("shop_aftersale_days", 7))
	var ids []uint64
	if err := s.st.DB.WithContext(ctx).Model(&model.ShopOrder{}).
		Where("status = ? AND commission_done = ? AND after_sale_open = ? AND completed_at <= ?", StatusCompleted, false, false, cutoff).
		Order("id asc").Limit(500).Pluck("id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.grantCommission(id, rate); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) grantCommission(orderID uint64, rate decimal.Decimal) error {
	var parent model.TelegramUser
	var commission decimal.Decimal
	var o model.ShopOrder
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if o, err = lockOrder(tx, orderID, Scope{}); err != nil {
			return err
		}
		if o.Status != StatusCompleted || o.CommissionDone || o.AfterSaleOpen {
			return nil
		}
		if err := tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Update("commission_done", true).Error; err != nil {
			return err
		}
		var buyer model.TelegramUser
		if err := tx.First(&buyer, o.UserID).Error; err != nil {
			return err
		}
		if buyer.ParentUserID == nil || !rate.IsPositive() {
			return nil
		}
		commission = amount(o.PayAmount).Sub(amount(o.RefundedAmount)).Mul(rate).Truncate(6)
		if !commission.IsPositive() {
			return nil
		}
		if err := tx.First(&parent, *buyer.ParentUserID).Error; err != nil {
			return err
		}
		rc := model.ReferralCommission{AgentUserID: parent.ID, FromUserID: buyer.ID, SourceType: "shop_order", SourceID: o.ID, Rate: rate.StringFixed(6), Amount: commission.StringFixed(6), Status: "credited"}
		if err := tx.Create(&rc).Error; err != nil {
			return err
		}
		return s.wallet.Credit(tx, parent.ID, commission, "referral_commission", "referral_commission", rc.ID, "商城推广佣金 "+o.OrderNo)
	})
	if err == nil && commission.IsPositive() {
		s.notify(0, parent.TelegramUserID, fmt.Sprintf("🎁 <b>推广佣金到账</b>\n\n你推荐的好友在商城的订单已完成，佣金 <code>%s USDT</code> 已计入钱包余额。", commission.String()), 0)
	}
	return err
}
