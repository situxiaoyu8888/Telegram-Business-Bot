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

func (s *Service) CreditDeposit(order model.DepositOrder) error {
	amount, err := decimal.NewFromString(order.Amount)
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(6)) || strings.TrimSpace(order.TxID) == "" {
		return fmt.Errorf("invalid deposit")
	}

	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&model.DepositOrder{}).Where("tx_id=?", order.TxID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return nil
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
			return err
		}
		return tx.Create(&model.WalletTransaction{UserID: order.UserID, Currency: s.currency, Type: "deposit", Amount: amt.StringFixed(6), Balance: newBal.StringFixed(6), RefType: "deposit", RefID: order.ID, Remark: "TRC20 deposit"}).Error
	})
}

func (s *Service) CreateWithdrawal(userID uint64, network, address, amountText, requestID string) (*model.WithdrawalOrder, error) {
	if requestID == "" || len(requestID) > 64 {
		return nil, fmt.Errorf("withdrawal request ID required")
	}
	if network != "TRC20" || !ValidTRONAddress(address) {
		return nil, fmt.Errorf("invalid TRC20 address/network")
	}
	amount, err := decimal.NewFromString(amountText)
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(6)) {
		return nil, fmt.Errorf("invalid amount (maximum 6 decimal places)")
	}
	min, minErr := decimal.NewFromString(s.minWithdraw)
	fee, feeErr := decimal.NewFromString(s.withdrawFee)
	if minErr != nil || feeErr != nil || !min.IsPositive() || fee.IsNegative() || !fee.Equal(fee.Truncate(6)) {
		return nil, fmt.Errorf("invalid withdrawal configuration")
	}
	if amount.LessThan(min) {
		return nil, fmt.Errorf("minimum withdrawal is %s", min.String())
	}
	actual := amount.Sub(fee)
	if actual.LessThanOrEqual(decimal.Zero) {
		return nil, fmt.Errorf("amount must be greater than fee")
	}
	order := &model.WithdrawalOrder{RequestID: &requestID, UserID: userID, Network: network, Currency: s.currency, Address: address, Amount: amount.Truncate(6).StringFixed(6), Fee: fee.Truncate(6).StringFixed(6), Actual: actual.Truncate(6).StringFixed(6), Status: "pending"}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		var acc model.WalletAccount
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=? AND currency=?", userID, s.currency).First(&acc).Error
		if err != nil {
			return fmt.Errorf("wallet not found")
		}
		var existing model.WithdrawalOrder
		lookup := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("request_id=?", requestID).First(&existing).Error
		if lookup == nil {
			if existing.UserID != userID || existing.Amount != order.Amount || existing.Address != address || existing.Network != network {
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
			return fmt.Errorf("insufficient balance")
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
		return tx.Create(&model.WalletTransaction{UserID: userID, Currency: s.currency, Type: "withdraw_freeze", Amount: amount.Neg().StringFixed(6), Balance: av.StringFixed(6), RefType: "withdraw", RefID: order.ID, Remark: "withdrawal pending review"}).Error
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
