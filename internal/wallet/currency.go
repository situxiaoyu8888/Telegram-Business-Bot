package wallet

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

// Currency kinds: the TRC20 and ERC20 USDT tokens, and fiat money.
const (
	KindTRC20 = "trc20"
	KindERC20 = "erc20"
	KindFiat  = "fiat"
)

// UserError carries a message meant for the person who made the request.
type UserError struct{ Msg string }

func (e UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return UserError{fmt.Sprintf(format, args...)} }

var (
	currencyCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,15}$`)
	erc20Address = regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
)

// ValidCurrencyCode reports whether code can name a currency (e.g. "CNY", "USDT_ERC20").
func ValidCurrencyCode(code string) bool { return currencyCode.MatchString(code) }

// ValidERC20Address checks the shape of an Ethereum address.
func ValidERC20Address(addr string) bool { return erc20Address.MatchString(addr) }

// Balance is one currency of a user's wallet.
type Balance struct {
	model.WalletCurrency
	Available string `json:"available"`
	Frozen    string `json:"frozen"`
}

// Currencies lists the configured currencies in display order.
func (s *Service) Currencies(enabledOnly bool) ([]model.WalletCurrency, error) {
	q := s.st.DB.Order("sort asc, id asc")
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	out := []model.WalletCurrency{}
	return out, q.Find(&out).Error
}

// Currency returns an enabled currency.
func (s *Service) Currency(code string) (*model.WalletCurrency, error) {
	return currencyIn(s.st.DB, code)
}

func currencyIn(db *gorm.DB, code string) (*model.WalletCurrency, error) {
	var c model.WalletCurrency
	err := db.Where("code = ? AND enabled = ?", code, true).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, userErr("不支持的币种：%s", code)
	}
	return &c, err
}

// Balances returns the user's balance in every enabled currency (zero where no account exists).
func (s *Service) Balances(userID uint64) ([]Balance, error) {
	cur, err := s.Currencies(true)
	if err != nil {
		return nil, err
	}
	var accs []model.WalletAccount
	if err := s.st.DB.Where("user_id = ?", userID).Find(&accs).Error; err != nil {
		return nil, err
	}
	byCode := map[string]model.WalletAccount{}
	for _, a := range accs {
		byCode[a.Currency] = a
	}
	out := make([]Balance, 0, len(cur))
	for _, c := range cur {
		b := Balance{WalletCurrency: c, Available: decimal.Zero.StringFixed(int32(c.Decimals)), Frozen: decimal.Zero.StringFixed(int32(c.Decimals))}
		if a, ok := byCode[c.Code]; ok {
			b.Available, b.Frozen = fixed(a.Available, c.Decimals), fixed(a.Frozen, c.Decimals)
		}
		out = append(out, b)
	}
	return out, nil
}

func fixed(v string, places int) string {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return v
	}
	return d.StringFixed(int32(places))
}

// ExchangeFeeRate reads the exchange fee (system config exchange_fee_rate, 0 when unset).
func (s *Service) ExchangeFeeRate() (decimal.Decimal, error) {
	return feeRateIn(s.st.DB)
}

func feeRateIn(db *gorm.DB) (decimal.Decimal, error) {
	var c model.SystemConfig
	err := db.Where("`key` = ?", "exchange_fee_rate").First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && strings.TrimSpace(c.Value) == "") {
		return decimal.Zero, nil
	}
	if err != nil {
		return decimal.Zero, err
	}
	fee, err := decimal.NewFromString(strings.TrimSpace(c.Value))
	if err != nil || fee.IsNegative() || fee.GreaterThanOrEqual(decimal.NewFromInt(1)) {
		return decimal.Zero, fmt.Errorf("invalid exchange_fee_rate %q", c.Value)
	}
	return fee, nil
}

// ExchangeRate is how many units of `to` one unit of `from` buys after the fee.
func ExchangeRate(from, to model.WalletCurrency, fee decimal.Decimal) (decimal.Decimal, error) {
	fr, err1 := decimal.NewFromString(from.Rate)
	tr, err2 := decimal.NewFromString(to.Rate)
	if err1 != nil || err2 != nil || !fr.IsPositive() || !tr.IsPositive() {
		return decimal.Zero, fmt.Errorf("invalid rate for %s/%s", from.Code, to.Code)
	}
	return tr.Mul(decimal.NewFromInt(1).Sub(fee)).DivRound(fr, 12), nil
}

// ExchangeRequest exchanges Amount of From into To. FromRate, ToRate and FeeRate are the values
// the user was shown; the exchange is refused when the configuration has changed since.
type ExchangeRequest struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Amount    string `json:"amount"`
	FromRate  string `json:"from_rate"`
	ToRate    string `json:"to_rate"`
	FeeRate   string `json:"fee_rate"`
	RequestID string `json:"request_id"`
}

func sameDecimal(a, b string) bool {
	x, err1 := decimal.NewFromString(strings.TrimSpace(a))
	y, err2 := decimal.NewFromString(strings.TrimSpace(b))
	return err1 == nil && err2 == nil && x.Equal(y)
}

// Exchange moves money between two of the user's balances at the configured rates. It is
// idempotent per request ID: a repeated request returns the first result without charging again.
func (s *Service) Exchange(userID uint64, r ExchangeRequest) (*model.WalletExchange, error) {
	if r.RequestID == "" || len(r.RequestID) > 64 {
		return nil, userErr("请求无效，请刷新后重试")
	}
	if r.From == r.To {
		return nil, userErr("请选择两种不同的币种")
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(r.Amount))
	if err != nil || !amount.IsPositive() {
		return nil, userErr("请输入正确的兑换金额")
	}
	var out model.WalletExchange
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		from, err := currencyIn(tx, r.From)
		if err != nil {
			return err
		}
		to, err := currencyIn(tx, r.To)
		if err != nil {
			return err
		}
		if !from.Exchangeable || !to.Exchangeable {
			return userErr("%s 暂不支持兑换为 %s", from.Name, to.Name)
		}
		if !amount.Equal(amount.Truncate(int32(from.Decimals))) {
			return userErr("%s 金额最多 %d 位小数", from.Name, from.Decimals)
		}
		// Lock both accounts in one statement (index order) so opposite exchanges cannot deadlock;
		// this also serialises retries of the same request.
		for _, code := range []string{from.Code, to.Code} {
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.WalletAccount{UserID: userID, Currency: code, Available: "0", Frozen: "0"}).Error; err != nil {
				return err
			}
		}
		codes := []string{from.Code, to.Code}
		sort.Strings(codes)
		var accs []model.WalletAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND currency IN ?", userID, codes).Order("currency").Find(&accs).Error; err != nil {
			return err
		}
		var prev model.WalletExchange
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("request_id = ?", r.RequestID).First(&prev).Error
		if err == nil {
			if prev.UserID != userID || prev.FromCurrency != from.Code || prev.ToCurrency != to.Code || !sameDecimal(prev.Amount, amount.String()) {
				return userErr("请求无效，请刷新后重试")
			}
			out = prev
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		fee, err := feeRateIn(tx)
		if err != nil {
			return err
		}
		if !sameDecimal(r.FromRate, from.Rate) || !sameDecimal(r.ToRate, to.Rate) || !sameDecimal(r.FeeRate, fee.String()) {
			return userErr("汇率已更新，请确认新的汇率后重试")
		}
		rate, err := ExchangeRate(*from, *to, fee)
		if err != nil {
			return err
		}
		received := amount.Mul(rate).Truncate(int32(to.Decimals))
		if !received.IsPositive() {
			return userErr("兑换金额过小")
		}
		out = model.WalletExchange{UserID: userID, RequestID: r.RequestID, FromCurrency: from.Code, ToCurrency: to.Code, Amount: amount.StringFixed(6), Received: received.StringFixed(6), Rate: rate.StringFixed(12), FeeRate: fee.StringFixed(6)}
		if err := tx.Create(&out).Error; err != nil {
			return err
		}
		remark := fmt.Sprintf("%s → %s", from.Name, to.Name)
		if err := adjustIn(tx, userID, from.Code, amount.Neg(), "exchange_out", "exchange", out.ID, remark); err != nil {
			if errors.Is(err, ErrInsufficientBalance) {
				return userErr("%s 可用余额不足", from.Name)
			}
			return err
		}
		return adjustIn(tx, userID, to.Code, received, "exchange_in", "exchange", out.ID, remark)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Exchanges lists the user's latest exchanges.
func (s *Service) Exchanges(userID uint64, limit int) ([]model.WalletExchange, error) {
	out := []model.WalletExchange{}
	return out, s.st.DB.Where("user_id = ?", userID).Order("id desc").Limit(limit).Find(&out).Error
}

// AdminAdjust credits (positive amount) or debits (negative amount) a user's balance from the
// console, e.g. for an ERC20 or cash deposit. A debit never takes the balance below zero.
func (s *Service) AdminAdjust(userID uint64, currency, amountText, operator, remark string) error {
	amount, err := decimal.NewFromString(strings.TrimSpace(amountText))
	if err != nil || amount.IsZero() {
		return userErr("请输入正确的金额（负数为扣减）")
	}
	remark = strings.TrimSpace(remark)
	if remark == "" || utf8.RuneCountInString(remark) > 100 {
		return userErr("请填写 100 字以内的调整原因")
	}
	c, err := s.Currency(currency)
	if err != nil {
		return err
	}
	if !amount.Equal(amount.Truncate(int32(c.Decimals))) {
		return userErr("%s 金额最多 %d 位小数", c.Name, c.Decimals)
	}
	kind := "admin_credit"
	if amount.IsNegative() {
		kind = "admin_debit"
	}
	note := remark
	if operator != "" {
		note = operator + "：" + remark
	}
	if utf8.RuneCountInString(note) > 255 {
		note = string([]rune(note)[:255])
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := adjustIn(tx, userID, c.Code, amount, kind, "admin", 0, note); err != nil {
			if errors.Is(err, ErrInsufficientBalance) {
				return userErr("%s 可用余额不足，无法扣减", c.Name)
			}
			return err
		}
		return nil
	})
}

// withdrawNetwork is the network recorded on a withdrawal of the given currency kind.
func withdrawNetwork(kind string) string {
	switch kind {
	case KindTRC20:
		return "TRC20"
	case KindERC20:
		return "ERC20"
	}
	return "BANK"
}

// WithdrawTarget describes where a currency is paid out, for prompts.
func WithdrawTarget(kind string) string {
	switch kind {
	case KindTRC20:
		return "TRON/TRC20 地址"
	case KindERC20:
		return "以太坊 ERC20 地址（0x 开头）"
	}
	return "收款信息（银行/开户名/账号等）"
}

// CheckWithdrawAddress validates the payout address or account for a currency kind.
func CheckWithdrawAddress(kind, address string) error {
	switch kind {
	case KindTRC20:
		if !ValidTRONAddress(address) {
			return userErr("地址格式不正确，请输入以 T 开头的 TRON 地址")
		}
	case KindERC20:
		if !ValidERC20Address(address) {
			return userErr("地址格式不正确，请输入 0x 开头的 42 位以太坊地址")
		}
	default:
		if n := utf8.RuneCountInString(strings.TrimSpace(address)); n < 4 || n > 120 {
			return userErr("请填写 4–120 字的收款信息")
		}
	}
	return nil
}

// CreateCurrencyWithdrawal freezes a withdrawal of any enabled, withdrawable currency for review.
// The USDT (TRC20) wallet keeps its environment-configured minimum and fee.
func (s *Service) CreateCurrencyWithdrawal(userID uint64, currency, address, amountText, requestID string) (*model.WithdrawalOrder, error) {
	// The console's switches apply to every currency, the USDT wallet included.
	c, err := s.Currency(currency)
	if err != nil {
		return nil, err
	}
	if !c.Withdrawable {
		return nil, userErr("%s 暂不支持提现", c.Name)
	}
	if currency == s.currency {
		return s.CreateWithdrawal(userID, "TRC20", address, amountText, requestID)
	}
	address = strings.TrimSpace(address)
	if err := CheckWithdrawAddress(c.Kind, address); err != nil {
		return nil, err
	}
	return s.createWithdrawal(userID, c.Code, withdrawNetwork(c.Kind), address, amountText, requestID, c.MinWithdraw, c.WithdrawFee, c.WithdrawFeeRate, c.Decimals)
}
