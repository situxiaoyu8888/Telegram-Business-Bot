package integration

import (
	"testing"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// Admin accounts, roles and reviews detect duplicates from the insert error, because on MariaDB
// an ON DUPLICATE KEY insert into an auto-increment table still reports one affected row.
func TestDuplicateKeysAreDetectedOnRealDatabase(t *testing.T) {
	db := testDatabase(t)
	must(t, db.AutoMigrate(&model.AdminUser{}, &model.AdminRole{}))
	must(t, db.Create(&model.AdminUser{Username: "ops", PasswordHash: "x", Role: "admin", Enabled: true}).Error)
	if err := db.Create(&model.AdminUser{Username: "ops", PasswordHash: "y", Role: "admin", Enabled: true}).Error; !store.IsDuplicate(err) {
		t.Fatal("duplicate username not detected:", err)
	}
	must(t, db.Create(&model.AdminRole{Name: "财务"}).Error)
	if err := db.Create(&model.AdminRole{Name: "财务"}).Error; !store.IsDuplicate(err) {
		t.Fatal("duplicate role not detected:", err)
	}
	if store.IsDuplicate(db.Create(&model.AdminRole{Name: "运营"}).Error) {
		t.Fatal("false positive")
	}
}
