package push

import (
	"context"
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
	"time"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/testutil"

	"gorm.io/gorm"
)

func TestPersonalizedGreeting(t *testing.T) {
	for _, v := range []struct {
		user model.TelegramUser
		want string
	}{{model.TelegramUser{FirstName: "Redis"}, "亲爱的Redis:"}, {model.TelegramUser{Username: "Redis"}, "亲爱的Redis:"}, {model.TelegramUser{}, "亲爱的朋友:"}, {model.TelegramUser{FirstName: "<b>Redis</b>"}, "亲爱的&lt;b&gt;Redis&lt;/b&gt;:"}, {model.TelegramUser{FirstName: "Ada", LastName: "Lovelace"}, "亲爱的Ada Lovelace:"}} {
		if got := greeting(v.user); got != v.want {
			t.Fatalf("got %s want %s", got, v.want)
		}
	}
}

type recordingSender struct{ texts []string }

func (r *recordingSender) SendMessageWithParseMode(_ context.Context, _ int64, text, _ string, _ *telegram.InlineKeyboardMarkup) error {
	r.texts = append(r.texts, text)
	return nil
}
func (r *recordingSender) SendPhoto(_ context.Context, _ int64, _, text, _ string, _ *telegram.InlineKeyboardMarkup) error {
	r.texts = append(r.texts, text)
	return nil
}
func (r *recordingSender) SendVideo(_ context.Context, _ int64, _, text, _ string, _ *telegram.InlineKeyboardMarkup) error {
	r.texts = append(r.texts, text)
	return nil
}

type resolver struct {
	sender   Sender
	botID    uint64
	category string
}

func (r *resolver) Sender(_ context.Context, id uint64) (telegram.Sender, error) {
	r.botID = id
	return r.sender, nil
}
func (r *resolver) Resolve(_ context.Context, category string, override *uint64) (uint64, error) {
	r.category = category
	return 7, nil
}
func TestIndividualGreetingsDoNotMutateSharedCampaign(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "telegram_user_id=?", Columns: []string{"first_name"}, Rows: [][]driver.Value{{"Redis"}}},
		testutil.Step{Contains: "telegram_user_id=?", Columns: []string{"first_name"}, Rows: [][]driver.Value{{"Trump"}}},
	)
	sender := &recordingSender{}
	s := New(&store.Store{DB: db}, sender, nil)
	c := &model.PushCampaign{Content: "新的岗位已发布", Personalize: true, MessageType: "text"}
	for _, id := range []int64{1, 2} {
		if err := s.sendRecipient(context.Background(), &model.PushRecipient{TelegramUserID: id}, c, nil); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(sender.texts, []string{"亲爱的Redis:\n\n新的岗位已发布", "亲爱的Trump:\n\n新的岗位已发布"}) {
		t.Fatal(sender.texts)
	}
	if c.Content != "新的岗位已发布" {
		t.Fatal("shared campaign modified")
	}
}
func TestDeliveryUsesSelectedBusinessBot(t *testing.T) {
	primary, other := &recordingSender{}, &recordingSender{}
	r := &resolver{sender: other}
	s := New(nil, primary, nil).WithBots(r)
	c := &model.PushCampaign{BotID: 7, Content: "test", MessageType: "text"}
	if err := s.sendRecipient(context.Background(), &model.PushRecipient{TelegramUserID: 1}, c, nil); err != nil {
		t.Fatal(err)
	}
	if len(primary.texts) != 0 || len(other.texts) != 1 || r.botID != 7 {
		t.Fatal("wrong sender")
	}
}
func TestCreateSnapshotsRouteAndDefaultsToGreeting(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "INSERT INTO `push_campaigns`", Affected: 1})
	db.Callback().Create().Before("gorm:create").Register("test:route", func(tx *gorm.DB) {
		c := tx.Statement.Dest.(*model.PushCampaign)
		if c.BotID != 7 || !c.Personalize {
			t.Error("routing/greeting not persisted")
		}
	})
	r := &resolver{}
	s := New(&store.Store{DB: db}, nil, nil).WithBots(r)
	_, err := s.Create(CreateRequest{Title: "岗位", Category: "招聘通知", Content: "正文", TargetType: "all"}, "admin")
	if err != nil || r.category != "招聘通知" {
		t.Fatalf("%v", err)
	}
}
func TestResumeKeepsFutureScheduleAndContinuesPartialTask(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	for _, v := range []struct {
		from      string
		scheduled *time.Time
		want      string
	}{{"scheduled", &future, "scheduled"}, {"scheduled", &past, "queued"}, {"queued", nil, "queued"}, {"processing", &past, "processing"}} {
		if got := resumeStatus(model.PushCampaign{PausedFrom: v.from, ScheduledAt: v.scheduled}, now); got != v.want {
			t.Fatalf("%s", got)
		}
	}
}
func TestPauseRecordsPreviousStateWithoutCancelingRecipients(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "scheduled"}}}, testutil.Step{Contains: "UPDATE `push_campaigns`", Affected: 1})
	db.Callback().Update().Before("gorm:update").Register("test:pause", func(tx *gorm.DB) {
		v := tx.Statement.Dest.(map[string]any)
		if v["status"] != "paused" || v["paused_from"] != "scheduled" {
			t.Error(v)
		}
	})
	if err := New(&store.Store{DB: db}, nil, nil).Pause(1); err != nil {
		t.Fatal(err)
	}
}
func TestPausedCampaignCannotSendFromStaleSnapshot(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "paused"}}})
	c := &model.PushCampaign{ID: 1, Status: "queued", TargetType: "telegram_ids", TargetValue: "123"}
	if err := New(&store.Store{DB: db}, nil, nil).processClaimedCampaign(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}
func TestDeleteFinishedTaskDeletesRecipientsFirst(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "completed"}}}, testutil.Step{Contains: "DELETE FROM `push_recipients`", Affected: 2}, testutil.Step{Contains: "DELETE FROM `push_campaigns`", Affected: 1})
	if err := New(&store.Store{DB: db}, nil, nil).deleteCampaign(1); err != nil {
		t.Fatal(err)
	}
}
func TestCannotDeleteActiveOrPausedTask(t *testing.T) {
	for _, status := range []string{"processing", "scheduled", "queued", "paused"} {
		db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), status}}})
		if err := New(&store.Store{DB: db}, nil, nil).deleteCampaign(1); err == nil {
			t.Fatal("active task deleted")
		}
	}
}
func TestExplicitRecipientsMustHaveStartedSelectedBot(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "queued"}}},
		testutil.Step{Contains: "bot_id=? AND telegram_user_id IN", Columns: []string{"telegram_user_id"}, Rows: [][]driver.Value{{int64(123)}}},
	)
	c := &model.PushCampaign{ID: 1, BotID: 7, Status: "queued", TargetType: "telegram_ids", TargetValue: "123,456"}
	err := New(&store.Store{DB: db}, nil, nil).prepareRecipients(c)
	if err == nil || !strings.Contains(err.Error(), "建立会话") {
		t.Fatal("non-subscribers accepted", err)
	}
}

func TestPersonalizedMediaCaptionUsesCorrectGreeting(t *testing.T) {
	for _, kind := range []string{"photo", "video"} {
		t.Run(kind, func(t *testing.T) {
			db := testutil.DB(t, testutil.Step{Contains: "telegram_users", Columns: []string{"first_name"}, Rows: [][]driver.Value{{"Redis"}}})
			sender := &recordingSender{}
			s := New(&store.Store{DB: db}, sender, nil)
			c := &model.PushCampaign{MessageType: kind, Content: "图片/视频说明", Personalize: true, MediaFileID: "file", ContentMode: "auto"}
			if err := s.sendRecipient(context.Background(), &model.PushRecipient{TelegramUserID: 123}, c, nil); err != nil {
				t.Fatal(err)
			}
			if len(sender.texts) != 1 || !strings.HasPrefix(sender.texts[0], "亲爱的Redis:\n\n") {
				t.Fatal(sender.texts)
			}
		})
	}
}
func TestResumeCannotChangeCompletedTask(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "status"}, Rows: [][]driver.Value{{int64(1), "completed"}}})
	if err := New(&store.Store{DB: db}, nil, nil).Resume(1); err == nil {
		t.Fatal("completed task resumed")
	}
}
