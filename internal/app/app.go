package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"telegram-business-bot/internal/admin"
	"telegram-business-bot/internal/backup"
	"telegram-business-bot/internal/business"
	"telegram-business-bot/internal/config"
	"telegram-business-bot/internal/cors"
	"telegram-business-bot/internal/menu"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/news"
	"telegram-business-bot/internal/push"
	"telegram-business-bot/internal/redpacket"
	"telegram-business-bot/internal/referral"
	"telegram-business-bot/internal/session"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/support"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/trongrid"
	"telegram-business-bot/internal/wallet"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type App struct {
	businessBots  *business.Service
	cfg           config.Config
	logger        *zap.Logger
	store         *store.Store
	tg            *telegram.Client
	tgRouter      *telegram.Router
	adminTG       *telegram.Client
	adminTGRouter *telegram.Router
	engine        *gin.Engine
	listeners     []Listener
	tronWatcher   *trongrid.Watcher
	pushService   *push.Service
	shop          *shop.Service
	packets       *redpacket.Service
	news          *news.Service
	admin         *admin.Handler
}

func New(cfg config.Config, logger *zap.Logger) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	st, err := store.New(cfg.MySQLDSN, cfg.RedisAddr, cfg.RedisPassword, cfg.RedisDB)
	if err != nil {
		return nil, err
	}
	walletSvc := wallet.New(st, cfg.WalletCurrency, cfg.WithdrawMinUSDT, cfg.WithdrawFeeUSDT)
	menuSvc := menu.New(st)
	supportSvc := support.New(st)
	refSvc := referral.New(st, cfg.ReferralRate)
	sessions := session.New(st.Redis, cfg.SessionTTL)

	tg := telegram.NewClient(cfg.TelegramBotToken)
	// The admin bot keeps the command list; user-facing bots may open the H5 shop instead.
	shopButton := telegram.MenuButton{Text: cfg.ShopH5ButtonText, URL: cfg.ShopH5URL}
	packets := redpacket.New(st, walletSvc, logger)
	tgRouter := telegram.NewRouter(tg, st, sessions, walletSvc, menuSvc, supportSvc, refSvc, cfg.AdminIDs, cfg.TronFallbackDepositAddress, false, logger).WithMenuButton(shopButton).WithRedPackets(packets)

	var adminTG *telegram.Client
	var adminTGRouter *telegram.Router
	if cfg.AdminTelegramBotToken != "" {
		adminTG = telegram.NewClient(cfg.AdminTelegramBotToken)
		adminTGRouter = telegram.NewRouter(adminTG, st, sessions.WithNamespace("admin"), walletSvc, menuSvc, supportSvc, refSvc, cfg.AdminIDs, cfg.TronFallbackDepositAddress, true, logger)
	}

	if cfg.AppEnv != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}
	newEngine := func() *gin.Engine {
		e := gin.New()
		e.Use(gin.Recovery())
		return e
	}
	engine := newEngine()
	if cfg.Split() {
		// The consoles upload files to the main listener from their own origins.
		engine.Use(cors.Allow([]string{cfg.OperatorPublicURL, cfg.MerchantPublicURL}))
	}
	engine.Static(
		"/uploads",
		cfg.UploadDir,
	)
	if err := admin.Bootstrap(st.DB, cfg.AdminWebUser, cfg.AdminWebPassword); err != nil {
		return nil, fmt.Errorf("bootstrap admin account: %w", err)
	}
	bots := business.New(st, tg, cfg.BusinessBotKey, cfg.TelegramBotToken, cfg.AdminTelegramBotToken)
	newsSvc := news.New(st, logger)
	bots.Router = func(bot model.BusinessBot, client *telegram.Client) *telegram.Router {
		r := telegram.NewRouter(client, st, sessions.WithNamespace(fmt.Sprintf("business:%d", bot.ID)), walletSvc, menuSvc, supportSvc, refSvc, cfg.AdminIDs, "", false, logger).WithBotID(bot.ID)
		if bot.Purpose == business.PurposeNews {
			return r.WithNewsOnly(newsSvc)
		}
		r.WithRedPackets(packets)
		if bot.Purpose == business.PurposeRedPacket {
			return r.WithRedPacketOnly(cfg.ShopH5URL)
		}
		return r.WithMenuButton(shopButton)
	}
	packets.WithNotifier(func(ctx context.Context, botID uint64, telegramUserID int64, html string) error {
		sender, err := bots.Sender(ctx, botID)
		if err != nil {
			return err
		}
		return sender.SendMessageWithParseMode(ctx, telegramUserID, html, "HTML", nil)
	})
	pushSvc := push.New(st, tg, logger).WithBots(bots)
	// Staff notices go through the admin bot when there is one, since admins already talk to it.
	var staffSender telegram.Sender = tg
	if adminTG != nil {
		staffSender = adminTG
	}
	staffIDs := make([]int64, 0, len(cfg.AdminIDs))
	for id := range cfg.AdminIDs {
		staffIDs = append(staffIDs, id)
	}
	shopSvc := shop.New(st, walletSvc, shop.BotNotifier{Bots: bots, Staff: staffSender, ShopURL: cfg.ShopH5URL}, logger).WithStaff(staffIDs)
	shopHandler := shop.NewHandler(shopSvc, st, bots, logger).WithBotUsernames(func(ctx context.Context, botID uint64) string {
		if botID == 0 {
			name, _ := tg.Username(ctx)
			return name
		}
		var b model.BusinessBot
		_ = st.DB.WithContext(ctx).Select("username").First(&b, botID).Error
		return b.Username
	})
	a := &App{shop: shopSvc, packets: packets, news: newsSvc, businessBots: bots, cfg: cfg, logger: logger, store: st, tg: tg, tgRouter: tgRouter, adminTG: adminTG, adminTGRouter: adminTGRouter, engine: engine, pushService: pushSvc}
	a.registerRoutes()
	a.admin = admin.New(
		st,
		walletSvc,
		supportSvc,
		tg,
		pushSvc,
		cfg.UploadDir,
		cfg.UploadPublicBaseURL,
		cfg.UploadMaxImageMB,
		cfg.UploadMaxVideoMB,
	).WithBots(bots).WithShop(shopSvc).WithNews(newsSvc).WithLogger(logger).WithPublicURL(cfg.AdminPublicURL)
	a.listeners = []Listener{{Addr: cfg.HTTPAddr, Handler: engine, Serves: "webhooks, uploads"}}
	if !cfg.Split() {
		shopHandler.Register(engine)
		a.admin.Register(engine)
		a.listeners[0].Serves = "everything"
	} else {
		// Pages, consoles and APIs each listen on their own address; the API answers only the
		// configured origins and tells the consoles apart by them.
		api, operator, merchant, shopPage := newEngine(), newEngine(), newEngine(), newEngine()
		api.Use(cors.Allow(cfg.AllowedOrigins()))
		a.admin.WithSplit(cfg.APIPublicURL, cfg.MainPublicURL, cfg.OperatorPublicURL, cfg.MerchantPublicURL).WithPublicURL(cfg.MerchantPublicURL + "/admin/")
		a.admin.RegisterConsole(operator, admin.ConsoleOperator)
		a.admin.RegisterConsole(merchant, admin.ConsoleMerchant)
		a.admin.RegisterAPI(api)
		a.admin.RegisterUpload(engine)
		shopHandler.WithAPIBase(cfg.APIPublicURL)
		shopHandler.RegisterPage(shopPage)
		shopHandler.RegisterAPI(api)
		for _, e := range []*gin.Engine{api, operator, merchant, shopPage} {
			a.registerHealth(e)
		}
		a.listeners = append(a.listeners,
			Listener{Addr: cfg.OperatorHTTPAddr, Handler: operator, Serves: "operator console"},
			Listener{Addr: cfg.MerchantHTTPAddr, Handler: merchant, Serves: "merchant console"},
			Listener{Addr: cfg.ShopHTTPAddr, Handler: shopPage, Serves: "shop page"},
			Listener{Addr: cfg.APIHTTPAddr, Handler: api, Serves: "API"},
		)
	}
	tronClient := trongrid.New(cfg.TronGridBaseURL, cfg.TronGridAPIKey)
	a.tronWatcher = trongrid.NewWatcher(st, walletSvc, refSvc, tronClient, cfg.TronUSDTContract, cfg.TronConfirmOnly, cfg.TronWatchInterval, logger)
	return a, nil
}

// registerRoutes adds health checks and Telegram webhooks to every listener: webhooks must stay
// reachable from Telegram even when the console port is private.
func (a *App) registerRoutes() {
	a.registerHealth(a.engine)
	a.engine.POST("/telegram/webhook", a.webhookHandler(false))
	a.engine.POST("/telegram/admin/webhook", a.webhookHandler(true))
}

func (a *App) registerHealth(e *gin.Engine) {
	e.GET("/healthz", func(c *gin.Context) {
		dbOK := a.store.DB.Exec("SELECT 1").Error == nil
		redisOK := a.store.Redis.Ping(c.Request.Context()).Err() == nil
		c.JSON(http.StatusOK, gin.H{"ok": dbOK && redisOK, "db": dbOK, "redis": redisOK, "tron_watcher": a.cfg.TronWatchEnabled, "admin_bot": a.adminTG != nil})
	})
}

func (a *App) webhookHandler(adminBot bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if a.cfg.TelegramMode != "webhook" {
			c.JSON(404, gin.H{"error": "webhook disabled"})
			return
		}
		router := a.tgRouter
		secret := a.cfg.TelegramWebhookSecret
		if adminBot {
			if a.adminTG == nil || a.adminTGRouter == nil {
				c.JSON(404, gin.H{"error": "admin bot disabled"})
				return
			}
			router = a.adminTGRouter
			secret = a.cfg.AdminTelegramWebhookSecret
		}
		if secret == "" || c.GetHeader("X-Telegram-Bot-Api-Secret-Token") != secret {
			c.JSON(401, gin.H{"error": "invalid secret"})
			return
		}
		var u telegram.Update
		if err := c.ShouldBindJSON(&u); err != nil {
			c.JSON(400, gin.H{"error": "bad update"})
			return
		}
		if err := router.Handle(c.Request.Context(), u); err != nil {
			c.JSON(503, gin.H{"error": "update processing failed"})
			return
		}
		c.JSON(200, gin.H{"ok": true})
	}
}

func (a *App) Router() http.Handler { return a.engine }

// ShopRouter is the separate shop listener's handler, or nil when the shop shares Router.
// Listener is one HTTP address the program serves and what it is for.
type Listener struct {
	Addr    string
	Handler http.Handler
	Serves  string
}

// Listeners returns the HTTP listeners to start: one, or five with split listeners.
func (a *App) Listeners() []Listener { return a.listeners }

func (a *App) StartWorkers(ctx context.Context) {
	go a.pushService.Run(ctx)
	go a.businessBots.Run(ctx)
	go a.shop.Run(ctx)
	go a.packets.Run(ctx)
	go a.news.Run(ctx)
	go telegram.RunNewsDelivery(ctx, a.news, a.store.Redis, a.businessBots.Sender, a.logger)
	go a.admin.PurgeAuditLogs(ctx, 180*24*time.Hour)
	if a.cfg.BackupDir != "" {
		if sqlDB, err := a.store.DB.DB(); err == nil {
			go backup.RunDaily(ctx, sqlDB, a.store.Redis, a.cfg.BackupDir, a.cfg.BackupKeep, a.cfg.BackupHour, a.logger)
			a.logger.Info("daily database backup enabled", zap.String("dir", a.cfg.BackupDir), zap.Int("keep", a.cfg.BackupKeep), zap.Int("hour", a.cfg.BackupHour))
		}
	}
	a.logger.Info("message push worker started")
	if a.cfg.TronWatchEnabled {
		go a.tronWatcher.Run(ctx)
		a.logger.Info("TRON deposit watcher started", zap.Duration("interval", a.cfg.TronWatchInterval))
	}
}

func (a *App) StartTelegram(ctx context.Context) error {
	return a.runBot(ctx, a.tg, a.tgRouter, a.cfg.TelegramWebhookURL, a.cfg.TelegramWebhookSecret, "user")
}

func (a *App) StartAdminTelegram(ctx context.Context) error {
	if a.adminTG == nil || a.adminTGRouter == nil {
		<-ctx.Done()
		return ctx.Err()
	}
	if a.cfg.TelegramMode == "webhook" && a.cfg.AdminTelegramWebhookURL == "" {
		return fmt.Errorf("ADMIN_TELEGRAM_WEBHOOK_URL is required when admin bot is enabled in webhook mode")
	}
	return a.runBot(ctx, a.adminTG, a.adminTGRouter, a.cfg.AdminTelegramWebhookURL, a.cfg.AdminTelegramWebhookSecret, "admin")
}

func (a *App) runBot(ctx context.Context, client *telegram.Client, router *telegram.Router, webhookURL, webhookSecret, name string) error {
	var me *telegram.User
	err := retry(ctx, func() (err error) { me, err = client.GetMe(ctx); return err }, func(err error, wait time.Duration) {
		a.logger.Warn("telegram getMe failed, retrying", zap.String("kind", name), zap.Error(err), zap.Duration("retry_in", wait))
	})
	if err != nil {
		return err
	}
	a.logger.Info("telegram authorized", zap.String("kind", name), zap.String("username", me.Username), zap.Int64("id", me.ID))
	go router.SyncCommandMenu(ctx)
	if a.cfg.TelegramMode == "webhook" {
		if !telegram.IsTelegramURL(webhookURL) {
			return fmt.Errorf("%s webhook URL must be valid https URL", name)
		}
		return keepWebhook(ctx, client, webhookURL, webhookSecret, a.logger.With(zap.String("kind", name)))
	}
	if err := client.DeleteWebhook(ctx, false); err != nil {
		return fmt.Errorf("delete %s webhook before polling: %w", name, err)
	}
	offset := 0
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := client.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.logger.Warn("getUpdates failed", zap.String("kind", name), zap.Error(err))
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			if err := router.Handle(ctx, u); err != nil {
				a.logger.Warn("process update", zap.Error(err))
				offset = u.UpdateID
				break
			}
		}
	}
}

func (a *App) Close() error { return a.store.Close() }
