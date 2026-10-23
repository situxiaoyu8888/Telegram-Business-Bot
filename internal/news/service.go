// Package news keeps news articles and decides who receives them: articles are written in the
// console, fetched from RSS/Atom feeds or copied from channel posts; published articles are
// queued once per subscriber and per group/channel of every news bot (NewsDelivery). Sending is
// done by the telegram package (RunNewsDelivery).
package news

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/lease"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// Article statuses and sources.
const (
	StatusDraft     = "draft"
	StatusPending   = "pending" // fetched or copied, waiting for review
	StatusScheduled = "scheduled"
	StatusPublished = "published"

	SourceManual  = "manual"
	SourceRSS     = "rss"
	SourceChannel = "channel"
)

type Service struct {
	st     *store.Store
	logger *zap.Logger
	now    func() time.Time
	fetch  Fetcher
}

func New(st *store.Store, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{st: st, logger: logger, now: time.Now, fetch: httpFetch}
}

// WithFetcher replaces the feed downloader (tests).
func (s *Service) WithFetcher(f Fetcher) *Service { s.fetch = f; return s }

// Categories lists categories in display order.
func (s *Service) Categories(enabledOnly bool) ([]model.NewsCategory, error) {
	q := s.st.DB.Order("sort asc, id asc")
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	out := []model.NewsCategory{}
	return out, q.Find(&out).Error
}

func (s *Service) published() *gorm.DB {
	return s.st.DB.Model(&model.NewsArticle{}).Where("status = ?", StatusPublished)
}

// Latest returns a page of published articles, newest first (categoryID 0 = all).
func (s *Service) Latest(categoryID uint64, page, size int) ([]model.NewsArticle, bool, error) {
	if page < 1 {
		page = 1
	}
	q := s.published()
	if categoryID != 0 {
		q = q.Where("category_id = ?", categoryID)
	}
	out := []model.NewsArticle{}
	if err := q.Order("published_at desc, id desc").Offset((page - 1) * size).Limit(size + 1).Find(&out).Error; err != nil {
		return nil, false, err
	}
	more := len(out) > size
	if more {
		out = out[:size]
	}
	return out, more, nil
}

// Search finds published articles whose title or text contains q.
func (s *Service) Search(q string, limit int) ([]model.NewsArticle, error) {
	q = strings.TrimSpace(q)
	out := []model.NewsArticle{}
	if q == "" {
		return out, nil
	}
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	return out, s.published().Where("title LIKE ? OR content LIKE ?", like, like).Order("published_at desc, id desc").Limit(limit).Find(&out).Error
}

// Article returns a published article.
func (s *Service) Article(id uint64) (*model.NewsArticle, error) {
	var a model.NewsArticle
	if err := s.published().Where("id = ?", id).First(&a).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// CategoryName returns a category's name ("" when missing).
func (s *Service) CategoryName(id uint64) string {
	var c model.NewsCategory
	if id == 0 || s.st.DB.Select("name").First(&c, id).Error != nil {
		return ""
	}
	return c.Name
}

// Subscribed returns the category IDs the user subscribed to on a bot (0 = every category).
func (s *Service) Subscribed(botID uint64, telegramUserID int64) (map[uint64]bool, error) {
	var rows []model.NewsSubscription
	if err := s.st.DB.Where("bot_id = ? AND telegram_user_id = ?", botID, telegramUserID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[uint64]bool{}
	for _, r := range rows {
		out[r.CategoryID] = true
	}
	return out, nil
}

// ToggleSubscription subscribes or unsubscribes a category and reports the new state.
func (s *Service) ToggleSubscription(botID uint64, telegramUserID int64, categoryID uint64) (bool, error) {
	res := s.st.DB.Where("bot_id = ? AND telegram_user_id = ? AND category_id = ?", botID, telegramUserID, categoryID).Delete(&model.NewsSubscription{})
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected > 0 {
		return false, nil
	}
	err := s.st.DB.Create(&model.NewsSubscription{BotID: botID, TelegramUserID: telegramUserID, CategoryID: categoryID}).Error
	if store.IsDuplicate(err) {
		return true, nil
	}
	return err == nil, err
}

// Unsubscribe removes every subscription of a user on a bot (they blocked it).
func (s *Service) Unsubscribe(botID uint64, telegramUserID int64) error {
	return s.st.DB.Where("bot_id = ? AND telegram_user_id = ?", botID, telegramUserID).Delete(&model.NewsSubscription{}).Error
}

// ChatInfo describes a group or channel a news bot is in.
type ChatInfo struct {
	ChatID   int64
	Type     string
	Title    string
	Username string
	Status   string // the bot's membership status
}

// RecordChat remembers a group or channel the bot joined or left. New chats start with pushing
// and copying off; the console turns them on.
func (s *Service) RecordChat(botID uint64, c ChatInfo) error {
	status := c.Status
	if status == "kicked" || status == "left" {
		status = "left"
	}
	row := model.NewsChat{BotID: botID, ChatID: c.ChatID, Type: c.Type, Title: truncate(c.Title, 255), Username: truncate(c.Username, 64), Status: status}
	return s.st.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "bot_id"}, {Name: "chat_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"type", "title", "username", "status", "updated_at"}),
	}).Create(&row).Error
}

// ChannelPost is a post in a channel a news bot is an administrator of.
type ChannelPost struct {
	BotID     uint64
	ChatID    int64
	Title     string // the channel's name
	Username  string // the channel's public @username, for links
	MessageID int
	Text      string // text or media caption
}

// IngestChannelPost turns a post of a source channel into an article (published or pending,
// as configured). It does nothing for channels that are not news sources.
func (s *Service) IngestChannelPost(p ChannelPost) (*model.NewsArticle, error) {
	var chat model.NewsChat
	err := s.st.DB.Where("bot_id = ? AND chat_id = ? AND source_enabled = ?", p.BotID, p.ChatID, true).First(&chat).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(p.Text)
	title, body := splitTitle(text)
	if title == "" {
		title = p.Title
		if title == "" {
			title = "频道消息"
		}
	}
	key := fmt.Sprintf("channel:%d:%d", p.ChatID, p.MessageID)
	a := model.NewsArticle{CategoryID: chat.SourceCategoryID, Title: truncate(title, 255), Content: body, Source: SourceChannel, SourceID: chat.ID, ExternalKey: &key,
		ChannelChat: p.ChatID, ChannelMsgID: p.MessageID, ChannelBotID: p.BotID, Push: true, Status: StatusPending}
	if p.Username != "" {
		a.LinkURL = fmt.Sprintf("https://t.me/%s/%d", p.Username, p.MessageID)
	}
	if chat.AutoPublish {
		now := s.now()
		a.Status, a.PublishedAt = StatusPublished, &now
	}
	if err := s.st.DB.Create(&a).Error; err != nil {
		if store.IsDuplicate(err) {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

// splitTitle uses the first line of a post as its title.
func splitTitle(text string) (string, string) {
	first, rest, _ := strings.Cut(text, "\n")
	first = strings.TrimSpace(first)
	if utf8.RuneCountInString(first) > 120 {
		return truncate(first, 60) + "…", text
	}
	return first, strings.TrimSpace(rest)
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Publish publishes a draft or pending article now.
func (s *Service) Publish(id uint64) error {
	now := s.now()
	res := s.st.DB.Model(&model.NewsArticle{}).Where("id = ? AND status IN ?", id, []string{StatusDraft, StatusPending, StatusScheduled}).
		Updates(map[string]any{"status": StatusPublished, "published_at": now, "publish_at": nil})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("新闻不存在或已发布")
	}
	return nil
}

// promoteScheduled publishes scheduled articles whose time has come.
func (s *Service) promoteScheduled() error {
	now := s.now()
	return s.st.DB.Model(&model.NewsArticle{}).Where("status = ? AND publish_at <= ?", StatusScheduled, now).
		Updates(map[string]any{"status": StatusPublished, "published_at": now}).Error
}

// Fanout queues deliveries for published articles not yet dispatched: every subscriber whose
// subscription covers the article's category, and every push-enabled chat, per news bot.
func (s *Service) Fanout() error {
	var list []model.NewsArticle
	if err := s.st.DB.Where("status = ? AND dispatched_at IS NULL", StatusPublished).Order("id asc").Limit(20).Find(&list).Error; err != nil {
		return err
	}
	bots, err := s.newsBots()
	if err != nil {
		return err
	}
	for _, a := range list {
		var rows []model.NewsDelivery
		if a.Push && len(bots) > 0 {
			var subs []model.NewsSubscription
			if err := s.st.DB.Where("bot_id IN ? AND category_id IN ?", bots, []uint64{0, a.CategoryID}).Find(&subs).Error; err != nil {
				return err
			}
			seen := map[[2]int64]bool{}
			for _, sub := range subs {
				k := [2]int64{int64(sub.BotID), sub.TelegramUserID}
				if !seen[k] {
					seen[k] = true
					rows = append(rows, model.NewsDelivery{ArticleID: a.ID, BotID: sub.BotID, ChatID: sub.TelegramUserID, Status: "pending"})
				}
			}
			var chats []model.NewsChat
			if err := s.st.DB.Where("bot_id IN ? AND push_enabled = ? AND status <> ?", bots, true, "left").Find(&chats).Error; err != nil {
				return err
			}
			for _, c := range chats {
				// A source channel never gets its own posts back.
				if c.ChatID == a.ChannelChat || !coversCategory(c.PushCategories, a.CategoryID) {
					continue
				}
				rows = append(rows, model.NewsDelivery{ArticleID: a.ID, BotID: c.BotID, ChatID: c.ChatID, Status: "pending"})
			}
		}
		err := s.st.DB.Transaction(func(tx *gorm.DB) error {
			if len(rows) > 0 {
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).CreateInBatches(rows, 500).Error; err != nil {
					return err
				}
			}
			return tx.Model(&model.NewsArticle{}).Where("id = ?", a.ID).Update("dispatched_at", s.now()).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func coversCategory(list string, id uint64) bool {
	if strings.TrimSpace(list) == "" {
		return true
	}
	for _, p := range strings.Split(list, ",") {
		if n, err := strconv.ParseUint(strings.TrimSpace(p), 10, 64); err == nil && n == id {
			return true
		}
	}
	return false
}

// newsBots returns the IDs of enabled business bots with the news purpose.
func (s *Service) newsBots() ([]uint64, error) {
	var ids []uint64
	err := s.st.DB.Model(&model.BusinessBot{}).Where("enabled = ? AND purpose = ?", true, "news").Pluck("id", &ids).Error
	return ids, err
}

// PendingDeliveries returns queued deliveries, oldest first.
func (s *Service) PendingDeliveries(limit int) ([]model.NewsDelivery, error) {
	now := s.now()
	// A send interrupted by a crash or restart is retried after a while (it may have gone out, so
	// only a few times), and ones that keep being interrupted are given up.
	stuck := s.st.DB.Model(&model.NewsDelivery{}).Where("status = ? AND (claimed_at < ? OR claimed_at IS NULL)", "sending", now.Add(-stuckSending))
	if err := stuck.Session(&gorm.Session{}).Where("attempts >= ?", maxDeliveryRetries).Updates(map[string]any{"status": "failed", "error": "发送中断次数过多"}).Error; err != nil {
		return nil, err
	}
	if err := stuck.Session(&gorm.Session{}).Where("attempts < ?", maxDeliveryRetries).Updates(map[string]any{"status": "pending", "attempts": gorm.Expr("attempts + 1")}).Error; err != nil {
		return nil, err
	}
	out := []model.NewsDelivery{}
	return out, s.st.DB.Where("status = ? AND (retry_at IS NULL OR retry_at <= ?)", "pending", now).Order("id asc").Limit(limit).Find(&out).Error
}

const (
	stuckSending       = 10 * time.Minute
	maxDeliveryRetries = 5
)

// ClaimDelivery marks a pending delivery as being sent; false when another worker took it.
func (s *Service) ClaimDelivery(id uint64) (bool, error) {
	res := s.st.DB.Model(&model.NewsDelivery{}).Where("id = ? AND status = ?", id, "pending").Updates(map[string]any{"status": "sending", "claimed_at": s.now()})
	return res.RowsAffected == 1, res.Error
}

// FinishDelivery records the result of sending. Failures where nothing went out (rate limits,
// server errors, no connection) are retried later, a few times; anything else is final, since a
// timeout may have delivered the message. A group or channel that removed the bot stops getting
// news.
func (s *Service) FinishDelivery(id uint64, sendErr error) error {
	updates := map[string]any{"status": "sent", "sent_at": s.now(), "error": ""}
	if sendErr != nil {
		msg := sendErr.Error()
		updates["status"], updates["error"] = "failed", truncate(msg, 255)
		var d model.NewsDelivery
		if err := s.st.DB.Select("id", "bot_id", "chat_id", "attempts").First(&d, id).Error; err != nil {
			return err
		}
		switch {
		case !retryableSendError(msg):
			// Permanent, or possibly delivered (timeout): not sent again.
			if permanentSendError(msg) && d.ChatID < 0 {
				if err := s.st.DB.Model(&model.NewsChat{}).Where("bot_id = ? AND chat_id = ?", d.BotID, d.ChatID).Update("push_enabled", false).Error; err != nil {
					return err
				}
			}
		case d.Attempts < maxDeliveryRetries:
			wait := time.Duration(30<<d.Attempts) * time.Second // 30s, 1m, 2m, 4m, 8m
			updates["status"], updates["attempts"], updates["retry_at"] = "pending", d.Attempts+1, s.now().Add(wait)
		}
	}
	return s.st.DB.Model(&model.NewsDelivery{}).Where("id = ?", id).Updates(updates).Error
}

// retryableSendError reports failures where the message certainly did not go out: rate limits,
// Telegram server errors and connections that never reached it.
func retryableSendError(msg string) bool {
	for _, s := range []string{"http 429", "error 429", "Too Many Requests", "http 5", "error 5", "connection refused", "no such host", "network is unreachable"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// permanentSendError reports Bot API errors that retrying cannot fix: the bot was blocked,
// kicked or never added, or the chat is gone.
func permanentSendError(msg string) bool {
	for _, s := range []string{"Forbidden", "chat not found", "bot was kicked", "bot is not a member", "user is deactivated", "CHAT_WRITE_FORBIDDEN", "have no rights"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Article loads any article (for delivery, regardless of status).
func (s *Service) ArticleByID(id uint64) (*model.NewsArticle, error) {
	var a model.NewsArticle
	if err := s.st.DB.First(&a, id).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// Tick runs one round of background work: scheduled publishing, due feeds and fan-out.
func (s *Service) Tick(ctx context.Context) {
	if err := s.promoteScheduled(); err != nil {
		s.logger.Error("news scheduling failed", zap.Error(err))
	}
	s.FetchDue(ctx)
	if err := s.Fanout(); err != nil {
		s.logger.Error("news fan-out failed", zap.Error(err))
	}
}

// Run calls Tick every 20 seconds on one replica at a time.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		err := lease.Run(ctx, s.st.Redis, "news:worker", func(ctx context.Context) error { s.Tick(ctx); return nil })
		if err != nil && !errors.Is(err, lease.ErrBusy) && ctx.Err() == nil {
			s.logger.Warn("news worker lease failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
