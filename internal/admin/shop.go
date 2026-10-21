package admin

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
)

func (h *Handler) WithShop(s *shop.Service) *Handler { h.shop = s; return h }

func (h *Handler) registerShop(api *gin.RouterGroup, can func(...string) gin.HandlerFunc) {
	api.GET("/shop-categories", can("shop_products"), h.shopCategories)
	api.POST("/shop-categories", can("shop_products"), platformOnly, h.saveShopCategory)
	api.DELETE("/shop-categories/:id", can("shop_products"), platformOnly, h.deleteShopCategory)
	api.GET("/shop-products", can("shop_products"), h.shopProducts)
	api.POST("/shop-products", can("shop_products"), h.saveShopProduct)
	api.POST("/shop-products/codes", can("shop_products"), h.importShopCodes)

	api.GET("/shop-orders", can("shop_orders"), h.shopOrders)
	api.POST("/shop-orders/:id/fulfil", can("shop_orders"), h.fulfilShopOrder)
	api.POST("/shop-orders/:id/complete", can("shop_orders"), h.completeShopOrder)
	api.POST("/shop-orders/:id/cancel", can("shop_orders"), h.cancelShopOrder)

	api.GET("/shop-aftersales", can("shop_aftersales"), h.shopAfterSales)
	api.POST("/shop-aftersales/:id/approve", can("shop_aftersales"), h.approveShopAfterSale)
	api.POST("/shop-aftersales/:id/refund", can("shop_aftersales"), h.refundShopAfterSale)
	api.POST("/shop-aftersales/:id/reject", can("shop_aftersales"), h.rejectShopAfterSale)

	api.GET("/shop-reviews", can("shop_reviews"), h.shopReviews)
	api.POST("/shop-reviews/:id/reply", can("shop_reviews"), h.replyShopReview)
	api.POST("/shop-reviews/:id/hidden", can("shop_reviews"), platformOnly, h.hideShopReview)

	api.GET("/shop-banners", can("shop_marketing"), h.shopBanners)
	api.POST("/shop-banners", can("shop_marketing"), h.saveShopBanner)
	api.DELETE("/shop-banners/:id", can("shop_marketing"), h.deleteShopBanner)

	api.GET("/shop-reports", can("shop_reports"), h.shopReport)
	api.GET("/shop-orders/export", can("shop_orders", "shop_reports"), h.exportShopOrders)

	api.GET("/shop-settlements", can("shop_settlements"), h.shopSettlements)
	api.GET("/shop-settlements/balances", can("shop_settlements"), h.shopBalances)
	api.GET("/shop-settlements/entries", can("shop_settlements"), h.shopEntries)
	api.POST("/shop-settlements", can("shop_settlements"), platformOnly, h.createShopSettlement)
	api.POST("/shop-settlements/:id/paid", can("shop_settlements"), platformOnly, h.payShopSettlement)

	api.GET("/shop-members", can("shop_members"), h.shopMembers)
	api.POST("/shop-members/adjust", can("shop_members"), platformOnly, h.adjustShopPoints)

	api.GET("/shop-messages", can("shop_messages"), h.shopConversations)
	api.GET("/shop-messages/:id", can("shop_messages"), h.shopThread)
	api.POST("/shop-messages/:id/reply", can("shop_messages"), h.replyShopMessage)

	api.GET("/shop-applications", can("shop_applications"), platformOnly, h.shopApplications)
	api.POST("/shop-applications/:id/approve", can("shop_applications"), platformOnly, h.approveShopApplication)
	api.POST("/shop-applications/:id/reject", can("shop_applications"), platformOnly, h.rejectShopApplication)

	api.GET("/shop-coupons", can("shop_coupons"), h.shopCoupons)
	api.POST("/shop-coupons", can("shop_coupons"), h.saveShopCoupon)
	api.POST("/shop-coupons/:id/issue", can("shop_coupons"), h.issueShopCoupon)
}

func (h *Handler) shopCategories(c *gin.Context) {
	rows, err := h.shop.AdminCategories()
	listOrError(c, rows, err)
}

func (h *Handler) saveShopCategory(c *gin.Context) {
	var b model.ShopCategory
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	respond(c, h.shop.SaveCategory(b))
}

func (h *Handler) deleteShopCategory(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.shop.DeleteCategory(id))
}

func (h *Handler) shopProducts(c *gin.Context) {
	rows, err := h.shop.AdminProducts(merchantScope(c))
	listOrError(c, rows, err)
}

func (h *Handler) saveShopProduct(c *gin.Context) {
	var b struct {
		model.ShopProduct
		SKUs []shop.SKUInput `json:"skus"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	id, err := h.shop.SaveProduct(b.ShopProduct, b.SKUs, merchantScope(c))
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true, "id": id})
}

func (h *Handler) importShopCodes(c *gin.Context) {
	var b struct {
		SKUID uint64 `json:"sku_id"`
		Codes string `json:"codes"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	n, err := h.shop.ImportCodes(b.SKUID, b.Codes, merchantScope(c))
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, gin.H{"ok": true, "imported": n})
}

func (h *Handler) shopOrders(c *gin.Context) {
	rows, err := h.shop.AdminOrders(shop.AdminOrderQuery{Status: c.Query("status"), Keyword: c.Query("q"), MerchantID: merchantScope(c)})
	listOrError(c, rows, err)
}

func (h *Handler) fulfilShopOrder(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Carrier    string `json:"carrier"`
		TrackingNo string `json:"tracking_no"`
		Note       string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.Fulfil(id, shop.Scope{MerchantID: merchantScope(c)}, b.Carrier, b.TrackingNo, b.Note, operator(c)))
}

func (h *Handler) completeShopOrder(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.shop.CompleteByAdmin(id, shop.Scope{MerchantID: merchantScope(c)}, operator(c)))
}

func (h *Handler) cancelShopOrder(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.CancelByAdmin(id, shop.Scope{MerchantID: merchantScope(c)}, b.Reason, operator(c)))
}

func (h *Handler) shopCoupons(c *gin.Context) {
	rows, err := h.shop.AdminCoupons()
	listOrError(c, rows, err)
}

func (h *Handler) saveShopCoupon(c *gin.Context) {
	var b struct {
		model.ShopCoupon
		EndsAt string `json:"ends_at"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	b.ShopCoupon.EndsAt = nil
	if s := strings.TrimSpace(b.EndsAt); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			c.JSON(400, gin.H{"error": "截止时间格式错误"})
			return
		}
		b.ShopCoupon.EndsAt = &t
	}
	respond(c, h.shop.SaveCoupon(b.ShopCoupon))
}

// issueShopCoupon accepts Telegram user IDs separated by commas, spaces or new lines.
func (h *Handler) issueShopCoupon(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		TelegramIDs string `json:"telegram_ids"`
	}
	_ = c.ShouldBindJSON(&b)
	ids := []int64{}
	for _, f := range strings.FieldsFunc(b.TelegramIDs, func(r rune) bool { return r == ',' || r == '，' || r == ' ' || r == '\n' || r == '\t' }) {
		n, err := strconv.ParseInt(f, 10, 64)
		if err != nil || n <= 0 {
			c.JSON(400, gin.H{"error": "Telegram ID 格式错误：" + f})
			return
		}
		ids = append(ids, n)
	}
	if len(ids) == 0 || len(ids) > 1000 {
		c.JSON(400, gin.H{"error": "请填写 1–1000 个 Telegram ID"})
		return
	}
	issued, skipped, err := h.shop.IssueCoupon(id, ids)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error(), "issued": issued})
		return
	}
	c.JSON(200, gin.H{"ok": true, "issued": issued, "skipped": skipped})
}

func (h *Handler) shopAfterSales(c *gin.Context) {
	rows, err := h.shop.AdminAfterSales(c.Query("status"), merchantScope(c))
	listOrError(c, rows, err)
}

func (h *Handler) approveShopAfterSale(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Address string `json:"address"`
		Note    string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.ApproveReturn(id, shop.Scope{MerchantID: merchantScope(c)}, b.Address, b.Note, operator(c)))
}

func (h *Handler) refundShopAfterSale(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Amount string `json:"amount"`
		Note   string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.RefundAfterSale(id, shop.Scope{MerchantID: merchantScope(c)}, b.Amount, b.Note, operator(c)))
}

func (h *Handler) rejectShopAfterSale(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.RejectAfterSale(id, shop.Scope{MerchantID: merchantScope(c)}, b.Note, operator(c)))
}

func (h *Handler) shopReviews(c *gin.Context) {
	rows, err := h.shop.AdminReviews(merchantScope(c))
	listOrError(c, rows, err)
}

func (h *Handler) replyShopReview(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Reply string `json:"reply"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.ReplyReview(id, shop.Scope{MerchantID: merchantScope(c)}, b.Reply))
}

func (h *Handler) hideShopReview(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Hidden bool `json:"hidden"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.SetReviewHidden(id, b.Hidden))
}

func (h *Handler) shopBanners(c *gin.Context) {
	rows, err := h.shop.AdminBanners()
	listOrError(c, rows, err)
}

func (h *Handler) saveShopBanner(c *gin.Context) {
	var b model.ShopBanner
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	respond(c, h.shop.SaveBanner(b))
}

func (h *Handler) deleteShopBanner(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.shop.DeleteBanner(id))
}

// dateRange reads from/to (YYYY-MM-DD, both inclusive, server time) and defaults to the last 30 days.
func dateRange(c *gin.Context) (time.Time, time.Time, bool) {
	today := time.Now()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	from, to := today.AddDate(0, 0, -29), today
	var err error
	if s := c.Query("from"); s != "" {
		if from, err = time.ParseInLocation("2006-01-02", s, time.Local); err != nil {
			c.JSON(400, gin.H{"error": "开始日期格式应为 YYYY-MM-DD"})
			return from, to, false
		}
	}
	if s := c.Query("to"); s != "" {
		if to, err = time.ParseInLocation("2006-01-02", s, time.Local); err != nil {
			c.JSON(400, gin.H{"error": "结束日期格式应为 YYYY-MM-DD"})
			return from, to, false
		}
	}
	return from, to.AddDate(0, 0, 1), true
}

func (h *Handler) shopReport(c *gin.Context) {
	from, to, ok := dateRange(c)
	if !ok {
		return
	}
	r, err := h.shop.SalesReport(merchantScope(c), from, to)
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, r)
}

func (h *Handler) exportShopOrders(c *gin.Context) {
	from, to, ok := dateRange(c)
	if !ok {
		return
	}
	var buf bytes.Buffer
	if err := h.shop.ExportOrders(&buf, merchantScope(c), c.Query("status"), from, to); err != nil {
		respond(c, err)
		return
	}
	name := fmt.Sprintf("shop-orders-%s-%s.csv", from.Format("20060102"), to.AddDate(0, 0, -1).Format("20060102"))
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Data(200, "text/csv; charset=utf-8", buf.Bytes())
}

func (h *Handler) shopSettlements(c *gin.Context) {
	rows, err := h.shop.Settlements(merchantScope(c))
	listOrError(c, rows, err)
}

func (h *Handler) shopBalances(c *gin.Context) {
	rows, err := h.shop.MerchantBalances(merchantScope(c))
	listOrError(c, rows, err)
}

func (h *Handler) shopEntries(c *gin.Context) {
	settlementID, _ := strconv.ParseUint(c.Query("settlement_id"), 10, 64)
	merchantID, _ := strconv.ParseUint(c.Query("merchant_id"), 10, 64)
	if scope := merchantScope(c); scope != 0 {
		merchantID = scope
	}
	rows, err := h.shop.MerchantEntries(merchantID, settlementID)
	listOrError(c, rows, err)
}

func (h *Handler) createShopSettlement(c *gin.Context) {
	var b struct {
		MerchantID uint64 `json:"merchant_id"`
		Note       string `json:"note"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.MerchantID == 0 {
		c.JSON(400, gin.H{"error": "请选择商家"})
		return
	}
	st, err := h.shop.CreateSettlement(b.MerchantID, b.Note, operator(c))
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, st)
}

func (h *Handler) payShopSettlement(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		PayRef string `json:"pay_ref"`
		Note   string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.MarkSettlementPaid(id, b.PayRef, b.Note, operator(c)))
}

func (h *Handler) shopMembers(c *gin.Context) {
	rows, err := h.shop.AdminMembers()
	listOrError(c, rows, err)
}

func (h *Handler) adjustShopPoints(c *gin.Context) {
	var b struct {
		TelegramUserID int64  `json:"telegram_user_id"`
		Delta          int64  `json:"delta"`
		Reason         string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.TelegramUserID <= 0 {
		c.JSON(400, gin.H{"error": "请填写 Telegram ID"})
		return
	}
	respond(c, h.shop.AdjustPoints(b.TelegramUserID, b.Delta, b.Reason, operator(c)))
}

func (h *Handler) shopConversations(c *gin.Context) {
	rows, err := h.shop.AdminConversations(merchantScope(c))
	listOrError(c, rows, err)
}

func (h *Handler) shopThread(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	t, err := h.shop.AdminThread(id, shop.Scope{MerchantID: merchantScope(c)})
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, t)
}

func (h *Handler) replyShopMessage(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Content string `json:"content"`
	}
	_ = c.ShouldBindJSON(&b)
	respond(c, h.shop.ReplyToBuyer(id, shop.Scope{MerchantID: merchantScope(c)}, b.Content, operator(c)))
}

func listOrError(c *gin.Context, rows any, err error) {
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, rows)
}
