package shop

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"telegram-business-bot/internal/model"
)

// MerchantTodo is what one merchant has waiting.
type MerchantTodo struct {
	MerchantID   uint64   `json:"merchant_id"`
	MerchantName string   `json:"merchant_name"`
	LowStock     int      `json:"low_stock"`
	LowStockList []string `json:"low_stock_list"` // up to 5 "product · sku (stock)"
	Overdue      int      `json:"overdue"`        // paid orders not shipped within shop_ship_overdue_hours
	AfterSales   int      `json:"after_sales"`    // open after-sale requests
	Unread       int      `json:"unread"`         // conversations with unread buyer messages
	NotifyChatID int64    `json:"-"`
}

func (t MerchantTodo) empty() bool {
	return t.LowStock+t.Overdue+t.AfterSales+t.Unread == 0
}

// Digest is the daily to-do summary; Sent counts the messages delivered.
type Digest struct {
	Merchants []MerchantTodo `json:"merchants"`
	Sent      int            `json:"sent"`
}

// Todos collects what each merchant has waiting at now.
func (s *Service) Todos(ctx context.Context, now time.Time) ([]MerchantTodo, error) {
	db := s.st.DB.WithContext(ctx)
	var merchants []model.Merchant
	if err := db.Select("id", "name", "notify_chat_id").Where("enabled = ?", true).Order("id asc").Find(&merchants).Error; err != nil {
		return nil, err
	}
	todos := map[uint64]*MerchantTodo{}
	out := make([]*MerchantTodo, 0, len(merchants))
	for _, m := range merchants {
		t := &MerchantTodo{MerchantID: m.ID, MerchantName: m.Name, NotifyChatID: m.NotifyChatID, LowStockList: []string{}}
		todos[m.ID] = t
		out = append(out, t)
	}
	var low []struct {
		MerchantID  uint64
		ProductName string
		SKUName     string
		Stock       int64
	}
	if err := db.Raw(`SELECT p.merchant_id, p.name AS product_name, k.name AS sku_name, k.stock FROM shop_skus k JOIN shop_products p ON p.id = k.product_id
		WHERE k.enabled = ? AND p.enabled = ? AND k.stock <= ? ORDER BY k.stock ASC, k.id ASC`, true, true, s.configInt("shop_low_stock", 5)).Scan(&low).Error; err != nil {
		return nil, err
	}
	for _, l := range low {
		if t := todos[l.MerchantID]; t != nil {
			t.LowStock++
			if len(t.LowStockList) < 5 {
				t.LowStockList = append(t.LowStockList, fmt.Sprintf("%s · %s（%d）", l.ProductName, l.SKUName, l.Stock))
			}
		}
	}
	count := func(query string, args []any, apply func(*MerchantTodo, int)) error {
		var rows []struct {
			MerchantID uint64
			N          int
		}
		if err := db.Raw(query, args...).Scan(&rows).Error; err != nil {
			return err
		}
		for _, r := range rows {
			if t := todos[r.MerchantID]; t != nil {
				apply(t, r.N)
			}
		}
		return nil
	}
	overdueBefore := now.Add(-time.Duration(s.configInt("shop_ship_overdue_hours", 24)) * time.Hour)
	if err := count(`SELECT merchant_id, COUNT(*) AS n FROM shop_orders WHERE status = ? AND paid_at < ? GROUP BY merchant_id`, []any{StatusPaid, overdueBefore}, func(t *MerchantTodo, n int) { t.Overdue = n }); err != nil {
		return nil, err
	}
	if err := count(`SELECT merchant_id, COUNT(*) AS n FROM shop_after_sales WHERE status IN ? GROUP BY merchant_id`, []any{[]string{AfterSalePending, AfterSaleApproved}}, func(t *MerchantTodo, n int) { t.AfterSales = n }); err != nil {
		return nil, err
	}
	if err := count(`SELECT merchant_id, COUNT(*) AS n FROM shop_conversations WHERE merchant_unread > 0 GROUP BY merchant_id`, nil, func(t *MerchantTodo, n int) { t.Unread = n }); err != nil {
		return nil, err
	}
	res := make([]MerchantTodo, 0, len(out))
	for _, t := range out {
		if !t.empty() {
			res = append(res, *t)
		}
	}
	return res, nil
}

func todoLines(t MerchantTodo) []string {
	lines := []string{}
	if t.Overdue > 0 {
		lines = append(lines, fmt.Sprintf("🚚 超时未发货订单：%d", t.Overdue))
	}
	if t.AfterSales > 0 {
		lines = append(lines, fmt.Sprintf("🧾 待处理售后：%d", t.AfterSales))
	}
	if t.Unread > 0 {
		lines = append(lines, fmt.Sprintf("💬 未回复的买家消息：%d", t.Unread))
	}
	if t.LowStock > 0 {
		lines = append(lines, fmt.Sprintf("📦 低库存规格：%d", t.LowStock))
		for _, l := range t.LowStockList {
			lines = append(lines, "  · "+htmlEscape(l))
		}
	}
	return lines
}

// SendDigest sends each merchant's notification contact its own to-do list and administrators a
// platform summary. Nothing is sent to anyone with nothing waiting.
func (s *Service) SendDigest(ctx context.Context, now time.Time) (*Digest, error) {
	todos, err := s.Todos(ctx, now)
	if err != nil {
		return nil, err
	}
	d := &Digest{Merchants: todos}
	if s.notifier == nil {
		return d, nil
	}
	for _, t := range todos {
		if t.NotifyChatID == 0 {
			continue
		}
		html := fmt.Sprintf("📋 <b>%s · 今日待办</b>\n\n%s\n\n请登录后台处理。", htmlEscape(t.MerchantName), strings.Join(todoLines(t), "\n"))
		if err := s.notifier.NotifyStaff(ctx, []int64{t.NotifyChatID}, html); err == nil {
			d.Sent++
		}
	}
	if len(todos) > 0 && len(s.staff) > 0 {
		sorted := append([]MerchantTodo{}, todos...)
		sort.Slice(sorted, func(i, j int) bool {
			return sorted[i].Overdue+sorted[i].AfterSales > sorted[j].Overdue+sorted[j].AfterSales
		})
		var b strings.Builder
		b.WriteString("📋 <b>商城今日待办汇总</b>\n")
		for i, t := range sorted {
			if i == 10 {
				fmt.Fprintf(&b, "\n…还有 %d 个商家", len(sorted)-10)
				break
			}
			fmt.Fprintf(&b, "\n<b>%s</b>：未发货 %d · 售后 %d · 未回复 %d · 低库存 %d", htmlEscape(t.MerchantName), t.Overdue, t.AfterSales, t.Unread, t.LowStock)
		}
		if err := s.notifier.NotifyStaff(ctx, s.staff, b.String()); err == nil {
			d.Sent += len(s.staff)
		}
	}
	return d, nil
}

// maybeSendDigest sends the digest once a day at shop_digest_hour (-1 disables). A Redis key per
// date makes sure only one replica sends it.
func (s *Service) maybeSendDigest(ctx context.Context, now time.Time) error {
	hour, err := strconv.Atoi(s.configString("shop_digest_hour"))
	if err != nil {
		hour = 9
	}
	if hour < 0 || hour > 23 || now.Hour() != hour || s.st.Redis == nil {
		return nil
	}
	ok, err := s.st.Redis.SetNX(ctx, "shop:digest:"+now.Format("2006-01-02"), "1", 26*time.Hour).Result()
	if err != nil || !ok {
		return err
	}
	_, err = s.SendDigest(ctx, now)
	return err
}
