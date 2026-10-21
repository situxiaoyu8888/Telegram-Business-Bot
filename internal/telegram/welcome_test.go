package telegram

import (
	"context"
	"database/sql/driver"
	"io"
	"net/http"
	"strings"
	"testing"

	"telegram-business-bot/internal/testutil"
)

func TestWelcomeUsesConfiguredTextEscaped(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "system_configs", Columns: []string{"key", "value"}, Rows: [][]driver.Value{{"welcome_text", "欢迎 <VIP>"}}})
	r.sendWelcome(context.Background(), 5)
	if !strings.HasSuffix(tr.method, "/sendMessage") || tr.payload["text"] != "欢迎 &lt;VIP&gt;" {
		t.Fatal(tr.method, tr.payload)
	}
}

func TestWelcomeEmptyTextDisabled(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "system_configs", Columns: []string{"key", "value"}, Rows: [][]driver.Value{{"welcome_text", "  "}}})
	r.sendWelcome(context.Background(), 5)
	if tr.method != "" {
		t.Fatal("sent welcome with empty text", tr.payload)
	}
}

func TestHomeWithOnlyInlineMenusSendsSingleMessage(t *testing.T) {
	r, tr := merchantRouter(t,
		testutil.Step{Contains: "system_configs", Columns: []string{"value"}, Rows: [][]driver.Value{{"SKK百事通"}}},
		testutil.Step{Contains: "placement=?", Columns: []string{"id"}},
		testutil.Step{Contains: "placement<>?", Columns: []string{"id", "code", "title", "action_type", "action_value", "row_no", "sort", "enabled", "role"}, Rows: [][]driver.Value{{int64(1), "wallet", "我的钱包", "builtin", "wallet", int64(1), int64(1), true, "user"}}},
	)
	tr.calls = 0
	r.sendHome(context.Background(), 5, 5)
	text, _ := tr.payload["text"].(string)
	if tr.calls != 1 || !strings.Contains(text, "SKK百事通") || strings.Contains(text, "更多功能") || tr.payload["reply_markup"].(map[string]any)["inline_keyboard"] == nil {
		t.Fatal(tr.calls, tr.payload)
	}
}

func TestUsernameIsCachedAfterSuccess(t *testing.T) {
	tr := &usernameTransport{}
	c := NewClient("test")
	c.http = &http.Client{Transport: tr}
	for i := 0; i < 2; i++ {
		if name, err := c.Username(context.Background()); err != nil || name != "skk_bot" {
			t.Fatal(name, err)
		}
	}
	if tr.calls != 1 {
		t.Fatal("getMe calls", tr.calls)
	}
}

type usernameTransport struct{ calls int }

func (u *usernameTransport) RoundTrip(*http.Request) (*http.Response, error) {
	u.calls++
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true,"result":{"id":1,"username":"skk_bot"}}`))}, nil
}

func TestSharePayloadCarriesProductAndReferralCode(t *testing.T) {
	m := sharePayload.FindStringSubmatch("p42_TG1E8A2C27E")
	if m == nil || m[1] != "p" || m[2] != "42" || m[3] != "TG1E8A2C27E" {
		t.Fatal(m)
	}
	if g := sharePayload.FindStringSubmatch("g7_TG1"); g == nil || g[1] != "g" || g[2] != "7" {
		t.Fatal(g)
	}
	for _, plain := range []string{"TG1E8A2C27E", "p_TG1", "p42", "p42_TG-1"} {
		if sharePayload.MatchString(plain) {
			t.Fatal("matched", plain)
		}
	}
}

func TestShopOrdersCommandListsOrdersWithShopButtons(t *testing.T) {
	r, tr := merchantRouter(t, testutil.Step{Contains: "shop_orders", Columns: []string{"id", "order_no", "status", "merchant_name", "pay_amount"}, Rows: [][]driver.Value{{int64(12), "S1", "shipped", "<店>", "9.500000"}}})
	r.menuButton = MenuButton{Text: "商城", URL: "https://shop.example.com/shop/"}
	r.sendShopOrders(context.Background(), 5, 1)
	text := tr.payload["text"].(string)
	if !strings.Contains(text, "S1") || !strings.Contains(text, "已发货") || !strings.Contains(text, "&lt;店&gt;") || !strings.Contains(text, "9.5 USDT") {
		t.Fatal(text)
	}
	rows := tr.payload["reply_markup"].(map[string]any)["inline_keyboard"].([]any)
	first := rows[0].([]any)[0].(map[string]any)
	if first["web_app"].(map[string]any)["url"] != "https://shop.example.com/shop/#/order/12" {
		t.Fatal(first)
	}
}
