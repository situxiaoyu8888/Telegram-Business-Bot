package app

import (
	"context"
	"time"

	"go.uber.org/zap"

	"telegram-business-bot/internal/telegram"
)

// Retry and check intervals; tests shorten them.
var (
	retryMin          = 5 * time.Second
	retryMax          = 5 * time.Minute
	webhookCheckEvery = 10 * time.Minute
)

// retry calls fn until it succeeds or ctx ends, waiting retryMin, doubling up to retryMax.
func retry(ctx context.Context, fn func() error, failed func(err error, wait time.Duration)) error {
	wait := retryMin
	for {
		err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		failed(err, wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, retryMax)
	}
}

type webhookClient interface {
	SetWebhook(ctx context.Context, url, secret string) error
	GetWebhookInfo(ctx context.Context) (*telegram.WebhookInfo, error)
}

// keepWebhook registers the webhook, retrying until Telegram accepts it (DNS, CDN or network
// trouble must not stop the program), then checks it periodically: if it was removed or points
// elsewhere it is registered again, and new delivery errors reported by Telegram are logged.
func keepWebhook(ctx context.Context, client webhookClient, url, secret string, logger *zap.Logger) error {
	register := func() error {
		return retry(ctx, func() error { return client.SetWebhook(ctx, url, secret) }, func(err error, wait time.Duration) {
			logger.Error("set webhook failed, retrying", zap.String("url", url), zap.Error(err), zap.Duration("retry_in", wait))
		})
	}
	if err := register(); err != nil {
		return err
	}
	logger.Info("webhook registered", zap.String("url", url))
	lastError := time.Now().Unix() // only report errors after registration
	ticker := time.NewTicker(webhookCheckEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		info, err := client.GetWebhookInfo(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Warn("get webhook info failed", zap.Error(err))
			continue
		}
		if info.URL != url {
			logger.Warn("webhook missing or changed, registering again", zap.String("current", info.URL), zap.String("url", url))
			if err := register(); err != nil {
				return err
			}
			logger.Info("webhook registered", zap.String("url", url))
			continue
		}
		if info.LastErrorDate > lastError && info.LastErrorMessage != "" {
			lastError = info.LastErrorDate
			logger.Warn("telegram reports webhook delivery error", zap.String("error", info.LastErrorMessage),
				zap.Time("at", time.Unix(info.LastErrorDate, 0)), zap.Int("pending_updates", info.PendingUpdateCount))
		}
	}
}
