package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/news"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"
)

type recordedSend struct {
	bot    uint64
	chat   int64
	text   string
	copied bool
}

type newsSender struct {
	mu   sync.Mutex
	bot  uint64
	sent *[]recordedSend
	fail map[int64]string
}

func (s *newsSender) record(chat int64, text string, copied bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg := s.fail[chat]; msg != "" {
		return fmt.Errorf("%s", msg)
	}
	*s.sent = append(*s.sent, recordedSend{s.bot, chat, text, copied})
	return nil
}
func (s *newsSender) SendMessageWithParseMode(_ context.Context, chat int64, text, _ string, _ *telegram.InlineKeyboardMarkup) error {
	return s.record(chat, text, false)
}
func (s *newsSender) SendPhoto(_ context.Context, chat int64, _, caption, _ string, _ *telegram.InlineKeyboardMarkup) error {
	return s.record(chat, caption, false)
}
func (s *newsSender) SendVideo(context.Context, int64, string, string, string, *telegram.InlineKeyboardMarkup) error {
	return nil
}
func (s *newsSender) CopyMessage(_ context.Context, chat, _ int64, _ int, _ *telegram.InlineKeyboardMarkup) error {
	return s.record(chat, "copy", true)
}

func TestNewsPublishingFanoutAndDelivery(t *testing.T) {
	db := testDatabase(t)
	feedBody := map[string]string{}
	svc := news.New(&store.Store{DB: db}, nil).WithFetcher(func(_ context.Context, url string) ([]byte, error) { return []byte(feedBody[url]), nil })
	bot := model.BusinessBot{Name: "news", TelegramBotID: 555, TokenEncrypted: "x", Enabled: true, Purpose: "news"}
	other := model.BusinessBot{Name: "shop", TelegramBotID: 556, TokenEncrypted: "y", Enabled: true, Purpose: "full"}
	must(t, db.Create(&bot).Error)
	must(t, db.Create(&other).Error)
	local, fx := model.NewsCategory{Name: "本地", Enabled: true}, model.NewsCategory{Name: "汇率", Enabled: true}
	must(t, db.Create(&local).Error)
	must(t, db.Create(&fx).Error)

	// Subscriptions: one user to everything, one to 汇率 only, one who blocked the bot.
	for _, s := range []struct {
		user int64
		cat  uint64
	}{{1001, 0}, {1002, fx.ID}, {1003, 0}} {
		if on, err := svc.ToggleSubscription(bot.ID, s.user, s.cat); err != nil || !on {
			t.Fatal(on, err)
		}
	}
	if on, _ := svc.ToggleSubscription(bot.ID, 1002, local.ID); !on {
		t.Fatal("toggle on")
	}
	if on, _ := svc.ToggleSubscription(bot.ID, 1002, local.ID); on {
		t.Fatal("toggle off")
	}
	// A group with pushing on for 本地, a channel used as a source, a group the bot left.
	must(t, svc.RecordChat(bot.ID, news.ChatInfo{ChatID: -100, Type: "supergroup", Title: "本地群", Status: "member"}))
	must(t, svc.RecordChat(bot.ID, news.ChatInfo{ChatID: -200, Type: "channel", Title: "源频道", Username: "srcchan", Status: "administrator"}))
	must(t, svc.RecordChat(bot.ID, news.ChatInfo{ChatID: -300, Type: "group", Title: "旧群", Status: "member"}))
	must(t, svc.RecordChat(bot.ID, news.ChatInfo{ChatID: -300, Type: "group", Title: "旧群", Status: "kicked"}))
	must(t, db.Model(&model.NewsChat{}).Where("chat_id IN ?", []int64{-100, -300}).Updates(map[string]any{"push_enabled": true, "push_categories": fmt.Sprint(local.ID)}).Error)
	must(t, db.Model(&model.NewsChat{}).Where("chat_id = ?", -200).Updates(map[string]any{"source_enabled": true, "source_category_id": local.ID, "auto_publish": true, "push_enabled": true}).Error)

	// A console article for 本地, published now.
	now := time.Now()
	manual := model.NewsArticle{CategoryID: local.ID, Title: "本地 <头条>", Content: "正文", Source: news.SourceManual, Status: news.StatusPublished, PublishedAt: &now, Push: true}
	must(t, db.Create(&manual).Error)
	// A scheduled one that is due, for 汇率.
	past := now.Add(-time.Minute)
	sched := model.NewsArticle{CategoryID: fx.ID, Title: "汇率早报", Source: news.SourceManual, Status: news.StatusScheduled, PublishAt: &past, Push: true}
	must(t, db.Create(&sched).Error)
	// A channel post becomes a published article; the same post again is ignored.
	post := news.ChannelPost{BotID: bot.ID, ChatID: -200, Title: "源频道", Username: "srcchan", MessageID: 7, Text: "频道快讯\n详情"}
	a, err := svc.IngestChannelPost(post)
	if err != nil || a == nil || a.Title != "频道快讯" || a.LinkURL != "https://t.me/srcchan/7" || a.Status != news.StatusPublished {
		t.Fatalf("%+v %v", a, err)
	}
	if again, err := svc.IngestChannelPost(post); err != nil || again != nil {
		t.Fatal("duplicate channel post stored", again, err)
	}
	if n, _ := svc.IngestChannelPost(news.ChannelPost{BotID: bot.ID, ChatID: -100, MessageID: 1, Text: "群消息"}); n != nil {
		t.Fatal("non-source chat ingested")
	}

	svc.Tick(context.Background())
	var sent []recordedSend
	sender := &newsSender{bot: bot.ID, sent: &sent, fail: map[int64]string{1003: "telegram api error 403: Forbidden: bot was blocked by the user"}}
	lookup := func(_ context.Context, id uint64) (telegram.Sender, error) { return sender, nil }
	for telegram.DeliverNews(context.Background(), svc, lookup, zap.NewNop()) > 0 {
	}
	got := map[string]bool{}
	for _, s := range sent {
		key := fmt.Sprintf("%d", s.chat)
		switch {
		case strings.Contains(s.text, "本地 &lt;头条&gt;"):
			key += ":manual"
		case strings.Contains(s.text, "汇率早报"):
			key += ":sched"
		case s.copied:
			key += ":copy"
		}
		got[key] = true
	}
	// The source channel gets other news (pushing is on) but never its own post back.
	want := []string{"1001:manual", "1001:sched", "1001:copy", "1002:sched", "-100:manual", "-100:copy", "-200:manual", "-200:sched"}
	for _, w := range want {
		if !got[w] {
			t.Fatalf("missing %s in %v", w, got)
		}
	}
	if len(sent) != len(want) {
		t.Fatalf("sent %d messages, want %d: %v", len(sent), len(want), got)
	}
	var subs int64
	db.Model(&model.NewsSubscription{}).Where("telegram_user_id = ?", 1003).Count(&subs)
	if subs != 0 {
		t.Fatal("blocked user still subscribed")
	}
	// Fan-out happens once per article.
	svc.Tick(context.Background())
	if telegram.DeliverNews(context.Background(), svc, lookup, zap.NewNop()) != 0 {
		t.Fatal("articles delivered twice")
	}

	// RSS: the first fetch archives existing items without pushing; later items follow the
	// feed's setting (review first here).
	url := "https://feed.example/rss"
	item := func(id string) string {
		return `<item><title>Item ` + id + `</title><link>https://feed.example/` + id + `</link><guid>` + id + `</guid><description>text</description></item>`
	}
	feedBody[url] = `<rss version="2.0"><channel>` + item("1") + item("2") + `</channel></rss>`
	feed := model.NewsFeed{Name: "demo", URL: url, CategoryID: local.ID, Interval: 30, Enabled: true}
	must(t, db.Create(&feed).Error)
	if n, err := svc.FetchFeed(context.Background(), feed); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	must(t, db.First(&feed, feed.ID).Error)
	feedBody[url] = `<rss version="2.0"><channel>` + item("3") + item("2") + item("1") + `</channel></rss>`
	if n, err := svc.FetchFeed(context.Background(), feed); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var states []string
	db.Model(&model.NewsArticle{}).Where("source = ?", news.SourceRSS).Order("id").Pluck("CONCAT(title, ':', status, ':', push, ':', dispatched_at IS NOT NULL)", &states)
	if strings.Join(states, ",") != "Item 2:published:0:1,Item 1:published:0:1,Item 3:pending:1:0" {
		t.Fatal(states)
	}
	list, more, err := svc.Latest(0, 1, 2)
	if err != nil || len(list) != 2 || !more {
		t.Fatal(len(list), more, err)
	}
	found, err := svc.Search("快讯", 10)
	if err != nil || len(found) != 1 {
		t.Fatal(found, err)
	}
}
