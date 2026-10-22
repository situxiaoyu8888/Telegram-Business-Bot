package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"telegram-business-bot/internal/backup"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
)

// restoreInto creates a fresh database, runs a dump into it and returns a connection to it.
func restoreInto(t *testing.T, dump string) *sql.DB {
	cfg, err := driver.ParseDSN(os.Getenv("TEST_MYSQL_DSN"))
	must(t, err)
	cfg.MultiStatements, cfg.ParseTime = true, true
	server, err := sql.Open("mysql", cfg.FormatDSN())
	must(t, err)
	name := fmt.Sprintf("tg_restore_%d", time.Now().UnixNano())
	_, err = server.Exec("CREATE DATABASE `" + name + "`")
	must(t, err)
	t.Cleanup(func() { server.Exec("DROP DATABASE `" + name + "`"); server.Close() })
	cfg.DBName = name
	db, err := sql.Open("mysql", cfg.FormatDSN())
	must(t, err)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(dump); err != nil {
		t.Fatal("restore failed:", err)
	}
	return db
}

func TestBackupRestoresEveryRowExactly(t *testing.T) {
	f := newShopFixture(t, 5, "physical")
	b, addr := f.buyer(t, 9401, "100")
	o, err := f.svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "bk", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 2}}, DeliveryType: shop.DeliveryExpress, AddressID: addr, Remark: "带'引号'\\反斜杠\n换行 和 中文 🎉"})
	must(t, err)
	must(t, f.db.Create(&model.ShopVirtualCode{SKUID: f.sku.ID, Code: "CODE\x00WITH\x1aCONTROL"}).Error)
	sqlDB, err := f.db.DB()
	must(t, err)

	var gz bytes.Buffer
	must(t, backup.WriteGzip(context.Background(), sqlDB, &gz))
	r, err := gzip.NewReader(&gz)
	must(t, err)
	dump, err := io.ReadAll(r)
	must(t, err)
	if !strings.Contains(string(dump), "CREATE TABLE `shop_orders`") {
		t.Fatal("dump lacks table definitions")
	}
	restored := restoreInto(t, string(dump))

	var tables []string
	rows, err := sqlDB.Query("SHOW TABLES")
	must(t, err)
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		tables = append(tables, n)
	}
	rows.Close()
	for _, table := range tables {
		var a, b int64
		must(t, sqlDB.QueryRow("SELECT COUNT(*) FROM `"+table+"`").Scan(&a))
		must(t, restored.QueryRow("SELECT COUNT(*) FROM `"+table+"`").Scan(&b))
		if a != b {
			t.Fatalf("%s: %d rows, restored %d", table, a, b)
		}
	}
	var remark, pay string
	var shipped sql.NullTime
	must(t, restored.QueryRow("SELECT remark, pay_amount, shipped_at FROM shop_orders WHERE id = ?", o.ID).Scan(&remark, &pay, &shipped))
	if remark != o.Remark || pay != "20.000000" || shipped.Valid {
		t.Fatalf("remark=%q pay=%s shipped=%v", remark, pay, shipped)
	}
	var before, after string
	must(t, sqlDB.QueryRow("SELECT CAST(created_at AS CHAR) FROM shop_orders WHERE id = ?", o.ID).Scan(&before))
	must(t, restored.QueryRow("SELECT CAST(created_at AS CHAR) FROM shop_orders WHERE id = ?", o.ID).Scan(&after))
	if before != after {
		t.Fatalf("created_at %s restored as %s", before, after)
	}
	var code string
	must(t, restored.QueryRow("SELECT code FROM shop_virtual_codes LIMIT 1").Scan(&code))
	if code != "CODE\x00WITH\x1aCONTROL" {
		t.Fatalf("code=%q", code)
	}
	// Backups land in the directory and only the newest are kept.
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		_, err := backup.ToDir(context.Background(), sqlDB, dir, 2, time.Now().Add(time.Duration(i)*time.Second))
		must(t, err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 2 {
		t.Fatal("kept", len(files))
	}
}

// digestNotifier records digest messages per chat. Order and message notices arrive on other
// goroutines at the same time, so access is locked and only digests are kept.
type digestNotifier struct {
	mu   *sync.Mutex
	sent map[int64]string
}

func (digestNotifier) Notify(context.Context, uint64, int64, string, *shop.Link) error { return nil }
func (d digestNotifier) NotifyMerchant(ctx context.Context, id int64, html string) error {
	return d.NotifyStaff(ctx, []int64{id}, html)
}
func (d digestNotifier) NotifyStaff(_ context.Context, ids []int64, html string) error {
	if !strings.Contains(html, "待办") {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		d.sent[id] = html
	}
	return nil
}

func (d digestNotifier) get(id int64) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sent[id]
}

func (d digestNotifier) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.sent)
}

func TestDailyDigestListsEachMerchantsTodos(t *testing.T) {
	f := newShopFixture(t, 3, "physical") // stock 3 is below the default low-stock threshold of 5
	f.secondMerchant(t, 50)               // a merchant with nothing waiting gets no message
	must(t, f.db.Model(&model.Merchant{}).Where("id = ?", f.mer.ID).Update("notify_chat_id", 4001).Error)
	n := digestNotifier{mu: &sync.Mutex{}, sent: map[int64]string{}}
	svc := shop.New(&store.Store{DB: f.db}, f.w, n, nil).WithStaff([]int64{777})
	b, addr := f.buyer(t, 9501, "100")
	o, err := svc.CreateOrder(b, shop.CheckoutRequest{RequestID: "late", Items: []shop.CheckoutItem{{SKUID: f.sku.ID, Quantity: 1}}, DeliveryType: shop.DeliveryExpress, AddressID: addr})
	must(t, err)
	must(t, f.db.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Update("paid_at", time.Now().Add(-30*time.Hour)).Error)
	_, err = svc.SendToMerchant(b, "小明", f.mer.ID, shop.MessageInput{Content: "在吗"})
	must(t, err)

	d, err := svc.SendDigest(context.Background(), time.Now())
	must(t, err)
	if len(d.Merchants) != 1 || d.Merchants[0].MerchantID != f.mer.ID || d.Merchants[0].Overdue != 1 || d.Merchants[0].Unread != 1 || d.Merchants[0].LowStock != 1 {
		t.Fatalf("%+v", d.Merchants)
	}
	if !strings.Contains(n.get(4001), "超时未发货订单：1") || !strings.Contains(n.get(4001), "低库存规格：1") || !strings.Contains(n.get(777), "测试商家") {
		t.Fatalf("%q / %q", n.get(4001), n.get(777))
	}
	// Nothing waiting, nothing sent.
	must(t, f.db.Exec("UPDATE shop_skus SET stock = 100").Error)
	must(t, f.db.Exec("UPDATE shop_orders SET status = 'shipped'").Error)
	must(t, f.db.Exec("UPDATE shop_conversations SET merchant_unread = 0").Error)
	n = digestNotifier{mu: &sync.Mutex{}, sent: map[int64]string{}}
	svc = shop.New(&store.Store{DB: f.db}, f.w, n, nil).WithStaff([]int64{777})
	d, err = svc.SendDigest(context.Background(), time.Now())
	must(t, err)
	if len(d.Merchants) != 0 || n.count() != 0 {
		t.Fatalf("%+v %d", d, n.count())
	}
}
