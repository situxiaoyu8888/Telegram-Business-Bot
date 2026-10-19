package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"telegram-business-bot/internal/app"
	"telegram-business-bot/internal/config"

	"go.uber.org/zap"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	logger, err := zap.NewProduction()
	if cfg.AppEnv == "dev" {
		logger, err = zap.NewDevelopment()
	}
	if err != nil {
		log.Fatal(err)
	}
	defer logger.Sync()

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

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           application.Router(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		logger.Info("http server started", zap.String("addr", cfg.HTTPAddr))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", zap.Error(err))
			cancel()
		}
	}()

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)
	_ = application.Close()
	logger.Info("application stopped")
}
