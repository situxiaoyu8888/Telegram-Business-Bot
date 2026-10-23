package wallet

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// USDT deposits arrive on chain. A transfer to an address only one user holds is credited by the
// TRON watcher at once. Everything else is claimed: the user submits the transaction hash (and,
// for ERC20, the amount), which records a "pending" claim, and the console confirms it (with the
// verified amount) or rejects it. A transaction hash is public, so several users may claim the
// same one; only one claim per transfer can be credited. Transfers to shared or no longer held
// addresses that the watcher sees are kept as "unclaimed": the console can assign one directly,
// and confirming a claim for it marks it "merged".

// Deposit statuses besides "credited".
const (
	DepositUnclaimed = "unclaimed"
	DepositPending   = "pending"
	DepositRejected  = "rejected"
	DepositMerged    = "merged" // a watcher record superseded by a confirmed claim
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

var errSeen = errors.New("deposit already recorded")

// TRC20EventKey identifies one TRC20 transfer; n counts identical transfers in one transaction.
func TRC20EventKey(txid, to string, amount decimal.Decimal, n int) string {
	return fmt.Sprintf("trc20:%s:%s:%s:%d", strings.ToLower(txid), to, amount.StringFixed(6), n)
}

// alreadyCredited reports whether a transfer of txid (to address, or to any address when
// address is empty) has been credited by any path: the watcher, a confirmed claim or an
// assignment.
func alreadyCredited(tx *gorm.DB, network, txid, address string) (bool, error) {
	q := tx.Model(&model.DepositOrder{}).Where("tx_id = ? AND network = ? AND status = 'credited'", strings.ToLower(txid), network)
	if address != "" {
		q = q.Where("address = ?", address)
	}
	var n int64
	err := q.Count(&n).Error
	return n > 0, err
}

// soleHolder reports whether userID is the only user an address was ever given to (or, for an
// address from before holders were recorded, its only current user), holding it since before at.
func soleHolder(tx *gorm.DB, network, address string, userID uint64, at time.Time) (bool, error) {
	var holders []model.DepositAddressHolder
	if err := tx.Where("network = ? AND address = ?", network, address).Find(&holders).Error; err != nil {
		return false, err
	}
	switch {
	case len(holders) > 1:
		return false, nil
	case len(holders) == 1:
		return holders[0].UserID == userID && !at.Before(holders[0].AssignedAt), nil
	}
	var users []uint64
	if err := tx.Model(&model.DepositAddress{}).Where("network = ? AND address = ? AND status = 1", network, address).Distinct().Pluck("user_id", &users).Error; err != nil {
		return false, err
	}
	return len(users) == 1 && users[0] == userID, nil
}

// lockTransfer locks the deposit records of one transaction (and, through the index gap, keeps
// new ones from being inserted meanwhile), so the watcher, claims and assignments of the same
// transfer run one after another. It must be the transaction's first statement.
func lockTransfer(tx *gorm.DB, network, txid string) error {
	var ids []uint64
	return tx.Model(&model.DepositOrder{}).Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("tx_id = ? AND network = ?", strings.ToLower(txid), network).Pluck("id", &ids).Error
}

// watcherRecords are the TRON watcher's records of a transaction (not users' claims).
func watcherRecords(tx *gorm.DB, txid string) ([]model.DepositOrder, error) {
	var list []model.DepositOrder
	err := tx.Where("tx_id = ? AND network = 'TRC20' AND event_key LIKE 'trc20:%'", strings.ToLower(txid)).Order("id asc").Find(&list).Error
	return list, err
}

// heldBy reports whether the user holds, or once held, a deposit address.
func heldBy(tx *gorm.DB, userID uint64, network, address string) (bool, error) {
	var n int64
	if err := tx.Model(&model.DepositAddressHolder{}).Where("user_id = ? AND network = ? AND address = ?", userID, network, address).Count(&n).Error; err != nil || n > 0 {
		return n > 0, err
	}
	err := tx.Model(&model.DepositAddress{}).Where("user_id = ? AND network = ? AND address = ?", userID, network, address).Count(&n).Error
	return n > 0, err
}

// RecordUnclaimed stores a deposit nobody can be credited with automatically; a transfer seen
// before, or already credited through a confirmed claim, is ignored.
func (s *Service) RecordUnclaimed(order model.DepositOrder) error {
	order.TxID = strings.ToLower(order.TxID)
	if order.EventKey == nil {
		amount, _ := decimal.NewFromString(order.Amount)
		key := TRC20EventKey(order.TxID, order.Address, amount, 0)
		order.EventKey = &key
	}
	order.ID, order.UserID, order.Status, order.Currency = 0, 0, DepositUnclaimed, s.currency
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockTransfer(tx, "TRC20", order.TxID); err != nil {
			return err
		}
		return tx.Create(&order).Error
	})
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
	return tx.Model(o).Updates(map[string]any{"user_id": userID, "amount": o.Amount, "status": "credited", "reviewed_by": o.ReviewedBy}).Error
}

func (s *Service) afterCredit(o *model.DepositOrder, announce bool) {
	if s.onDeposit != nil {
		s.onDeposit(*o, announce)
	}
}

// SubmitClaim records a user's claim of an on-chain USDT deposit by its transaction hash, for the
// console to confirm. When the TRON watcher has seen the transaction, the claim must be for a
// transfer to an address the user holds or held, not yet credited, and takes its amount; an
// ERC20 claim carries the amount the user gives.
func (s *Service) SubmitClaim(userID uint64, hash, amountText string) (*model.DepositOrder, error) {
	hash = strings.ToLower(strings.TrimSpace(hash))
	network := DepositHashNetwork(hash)
	if network == "" {
		return nil, userErr("交易哈希格式不正确：TRC20 为 64 位字符，ERC20 为 0x 开头的 66 位字符")
	}
	var addrs []string
	if err := s.st.DB.Model(&model.DepositAddress{}).Where("user_id = ? AND network = ? AND status = 1", userID, network).Pluck("address", &addrs).Error; err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, userErr("你还没有 USDT-%s 充值地址，请联系客服", network)
	}
	o := model.DepositOrder{UserID: userID, Network: network, Address: addrs[0], TxID: hash, Status: DepositPending}
	if network == "ERC20" {
		amount, err := decimal.NewFromString(strings.TrimSpace(amountText))
		if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(6)) {
			return nil, userErr("请输入正确的充值金额（最多 6 位小数）")
		}
		o.Currency, o.Amount = ERC20Currency, amount.StringFixed(6)
		done, err := alreadyCredited(s.st.DB, network, hash, "")
		if err != nil {
			return nil, err
		}
		if done {
			return nil, userErr("这笔交易已经入账")
		}
	} else {
		o.Currency, o.Amount = s.currency, "0"
		seen, err := watcherRecords(s.st.DB, hash)
		if err != nil {
			return nil, err
		}
		var mine *model.DepositOrder
		for i := range seen {
			ok, err := heldBy(s.st.DB, userID, network, seen[i].Address)
			if err != nil {
				return nil, err
			}
			if ok && (mine == nil || seen[i].Status == DepositUnclaimed) {
				mine = &seen[i]
			}
		}
		switch {
		case len(seen) > 0 && mine == nil:
			return nil, userErr("这笔交易不是转入你的充值地址，如有疑问请联系客服")
		case mine != nil && mine.Status != DepositUnclaimed:
			return nil, userErr("这笔充值已经入账")
		case mine != nil:
			o.Address, o.Amount, o.FromAddress, o.BlockTimestamp = mine.Address, mine.Amount, mine.FromAddress, mine.BlockTimestamp
		default:
			done, err := alreadyCredited(s.st.DB, network, hash, "")
			if err != nil {
				return nil, err
			}
			if done {
				return nil, userErr("这笔交易已经入账")
			}
		}
	}
	// Each user's claim is its own record, so nobody can block a hash by submitting it first.
	key := fmt.Sprintf("claim:%s:%s:%d", strings.ToLower(network), hash, userID)
	o.EventKey = &key
	if err := s.st.DB.Create(&o).Error; err != nil {
		if store.IsDuplicate(err) {
			return nil, userErr("这笔交易你已经提交过了，请等待客服核对")
		}
		return nil, err
	}
	return &o, nil
}

// orderTx looks up which transaction a deposit record belongs to (outside any transaction), so
// a transaction can lock that transfer's records as its first statement.
func (s *Service) orderTx(id uint64) (network, txid string, err error) {
	var o model.DepositOrder
	if err := s.st.DB.Select("id", "tx_id", "network").First(&o, id).Error; err != nil {
		return "", "", err
	}
	return o.Network, o.TxID, nil
}

// lockOrderIn locks the deposit records of a transaction, then returns the order (read under
// the lock, so every check after it sees what other paths committed).
func lockOrderIn(tx *gorm.DB, id uint64, network, txid string) (*model.DepositOrder, error) {
	if err := lockTransfer(tx, network, txid); err != nil {
		return nil, err
	}
	var o model.DepositOrder
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&o, id).Error
	return &o, err
}

// AssignDeposit credits an unclaimed deposit to a user chosen in the console; the user must hold
// or have held the receiving address.
func (s *Service) AssignDeposit(id, userID uint64, operator string) (*model.DepositOrder, error) {
	network, txid, err := s.orderTx(id)
	if err != nil {
		return nil, err
	}
	var order *model.DepositOrder
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		o, err := lockOrderIn(tx, id, network, txid)
		if err != nil {
			return err
		}
		if o.Status != DepositUnclaimed {
			return userErr("这笔到账已处理")
		}
		if ok, err := heldBy(tx, userID, o.Network, o.Address); err != nil || !ok {
			if err != nil {
				return err
			}
			return userErr("该用户从未持有这个充值地址，不能指定给他")
		}
		amount, err := decimal.NewFromString(o.Amount)
		if err != nil {
			return err
		}
		order = o
		o.ReviewedBy = operator
		return s.creditOrderTx(tx, o, userID, amount, "TRC20 deposit (assigned)")
	})
	if err != nil {
		return nil, err
	}
	s.afterCredit(order, true)
	return order, nil
}

// ConfirmDeposit credits a claim with the amount the console verified on chain. Only one claim
// per transfer can be credited; when the watcher has seen the transfer, the claim must match its
// record (address and amount), which is then marked merged.
func (s *Service) ConfirmDeposit(id uint64, amountText, operator string) (*model.DepositOrder, error) {
	amount, err := decimal.NewFromString(strings.TrimSpace(amountText))
	if err != nil || !amount.IsPositive() || !amount.Equal(amount.Truncate(6)) {
		return nil, userErr("请输入正确的到账金额（最多 6 位小数）")
	}
	network, txid, err := s.orderTx(id)
	if err != nil {
		return nil, err
	}
	var order *model.DepositOrder
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		o, err := lockOrderIn(tx, id, network, txid)
		if err != nil {
			return err
		}
		if o.Status != DepositPending {
			return userErr("这笔充值已处理")
		}
		if o.Network == "ERC20" {
			// The destination of an ERC20 claim is checked by people; one credit per transaction.
			if done, err := alreadyCredited(tx, o.Network, o.TxID, ""); err != nil || done {
				if err != nil {
					return err
				}
				return userErr("这笔交易已经入账（可能是其他提交人），不能重复入账")
			}
		} else {
			// A TRC20 claim is credited only against the watcher's record of the transfer, to an
			// address the claimant holds or held; the record is then merged, so every transfer is
			// credited once whichever path gets to it.
			seen, err := watcherRecords(tx, o.TxID)
			if err != nil {
				return err
			}
			if len(seen) == 0 {
				return userErr("系统还没有扫描到这笔转账，请稍后再确认（扫描通常在几分钟内完成）")
			}
			// One transaction may carry several transfers: the claim takes the one with its amount.
			var match *model.DepositOrder
			var held []string
			for i := range seen {
				if seen[i].Status != DepositUnclaimed || match != nil {
					continue
				}
				ok, err := heldBy(tx, o.UserID, o.Network, seen[i].Address)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				if decimal.RequireFromString(seen[i].Amount).Equal(amount) {
					match = &seen[i]
				} else {
					held = append(held, decimal.RequireFromString(seen[i].Amount).String())
				}
			}
			if match == nil && len(held) > 0 {
				return userErr("金额与链上记录不一致（链上为 %s）", strings.Join(held, "、"))
			}
			if match == nil {
				return userErr("链上记录显示这笔转账不是转入该用户持有过的地址，或已经入账")
			}
			if err := tx.Model(match).Updates(map[string]any{"status": DepositMerged, "reviewed_by": operator}).Error; err != nil {
				return err
			}
			o.Address, o.FromAddress, o.BlockTimestamp = match.Address, match.FromAddress, match.BlockTimestamp
			if err := tx.Model(o).Updates(map[string]any{"address": o.Address, "from_address": o.FromAddress, "block_timestamp": o.BlockTimestamp}).Error; err != nil {
				return err
			}
		}
		order = o
		o.ReviewedBy = operator
		return s.creditOrderTx(tx, o, o.UserID, amount, o.Network+" deposit (claimed)")
	})
	if err != nil {
		return nil, err
	}
	s.afterCredit(order, true)
	return order, nil
}

// RejectDeposit turns down a claim.
func (s *Service) RejectDeposit(id uint64, operator string) (*model.DepositOrder, error) {
	var o model.DepositOrder
	res := s.st.DB.Model(&o).Where("id = ? AND status = ?", id, DepositPending).Updates(map[string]any{"status": DepositRejected, "reviewed_by": operator})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected == 0 {
		return nil, userErr("这笔充值已处理")
	}
	err := s.st.DB.First(&o, id).Error
	return &o, err
}
