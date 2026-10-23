package shop

import (
	"errors"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

// Points log types.
const (
	PointsEarn   = "earn"
	PointsSpend  = "spend"
	PointsRefund = "refund"
	PointsAdjust = "adjust"
	PointsRevoke = "revoke" // taken back after an after-sale refund
)

// Level is a member tier reached at Threshold of completed spend; Rate is the price multiplier.
type Level struct {
	Name      string `json:"name"`
	Threshold string `json:"threshold"`
	Rate      string `json:"rate"`
}

// levels parses shop_member_levels ("name:threshold:rate,…"), ignoring malformed entries.
func (s *Service) levels() []Level {
	out := []Level{}
	for _, part := range strings.Split(s.configString("shop_member_levels"), ",") {
		f := strings.Split(strings.TrimSpace(part), ":")
		if len(f) != 3 || strings.TrimSpace(f[0]) == "" {
			continue
		}
		th, e1 := decimal.NewFromString(strings.TrimSpace(f[1]))
		rate, e2 := decimal.NewFromString(strings.TrimSpace(f[2]))
		if e1 != nil || e2 != nil || th.IsNegative() || !rate.IsPositive() || rate.GreaterThan(decimal.NewFromInt(1)) {
			continue
		}
		out = append(out, Level{Name: strings.TrimSpace(f[0]), Threshold: th.StringFixed(6), Rate: rate.StringFixed(6)})
	}
	sort.Slice(out, func(i, j int) bool { return amount(out[i].Threshold).LessThan(amount(out[j].Threshold)) })
	return out
}

// levelFor returns the tier reached with spent (nil if none) and the next one (nil at the top).
func (s *Service) levelFor(spent decimal.Decimal) (current, next *Level) {
	ls := s.levels()
	for i := range ls {
		if spent.GreaterThanOrEqual(amount(ls[i].Threshold)) {
			current = &ls[i]
		} else {
			next = &ls[i]
			break
		}
	}
	return current, next
}

func (s *Service) pointsValue() decimal.Decimal {
	v, err := decimal.NewFromString(s.configString("shop_points_value"))
	if err != nil || v.IsNegative() {
		return decimal.Zero
	}
	return v
}

func (s *Service) pointsMaxRatio() decimal.Decimal {
	v, err := decimal.NewFromString(s.configString("shop_points_max_ratio"))
	if err != nil || v.IsNegative() {
		return decimal.Zero
	}
	return decimal.Min(v, decimal.NewFromInt(1))
}

// member reads a member row without locking; a missing row is an empty member.
func (s *Service) member(db *gorm.DB, userID uint64) (model.ShopMember, error) {
	m := model.ShopMember{UserID: userID, Spent: "0"}
	err := db.Where("user_id = ?", userID).First(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return m, nil
	}
	return m, err
}

// lockMember loads (creating if needed) and locks a member row inside tx.
func lockMember(tx *gorm.DB, userID uint64) (model.ShopMember, error) {
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ShopMember{UserID: userID, Spent: "0"}).Error; err != nil {
		return model.ShopMember{}, err
	}
	var m model.ShopMember
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", userID).First(&m).Error
	return m, err
}

// addPoints changes a member's points inside tx and logs it; the balance may not go negative.
func addPoints(tx *gorm.DB, userID uint64, delta int64, typ string, orderID uint64, remark, operator string) error {
	if delta == 0 {
		return nil
	}
	m, err := lockMember(tx, userID)
	if err != nil {
		return err
	}
	balance := m.Points + delta
	if balance < 0 {
		return userErr("积分不足，当前 %d", m.Points)
	}
	if err := tx.Model(&model.ShopMember{}).Where("user_id = ?", userID).Update("points", balance).Error; err != nil {
		return err
	}
	return tx.Create(&model.ShopPointsLog{UserID: userID, Delta: delta, Balance: balance, Type: typ, OrderID: orderID, Remark: remark, Operator: operator}).Error
}

// grantMemberTx counts a completed order towards the buyer's member spend (tier) and points, once,
// inside the caller's transaction (which has locked the order).
func (s *Service) grantMemberTx(tx *gorm.DB, o model.ShopOrder) error {
	if o.MemberGranted || o.Status != StatusCompleted {
		return nil
	}
	net := amount(o.PayAmount).Sub(amount(o.RefundedAmount))
	earned := int64(0)
	if net.IsPositive() {
		m, err := lockMember(tx, o.UserID)
		if err != nil {
			return err
		}
		if err := tx.Model(&model.ShopMember{}).Where("user_id = ?", o.UserID).Update("spent", amount(m.Spent).Add(net).StringFixed(6)).Error; err != nil {
			return err
		}
		if earned = net.Mul(decimal.NewFromInt(int64(s.configInt("shop_points_per_usdt", 10)))).Floor().IntPart(); earned > 0 {
			if err := addPoints(tx, o.UserID, earned, PointsEarn, o.ID, "商城订单 "+o.OrderNo, ""); err != nil {
				return err
			}
		}
	} else {
		net = decimal.Zero
	}
	return tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Updates(map[string]any{"member_granted": true, "member_spent": net.StringFixed(6), "points_earned": earned}).Error
}

// revokeMemberTx takes back the member spend and points a completed order earned for the part
// refunded after completion (points already spent are taken only as far as the balance goes).
func (s *Service) revokeMemberTx(tx *gorm.DB, o model.ShopOrder, refund decimal.Decimal) error {
	if !o.MemberGranted || !refund.IsPositive() {
		return nil
	}
	granted := amount(o.MemberSpent)
	if !granted.IsPositive() {
		return nil
	}
	back := decimal.Min(refund, granted)
	points := decimal.NewFromInt(o.PointsEarned).Mul(back).Div(granted).Ceil().IntPart()
	if points > o.PointsEarned {
		points = o.PointsEarned
	}
	m, err := lockMember(tx, o.UserID)
	if err != nil {
		return err
	}
	spent := amount(m.Spent).Sub(back)
	if spent.IsNegative() {
		spent = decimal.Zero
	}
	if err := tx.Model(&model.ShopMember{}).Where("user_id = ?", o.UserID).Update("spent", spent.StringFixed(6)).Error; err != nil {
		return err
	}
	if take := min(points, m.Points); take > 0 {
		if err := addPoints(tx, o.UserID, -take, PointsRevoke, o.ID, "售后退款扣回 "+o.OrderNo, ""); err != nil {
			return err
		}
	}
	return tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Updates(map[string]any{"member_spent": granted.Sub(back).StringFixed(6), "points_earned": o.PointsEarned - points}).Error
}

// MemberInfo is the buyer's points, tier and progress to the next tier.
type MemberInfo struct {
	Points      int64  `json:"points"`
	PointsValue string `json:"points_value"` // USDT per point
	Spent       string `json:"spent"`
	Level       *Level `json:"level"`
	Next        *Level `json:"next"`
}

func (s *Service) Member(userID uint64) (*MemberInfo, error) {
	m, err := s.member(s.st.DB, userID)
	if err != nil {
		return nil, err
	}
	cur, next := s.levelFor(amount(m.Spent))
	return &MemberInfo{Points: m.Points, PointsValue: s.pointsValue().StringFixed(6), Spent: amount(m.Spent).StringFixed(6), Level: cur, Next: next}, nil
}

func (s *Service) PointsLog(userID uint64) ([]model.ShopPointsLog, error) {
	rows := []model.ShopPointsLog{}
	err := s.st.DB.Where("user_id = ?", userID).Order("id desc").Limit(100).Find(&rows).Error
	return rows, err
}

// orderDiscounts works out the tier and points discounts for a group after its coupon discount.
// usePoints spends as many points as allowed; points is the buyer's balance.
func (s *Service) orderDiscounts(spent decimal.Decimal, points int64, goods, coupon decimal.Decimal, usePoints bool) (level string, member decimal.Decimal, pointsUsed int64, pointsDiscount decimal.Decimal) {
	if cur, _ := s.levelFor(spent); cur != nil {
		level = cur.Name
		member = goods.Sub(goods.Mul(amount(cur.Rate))).Truncate(6)
	}
	remaining := goods.Sub(member).Sub(coupon)
	if remaining.IsNegative() {
		// The coupon already covers the rest; the tier discount gives way.
		member = decimal.Max(decimal.Zero, goods.Sub(coupon))
		remaining = decimal.Zero
	}
	value := s.pointsValue()
	if usePoints && value.IsPositive() && points > 0 && remaining.IsPositive() {
		maxPoints := remaining.Mul(s.pointsMaxRatio()).Div(value).Floor().IntPart()
		pointsUsed = min(points, maxPoints)
		pointsDiscount = value.Mul(decimal.NewFromInt(pointsUsed)).Truncate(6)
	}
	return level, member, pointsUsed, pointsDiscount
}

// ---- Administration ----

type AdminMember struct {
	ID             uint64 `json:"id"` // the user ID, so console rows have an id
	UserID         uint64 `json:"user_id"`
	TelegramUserID int64  `json:"telegram_user_id"`
	Name           string `json:"name"`
	Points         int64  `json:"points"`
	Spent          string `json:"spent"`
	Level          string `json:"level"`
}

// AdminMembers lists members with points or spend, highest points first.
func (s *Service) AdminMembers() ([]AdminMember, error) {
	var rows []struct {
		model.ShopMember
		TelegramUserID int64
		FirstName      string
		Username       string
	}
	if err := s.st.DB.Table("shop_members").Select("shop_members.*, telegram_users.telegram_user_id, telegram_users.first_name, telegram_users.username").
		Joins("JOIN telegram_users ON telegram_users.id = shop_members.user_id").Order("shop_members.points desc, shop_members.spent desc").Limit(500).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]AdminMember, 0, len(rows))
	for _, r := range rows {
		name := r.FirstName
		if r.Username != "" {
			name += " @" + r.Username
		}
		m := AdminMember{ID: r.UserID, UserID: r.UserID, TelegramUserID: r.TelegramUserID, Name: strings.TrimSpace(name), Points: r.Points, Spent: amount(r.Spent).StringFixed(6)}
		if cur, _ := s.levelFor(amount(r.Spent)); cur != nil {
			m.Level = cur.Name
		}
		out = append(out, m)
	}
	return out, nil
}

// AdjustPoints adds (or with a negative delta, deducts) points by Telegram ID with a reason.
func (s *Service) AdjustPoints(telegramUserID, delta int64, reason, operator string) error {
	if delta == 0 {
		return userErr("请填写调整的积分数量")
	}
	if err := checkLen(reason, "调整原因", 255, true); err != nil {
		return err
	}
	var u model.TelegramUser
	if err := s.st.DB.Select("id").Where("telegram_user_id = ?", telegramUserID).First(&u).Error; err != nil {
		return userErr("用户不存在（需先使用过机器人）")
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		return addPoints(tx, u.ID, delta, PointsAdjust, 0, strings.TrimSpace(reason), operator)
	})
}
