package integration

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

func exchangeFixture(t *testing.T, fee string) (*gorm.DB, *wallet.Service, uint64) {
	db := testDatabase(t)
	for _, c := range store.DefaultCurrencies {
		switch c.Code {
		case "CNY":
			c.Withdrawable = true
		case "THB":
			c.Exchangeable = false
		}
		must(t, db.Create(&c).Error)
	}
	must(t, db.Create(&model.SystemConfig{Key: "exchange_fee_rate", Value: fee}).Error)
	w := wallet.New(&store.Store{DB: db}, "USDT", "1", "0")
	u := model.TelegramUser{TelegramUserID: 7001, ReferralCode: "ex7001", Status: 1}
	must(t, db.Create(&u).Error)
	must(t, w.CreditDeposit(model.DepositOrder{UserID: u.ID, Network: "TRC20", Address: "addr", Amount: "100", TxID: "tx-ex"}))
	return db, w, u.ID
}

func balanceOf(t *testing.T, w *wallet.Service, userID uint64, code string) string {
	t.Helper()
	list, err := w.Balances(userID)
	must(t, err)
	for _, b := range list {
		if b.Code == code {
			d, _ := decimal.NewFromString(b.Available)
			return d.String()
		}
	}
	t.Fatalf("no %s balance", code)
	return ""
}

func usdtToCNY(amount, requestID string) wallet.ExchangeRequest {
	return wallet.ExchangeRequest{From: "USDT", To: "CNY", Amount: amount, FromRate: "1", ToRate: "7.2", FeeRate: "0.01", RequestID: requestID}
}

func TestExchangeAppliesRateFeeAndIsIdempotent(t *testing.T) {
	db, w, uid := exchangeFixture(t, "0.01")
	rec, err := w.Exchange(uid, usdtToCNY("10", "ex-1"))
	must(t, err)
	// 10 × 7.2 × (1 − 1%) = 71.28 CNY
	if got := balanceOf(t, w, uid, "USDT") + " " + balanceOf(t, w, uid, "CNY"); got != "90 71.28" || !strings.HasPrefix(rec.Received, "71.28") {
		t.Fatalf("balances %s, received %s", got, rec.Received)
	}
	again, err := w.Exchange(uid, usdtToCNY("10", "ex-1"))
	must(t, err)
	if again.ID != rec.ID || balanceOf(t, w, uid, "USDT") != "90" {
		t.Fatal("repeated request exchanged twice")
	}
	var ledger int64
	must(t, db.Model(&model.WalletTransaction{}).Where("ref_type = ? AND ref_id = ?", "exchange", rec.ID).Count(&ledger).Error)
	if ledger != 2 {
		t.Fatalf("ledger entries %d", ledger)
	}
	// Received amounts are truncated to the target currency's decimals (CNY: 2).
	rec, err = w.Exchange(uid, usdtToCNY("0.123456", "ex-2"))
	must(t, err)
	if rec.Received != "0.870000" { // 0.123456 × 7.128 = 0.879994… → 0.87
		t.Fatalf("received %s", rec.Received)
	}
}

func TestExchangeRefusesStaleRatesBadAmountsAndOverdraft(t *testing.T) {
	_, w, uid := exchangeFixture(t, "0.01")
	stale := usdtToCNY("10", "ex-stale")
	stale.ToRate = "7.1"
	for name, req := range map[string]wallet.ExchangeRequest{
		"stale rate":   stale,
		"stale fee":    {From: "USDT", To: "CNY", Amount: "10", FromRate: "1", ToRate: "7.2", FeeRate: "0", RequestID: "ex-fee"},
		"overdraft":    usdtToCNY("100.5", "ex-over"),
		"same":         {From: "USDT", To: "USDT", Amount: "1", FromRate: "1", ToRate: "1", FeeRate: "0.01", RequestID: "ex-same"},
		"closed":       {From: "USDT", To: "THB", Amount: "1", FromRate: "1", ToRate: "33", FeeRate: "0.01", RequestID: "ex-thb"},
		"cny decimals": {From: "CNY", To: "USDT", Amount: "1.234", FromRate: "7.2", ToRate: "1", FeeRate: "0.01", RequestID: "ex-dec"},
	} {
		if _, err := w.Exchange(uid, req); err == nil {
			t.Fatalf("%s: exchange accepted", name)
		}
	}
	if balanceOf(t, w, uid, "USDT") != "100" || balanceOf(t, w, uid, "CNY") != "0" {
		t.Fatal("refused exchanges changed balances")
	}
}

func TestConcurrentExchangesNeverOverdraw(t *testing.T) {
	_, w, uid := exchangeFixture(t, "0")
	_, err := w.Exchange(uid, wallet.ExchangeRequest{From: "USDT", To: "CNY", Amount: "10", FromRate: "1", ToRate: "7.2", FeeRate: "0", RequestID: "seed"})
	must(t, err)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Opposite directions at once must neither deadlock nor overdraw.
			req := wallet.ExchangeRequest{From: "USDT", To: "CNY", Amount: "15", FromRate: "1", ToRate: "7.2", FeeRate: "0", RequestID: fmt.Sprintf("c-%d", i)}
			if i%3 == 0 {
				req = wallet.ExchangeRequest{From: "CNY", To: "USDT", Amount: "7.2", FromRate: "7.2", ToRate: "1", FeeRate: "0", RequestID: fmt.Sprintf("c-%d", i)}
			}
			if _, err := w.Exchange(uid, req); err == nil && i%3 != 0 {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	usdt, _ := decimal.NewFromString(balanceOf(t, w, uid, "USDT"))
	cny, _ := decimal.NewFromString(balanceOf(t, w, uid, "CNY"))
	if usdt.IsNegative() || cny.IsNegative() {
		t.Fatalf("negative balance: USDT %s CNY %s", usdt, cny)
	}
	// Value is conserved at a zero fee: USDT + CNY/7.2 stays 100.
	if total := usdt.Add(cny.Div(decimal.NewFromFloat(7.2))); !total.Round(4).Equal(decimal.NewFromInt(100)) {
		t.Fatalf("value not conserved: %s (USDT %s, CNY %s, %d out)", total, usdt, cny, ok)
	}
}

func TestAdminAdjustAndFiatWithdrawal(t *testing.T) {
	db, w, uid := exchangeFixture(t, "0")
	if err := w.AdminAdjust(uid, "CNY", "-1", "admin", "扣减"); err == nil {
		t.Fatal("debit below zero accepted")
	}
	must(t, w.AdminAdjust(uid, "CNY", "500", "admin", "现金充值"))
	if err := w.AdminAdjust(uid, "THB", "1.001", "admin", "x"); err == nil {
		t.Fatal("too many decimals accepted")
	}
	if _, err := w.CreateCurrencyWithdrawal(uid, "CNY", "工商银行 张三 6222", "50", "wd-low"); err == nil {
		t.Fatal("withdrawal below the minimum accepted")
	}
	o, err := w.CreateCurrencyWithdrawal(uid, "CNY", "工商银行 张三 6222", "200", "wd-1")
	must(t, err)
	if o.Network != "BANK" || o.Currency != "CNY" || balanceOf(t, w, uid, "CNY") != "300" {
		t.Fatalf("order %+v balance %s", o, balanceOf(t, w, uid, "CNY"))
	}
	if _, err := w.CreateCurrencyWithdrawal(uid, "THB", "Bangkok Bank 123", "500", "wd-thb"); err == nil {
		t.Fatal("withdrawal of a non-withdrawable currency accepted")
	}
	must(t, w.RejectWithdrawal(o.ID, "admin", "测试"))
	if balanceOf(t, w, uid, "CNY") != "500" {
		t.Fatal("rejected withdrawal not returned")
	}
	var adj int64
	must(t, db.Model(&model.WalletTransaction{}).Where("user_id = ? AND type = ?", uid, "admin_credit").Count(&adj).Error)
	if adj != 1 {
		t.Fatalf("admin ledger entries %d", adj)
	}
}

func TestUSDTWithdrawalFollowsTheConsoleSwitch(t *testing.T) {
	db, w, uid := exchangeFixture(t, "0")
	must(t, db.Model(&model.WalletCurrency{}).Where("code = ?", "USDT").Update("withdrawable", false).Error)
	if _, err := w.CreateCurrencyWithdrawal(uid, "USDT", "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7", "10", "wd-off"); err == nil {
		t.Fatal("USDT withdrawal accepted while switched off")
	}
	must(t, db.Model(&model.WalletCurrency{}).Where("code = ?", "USDT").Update("withdrawable", true).Error)
	if _, err := w.CreateCurrencyWithdrawal(uid, "USDT", "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7", "10", "wd-on"); err != nil {
		t.Fatal(err)
	}
}
