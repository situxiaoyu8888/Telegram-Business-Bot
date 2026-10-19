package support

import (
	"context"
	"fmt"
	"strings"
	"telegram-business-bot/internal/lease"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"time"
)

type Service struct{ st *store.Store }

func New(st *store.Store) *Service { return &Service{st: st} }
func (s *Service) Create(userID uint64, tgID int64, content string, botID ...uint64) (*model.SupportTicket, error) {
	t := &model.SupportTicket{UserID: userID, TelegramUserID: tgID, Subject: "Telegram客服", Content: content, Status: "open"}
	if len(botID) > 0 {
		t.BotID = botID[0]
	}
	return t, s.st.DB.Create(t).Error
}

// Reply keeps the saved answer and delivery outcome separate; failed answers remain retryable.
func (s *Service) Reply(ctx context.Context, id uint64, admin, reply string, send func(context.Context, int64, string) error) error {
	if strings.TrimSpace(reply) == "" {
		return fmt.Errorf("reply required")
	}
	return lease.Run(ctx, s.st.Redis, fmt.Sprintf("support:reply:%d", id), func(ctx context.Context) error {
		return s.deliverReply(ctx, id, admin, reply, send)
	})
}
func (s *Service) deliverReply(ctx context.Context, id uint64, admin, reply string, send func(context.Context, int64, string) error) error {
	var ticket model.SupportTicket
	if err := s.st.DB.First(&ticket, id).Error; err != nil {
		return err
	}
	if ticket.Status == "answered" {
		return fmt.Errorf("ticket already answered")
	}
	if err := s.st.DB.Model(&ticket).Updates(map[string]any{"admin_reply": reply, "replied_by": admin, "delivery_status": "sending", "delivery_error": ""}).Error; err != nil {
		return err
	}
	if err := send(ctx, ticket.TelegramUserID, reply); err != nil {
		if saveErr := s.st.DB.Model(&ticket).Updates(map[string]any{"delivery_status": "failed", "delivery_error": err.Error()}).Error; saveErr != nil {
			return fmt.Errorf("delivery failed: %v; saving failure: %w", err, saveErr)
		}
		return fmt.Errorf("reply saved but delivery failed; retry required: %w", err)
	}
	now := time.Now()
	return s.st.DB.Model(&ticket).Updates(map[string]any{"status": "answered", "delivery_status": "sent", "delivery_error": "", "replied_at": &now}).Error
}
