package trongrid

import (
	"context"
	"math/big"
	"strings"
	"time"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/referral"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"
)

type Watcher struct {
	st          *store.Store
	wallet      *wallet.Service
	referral    *referral.Service
	client      *Client
	contract    string
	confirmOnly bool
	interval    time.Duration
	logger      *zap.Logger
}

func NewWatcher(st *store.Store, w *wallet.Service, ref *referral.Service, c *Client, contract string, confirmOnly bool, interval time.Duration, logger *zap.Logger) *Watcher {
	return &Watcher{st: st, wallet: w, referral: ref, client: c, contract: contract, confirmOnly: confirmOnly, interval: interval, logger: logger}
}
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	w.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.scan(ctx)
		}
	}
}
func (w *Watcher) scan(ctx context.Context) {
	if strings.TrimSpace(w.contract) == "" {
		w.logger.Warn("TRON watcher enabled but TRON_USDT_CONTRACT is empty")
		return
	}
	// Each address is scanned once; one used by several users (shared) records its deposits as
	// unclaimed for the sender to claim, since the chain does not say whose they are.
	var addresses []struct {
		Address       string
		ScanTimestamp int64
		Users         int64
		UserID        uint64
	}
	if err := w.st.DB.Model(&model.DepositAddress{}).Select("address, MIN(scan_timestamp) AS scan_timestamp, COUNT(DISTINCT user_id) AS users, MIN(user_id) AS user_id").
		Where("network='TRC20' AND status=1").Group("address").Scan(&addresses).Error; err != nil {
		w.logger.Error("load deposit addresses", zap.Error(err))
		return
	}
	for _, a := range addresses {
		since := a.ScanTimestamp - (48 * time.Hour).Milliseconds()
		if since < 0 {
			since = 0
		}
		checkpoint := a.ScanTimestamp
		complete := true
		trs, err := w.client.AccountTRC20(ctx, a.Address, w.contract, w.confirmOnly, since)
		if err != nil {
			w.logger.Warn("trongrid poll failed", zap.String("address", a.Address), zap.Error(err))
			continue
		}
		for _, t := range trs {
			if t.To != a.Address || t.TokenInfo.Address != w.contract || t.Type != "Transfer" || t.TransactionID == "" {
				continue
			}
			amount, ok := rawToDecimal(t.Value, t.TokenInfo.Decimals)
			if !ok || !amount.IsPositive() {
				continue
			}
			order := model.DepositOrder{UserID: a.UserID, Network: "TRC20", Currency: "USDT", Address: a.Address, FromAddress: t.From, Amount: amount.StringFixed(6), TxID: t.TransactionID, BlockTimestamp: t.BlockTimestamp, Status: "confirmed"}
			if a.Users > 1 {
				if err := w.wallet.RecordUnclaimed(order); err != nil {
					complete = false
					w.logger.Error("record unclaimed deposit", zap.String("txid", t.TransactionID), zap.Error(err))
				} else if t.BlockTimestamp > checkpoint {
					checkpoint = t.BlockTimestamp
				}
				continue
			}
			if err := w.wallet.CreditDeposit(order); err != nil {
				complete = false
				w.logger.Error("credit deposit", zap.String("txid", t.TransactionID), zap.Error(err))
				continue
			}
			var saved model.DepositOrder
			err = w.st.DB.Where("tx_id=?", t.TransactionID).First(&saved).Error
			if err == nil {
				err = w.referral.AwardFromDeposit(saved.ID, saved.UserID, saved.Amount, saved.Currency)
			}
			if err != nil {
				complete = false
				w.logger.Error("award commission", zap.Error(err))
				continue
			}
			if t.BlockTimestamp > checkpoint {
				checkpoint = t.BlockTimestamp
			}
		}
		// Rows reset for a rescan meanwhile (below the scanned start) keep their reset.
		if complete && checkpoint > a.ScanTimestamp {
			if err := w.st.DB.Model(&model.DepositAddress{}).Where("network='TRC20' AND address=? AND scan_timestamp >= ? AND scan_timestamp < ?", a.Address, a.ScanTimestamp, checkpoint).Update("scan_timestamp", checkpoint).Error; err != nil {
				w.logger.Error("save scan progress", zap.Error(err))
			}
		}
	}
}
func rawToDecimal(raw string, decimals int) (decimal.Decimal, bool) {
	if decimals < 0 || decimals > 30 {
		return decimal.Zero, false
	}
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok || n.Sign() < 0 {
		return decimal.Zero, false
	}
	d := decimal.NewFromBigInt(n, 0)
	if decimals > 0 {
		d = d.Shift(int32(-decimals))
	}
	return d.Truncate(6), true
}
