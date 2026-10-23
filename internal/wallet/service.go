package wallet

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	st                                 *store.Store
	currency, minWithdraw, withdrawFee string
	onDeposit                          func(o model.DepositOrder, announce bool)
}

func New(st *store.Store, currency, minWithdraw, withdrawFee string) *Service {
	return &Service{st: st, currency: currency, minWithdraw: minWithdraw, withdrawFee: withdrawFee}
}

func (s *Service) Balance(userID uint64) (decimal.Decimal, decimal.Decimal, error) {
	a := model.WalletAccount{UserID: userID, Currency: s.currency, Available: "0", Frozen: "0"}
	if err := s.st.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&a).Error; err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	if err := s.st.DB.Where("user_id=? AND currency=?", userID, s.currency).First(&a).Error; err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	av, e := decimal.NewFromString(a.Available)
	if e != nil {
		return decimal.Zero, decimal.Zero, e
	}
	fr, e := decimal.NewFromString(a.Frozen)
	return av, fr, e
}

// OrderNo is how a wallet record is numbered for people: a kind letter (W withdrawal, D on-chain
// deposit, A console adjustment), the creation date and the record ID, e.g. W20261023000042.
func OrderNo(kind string, id uint64, created time.Time) string {
	return fmt.Sprintf("%s%s%06d", kind, created.Format("20060102"), id)
}

// WithDepositHook sets what runs after a deposit is credited: announce is false when the user
// claimed it themselves and already got the answer.
func (s *Service) WithDepositHook(f func(o model.DepositOrder, announce bool)) *Service {
	s.onDeposit = f
	return s
}

// SetWithdrawalBot records which bot a withdrawal was requested through; its review notices go
// out through that bot.
func (s *Service) SetWithdrawalBot(id, botID uint64) error {
	return s.st.DB.Model(&model.WithdrawalOrder{}).Where("id = ?", id).Update("bot_id", botID).Error
}

func (s *Service) CreditDeposit(order model.DepositOrder) error {
	return s.creditDeposit(order, false)
}

// CreditToHolder credits a deposit the TRON watcher saw on a single-holder address, re-checking
// under the transfer's lock that the user is still its only holder ever and got it before the
// transfer; otherwise the deposit is recorded as unclaimed (the address was shared meanwhile).
func (s *Service) CreditToHolder(order model.DepositOrder) error {
	return s.creditDeposit(order, true)
}

func (s *Service) creditDeposit(order model.DepositOrder, checkHolder bool) error {
	amount, err := decimal.NewFromString(order.Amount)
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(6)) || strings.TrimSpace(order.TxID) == "" {
		return fmt.Errorf("invalid deposit")
	}

	order.TxID = strings.ToLower(strings.TrimSpace(order.TxID))
	if order.EventKey == nil {
		key := TRC20EventKey(order.TxID, order.Address, amount, 0)
		order.EventKey = &key
	}
	credited := false
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Serialise with claims and assignments of the same transaction (first statement, so the
		// checks below see what they committed).
		if err := lockTransfer(tx, "TRC20", order.TxID); err != nil {
			return err
		}
		var n int64
		if err := tx.Model(&model.DepositOrder{}).Where("event_key = ?", *order.EventKey).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		if checkHolder {
			ok, err := soleHolder(tx, order.Network, order.Address, order.UserID, time.UnixMilli(order.BlockTimestamp))
			if err != nil {
				return err
			}
			if !ok {
				order.ID, order.UserID, order.Status, order.Currency = 0, 0, DepositUnclaimed, s.currency
				if err := tx.Create(&order).Error; err != nil && !store.IsDuplicate(err) {
					return err
				}
				return nil
			}
		}
		var acc model.WalletAccount
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=? AND currency=?", order.UserID, s.currency).First(&acc).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			acc = model.WalletAccount{UserID: order.UserID, Currency: s.currency, Available: "0", Frozen: "0"}
			if err = tx.Create(&acc).Error; err != nil {
				return err
			}
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		old, _ := decimal.NewFromString(acc.Available)
		amt, err := decimal.NewFromString(order.Amount)
		if err != nil {
			return err
		}
		newBal := old.Add(amt).Truncate(6)
		acc.Available = newBal.StringFixed(6)
		if err := tx.Save(&acc).Error; err != nil {
			return err
		}
		order.Currency = s.currency
		order.Status = "credited"
		if err := tx.Create(&order).Error; err != nil {
			if store.IsDuplicate(err) {
				return errSeen // another instance recorded it meanwhile; roll back the credit
			}
			return err
		}
		credited = true
		return tx.Create(&model.WalletTransaction{UserID: order.UserID, Currency: s.currency, Type: "deposit", Amount: amt.StringFixed(6), Balance: newBal.StringFixed(6), RefType: "deposit", RefID: order.ID, Remark: "TRC20 deposit"}).Error
	})
	if errors.Is(err, errSeen) {
		return nil
	}
	if err == nil && credited && s.onDeposit != nil {
		s.onDeposit(order, true)
	}
	return err
}

func (s *Service) CreateWithdrawal(userID uint64, network, address, amountText, requestID string) (*model.WithdrawalOrder, error) {
	if network != "TRC20" || !ValidTRONAddress(address) {
		return nil, userErr("地址格式不正确，请输入以 T 开头的 TRON 地址")
	}
	return s.createWithdrawal(userID, s.currency, network, address, amountText, requestID, s.minWithdraw, s.withdrawFee, 6)
}

// createWithdrawal freezes amount for a pending withdrawal; a repeated request ID returns the
// first order.
func (s *Service) createWithdrawal(userID uint64, currency, network, address, amountText, requestID, minText, feeText string, places int) (*model.WithdrawalOrder, error) {
	if requestID == "" || len(requestID) > 64 {
		return nil, fmt.Errorf("withdrawal request ID required")
	}
	amount, err := decimal.NewFromString(amountText)
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(int32(places))) {
		return nil, userErr("金额格式不正确（最多 %d 位小数）", places)
	}
	min, minErr := decimal.NewFromString(minText)
	fee, feeErr := decimal.NewFromString(feeText)
	if minErr != nil || feeErr != nil || min.IsNegative() || fee.IsNegative() || !fee.Equal(fee.Truncate(6)) {
		return nil, fmt.Errorf("invalid withdrawal configuration")
	}
	if amount.LessThan(min) {
		return nil, userErr("最低提现金额为 %s", min.String())
	}
	actual := amount.Sub(fee)
	if actual.LessThanOrEqual(decimal.Zero) {
		return nil, userErr("提现金额需大于手续费 %s", fee.String())
	}
	order := &model.WithdrawalOrder{RequestID: &requestID, UserID: userID, Network: network, Currency: currency, Address: address, Amount: amount.StringFixed(6), Fee: fee.StringFixed(6), Actual: actual.StringFixed(6), Status: "pending"}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		var acc model.WalletAccount
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=? AND currency=?", userID, currency).First(&acc).Error
		if err != nil {
			return userErr("可用余额不足")
		}
		var existing model.WithdrawalOrder
		lookup := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("request_id=?", requestID).First(&existing).Error
		if lookup == nil {
			if existing.UserID != userID || existing.Amount != order.Amount || existing.Address != address || existing.Network != network || existing.Currency != currency {
				return fmt.Errorf("request ID reused with different withdrawal")
			}
			*order = existing
			return nil
		}
		if !errors.Is(lookup, gorm.ErrRecordNotFound) {
			return lookup
		}
		av, _ := decimal.NewFromString(acc.Available)
		fr, _ := decimal.NewFromString(acc.Frozen)
		if av.LessThan(amount) {
			return userErr("可用余额不足")
		}
		av = av.Sub(amount)
		fr = fr.Add(amount)
		acc.Available = av.StringFixed(6)
		acc.Frozen = fr.StringFixed(6)
		if err := tx.Save(&acc).Error; err != nil {
			return err
		}
		if err := tx.Create(order).Error; err != nil {
			return err
		}
		return tx.Create(&model.WalletTransaction{UserID: userID, Currency: currency, Type: "withdraw_freeze", Amount: amount.Neg().StringFixed(6), Balance: av.StringFixed(6), RefType: "withdraw", RefID: order.ID, Remark: "withdrawal pending review"}).Error
	})
	return order, err
}

func (s *Service) RejectWithdrawal(id uint64, reviewer, note string) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var o model.WithdrawalOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, id).Error; err != nil {
			return err
		}
		if o.Status != "pending" && o.Status != "approved" {
			return fmt.Errorf("order status is %s", o.Status)
		}
		var a model.WalletAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=? AND currency=?", o.UserID, o.Currency).First(&a).Error; err != nil {
			return err
		}
		av, _ := decimal.NewFromString(a.Available)
		fr, _ := decimal.NewFromString(a.Frozen)
		amt, _ := decimal.NewFromString(o.Amount)
		if fr.LessThan(amt) {
			return fmt.Errorf("frozen balance mismatch")
		}
		av = av.Add(amt)
		fr = fr.Sub(amt)
		a.Available = av.StringFixed(6)
		a.Frozen = fr.StringFixed(6)
		if err := tx.Save(&a).Error; err != nil {
			return err
		}
		now := time.Now()
		o.Status = "rejected"
		o.ReviewedBy = reviewer
		o.ReviewNote = note
		o.ReviewedAt = &now
		if err := tx.Save(&o).Error; err != nil {
			return err
		}
		return tx.Create(&model.WalletTransaction{UserID: o.UserID, Currency: o.Currency, Type: "withdraw_unfreeze", Amount: amt.StringFixed(6), Balance: av.StringFixed(6), RefType: "withdraw", RefID: o.ID, Remark: "withdrawal rejected"}).Error
	})
}

func (s *Service) ApproveWithdrawal(id uint64, reviewer, note string) error {
	now := time.Now()
	res := s.st.DB.Model(&model.WithdrawalOrder{}).Where("id=? AND status='pending'", id).Updates(map[string]any{"status": "approved", "reviewed_by": reviewer, "review_note": note, "reviewed_at": &now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("withdrawal not found or no longer pending")
	}
	return nil
}
func (s *Service) CompleteWithdrawal(id uint64, txid, reviewer string) error {
	txid = strings.TrimSpace(txid)
	if txid == "" {
		return fmt.Errorf("txid required")
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var o model.WithdrawalOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, id).Error; err != nil {
			return err
		}
		if o.Status != "approved" {
			return fmt.Errorf("order must be approved first")
		}
		var a model.WalletAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=? AND currency=?", o.UserID, o.Currency).First(&a).Error; err != nil {
			return err
		}
		fr, _ := decimal.NewFromString(a.Frozen)
		amt, _ := decimal.NewFromString(o.Amount)
		if fr.LessThan(amt) {
			return fmt.Errorf("frozen balance mismatch")
		}
		a.Frozen = fr.Sub(amt).StringFixed(6)
		if err := tx.Save(&a).Error; err != nil {
			return err
		}
		now := time.Now()
		o.Status = "completed"
		o.TxID = txid
		o.ReviewedBy = reviewer
		o.ReviewedAt = &now
		return tx.Save(&o).Error
	})
}

// ErrInsufficientBalance is returned by Spend when the available balance is too low.
var ErrInsufficientBalance = errors.New("insufficient balance")

// Spend deducts a shop payment from the available balance inside the caller's transaction,
// so the order and the ledger entry commit or roll back together.
func (s *Service) Spend(tx *gorm.DB, userID uint64, amount decimal.Decimal, refType string, refID uint64, remark string) error {
	return s.adjust(tx, userID, amount.Neg(), "shop_pay", refType, refID, remark)
}

// Refund returns a shop payment to the available balance inside the caller's transaction.
func (s *Service) Refund(tx *gorm.DB, userID uint64, amount decimal.Decimal, refType string, refID uint64, remark string) error {
	return s.adjust(tx, userID, amount, "shop_refund", refType, refID, remark)
}

func (s *Service) adjust(tx *gorm.DB, userID uint64, delta decimal.Decimal, txType, refType string, refID uint64, remark string) error {
	return adjustIn(tx, userID, s.currency, delta, txType, refType, refID, remark)
}

// AdjustCurrency changes a user's available balance in one currency inside the caller's
// transaction (negative delta debits; ErrInsufficientBalance when it would go below zero).
func (s *Service) AdjustCurrency(tx *gorm.DB, userID uint64, currency string, delta decimal.Decimal, txType, refType string, refID uint64, remark string) error {
	return adjustIn(tx, userID, currency, delta, txType, refType, refID, remark)
}

// adjustIn changes a user's available balance in one currency and records the ledger entry.
func adjustIn(tx *gorm.DB, userID uint64, currency string, delta decimal.Decimal, txType, refType string, refID uint64, remark string) error {
	if delta.IsZero() {
		return nil
	}
	if !delta.Equal(delta.Truncate(6)) {
		return fmt.Errorf("invalid amount (maximum 6 decimal places)")
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.WalletAccount{UserID: userID, Currency: currency, Available: "0", Frozen: "0"}).Error; err != nil {
		return err
	}
	var acc model.WalletAccount
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=? AND currency=?", userID, currency).First(&acc).Error; err != nil {
		return err
	}
	av, err := decimal.NewFromString(acc.Available)
	if err != nil {
		return err
	}
	av = av.Add(delta)
	if av.IsNegative() {
		return ErrInsufficientBalance
	}
	if err := tx.Model(&acc).Update("available", av.StringFixed(6)).Error; err != nil {
		return err
	}
	return tx.Create(&model.WalletTransaction{UserID: userID, Currency: currency, Type: txType, Amount: delta.StringFixed(6), Balance: av.StringFixed(6), RefType: refType, RefID: refID, Remark: remark}).Error
}

// Credit adds a positive amount to the available balance inside the caller's transaction,
// e.g. a shop referral commission, and records it in the ledger as txType.
func (s *Service) Credit(tx *gorm.DB, userID uint64, amount decimal.Decimal, txType, refType string, refID uint64, remark string) error {
	if !amount.IsPositive() {
		return fmt.Errorf("credit must be positive")
	}
	return s.adjust(tx, userID, amount, txType, refType, refID, remark)
}

// TxTypeNames are the Chinese names of wallet transaction types, shown to users.
var TxTypeNames = map[string]string{
	"deposit": "充值", "withdraw_freeze": "提现", "withdraw_unfreeze": "提现退回",
	"shop_pay": "商城付款", "shop_refund": "商城退款", "referral_commission": "推广佣金",
	"exchange_out": "换汇兑出", "exchange_in": "换汇兑入", "admin_credit": "后台加款", "admin_debit": "后台扣款",
	"redpacket_send": "发红包", "redpacket_claim": "领取红包", "redpacket_refund": "红包退回",
	"transfer_out": "转账转出", "transfer_in": "转账转入",
}

// TxTypeName returns a transaction type's Chinese name, or the type itself when unknown.
func TxTypeName(t string) string {
	if n, ok := TxTypeNames[t]; ok {
		return n
	}
	return t
}
