package integration

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

func TestSharedAddressDepositsAreClaimedOnce(t *testing.T) {
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
	user := func(tg int64) uint64 {
		u := model.TelegramUser{TelegramUserID: tg, ReferralCode: fmt.Sprintf("dc%d", tg), Status: 1}
		must(t, db.Create(&u).Error)
		return u.ID
	}
	a, b, c := user(7201), user(7202), user(7203)
	const shared = "TA4Y62o6YC2Zsck9rZVGTvqW1AQ7X9zTnj"
	for _, id := range []uint64{a, b} {
		must(t, db.Create(&model.DepositAddress{UserID: id, Network: "TRC20", Address: shared, Status: 1}).Error)
	}
	must(t, db.Create(&model.DepositAddress{UserID: c, Network: "TRC20", Address: "TA9pkx4DFxrEw8JZzUtyDrh2uAat1LDuJL", Status: 1}).Error)
	hash := func(ch string) string { return strings.Repeat(ch, 64) }
	for _, h := range []string{hash("a"), hash("b"), hash("c")} {
		must(t, w.RecordUnclaimed(model.DepositOrder{Network: "TRC20", Address: shared, Amount: "10", TxID: h}))
	}
	must(t, w.RecordUnclaimed(model.DepositOrder{Network: "TRC20", Address: shared, Amount: "10", TxID: hash("a")})) // seen again

	// Only a user of the address may claim, once; both racing for it leave one winner.
	if _, err := w.ClaimTRC20(c, hash("a")); err == nil || !strings.Contains(err.Error(), "不是转入你的充值地址") {
		t.Fatal("claimed by a user of another address:", err)
	}
	if _, err := w.ClaimTRC20(a, "not-a-hash"); err == nil {
		t.Fatal("bad hash accepted")
	}
	var wg sync.WaitGroup
	wins := make(chan uint64, 2)
	for _, id := range []uint64{a, b} {
		wg.Add(1)
		go func(id uint64) {
			defer wg.Done()
			if _, err := w.ClaimTRC20(id, strings.ToUpper(hash("a"))); err == nil {
				wins <- id
			}
		}(id)
	}
	wg.Wait()
	close(wins)
	var winner []uint64
	for id := range wins {
		winner = append(winner, id)
	}
	if len(winner) != 1 {
		t.Fatalf("claim winners %v", winner)
	}
	if _, err := w.ClaimTRC20(winner[0], hash("a")); err == nil || !strings.Contains(err.Error(), "已经入账") {
		t.Fatal("second claim by the winner:", err)
	}
	if got := balanceOf(t, w, a, "USDT") + " " + balanceOf(t, w, b, "USDT"); got != "10 0" && got != "0 10" {
		t.Fatalf("balances after claim %s", got)
	}
	var claimed model.DepositOrder
	must(t, db.Where("tx_id = ?", hash("a")).First(&claimed).Error)
	if claimed.Status != "credited" || announced[claimed.ID] {
		t.Fatalf("claimed order %+v announced %v", claimed, announced[claimed.ID])
	}

	// The console assigns an unclaimed deposit; it is announced.
	o, err := w.AssignDeposit(mustOrder(t, db, hash("b")), c)
	must(t, err)
	if o.UserID != c || !announced[o.ID] || balanceOf(t, w, c, "USDT") != "10" {
		t.Fatalf("assigned %+v", o)
	}
	if _, err := w.AssignDeposit(o.ID, a); err == nil {
		t.Fatal("assigned twice")
	}

	// ERC20: submitted with an amount, confirmed (possibly corrected) or rejected by the console.
	erc := "0x" + hash("d")
	if _, err := w.SubmitERC20(a, erc, "5"); err == nil {
		t.Fatal("ERC20 submitted without an ERC20 address")
	}
	must(t, db.Create(&model.DepositAddress{UserID: a, Network: "ERC20", Address: "0x" + strings.Repeat("ab", 20), Status: 1}).Error)
	p1, err := w.SubmitERC20(a, erc, "5")
	must(t, err)
	if _, err := w.SubmitERC20(b, erc, "5"); err == nil {
		t.Fatal("same ERC20 hash submitted twice")
	}
	p2, err := w.SubmitERC20(a, "0x"+hash("e"), "8")
	must(t, err)
	if _, err := w.ConfirmDeposit(p1.ID, "4.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ConfirmDeposit(p1.ID, "4.5"); err == nil {
		t.Fatal("confirmed twice")
	}
	if _, err := w.RejectDeposit(p2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ConfirmDeposit(p2.ID, "8"); err == nil {
		t.Fatal("confirmed a rejected deposit")
	}
	if got := balanceOf(t, w, a, "USDT_ERC20"); got != "4.5" {
		t.Fatalf("ERC20 balance %s", got)
	}
}

func mustOrder(t *testing.T, db *gorm.DB, txid string) uint64 {
	t.Helper()
	var o model.DepositOrder
	must(t, db.Where("tx_id = ?", txid).First(&o).Error)
	return o.ID
}
