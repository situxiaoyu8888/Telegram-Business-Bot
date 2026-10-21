package shop

import (
	"encoding/csv"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"telegram-business-bot/internal/model"
)

// DayStat is one day of shop activity. Sales are what buyers paid for orders placed that day
// (canceled orders excluded); Refunds are after-sale refunds paid that day.
type DayStat struct {
	Day      string `json:"day"`
	Orders   int64  `json:"orders"`
	Sales    string `json:"sales"`
	Canceled int64  `json:"canceled"`
	Refunds  string `json:"refunds"`
}

type TopProduct struct {
	ProductID   uint64 `json:"product_id"`
	ProductName string `json:"product_name"`
	Quantity    int64  `json:"quantity"`
	Sales       string `json:"sales"` // purchase price × quantity, before coupon discounts
}

type LowStock struct {
	SKUID        uint64 `json:"sku_id"`
	ProductID    uint64 `json:"product_id"`
	ProductName  string `json:"product_name"`
	SKUName      string `json:"sku_name"`
	MerchantName string `json:"merchant_name"`
	Stock        int64  `json:"stock"`
}

type Report struct {
	From        string       `json:"from"`
	To          string       `json:"to"`
	Orders      int64        `json:"orders"`
	Sales       string       `json:"sales"`
	Canceled    int64        `json:"canceled"`
	Refunds     string       `json:"refunds"`
	Days        []DayStat    `json:"days"`
	Top         []TopProduct `json:"top"`
	LowStock    []LowStock   `json:"low_stock"`
	LowStockMax int          `json:"low_stock_max"`
}

// SalesReport summarises orders placed in [from, to) (dates in server time), limited to a
// merchant when merchantID is set.
func (s *Service) SalesReport(merchantID uint64, from, to time.Time) (*Report, error) {
	if !to.After(from) || to.Sub(from) > 366*24*time.Hour {
		return nil, userErr("统计区间需在 1–366 天之间")
	}
	scope := func(column string) (string, []any) {
		if merchantID == 0 {
			return "", nil
		}
		return " AND " + column + " = ?", []any{merchantID}
	}
	r := &Report{From: from.Format("2006-01-02"), To: to.AddDate(0, 0, -1).Format("2006-01-02"), Days: []DayStat{}, Top: []TopProduct{}, LowStock: []LowStock{}}

	var orderRows []struct {
		Day      string
		Orders   int64
		Sales    string
		Canceled int64
	}
	extra, args := scope("merchant_id")
	if err := s.st.DB.Raw(`SELECT DATE_FORMAT(created_at, '%Y-%m-%d') AS day,
		SUM(status <> 'canceled') AS orders,
		COALESCE(SUM(CASE WHEN status <> 'canceled' THEN pay_amount ELSE 0 END), 0) AS sales,
		SUM(status = 'canceled') AS canceled
		FROM shop_orders WHERE created_at >= ? AND created_at < ?`+extra+` GROUP BY day`, append([]any{from, to}, args...)...).Scan(&orderRows).Error; err != nil {
		return nil, err
	}
	var refundRows []struct {
		Day     string
		Refunds string
	}
	extra, args = scope("merchant_id")
	if err := s.st.DB.Raw(`SELECT DATE_FORMAT(closed_at, '%Y-%m-%d') AS day, COALESCE(SUM(refund_amount), 0) AS refunds
		FROM shop_after_sales WHERE status = 'refunded' AND closed_at >= ? AND closed_at < ?`+extra+` GROUP BY day`, append([]any{from, to}, args...)...).Scan(&refundRows).Error; err != nil {
		return nil, err
	}
	byDay := map[string]*DayStat{}
	for d := from; d.Before(to); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		r.Days = append(r.Days, DayStat{Day: key, Sales: "0.000000", Refunds: "0.000000"})
		byDay[key] = &r.Days[len(r.Days)-1]
	}
	sales, refunds := decimal.Zero, decimal.Zero
	for _, row := range orderRows {
		if d := byDay[row.Day]; d != nil {
			d.Orders, d.Sales, d.Canceled = row.Orders, amount(row.Sales).StringFixed(6), row.Canceled
			r.Orders += row.Orders
			r.Canceled += row.Canceled
			sales = sales.Add(amount(row.Sales))
		}
	}
	for _, row := range refundRows {
		if d := byDay[row.Day]; d != nil {
			d.Refunds = amount(row.Refunds).StringFixed(6)
			refunds = refunds.Add(amount(row.Refunds))
		}
	}
	r.Sales, r.Refunds = sales.StringFixed(6), refunds.StringFixed(6)

	extra, args = scope("o.merchant_id")
	if err := s.st.DB.Raw(`SELECT i.product_id, MAX(i.product_name) AS product_name, SUM(i.quantity) AS quantity, SUM(i.price * i.quantity) AS sales
		FROM shop_order_items i JOIN shop_orders o ON o.id = i.order_id
		WHERE o.status <> 'canceled' AND o.created_at >= ? AND o.created_at < ?`+extra+`
		GROUP BY i.product_id ORDER BY quantity DESC, sales DESC LIMIT 10`, append([]any{from, to}, args...)...).Scan(&r.Top).Error; err != nil {
		return nil, err
	}
	for i := range r.Top {
		r.Top[i].Sales = amount(r.Top[i].Sales).StringFixed(6)
	}

	r.LowStockMax = s.configInt("shop_low_stock", 5)
	extra, args = scope("p.merchant_id")
	if err := s.st.DB.Raw(`SELECT k.id AS sku_id, p.id AS product_id, p.name AS product_name, k.name AS sku_name, m.name AS merchant_name, k.stock
		FROM shop_skus k JOIN shop_products p ON p.id = k.product_id JOIN merchants m ON m.id = p.merchant_id
		WHERE k.enabled = ? AND p.enabled = ? AND k.stock <= ?`+extra+` ORDER BY k.stock ASC, k.id ASC LIMIT 100`, append([]any{true, true, r.LowStockMax}, args...)...).Scan(&r.LowStock).Error; err != nil {
		return nil, err
	}
	return r, nil
}

var statusNames = map[string]string{StatusGrouping: "拼团中", StatusPaid: "待处理", StatusShipped: "已发货", StatusReady: "待自提", StatusCompleted: "已完成", StatusCanceled: "已取消"}

// ExportOrders writes orders placed in [from, to) as CSV with a UTF-8 BOM so Excel shows Chinese correctly.
func (s *Service) ExportOrders(w io.Writer, merchantID uint64, status string, from, to time.Time) error {
	if !to.After(from) || to.Sub(from) > 366*24*time.Hour {
		return userErr("导出区间需在 1–366 天之间")
	}
	q := s.st.DB.Model(&model.ShopOrder{}).Where("created_at >= ? AND created_at < ?", from, to)
	if merchantID != 0 {
		q = q.Where("merchant_id = ?", merchantID)
	}
	if status != "" {
		q = q.Where("status = ?", status)
	}
	var orders []model.ShopOrder
	if err := q.Order("id asc").Limit(50000).Find(&orders).Error; err != nil {
		return err
	}
	details, err := s.withItems(orders)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"订单号", "下单时间", "商家", "买家 Telegram ID", "配送方式", "状态", "商品明细", "商品金额", "优惠", "实付", "已退款", "收货人", "电话", "地址", "快递公司", "快递单号", "备注", "售后中"})
	for _, o := range details {
		items := make([]string, 0, len(o.Items))
		for _, it := range o.Items {
			items = append(items, it.ProductName+" "+it.SKUName+" ×"+strconv.FormatInt(it.Quantity, 10))
		}
		open := ""
		if o.AfterSaleOpen {
			open = "是"
		}
		// Leading characters Excel treats as formulas are neutralised in free-text cells.
		_ = cw.Write([]string{o.OrderNo, o.CreatedAt.Format("2006-01-02 15:04:05"), safeCell(o.MerchantName), strconv.FormatInt(o.TelegramUserID, 10), deliveryNames[o.DeliveryType], statusNames[o.Status], safeCell(strings.Join(items, "; ")), o.GoodsAmount, o.Discount, o.PayAmount, o.RefundedAmount, safeCell(o.ContactName), safeCell(o.ContactPhone), safeCell(o.Address), safeCell(o.Carrier), safeCell(o.TrackingNo), safeCell(o.Remark), open})
	}
	cw.Flush()
	return cw.Error()
}

func safeCell(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}
