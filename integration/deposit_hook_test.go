package integration

import (
	"testing"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

func TestDepositHookRunsOncePerCreditedDeposit(t *testing.T) {
	db := testDatabase(t)
	var seen []model.DepositOrder
	w := wallet.New(&store.Store{DB: db}, "USDT", "1", "0").WithDepositHook(func(o model.DepositOrder, _ bool) { seen = append(seen, o) })
	u := model.TelegramUser{TelegramUserID: 7101, ReferralCode: "dep7101", Status: 1}
	must(t, db.Create(&u).Error)
	order := model.DepositOrder{UserID: u.ID, Network: "TRC20", Address: "addr", Amount: "12.5", TxID: "tx-hook"}
	must(t, w.CreditDeposit(order))
	must(t, w.CreditDeposit(order)) // the watcher sees the same transfer again
	if len(seen) != 1 || seen[0].ID == 0 || seen[0].Currency != "USDT" || seen[0].Amount != "12.5" {
		t.Fatalf("hook calls %+v", seen)
	}
}
