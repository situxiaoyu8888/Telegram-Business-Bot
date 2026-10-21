package admin

import (
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

func listOrError(c *gin.Context, rows any, err error) {
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, rows)
}
