package shop

import (
	"context"
	_ "embed"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"telegram-business-bot/internal/business"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"
)

//go:embed web/index.html
var indexHTML []byte

// initDataMaxAge bounds how long a Mini App session stays valid without reopening the shop.
const initDataMaxAge = 24 * time.Hour

const buyerKey = "shop_buyer"

// TokenSource lists the bots whose Mini App sign-ins are accepted.
type TokenSource interface {
	Tokens(ctx context.Context) ([]business.BotToken, error)
}

type Handler struct {
	svc    *Service
	st     *store.Store
	tokens TokenSource
	logger *zap.Logger

	mu       sync.Mutex
	cached   []business.BotToken
	cachedAt time.Time
	now      func() time.Time
}

func NewHandler(svc *Service, st *store.Store, tokens TokenSource, logger *zap.Logger) *Handler {
	return &Handler{svc: svc, st: st, tokens: tokens, logger: logger, now: time.Now}
}

// botTokens caches the token list briefly so each request does not decrypt every bot token.
func (h *Handler) botTokens(ctx context.Context) ([]business.BotToken, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cached != nil && time.Since(h.cachedAt) < 30*time.Second {
		return h.cached, nil
	}
	list, err := h.tokens.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	h.cached, h.cachedAt = list, time.Now()
	return list, nil
}

func (h *Handler) Register(r *gin.Engine) {
	page := func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", indexHTML)
	}
	r.GET("/shop", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, "/shop/") })
	r.GET("/shop/", page)

	api := r.Group("/shop/api", h.authenticate)
	api.GET("/me", h.me)
	api.GET("/categories", h.categories)
	api.GET("/products", h.products)
	api.GET("/products/:id", h.product)
	api.GET("/cart", h.cart)
	api.POST("/cart", h.setCart)
	api.GET("/addresses", h.addresses)
	api.POST("/addresses", h.saveAddress)
	api.DELETE("/addresses/:id", h.deleteAddress)
	api.GET("/coupons", h.myCoupons)
	api.GET("/coupons/center", h.couponCenter)
	api.POST("/coupons/:id/claim", h.claimCoupon)
	api.POST("/checkout/preview", h.preview)
	api.GET("/orders", h.orders)
	api.POST("/orders", h.createOrder)
	api.GET("/orders/:id", h.order)
	api.POST("/orders/:id/cancel", h.cancelOrder)
	api.POST("/orders/:id/confirm", h.confirmOrder)
}

// authenticate accepts requests signed by any enabled bot's Mini App and maps them to the
// same telegram_users row (and wallet) the bots use.
func (h *Handler) authenticate(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	initData := c.GetHeader("X-Telegram-Init-Data")
	if initData == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "请从 Telegram 机器人中打开商城"})
		return
	}
	tokens, err := h.botTokens(c.Request.Context())
	if err != nil {
		h.fail(c, err)
		return
	}
	var tgUser *telegram.User
	var botID uint64
	var lastErr error
	for _, t := range tokens {
		if tgUser, lastErr = telegram.VerifyInitData(initData, t.Token, initDataMaxAge, h.now()); lastErr == nil {
			botID = t.ID
			break
		}
	}
	if tgUser == nil {
		msg := "登录验证失败，请关闭后从机器人重新打开商城"
		if lastErr != nil && lastErr != telegram.ErrInvalidInitData {
			msg = "登录已过期，请关闭后从机器人重新打开商城"
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": msg})
		return
	}
	u, err := h.st.UpsertTelegramUser(tgUser.ID, tgUser.Username, tgUser.FirstName, tgUser.LastName, tgUser.LanguageCode)
	if err != nil {
		h.fail(c, err)
		return
	}
	if u.Status != 1 {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "账号已被限制使用"})
		return
	}
	c.Set(buyerKey, Buyer{UserID: u.ID, TelegramUserID: u.TelegramUserID, BotID: botID})
	c.Next()
}

func buyer(c *gin.Context) Buyer { return c.MustGet(buyerKey).(Buyer) }

// fail reports user errors as 400 and hides internal ones behind a generic message.
func (h *Handler) fail(c *gin.Context, err error) {
	if IsUserError(err) {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if h.logger != nil {
		h.logger.Error("shop request failed", zap.String("path", c.FullPath()), zap.Error(err))
	}
	c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "服务暂时不可用，请稍后重试"})
}

func (h *Handler) reply(c *gin.Context, data any, err error) {
	if err != nil {
		h.fail(c, err)
		return
	}
	c.JSON(http.StatusOK, data)
}

func idParam(c *gin.Context) uint64 {
	id, _ := strconv.ParseUint(c.Param("id"), 10, 64)
	return id
}
func queryUint(c *gin.Context, key string) uint64 {
	v, _ := strconv.ParseUint(c.Query(key), 10, 64)
	return v
}

func (h *Handler) me(c *gin.Context) {
	b := buyer(c)
	available, _, err := h.svc.wallet.Balance(b.UserID)
	if err != nil {
		h.fail(c, err)
		return
	}
	var u model.TelegramUser
	if err := h.st.DB.First(&u, b.UserID).Error; err != nil {
		h.fail(c, err)
		return
	}
	name := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if name == "" {
		name = u.Username
	}
	c.JSON(http.StatusOK, gin.H{"name": name, "username": u.Username, "telegram_user_id": u.TelegramUserID, "balance": available.StringFixed(6)})
}

func (h *Handler) categories(c *gin.Context) {
	rows, err := h.svc.Categories()
	h.reply(c, rows, err)
}

func (h *Handler) products(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	rows, err := h.svc.Products(ProductQuery{CategoryID: queryUint(c, "category_id"), MerchantID: queryUint(c, "merchant_id"), Keyword: c.Query("q"), Page: page})
	h.reply(c, rows, err)
}

func (h *Handler) product(c *gin.Context) {
	p, err := h.svc.Product(idParam(c))
	h.reply(c, p, err)
}

func (h *Handler) cart(c *gin.Context) {
	rows, err := h.svc.Cart(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) setCart(c *gin.Context) {
	var b struct {
		SKUID    uint64 `json:"sku_id"`
		Quantity int64  `json:"quantity"`
		Add      bool   `json:"add"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	h.reply(c, gin.H{"ok": true}, h.svc.SetCart(buyer(c).UserID, b.SKUID, b.Quantity, b.Add))
}

func (h *Handler) addresses(c *gin.Context) {
	rows, err := h.svc.Addresses(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) saveAddress(c *gin.Context) {
	var a model.ShopAddress
	if err := c.ShouldBindJSON(&a); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	id, err := h.svc.SaveAddress(buyer(c).UserID, a)
	h.reply(c, gin.H{"id": id}, err)
}

func (h *Handler) deleteAddress(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.DeleteAddress(buyer(c).UserID, idParam(c)))
}

func (h *Handler) myCoupons(c *gin.Context) {
	rows, err := h.svc.MyCoupons(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) couponCenter(c *gin.Context) {
	rows, err := h.svc.CouponCenter(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) claimCoupon(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.Claim(buyer(c).UserID, idParam(c)))
}

func (h *Handler) preview(c *gin.Context) {
	var req CheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	q, err := h.svc.Preview(buyer(c).UserID, req)
	h.reply(c, q, err)
}

func (h *Handler) createOrder(c *gin.Context) {
	var req CheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	o, err := h.svc.CreateOrder(buyer(c), req)
	h.reply(c, o, err)
}

func (h *Handler) orders(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	rows, err := h.svc.Orders(buyer(c).UserID, c.Query("status"), page)
	h.reply(c, rows, err)
}

func (h *Handler) order(c *gin.Context) {
	o, err := h.svc.Order(buyer(c).UserID, idParam(c))
	h.reply(c, o, err)
}

func (h *Handler) cancelOrder(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.CancelByBuyer(buyer(c).UserID, idParam(c)))
}

func (h *Handler) confirmOrder(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.ConfirmReceipt(buyer(c).UserID, idParam(c)))
}

// BotNotifier sends order messages through the bot the order was placed from.
type BotNotifier struct{ Bots *business.Service }

func (n BotNotifier) Notify(ctx context.Context, botID uint64, telegramUserID int64, html string) error {
	sender, err := n.Bots.Sender(ctx, botID)
	if err != nil {
		return err
	}
	return sender.SendMessageWithParseMode(ctx, telegramUserID, html, "HTML", nil)
}
