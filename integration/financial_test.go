package integration

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/referral"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

// This suite never reads .env. TEST_MYSQL_DSN must target a test server, with no database name.
// A uniquely named database is created and dropped; no existing database is migrated.
func testDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set TEST_MYSQL_DSN to run isolated MySQL/MariaDB integration tests")
	}
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test DSN")
	}
	if cfg.DBName != "" {
		t.Fatal("TEST_MYSQL_DSN must omit database name")
	}
	cfg.ParseTime = true
	server, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal("open test server failed")
	}
	name := fmt.Sprintf("tg_bot_test_%d", time.Now().UnixNano())
	if _, err = server.Exec("CREATE DATABASE `" + name + "`"); err != nil {
		server.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, e := server.Exec("DROP DATABASE `" + name + "`")
		server.Close()
		if e != nil {
			t.Error(e)
		}
	})
	cfg.DBName = name
	db, err := gorm.Open(mysql.Open(cfg.FormatDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := db.DB()
	t.Cleanup(func() { raw.Close() })
	if err := db.AutoMigrate(&model.TelegramUser{}, &model.WalletAccount{}, &model.WalletTransaction{}, &model.DepositOrder{}, &model.WithdrawalOrder{}, &model.ReferralCommission{}, &model.ReferralGrant{}); err != nil {
		t.Fatal(err)
	}
	return db
}
func TestConcurrentFinancialIdempotency(t *testing.T) {
	db := testDatabase(t)
	st := &store.Store{DB: db}
	w := wallet.New(st, "USDT", "1", "0")
	parent := model.TelegramUser{TelegramUserID: 1001, ReferralCode: "parent", Status: 1}
	if err := db.Create(&parent).Error; err != nil {
		t.Fatal(err)
	}
	child := model.TelegramUser{TelegramUserID: 1002, ReferralCode: "child", ParentUserID: &parent.ID, Status: 1}
	if err := db.Create(&child).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.Balance(parent.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.CreditDeposit(model.DepositOrder{UserID: child.ID, Network: "TRC20", Address: "test-address", Amount: "100", TxID: "integration-deposit"}); err != nil {
		t.Fatal(err)
	}
	run := func(work func() error) {
		t.Helper()
		var wg sync.WaitGroup
		errs := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- work() }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Error(err)
			}
		}
	}
	run(func() error {
		_, err := w.CreateWithdrawal(child.ID, "TRC20", "TLa2f6VPqDgRE67v1736s7bJ8Ray5wYjU7", "10", "same-request")
		return err
	})
	var orders int64
	db.Model(&model.WithdrawalOrder{}).Count(&orders)
	av, fr, err := w.Balance(child.ID)
	if err != nil || orders != 1 || av.String() != "90" || fr.String() != "10" {
		t.Fatalf("orders=%d available=%s frozen=%s err=%v", orders, av, fr, err)
	}
	var dep model.DepositOrder
	db.First(&dep)
	ref := referral.New(st, "0.005")
	run(func() error { return ref.AwardFromDeposit(dep.ID, child.ID, dep.Amount, "USDT") })
	var count int64
	db.Model(&model.ReferralCommission{}).Count(&count)
	av, _, err = w.Balance(parent.ID)
	if err != nil || count != 1 || av.String() != "0.5" {
		t.Fatalf("commissions=%d balance=%s err=%v", count, av, err)
	}
	// Existing rows can all have NULL request IDs; the migration does not rewrite historical orders.
	if err := db.Create(&[]model.WithdrawalOrder{{UserID: child.ID, Amount: "1", Fee: "0", Actual: "1", Status: "legacy"}, {UserID: child.ID, Amount: "1", Fee: "0", Actual: "1", Status: "legacy"}}).Error; err != nil {
		t.Fatal(err)
	}
	// An update to profile fields must not reset the relationship.
	run(func() error {
		_, err := st.UpsertTelegramUser(child.TelegramUserID, "name", "first", "last", "en")
		return err
	})
	var saved model.TelegramUser
	if err := db.First(&saved, child.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ParentUserID == nil || *saved.ParentUserID != parent.ID {
		t.Fatal("parent overwritten")
	}
	if strings.TrimSpace(saved.Username) != "name" {
		t.Fatal("profile not updated")
	}
}
