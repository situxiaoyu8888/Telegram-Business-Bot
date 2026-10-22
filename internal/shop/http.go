package shop

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"telegram-business-bot/internal/business"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/ratelimit"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/wallet"
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

	botUsername func(ctx context.Context, botID uint64) string
	apiBase     string
}

// WithBotUsernames lets the shop build share links that open the bot the buyer is using.
func (h *Handler) WithBotUsernames(fn func(ctx context.Context, botID uint64) string) *Handler {
	h.botUsername = fn
	return h
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

// WithAPIBase makes the shop page call the API at base (a separate API listener).
func (h *Handler) WithAPIBase(base string) *Handler { h.apiBase = base; return h }

// Register serves the shop page and its API on one listener.
func (h *Handler) Register(r *gin.Engine) {
	h.RegisterPage(r)
	h.RegisterAPI(r)
}

// RegisterPage serves the H5 shop page.
func (h *Handler) RegisterPage(r *gin.Engine) {
	page := []byte(strings.Replace(string(indexHTML), `<meta name="api-base" content="">`, `<meta name="api-base" content="`+h.apiBase+`">`, 1))
	r.GET("/shop", func(c *gin.Context) { c.Redirect(http.StatusMovedPermanently, "/shop/") })
	r.GET("/shop/", func(c *gin.Context) {
		c.Header("Cache-Control", "no-cache")
		c.Data(http.StatusOK, "text/html; charset=utf-8", page)
	})
}

// RegisterAPI serves the shop API.
func (h *Handler) RegisterAPI(r *gin.Engine) {
	api := r.Group("/shop/api", h.authenticate, h.throttle("rl:shop:", maxRequestsPerMinute))
	api.GET("/me", h.me)
	api.GET("/home", h.home)
	api.GET("/categories", h.categories)
	api.GET("/directory", h.directory)
	api.GET("/merchants/:id", h.merchant)
	api.GET("/products", h.products)
	api.GET("/products/:id", h.product)
	api.GET("/products/:id/reviews", h.productReviews)
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
	api.POST("/orders", h.throttle("rl:shop-order:", maxOrdersPerMinute), h.createOrder)
	api.GET("/orders/:id", h.order)
	api.POST("/orders/:id/cancel", h.cancelOrder)
	api.POST("/orders/:id/confirm", h.confirmOrder)
	api.POST("/orders/:id/review", h.reviewOrder)
	api.GET("/member", h.member)
	api.GET("/member/points", h.pointsLog)
	api.GET("/groups/:id", h.group)
	api.GET("/products/:id/groups", h.productGroups)
	api.GET("/merchant-applications/mine", h.myApplication)
	api.POST("/merchant-applications", h.applyMerchant)
	api.GET("/conversations", h.conversations)
	api.GET("/conversations/merchant/:id", h.thread)
	api.POST("/conversations/merchant/:id", h.sendMessage)
	api.GET("/favorites", h.favorites)
	api.POST("/favorites/:id", h.addFavorite)
	api.DELETE("/favorites/:id", h.removeFavorite)
	api.GET("/history", h.history)
	api.DELETE("/history", h.clearHistory)
	api.GET("/exchange", h.exchangeInfo)
	api.POST("/exchange", h.throttle("rl:shop-order:", maxOrdersPerMinute), h.exchange)
	api.GET("/aftersales", h.myAfterSales)
	api.POST("/aftersales", h.applyAfterSale)
	api.POST("/aftersales/:id/cancel", h.cancelAfterSale)
	api.POST("/aftersales/:id/return", h.submitReturn)
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

const (
	maxRequestsPerMinute = 180
	maxOrdersPerMinute   = 20
)

// throttle limits each buyer to limit requests a minute under prefix. Redis errors let the
// request through rather than block shopping.
func (h *Handler) throttle(prefix string, limit int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if h.st == nil || h.st.Redis == nil {
			c.Next()
			return
		}
		n, err := ratelimit.Hit(c.Request.Context(), h.st.Redis, fmt.Sprintf("%s%d", prefix, buyer(c).TelegramUserID), time.Minute)
		if err == nil && n > limit {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "操作太频繁，请稍后再试"})
			return
		}
		c.Next()
	}
}

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
	isMerchant, err := h.svc.IsMerchant(b)
	if err != nil {
		h.fail(c, err)
		return
	}
	res := gin.H{"name": name, "username": u.Username, "telegram_user_id": u.TelegramUserID, "balance": available.StringFixed(6), "referral_code": u.ReferralCode, "is_merchant": isMerchant}
	if h.botUsername != nil {
		res["bot_username"] = h.botUsername(c.Request.Context(), buyer(c).BotID)
	}
	c.JSON(http.StatusOK, res)
}

func (h *Handler) home(c *gin.Context) {
	home, err := h.svc.Home()
	h.reply(c, home, err)
}

func (h *Handler) productReviews(c *gin.Context) {
	page, _ := strconv.Atoi(c.Query("page"))
	rows, err := h.svc.ProductReviews(idParam(c), page)
	h.reply(c, rows, err)
}

func (h *Handler) reviewOrder(c *gin.Context) {
	var b struct {
		Reviews []ReviewInput `json:"reviews"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	h.reply(c, gin.H{"ok": true}, h.svc.Review(buyer(c).UserID, idParam(c), b.Reviews))
}

func (h *Handler) directory(c *gin.Context) {
	rows, err := h.svc.Directory()
	h.reply(c, rows, err)
}

func (h *Handler) merchant(c *gin.Context) {
	m, err := h.svc.Merchant(idParam(c))
	h.reply(c, m, err)
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
	b := buyer(c)
	p, err := h.svc.Product(idParam(c))
	if err == nil {
		p.Favorited = h.svc.IsFavorite(b.UserID, p.ID)
		if e := h.svc.RecordView(b.UserID, p.ID); e != nil && h.logger != nil {
			h.logger.Warn("record product view", zap.Error(e))
		}
	}
	h.reply(c, p, err)
}

func (h *Handler) member(c *gin.Context) {
	m, err := h.svc.Member(buyer(c).UserID)
	h.reply(c, m, err)
}

func (h *Handler) pointsLog(c *gin.Context) {
	rows, err := h.svc.PointsLog(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) group(c *gin.Context) {
	g, err := h.svc.Group(buyer(c).UserID, idParam(c))
	h.reply(c, g, err)
}

func (h *Handler) productGroups(c *gin.Context) {
	rows, err := h.svc.OpenGroups(buyer(c).UserID, idParam(c))
	h.reply(c, rows, err)
}

func (h *Handler) myApplication(c *gin.Context) {
	a, err := h.svc.MyApplication(buyer(c).UserID)
	h.reply(c, gin.H{"application": a}, err)
}

func (h *Handler) applyMerchant(c *gin.Context) {
	var in ApplicationInput
	if err := c.ShouldBindJSON(&in); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	a, err := h.svc.ApplyMerchant(buyer(c), in)
	h.reply(c, a, err)
}

func (h *Handler) conversations(c *gin.Context) {
	rows, err := h.svc.BuyerConversations(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) thread(c *gin.Context) {
	t, err := h.svc.BuyerThread(buyer(c).UserID, idParam(c))
	h.reply(c, t, err)
}

func (h *Handler) sendMessage(c *gin.Context) {
	var in MessageInput
	if err := c.ShouldBindJSON(&in); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	b := buyer(c)
	var u model.TelegramUser
	name := ""
	if h.st.DB.Select("first_name", "last_name", "username").First(&u, b.UserID).Error == nil {
		name = strings.TrimSpace(u.FirstName + " " + u.LastName)
		if u.Username != "" {
			name = strings.TrimSpace(name + " @" + u.Username)
		}
	}
	m, err := h.svc.SendToMerchant(b, name, idParam(c), in)
	h.reply(c, m, err)
}

func (h *Handler) favorites(c *gin.Context) {
	rows, err := h.svc.Favorites(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) addFavorite(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.SetFavorite(buyer(c).UserID, idParam(c), true))
}

func (h *Handler) removeFavorite(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.SetFavorite(buyer(c).UserID, idParam(c), false))
}

func (h *Handler) history(c *gin.Context) {
	rows, err := h.svc.History(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) clearHistory(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.ClearHistory(buyer(c).UserID))
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
	orders, err := h.svc.CreateOrders(buyer(c), req)
	if err != nil {
		h.fail(c, err)
		return
	}
	// Single-group requests keep returning the order itself.
	if len(req.Groups) == 0 {
		c.JSON(http.StatusOK, orders[0])
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
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

func (h *Handler) myAfterSales(c *gin.Context) {
	rows, err := h.svc.MyAfterSales(buyer(c).UserID)
	h.reply(c, rows, err)
}

func (h *Handler) applyAfterSale(c *gin.Context) {
	var req AfterSaleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	a, err := h.svc.ApplyAfterSale(buyer(c).UserID, req)
	h.reply(c, a, err)
}

func (h *Handler) cancelAfterSale(c *gin.Context) {
	h.reply(c, gin.H{"ok": true}, h.svc.CancelAfterSale(buyer(c).UserID, idParam(c)))
}

func (h *Handler) submitReturn(c *gin.Context) {
	var b struct {
		Carrier    string `json:"carrier"`
		TrackingNo string `json:"tracking_no"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	h.reply(c, gin.H{"ok": true}, h.svc.SubmitReturn(buyer(c).UserID, idParam(c), b.Carrier, b.TrackingNo))
}

// BotNotifier sends buyer messages through the bot the order was placed from, staff messages
// through Staff (the admin bot when configured, otherwise the primary bot) and merchant messages
// through the primary bot, which merchants use (falling back to Staff).
type BotNotifier struct {
	Bots    *business.Service
	Staff   telegram.Sender
	ShopURL string // H5 shop address; enables the "view order" button when set
}

func (n BotNotifier) Notify(ctx context.Context, botID uint64, telegramUserID int64, html string, link *Link) error {
	sender, err := n.Bots.Sender(ctx, botID)
	if err != nil {
		return err
	}
	var kb *telegram.InlineKeyboardMarkup
	if n.ShopURL != "" && link != nil {
		kb = &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{telegram.ShopButton(link.Text, n.ShopURL, link.Fragment)}}}
	}
	return sender.SendMessageWithParseMode(ctx, telegramUserID, html, "HTML", kb)
}

// NotifyStaff tries every chat and reports the ones that failed.
func (n BotNotifier) NotifyStaff(ctx context.Context, chatIDs []int64, html string) error {
	var errs []error
	for _, id := range chatIDs {
		if err := n.Staff.SendMessageWithParseMode(ctx, id, html, "HTML", nil); err != nil {
			errs = append(errs, fmt.Errorf("chat %d: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

func (n BotNotifier) NotifyMerchant(ctx context.Context, chatID int64, html string) error {
	sender, err := n.Bots.Sender(ctx, 0)
	if err == nil {
		if err = sender.SendMessageWithParseMode(ctx, chatID, html, "HTML", nil); err == nil {
			return nil
		}
	}
	if n.Staff == nil {
		return fmt.Errorf("chat %d: %w", chatID, err)
	}
	if staffErr := n.Staff.SendMessageWithParseMode(ctx, chatID, html, "HTML", nil); staffErr != nil {
		return fmt.Errorf("chat %d: %w (admin bot: %v)", chatID, err, staffErr)
	}
	return nil
}

// exchangeInfo returns the buyer's balances, the rates and fee, and their latest exchanges.
func (h *Handler) exchangeInfo(c *gin.Context) {
	b := buyer(c)
	balances, err := h.svc.wallet.Balances(b.UserID)
	if err != nil {
		h.fail(c, err)
		return
	}
	fee, err := h.svc.wallet.ExchangeFeeRate()
	if err != nil {
		h.fail(c, err)
		return
	}
	records, err := h.svc.wallet.Exchanges(b.UserID, 20)
	h.reply(c, gin.H{"balances": balances, "fee_rate": fee.String(), "records": records}, err)
}

func (h *Handler) exchange(c *gin.Context) {
	var in wallet.ExchangeRequest
	if err := c.ShouldBindJSON(&in); err != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	rec, err := h.svc.wallet.Exchange(buyer(c).UserID, in)
	h.reply(c, rec, err)
}
