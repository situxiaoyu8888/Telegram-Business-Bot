package trongrid

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"time"

	"telegram-business-bot/internal/lease"
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
	// One instance scans at a time, so replicas do not multiply TronGrid calls.
	if w.st.Redis != nil {
		err := lease.Run(ctx, w.st.Redis, "tron:watch", func(ctx context.Context) error { w.scanAll(ctx); return nil })
		if err != nil && !errors.Is(err, lease.ErrBusy) {
			w.logger.Warn("tron watcher lease", zap.Error(err))
		}
		return
	}
	w.scanAll(ctx)
}

func (w *Watcher) scanAll(ctx context.Context) {
	// Each held address is scanned once. One held by a single user credits that user; deposits to
	// one shared by several wait unclaimed until a user's claim is confirmed in the console.
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
		if ctx.Err() != nil {
			return
		}
		// An address that ever had another holder is never credited automatically (the old holder
		// may still send to it), nor are transfers made before its holder got it.
		users, heldFrom := a.Users, time.Time{}
		if users == 1 {
			var holders []model.DepositAddressHolder
			if err := w.st.DB.Where("network = 'TRC20' AND address = ?", a.Address).Find(&holders).Error; err != nil {
				w.logger.Error("load deposit address holders", zap.Error(err))
				continue
			}
			switch {
			case len(holders) > 1, len(holders) == 1 && holders[0].UserID != a.UserID:
				users = 2
			case len(holders) == 1:
				heldFrom = holders[0].AssignedAt
			}
		}
		if checkpoint, ok := w.scanAddress(ctx, a.Address, a.ScanTimestamp, users, a.UserID, heldFrom); ok && checkpoint > a.ScanTimestamp {
			// Rows reset for a rescan meanwhile (below the scanned start) keep their reset.
			if err := w.st.DB.Model(&model.DepositAddress{}).Where("network='TRC20' AND address=? AND scan_timestamp >= ? AND scan_timestamp < ?", a.Address, a.ScanTimestamp, checkpoint).Update("scan_timestamp", checkpoint).Error; err != nil {
				w.logger.Error("save scan progress", zap.Error(err))
			}
		}
	}
	// Pool addresses once held but now held by nobody stay watched: late deposits to them wait
	// unclaimed instead of being lost (or credited to a later holder).
	var retired []model.DepositPoolAddress
	if err := w.st.DB.Where("network = 'TRC20' AND assigned_at IS NOT NULL AND address NOT IN (?)",
		w.st.DB.Model(&model.DepositAddress{}).Select("address").Where("network = 'TRC20' AND status = 1")).Find(&retired).Error; err != nil {
		w.logger.Error("load retired deposit addresses", zap.Error(err))
		return
	}
	for _, p := range retired {
		if ctx.Err() != nil {
			return
		}
		if checkpoint, ok := w.scanAddress(ctx, p.Address, p.ScanTimestamp, 0, 0, time.Time{}); ok && checkpoint > p.ScanTimestamp {
			if err := w.st.DB.Model(&model.DepositPoolAddress{}).Where("id = ?", p.ID).Update("scan_timestamp", checkpoint).Error; err != nil {
				w.logger.Error("save scan progress", zap.Error(err))
			}
		}
	}
}

// scanAddress records the deposits to address since its checkpoint: credited to owner when it
// is the only holder (users == 1) and the transfer came after heldFrom, otherwise unclaimed. It returns the new checkpoint and whether every transfer was recorded.
func (w *Watcher) scanAddress(ctx context.Context, address string, scanned, users int64, owner uint64, heldFrom time.Time) (int64, bool) {
	since := scanned - (48 * time.Hour).Milliseconds()
	if since < 0 {
		since = 0
	}
	trs, err := w.client.AccountTRC20(ctx, address, w.contract, w.confirmOnly, since)
	if err != nil {
		w.logger.Warn("trongrid poll failed", zap.String("address", address), zap.Error(err))
		return scanned, false
	}
	checkpoint, complete := scanned, true
	seen := map[string]int{} // identical transfers within one transaction
	for _, t := range trs {
		if t.To != address || t.TokenInfo.Address != w.contract || t.Type != "Transfer" || t.TransactionID == "" {
			continue
		}
		amount, ok := rawToDecimal(t.Value, t.TokenInfo.Decimals)
		if !ok || !amount.IsPositive() {
			continue
		}
		base := wallet.TRC20EventKey(t.TransactionID, address, amount, 0)
		key := wallet.TRC20EventKey(t.TransactionID, address, amount, seen[base])
		seen[base]++
		order := model.DepositOrder{UserID: owner, Network: "TRC20", Currency: "USDT", Address: address, FromAddress: t.From, Amount: amount.StringFixed(6), TxID: t.TransactionID, EventKey: &key, BlockTimestamp: t.BlockTimestamp, Status: "confirmed"}
		if users == 1 && !time.UnixMilli(t.BlockTimestamp).Before(heldFrom) {
			// Referral commission is paid by the wallet's deposit hook.
			err = w.wallet.CreditToHolder(order)
		} else {
			err = w.wallet.RecordUnclaimed(order)
		}
		if err != nil {
			complete = false
			w.logger.Error("record deposit", zap.String("txid", t.TransactionID), zap.Error(err))
			continue
		}
		if t.BlockTimestamp > checkpoint {
			checkpoint = t.BlockTimestamp
		}
	}
	return checkpoint, complete
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
