package integration

import (
	"strings"
	"testing"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/paypass"
	"telegram-business-bot/internal/store"
)

func TestPaymentPasswordRulesAttemptsAndLock(t *testing.T) {
	db := testDatabase(t)
	svc := paypass.New(&store.Store{DB: db})
	for _, weak := range []string{"12345", "1234567", "abcdef", "111111", "123456", "987654"} {
		if paypass.Check(weak) == nil {
			t.Fatalf("accepted %q", weak)
		}
	}
	if err := svc.Verify(1, "246810"); err != paypass.ErrNotSet {
		t.Fatal("verify without a password:", err)
	}
	must(t, svc.Set(1, "246810"))
	if svc.Set(1, "135790") == nil {
		t.Fatal("password set twice")
	}
	must(t, svc.Verify(1, "246810"))
	// Wrong attempts count down, a right one resets the count, five in a row lock it.
	if err := svc.Verify(1, "000001"); err == nil || !strings.Contains(err.Error(), "还可以再试 4 次") {
		t.Fatal(err)
	}
	must(t, svc.Verify(1, "246810"))
	for i := 0; i < paypass.MaxAttempts; i++ {
		_ = svc.Verify(1, "000001")
	}
	if err := svc.Verify(1, "246810"); err == nil || !strings.Contains(err.Error(), "锁定") {
		t.Fatal("not locked:", err)
	}
	var p model.PaymentPassword
	must(t, db.First(&p, 1).Error)
	if p.LockedUntil == nil {
		t.Fatal("lock not stored")
	}
	// The console reset clears it; the next payment sets a new one.
	must(t, svc.Reset(1))
	if set, _ := svc.IsSet(1); set {
		t.Fatal("reset did not clear the password")
	}
	must(t, svc.Set(1, "135790"))
	if svc.Change(1, "246810", "975310") == nil {
		t.Fatal("changed with the wrong old password")
	}
	must(t, svc.Change(1, "135790", "975310"))
	must(t, svc.Verify(1, "975310"))
}
