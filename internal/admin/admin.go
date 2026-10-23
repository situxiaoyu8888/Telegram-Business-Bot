package admin

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/business"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/news"
	"telegram-business-bot/internal/paypass"
	"telegram-business-bot/internal/push"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/support"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/wallet"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"telegram-business-bot/internal/ratelimit"
)

type Handler struct {
	sessionBackend sessionStore
	accountBackend accountStore
	limiter        ratelimit.Store // login throttling; defaults to the store's Redis
	logger         *zap.Logger
	publicURL      string                          // console address sent to approved merchants
	auditSink      func(model.AdminAuditLog) error // where audit records go; defaults to the database

	// Split listeners: pages call the API at apiBase, uploads go to uploadURL, and the API tells
	// the operator and merchant consoles apart by request origin (portals: origin → portal).
	split     bool
	apiBase   string
	uploadURL string
	portals   map[string]string
	bots      *business.Service
	st        *store.Store
	wallet    *wallet.Service
	news      *news.Service
	pay       *paypass.Service
	support   *support.Service
	tg        *telegram.Client
	push      *push.Service
	shop      *shop.Service

	uploadDir           string
	uploadPublicBaseURL string
	uploadMaxImageMB    int64
	uploadMaxVideoMB    int64
}

func New(
	st *store.Store,
	w *wallet.Service,
	s *support.Service,
	tg *telegram.Client,
	pushSvc *push.Service,
	uploadDir string,
	uploadPublicBaseURL string,
	uploadMaxImageMB int64,
	uploadMaxVideoMB int64,
) *Handler {
	return &Handler{
		st:                  st,
		wallet:              w,
		support:             s,
		tg:                  tg,
		push:                pushSvc,
		uploadDir:           uploadDir,
		uploadPublicBaseURL: uploadPublicBaseURL,
		uploadMaxImageMB:    uploadMaxImageMB,
		uploadMaxVideoMB:    uploadMaxVideoMB,
	}
}

// WithPublicURL sets the console address included in merchant approval messages.
func (h *Handler) WithPublicURL(u string) *Handler { h.publicURL = u; return h }

// WithLogger enables the access log of console actions.
func (h *Handler) WithLogger(l *zap.Logger) *Handler { h.logger = l; return h }

// Console portals in split mode.
const (
	portalOperator = "operator"
	portalMerchant = "merchant"
)

// WithSplit configures separate listeners: the console pages call the API at apiBase, upload to
// mainBase, and sign-ins are bound to the console (operator or merchant) they came from.
func (h *Handler) WithSplit(apiBase, mainBase, operatorOrigin, merchantOrigin string) *Handler {
	h.split, h.apiBase, h.uploadURL = true, apiBase, mainBase+"/admin/api/upload"
	h.portals = map[string]string{operatorOrigin: portalOperator, merchantOrigin: portalMerchant}
	return h
}

// Register serves the whole console (pages, API and uploads) on one listener.
func (h *Handler) Register(r *gin.Engine) {
	h.RegisterConsole(r)
	h.RegisterAPI(r)
	h.RegisterUpload(r)
}

// RegisterConsole serves the console pages. With split listeners the page holds no data and
// signs in through the API (whose cookie this listener may not see), so it needs no session.
// Console names shown on the login page in split mode.
const (
	ConsoleOperator = "运营后台"
	ConsoleMerchant = "商户后台"
)

func (h *Handler) RegisterConsole(r *gin.Engine, name ...string) {
	title := ""
	if len(name) > 0 {
		title = name[0]
	}
	portal := map[string]string{ConsoleOperator: portalOperator, ConsoleMerchant: portalMerchant}[title]
	page := func(c *gin.Context) { h.page(c, portal) }
	r.GET("/admin/login", func(c *gin.Context) { h.loginPage(c, title) })
	if h.split {
		r.GET("/admin/", page)
	} else {
		r.GET("/admin/", h.requireSession, page)
	}
}

// RegisterUpload serves file uploads, authorised by a one-time token from POST /admin/api/upload-token.
func (h *Handler) RegisterUpload(r *gin.Engine) {
	r.POST("/admin/api/upload", h.uploadAuth, h.accessLog, h.uploadMedia)
}

// RegisterAPI serves sign-in and every console API except uploads.
func (h *Handler) RegisterAPI(r *gin.Engine) {
	r.POST("/admin/login", h.login)
	r.POST("/admin/logout", h.logout)
	g := r.Group("/admin", h.requireSession, h.accessLog)
	g.GET("/api/session", h.session)

	api := g.Group("/api")
	// Each route names the pages that may use it; super administrators pass every check.
	can := requirePermission

	api.GET("/dashboard", h.dashboard)
	api.GET("/overview", can("home"), platformOnly, h.overview)
	api.GET("/users", can("users"), h.users)

	api.GET("/withdrawals", can("withdrawals"), h.withdrawals)
	api.POST("/withdrawals/:id/approve", can("withdrawals"), h.approve)
	api.POST("/withdrawals/:id/reject", can("withdrawals"), h.reject)
	api.POST("/withdrawals/:id/complete", can("withdrawals"), h.complete)

	api.GET("/deposits", can("deposits"), h.deposits)
	// Crediting money needs the funds permission on top of seeing the page.
	api.POST("/deposits/:id/assign", can("deposits"), can("funds"), h.assignDeposit)
	api.GET("/deposits/:id/review", can("deposits"), h.depositReview)
	api.POST("/deposits/:id/confirm", can("deposits"), can("funds"), h.confirmDeposit)
	api.POST("/deposits/:id/reject", can("deposits"), can("funds"), h.rejectDeposit)

	// Currency names label balances, withdrawals and exchanges.
	api.GET("/currencies", can("currencies", "users", "withdrawals", "exchanges", "redpackets", "deposits"), h.currencies)
	api.POST("/currencies", can("currencies"), h.saveCurrency)
	api.GET("/exchanges", can("exchanges"), h.exchanges)
	api.GET("/redpackets", can("redpackets"), h.redPackets)
	api.GET("/redpackets/:id/claims", can("redpackets"), h.redPacketClaims)
	api.GET("/redpacket-covers", can("redpacket_covers"), h.redPacketCovers)
	api.POST("/redpacket-covers", can("redpacket_covers"), h.saveRedPacketCover)
	api.DELETE("/redpacket-covers/:id", can("redpacket_covers"), h.deleteRedPacketCover)
	api.GET("/users/:id/balances", can("users"), h.userBalances)
	api.POST("/users/:id/adjust", can("users"), can("funds"), h.adjustBalance)
	api.POST("/users/:id/paypass", can("users"), h.resetPayPassword)

	api.GET("/addresses", can("addresses"), h.addresses)
	api.GET("/addresses/users", can("addresses", "deposits"), h.addressUsers)
	api.POST("/addresses", can("addresses"), h.saveAddress)
	api.GET("/addresses/bulk", can("addresses"), h.bulkAddressPreview)
	api.POST("/addresses/bulk", can("addresses"), h.bulkAssignAddresses)
	api.GET("/address-pool", can("addresses"), h.addressPool)
	api.POST("/address-pool", can("addresses"), h.addPoolAddresses)
	api.DELETE("/address-pool/:id", can("addresses"), h.deletePoolAddress)
	api.POST("/addresses/:id/rescan", can("addresses"), func(c *gin.Context) {
		id, ok := idParam(c)
		if !ok {
			return
		}
		var a model.DepositAddress
		if err := h.st.DB.First(&a, id).Error; err != nil {
			c.JSON(404, gin.H{"error": "address not found"})
			return
		}
		if a.Network != "TRC20" {
			c.JSON(400, gin.H{"error": "只有 TRC20 地址会自动扫描入账；ERC20 到账请在「用户管理」中手动加款"})
			return
		}
		if err := h.st.DB.Model(&a).Update("scan_timestamp", 0).Error; err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	api.GET("/tickets", can("tickets"), h.tickets)
	api.POST("/tickets/:id/reply", can("tickets"), h.replyTicket)

	// Product and coupon forms pick a merchant from this list.
	api.GET("/merchants", can("merchants", "shop_products", "shop_coupons"), h.merchants)
	api.POST("/merchants", can("merchants"), h.saveMerchant)
	api.DELETE("/merchants/:id", can("merchants"), h.deleteMerchant)
	// The merchant form picks its category from the menu list.
	api.GET("/menus", can("menus", "merchants", "shop_applications"), h.menus)
	api.POST("/menus", can("menus"), h.saveMenu)
	api.DELETE("/menus/:id", can("menus"), h.deleteMenu)

	api.GET("/configs", can("configs"), h.configs)
	api.POST("/configs", can("configs"), h.saveConfig)

	api.POST("/upload-token", can(uploadPages...), h.uploadToken)

	api.GET("/pushes", can("pushes"), h.pushes)
	api.POST("/pushes", can("pushes"), h.createPush)
	api.GET("/pushes/:id/recipients", can("pushes"), h.pushRecipients)
	api.POST("/pushes/:id/cancel", can("pushes"), h.cancelPush)
	api.POST("/pushes/:id/retry", can("pushes"), h.retryPush)
	api.POST("/pushes/:id/pause", can("pushes"), h.pausePush)
	api.POST("/pushes/:id/resume", can("pushes"), h.resumePush)
	api.DELETE("/pushes/:id", can("pushes"), h.deletePush)
	// Push and route pages show bot names and the route table.
	api.GET("/bots", can("bots", "pushes", "routes"), h.listBots)
	api.POST("/bots", can("bots"), h.saveBot)
	api.GET("/routes", can("routes", "pushes"), h.listRoutes)
	api.POST("/routes", can("routes"), h.saveRoute)
	api.DELETE("/routes/:id", can("routes"), h.deleteRoute)

	if h.shop != nil {
		h.registerShop(api, can)
	}
	if h.news != nil {
		h.registerNews(api, can)
	}

	api.POST("/account/password", h.changePassword)
	api.GET("/audit-logs", requireSuper, h.auditLogs)
	api.GET("/backup", requireSuper, h.downloadBackup)
	if h.shop != nil {
		api.POST("/shop-digest/send", requireSuper, h.sendDigest)
	}
	// A merchant account manages its own staff; staff and platform accounts are refused.
	api.GET("/merchant-staff", merchantOnly, h.listStaff)
	api.POST("/merchant-staff", merchantOnly, h.saveStaff)
	api.DELETE("/merchant-staff/:id", merchantOnly, h.deleteStaff)
	accounts := api.Group("/accounts", requireSuper)
	accounts.GET("", h.listAccounts)
	accounts.POST("", h.saveAccount)
	accounts.DELETE("/:id", h.deleteAccount)
	roles := api.Group("/roles", requireSuper)
	roles.GET("", h.listRoles)
	roles.POST("", h.saveRole)
	roles.DELETE("/:id", h.deleteRole)
}

func (h *Handler) page(c *gin.Context, portal string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	html := withAPIBase(adminHTML, h.apiBase)
	c.String(http.StatusOK, strings.Replace(html, `<meta name="portal" content="">`, `<meta name="portal" content="`+portal+`">`, 1))
}

// withAPIBase fills the page's api-base meta tag, which its scripts prefix to API paths.
func withAPIBase(html, base string) string {
	return strings.Replace(html, `<meta name="api-base" content="">`, `<meta name="api-base" content="`+base+`">`, 1)
}

func (h *Handler) dashboard(c *gin.Context) {
	counts := map[string]int64{}
	// Platform-wide figures are not shown to merchant accounts.
	if merchantScope(c) != 0 {
		c.JSON(http.StatusOK, counts)
		return
	}

	queries := []struct {
		name  string
		model any
		where string
	}{
		{"users", &model.TelegramUser{}, ""},
		{"pending_withdrawals", &model.WithdrawalOrder{}, "status='pending'"},
		{"open_tickets", &model.SupportTicket{}, "status='open'"},
		{"deposits", &model.DepositOrder{}, ""},
		{"active_pushes", &model.PushCampaign{}, "status IN ('queued','scheduled','processing')"},
	}

	for _, item := range queries {
		var n int64
		q := h.st.DB.Model(item.model)
		if item.where != "" {
			q = q.Where(item.where)
		}
		_ = q.Count(&n).Error
		counts[item.name] = n
	}

	c.JSON(http.StatusOK, counts)
}

func (h *Handler) users(c *gin.Context) {
	var rows []model.TelegramUser
	h.st.DB.Order("id desc").Limit(200).Find(&rows)
	c.JSON(http.StatusOK, rows)
}

// walletUser is who a withdrawal or deposit belongs to, as the console lists it.
type walletUser struct {
	TelegramUserID int64  `json:"telegram_user_id"`
	Username       string `json:"username"`
	UserName       string `json:"user_name"`
}

// walletUsers looks up the Telegram identity of the given users.
func (h *Handler) walletUsers(ids []uint64) map[uint64]walletUser {
	out := map[uint64]walletUser{}
	if len(ids) == 0 {
		return out
	}
	var users []model.TelegramUser
	h.st.DB.Select("id", "telegram_user_id", "username", "first_name", "last_name").Where("id IN ?", ids).Find(&users)
	for _, u := range users {
		out[u.ID] = walletUser{TelegramUserID: u.TelegramUserID, Username: u.Username, UserName: strings.TrimSpace(u.FirstName + " " + u.LastName)}
	}
	return out
}

type withdrawalRow struct {
	model.WithdrawalOrder
	walletUser
	OrderNo string `json:"order_no"`
}

func (h *Handler) withdrawals(c *gin.Context) {
	var orders []model.WithdrawalOrder
	h.st.DB.Order("id desc").Limit(200).Find(&orders)
	ids := make([]uint64, 0, len(orders))
	for _, o := range orders {
		ids = append(ids, o.UserID)
	}
	users := h.walletUsers(ids)
	rows := make([]withdrawalRow, 0, len(orders))
	for _, o := range orders {
		rows = append(rows, withdrawalRow{WithdrawalOrder: o, walletUser: users[o.UserID], OrderNo: wallet.OrderNo("W", o.ID, o.CreatedAt)})
	}
	c.JSON(http.StatusOK, rows)
}

// depositRow is one line of the deposit page: an on-chain deposit, or a balance adjustment made
// in the console (credits and debits, any currency).
type depositRow struct {
	ID     uint64 `json:"id"`   // adjustments are offset by adjustmentRowBase so IDs never collide
	Kind   string `json:"kind"` // chain, admin_credit or admin_debit
	UserID uint64 `json:"user_id"`
	walletUser
	OrderNo    string    `json:"order_no"`
	Currency   string    `json:"currency"`
	Network    string    `json:"network"`
	Address    string    `json:"address"`
	From       string    `json:"from_address"`
	Amount     string    `json:"amount"`
	Status     string    `json:"status"`
	TxID       string    `json:"txid"`
	Remark     string    `json:"remark"`
	ReviewedBy string    `json:"reviewed_by"`
	CreatedAt  time.Time `json:"created_at"`
}

const adjustmentRowBase = 1_000_000_000_000

func (h *Handler) deposits(c *gin.Context) {
	var orders []model.DepositOrder
	if err := h.st.DB.Order("id desc").Limit(300).Find(&orders).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "充值记录加载失败"})
		return
	}
	var adjustments []model.WalletTransaction
	if err := h.st.DB.Where("type IN ?", []string{"admin_credit", "admin_debit"}).Order("id desc").Limit(300).Find(&adjustments).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "充值记录加载失败"})
		return
	}
	rows := make([]depositRow, 0, len(orders)+len(adjustments))
	userIDs := []uint64{}
	for _, o := range orders {
		rows = append(rows, depositRow{ID: o.ID, Kind: "chain", UserID: o.UserID, OrderNo: wallet.OrderNo("D", o.ID, o.CreatedAt), Currency: o.Currency, Network: o.Network, Address: o.Address, From: o.FromAddress, Amount: o.Amount, Status: o.Status, TxID: o.TxID, ReviewedBy: o.ReviewedBy, CreatedAt: o.CreatedAt})
		userIDs = append(userIDs, o.UserID)
	}
	for _, t := range adjustments {
		rows = append(rows, depositRow{ID: adjustmentRowBase + t.ID, Kind: t.Type, UserID: t.UserID, OrderNo: wallet.OrderNo("A", t.ID, t.CreatedAt), Currency: t.Currency, Amount: t.Amount, Status: "completed", Remark: t.Remark, CreatedAt: t.CreatedAt})
		userIDs = append(userIDs, t.UserID)
	}
	users := h.walletUsers(userIDs)
	for i := range rows {
		rows[i].walletUser = users[rows[i].UserID]
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].CreatedAt.After(rows[j].CreatedAt) })
	if len(rows) > 300 {
		rows = rows[:300]
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) addresses(c *gin.Context) {
	var rows []model.DepositAddress
	h.st.DB.Order("id desc").Limit(500).Find(&rows)
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) tickets(c *gin.Context) {
	var rows []model.SupportTicket
	h.st.DB.Order("id desc").Limit(200).Find(&rows)
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) menus(c *gin.Context) {
	var rows []model.MenuItem
	h.st.DB.Order("parent_id,sort,id").Find(&rows)
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) configs(c *gin.Context) {
	var rows []model.SystemConfig
	h.st.DB.Order("`key`").Find(&rows)
	c.JSON(http.StatusOK, rows)
}

func idParam(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad id"})
		return 0, false
	}
	return id, true
}

func (h *Handler) approve(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	if err := h.wallet.ApproveWithdrawal(id, operator(c), b.Note); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.notifyWithdrawal(id, "approved", b.Note)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) reject(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	if err := h.wallet.RejectWithdrawal(id, operator(c), b.Note); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.notifyWithdrawal(id, "rejected", b.Note)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) complete(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		TxID string `json:"txid"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || strings.TrimSpace(b.TxID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "txid required"})
		return
	}
	if err := h.wallet.CompleteWithdrawal(id, b.TxID, operator(c)); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	h.notifyWithdrawal(id, "completed", strings.TrimSpace(b.TxID))
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) replyTicket(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Reply string `json:"reply"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || strings.TrimSpace(b.Reply) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reply required"})
		return
	}
	if err := h.support.Reply(c.Request.Context(), id, operator(c), b.Reply, func(ctx context.Context, tgID int64, reply string) error {
		var source model.SupportTicket
		if err := h.st.DB.Select("bot_id").First(&source, id).Error; err != nil {
			return err
		}
		var sender telegram.Sender = h.tg
		if h.bots != nil {
			var err error
			sender, err = h.bots.Sender(ctx, source.BotID)
			if err != nil {
				return err
			}
		} else if source.BotID != 0 || h.tg == nil {
			return errors.New("Telegram client unavailable")
		}
		return sender.SendMessageWithParseMode(ctx, tgID, "<b>🎧 客服回复</b>\n\n"+htmlEscape(reply)+"\n\n工单：<code>#"+strconv.FormatUint(id, 10)+"</code>", "HTML", nil)
	}); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func htmlEscape(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
	).Replace(s)
}

// addressUsers lists users for the address form's searchable picker.
func (h *Handler) addressUsers(c *gin.Context) {
	type row struct {
		ID             uint64 `json:"id"`
		TelegramUserID int64  `json:"telegram_user_id"`
		Username       string `json:"username"`
		FirstName      string `json:"first_name"`
		LastName       string `json:"last_name"`
	}
	rows := []row{}
	if err := h.st.DB.Model(&model.TelegramUser{}).Select("id, telegram_user_id, username, first_name, last_name").Order("id desc").Limit(5000).Scan(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "用户加载失败"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) saveMenu(c *gin.Context) {
	var b model.MenuItem
	if err := c.ShouldBindJSON(&b); err != nil || b.Code == "" || b.Title == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code/title required"})
		return
	}

	b.ImageURL = strings.TrimSpace(b.ImageURL)
	b.ButtonText = strings.TrimSpace(b.ButtonText)
	if b.ActionType == "photo" {
		imageURL, imageErr := url.Parse(b.ImageURL)
		buttonURL, buttonErr := url.Parse(b.ActionValue)
		if imageErr != nil || (imageURL.Scheme != "https" && imageURL.Scheme != "http") || imageURL.Hostname() == "" || imageURL.User != nil || len(b.ImageURL) > 1024 ||
			buttonErr != nil || buttonURL.Scheme != "https" || buttonURL.Host == "" || len(b.ActionValue) > 255 ||
			b.ButtonText == "" || utf8.RuneCountInString(b.ButtonText) > 64 || utf8.RuneCountInString(b.Description) > 1000 {
			c.JSON(400, gin.H{"error": "图片菜单需填写 HTTP/HTTPS 图片地址、HTTPS 按钮链接及 1–64 字按钮文字，说明最多 1000 字"})
			return
		}
	}
	b.Title = strings.TrimSpace(b.Title)
	if b.Placement == "" {
		b.Placement = "inline"
	}
	if b.Placement != "inline" && b.Placement != "reply" {
		c.JSON(400, gin.H{"error": "无效的菜单展示位置"})
		return
	}
	if b.Title == "" || utf8.RuneCountInString(b.Title) > 64 || strings.ContainsAny(b.Title, "\r\n") {
		c.JSON(400, gin.H{"error": "菜单标题须为 1–64 字且不能换行"})
		return
	}
	b.ReplyTitle = nil
	if b.Placement == "reply" {
		if strings.HasPrefix(b.Title, "/") || b.RowNo < 0 || b.Title == telegram.ReplyHomeText {
			c.JSON(400, gin.H{"error": "底部菜单标题不能以 / 开头或使用系统返回按钮名称，行号不能为负数"})
			return
		}
		b.ReplyTitle = &b.Title
	}
	if b.ActionType == "text" && utf8.RuneCountInString(b.Description) > 4000 {
		c.JSON(400, gin.H{"error": "回复内容最多 4000 字"})
		return
	}
	if b.ActionType == "" {
		b.ActionType = "builtin"
	}
	if b.Role == "" {
		b.Role = "user"
	}

	err := h.st.DB.Transaction(func(tx *gorm.DB) error {
		var old model.MenuItem
		var lookup error
		if b.ID > 0 {
			lookup = tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&old, b.ID).Error
			if lookup != nil {
				return lookup
			}
		} else {
			lookup = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("code=?", b.Code).First(&old).Error
		}
		if lookup == nil {
			b.ID = old.ID
			b.CreatedAt = old.CreatedAt
		} else if !errors.Is(lookup, gorm.ErrRecordNotFound) {
			return lookup
		}
		if len("menu:"+b.Code) > 64 {
			return errors.New("menu code exceeds Telegram callback limit")
		}
		if b.Role != "user" && b.Role != "admin" {
			return errors.New("invalid menu role")
		}
		if b.ActionType != "builtin" && b.ActionType != "url" && b.ActionType != "submenu" && b.ActionType != "text" && b.ActionType != "merchants" && b.ActionType != "photo" {
			return errors.New("invalid action type")
		}

		if b.ActionType == "merchants" && b.ParentID == 0 {
			return errors.New("商家列表必须关联父菜单")
		}
		if lookup == nil && old.ActionType == "merchants" && b.ActionType != "merchants" && b.ActionType != "photo" {
			var count int64
			if err := tx.Model(&model.Merchant{}).Where("menu_id=?", b.ID).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return errors.New("请先迁移或删除关联商家，再更改菜单类型")
			}
		}

		if lookup == nil && old.ActionType == "submenu" && b.ActionType != "submenu" {
			var count int64
			if err := tx.Model(&model.MenuItem{}).Where("parent_id=?", b.ID).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return errors.New("请先迁移或删除子菜单，再更改父菜单类型")
			}
		}
		if b.ActionType == "url" && !telegram.IsTelegramURL(b.ActionValue) {
			return errors.New("valid https URL required")
		}
		visited := map[uint64]bool{}
		for parent := b.ParentID; parent != 0; {
			if parent == b.ID || visited[parent] {
				return errors.New("menu parent cycle")
			}
			visited[parent] = true
			var p model.MenuItem
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&p, parent).Error; err != nil {
				return errors.New("parent menu not found")
			}
			if p.ActionType != "submenu" {
				return errors.New("parent must be a submenu")
			}
			parent = p.ParentID
		}
		if lookup == nil {
			return tx.Save(&b).Error
		}
		b.ID = 0
		return tx.Create(&b).Error
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, b)
}

func (h *Handler) deleteMenu(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	err := h.st.DB.Transaction(func(tx *gorm.DB) error {
		var item model.MenuItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&item, id).Error; err != nil {
			return err
		}
		if item.ActionType == "merchants" {
			var count int64
			if err := tx.Model(&model.Merchant{}).Where("menu_id=?", id).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				return errors.New("请先迁移或删除该菜单关联的商家")
			}
		}

		var children int64
		if err := tx.Model(&model.MenuItem{}).Where("parent_id=?", id).Count(&children).Error; err != nil {
			return err
		}
		if children > 0 {
			return errors.New("remove child menus first")
		}
		res := tx.Delete(&model.MenuItem{}, id)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) saveConfig(c *gin.Context) {
	var b model.SystemConfig
	if err := c.ShouldBindJSON(&b); err != nil || b.Key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "key required"})
		return
	}

	var old model.SystemConfig
	err := h.st.DB.Where("`key`=?", b.Key).First(&old).Error
	if err == nil {
		old.Value = b.Value
		old.Remark = b.Remark
		err = h.st.DB.Save(&old).Error
	} else {
		err = h.st.DB.Create(&b).Error
	}
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) pushes(c *gin.Context) {
	rows, err := h.push.List(200)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) createPush(c *gin.Context) {
	var b struct {
		RepeatSeconds int64   `json:"repeat_seconds"`
		BotID         *uint64 `json:"bot_id"`
		Category      string  `json:"category"`
		Personalize   *bool   `json:"personalize"`
		Title         string  `json:"title"`
		MessageType   string  `json:"message_type"`
		Content       string  `json:"content"`
		ParseMode     string  `json:"parse_mode"`
		MediaURL      string  `json:"media_url"`
		MediaFileID   string  `json:"media_file_id"`
		ContentMode   string  `json:"content_mode"`
		ButtonText    string  `json:"button_text"`
		ButtonURL     string  `json:"button_url"`
		TargetType    string  `json:"target_type"`
		TargetValue   string  `json:"target_value"`
		ScheduledAt   string  `json:"scheduled_at"`
	}

	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request"})
		return
	}

	var scheduled *time.Time
	if strings.TrimSpace(b.ScheduledAt) != "" {
		t, err := time.Parse(time.RFC3339, b.ScheduledAt)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "scheduled_at must be RFC3339"})
			return
		}
		scheduled = &t
	}

	row, err := h.push.Create(
		push.CreateRequest{
			RepeatSeconds: b.RepeatSeconds,
			BotID:         b.BotID, Category: b.Category, Personalize: b.Personalize,
			Title:       b.Title,
			MessageType: b.MessageType,
			Content:     b.Content,
			ParseMode:   b.ParseMode,
			MediaURL:    b.MediaURL,
			MediaFileID: b.MediaFileID,
			ContentMode: b.ContentMode,
			ButtonText:  b.ButtonText,
			ButtonURL:   b.ButtonURL,
			TargetType:  b.TargetType,
			TargetValue: b.TargetValue,
			ScheduledAt: scheduled,
		},
		operator(c),
	)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, row)
}

func (h *Handler) pushRecipients(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	rows, err := h.push.Recipients(id, 500)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) cancelPush(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.push.Cancel(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handler) retryPush(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if err := h.push.RetryFailed(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

//go:embed index.html
var adminHTML string

func (h *Handler) WithBots(b *business.Service) *Handler { h.bots = b; return h }
func (h *Handler) pausePush(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.push.Pause(id))
}
func (h *Handler) resumePush(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.push.Resume(id))
}
func (h *Handler) deletePush(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.push.Delete(c.Request.Context(), id))
}
func respond(c *gin.Context, err error) {
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
func (h *Handler) listBots(c *gin.Context) {
	rows, err := h.bots.List()
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, rows)
}
func (h *Handler) saveBot(c *gin.Context) {
	var b struct {
		ID      uint64 `json:"id"`
		Name    string `json:"name"`
		Token   string `json:"token"`
		Enabled bool   `json:"enabled"`
		Purpose string `json:"purpose"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	row, err := h.bots.Save(c.Request.Context(), b.ID, b.Name, b.Token, b.Enabled, b.Purpose)
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, row)
}
func (h *Handler) listRoutes(c *gin.Context) {
	rows, err := h.bots.Routes()
	if err != nil {
		respond(c, err)
		return
	}
	c.JSON(200, rows)
}
func (h *Handler) saveRoute(c *gin.Context) {
	var b model.PushRoute
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	respond(c, h.bots.SaveRoute(c.Request.Context(), b.Category, b.BotID))
}
func (h *Handler) deleteRoute(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.bots.DeleteRoute(id))
}
