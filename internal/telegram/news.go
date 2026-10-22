package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"telegram-business-bot/internal/lease"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/news"
)

// A news bot (business bot purpose "news") lets users browse, search and subscribe to news in
// its private chat. It also records the groups and channels it is added to (for pushing news,
// and channels as sources) and turns posts of source channels into articles.

const newsPageSize = 8

// WithNewsOnly makes this a news bot.
func (r *Router) WithNewsOnly(s *news.Service) *Router {
	r.news, r.menuButton = s, MenuButton{}
	return r
}

const newsHomeText = "<b>📰 新闻资讯</b>\n\n" +
	"• 最新新闻：按时间浏览，也可以按分类看\n" +
	"• 订阅：选择感兴趣的分类，有新闻会第一时间推送给你\n" +
	"• 搜索：直接发送关键词即可"

func newsHomeKeyboard() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{btn("📰 最新新闻", "nw:l:0:1"), btn("🗂 分类浏览", "nw:c")},
		{btn("🔔 订阅管理", "nw:s")},
	}}
}

// handleNewsUpdate handles the updates of a news bot that do not come from a user: channel
// posts and changes of the bot's membership. It reports whether u was one of them.
func (r *Router) handleNewsUpdate(ctx context.Context, u Update) bool {
	if r.news == nil {
		return false
	}
	switch {
	case u.ChannelPost != nil:
		p := u.ChannelPost
		text := p.Text
		if text == "" {
			text = p.Caption
		}
		a, err := r.news.IngestChannelPost(news.ChannelPost{BotID: r.botID, ChatID: p.Chat.ID, Title: p.Chat.Title, Username: p.Chat.Username, MessageID: p.MessageID, Text: text})
		if err != nil {
			r.logger.Error("ingest channel post failed", zap.Int64("chat_id", p.Chat.ID), zap.Error(err))
		} else if a != nil {
			r.logger.Info("channel post saved as news", zap.Uint64("article_id", a.ID), zap.String("status", a.Status))
		}
		return true
	case u.MyChatMember != nil:
		m := u.MyChatMember
		if m.Chat.Type == "private" {
			// The user blocked the bot: stop pushing to them.
			if m.NewChatMember.Status == "kicked" {
				_ = r.news.Unsubscribe(r.botID, m.Chat.ID)
			}
			return true
		}
		if err := r.news.RecordChat(r.botID, news.ChatInfo{ChatID: m.Chat.ID, Type: m.Chat.Type, Title: m.Chat.Title, Username: m.Chat.Username, Status: m.NewChatMember.Status}); err != nil {
			r.logger.Error("record news chat failed", zap.Int64("chat_id", m.Chat.ID), zap.Error(err))
		}
		return true
	}
	return false
}

// NewsMessage renders an article as a message: HTML text (or a photo caption) and a button to
// the full story.
func NewsMessage(a *model.NewsArticle, category string) (text, photo string, kb *InlineKeyboardMarkup) {
	limit := 3000
	if a.ImageURL != "" {
		photo, limit = a.ImageURL, 700 // captions are limited to 1024 characters
	}
	body := strings.TrimSpace(a.Content)
	if utf8.RuneCountInString(body) > limit {
		body = string([]rune(body)[:limit]) + "…"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📰 <b>%s</b>", htmlEscape(a.Title))
	if body != "" {
		b.WriteString("\n\n" + htmlEscape(body))
	}
	var meta []string
	if category != "" {
		meta = append(meta, "🏷 "+htmlEscape(category))
	}
	if a.PublishedAt != nil {
		meta = append(meta, a.PublishedAt.Format("01-02 15:04"))
	}
	if len(meta) > 0 {
		b.WriteString("\n\n" + strings.Join(meta, " · "))
	}
	if a.LinkURL != "" {
		kb = &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "阅读全文", URL: a.LinkURL}}}}
	}
	return b.String(), photo, kb
}

func (r *Router) newsList(ctx context.Context, chatID int64, msgID int, categoryID uint64, page int) {
	list, more, err := r.news.Latest(categoryID, page, newsPageSize)
	if err != nil {
		return
	}
	title := "最新新闻"
	if categoryID != 0 {
		title = r.news.CategoryName(categoryID)
	}
	r.showArticleList(ctx, chatID, msgID, fmt.Sprintf("<b>📰 %s</b>（第 %d 页）", htmlEscape(title), page), list, func(rows [][]InlineKeyboardButton) [][]InlineKeyboardButton {
		var nav []InlineKeyboardButton
		if page > 1 {
			nav = append(nav, btn("⬅ 上一页", fmt.Sprintf("nw:l:%d:%d", categoryID, page-1)))
		}
		if more {
			nav = append(nav, btn("下一页 ➡", fmt.Sprintf("nw:l:%d:%d", categoryID, page+1)))
		}
		if len(nav) > 0 {
			rows = append(rows, nav)
		}
		return rows
	})
}

func (r *Router) showArticleList(ctx context.Context, chatID int64, msgID int, title string, list []model.NewsArticle, extra func([][]InlineKeyboardButton) [][]InlineKeyboardButton) {
	var rows [][]InlineKeyboardButton
	for _, a := range list {
		label := a.Title
		if utf8.RuneCountInString(label) > 40 {
			label = string([]rune(label)[:40]) + "…"
		}
		rows = append(rows, []InlineKeyboardButton{btn(label, fmt.Sprintf("nw:a:%d", a.ID))})
	}
	if extra != nil {
		rows = extra(rows)
	}
	rows = append(rows, []InlineKeyboardButton{btn("⬅ 返回", "home")})
	text := title
	if len(list) == 0 {
		text += "\n\n暂时没有新闻。"
	} else {
		text += "\n\n点击标题查看详情："
	}
	kb := &InlineKeyboardMarkup{InlineKeyboard: rows}
	if msgID != 0 {
		_ = r.client.EditMessage(ctx, chatID, msgID, text, kb)
	} else {
		_ = r.client.SendMessage(ctx, chatID, text, kb)
	}
}

func (r *Router) showArticle(ctx context.Context, chatID int64, id uint64) {
	a, err := r.news.Article(id)
	if err != nil {
		_ = r.client.SendMessage(ctx, chatID, "这条新闻已下线。", nil)
		return
	}
	text, photo, kb := NewsMessage(a, r.news.CategoryName(a.CategoryID))
	if kb == nil {
		kb = &InlineKeyboardMarkup{}
	}
	kb.InlineKeyboard = append(kb.InlineKeyboard, []InlineKeyboardButton{btn("📰 更多新闻", "nw:l:0:1")})
	if photo != "" && r.client.SendPhoto(ctx, chatID, photo, text, "HTML", kb) == nil {
		return
	}
	_ = r.client.SendMessage(ctx, chatID, text, kb)
}

func (r *Router) newsCategories(ctx context.Context, chatID int64, msgID int) {
	cats, err := r.news.Categories(true)
	if err != nil {
		return
	}
	rows := [][]InlineKeyboardButton{}
	for i, c := range cats {
		b := btn(c.Name, fmt.Sprintf("nw:l:%d:1", c.ID))
		if i%2 == 1 {
			rows[len(rows)-1] = append(rows[len(rows)-1], b)
		} else {
			rows = append(rows, []InlineKeyboardButton{b})
		}
	}
	rows = append(rows, []InlineKeyboardButton{btn("⬅ 返回", "home")})
	text := "<b>🗂 新闻分类</b>\n\n请选择分类："
	if len(cats) == 0 {
		text = "<b>🗂 新闻分类</b>\n\n暂无分类。"
	}
	_ = r.client.EditMessage(ctx, chatID, msgID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

func (r *Router) newsSubscriptions(ctx context.Context, chatID int64, msgID int, userID int64) {
	cats, err := r.news.Categories(true)
	if err != nil {
		return
	}
	subs, err := r.news.Subscribed(r.botID, userID)
	if err != nil {
		return
	}
	mark := func(on bool) string {
		if on {
			return "✅ "
		}
		return "⬜ "
	}
	rows := [][]InlineKeyboardButton{{btn(mark(subs[0])+"全部分类", "nw:t:0")}}
	for _, c := range cats {
		rows = append(rows, []InlineKeyboardButton{btn(mark(subs[c.ID])+c.Name, fmt.Sprintf("nw:t:%d", c.ID))})
	}
	rows = append(rows, []InlineKeyboardButton{btn("⬅ 返回", "home")})
	text := "<b>🔔 订阅管理</b>\n\n点击分类订阅或取消。订阅「全部分类」会收到所有新闻。"
	_ = r.client.EditMessage(ctx, chatID, msgID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
}

// handleNewsCallback handles the "nw:" buttons of the private chat.
func (r *Router) handleNewsCallback(ctx context.Context, cb *CallbackQuery) {
	chatID, msgID, userID := cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID
	parts := strings.Split(cb.Data, ":")
	switch {
	case len(parts) == 4 && parts[1] == "l":
		cat, _ := strconv.ParseUint(parts[2], 10, 64)
		page, _ := strconv.Atoi(parts[3])
		r.newsList(ctx, chatID, msgID, cat, page)
	case len(parts) == 3 && parts[1] == "a":
		id, _ := strconv.ParseUint(parts[2], 10, 64)
		r.showArticle(ctx, chatID, id)
	case cb.Data == "nw:c":
		r.newsCategories(ctx, chatID, msgID)
	case cb.Data == "nw:s":
		r.newsSubscriptions(ctx, chatID, msgID, userID)
	case len(parts) == 3 && parts[1] == "t":
		cat, _ := strconv.ParseUint(parts[2], 10, 64)
		if _, err := r.news.ToggleSubscription(r.botID, userID, cat); err != nil {
			r.logger.Error("toggle news subscription failed", zap.Error(err))
			return
		}
		r.newsSubscriptions(ctx, chatID, msgID, userID)
	}
}

// handleNewsText handles a private-chat message of a news bot: commands, or a search.
func (r *Router) handleNewsText(ctx context.Context, m *Message) {
	text := strings.TrimSpace(m.Text)
	switch commandName(text) {
	case "/news":
		r.newsList(ctx, m.Chat.ID, 0, 0, 1)
		return
	case "/subscribe":
		sent, err := r.client.SendMessageID(ctx, m.Chat.ID, "<b>🔔 订阅管理</b>", nil)
		if err == nil {
			r.newsSubscriptions(ctx, m.Chat.ID, sent, m.From.ID)
		}
		return
	}
	if text == "" || strings.HasPrefix(text, "/") {
		r.sendHome(ctx, m.Chat.ID, m.From.ID)
		return
	}
	list, err := r.news.Search(text, 10)
	if err != nil {
		return
	}
	r.showArticleList(ctx, m.Chat.ID, 0, "<b>🔍 搜索「"+htmlEscape(text)+"」</b>", list, nil)
}

// copier is the part of Client that copies channel posts.
type copier interface {
	CopyMessage(ctx context.Context, chatID, fromChatID int64, messageID int, kb *InlineKeyboardMarkup) error
}

// RunNewsDelivery sends queued news deliveries through the news bots until ctx ends, on one
// replica at a time.
func RunNewsDelivery(ctx context.Context, svc *news.Service, rdb lease.Client, sender func(context.Context, uint64) (Sender, error), logger *zap.Logger) {
	for ctx.Err() == nil {
		err := lease.Run(ctx, rdb, "news:deliver", func(ctx context.Context) error {
			for ctx.Err() == nil {
				if n := DeliverNews(ctx, svc, sender, logger); n == 0 {
					select {
					case <-ctx.Done():
					case <-time.After(5 * time.Second):
					}
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, lease.ErrBusy) && ctx.Err() == nil {
			logger.Warn("news delivery lease failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
}

// DeliverNews sends one batch of queued deliveries and returns how many it handled.
func DeliverNews(ctx context.Context, svc *news.Service, sender func(context.Context, uint64) (Sender, error), logger *zap.Logger) int {
	list, err := svc.PendingDeliveries(100)
	if err != nil {
		logger.Error("load news deliveries failed", zap.Error(err))
		return 0
	}
	articles := map[uint64]*model.NewsArticle{}
	categories := map[uint64]string{}
	for _, d := range list {
		if ctx.Err() != nil {
			break
		}
		if ok, err := svc.ClaimDelivery(d.ID); err != nil || !ok {
			continue
		}
		a := articles[d.ArticleID]
		if a == nil {
			if a, err = svc.ArticleByID(d.ArticleID); err != nil {
				_ = svc.FinishDelivery(d.ID, err)
				continue
			}
			articles[d.ArticleID] = a
		}
		if _, ok := categories[a.CategoryID]; !ok {
			categories[a.CategoryID] = svc.CategoryName(a.CategoryID)
		}
		s, err := sender(ctx, d.BotID)
		if err == nil {
			err = sendNews(ctx, s, d, a, categories[a.CategoryID])
		}
		if err != nil && d.ChatID > 0 && (strings.Contains(err.Error(), "blocked") || strings.Contains(err.Error(), "deactivated")) {
			_ = svc.Unsubscribe(d.BotID, d.ChatID)
		}
		_ = svc.FinishDelivery(d.ID, err)
		time.Sleep(40 * time.Millisecond) // stay well below Telegram's 30 messages per second
	}
	return len(list)
}

func sendNews(ctx context.Context, s Sender, d model.NewsDelivery, a *model.NewsArticle, category string) error {
	text, photo, kb := NewsMessage(a, category)
	// A channel post is copied as is (with its media) by the bot that can read the channel.
	if a.ChannelChat != 0 && a.ChannelBotID == d.BotID {
		if c, ok := s.(copier); ok {
			if err := c.CopyMessage(ctx, d.ChatID, a.ChannelChat, a.ChannelMsgID, kb); err == nil {
				return nil
			}
		}
	}
	if photo != "" {
		if err := s.SendPhoto(ctx, d.ChatID, photo, text, "HTML", kb); err == nil {
			return nil
		}
	}
	return s.SendMessageWithParseMode(ctx, d.ChatID, text, "HTML", kb)
}
