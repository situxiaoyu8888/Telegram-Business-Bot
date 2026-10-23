package integration

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/wallet"
)

func TestTransferMovesMoneyOnceAndChecksRecipient(t *testing.T) {
	db, w, a := exchangeFixture(t, "0")
	b := model.TelegramUser{TelegramUserID: 7002, Username: "bob_tf", FirstName: "Bob", ReferralCode: "tf7002", Status: 1}
	must(t, db.Create(&b).Error)

	req := wallet.TransferRequest{To: "@bob_tf", Currency: "USDT", Amount: "12.5", Remark: "午饭", RequestID: "tf-1"}
	tf, to, err := w.Transfer(a, req)
	must(t, err)
	if to.ID != b.ID || tf.Amount != "12.500000" {
		t.Fatalf("transfer %+v to %d", tf, to.ID)
	}
	// The same request again is not charged twice; a changed one with the same ID is refused.
	again, _, err := w.Transfer(a, req)
	must(t, err)
	if again.ID != tf.ID {
		t.Fatal("repeated request made a second transfer")
	}
	req.Amount = "13"
	if _, _, err := w.Transfer(a, req); err == nil {
		t.Fatal("request ID reused for a different amount")
	}
	if got := balanceOf(t, w, a, "USDT") + " " + balanceOf(t, w, b.ID, "USDT"); got != "87.5 12.5" {
		t.Fatalf("balances %s", got)
	}
	var types []string
	must(t, db.Model(&model.WalletTransaction{}).Where("ref_type = 'transfer' AND ref_id = ?", tf.ID).Order("id").Pluck("type", &types).Error)
	if strings.Join(types, ",") != "transfer_out,transfer_in" {
		t.Fatalf("ledger %v", types)
	}

	for name, r := range map[string]wallet.TransferRequest{
		"self":         {To: "7001", Currency: "USDT", Amount: "1", RequestID: "tf-self"},
		"unknown":      {To: "@nobody_here", Currency: "USDT", Amount: "1", RequestID: "tf-unknown"},
		"insufficient": {To: "7002", Currency: "USDT", Amount: "100", RequestID: "tf-big"},
		"decimals":     {To: "7002", Currency: "CNY", Amount: "0.001", RequestID: "tf-dec"},
	} {
		if _, _, err := w.Transfer(a, r); err == nil {
			t.Fatalf("%s transfer accepted", name)
		}
	}

	// Transfers in both directions at once neither deadlock nor lose money.
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_, _, err := w.Transfer(a, wallet.TransferRequest{To: "7002", Currency: "USDT", Amount: "1", RequestID: fmt.Sprintf("ab-%d", i)})
			errs <- err
		}(i)
		go func(i int) {
			defer wg.Done()
			_, _, err := w.Transfer(b.ID, wallet.TransferRequest{To: "7001", Currency: "USDT", Amount: "1", RequestID: fmt.Sprintf("ba-%d", i)})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		must(t, err)
	}
	if got := balanceOf(t, w, a, "USDT") + " " + balanceOf(t, w, b.ID, "USDT"); got != "87.5 12.5" {
		t.Fatalf("balances after concurrent transfers %s", got)
	}
}
