package wallet

import (
	"errors"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// Deposits to an address that several users share cannot be told apart on chain, so they are
// not credited automatically: the TRON watcher records them as "unclaimed" and the sender claims
// one by its transaction hash (or the console assigns it). ERC20 deposits are never scanned; a
// user submits the hash and amount, and the console confirms ("pending" → "credited") or rejects.

// Deposit statuses besides "credited".
const (
	DepositUnclaimed = "unclaimed"
	DepositPending   = "pending"
	DepositRejected  = "rejected"
)

// ERC20Currency is the wallet currency of USDT on Ethereum.
const ERC20Currency = "USDT_ERC20"

var (
	tronTxHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ethTxHash  = regexp.MustCompile(`^0x[0-9a-f]{64}$`)
)

// DepositHashNetwork tells which network a transaction hash belongs to ("" when neither).
func DepositHashNetwork(hash string) string {
	hash = strings.ToLower(strings.TrimSpace(hash))
	switch {
	case tronTxHash.MatchString(hash):
		return "TRC20"
	case ethTxHash.MatchString(hash):
		return "ERC20"
	}
	return ""
}

// RecordUnclaimed stores a deposit to a shared address until someone claims it; a transaction
// seen before is ignored.
func (s *Service) RecordUnclaimed(order model.DepositOrder) error {
	order.ID, order.UserID, order.Status, order.Currency = 0, 0, DepositUnclaimed, s.currency
	err := s.st.DB.Create(&order).Error
	if store.IsDuplicate(err) {
		return nil
	}
	return err
}

// creditOrderTx credits a deposit order to userID inside tx and marks it credited.
func (s *Service) creditOrderTx(tx *gorm.DB, o *model.DepositOrder, userID uint64, amount decimal.Decimal, remark string) error {
	if err := adjustIn(tx, userID, o.Currency, amount, "deposit", "deposit", o.ID, remark); err != nil {
		return err
	}
	o.UserID, o.Amount, o.Status = userID, amount.StringFixed(6), "credited"
	return tx.Model(o).Updates(map[string]any{"user_id": userID, "amount": o.Amount, "status": "credited"}).Error
}

// lockOrder locks a deposit order (by ID or transaction hash) as the transaction's first read.
func lockOrder(tx *gorm.DB, where string, arg any) (*model.DepositOrder, error) {
	var o model.DepositOrder
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where(where, arg).First(&o).Error
	return &o, err
}

func (s *Service) afterCredit(o *model.DepositOrder, announce bool) {
	if s.onDeposit != nil {
		s.onDeposit(*o, announce)
	}
}

// ClaimTRC20 credits an unclaimed TRC20 deposit to the user who sent it, when it went to one of
// their deposit addresses.
func (s *Service) ClaimTRC20(userID uint64, hash string) (*model.DepositOrder, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if !tronTxHash.MatchString(hash) {
		return nil, userErr("交易哈希格式不正确（TRC20 为 64 位十六进制字符）")
	}
	var order *model.DepositOrder
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		o, err := lockOrder(tx, "tx_id = ?", hash)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return userErr("还没有查到这笔到账：请确认交易哈希正确、转入的是你的 USDT-TRC20 充值地址；刚转账的话请几分钟后再试")
		}
		if err != nil {
			return err
		}
		switch {
		case o.UserID == userID:
			return userErr("这笔充值已经入账")
		case o.Status != DepositUnclaimed:
			return userErr("这笔交易已被认领，如有疑问请联系客服")
		}
		var n int64
		if err := tx.Model(&model.DepositAddress{}).Where("user_id = ? AND network = 'TRC20' AND address = ?", userID, o.Address).Count(&n).Error; err != nil {
			return err
		}
		if n == 0 {
			return userErr("这笔交易不是转入你的充值地址，如有疑问请联系客服")
		}
		amount, err := decimal.NewFromString(o.Amount)
		if err != nil {
			return err
		}
		order = o
		return s.creditOrderTx(tx, o, userID, amount, "TRC20 deposit (claimed)")
	})
	if err != nil {
		return nil, err
	}
	s.afterCredit(order, false)
	return order, nil
}

// SubmitERC20 records a user's ERC20 deposit (hash and amount) for the console to confirm.
func (s *Service) SubmitERC20(userID uint64, hash, amountText string) (*model.DepositOrder, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if !ethTxHash.MatchString(hash) {
		return nil, userErr("交易哈希格式不正确（ERC20 为 0x 开头的 66 位字符）")
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(amountText))
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(6)) {
		return nil, userErr("请输入正确的充值金额（最多 6 位小数）")
	}
	var a model.DepositAddress
	if err := s.st.DB.Where("user_id = ? AND network = 'ERC20' AND status = 1", userID).First(&a).Error; err != nil {
		return nil, userErr("你还没有 USDT-ERC20 充值地址，请联系客服")
	}
	o := model.DepositOrder{UserID: userID, Network: "ERC20", Currency: ERC20Currency, Address: a.Address, Amount: amount.StringFixed(6), TxID: hash, Status: DepositPending}
	if err := s.st.DB.Create(&o).Error; err != nil {
		if store.IsDuplicate(err) {
			return nil, userErr("这笔交易已经提交过了")
		}
		return nil, err
	}
	return &o, nil
}

// AssignDeposit credits an unclaimed deposit to a user chosen in the console.
func (s *Service) AssignDeposit(id, userID uint64) (*model.DepositOrder, error) {
	var order *model.DepositOrder
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		o, err := lockOrder(tx, "id = ?", id)
		if err != nil {
			return err
		}
		if o.Status != DepositUnclaimed {
			return userErr("这笔到账已处理")
		}
		amount, err := decimal.NewFromString(o.Amount)
		if err != nil {
			return err
		}
		order = o
		return s.creditOrderTx(tx, o, userID, amount, "TRC20 deposit (assigned)")
	})
	if err != nil {
		return nil, err
	}
	s.afterCredit(order, true)
	return order, nil
}

// ConfirmDeposit credits a submitted ERC20 deposit, with the amount the console verified.
func (s *Service) ConfirmDeposit(id uint64, amountText string) (*model.DepositOrder, error) {
	amount, err := decimal.NewFromString(strings.TrimSpace(amountText))
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(6)) {
		return nil, userErr("请输入正确的到账金额（最多 6 位小数）")
	}
	var order *model.DepositOrder
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		o, err := lockOrder(tx, "id = ?", id)
		if err != nil {
			return err
		}
		if o.Status != DepositPending {
			return userErr("这笔充值已处理")
		}
		order = o
		return s.creditOrderTx(tx, o, o.UserID, amount, "ERC20 deposit")
	})
	if err != nil {
		return nil, err
	}
	s.afterCredit(order, true)
	return order, nil
}

// RejectDeposit turns down a submitted ERC20 deposit.
func (s *Service) RejectDeposit(id uint64) (*model.DepositOrder, error) {
	var o model.DepositOrder
	res := s.st.DB.Model(&o).Where("id = ? AND status = ?", id, DepositPending).Update("status", DepositRejected)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, userErr("这笔充值已处理")
	}
	err := s.st.DB.First(&o, id).Error
	return &o, err
}
