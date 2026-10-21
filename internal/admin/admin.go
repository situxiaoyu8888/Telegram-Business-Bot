package admin

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/business"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/push"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/support"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/wallet"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	sessionBackend sessionStore
	accountBackend accountStore
	bots           *business.Service
	st             *store.Store
	wallet         *wallet.Service
	support        *support.Service
	tg             *telegram.Client
	push           *push.Service
	shop           *shop.Service

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

func (h *Handler) Register(r *gin.Engine) {
	r.GET("/admin/login", h.loginPage)
	r.POST("/admin/login", h.login)
	r.POST("/admin/logout", h.logout)
	g := r.Group("/admin", h.requireSession)
	g.GET("/api/session", h.session)

	g.GET("/", h.page)

	api := g.Group("/api")
	// Each route names the pages that may use it; super administrators pass every check.
	can := requirePermission

	api.GET("/dashboard", h.dashboard)
	api.GET("/users", can("users"), h.users)

	api.GET("/withdrawals", can("withdrawals"), h.withdrawals)
	api.POST("/withdrawals/:id/approve", can("withdrawals"), h.approve)
	api.POST("/withdrawals/:id/reject", can("withdrawals"), h.reject)
	api.POST("/withdrawals/:id/complete", can("withdrawals"), h.complete)

	api.GET("/deposits", can("deposits"), h.deposits)

	api.GET("/addresses", can("addresses"), h.addresses)
	api.POST("/addresses", can("addresses"), h.saveAddress)
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
	api.GET("/menus", can("menus", "merchants"), h.menus)
	api.POST("/menus", can("menus"), h.saveMenu)
	api.DELETE("/menus/:id", can("menus"), h.deleteMenu)

	api.GET("/configs", can("configs"), h.configs)
	api.POST("/configs", can("configs"), h.saveConfig)

	api.POST("/upload", can("pushes", "menus", "shop_products"), h.uploadMedia)

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

	api.POST("/account/password", h.changePassword)
	accounts := api.Group("/accounts", requireSuper)
	accounts.GET("", h.listAccounts)
	accounts.POST("", h.saveAccount)
	accounts.DELETE("/:id", h.deleteAccount)
	roles := api.Group("/roles", requireSuper)
	roles.GET("", h.listRoles)
	roles.POST("", h.saveRole)
	roles.DELETE("/:id", h.deleteRole)
}

func (h *Handler) page(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, adminHTML)
}

func (h *Handler) dashboard(c *gin.Context) {
	counts := map[string]int64{}

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

func (h *Handler) withdrawals(c *gin.Context) {
	var rows []model.WithdrawalOrder
	h.st.DB.Order("id desc").Limit(200).Find(&rows)
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) deposits(c *gin.Context) {
	var rows []model.DepositOrder
	h.st.DB.Order("id desc").Limit(200).Find(&rows)
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

func (h *Handler) saveAddress(c *gin.Context) {
	var b model.DepositAddress
	if err := c.ShouldBindJSON(&b); err != nil || b.UserID == 0 || strings.TrimSpace(b.Address) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "user_id/address required"})
		return
	}
	b.Address = strings.TrimSpace(b.Address)
	if b.Network == "" {
		b.Network = "TRC20"
	}
	if b.Network != "TRC20" || !wallet.ValidTRONAddress(b.Address) {
		c.JSON(400, gin.H{"error": "valid TRC20 address required"})
		return
	}
	var user model.TelegramUser
	if err := h.st.DB.First(&user, b.UserID).Error; err != nil {
		c.JSON(400, gin.H{"error": "user not found"})
		return
	}
	b.Status = 1

	var old model.DepositAddress
	err := h.st.DB.Where("user_id=? AND network=?", b.UserID, b.Network).First(&old).Error
	if err == nil {
		if old.Address != b.Address {
			old.ScanTimestamp = 0
		}
		old.Address = b.Address
		old.Status = b.Status
		err = h.st.DB.Save(&old).Error
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		b.ID = 0
		b.ScanTimestamp = 0
		err = h.st.DB.Create(&b).Error
	}

	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
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
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		respond(c, err)
		return
	}
	row, err := h.bots.Save(c.Request.Context(), b.ID, b.Name, b.Token, b.Enabled)
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
