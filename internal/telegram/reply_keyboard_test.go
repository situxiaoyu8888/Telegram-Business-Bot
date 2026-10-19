package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"telegram-business-bot/internal/model"
	"testing"
)

func TestReplyKeyboardRowsAllowManualToggle(t *testing.T) {
	items := []model.MenuItem{{Title: "🍴 美食", RowNo: 1}, {Title: "🛍 购物", RowNo: 1}, {Title: "🏨 酒店", RowNo: 1}, {Title: "🥳 娱乐", RowNo: 1}, {Title: "💵 换汇", RowNo: 2}}
	kb := replyKeyboard(items).(ReplyKeyboardMarkup)
	if len(kb.Keyboard) != 2 || len(kb.Keyboard[0]) != 4 || kb.Keyboard[1][0].Text != "💵 换汇" || !kb.ResizeKeyboard || kb.IsPersistent || kb.OneTimeKeyboard {
		t.Fatalf("%+v", kb)
	}
	if replyKeyboard(nil).(map[string]bool)["remove_keyboard"] != true {
		t.Fatal("obsolete keyboard not removed")
	}
}

type menuTransport struct {
	t       *testing.T
	method  string
	payload map[string]any
	calls   int
}

func (m *menuTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.method = req.URL.Path
	m.calls++
	if err := json.NewDecoder(req.Body).Decode(&m.payload); err != nil {
		m.t.Fatal(err)
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{}}`))}, nil
}
func TestReplyTextAndURLActionsSendNewMessages(t *testing.T) {
	for _, kind := range []string{"text", "url"} {
		t.Run(kind, func(t *testing.T) {
			tr := &menuTransport{t: t}
			c := NewClient("test")
			c.http = &http.Client{Transport: tr}
			r := &Router{client: c}
			r.dispatchMenu(context.Background(), &CallbackQuery{From: User{ID: 5}, Message: &Message{Chat: Chat{ID: 5}}}, &model.TelegramUser{}, model.MenuItem{Title: "美食", ActionType: kind, ActionValue: "https://example.com", Description: "<美食>"})
			if !strings.HasSuffix(tr.method, "/sendMessage") || tr.payload["message_id"] != nil {
				t.Fatal("attempted to edit user message", tr.method, tr.payload)
			}
			if !strings.Contains(tr.payload["text"].(string), "&lt;美食&gt;") {
				t.Fatal("unescaped content")
			}
			if kind == "url" {
				kb := tr.payload["reply_markup"].(map[string]any)
				button := kb["inline_keyboard"].([]any)[0].([]any)[0].(map[string]any)
				if button["url"] != "https://example.com" {
					t.Fatal(button)
				}
			}
		})
	}
}
func TestSendReplyKeyboardUsesReplyMarkup(t *testing.T) {
	tr := &menuTransport{t: t}
	c := NewClient("test")
	c.http = &http.Client{Transport: tr}
	if err := c.SendReplyKeyboard(context.Background(), 5, "选择菜单", []model.MenuItem{{Title: "客服", RowNo: 1}}); err != nil {
		t.Fatal(err)
	}
	kb := tr.payload["reply_markup"].(map[string]any)
	if kb["is_persistent"] != false || kb["one_time_keyboard"] != false || kb["keyboard"] == nil || kb["inline_keyboard"] != nil {
		t.Fatal(kb)
	}
}

func TestSubmenuKeyboardHasFullWidthHomeAndThreeColumnRows(t *testing.T) {
	items := []model.MenuItem{{Title: "餐厅", RowNo: 1}, {Title: "火锅", RowNo: 1}, {Title: "烧烤", RowNo: 1}, {Title: "奶茶", RowNo: 2}, {Title: "小吃", RowNo: 2}, {Title: "面食", RowNo: 2}}
	kb := replyKeyboard(submenuReplyItems(items)).(ReplyKeyboardMarkup)
	if len(kb.Keyboard) != 3 || len(kb.Keyboard[0]) != 1 || kb.Keyboard[0][0].Text != ReplyHomeText || len(kb.Keyboard[1]) != 3 || len(kb.Keyboard[2]) != 3 {
		t.Fatal(kb)
	}
	if items[0].Title != "餐厅" {
		t.Fatal("mutated source")
	}
	empty := replyKeyboard(submenuReplyItems(nil)).(ReplyKeyboardMarkup)
	if len(empty.Keyboard) != 1 || empty.Keyboard[0][0].Text != ReplyHomeText {
		t.Fatal(empty)
	}
}

func TestPhotoMenuSendsImageWithLinkAndOptionalCaption(t *testing.T) {
	for _, caption := range []string{"", "联系客服 <说明>"} {
		tr := &menuTransport{t: t}
		c := NewClient("test")
		c.http = &http.Client{Transport: tr}
		r := &Router{client: c}
		r.dispatchMenu(context.Background(), &CallbackQuery{Message: &Message{Chat: Chat{ID: 5}}}, nil, model.MenuItem{ActionType: "photo", ImageURL: "https://example.com/contact.png", ButtonText: "联系客服", ActionValue: "https://t.me/contact", Description: caption})
		if !strings.HasSuffix(tr.method, "/sendPhoto") || tr.payload["photo"] != "https://example.com/contact.png" {
			t.Fatal(tr.payload)
		}
		if caption == "" {
			if tr.payload["caption"] != nil {
				t.Fatal(tr.payload)
			}
		} else if tr.payload["caption"] != caption {
			t.Fatal(tr.payload)
		}
		kb := tr.payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
		b := kb[0].([]any)[0].(map[string]any)
		if b["text"] != "联系客服" || b["url"] != "https://t.me/contact" {
			t.Fatal(b)
		}
	}
}
