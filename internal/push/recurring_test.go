package push

import (
	"database/sql/driver"
	"errors"
	"gorm.io/gorm"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
	"time"
)

func TestRepeatValidation(t *testing.T) {
	future := time.Now().Add(time.Hour)
	past := time.Now().Add(-time.Hour)
	for _, tt := range []struct {
		seconds int64
		at      *time.Time
		valid   bool
	}{{0, nil, true}, {-1, &future, false}, {59, &future, false}, {60, &future, true}, {366 * 86400, &future, true}, {366*86400 + 1, &future, false}, {3600, nil, false}, {3600, &past, false}} {
		if err := validateRepeat(tt.seconds, tt.at); (err == nil) != tt.valid {
			t.Fatalf("%+v: %v", tt, err)
		}
	}
}
func TestNextOccurrenceSkipsMissedPeriodsWithoutDrift(t *testing.T) {
	start := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		offset time.Duration
		want   time.Duration
	}{{-time.Minute, 0}, {0, time.Hour}, {59 * time.Minute, time.Hour}, {time.Hour, 2 * time.Hour}, {72*time.Hour + 31*time.Minute, 73 * time.Hour}} {
		got := nextOccurrence(start, start.Add(tt.offset), 3600)
		if !got.Equal(start.Add(tt.want)) {
			t.Fatal(got, tt)
		}
	}
}
func TestRecurringResumeSkipsPausedRoundsButContinuesInFlightDelivery(t *testing.T) {
	now := time.Now()
	past := now.Add(-3*time.Hour - 15*time.Minute)
	c := model.PushCampaign{RepeatSeconds: 3600, ScheduledAt: &past, PausedFrom: "scheduled"}
	v := resumeUpdates(c, now)
	if v["status"] != "scheduled" || !v["scheduled_at"].(time.Time).Equal(past.Add(4*time.Hour)) {
		t.Fatal(v)
	}
	c.PausedFrom = "processing"
	v = resumeUpdates(c, now)
	if v["status"] != "processing" || v["scheduled_at"] != nil {
		t.Fatal(v)
	}
}
func TestRecurringFinishArchivesRecipientsAndResetsOnlyLiveTask(t *testing.T) {
	now := time.Now()
	scheduled := now.Add(-2*time.Hour - 10*time.Minute)
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status", "repeat_seconds", "scheduled_at", "sent_count", "failed_count", "bot_id"}, Rows: [][]driver.Value{{int64(42), "processing", int64(3600), scheduled, int64(2), int64(1), int64(7)}}}, testutil.Step{Contains: "INSERT INTO `push_campaigns`", Affected: 1}, testutil.Step{Contains: "UPDATE `push_recipients` SET", Affected: 3}, testutil.Step{Contains: "UPDATE `push_campaigns` SET", Affected: 1})
	db.Callback().Create().Before("gorm:create").Register("test:archive", func(tx *gorm.DB) {
		c := tx.Statement.Dest.(*model.PushCampaign)
		if c.ParentID != 42 || c.RepeatSeconds != 0 || c.Status != "completed" || c.SentCount != 2 || c.FailedCount != 1 || c.BotID != 7 || c.CompletedAt == nil {
			t.Fatalf("bad archive: %+v", c)
		}
	})
	db.Callback().Update().Before("gorm:update").Register("test:rearm", func(tx *gorm.DB) {
		v := tx.Statement.Dest.(map[string]any)
		if tx.Statement.Table == "push_recipients" {
			if v["campaign_id"] != uint64(1) {
				t.Fatal(v)
			}
			return
		}
		if v["status"] != "scheduled" || v["sent_count"] != 0 || v["failed_count"] != 0 || v["started_at"] != nil || !v["scheduled_at"].(time.Time).After(now) {
			t.Fatal(v)
		}
	})
	if err := New(&store.Store{DB: db}, nil, nil).finishRecurring(42, now); err != nil {
		t.Fatal(err)
	}
}
func TestRecurringFinishCannotRearmPausedCanceledOrAlreadyScheduledTask(t *testing.T) {
	for _, status := range []string{"paused", "canceled", "scheduled", "completed"} {
		db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status", "repeat_seconds"}, Rows: [][]driver.Value{{int64(42), status, int64(3600)}}})
		if err := New(&store.Store{DB: db}, nil, nil).finishRecurring(42, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRecurringArchiveFailureDoesNotResetDeliveryState(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status", "repeat_seconds"}, Rows: [][]driver.Value{{int64(42), "processing", int64(3600)}}}, testutil.Step{Contains: "INSERT INTO `push_campaigns`", Err: errors.New("write failed")})
	if err := New(&store.Store{DB: db}, nil, nil).finishRecurring(42, time.Now()); err == nil {
		t.Fatal("ignored archive failure")
	}
}

func TestCreatePersistsRepeatSettings(t *testing.T) {
	start := time.Now().Add(time.Hour)
	db := testutil.DB(t, testutil.Step{Contains: "INSERT INTO `push_campaigns`", Affected: 1})
	db.Callback().Create().Before("gorm:create").Register("test:recurrence", func(tx *gorm.DB) {
		c := tx.Statement.Dest.(*model.PushCampaign)
		if c.RepeatSeconds != 86400 || c.Status != "scheduled" || c.ParentID != 0 || !c.ScheduledAt.Equal(start) {
			t.Fatalf("lost recurrence: %+v", c)
		}
	})
	if _, err := New(&store.Store{DB: db}, nil, nil).Create(CreateRequest{Title: "daily", Content: "hello", TargetType: "all", ScheduledAt: &start, RepeatSeconds: 86400}, "admin"); err != nil {
		t.Fatal(err)
	}
}
func TestRetryCanceledRecurringTaskDoesNotRestartRecurrence(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status", "repeat_seconds"}, Rows: [][]driver.Value{{int64(42), "canceled", int64(3600)}}}, testutil.Step{Contains: "UPDATE `push_recipients`", Affected: 1}, testutil.Step{Contains: "UPDATE `push_recipients`", Affected: 1}, testutil.Step{Contains: "UPDATE `push_campaigns`", Affected: 1})
	db.Callback().Update().Before("gorm:update").Register("test:retry-recurrence", func(tx *gorm.DB) {
		if tx.Statement.Table == "push_campaigns" {
			v := tx.Statement.Dest.(map[string]any)
			if v["repeat_seconds"] != 0 || v["status"] != "processing" {
				t.Fatal(v)
			}
		}
	})
	if err := New(&store.Store{DB: db}, nil, nil).retryFailed(42); err != nil {
		t.Fatal(err)
	}
}
