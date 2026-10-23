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

const (
	sharedAddr = "TA4Y62o6YC2Zsck9rZVGTvqW1AQ7X9zTnj"
	ownAddr    = "TA9pkx4DFxrEw8JZzUtyDrh2uAat1LDuJL"
	otherAddr  = "TAF8dttxK5iPKbvYC626aDBytrWANpLRXp"
)

func depositFixture(t *testing.T) (*gorm.DB, *wallet.Service, map[uint64]bool, func(int64, ...string) uint64) {
	db := testDatabase(t)
	for _, c := range store.DefaultCurrencies {
		must(t, db.Create(&c).Error)
	}
	announced := map[uint64]bool{}
	var mu sync.Mutex
	w := wallet.New(&store.Store{DB: db}, "USDT", "1", "0").WithDepositHook(func(o model.DepositOrder, announce bool) {
		mu.Lock()
		announced[o.ID] = announce
		mu.Unlock()
	})
	// user creates a user holding the given TRC20 addresses (ERC20 when one starts with 0x).
	user := func(tg int64, addrs ...string) uint64 {
		u := model.TelegramUser{TelegramUserID: tg, ReferralCode: fmt.Sprintf("dc%d", tg), Status: 1}
		must(t, db.Create(&u).Error)
		for _, a := range addrs {
			network := "TRC20"
			if strings.HasPrefix(a, "0x") {
				network = "ERC20"
			}
			must(t, db.Create(&model.DepositAddress{UserID: u.ID, Network: network, Address: a, Status: 1}).Error)
		}
		return u.ID
	}
	return db, w, announced, user
}

func hash64(ch string) string { return strings.Repeat(ch, 64) }

func TestClaimsAreConfirmedInTheConsoleOnce(t *testing.T) {
	db, w, announced, user := depositFixture(t)
	a, b := user(7201, sharedAddr), user(7202, sharedAddr)
	c, d := user(7203, otherAddr), user(7204, ownAddr)

	// The watcher saw a deposit to the shared address; it waits unclaimed.
	must(t, w.RecordUnclaimed(model.DepositOrder{Network: "TRC20", Address: sharedAddr, FromAddress: "Tsender", Amount: "10", TxID: hash64("a")}))
	if _, err := w.SubmitClaim(c, hash64("a"), ""); err == nil || !strings.Contains(err.Error(), "不是转入你的充值地址") {
		t.Fatal("claimed by a user of another address:", err)
	}
	// Anyone holding the address may claim the public hash; the console decides.
	squat, err := w.SubmitClaim(b, hash64("a"), "")
	must(t, err)
	own, err := w.SubmitClaim(a, strings.ToUpper(hash64("a")), "")
	must(t, err)
	if own.Status != wallet.DepositPending || own.Amount != "10.000000" || own.FromAddress != "Tsender" {
		t.Fatalf("claim %+v", own)
	}
	if _, err := w.SubmitClaim(a, hash64("a"), ""); err == nil {
		t.Fatal("same claim submitted twice")
	}
	if balanceOf(t, w, a, "USDT") != "0" {
		t.Fatal("a claim was credited before review")
	}
	if _, err := w.ConfirmDeposit(own.ID, "9", "admin"); err == nil || !strings.Contains(err.Error(), "链上") {
		t.Fatal("confirmed an amount different from the chain record:", err)
	}
	// Two operators confirming both claims at once: one transfer, one credit.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []uint64{own.ID, squat.ID} {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			_, err := w.ConfirmDeposit(id, "10", "admin")
			results <- err
		}(id)
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d concurrent confirmations succeeded", wins)
	}
	var winner model.DepositOrder
	must(t, db.Where("tx_id = ? AND status = 'credited'", hash64("a")).First(&winner).Error)
	if winner.ReviewedBy != "admin" || !announced[winner.ID] {
		t.Fatalf("confirmed %+v", winner)
	}
	if _, err := w.ConfirmDeposit(own.ID, "10", "admin"); err == nil {
		t.Fatal("one transfer credited twice")
	}
	var watcherRow model.DepositOrder
	must(t, db.Where("tx_id = ? AND user_id = 0", hash64("a")).First(&watcherRow).Error)
	if watcherRow.Status != wallet.DepositMerged {
		t.Fatal("watcher record not merged:", watcherRow.Status)
	}
	if _, err := w.AssignDeposit(watcherRow.ID, b, "admin"); err == nil {
		t.Fatal("merged watcher record assigned again")
	}
	must(t, w.RecordUnclaimed(model.DepositOrder{Network: "TRC20", Address: sharedAddr, Amount: "10", TxID: hash64("a")})) // seen again
	if got := balanceOf(t, w, a, "USDT") + " " + balanceOf(t, w, b, "USDT"); got != "10 0" && got != "0 10" {
		t.Fatalf("balances %s", got)
	}

	// A TRC20 claim cannot be confirmed before the watcher has seen the transfer (the console
	// could not know which address it went to); once seen, the watcher credits the sole holder
	// and the claim can no longer be confirmed.
	early, err := w.SubmitClaim(d, hash64("b"), "")
	must(t, err)
	if early.Amount != "0" {
		t.Fatal("unseen transfer got an amount:", early.Amount)
	}
	if _, err := w.ConfirmDeposit(early.ID, "5", "admin"); err == nil || !strings.Contains(err.Error(), "还没有扫描到") {
		t.Fatal("confirmed a transfer the watcher has not seen:", err)
	}
	must(t, w.CreditToHolder(model.DepositOrder{UserID: d, Network: "TRC20", Address: ownAddr, Amount: "5", TxID: hash64("b")}))
	if _, err := w.ConfirmDeposit(early.ID, "5", "admin"); err == nil {
		t.Fatal("an auto-credited transfer was confirmed again")
	}
	if got := balanceOf(t, w, d, "USDT"); got != "5" {
		t.Fatalf("auto-credit after a claim: %s", got)
	}

	// A claim for a transfer the watcher recorded on a retired address of the claimant is
	// confirmed against that record, at the address it really went to.
	must(t, db.Create(&model.DepositAddressHolder{Network: "TRC20", Address: "TRetired111111111111111111111111", UserID: c}).Error)
	must(t, w.RecordUnclaimed(model.DepositOrder{Network: "TRC20", Address: "TRetired111111111111111111111111", Amount: "4", TxID: hash64("7")}))
	late, err := w.SubmitClaim(c, hash64("7"), "")
	must(t, err)
	{
		_, err = w.ConfirmDeposit(late.ID, "4", "admin")
		must(t, err)
		var got model.DepositOrder
		must(t, db.First(&got, late.ID).Error)
		if got.Address != "TRetired111111111111111111111111" {
			t.Fatal("claim not moved to the address the transfer went to:", got.Address)
		}
		var rec model.DepositOrder
		must(t, db.Where("tx_id = ? AND user_id = 0", hash64("7")).First(&rec).Error)
		if _, err := w.AssignDeposit(rec.ID, c, "admin"); err == nil {
			t.Fatal("transfer credited twice through assignment")
		}
		if got := balanceOf(t, w, c, "USDT"); got != "4" {
			t.Fatalf("retired-address claim credited %s", got)
		}
	}

	// The watcher re-checks holders under the lock: an address shared meanwhile is not
	// auto-credited to the old holder.
	e := user(7205, "TShared2222222222222222222222222")
	must(t, db.Create(&model.DepositAddressHolder{Network: "TRC20", Address: "TShared2222222222222222222222222", UserID: e}).Error)
	must(t, db.Create(&model.DepositAddressHolder{Network: "TRC20", Address: "TShared2222222222222222222222222", UserID: c}).Error)
	must(t, w.CreditToHolder(model.DepositOrder{UserID: e, Network: "TRC20", Address: "TShared2222222222222222222222222", Amount: "6", TxID: hash64("8")}))
	if got := balanceOf(t, w, e, "USDT"); got != "0" {
		t.Fatalf("shared address auto-credited %s", got)
	}
	var shared model.DepositOrder
	must(t, db.Where("tx_id = ?", hash64("8")).First(&shared).Error)
	if shared.Status != wallet.DepositUnclaimed || shared.UserID != 0 {
		t.Fatalf("shared deposit %+v", shared)
	}

	// A transfer the watcher credited to a single-holder address cannot be claimed by anyone.
	must(t, w.CreditDeposit(model.DepositOrder{UserID: d, Network: "TRC20", Address: ownAddr, Amount: "3", TxID: hash64("e")}))
	if _, err := w.SubmitClaim(b, hash64("e"), ""); err == nil || !strings.Contains(err.Error(), "不是转入你的充值地址") {
		t.Fatal("another user claimed an auto-credited transfer:", err)
	}
	if _, err := w.SubmitClaim(d, hash64("e"), ""); err == nil || !strings.Contains(err.Error(), "已经入账") {
		t.Fatal("an auto-credited transfer was claimed again:", err)
	}

	// The console assigns an unclaimed deposit; a later claim of it is refused.
	must(t, w.RecordUnclaimed(model.DepositOrder{Network: "TRC20", Address: sharedAddr, Amount: "7", TxID: hash64("c")}))
	var unclaimed model.DepositOrder
	must(t, db.Where("tx_id = ?", hash64("c")).First(&unclaimed).Error)
	if _, err := w.AssignDeposit(unclaimed.ID, c, "admin"); err == nil {
		t.Fatal("assigned to a user who never held the address")
	}
	_, err = w.AssignDeposit(unclaimed.ID, b, "admin")
	must(t, err)
	if _, err := w.SubmitClaim(a, hash64("c"), ""); err == nil {
		t.Fatal("claimed a deposit already assigned")
	}

	// One transaction carrying two transfers to a dedicated address credits both.
	for i, amount := range []string{"1", "2"} {
		key := wallet.TRC20EventKey(hash64("m"), ownAddr, decimal.RequireFromString(amount), i)
		must(t, w.CreditDeposit(model.DepositOrder{UserID: d, Network: "TRC20", Address: ownAddr, Amount: amount, TxID: hash64("m"), EventKey: &key}))
		must(t, w.CreditDeposit(model.DepositOrder{UserID: d, Network: "TRC20", Address: ownAddr, Amount: amount, TxID: hash64("m"), EventKey: &key})) // seen again
	}
	if got := balanceOf(t, w, d, "USDT"); got != "11" {
		t.Fatalf("multi-transfer transaction credited %s", got)
	}
}

func TestERC20ClaimsCannotBeSquatted(t *testing.T) {
	_, w, _, user := depositFixture(t)
	a := user(7301, "0x"+strings.Repeat("1", 40))
	b := user(7302, "0x"+strings.Repeat("2", 40))
	nobody := user(7303)
	erc := "0x" + hash64("d")
	if _, err := w.SubmitClaim(nobody, erc, "5"); err == nil {
		t.Fatal("ERC20 claimed without an ERC20 address")
	}
	if _, err := w.SubmitClaim(a, erc, "abc"); err == nil {
		t.Fatal("ERC20 claim without an amount")
	}
	// B submits A's public hash first; A can still submit, and only one can be credited.
	squat, err := w.SubmitClaim(b, erc, "5")
	must(t, err)
	own, err := w.SubmitClaim(a, erc, "5")
	must(t, err)
	if _, err := w.ConfirmDeposit(own.ID, "4.5", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ConfirmDeposit(squat.ID, "5", "admin"); err == nil {
		t.Fatal("a transaction was credited twice")
	}
	if _, err := w.RejectDeposit(squat.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if got := balanceOf(t, w, a, "USDT_ERC20") + " " + balanceOf(t, w, b, "USDT_ERC20"); got != "4.5 0" {
		t.Fatalf("ERC20 balances %s", got)
	}
}

func TestUSDTWithdrawalRuleIsSetInTheConsole(t *testing.T) {
	db, w, _, user := depositFixture(t)
	u := user(7401)
	must(t, w.AdminAdjust(u, "USDT", "100", "admin", "test"))
	// Seeded once from the environment (here min 1, fee 0), then the console owns it.
	must(t, w.SeedWithdrawRule())
	must(t, db.Model(&model.WalletCurrency{}).Where("code = 'USDT'").Updates(map[string]any{"min_withdraw": "20", "withdraw_fee": "2.5"}).Error)
	must(t, w.SeedWithdrawRule())
	if _, err := w.CreateWithdrawal(u, "TRC20", "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7", "10", "w-low"); err == nil || !strings.Contains(err.Error(), "最低") {
		t.Fatal("console minimum ignored:", err)
	}
	o, err := w.CreateWithdrawal(u, "TRC20", "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7", "30", "w-ok")
	must(t, err)
	if o.Fee != "2.500000" || o.Actual != "27.500000" {
		t.Fatalf("fee %s actual %s", o.Fee, o.Actual)
	}
	// A percentage is charged on top of the fixed fee, rounded up to the currency's places.
	must(t, db.Model(&model.WalletCurrency{}).Where("code = 'USDT'").Update("withdraw_fee_rate", "0.01").Error)
	o, err = w.CreateWithdrawal(u, "TRC20", "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7", "30.5", "w-pct")
	must(t, err)
	if o.Fee != "2.805000" || o.Actual != "27.695000" {
		t.Fatalf("percentage fee %s actual %s", o.Fee, o.Actual)
	}
	must(t, db.Model(&model.WalletCurrency{}).Where("code = 'CNY'").Updates(map[string]any{"withdrawable": true, "min_withdraw": "0", "withdraw_fee": "0", "withdraw_fee_rate": "0.003"}).Error)
	must(t, w.AdminAdjust(u, "CNY", "1000", "admin", "test"))
	cny, err := w.CreateCurrencyWithdrawal(u, "CNY", "工商银行 张三 6222", "333.33", "w-cny")
	must(t, err)
	if cny.Fee != "1.000000" || cny.Actual != "332.330000" { // 0.99999 rounds up to 1.00
		t.Fatalf("CNY fee %s actual %s", cny.Fee, cny.Actual)
	}
	rules, err := w.WithdrawRules()
	must(t, err)
	if rules[0].Currency != "USDT" || rules[0].WithdrawFee != "2.500000" || rules[0].WithdrawFeeRate != "0.010000" {
		t.Fatalf("rules %+v", rules[0])
	}
}
