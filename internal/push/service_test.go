package push

import (
	"context"
	"database/sql/driver"
	"errors"
	"gorm.io/gorm"
	"reflect"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func TestTargetsPreserveIntegerIDs(t *testing.T) {
	for _, input := range []string{`[123456789,987654321]`, `["123456789","987654321"]`, `123456789,987654321,123456789`} {
		ids, err := parseTargets(input)
		if err != nil || !reflect.DeepEqual(ids, []int64{123456789, 987654321}) {
			t.Fatalf("%s: %v %v", input, ids, err)
		}
	}
	for _, input := range []string{`[]`, `[1.5]`, `[null]`, `abc`, `[1e8]`, `0`, `[123,`} {
		if _, err := parseTargets(input); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

type failingTextSender struct{ media, text int }

func (f *failingTextSender) SendPhoto(context.Context, int64, string, string, string, *telegram.InlineKeyboardMarkup) error {
	f.media++
	return nil
}
func (f *failingTextSender) SendVideo(context.Context, int64, string, string, string, *telegram.InlineKeyboardMarkup) error {
	f.media++
	return nil
}
func (f *failingTextSender) SendMessageWithParseMode(context.Context, int64, string, string, *telegram.InlineKeyboardMarkup) error {
	f.text++
	if f.text == 1 {
		return errors.New("temporary failure")
	}
	return nil
}
func TestMediaNotResentWhenTextRetries(t *testing.T) {
	for _, kind := range []string{"photo", "video"} {
		t.Run(kind, func(t *testing.T) {
			db := testutil.DB(t, testutil.Step{Contains: "media_sent", Affected: 1})
			sender := &failingTextSender{}
			s := &Service{st: &store.Store{DB: db}, tg: sender}
			c := &model.PushCampaign{MessageType: kind, MediaFileID: "file", Content: "hello", ContentMode: ContentModeSeparate}
			r := &model.PushRecipient{ID: 1, TelegramUserID: 1}
			if err := s.sendRecipient(context.Background(), r, c, nil); err == nil {
				t.Fatal("expected text failure")
			}
			// Simulate reloading the persisted progress in the next attempt.
			r = &model.PushRecipient{ID: 1, TelegramUserID: 1, MediaSent: true}
			if err := s.sendRecipient(context.Background(), r, c, nil); err != nil {
				t.Fatal(err)
			}
			if sender.media != 1 || sender.text != 2 {
				t.Fatalf("media=%d text=%d", sender.media, sender.text)
			}
		})
	}
}
func TestCanceledCampaignCannotBePreparedFromStaleSnapshot(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "canceled"}}})
	s := &Service{st: &store.Store{DB: db}}
	c := &model.PushCampaign{ID: 1, Status: "queued", TargetType: "telegram_ids", TargetValue: "123456789"}
	if err := s.prepareRecipients(c); err != nil {
		t.Fatal(err)
	}
	if c.Status != "canceled" {
		t.Fatal("cancel overwritten")
	}
}
func TestRetryCanceledLeavesUnsentCanceled(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "canceled"}}},
		testutil.Step{Contains: "status='pending'", Affected: 2},
		testutil.Step{Contains: "status='failed'", Affected: 1},
		testutil.Step{Contains: "UPDATE `push_campaigns`", Affected: 1},
	)
	if err := (&Service{st: &store.Store{DB: db}}).retryFailed(1); err != nil {
		t.Fatal(err)
	}
}
func TestRetryActiveCampaignRejected(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "processing"}}})
	if err := (&Service{st: &store.Store{DB: db}}).retryFailed(1); err == nil {
		t.Fatal("active campaign retry allowed")
	}
}

func TestCanceledDeliveryFailureCannotRequeue(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "canceled"}}},
		testutil.Step{Contains: "UPDATE `push_recipients`", Affected: 1},
	)
	db.Callback().Update().Before("gorm:update").Register("test:canceled", func(tx *gorm.DB) {
		v := tx.Statement.Dest.(map[string]any)
		if v["status"] != "canceled" {
			t.Errorf("resurrected: %v", v)
		}
	})
	if err := (&Service{st: &store.Store{DB: db}}).recordDelivery(1, &model.PushRecipient{ID: 2}, errors.New("failed")); err != nil {
		t.Fatal(err)
	}
}
