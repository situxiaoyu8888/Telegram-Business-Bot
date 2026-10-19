package business

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"gorm.io/gorm"
	"strings"
	"testing"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/testutil"
)

func TestBotTokensEncryptedAndNeverReturned(t *testing.T) {
	s := New(nil, nil, "stable-secret", "1:primary", "")
	token := "123:private-token"
	encrypted, err := s.encrypt(token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encrypted, token) {
		t.Fatal("plaintext token stored")
	}
	plain, err := s.decrypt(encrypted)
	if err != nil || plain != token {
		t.Fatal("round trip failed")
	}
	b, _ := json.Marshal(model.BusinessBot{TokenEncrypted: encrypted, Name: "test"})
	if strings.Contains(string(b), encrypted) || strings.Contains(string(b), token) {
		t.Fatal("API leaked token")
	}
	wrong := New(nil, nil, "different-secret", "1:primary", "")
	if _, err := wrong.decrypt(encrypted); err == nil {
		t.Fatal("wrong encryption key accepted")
	}
}
func TestCannotReplaceBotIdentity(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "business_bots", Columns: []string{"id", "telegram_bot_id", "token_encrypted"}, Rows: [][]driver.Value{{int64(1), int64(42), "cipher"}}})
	s := New(&store.Store{DB: db}, nil, "key", "1:primary", "")
	s.verify = func(context.Context, string) (*telegram.User, error) {
		return &telegram.User{ID: 43, Username: "different"}, nil
	}
	if _, err := s.Save(context.Background(), 1, "test", "token", true); err == nil {
		t.Fatal("bot identity replaced, would break recipients")
	}
}
func TestDefaultAndAdminBotsCannotBeRegisteredTwice(t *testing.T) {
	for _, id := range []int64{1, 2} {
		s := New(nil, nil, "key", "1:primary", "2:admin")
		s.verify = func(context.Context, string) (*telegram.User, error) { return &telegram.User{ID: id}, nil }
		if _, err := s.Save(context.Background(), 0, "test", "token", true); err == nil {
			t.Fatal("reserved bot accepted")
		}
	}
}
func TestRouteResolvesCategoryAndHonorsExplicitDefault(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "category=?", Args: []any{"招聘通知", 1}, Columns: []string{"bot_id"}, Rows: [][]driver.Value{{int64(7)}}},
		testutil.Step{Contains: "business_bots", Columns: []string{"id", "enabled", "token_encrypted"}},
	)
	s := New(&store.Store{DB: db}, nil, "key", "1:primary", "")
	// A configured channel that disappeared must produce an error, never fall back to another bot.
	if _, err := s.Resolve(context.Background(), "招聘通知", nil); err == nil {
		t.Fatal("missing routed bot silently fell back")
	}
	zero := uint64(0)
	id, err := s.Resolve(context.Background(), "招聘通知", &zero)
	if err != nil || id != 0 {
		t.Fatalf("explicit default ignored: %d %v", id, err)
	}
}
func TestDisabledBotDoesNotFallBack(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "business_bots", Columns: []string{"id", "enabled"}, Rows: [][]driver.Value{{int64(3), false}}})
	s := New(&store.Store{DB: db}, nil, "key", "1:primary", "")
	if _, err := s.Sender(context.Background(), 3); err == nil {
		t.Fatal("disabled bot accepted")
	}
}

func TestEditBotPreservesPollerOffsetAndCredentials(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "business_bots", Columns: []string{"id", "name", "telegram_bot_id", "token_encrypted", "offset"}, Rows: [][]driver.Value{{int64(3), "old", int64(42), "encrypted", int64(800)}}},
		testutil.Step{Contains: "UPDATE `business_bots`", Affected: 1},
	)
	db.Callback().Update().Before("gorm:update").Register("test:bot_fields", func(tx *gorm.DB) {
		v := tx.Statement.Dest.(map[string]any)
		if _, ok := v["offset"]; ok {
			t.Error("offset overwritten")
		}
		if v["token_encrypted"] != "encrypted" {
			t.Error("empty token erased secret")
		}
	})
	s := New(&store.Store{DB: db}, nil, "key", "1:primary", "")
	if _, err := s.Save(context.Background(), 3, "renamed", "", false); err != nil {
		t.Fatal(err)
	}
}
