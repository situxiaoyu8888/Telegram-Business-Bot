// Package redpacket implements red packets: a user locks an amount of one wallet currency, the
// bot shares it in a group or through inline mode, and others claim shares into their wallets.
package redpacket

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

// Packet types.
const (
	TypeLucky     = "lucky"     // a total split at random
	TypeEqual     = "equal"     // equal shares
	TypeExclusive = "exclusive" // one share for one named user
	TypePassword  = "password"  // a total split at random, claimed by sending the password
)

// Packet statuses.
const (
	StatusActive   = "active"
	StatusFinished = "finished"
	StatusExpired  = "expired"
)

// DefaultGreeting is used when the sender gives none.
const DefaultGreeting = "恭喜发财，大吉大利"

// TypeNames are the Chinese names of the packet types.
var TypeNames = map[string]string{TypeLucky: "拼手气", TypeEqual: "普通", TypeExclusive: "专属", TypePassword: "口令"}

// UserError carries a message meant for the person who made the request.
type UserError struct{ Msg string }

func (e UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return UserError{fmt.Sprintf(format, args...)} }

// Notifier sends a message to a user through the bot with the given ID (0 = primary bot).
type Notifier func(ctx context.Context, botID uint64, telegramUserID int64, html string) error

type Service struct {
	st     *store.Store
	wallet *wallet.Service
	notify Notifier
	logger *zap.Logger
	now    func() time.Time
}

func New(st *store.Store, w *wallet.Service, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{st: st, wallet: w, logger: logger, now: time.Now}
}

// WithNotifier sets how senders are told about refunds of expired packets.
func (s *Service) WithNotifier(n Notifier) *Service { s.notify = n; return s }

// CreateInput describes a new packet. Amount is the total for lucky and password packets and
// the amount of each share for equal and exclusive packets.
type CreateInput struct {
	BotID            uint64
	SenderUserID     uint64
	SenderTelegramID int64
	SenderName       string
	Type             string
	Currency         string
	Amount           string
	Count            int
	Target           string // exclusive: @username or Telegram ID
	Password         string
	Greeting         string
	ChatID           int64
	RequestID        string
}

var usernamePattern = regexp.MustCompile(`^@?([A-Za-z][A-Za-z0-9_]{3,31})$`)

// ParseTarget accepts "@username" or a numeric Telegram ID for an exclusive packet.
func ParseTarget(s string) (username string, id int64, err error) {
	s = strings.TrimSpace(s)
	if n, e := strconv.ParseInt(s, 10, 64); e == nil && n > 0 {
		return "", n, nil
	}
	if m := usernamePattern.FindStringSubmatch(s); m != nil {
		return m[1], 0, nil
	}
	return "", 0, userErr("请输入对方的 @用户名 或数字 Telegram ID")
}

func (s *Service) configInt(key string, def, min, max int) int {
	var c model.SystemConfig
	if err := s.st.DB.Where("`key` = ?", key).First(&c).Error; err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(c.Value))
	if err != nil || n < min || n > max {
		return def
	}
	return n
}

// MaxCount is the most shares one packet may have.
func (s *Service) MaxCount() int { return s.configInt("redpacket_max_count", 100, 1, 1000) }

// ExpireHours is how long a packet stays claimable.
func (s *Service) ExpireHours() int { return s.configInt("redpacket_expire_hours", 24, 1, 168) }

// Create debits the sender and records the packet with its shares. It is idempotent per request ID.
func (s *Service) Create(in CreateInput) (*model.RedPacket, error) {
	if in.RequestID == "" || len(in.RequestID) > 64 {
		return nil, fmt.Errorf("red packet request ID required")
	}
	if _, ok := TypeNames[in.Type]; !ok {
		return nil, userErr("请选择红包类型")
	}
	cur, err := s.wallet.Currency(in.Currency)
	if err != nil {
		return nil, err
	}
	unit := decimal.New(1, -int32(cur.Decimals))
	amount, err := decimal.NewFromString(strings.TrimSpace(in.Amount))
	if err != nil || !amount.IsPositive() {
		return nil, userErr("请输入正确的金额")
	}
	if !amount.Equal(amount.Truncate(int32(cur.Decimals))) {
		return nil, userErr("%s 金额最多 %d 位小数", cur.Name, cur.Decimals)
	}
	if in.Type == TypeExclusive {
		in.Count = 1
	}
	if max := s.MaxCount(); in.Count < 1 || in.Count > max {
		return nil, userErr("红包个数需在 1–%d 之间", max)
	}
	p := model.RedPacket{BotID: in.BotID, SenderUserID: in.SenderUserID, SenderTelegramID: in.SenderTelegramID, SenderName: truncate(in.SenderName, 128),
		Type: in.Type, Currency: cur.Code, Count: in.Count, ClaimedAmount: "0", RefundAmount: "0", ChatID: in.ChatID, Status: StatusActive, RequestID: in.RequestID}
	total := amount
	if in.Type == TypeEqual || in.Type == TypeExclusive {
		total = amount.Mul(decimal.NewFromInt(int64(in.Count)))
	}
	if total.GreaterThan(decimal.New(1, 12)) {
		return nil, userErr("金额过大")
	}
	units := total.Div(unit).IntPart()
	if units < int64(in.Count) {
		return nil, userErr("金额不足以分成 %d 份（每份至少 %s）", in.Count, unit.String())
	}
	var shares []int64
	if in.Type == TypeLucky || in.Type == TypePassword {
		if shares, err = splitLucky(units, in.Count); err != nil {
			return nil, err
		}
	} else {
		per := units / int64(in.Count)
		for i := 0; i < in.Count; i++ {
			shares = append(shares, per)
		}
	}
	list := make([]string, len(shares))
	for i, u := range shares {
		list[i] = decimal.NewFromInt(u).Mul(unit).StringFixed(6)
	}
	raw, _ := json.Marshal(list)
	p.Shares, p.Total = string(raw), total.StringFixed(6)
	switch in.Type {
	case TypeExclusive:
		if p.TargetUsername, p.TargetTelegramID, err = ParseTarget(in.Target); err != nil {
			return nil, err
		}
	case TypePassword:
		p.Password = strings.TrimSpace(in.Password)
		if n := utf8.RuneCountInString(p.Password); n < 1 || n > 20 || strings.HasPrefix(p.Password, "/") {
			return nil, userErr("口令需为 1–20 个字，且不能以 / 开头")
		}
	}
	p.Greeting = strings.TrimSpace(in.Greeting)
	if p.Greeting == "" {
		p.Greeting = DefaultGreeting
	}
	if utf8.RuneCountInString(p.Greeting) > 40 {
		return nil, userErr("祝福语最多 40 个字")
	}
	p.Code, err = newCode()
	if err != nil {
		return nil, err
	}
	p.ExpiresAt = s.now().Add(time.Duration(s.ExpireHours()) * time.Hour)
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		var prev model.RedPacket
		if err := tx.Where("request_id = ?", in.RequestID).First(&prev).Error; err == nil {
			p = prev
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Create(&p).Error; err != nil {
			if store.IsDuplicate(err) {
				return userErr("红包正在创建，请勿重复提交")
			}
			return err
		}
		remark := TypeNames[p.Type] + "红包"
		if err := s.wallet.AdjustCurrency(tx, p.SenderUserID, p.Currency, total.Neg(), "redpacket_send", "redpacket", p.ID, remark); err != nil {
			if errors.Is(err, wallet.ErrInsufficientBalance) {
				return userErr("%s 可用余额不足", cur.Name)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// splitLucky splits units into n random positive shares ("double average": each share is at
// most twice the average of what is left, so early claimers have no advantage on average).
func splitLucky(units int64, n int) ([]int64, error) {
	out := make([]int64, 0, n)
	left := units
	for i := n; i > 1; i-- {
		max := left * 2 / int64(i)
		if limit := left - int64(i-1); max > limit {
			max = limit
		}
		share := int64(1)
		if max > 1 {
			r, err := rand.Int(rand.Reader, big.NewInt(max))
			if err != nil {
				return nil, err
			}
			share = r.Int64() + 1
		}
		out = append(out, share)
		left -= share
	}
	return append(out, left), nil
}

func newCode() (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 12)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ByCode returns a packet and its claims in claim order.
func (s *Service) ByCode(code string) (*model.RedPacket, []model.RedPacketClaim, error) {
	var p model.RedPacket
	if err := s.st.DB.Where("code = ?", code).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, userErr("红包不存在")
		}
		return nil, nil, err
	}
	claims := []model.RedPacketClaim{}
	err := s.st.DB.Where("packet_id = ?", p.ID).Order("id asc").Find(&claims).Error
	return &p, claims, err
}

// Claimer is the user claiming a share.
type Claimer struct {
	UserID         uint64
	TelegramUserID int64
	Username       string
	Name           string
}

// Claim takes the next share of the packet for the claimer and credits it to their wallet.
// Password packets are claimed only through ClaimByPassword.
func (s *Service) Claim(code string, c Claimer) (*model.RedPacketClaim, *model.RedPacket, error) {
	return s.claim(code, c, false)
}

func (s *Service) claim(code string, c Claimer, viaPassword bool) (*model.RedPacketClaim, *model.RedPacket, error) {
	var claim model.RedPacketClaim
	var p model.RedPacket
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("code = ?", code).First(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return userErr("红包不存在")
			}
			return err
		}
		var prev model.RedPacketClaim
		if err := tx.Where("packet_id = ? AND user_id = ?", p.ID, c.UserID).First(&prev).Error; err == nil {
			claim = prev
			return errAlreadyClaimed
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		switch {
		case p.Status == StatusFinished || p.ClaimedCount >= p.Count:
			return userErr("手慢了，红包已被领完")
		case p.Status == StatusExpired || !s.now().Before(p.ExpiresAt):
			return userErr("红包已过期")
		case p.Type == TypePassword && !viaPassword:
			return userErr("这是口令红包，请发送口令「%s」领取", p.Password)
		case p.Type == TypeExclusive && !isTarget(p, c):
			return userErr("这是专属红包，只有指定的人可以领取")
		}
		var shares []string
		if err := json.Unmarshal([]byte(p.Shares), &shares); err != nil || len(shares) != p.Count {
			return fmt.Errorf("red packet %d: bad shares", p.ID)
		}
		amount, _ := decimal.NewFromString(shares[p.ClaimedCount])
		claim = model.RedPacketClaim{PacketID: p.ID, UserID: c.UserID, TelegramUserID: c.TelegramUserID, Name: truncate(c.Name, 128), Amount: amount.StringFixed(6)}
		if err := tx.Create(&claim).Error; err != nil {
			return err
		}
		claimed, _ := decimal.NewFromString(p.ClaimedAmount)
		p.ClaimedCount++
		p.ClaimedAmount = claimed.Add(amount).StringFixed(6)
		updates := map[string]any{"claimed_count": p.ClaimedCount, "claimed_amount": p.ClaimedAmount}
		if p.ClaimedCount >= p.Count {
			now := s.now()
			p.Status, p.FinishedAt = StatusFinished, &now
			updates["status"], updates["finished_at"] = p.Status, now
		}
		if err := tx.Model(&model.RedPacket{}).Where("id = ?", p.ID).Updates(updates).Error; err != nil {
			return err
		}
		return s.wallet.AdjustCurrency(tx, c.UserID, p.Currency, amount, "redpacket_claim", "redpacket", p.ID, "领取红包："+truncate(p.SenderName, 40))
	})
	if errors.Is(err, errAlreadyClaimed) {
		return &claim, &p, userErr("你已经领过这个红包了")
	}
	if err != nil {
		return nil, nil, err
	}
	return &claim, &p, nil
}

var errAlreadyClaimed = errors.New("already claimed")

func isTarget(p model.RedPacket, c Claimer) bool {
	if p.TargetTelegramID != 0 {
		return p.TargetTelegramID == c.TelegramUserID
	}
	return c.Username != "" && strings.EqualFold(p.TargetUsername, c.Username)
}

// ClaimByPassword claims the newest claimable password packet whose password is text. chatID
// limits the search to packets posted in that group; 0 searches every packet (a private chat
// with the bot, for packets shared through inline mode). matched is false when no active packet
// has that password, so the caller can treat the text as an ordinary message.
func (s *Service) ClaimByPassword(chatID int64, text string, c Claimer) (claim *model.RedPacketClaim, p *model.RedPacket, matched bool, err error) {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > 20 {
		return nil, nil, false, nil
	}
	q := s.st.DB.Where("type = ? AND status = ? AND password = ? AND expires_at > ?", TypePassword, StatusActive, text, s.now())
	if chatID != 0 {
		q = q.Where("chat_id = ?", chatID)
	}
	var list []model.RedPacket
	if err := q.Order("id desc").Limit(20).Find(&list).Error; err != nil {
		return nil, nil, false, err
	}
	if len(list) == 0 {
		return nil, nil, false, nil
	}
	for _, cand := range list {
		claim, p, err = s.claim(cand.Code, c, true)
		if err == nil {
			return claim, p, true, nil
		}
	}
	// Every matching packet was already claimed by this user, or was just emptied.
	return nil, nil, true, err
}

// Active returns the sender's claimable packets, newest first.
func (s *Service) Active(senderUserID uint64, botID uint64, limit int) ([]model.RedPacket, error) {
	out := []model.RedPacket{}
	return out, s.st.DB.Where("sender_user_id = ? AND bot_id = ? AND status = ? AND expires_at > ?", senderUserID, botID, StatusActive, s.now()).Order("id desc").Limit(limit).Find(&out).Error
}

// SetMessage records the group message showing the packet so later claims can update it.
func (s *Service) SetMessage(id uint64, chatID int64, messageID int) error {
	return s.st.DB.Model(&model.RedPacket{}).Where("id = ?", id).Updates(map[string]any{"chat_id": chatID, "message_id": messageID}).Error
}

// ExpireDue refunds what is left of packets past their expiry and marks them expired.
func (s *Service) ExpireDue(ctx context.Context) error {
	var due []model.RedPacket
	if err := s.st.DB.Where("status = ? AND expires_at <= ?", StatusActive, s.now()).Order("id asc").Limit(100).Find(&due).Error; err != nil {
		return err
	}
	for _, d := range due {
		var p model.RedPacket
		var refund decimal.Decimal
		err := s.st.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, d.ID).Error; err != nil {
				return err
			}
			if p.Status != StatusActive || s.now().Before(p.ExpiresAt) {
				return nil
			}
			total, _ := decimal.NewFromString(p.Total)
			claimed, _ := decimal.NewFromString(p.ClaimedAmount)
			refund = total.Sub(claimed)
			now := s.now()
			if err := tx.Model(&model.RedPacket{}).Where("id = ?", p.ID).Updates(map[string]any{"status": StatusExpired, "refund_amount": refund.StringFixed(6), "finished_at": now}).Error; err != nil {
				return err
			}
			if !refund.IsPositive() {
				return nil
			}
			return s.wallet.AdjustCurrency(tx, p.SenderUserID, p.Currency, refund, "redpacket_refund", "redpacket", p.ID, "红包过期退回")
		})
		if err != nil {
			s.logger.Error("red packet expiry failed", zap.Uint64("packet_id", d.ID), zap.Error(err))
			continue
		}
		if refund.IsPositive() && s.notify != nil {
			name := p.Currency
			if c, err := s.wallet.Currency(p.Currency); err == nil {
				name = c.Name
			}
			html := fmt.Sprintf("🧧 你的%s红包已过期，%d/%d 个被领取，剩余 <code>%s %s</code> 已退回钱包。", TypeNames[p.Type], p.ClaimedCount, p.Count, refund.String(), name)
			if err := s.notify(ctx, p.BotID, p.SenderTelegramID, html); err != nil {
				s.logger.Warn("red packet refund notice failed", zap.Uint64("packet_id", p.ID), zap.Error(err))
			}
		}
	}
	return nil
}

// Run refunds expired packets every minute until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		if err := s.ExpireDue(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("red packet expiry scan failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// IsUserError reports whether err carries a message meant for people.
func IsUserError(err error) bool {
	var u UserError
	var w wallet.UserError
	return errors.As(err, &u) || errors.As(err, &w)
}
