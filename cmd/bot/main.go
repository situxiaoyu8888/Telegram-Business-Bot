package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"telegram-business-bot/internal/app"
	"telegram-business-bot/internal/config"
	"telegram-business-bot/internal/logging"
	"telegram-business-bot/internal/telegram"

	"github.com/gin-gonic/gin"

	"go.uber.org/zap"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	logger, logFile, err := logging.New(logging.Options{
		Dev:        cfg.AppEnv == "dev",
		Dir:        cfg.LogDir,
		MaxSizeMB:  cfg.LogMaxSizeMB,
		MaxBackups: cfg.LogMaxBackups,
		MaxAgeDays: cfg.LogMaxAgeDays,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer logger.Sync()
	if logFile != nil {
		defer logFile.Close()
		// HTTP panic stacks from gin.Recovery go to the log file as well.
		gin.DefaultErrorWriter = io.MultiWriter(os.Stderr, logFile)
		logger.Info("file logging enabled", zap.String("dir", cfg.LogDir))
	}

	if cfg.TelegramAPIURL != "" {
		telegram.APIBase = cfg.TelegramAPIURL
		logger.Info("using custom Telegram Bot API server", zap.String("url", cfg.TelegramAPIURL))
	}
	application, err := app.New(cfg, logger)
	if err != nil {
		logger.Fatal("initialize app", zap.Error(err))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	application.StartWorkers(ctx)

	go func() {
		if err := application.StartTelegram(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("telegram stopped", zap.Error(err))
			cancel()
		}
	}()

	go func() {
		if err := application.StartAdminTelegram(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("admin telegram stopped", zap.Error(err))
			cancel()
		}
	}()

	newServer := func(addr string, h http.Handler) *http.Server {
		return &http.Server{
			Addr:              addr,
			Handler:           h,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
	}
	var servers []*http.Server
	for _, l := range application.Listeners() {
		server := newServer(l.Addr, l.Handler)
		servers = append(servers, server)
		go func(server *http.Server, kind string) {
			logger.Info("http server started", zap.String("addr", server.Addr), zap.String("serves", kind))
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("http server error", zap.String("addr", server.Addr), zap.Error(err))
				cancel()
			}
		}(server, l.Serves)
	}

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdownCtx)
	}
	_ = application.Close()
	logger.Info("application stopped")
}
