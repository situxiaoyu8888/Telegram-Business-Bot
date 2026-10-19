package wallet

import (
	"database/sql/driver"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
)

const validAddress = "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7"

func TestTRONAddressChecksum(t *testing.T) {
	if !ValidTRONAddress(validAddress) {
		t.Fatal("valid address rejected")
	}
	for _, v := range []string{"T123456789012345678901234567890", validAddress[:33] + "8", "0x1234", ""} {
		if ValidTRONAddress(v) {
			t.Fatalf("accepted %s", v)
		}
	}
}
func TestWithdrawalRejectsInvalidInputsBeforeDB(t *testing.T) {
	s := New(nil, "USDT", "1", "0")
	for _, v := range []struct{ network, address, amount, key string }{{"ERC20", validAddress, "10", "x"}, {"TRC20", "Txxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx", "10", "x"}, {"TRC20", validAddress, "10.0000009", "x"}, {"TRC20", validAddress, "-1", "x"}, {"TRC20", validAddress, "10", ""}} {
		if _, err := s.CreateWithdrawal(1, v.network, v.address, v.amount, v.key); err == nil {
			t.Fatalf("accepted invalid request %+v", v)
		}
	}
}
func TestWithdrawalReplayDoesNotFreezeAgain(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "user_id", "currency", "available", "frozen"}, Rows: [][]driver.Value{{int64(1), int64(1), "USDT", "90.000000", "10.000000"}}},
		testutil.Step{Contains: "request_id=?", Columns: []string{"id", "user_id", "network", "address", "amount", "status", "request_id"}, Rows: [][]driver.Value{{int64(17), int64(1), "TRC20", validAddress, "10.000000", "pending", "same-request"}}},
	)
	order, err := New(&store.Store{DB: db}, "USDT", "1", "0").CreateWithdrawal(1, "TRC20", validAddress, "10", "same-request")
	if err != nil || order.ID != 17 {
		t.Fatalf("order=%+v err=%v", order, err)
	}
}
func TestApproveMissingOrStaleOrderFails(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "status='pending'", Affected: 0})
	if err := New(&store.Store{DB: db}, "USDT", "1", "0").ApproveWithdrawal(77, "admin", ""); err == nil {
		t.Fatal("zero-row approval succeeded")
	}
}
