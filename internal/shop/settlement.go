package shop

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

// Settlement statuses.
const (
	SettlementPending = "pending"
	SettlementPaid    = "paid"
)

// recordSale adds the merchant's ledger entry for an order that has just completed. The amount is
// what the buyer paid minus refunds so far; later refunds are recorded by recordRefund.
func (s *Service) recordSale(tx *gorm.DB, orderID uint64) error {
	var o model.ShopOrder
	if err := tx.First(&o, orderID).Error; err != nil {
		return err
	}
	gross := amount(o.PayAmount).Sub(amount(o.RefundedAmount))
	if !gross.IsPositive() {
		return nil
	}
	var m model.Merchant
	if err := tx.Select("id", "commission_rate").First(&m, o.MerchantID).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	rate := amount(m.CommissionRate)
	commission := gross.Mul(rate).Truncate(6)
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ShopMerchantEntry{EntryKey: fmt.Sprintf("sale:%d", o.ID), MerchantID: o.MerchantID, OrderID: o.ID, OrderNo: o.OrderNo, Type: "sale", Amount: gross.StringFixed(6), Commission: commission.StringFixed(6), Net: gross.Sub(commission).StringFixed(6), Rate: rate.StringFixed(6)}).Error
}

// recordRefund reverses part of a completed order's sale entry at the rate the sale was booked at.
func (s *Service) recordRefund(tx *gorm.DB, o model.ShopOrder, afterSaleID uint64, refund decimal.Decimal) error {
	var sale model.ShopMerchantEntry
	err := tx.Where("entry_key = ?", fmt.Sprintf("sale:%d", o.ID)).First(&sale).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil // the order completed with nothing left to pay out
	}
	if err != nil {
		return err
	}
	rate := amount(sale.Rate)
	commission := refund.Mul(rate).Truncate(6)
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ShopMerchantEntry{EntryKey: fmt.Sprintf("refund:%d", afterSaleID), MerchantID: o.MerchantID, OrderID: o.ID, OrderNo: o.OrderNo, Type: "refund", Amount: refund.Neg().StringFixed(6), Commission: commission.Neg().StringFixed(6), Net: refund.Sub(commission).Neg().StringFixed(6), Rate: sale.Rate}).Error
}

// MerchantBalance sums a merchant's unsettled ledger entries.
type MerchantBalance struct {
	MerchantID   uint64 `json:"merchant_id"`
	MerchantName string `json:"merchant_name"`
	Rate         string `json:"commission_rate"`
	Entries      int64  `json:"entries"`
	Amount       string `json:"amount"`
	Commission   string `json:"commission"`
	Net          string `json:"net"`
	SettledNet   string `json:"settled_net"`
	PendingNet   string `json:"pending_net"` // in settlements not yet marked paid
}

// MerchantBalances lists every merchant (or the scoped one) with unsettled and settled totals.
func (s *Service) MerchantBalances(merchantID uint64) ([]MerchantBalance, error) {
	var merchants []model.Merchant
	q := s.st.DB.Select("id", "name", "commission_rate").Order("id asc")
	if merchantID != 0 {
		q = q.Where("id = ?", merchantID)
	}
	if err := q.Find(&merchants).Error; err != nil {
		return nil, err
	}
	var open []struct {
		MerchantID uint64
		N          int64
		Amount     string
		Commission string
		Net        string
	}
	if err := s.st.DB.Model(&model.ShopMerchantEntry{}).Select("merchant_id, COUNT(*) AS n, SUM(amount) AS amount, SUM(commission) AS commission, SUM(net) AS net").Where("settlement_id = 0").Group("merchant_id").Scan(&open).Error; err != nil {
		return nil, err
	}
	var settled []struct {
		MerchantID uint64
		Status     string
		Net        string
	}
	if err := s.st.DB.Model(&model.ShopSettlement{}).Select("merchant_id, status, SUM(net) AS net").Group("merchant_id, status").Scan(&settled).Error; err != nil {
		return nil, err
	}
	out := make([]MerchantBalance, 0, len(merchants))
	for _, m := range merchants {
		b := MerchantBalance{MerchantID: m.ID, MerchantName: m.Name, Rate: amount(m.CommissionRate).StringFixed(6), Amount: "0.000000", Commission: "0.000000", Net: "0.000000", SettledNet: "0.000000", PendingNet: "0.000000"}
		for _, o := range open {
			if o.MerchantID == m.ID {
				b.Entries, b.Amount, b.Commission, b.Net = o.N, amount(o.Amount).StringFixed(6), amount(o.Commission).StringFixed(6), amount(o.Net).StringFixed(6)
			}
		}
		for _, st := range settled {
			if st.MerchantID != m.ID {
				continue
			}
			if st.Status == SettlementPaid {
				b.SettledNet = amount(st.Net).StringFixed(6)
			} else {
				b.PendingNet = amount(st.Net).StringFixed(6)
			}
		}
		out = append(out, b)
	}
	return out, nil
}

// MerchantEntries lists ledger entries, newest first. settlementID 0 lists unsettled entries.
func (s *Service) MerchantEntries(merchantID, settlementID uint64) ([]model.ShopMerchantEntry, error) {
	rows := []model.ShopMerchantEntry{}
	q := s.st.DB.Where("settlement_id = ?", settlementID).Order("id desc").Limit(1000)
	if merchantID != 0 {
		q = q.Where("merchant_id = ?", merchantID)
	}
	err := q.Find(&rows).Error
	return rows, err
}

// CreateSettlement moves all of a merchant's unsettled entries into a new pending settlement.
// A negative balance (refunds exceeding sales) stays unsettled until later sales cover it.
func (s *Service) CreateSettlement(merchantID uint64, note, operator string) (*model.ShopSettlement, error) {
	if err := checkLen(note, "备注", 512, false); err != nil {
		return nil, err
	}
	var st model.ShopSettlement
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var entries []model.ShopMerchantEntry
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("merchant_id = ? AND settlement_id = 0", merchantID).Find(&entries).Error; err != nil {
			return err
		}
		total, commission, net := decimal.Zero, decimal.Zero, decimal.Zero
		ids := []uint64{}
		for _, e := range entries {
			total, commission, net = total.Add(amount(e.Amount)), commission.Add(amount(e.Commission)), net.Add(amount(e.Net))
			ids = append(ids, e.ID)
		}
		if !net.IsPositive() {
			return userErr("该商家没有可结算的金额")
		}
		st = model.ShopSettlement{MerchantID: merchantID, Amount: total.StringFixed(6), Commission: commission.StringFixed(6), Net: net.StringFixed(6), EntryCount: int64(len(ids)), Status: SettlementPending, Note: strings.TrimSpace(note), CreatedBy: operator}
		if err := tx.Create(&st).Error; err != nil {
			return err
		}
		return tx.Model(&model.ShopMerchantEntry{}).Where("id IN ?", ids).Update("settlement_id", st.ID).Error
	})
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// MarkSettlementPaid records that the platform paid a pending settlement outside the system.
func (s *Service) MarkSettlementPaid(id uint64, payRef, note, operator string) error {
	if err := checkLen(payRef, "打款凭证", 255, true); err != nil {
		return err
	}
	if err := checkLen(note, "备注", 512, false); err != nil {
		return err
	}
	var st model.ShopSettlement
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&st, id).Error; err != nil {
			return userErr("结算单不存在")
		}
		if st.Status != SettlementPending {
			return userErr("结算单已打款")
		}
		updates := map[string]any{"status": SettlementPaid, "pay_ref": strings.TrimSpace(payRef), "paid_by": operator, "paid_at": time.Now()}
		if n := strings.TrimSpace(note); n != "" {
			updates["note"] = n
		}
		return tx.Model(&model.ShopSettlement{}).Where("id = ?", st.ID).Updates(updates).Error
	})
	if err == nil {
		s.notifyStaff(st.MerchantID, fmt.Sprintf("💸 <b>商城结算已打款</b>\n\n结算单：#%d\n金额：<code>%s USDT</code>\n凭证：<code>%s</code>", st.ID, amount(st.Net).String(), htmlEscape(strings.TrimSpace(payRef))))
	}
	return err
}

// Settlements lists settlements, newest first, limited to a merchant when scoped.
func (s *Service) Settlements(merchantID uint64) ([]model.ShopSettlement, error) {
	rows := []model.ShopSettlement{}
	q := s.st.DB.Order("id desc").Limit(500)
	if merchantID != 0 {
		q = q.Where("merchant_id = ?", merchantID)
	}
	err := q.Find(&rows).Error
	return rows, err
}
