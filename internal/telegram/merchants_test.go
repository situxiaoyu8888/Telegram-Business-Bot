package telegram

import (
	"context"
	"database/sql/driver"
	"net/http"
	"strings"
	"telegram-business-bot/internal/menu"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func merchantRouter(t *testing.T, steps ...testutil.Step) (*Router, *menuTransport) {
	db := testutil.DB(t, steps...)
	st := &store.Store{DB: db}
	transport := &menuTransport{t: t}
	client := NewClient("test")
	client.http = &http.Client{Transport: transport}
	return &Router{store: st, menus: menu.New(st), client: client}, transport
}
func merchantCallback(data string) *CallbackQuery {
	return &CallbackQuery{Data: data, From: User{ID: 5}, Message: &Message{MessageID: 10, Chat: Chat{ID: 5}}}
}
func categoryStep() testutil.Step {
	return testutil.Step{Contains: "menu_items", Columns: []string{"id", "parent_id", "action_type", "enabled", "title", "role"}, Rows: [][]driver.Value{{int64(2), int64(1), "merchants", true, "中餐", "user"}}}
}
func parentStep(enabled bool, role string) testutil.Step {
	return testutil.Step{Contains: "menu_items", Columns: []string{"id", "parent_id", "action_type", "enabled", "role", "code"}, Rows: [][]driver.Value{{int64(1), int64(0), "submenu", enabled, role, "food"}}}
}
func TestMerchantListPagination(t *testing.T) {
	rows := [][]driver.Value{}
	for i := 1; i <= 11; i++ {
		rows = append(rows, []driver.Value{int64(i), "店铺"})
	}
	r, tr := merchantRouter(t, categoryStep(), parentStep(true, "user"), testutil.Step{Contains: "menu_id=? AND enabled=1", Columns: []string{"id", "name"}, Rows: rows}, parentStep(true, "user"))
	r.showMerchants(context.Background(), merchantCallback(""), 2, 0)
	kb := tr.payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if len(kb) != 13 {
		t.Fatalf("expected ten shops, next, parent, home: %d", len(kb))
	}
	next := kb[10].([]any)[0].(map[string]any)
	if next["callback_data"] != "shops:2:1" {
		t.Fatal(next)
	}
}
func TestMerchantDetailEscapesTextAndKeepsReturnPage(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "id=? AND enabled=1", Columns: []string{"id", "menu_id", "name", "description", "contact_url"}, Rows: [][]driver.Value{{int64(9), int64(2), "<店铺>", "A&B", "https://t.me/shop"}}}, categoryStep(), parentStep(true, "user"))
	r.handleMerchantCallback(context.Background(), merchantCallback("shop:9:3"))
	text := tr.payload["text"].(string)
	if !strings.Contains(text, "&lt;店铺&gt;") || !strings.Contains(text, "A&amp;B") {
		t.Fatal(text)
	}
	kb := tr.payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	if kb[1].([]any)[0].(map[string]any)["callback_data"] != "shops:2:3" {
		t.Fatal(kb)
	}
}
func TestMerchantCallbackCannotBypassHiddenOrAdminParent(t *testing.T) {
	for _, v := range []struct {
		enabled bool
		role    string
	}{{false, "user"}, {true, "admin"}} {
		r, tr := merchantRouter(t, categoryStep(), parentStep(v.enabled, v.role))
		r.showMerchants(context.Background(), merchantCallback(""), 2, 0)
		if !strings.Contains(tr.payload["text"].(string), "暂不可用") {
			t.Fatal(tr.payload)
		}
	}
}
func TestDisabledMerchantAndMalformedCallbacksFailClosed(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "id=? AND enabled=1", Columns: []string{"id"}})
	r.handleMerchantCallback(context.Background(), merchantCallback("shop:9:0"))
	if !strings.Contains(tr.payload["text"].(string), "暂不可用") {
		t.Fatal(tr.payload)
	}
	for _, data := range []string{"shops:2:-1", "shop:9:100001", "shop:bad:0", "shop:9", "shops:0:0"} {
		r, tr = merchantRouter(t)
		r.handleMerchantCallback(context.Background(), merchantCallback(data))
		if !strings.Contains(tr.payload["text"].(string), "暂不可用") {
			t.Fatal(data)
		}
	}
}
func TestMerchantTextIncludesContactDetails(t *testing.T) {
	text := merchantText(model.Merchant{Name: "店铺", Address: "地址", Hours: "10–22", Phone: "123"})
	for _, part := range []string{"店铺", "地址", "10–22", "123"} {
		if !strings.Contains(text, part) {
			t.Fatal(text)
		}
	}
}

func TestReplySubmenuReplacesKeyboardInsteadOfEditingMessage(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "parent_id=? AND enabled=1 AND placement=?", Columns: []string{"title", "row_no"}, Rows: [][]driver.Value{{"餐厅", int64(1)}, {"火锅", int64(1)}, {"烧烤", int64(1)}}}, testutil.Step{Contains: "placement<>?", Columns: []string{"id"}}, testutil.Step{Contains: "enabled=1", Columns: []string{"id"}})
	r.dispatchMenu(context.Background(), merchantCallback("menu:food"), &model.TelegramUser{}, model.MenuItem{ID: 1, Title: "美食", Placement: "reply", ActionType: "submenu"})
	if !strings.HasSuffix(tr.method, "/sendMessage") {
		t.Fatal(tr.method)
	}
	keyboard := tr.payload["reply_markup"].(map[string]any)["keyboard"].([]any)
	if len(keyboard) != 2 || len(keyboard[0].([]any)) != 1 || len(keyboard[1].([]any)) != 3 {
		t.Fatal(keyboard)
	}
}

func TestHomeRestoresOnlyRootReplyKeyboard(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "system_configs", Columns: []string{"value"}, Rows: [][]driver.Value{{"讯舟"}}}, testutil.Step{Contains: "parent_id=0 AND enabled=1 AND placement=?", Columns: []string{"title", "row_no"}, Rows: [][]driver.Value{{"美食", int64(1)}, {"购物", int64(1)}}}, testutil.Step{Contains: "placement<>?", Columns: []string{"id"}})
	r.sendHome(context.Background(), 5, 5)
	kb := tr.payload["reply_markup"].(map[string]any)["keyboard"].([]any)
	if len(kb) != 1 || len(kb[0].([]any)) != 2 || kb[0].([]any)[0].(map[string]any)["text"] != "美食" {
		t.Fatal(kb)
	}
}

func TestRecommendationButtonsUseOneThenTwoColumns(t *testing.T) {
	items := []model.Merchant{{ID: 1, Name: "第一家"}, {ID: 2, Name: "第二家"}, {ID: 3, Name: "第三家"}, {ID: 4, Name: "第四家"}, {ID: 5, Name: "第五家"}}
	kb := recommendationKeyboard(items, 1).InlineKeyboard
	if len(kb) != 4 || kb[3][0].CallbackData != "recommend:1" || len(kb[0]) != 1 || len(kb[1]) != 2 || len(kb[2]) != 2 || kb[0][0].CallbackData != "shop:1:0" || kb[2][1].Text != "🟢 第五家" {
		t.Fatal(kb)
	}
}
func recommendationCategoryStep() testutil.Step {
	return testutil.Step{Contains: "role<>'admin'", Columns: []string{"id", "parent_id", "action_type"}, Rows: [][]driver.Value{{int64(1), int64(0), "submenu"}, {int64(2), int64(1), "merchants"}, {int64(3), int64(99), "merchants"}}}
}
func TestRecommendationsPrioritizePinnedAndExcludeFirstFromRest(t *testing.T) {
	for _, pinned := range []bool{true, false} {
		r, tr := merchantRouter(t, recommendationCategoryStep(),
			testutil.Step{Contains: "WHERE menu_id IN (?) AND enabled=1 ORDER BY pinned DESC,RAND() LIMIT ?", Args: []any{int64(2), int64(1)}, Columns: []string{"id", "name", "pinned"}, Rows: [][]driver.Value{{int64(9), "推荐店铺", pinned}}},
			testutil.Step{Contains: "WHERE menu_id IN (?) AND enabled=1 AND id<>? ORDER BY RAND() LIMIT ?", Args: []any{int64(2), int64(9), int64(12)}, Columns: []string{"id", "name"}, Rows: [][]driver.Value{{int64(10), "其他店铺"}}})
		r.sendMerchantRecommendations(context.Background(), merchantCallback(""), 1)
		if tr.payload["text"] != "随机给您推荐的商铺" {
			t.Fatal(tr.payload)
		}
	}
}
func TestRecommendationRefreshEditsExistingMessage(t *testing.T) {
	r, tr := merchantRouter(t, recommendationCategoryStep(),
		testutil.Step{Contains: "pinned DESC,RAND()", Columns: []string{"id", "name"}, Rows: [][]driver.Value{{int64(9), "推荐店铺"}}},
		testutil.Step{Contains: "id<>?", Columns: []string{"id"}})
	r.handleRecommendationCallback(context.Background(), merchantCallback("recommend:1"))
	if !strings.HasSuffix(tr.method, "/editMessageText") || tr.payload["message_id"] != float64(10) {
		t.Fatal(tr.method, tr.payload)
	}
}
func TestRecommendationRefreshRejectsUnavailableCategories(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "enabled=1", Columns: []string{"id"}})
	r.handleRecommendationCallback(context.Background(), merchantCallback("recommend:1"))
	if !strings.Contains(tr.payload["text"].(string), "暂不可用") {
		t.Fatal(tr.payload)
	}
}
func TestRecommendationRefreshClearsEmptyList(t *testing.T) {
	r, tr := merchantRouter(t, recommendationCategoryStep(), testutil.Step{Contains: "pinned DESC,RAND()", Columns: []string{"id"}})
	r.handleRecommendationCallback(context.Background(), merchantCallback("recommend:1"))
	if tr.payload["text"] != "当前分类暂无可推荐的商家。" {
		t.Fatal(tr.payload)
	}
}

func TestKeywordSearchAndRefreshKeepSearchScope(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		r, tr := merchantRouter(t,
			testutil.Step{Contains: "role<>'admin'", Columns: []string{"id", "parent_id", "action_type", "title"}, Rows: [][]driver.Value{{int64(1), int64(0), "submenu", "美食"}, {int64(2), int64(1), "merchants", "🍢 烧烤"}}},
			testutil.Step{Contains: "pinned DESC,RAND()", Args: []any{int64(2), int64(1)}, Columns: []string{"id", "name"}, Rows: [][]driver.Value{{int64(9), "烧烤店"}}},
			testutil.Step{Contains: "id<>?", Args: []any{int64(2), int64(9), int64(12)}, Columns: []string{"id"}})
		if !r.searchMerchants(context.Background(), merchantCallback(""), "烧烤", refresh) {
			t.Fatal("not handled")
		}
		want := "/sendMessage"
		if refresh {
			want = "/editMessageText"
		}
		if !strings.HasSuffix(tr.method, want) {
			t.Fatal(tr.method)
		}
		kb := tr.payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
		last := kb[len(kb)-1].([]any)[0].(map[string]any)
		if last["callback_data"] != "search:烧烤" {
			t.Fatal(last)
		}
	}
}
