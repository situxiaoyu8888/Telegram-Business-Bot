package shop

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

// Coupon statuses held by users.
const (
	CouponUnused = "unused"
	CouponUsed   = "used"
)

// UserCoupon is a held coupon with its template, as shown in the H5 shop.
type UserCoupon struct {
	model.ShopUserCoupon
	Name         string `json:"name"`
	MerchantID   uint64 `json:"merchant_id"`
	MerchantName string `json:"merchant_name"`
	Threshold    string `json:"threshold"`
	Discount     string `json:"discount"`
	Expired      bool   `json:"expired"`
	Enabled      bool   `json:"-"`
}

// CenterCoupon is a claimable coupon with how many the buyer already holds.
type CenterCoupon struct {
	model.ShopCoupon
	MerchantName string `json:"merchant_name"`
	Held         int64  `json:"held"`
	SoldOut      bool   `json:"sold_out"`
}

func (s *Service) merchantNames(ids []uint64) (map[uint64]string, error) {
	names := map[uint64]string{}
	if len(ids) == 0 {
		return names, nil
	}
	var ms []model.Merchant
	err := s.st.DB.Select("id", "name").Where("id IN ?", ids).Find(&ms).Error
	for _, m := range ms {
		names[m.ID] = m.Name
	}
	return names, err
}

func (s *Service) MyCoupons(userID uint64) ([]UserCoupon, error) {
	var held []model.ShopUserCoupon
	if err := s.st.DB.Where("user_id = ?", userID).Order("id desc").Limit(200).Find(&held).Error; err != nil {
		return nil, err
	}
	ids := []uint64{}
	for _, h := range held {
		ids = append(ids, h.CouponID)
	}
	var templates []model.ShopCoupon
	if len(ids) > 0 {
		if err := s.st.DB.Where("id IN ?", ids).Find(&templates).Error; err != nil {
			return nil, err
		}
	}
	byID := map[uint64]model.ShopCoupon{}
	merchantIDs := []uint64{}
	for _, t := range templates {
		byID[t.ID] = t
		merchantIDs = append(merchantIDs, t.MerchantID)
	}
	names, err := s.merchantNames(merchantIDs)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := []UserCoupon{}
	for _, h := range held {
		t, ok := byID[h.CouponID]
		if !ok {
			continue
		}
		out = append(out, UserCoupon{ShopUserCoupon: h, Name: t.Name, MerchantID: t.MerchantID, MerchantName: names[t.MerchantID], Threshold: t.Threshold, Discount: t.Discount, Expired: h.ExpiresAt != nil && h.ExpiresAt.Before(now), Enabled: t.Enabled})
	}
	return out, nil
}

func (s *Service) CouponCenter(userID uint64) ([]CenterCoupon, error) {
	var templates []model.ShopCoupon
	if err := s.st.DB.Where("enabled = ? AND claimable = ? AND (ends_at IS NULL OR ends_at > ?)", true, true, time.Now()).Order("id desc").Find(&templates).Error; err != nil {
		return nil, err
	}
	var held []struct {
		CouponID uint64
		N        int64
	}
	if err := s.st.DB.Model(&model.ShopUserCoupon{}).Select("coupon_id, count(*) as n").Where("user_id = ?", userID).Group("coupon_id").Scan(&held).Error; err != nil {
		return nil, err
	}
	heldBy := map[uint64]int64{}
	for _, h := range held {
		heldBy[h.CouponID] = h.N
	}
	merchantIDs := []uint64{}
	for _, t := range templates {
		merchantIDs = append(merchantIDs, t.MerchantID)
	}
	names, err := s.merchantNames(merchantIDs)
	if err != nil {
		return nil, err
	}
	out := []CenterCoupon{}
	for _, t := range templates {
		out = append(out, CenterCoupon{ShopCoupon: t, MerchantName: names[t.MerchantID], Held: heldBy[t.ID], SoldOut: t.TotalQty > 0 && t.IssuedQty >= t.TotalQty})
	}
	return out, nil
}

// Claim gives the buyer one coupon from the coupon center.
func (s *Service) Claim(userID, couponID uint64) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		return s.grant(tx, userID, couponID, true)
	})
}

// grant issues one coupon, enforcing the total and per-user limits under a lock on the template.
func (s *Service) grant(tx *gorm.DB, userID, couponID uint64, selfClaim bool) error {
	var c model.ShopCoupon
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, couponID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return userErr("优惠券不存在")
		}
		return err
	}
	now := time.Now()
	if !c.Enabled || (selfClaim && !c.Claimable) || (c.EndsAt != nil && !c.EndsAt.After(now)) {
		return userErr("优惠券已停止发放")
	}
	if c.TotalQty > 0 && c.IssuedQty >= c.TotalQty {
		return userErr("优惠券已领完")
	}
	if c.PerUserLimit > 0 {
		var n int64
		if err := tx.Model(&model.ShopUserCoupon{}).Where("user_id = ? AND coupon_id = ?", userID, couponID).Count(&n).Error; err != nil {
			return err
		}
		if n >= c.PerUserLimit {
			return userErr("每人限领 %d 张", c.PerUserLimit)
		}
	}
	var expires *time.Time
	if c.ValidDays > 0 {
		t := now.AddDate(0, 0, c.ValidDays)
		expires = &t
	}
	if c.EndsAt != nil && (expires == nil || c.EndsAt.Before(*expires)) {
		t := *c.EndsAt
		expires = &t
	}
	if err := tx.Model(&model.ShopCoupon{}).Where("id = ?", c.ID).Update("issued_qty", gorm.Expr("issued_qty + 1")).Error; err != nil {
		return err
	}
	return tx.Create(&model.ShopUserCoupon{CouponID: c.ID, UserID: userID, Status: CouponUnused, ExpiresAt: expires}).Error
}

// ---- Administration ----

func (s *Service) AdminCoupons() ([]model.ShopCoupon, error) {
	rows := []model.ShopCoupon{}
	err := s.st.DB.Order("id desc").Find(&rows).Error
	return rows, err
}

func (s *Service) SaveCoupon(c model.ShopCoupon) error {
	c.Name = strings.TrimSpace(c.Name)
	if err := checkLen(c.Name, "优惠券名称", 64, true); err != nil {
		return err
	}
	threshold, err := parseAmount(c.Threshold, "使用门槛")
	if err != nil {
		return err
	}
	discount, err := parseAmount(c.Discount, "优惠金额")
	if err != nil {
		return err
	}
	if !discount.IsPositive() {
		return userErr("优惠金额必须大于 0")
	}
	if c.TotalQty < 0 || c.PerUserLimit < 0 || c.ValidDays < 0 {
		return userErr("数量和有效天数不能为负数")
	}
	if c.ValidDays == 0 && c.EndsAt == nil {
		return userErr("请设置领取后有效天数或截止时间")
	}
	if c.MerchantID > 0 {
		if err := s.st.DB.Select("id").First(&model.Merchant{}, c.MerchantID).Error; err != nil {
			return userErr("请选择有效的商家")
		}
	}
	c.Threshold, c.Discount = threshold.StringFixed(6), discount.StringFixed(6)
	if c.ID == 0 {
		c.IssuedQty = 0
		return s.st.DB.Create(&c).Error
	}
	// Amounts and the merchant are fixed once created, so coupons already held keep their meaning.
	res := s.st.DB.Model(&model.ShopCoupon{}).Where("id = ?", c.ID).Updates(map[string]any{"name": c.Name, "total_qty": c.TotalQty, "per_user_limit": c.PerUserLimit, "valid_days": c.ValidDays, "ends_at": c.EndsAt, "claimable": c.Claimable, "enabled": c.Enabled})
	if res.Error == nil && res.RowsAffected == 0 {
		return userErr("优惠券不存在")
	}
	return res.Error
}

// IssueCoupon gives one coupon to each listed Telegram user who has used a bot. It returns how many
// were issued; users who are unknown or already at the per-user limit are skipped.
func (s *Service) IssueCoupon(couponID uint64, telegramIDs []int64) (issued int, skipped []int64, err error) {
	for _, tgID := range telegramIDs {
		var u model.TelegramUser
		if e := s.st.DB.Select("id").Where("telegram_user_id = ?", tgID).First(&u).Error; e != nil {
			if errors.Is(e, gorm.ErrRecordNotFound) {
				skipped = append(skipped, tgID)
				continue
			}
			return issued, skipped, e
		}
		e := s.st.DB.Transaction(func(tx *gorm.DB) error { return s.grant(tx, u.ID, couponID, false) })
		if IsUserError(e) {
			if strings.Contains(e.Error(), "限领") {
				skipped = append(skipped, tgID)
				continue
			}
			return issued, skipped, e
		}
		if e != nil {
			return issued, skipped, e
		}
		issued++
	}
	return issued, skipped, nil
}
