// Package shop is the H5 shop opened as a Telegram Mini App: catalog, cart, addresses,
// coupons and orders paid from the USDT wallet. Products belong to existing merchants.
package shop

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"telegram-business-bot/internal/model"
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

// Notifier delivers shop messages: Notify reaches a buyer through the bot they shopped from,
// NotifyStaff reaches administrators and merchant contacts.
type Notifier interface {
	// link, when set, adds a button that opens that shop page.
	Notify(ctx context.Context, botID uint64, telegramUserID int64, html string, link *Link) error
	NotifyStaff(ctx context.Context, chatIDs []int64, html string) error
}

// Link is a button that opens a page of the H5 shop, e.g. Fragment "/order/12".
type Link struct {
	Text     string
	Fragment string
}

// autoOperator marks changes made by the shop's background worker.
const autoOperator = "auto"

type Service struct {
	st       *store.Store
	wallet   *wallet.Service
	notifier Notifier
	logger   *zap.Logger
	staff    []int64 // administrators told about new orders and after-sales
}

// WithStaff sets the administrators' Telegram IDs that receive order and after-sale notices.
func (s *Service) WithStaff(ids []int64) *Service { s.staff = ids; return s }

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

// notify messages a buyer, with a "view order" button when orderID is set.
func (s *Service) notify(botID uint64, tgUserID int64, html string, orderID uint64) {
	var link *Link
	if orderID != 0 {
		link = &Link{Text: "📦 查看订单", Fragment: fmt.Sprintf("/order/%d", orderID)}
	}
	s.send(botID, tgUserID, html, link)
}

// notifyChat messages a buyer with a button that opens a conversation page.
func (s *Service) notifyChat(botID uint64, tgUserID int64, html, fragment string) {
	s.send(botID, tgUserID, html, &Link{Text: "💬 查看对话", Fragment: fragment})
}

func (s *Service) send(botID uint64, tgUserID int64, html string, link *Link) {
	if s.notifier == nil || tgUserID == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := s.notifier.Notify(ctx, botID, tgUserID, html, link); err != nil && s.logger != nil {
			s.logger.Warn("shop notification failed", zap.Int64("telegram_user_id", tgUserID), zap.Error(err))
		}
	}()
}

// notifyStaff tells administrators and the merchant's notification contact, if any.
func (s *Service) notifyStaff(merchantID uint64, html string) {
	if s.notifier == nil {
		return
	}
	ids := append([]int64{}, s.staff...)
	var m model.Merchant
	if merchantID != 0 && s.st.DB.Select("notify_chat_id").First(&m, merchantID).Error == nil && m.NotifyChatID != 0 {
		dup := false
		for _, id := range ids {
			dup = dup || id == m.NotifyChatID
		}
		if !dup {
			ids = append(ids, m.NotifyChatID)
		}
	}
	if len(ids) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.notifier.NotifyStaff(ctx, ids, html); err != nil && s.logger != nil {
			s.logger.Warn("shop staff notification failed", zap.Error(err))
		}
	}()
}

// config reads an integer shop setting from system_configs, falling back to def.
func (s *Service) configInt(key string, def int) int {
	var c model.SystemConfig
	if err := s.st.DB.Where("`key` = ?", key).First(&c).Error; err != nil {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(c.Value))
	if err != nil || n < 0 {
		return def
	}
	return n
}

func (s *Service) configString(key string) string {
	var c model.SystemConfig
	if err := s.st.DB.Where("`key` = ?", key).First(&c).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

// effectivePrice is the SKU price at now: its promo price while the product's promo window is
// open and promo stock (when limited) remains, otherwise the regular price. Nothing is stored,
// so prices revert by themselves.
func effectivePrice(price, promoPrice string, promoStock *int64, startsAt, endsAt *time.Time, now time.Time) (current string, onPromo bool) {
	promo := amount(promoPrice)
	if promo.IsPositive() && (promoStock == nil || *promoStock > 0) && startsAt != nil && endsAt != nil && !now.Before(*startsAt) && now.Before(*endsAt) {
		return promo.StringFixed(6), true
	}
	return price, false
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
