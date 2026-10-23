package admin

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
)

// currencyStats are one wallet currency's figures on the home page. Flows (deposits,
// withdrawals, red packets, exchanges, transfers, console adjustments) cover the chosen period;
// balances are the current totals.
type currencyStats struct {
	Code             string `json:"code"`
	Name             string `json:"name"`
	DepositCount     int64  `json:"deposit_count"`
	DepositAmount    string `json:"deposit_amount"`
	WithdrawCount    int64  `json:"withdraw_count"`
	WithdrawAmount   string `json:"withdraw_amount"`
	ReviewCount      int64  `json:"review_count"` // withdrawals still pending or approved
	ReviewAmount     string `json:"review_amount"`
	NetFlow          string `json:"net_flow"` // deposits − completed withdrawals
	CreditAmount     string `json:"credit_amount"`
	DebitAmount      string `json:"debit_amount"`
	PacketCount      int64  `json:"packet_count"`
	PacketAmount     string `json:"packet_amount"`
	PacketClaimed    string `json:"packet_claimed"`
	PacketRefunded   string `json:"packet_refunded"`
	ExchangeOut      string `json:"exchange_out"`
	ExchangeIn       string `json:"exchange_in"`
	TransferCount    int64  `json:"transfer_count"`
	TransferAmount   string `json:"transfer_amount"`
	BalanceAvailable string `json:"balance_available"`
	BalanceFrozen    string `json:"balance_frozen"`
}

type sumRow struct {
	Key   string
	N     int64
	Total decimal.NullDecimal
	Extra decimal.NullDecimal
	More  decimal.NullDecimal
}

// overviewSince is the start of the period named by range: today, 7d, 30d or all (zero time).
func overviewSince(r string, now time.Time) (string, time.Time) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch r {
	case "today":
		return r, day
	case "7d":
		return r, day.AddDate(0, 0, -6)
	case "30d":
		return r, day.AddDate(0, 0, -29)
	}
	return "all", time.Time{}
}

// overview serves the console home page: what needs attention now and the platform's figures.
func (h *Handler) overview(c *gin.Context) {
	rng, since := overviewSince(c.Query("range"), time.Now())
	db := h.st.DB
	period := func(q *gorm.DB, col string) *gorm.DB {
		if since.IsZero() {
			return q
		}
		return q.Where(col+" >= ?", since)
	}
	count := func(m any, where string, args ...any) int64 {
		var n int64
		q := db.Model(m)
		if where != "" {
			q = q.Where(where, args...)
		}
		_ = q.Count(&n).Error
		return n
	}
	countIn := func(m any, where string, args ...any) int64 {
		var n int64
		q := period(db.Model(m), "created_at")
		if where != "" {
			q = q.Where(where, args...)
		}
		_ = q.Count(&n).Error
		return n
	}
	sums := func(q *gorm.DB, sel string) map[string]sumRow {
		var rows []sumRow
		out := map[string]sumRow{}
		if err := q.Select(sel).Scan(&rows).Error; err == nil {
			for _, r := range rows {
				out[r.Key] = r
			}
		}
		return out
	}
	fixed := func(d decimal.NullDecimal) string {
		if !d.Valid {
			return "0"
		}
		return d.Decimal.String()
	}

	todo := gin.H{
		"pending_withdrawals": count(&model.WithdrawalOrder{}, "status = 'pending'"),
		"pending_deposits":    count(&model.DepositOrder{}, "status = 'pending'"),
		"unclaimed_deposits":  count(&model.DepositOrder{}, "status = 'unclaimed'"),
		"open_tickets":        count(&model.SupportTicket{}, "status = 'open'"),
		"active_pushes":       count(&model.PushCampaign{}, "status IN ('queued','scheduled','processing')"),
		"pending_orders":      count(&model.ShopOrder{}, "status = 'paid'"),
		"pending_aftersales":  count(&model.ShopAfterSale{}, "status = 'pending'"),
		"pending_merchants":   count(&model.ShopMerchantApplication{}, "status = 'pending'"),
	}
	platform := gin.H{
		"users":             count(&model.TelegramUser{}, ""),
		"new_users":         countIn(&model.TelegramUser{}, ""),
		"merchants":         count(&model.Merchant{}, ""),
		"enabled_merchants": count(&model.Merchant{}, "enabled = ?", true),
		"products":          count(&model.ShopProduct{}, "enabled = ?", true),
		"bots":              count(&model.BusinessBot{}, "enabled = ?", true),
		"tickets":           countIn(&model.SupportTicket{}, ""),
		"news":              countIn(&model.NewsArticle{}, ""),
		"pushes":            countIn(&model.PushCampaign{}, ""),
	}

	// Shop orders (USDT): every order placed in the period, by status, with what was paid and
	// refunded (canceled orders are refunded in full).
	var statusRows []struct {
		Status string
		N      int64
		Pay    decimal.NullDecimal
		Refund decimal.NullDecimal
	}
	_ = period(db.Model(&model.ShopOrder{}), "created_at").Select("status, COUNT(*) AS n, SUM(pay_amount) AS pay, SUM(refunded_amount) AS refund").Group("status").Scan(&statusRows).Error
	byStatus := map[string]int64{}
	var orders int64
	paid, refunded := decimal.Zero, decimal.Zero
	for _, r := range statusRows {
		byStatus[r.Status] = r.N
		orders += r.N
		if r.Pay.Valid {
			paid = paid.Add(r.Pay.Decimal)
		}
		if r.Refund.Valid {
			refunded = refunded.Add(r.Refund.Decimal)
		}
	}
	shopStats := gin.H{"orders": orders, "by_status": byStatus, "paid": paid.String(), "refunded": refunded.String(), "net": paid.Sub(refunded).String()}

	// Wallet currencies.
	var currencies []model.WalletCurrency
	_ = db.Order("sort asc, id asc").Find(&currencies).Error
	deposits := sums(period(db.Model(&model.DepositOrder{}).Where("status = 'credited'"), "created_at").Group("currency"), "currency AS `key`, COUNT(*) AS n, SUM(amount) AS total")
	withdrawn := sums(period(db.Model(&model.WithdrawalOrder{}).Where("status = 'completed'"), "created_at").Group("currency"), "currency AS `key`, COUNT(*) AS n, SUM(amount) AS total")
	reviewing := sums(period(db.Model(&model.WithdrawalOrder{}).Where("status IN ('pending','approved')"), "created_at").Group("currency"), "currency AS `key`, COUNT(*) AS n, SUM(amount) AS total")
	credits := sums(period(db.Model(&model.WalletTransaction{}).Where("type IN ('admin_credit','admin_debit')"), "created_at").Group("currency"), "currency AS `key`, COUNT(*) AS n, SUM(CASE WHEN type = 'admin_credit' THEN amount ELSE 0 END) AS total, SUM(CASE WHEN type = 'admin_debit' THEN -amount ELSE 0 END) AS extra")
	packets := sums(period(db.Model(&model.RedPacket{}), "created_at").Group("currency"), "currency AS `key`, COUNT(*) AS n, SUM(total) AS total, SUM(claimed_amount) AS extra, SUM(refund_amount) AS more")
	exOut := sums(period(db.Model(&model.WalletExchange{}), "created_at").Group("from_currency"), "from_currency AS `key`, COUNT(*) AS n, SUM(amount) AS total")
	exIn := sums(period(db.Model(&model.WalletExchange{}), "created_at").Group("to_currency"), "to_currency AS `key`, COUNT(*) AS n, SUM(received) AS total")
	transfers := sums(period(db.Model(&model.WalletTransfer{}), "created_at").Group("currency"), "currency AS `key`, COUNT(*) AS n, SUM(amount) AS total")
	balances := sums(db.Model(&model.WalletAccount{}).Group("currency"), "currency AS `key`, COUNT(*) AS n, SUM(available) AS total, SUM(frozen) AS extra")

	list := make([]currencyStats, 0, len(currencies))
	for _, cur := range currencies {
		d, w := deposits[cur.Code], withdrawn[cur.Code]
		net := decimal.Zero
		if d.Total.Valid {
			net = net.Add(d.Total.Decimal)
		}
		if w.Total.Valid {
			net = net.Sub(w.Total.Decimal)
		}
		p, b := packets[cur.Code], balances[cur.Code]
		list = append(list, currencyStats{
			Code: cur.Code, Name: cur.Name,
			DepositCount: d.N, DepositAmount: fixed(d.Total),
			WithdrawCount: w.N, WithdrawAmount: fixed(w.Total),
			ReviewCount: reviewing[cur.Code].N, ReviewAmount: fixed(reviewing[cur.Code].Total),
			NetFlow:      net.String(),
			CreditAmount: fixed(credits[cur.Code].Total), DebitAmount: fixed(credits[cur.Code].Extra),
			PacketCount: p.N, PacketAmount: fixed(p.Total), PacketClaimed: fixed(p.Extra), PacketRefunded: fixed(p.More),
			ExchangeOut: fixed(exOut[cur.Code].Total), ExchangeIn: fixed(exIn[cur.Code].Total),
			TransferCount: transfers[cur.Code].N, TransferAmount: fixed(transfers[cur.Code].Total),
			BalanceAvailable: fixed(b.Total), BalanceFrozen: fixed(b.Extra),
		})
	}
	from := ""
	if !since.IsZero() {
		from = since.Format("2006-01-02")
	}
	c.JSON(http.StatusOK, gin.H{"range": rng, "since": from, "todo": todo, "platform": platform, "shop": shopStats, "currencies": list})
}
