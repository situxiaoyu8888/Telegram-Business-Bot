package store

import (
	"database/sql/driver"
	"gorm.io/gorm"
	"strings"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func TestUpsertOnlyChangesProfileFields(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "ON DUPLICATE KEY UPDATE", Affected: 1},
		testutil.Step{Contains: "telegram_user_id=?", Columns: []string{"id", "telegram_user_id", "parent_user_id", "status"}, Rows: [][]driver.Value{{int64(1), int64(123), int64(9), int64(0)}}},
	)
	db.Callback().Create().After("gorm:create").Register("test:profile_columns", func(tx *gorm.DB) {
		q := tx.Statement.SQL.String()
		parts := strings.Split(q, "ON DUPLICATE KEY UPDATE")
		if len(parts) != 2 || strings.Contains(parts[1], "parent_user_id") || strings.Contains(parts[1], "status") {
			t.Errorf("profile upsert overwrites business fields: %s", q)
		}
	})
	u, err := (&Store{DB: db}).UpsertTelegramUser(123, "name", "first", "last", "en")
	if err != nil || u.ParentUserID == nil || *u.ParentUserID != 9 || u.Status != 0 {
		t.Fatalf("%+v %v", u, err)
	}
}
func TestSeedDoesNotRecreateDeletedMenus(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "seed_migrations", Columns: []string{"name"}, Rows: [][]driver.Value{{"defaults_v1"}}})
	if err := (&Store{DB: db}).seedDefaults(); err != nil {
		t.Fatal(err)
	}
}
func TestDisabledMenuRemainsDisabledOnCreate(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "INSERT INTO `menu_items`", Affected: 1})
	row := model.MenuItem{Code: "disabled", Title: "Disabled", Enabled: false}
	if err := db.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Enabled {
		t.Fatal("default forced disabled menu on")
	}
}
