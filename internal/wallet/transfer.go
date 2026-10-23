package wallet

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

// TransferRequest moves money from one user's balance to another's. To is "@username" or a
// numeric Telegram ID of someone who has used one of the bots.
type TransferRequest struct {
	To        string `json:"to"`
	Currency  string `json:"currency"`
	Amount    string `json:"amount"`
	Remark    string `json:"remark"`
	RequestID string `json:"request_id"`
}

var transferUsername = regexp.MustCompile(`^@?([A-Za-z][A-Za-z0-9_]{3,31})$`)

// UserName is how a user is shown to others: their name, or @username, or the Telegram ID.
func UserName(u *model.TelegramUser) string {
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	if u.Username != "" {
		return "@" + u.Username
	}
	return fmt.Sprintf("用户%d", u.TelegramUserID)
}

// FindRecipient looks up a transfer recipient by @username or Telegram ID.
func (s *Service) FindRecipient(to string) (*model.TelegramUser, error) {
	to = strings.TrimSpace(to)
	var u model.TelegramUser
	var err error
	if id, perr := strconv.ParseInt(to, 10, 64); perr == nil && id > 0 {
		err = s.st.DB.Where("telegram_user_id = ?", id).First(&u).Error
	} else if m := transferUsername.FindStringSubmatch(to); m != nil {
		err = s.st.DB.Where("username = ?", m[1]).Order("id desc").First(&u).Error
	} else {
		return nil, userErr("请输入收款人的 @用户名 或数字 Telegram ID")
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, userErr("找不到该用户，对方需要先使用过本平台的机器人")
	}
	if err != nil {
		return nil, err
	}
	if u.Status != 1 {
		return nil, userErr("该用户暂时无法收款")
	}
	return &u, nil
}

// Transfer moves an amount of one currency to another user at once, without a fee. It is
// idempotent per request ID: a repeated request returns the first transfer.
func (s *Service) Transfer(fromUserID uint64, r TransferRequest) (*model.WalletTransfer, *model.TelegramUser, error) {
	if r.RequestID == "" || len(r.RequestID) > 64 {
		return nil, nil, userErr("请求无效，请刷新后重试")
	}
	r.Remark = strings.TrimSpace(r.Remark)
	if utf8.RuneCountInString(r.Remark) > 40 {
		return nil, nil, userErr("备注最多 40 个字")
	}
	to, err := s.FindRecipient(r.To)
	if err != nil {
		return nil, nil, err
	}
	if to.ID == fromUserID {
		return nil, nil, userErr("不能转账给自己")
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(r.Amount))
	if err != nil || !amount.IsPositive() {
		return nil, nil, userErr("请输入正确的转账金额")
	}
	var from model.TelegramUser
	if err := s.st.DB.First(&from, fromUserID).Error; err != nil {
		return nil, nil, err
	}
	c, err := s.Currency(r.Currency)
	if err != nil {
		return nil, nil, err
	}
	if !amount.Equal(amount.Truncate(int32(c.Decimals))) {
		return nil, nil, userErr("%s 金额最多 %d 位小数", c.Name, c.Decimals)
	}
	// Both accounts exist before the transaction: inserting them inside it would take shared
	// locks that two opposite transfers then both try to upgrade (a deadlock).
	for _, id := range []uint64{fromUserID, to.ID} {
		if err := s.st.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.WalletAccount{UserID: id, Currency: c.Code, Available: "0", Frozen: "0"}).Error; err != nil {
			return nil, nil, err
		}
	}
	var out model.WalletTransfer
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Both accounts are locked in one statement (index order), so transfers in opposite
		// directions cannot deadlock; this also serialises retries of the same request. Nothing
		// is read before the lock: under REPEATABLE READ an earlier plain read would fix a
		// snapshot that a concurrent transfer then invalidates.
		var accs []model.WalletAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id IN ? AND currency = ?", []uint64{fromUserID, to.ID}, c.Code).Order("user_id").Find(&accs).Error; err != nil {
			return err
		}
		var prev model.WalletTransfer
		err = tx.Where("request_id = ?", r.RequestID).First(&prev).Error
		if err == nil {
			if prev.FromUserID != fromUserID || prev.ToUserID != to.ID || prev.Currency != c.Code || !sameDecimal(prev.Amount, amount.String()) {
				return userErr("请求无效，请刷新后重试")
			}
			out = prev
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		out = model.WalletTransfer{RequestID: r.RequestID, FromUserID: fromUserID, ToUserID: to.ID, Currency: c.Code, Amount: amount.StringFixed(6), Remark: r.Remark}
		if err := tx.Create(&out).Error; err != nil {
			return err
		}
		note := func(prefix string, u *model.TelegramUser) string {
			text := prefix + UserName(u)
			if r.Remark != "" {
				text += "（" + r.Remark + "）"
			}
			return text
		}
		if err := adjustIn(tx, fromUserID, c.Code, amount.Neg(), "transfer_out", "transfer", out.ID, note("转账给 ", to)); err != nil {
			if errors.Is(err, ErrInsufficientBalance) {
				return userErr("%s 可用余额不足", c.Name)
			}
			return err
		}
		return adjustIn(tx, to.ID, c.Code, amount, "transfer_in", "transfer", out.ID, note("来自 ", &from))
	})
	if err != nil {
		return nil, nil, err
	}
	return &out, to, nil
}

// Transfers lists a user's latest transfers, sent and received.
func (s *Service) Transfers(userID uint64, limit int) ([]model.WalletTransfer, error) {
	list := []model.WalletTransfer{}
	err := s.st.DB.Where("from_user_id = ? OR to_user_id = ?", userID, userID).Order("id desc").Limit(limit).Find(&list).Error
	return list, err
}

// WithdrawRule is what a withdrawal of one currency needs: minimum, fee and payout target.
type WithdrawRule struct {
	Currency    string `json:"currency"`
	Name        string `json:"name"`
	Icon        string `json:"icon"`
	Kind        string `json:"kind"`
	Decimals    int    `json:"decimals"`
	MinWithdraw string `json:"min_withdraw"`
	WithdrawFee string `json:"withdraw_fee"`
	Target      string `json:"target"`
}

// WithdrawRules lists the withdrawable currencies (as the bot's withdrawal menu does); the USDT
// (TRC20) wallet keeps its environment-configured minimum and fee.
func (s *Service) WithdrawRules() ([]WithdrawRule, error) {
	list, err := s.Currencies(true)
	if err != nil {
		return nil, err
	}
	out := []WithdrawRule{}
	for _, c := range list {
		if !c.Withdrawable {
			continue
		}
		rule := WithdrawRule{Currency: c.Code, Name: c.Name, Icon: c.Icon, Kind: c.Kind, Decimals: c.Decimals, MinWithdraw: c.MinWithdraw, WithdrawFee: c.WithdrawFee, Target: WithdrawTarget(c.Kind)}
		if c.Code == s.currency {
			rule.Decimals, rule.Target = 6, WithdrawTarget(KindTRC20)
		}
		out = append(out, rule)
	}
	return out, nil
}
