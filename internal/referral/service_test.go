package referral

import (
	"database/sql/driver"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func TestBindCannotOverwriteConcurrentBinding(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "telegram_users", Columns: []string{"id", "parent_user_id"}, Rows: [][]driver.Value{{int64(1), nil}}},
		testutil.Step{Contains: "referral_code=?", Columns: []string{"id"}, Rows: [][]driver.Value{{int64(2)}}},
		testutil.Step{Contains: "parent_user_id IS NULL", Affected: 0},
	)
	if err := New(&store.Store{DB: db}, "0.005").BindByCode(1, "code"); err == nil {
		t.Fatal("concurrent binding overwritten")
	}
}
func TestHistoricalCommissionNotAwardedAgain(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "user_id", "status", "amount", "currency"}, Rows: [][]driver.Value{{int64(9), int64(1), "credited", "100", "USDT"}}},
		testutil.Step{Contains: "telegram_users", Columns: []string{"id", "parent_user_id"}, Rows: [][]driver.Value{{int64(1), int64(2)}}},
		testutil.Step{Contains: "source_type='deposit' AND source_id=?", Args: []any{9}, Columns: []string{"count"}, Rows: [][]driver.Value{{int64(1)}}},
	)
	if err := New(&store.Store{DB: db}, "0.005").AwardFromDeposit(9, 1, "100", "USDT"); err != nil {
		t.Fatal(err)
	}
}
func TestUniqueGrantClaimPreventsSecondCredit(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "user_id", "status", "amount", "currency"}, Rows: [][]driver.Value{{int64(9), int64(1), "credited", "100", "USDT"}}},
		testutil.Step{Contains: "telegram_users", Columns: []string{"id", "parent_user_id"}, Rows: [][]driver.Value{{int64(1), int64(2)}}},
		testutil.Step{Contains: "source_id=?", Columns: []string{"count"}, Rows: [][]driver.Value{{int64(0)}}},
		testutil.Step{Contains: "ON DUPLICATE KEY UPDATE", Affected: 0},
	)
	if err := New(&store.Store{DB: db}, "0.005").AwardFromDeposit(9, 1, "100", "USDT"); err != nil {
		t.Fatal(err)
	}
}
