package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"telegram-business-bot/internal/admin"
	"telegram-business-bot/internal/business"
	"telegram-business-bot/internal/config"
	"telegram-business-bot/internal/menu"
	"telegram-business-bot/internal/push"
	"telegram-business-bot/internal/referral"
	"telegram-business-bot/internal/session"
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
	tronWatcher   *trongrid.Watcher
	pushService   *push.Service
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
	tgRouter := telegram.NewRouter(tg, st, sessions, walletSvc, menuSvc, supportSvc, refSvc, cfg.AdminIDs, cfg.TronFallbackDepositAddress, false, logger)

	var adminTG *telegram.Client
	var adminTGRouter *telegram.Router
	if cfg.AdminTelegramBotToken != "" {
		adminTG = telegram.NewClient(cfg.AdminTelegramBotToken)
		adminTGRouter = telegram.NewRouter(adminTG, st, sessions.WithNamespace("admin"), walletSvc, menuSvc, supportSvc, refSvc, cfg.AdminIDs, cfg.TronFallbackDepositAddress, true, logger)
	}

	if cfg.AppEnv != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Static(
		"/uploads",
		cfg.UploadDir,
	)
	if err := admin.Bootstrap(st.DB, cfg.AdminWebUser, cfg.AdminWebPassword); err != nil {
		return nil, fmt.Errorf("bootstrap admin account: %w", err)
	}
	bots := business.New(st, tg, cfg.BusinessBotKey, cfg.TelegramBotToken, cfg.AdminTelegramBotToken)
	bots.Router = func(id uint64, client *telegram.Client) *telegram.Router {
		return telegram.NewRouter(client, st, sessions.WithNamespace(fmt.Sprintf("business:%d", id)), walletSvc, menuSvc, supportSvc, refSvc, cfg.AdminIDs, "", false, logger).WithBotID(id)
	}
	pushSvc := push.New(st, tg, logger).WithBots(bots)
	a := &App{businessBots: bots, cfg: cfg, logger: logger, store: st, tg: tg, tgRouter: tgRouter, adminTG: adminTG, adminTGRouter: adminTGRouter, engine: engine, pushService: pushSvc}
	a.registerRoutes()
	admin.New(
		st,
		walletSvc,
		supportSvc,
		tg,
		pushSvc,
		cfg.UploadDir,
		cfg.UploadPublicBaseURL,
		cfg.UploadMaxImageMB,
		cfg.UploadMaxVideoMB,
	).WithBots(bots).Register(engine)
	tronClient := trongrid.New(cfg.TronGridBaseURL, cfg.TronGridAPIKey)
	a.tronWatcher = trongrid.NewWatcher(st, walletSvc, refSvc, tronClient, cfg.TronUSDTContract, cfg.TronConfirmOnly, cfg.TronWatchInterval, logger)
	return a, nil
}

func (a *App) registerRoutes() {
	a.engine.GET("/healthz", func(c *gin.Context) {
		dbOK := a.store.DB.Exec("SELECT 1").Error == nil
		redisOK := a.store.Redis.Ping(c.Request.Context()).Err() == nil
		c.JSON(http.StatusOK, gin.H{"ok": dbOK && redisOK, "db": dbOK, "redis": redisOK, "tron_watcher": a.cfg.TronWatchEnabled, "admin_bot": a.adminTG != nil})
	})
	a.engine.POST("/telegram/webhook", a.webhookHandler(false))
	a.engine.POST("/telegram/admin/webhook", a.webhookHandler(true))
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

func (a *App) StartWorkers(ctx context.Context) {
	go a.pushService.Run(ctx)
	go a.businessBots.Run(ctx)
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
	me, err := client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("%s telegram getMe: %w", name, err)
	}
	a.logger.Info("telegram authorized", zap.String("kind", name), zap.String("username", me.Username), zap.Int64("id", me.ID))
	go router.SyncCommandMenu(ctx)
	if a.cfg.TelegramMode == "webhook" {
		if !telegram.IsTelegramURL(webhookURL) {
			return fmt.Errorf("%s webhook URL must be valid https URL", name)
		}
		if err := client.SetWebhook(ctx, webhookURL, webhookSecret); err != nil {
			return fmt.Errorf("set %s webhook: %w", name, err)
		}
		<-ctx.Done()
		return ctx.Err()
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
