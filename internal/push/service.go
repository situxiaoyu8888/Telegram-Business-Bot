package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"

	"telegram-business-bot/internal/lease"

	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	maxAttempts = 3

	MessageTypeText  = "text"
	MessageTypePhoto = "photo"
	MessageTypeVideo = "video"

	ContentModeAuto     = "auto"
	ContentModeCaption  = "caption"
	ContentModeSeparate = "separate"

	// Telegram photo/video caption 最大约 1024 字符。
	// 这里预留一些 HTML 标签/entity 的安全空间。
	safeCaptionRuneLimit = 900
)

type Sender = telegram.Sender

type BotResolver interface {
	Sender(context.Context, uint64) (telegram.Sender, error)
	Resolve(context.Context, string, *uint64) (uint64, error)
}

type Service struct {
	bots     BotResolver
	st       *store.Store
	tg       Sender
	logger   *zap.Logger
	interval time.Duration
	batch    int
	rate     int
}

func New(
	st *store.Store,
	tg Sender,
	logger *zap.Logger,
) *Service {
	return &Service{
		st:       st,
		tg:       tg,
		logger:   logger,
		interval: time.Second,
		batch:    50,
		rate:     20,
	}
}

type CreateRequest struct {
	RepeatSeconds int64   `json:"repeat_seconds"`
	BotID         *uint64 `json:"bot_id"`
	Category      string  `json:"category"`
	Personalize   *bool   `json:"personalize"`
	Title         string  `json:"title"`

	MessageType string `json:"message_type"`
	Content     string `json:"content"`
	ParseMode   string `json:"parse_mode"`

	MediaURL    string `json:"media_url"`
	MediaFileID string `json:"media_file_id"`
	ContentMode string `json:"content_mode"`

	ButtonText string `json:"button_text"`
	ButtonURL  string `json:"button_url"`

	TargetType  string `json:"target_type"`
	TargetValue string `json:"target_value"`

	ScheduledAt *time.Time `json:"scheduled_at"`
}

func (s *Service) Create(
	req CreateRequest,
	createdBy string,
) (*model.PushCampaign, error) {
	if err := validateRepeat(req.RepeatSeconds, req.ScheduledAt); err != nil {
		return nil, err
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Category = strings.TrimSpace(req.Category)
	personalized := req.Personalize == nil || *req.Personalize
	req.Content = strings.TrimSpace(req.Content)

	req.MessageType = strings.ToLower(
		strings.TrimSpace(req.MessageType),
	)

	req.ParseMode = strings.TrimSpace(req.ParseMode)

	req.MediaURL = strings.TrimSpace(req.MediaURL)
	req.MediaFileID = strings.TrimSpace(req.MediaFileID)

	req.ContentMode = strings.ToLower(
		strings.TrimSpace(req.ContentMode),
	)

	req.ButtonText = strings.TrimSpace(req.ButtonText)
	req.ButtonURL = strings.TrimSpace(req.ButtonURL)

	req.TargetType = strings.TrimSpace(req.TargetType)
	req.TargetValue = strings.TrimSpace(req.TargetValue)

	if req.Title == "" {
		return nil, errors.New("title required")
	}

	if req.Content == "" {
		return nil, errors.New("content required")
	}

	if req.MessageType == "" {
		req.MessageType = MessageTypeText
	}

	switch req.MessageType {
	case MessageTypeText:
	case MessageTypePhoto:
	case MessageTypeVideo:
	default:
		return nil, errors.New(
			"message_type must be text, photo or video",
		)
	}

	if req.ParseMode == "" {
		req.ParseMode = "HTML"
	}

	if req.ParseMode != "HTML" {
		return nil, errors.New(
			"only HTML parse mode is supported",
		)
	}

	if req.ContentMode == "" {
		req.ContentMode = ContentModeAuto
	}

	switch req.ContentMode {
	case ContentModeAuto:
	case ContentModeCaption:
	case ContentModeSeparate:
	default:
		return nil, errors.New(
			"content_mode must be auto, caption or separate",
		)
	}

	if req.MessageType == MessageTypePhoto ||
		req.MessageType == MessageTypeVideo {

		if req.MediaURL == "" &&
			req.MediaFileID == "" {

			return nil, errors.New(
				"media_url or media_file_id required for photo/video",
			)
		}

		if req.MediaURL != "" &&
			req.MediaFileID != "" {

			return nil, errors.New(
				"media_url and media_file_id cannot both be set",
			)
		}
	}

	switch req.TargetType {
	case "all",
		"telegram_ids",
		"language":

	default:
		return nil, errors.New(
			"target_type must be all, telegram_ids or language",
		)
	}

	if req.TargetType != "all" &&
		req.TargetValue == "" {

		return nil, errors.New(
			"target_value required",
		)
	}

	if (req.ButtonText == "") !=
		(req.ButtonURL == "") {

		return nil, errors.New(
			"button_text and button_url must be used together",
		)
	}

	if req.TargetType == "telegram_ids" {
		if _, err := parseTargets(req.TargetValue); err != nil {
			return nil, err
		}
	}
	limit := 4096
	captionLimit := 1024
	if personalized {
		limit = 4000
		captionLimit = 928
	}
	if utf8.RuneCountInString(req.Content) > limit {
		return nil, errors.New("正文过长，请为自动称呼预留空间（启用称呼时最多 4000 字）")
	}
	if req.MessageType != "text" && req.ContentMode == ContentModeCaption && utf8.RuneCountInString(req.Content) > captionLimit {
		return nil, errors.New("媒体说明过长，请使用自动或独立正文模式")
	}
	if req.Category == "" {
		req.Category = req.MessageType
	}
	if utf8.RuneCountInString(req.Category) > 64 {
		return nil, errors.New("业务类型最多 64 字")
	}
	botID := uint64(0)
	if s.bots != nil {
		var err error
		botID, err = s.bots.Resolve(context.Background(), req.Category, req.BotID)
		if err != nil {
			return nil, err
		}
	} else if req.BotID != nil && *req.BotID != 0 {
		return nil, errors.New("business bots unavailable")
	}
	status := "queued"

	if req.ScheduledAt != nil &&
		req.ScheduledAt.After(time.Now()) {

		status = "scheduled"
	}

	row := &model.PushCampaign{
		RepeatSeconds: req.RepeatSeconds,
		BotID:         botID, Category: req.Category, Personalize: personalized,
		Title: req.Title,

		MessageType: req.MessageType,
		Content:     req.Content,
		ParseMode:   req.ParseMode,

		MediaURL:    req.MediaURL,
		MediaFileID: req.MediaFileID,
		ContentMode: req.ContentMode,

		ButtonText: req.ButtonText,
		ButtonURL:  req.ButtonURL,

		TargetType:  req.TargetType,
		TargetValue: req.TargetValue,

		Status: status,

		ScheduledAt: req.ScheduledAt,
		CreatedBy:   createdBy,
	}

	if err := s.st.DB.Create(row).Error; err != nil {
		return nil, err
	}

	return row, nil
}

func (s *Service) Cancel(id uint64) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.PushCampaign{}).Where("id=? AND status IN ?", id, []string{"queued", "scheduled", "processing", "paused"}).Update("status", "canceled")
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errors.New("campaign not cancelable")
		}
		return tx.Model(&model.PushRecipient{}).Where("campaign_id=? AND status='pending'", id).Update("status", "canceled").Error
	})
}

func (s *Service) RetryFailed(id uint64) error {
	return lease.Run(context.Background(), s.st.Redis, fmt.Sprintf("push:campaign:%d", id), func(context.Context) error { return s.retryFailed(id) })
}
func (s *Service) retryFailed(id uint64) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var c model.PushCampaign
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error; err != nil {
			return err
		}
		if c.Status != "completed" && c.Status != "canceled" {
			return errors.New("only completed or canceled campaigns can retry failed recipients")
		}
		// Unsent recipients from a canceled campaign stay canceled, including legacy rows.
		if c.Status == "canceled" {
			if err := tx.Model(&model.PushRecipient{}).Where("campaign_id=? AND status='pending'", id).Update("status", "canceled").Error; err != nil {
				return err
			}
		}
		res := tx.Model(&model.PushRecipient{}).Where("campaign_id=? AND status='failed'", id).Updates(map[string]any{"status": "pending", "attempts": 0, "last_error": ""})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return errors.New("no failed recipients")
		}
		return tx.Model(&c).Updates(map[string]any{"status": "processing", "completed_at": nil, "failed_count": 0, "repeat_seconds": 0}).Error
	})
}

func (s *Service) Run(
	ctx context.Context,
) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-ticker.C:
			if err := s.tick(ctx); err != nil {
				s.logger.Warn(
					"push worker tick failed",
					zap.Error(err),
				)
			}
		}
	}
}

func (s *Service) tick(
	ctx context.Context,
) error {
	var campaigns []model.PushCampaign

	now := time.Now()

	if err := s.st.DB.
		Where(
			"status IN ? AND (scheduled_at IS NULL OR scheduled_at <= ?)",
			[]string{
				"queued",
				"scheduled",
				"processing",
			},
			now,
		).
		Order("id asc").
		Limit(5).
		Find(&campaigns).
		Error; err != nil {

		return err
	}

	for i := range campaigns {
		if err := s.processCampaign(
			ctx,
			&campaigns[i],
		); err != nil {

			s.logger.Warn(
				"push campaign failed",
				zap.Uint64(
					"campaign_id",
					campaigns[i].ID,
				),
				zap.Error(err),
			)
		}
	}

	return nil
}

func (s *Service) processCampaign(ctx context.Context, c *model.PushCampaign) error {
	return lease.Run(ctx, s.st.Redis, fmt.Sprintf("push:campaign:%d", c.ID), func(ctx context.Context) error {
		if err := s.st.DB.First(c, c.ID).Error; err != nil {
			return err
		}
		return s.processClaimedCampaign(ctx, c)
	})
}

func (s *Service) processClaimedCampaign(ctx context.Context, c *model.PushCampaign) error {
	if c.Status == "queued" || c.Status == "scheduled" {
		if err := s.prepareRecipients(c); err != nil {
			s.st.DB.Model(&model.PushCampaign{}).Where("id=? AND status IN ?", c.ID, []string{"queued", "scheduled"}).Updates(map[string]any{"status": "failed", "last_error": err.Error()})
			return err
		}
	}
	if c.Status != "processing" {
		return nil
	}
	if s.bots != nil {
		if _, err := s.bots.Sender(ctx, c.BotID); err != nil {
			if pauseErr := s.Pause(c.ID); pauseErr != nil {
				return pauseErr
			}
			s.st.DB.Model(&model.PushCampaign{}).Where("id=? AND status='paused'", c.ID).Update("last_error", err.Error())
			return err
		}
	}
	var recips []model.PushRecipient
	if err := s.st.DB.Where("campaign_id=? AND status='pending' AND attempts < ?", c.ID, maxAttempts).Order("id asc").Limit(s.batch).Find(&recips).Error; err != nil {
		return err
	}
	if len(recips) == 0 {
		return s.finishIfDone(c.ID)
	}
	kb := buildKeyboard(c)
	for i := range recips {
		if err := ctx.Err(); err != nil {
			return err
		}
		var fresh model.PushCampaign
		if err := s.st.DB.Select("status").First(&fresh, c.ID).Error; err != nil {
			return err
		}
		if fresh.Status != "processing" {
			return s.recount(c.ID)
		}
		r := &recips[i]
		err := s.sendRecipient(ctx, r, c, kb)
		if e := s.recordDelivery(c.ID, r, err); e != nil {
			return e
		}

		timer := time.NewTimer(time.Second / time.Duration(s.rate))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return s.recount(c.ID)
}

// Serialize status recording with cancellation to avoid reviving canceled recipients.
func (s *Service) recordDelivery(campaignID uint64, r *model.PushRecipient, deliveryErr error) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var state model.PushCampaign
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&state, campaignID).Error; err != nil {
			return err
		}
		values := map[string]any{"attempts": r.Attempts + 1, "last_error": ""}
		if deliveryErr == nil {
			now := time.Now()
			values["status"] = "sent"
			values["sent_at"] = &now
		} else {
			status := "pending"
			if r.Attempts+1 >= maxAttempts {
				status = "failed"
			}
			if state.Status == "canceled" && status == "pending" {
				status = "canceled"
			}
			values["status"] = status
			values["last_error"] = truncate(deliveryErr.Error(), 2000)
		}
		return tx.Model(&model.PushRecipient{}).Where("id=?", r.ID).Updates(values).Error
	})
}

func (s *Service) sendRecipient(ctx context.Context, r *model.PushRecipient, c *model.PushCampaign, kb *telegram.InlineKeyboardMarkup) error {
	sender := s.tg
	if s.bots != nil {
		var err error
		sender, err = s.bots.Sender(ctx, c.BotID)
		if err != nil {
			return err
		}
	} else if c.BotID != 0 {
		return errors.New("configured business bot unavailable")
	}
	if c.Personalize {
		var u model.TelegramUser
		err := s.st.DB.Where("telegram_user_id=?", r.TelegramUserID).First(&u).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		copy := *c
		copy.Content = greeting(u) + "\n\n" + c.Content
		c = &copy
	}
	text := func() error {
		return sender.SendMessageWithParseMode(ctx, r.TelegramUserID, c.Content, c.ParseMode, kb)
	}
	if c.MessageType == "" || c.MessageType == MessageTypeText {
		return text()
	}
	var send func(context.Context, int64, string, string, string, *telegram.InlineKeyboardMarkup) error
	switch c.MessageType {
	case MessageTypePhoto:
		send = sender.SendPhoto
	case MessageTypeVideo:
		send = sender.SendVideo
	default:
		return errors.New("unsupported message type")
	}
	media := campaignMedia(c)
	if media == "" {
		return errors.New("media is empty")
	}
	if shouldUseCaption(c) {
		return send(ctx, r.TelegramUserID, media, c.Content, c.ParseMode, kb)
	}
	if !r.MediaSent {
		if err := send(ctx, r.TelegramUserID, media, "", "", nil); err != nil {
			return err
		}
		// Persist before attempting the second message so text retries do not resend media.
		if err := s.st.DB.Model(&model.PushRecipient{}).Where("id=?", r.ID).Update("media_sent", true).Error; err != nil {
			return err
		}
		r.MediaSent = true
	}
	return text()
}

func shouldUseCaption(
	c *model.PushCampaign,
) bool {
	switch c.ContentMode {
	case ContentModeCaption:
		return true

	case ContentModeSeparate:
		return false

	case "",
		ContentModeAuto:

		return utf8.RuneCountInString(
			c.Content,
		) <= safeCaptionRuneLimit

	default:
		return false
	}
}

func campaignMedia(
	c *model.PushCampaign,
) string {
	if strings.TrimSpace(
		c.MediaFileID,
	) != "" {

		return strings.TrimSpace(
			c.MediaFileID,
		)
	}

	return strings.TrimSpace(
		c.MediaURL,
	)
}

func buildKeyboard(
	c *model.PushCampaign,
) *telegram.InlineKeyboardMarkup {
	if strings.TrimSpace(
		c.ButtonText,
	) == "" ||
		strings.TrimSpace(
			c.ButtonURL,
		) == "" {

		return nil
	}

	k := telegram.InlineKeyboardMarkup{
		InlineKeyboard: [][]telegram.InlineKeyboardButton{
			{
				{
					Text: c.ButtonText,
					URL:  c.ButtonURL,
				},
			},
		},
	}

	return &k
}

func (s *Service) prepareRecipients(
	c *model.PushCampaign,
) error {
	var tgIDs []int64

	switch c.TargetType {
	case "all":
		if err := s.st.DB.
			Model(
				&model.TelegramUser{},
			).
			Where("status=1").
			Pluck(
				"telegram_user_id",
				&tgIDs,
			).
			Error; err != nil {

			return err
		}

	case "language":
		if err := s.st.DB.
			Model(
				&model.TelegramUser{},
			).
			Where(
				"status=1 AND language_code=?",
				strings.TrimSpace(
					c.TargetValue,
				),
			).
			Pluck(
				"telegram_user_id",
				&tgIDs,
			).
			Error; err != nil {

			return err
		}

	case "telegram_ids":
		var err error
		tgIDs, err = parseTargets(c.TargetValue)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf(
			"unsupported target type %s",
			c.TargetType,
		)
	}

	return s.st.DB.Transaction(
		func(tx *gorm.DB) error {
			var current model.PushCampaign
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, c.ID).Error; err != nil {
				return err
			}
			if current.Status != "queued" && current.Status != "scheduled" {
				*c = current
				return nil
			}
			if current.ScheduledAt != nil && current.ScheduledAt.After(time.Now()) {
				*c = current
				return nil
			}
			if len(tgIDs) > 0 {
				var subscribed []int64
				if err := tx.Model(&model.BotSubscription{}).Where("bot_id=? AND telegram_user_id IN ?", c.BotID, tgIDs).Pluck("telegram_user_id", &subscribed).Error; err != nil {
					return err
				}
				if c.TargetType == "telegram_ids" && len(subscribed) != len(tgIDs) {
					return errors.New("部分指定用户尚未与所选机器人建立会话，请先让用户打开该机器人并发送 /start")
				}
				tgIDs = subscribed
			}
			if len(tgIDs) == 0 {
				return errors.New("所选机器人暂无符合条件的订阅用户")
			}
			for _, id := range tgIDs {
				r := model.PushRecipient{
					CampaignID:     c.ID,
					TelegramUserID: id,
					Status:         "pending",
				}

				if err := tx.
					Where(
						"campaign_id=? AND telegram_user_id=?",
						c.ID,
						id,
					).
					FirstOrCreate(&r).
					Error; err != nil {

					return err
				}
			}

			now := time.Now()
			err := tx.Model(&model.PushCampaign{}).Where("id=?", c.ID).Updates(map[string]any{"total_count": len(tgIDs), "status": "processing", "started_at": &now}).Error
			if err == nil {
				c.Status = "processing"
			}
			return err
		},
	)
}

func (s *Service) finishIfDone(
	id uint64,
) error {
	var pending int64

	if err := s.st.DB.
		Model(
			&model.PushRecipient{},
		).
		Where(
			"campaign_id=? AND status='pending'",
			id,
		).
		Count(&pending).
		Error; err != nil {

		return err
	}

	if pending > 0 {
		return nil
	}

	if err := s.recount(id); err != nil {
		return err
	}

	var c model.PushCampaign

	if err := s.st.DB.
		First(
			&c,
			id,
		).
		Error; err != nil {

		return err
	}

	if c.Status == "canceled" {
		return nil
	}

	now := time.Now()
	if c.RepeatSeconds > 0 {
		return s.finishRecurring(id, now)
	}

	return s.st.DB.
		Model(&c).
		Where("status='processing'").
		Updates(
			map[string]any{
				"status":       "completed",
				"completed_at": &now,
			},
		).
		Error
}

func (s *Service) recount(
	id uint64,
) error {
	var sent int64
	var failed int64

	if err := s.st.DB.
		Model(
			&model.PushRecipient{},
		).
		Where(
			"campaign_id=? AND status='sent'",
			id,
		).
		Count(&sent).
		Error; err != nil {

		return err
	}

	if err := s.st.DB.
		Model(
			&model.PushRecipient{},
		).
		Where(
			"campaign_id=? AND status='failed'",
			id,
		).
		Count(&failed).
		Error; err != nil {

		return err
	}

	return s.st.DB.
		Model(
			&model.PushCampaign{},
		).
		Where(
			"id=?",
			id,
		).
		Updates(
			map[string]any{
				"sent_count":   sent,
				"failed_count": failed,
			},
		).
		Error
}

func (s *Service) List(
	limit int,
) ([]model.PushCampaign, error) {
	if limit <= 0 ||
		limit > 500 {

		limit = 200
	}

	var rows []model.PushCampaign

	err := s.st.DB.
		Order("CASE WHEN status IN ('scheduled','queued','processing','paused') THEN 0 ELSE 1 END").
		Order("id desc").
		Limit(limit).
		Find(&rows).
		Error

	return rows, err
}

func (s *Service) Recipients(
	id uint64,
	limit int,
) ([]model.PushRecipient, error) {
	if limit <= 0 ||
		limit > 1000 {

		limit = 500
	}

	var rows []model.PushRecipient

	err := s.st.DB.
		Where(
			"campaign_id=?",
			id,
		).
		Order("id desc").
		Limit(limit).
		Find(&rows).
		Error

	return rows, err
}

func splitTargets(
	v string,
) []string {
	var jsonIDs []json.RawMessage

	if json.Unmarshal(
		[]byte(v),
		&jsonIDs,
	) == nil {

		out := make(
			[]string,
			0,
			len(jsonIDs),
		)

		for _, x := range jsonIDs {
			out = append(
				out,
				strings.Trim(string(x), `"`),
			)
		}

		return out
	}

	return strings.FieldsFunc(
		v,
		func(r rune) bool {
			return r == ',' ||
				r == '\n' ||
				r == ';' ||
				r == ' ' ||
				r == '\t'
		},
	)
}

func truncate(
	s string,
	n int,
) string {
	if len(s) <= n {
		return s
	}

	for n > 0 && !utf8.ValidString(s[:n]) {
		n--
	}
	return s[:n]
}

func parseTargets(v string) ([]int64, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, p := range splitTargets(v) {
		id, err := strconv.ParseInt(p, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid Telegram user ID: %s", p)
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("at least one Telegram user ID required")
	}
	return ids, nil
}

func (s *Service) WithBots(bots BotResolver) *Service { s.bots = bots; return s }
func greeting(u model.TelegramUser) string {
	name := strings.TrimSpace(strings.Join([]string{u.FirstName, u.LastName}, " "))
	if name == "" {
		name = strings.TrimPrefix(strings.TrimSpace(u.Username), "@")
	}
	if name == "" {
		name = "朋友"
	}
	name = strings.Join(strings.Fields(name), " ")
	runes := []rune(name)
	if len(runes) > 64 {
		name = string(runes[:64])
	}
	return "亲爱的" + html.EscapeString(name) + ":"
}

func (s *Service) Pause(id uint64) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var c model.PushCampaign
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error; err != nil {
			return err
		}
		if c.Status == "paused" {
			return nil
		}
		if c.Status != "queued" && c.Status != "scheduled" && c.Status != "processing" {
			return errors.New("当前任务不能暂停")
		}
		return tx.Model(&c).Updates(map[string]any{"paused_from": c.Status, "status": "paused"}).Error
	})
}
func (s *Service) Resume(id uint64) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var c model.PushCampaign
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error; err != nil {
			return err
		}
		if c.Status != "paused" {
			return errors.New("任务未暂停")
		}
		if s.bots != nil {
			if _, err := s.bots.Sender(context.Background(), c.BotID); err != nil {
				return err
			}
		}
		return tx.Model(&c).Updates(resumeUpdates(c, time.Now())).Error
	})
}
func resumeStatus(c model.PushCampaign, now time.Time) string {
	if c.PausedFrom == "processing" {
		return "processing"
	}
	if c.ScheduledAt != nil && c.ScheduledAt.After(now) {
		return "scheduled"
	}
	return "queued"
}
func (s *Service) Delete(ctx context.Context, id uint64) error {
	return lease.Run(ctx, s.st.Redis, fmt.Sprintf("push:campaign:%d", id), func(context.Context) error { return s.deleteCampaign(id) })
}
func (s *Service) deleteCampaign(id uint64) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var c model.PushCampaign
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error; err != nil {
			return err
		}
		if c.Status != "completed" && c.Status != "canceled" && c.Status != "failed" {
			return errors.New("请先取消任务，或等待任务结束后删除")
		}
		if err := tx.Where("campaign_id=?", id).Delete(&model.PushRecipient{}).Error; err != nil {
			return err
		}
		return tx.Delete(&c).Error
	})
}
