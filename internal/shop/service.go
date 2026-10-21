// Package shop is the H5 shop opened as a Telegram Mini App: catalog, cart, addresses,
// coupons and orders paid from the USDT wallet. Products belong to existing merchants.
package shop

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

// Order statuses. Express orders go paid → shipped → completed, pickup orders
// paid → ready → completed, and virtual orders paid → completed once codes are delivered.
// Paid, shipped and ready orders can be canceled, which refunds the wallet.
const (
	StatusPaid      = "paid"
	StatusShipped   = "shipped"
	StatusReady     = "ready"
	StatusCompleted = "completed"
	StatusCanceled  = "canceled"
)

// Delivery types.
const (
	DeliveryExpress = "express"
	DeliveryPickup  = "pickup"
	DeliveryVirtual = "virtual"
)

// Notifier tells a buyer about their order through the bot they shopped from.
type Notifier interface {
	Notify(ctx context.Context, botID uint64, telegramUserID int64, html string) error
}

type Service struct {
	st       *store.Store
	wallet   *wallet.Service
	notifier Notifier
	logger   *zap.Logger
}

func New(st *store.Store, w *wallet.Service, n Notifier, logger *zap.Logger) *Service {
	return &Service{st: st, wallet: w, notifier: n, logger: logger}
}

// UserError is a message safe to show to the buyer or administrator.
type UserError struct{ Msg string }

func (e UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return UserError{fmt.Sprintf(format, args...)} }

// IsUserError reports whether err carries a message meant for people rather than logs.
func IsUserError(err error) bool {
	var u UserError
	return errors.As(err, &u)
}

// parseAmount accepts a non-negative USDT amount with at most 6 decimal places.
func parseAmount(s, field string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil || d.IsNegative() || !d.Equal(d.Truncate(6)) {
		return decimal.Zero, userErr("%s需为非负数，最多 6 位小数", field)
	}
	return d, nil
}

func amount(s string) decimal.Decimal {
	d, _ := decimal.NewFromString(s)
	return d
}

func checkLen(value, field string, max int, required bool) error {
	n := utf8.RuneCountInString(strings.TrimSpace(value))
	if required && n == 0 {
		return userErr("请填写%s", field)
	}
	if n > max {
		return userErr("%s不能超过 %d 个字", field, max)
	}
	return nil
}

func (s *Service) notify(botID uint64, tgUserID int64, html string) {
	if s.notifier == nil || tgUserID == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.notifier.Notify(ctx, botID, tgUserID, html); err != nil && s.logger != nil {
			s.logger.Warn("shop notification failed", zap.Int64("telegram_user_id", tgUserID), zap.Error(err))
		}
	}()
}

// firstImage returns the cover image of a newline-separated image list.
func firstImage(images string) string {
	for _, line := range strings.Split(images, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
