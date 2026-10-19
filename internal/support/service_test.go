package support

import (
	"context"
	"database/sql/driver"
	"errors"
	"gorm.io/gorm"
	"strings"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func TestFailedReplyIsSavedWithoutClosingTicket(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "support_tickets", Columns: []string{"id", "telegram_user_id", "status"}, Rows: [][]driver.Value{{int64(1), int64(123), "open"}}},
		testutil.Step{Contains: "admin_reply", Affected: 1},
		testutil.Step{Contains: "delivery_status", Affected: 1},
	)
	db.Callback().Update().Before("gorm:update").Register("test:never_answered", func(tx *gorm.DB) {
		if values, ok := tx.Statement.Dest.(map[string]any); ok && values["status"] == "answered" {
			t.Error("failed reply marked answered")
		}
	})
	err := New(&store.Store{DB: db}).deliverReply(context.Background(), 1, "admin", "answer", func(context.Context, int64, string) error { return errors.New("offline") })
	if err == nil || !strings.Contains(err.Error(), "retry required") {
		t.Fatalf("%v", err)
	}
}
func TestReplyRetryClosesOnlyAfterSuccessfulDelivery(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "support_tickets", Columns: []string{"id", "telegram_user_id", "status", "delivery_status"}, Rows: [][]driver.Value{{int64(1), int64(123), "open", "failed"}}},
		testutil.Step{Contains: "admin_reply", Affected: 1},
		testutil.Step{Contains: "replied_at", Affected: 1},
	)
	delivered := false
	db.Callback().Update().Before("gorm:update").Register("test:sent_before_close", func(tx *gorm.DB) {
		if v, ok := tx.Statement.Dest.(map[string]any); ok && v["status"] == "answered" && !delivered {
			t.Error("closed before send")
		}
	})
	err := New(&store.Store{DB: db}).deliverReply(context.Background(), 1, "admin", "answer", func(context.Context, int64, string) error { delivered = true; return nil })
	if err != nil || !delivered {
		t.Fatal(err)
	}
}
