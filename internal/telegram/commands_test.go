package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type commandRequest struct {
	method  string
	payload map[string]any
}
type commandTransport struct {
	calls []commandRequest
	fail  bool
	t     *testing.T
}

func (c *commandTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var payload map[string]any
	if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
		c.t.Fatal(err)
	}
	c.calls = append(c.calls, commandRequest{method: req.URL.Path, payload: payload})
	status, body := 200, `{"ok":true,"result":true}`
	if c.fail {
		status = 503
		body = `{"ok":false,"description":"retry later"}`
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}
func TestRegisterPrivateCommandsAndMenuButton(t *testing.T) {
	for _, admin := range []bool{false, true} {
		tr := &commandTransport{t: t}
		c := NewClient("test")
		c.http = &http.Client{Transport: tr}
		if err := c.RegisterCommandMenu(context.Background(), admin, MenuButton{}); err != nil {
			t.Fatal(err)
		}
		if len(tr.calls) != 2 || !strings.HasSuffix(tr.calls[0].method, "/setMyCommands") || !strings.HasSuffix(tr.calls[1].method, "/setChatMenuButton") {
			t.Fatal(tr.calls)
		}
		if tr.calls[0].payload["scope"].(map[string]any)["type"] != "all_private_chats" {
			t.Fatal(tr.calls[0])
		}
		commands := tr.calls[0].payload["commands"].([]any)
		if !admin && (len(commands) != 3 || commands[0].(map[string]any)["command"] != "start" || commands[1].(map[string]any)["command"] != "hb" || commands[2].(map[string]any)["command"] != "contactus" || commands[2].(map[string]any)["description"] != "联系客服") {
			t.Fatal(commands)
		}
		if admin && commands[1].(map[string]any)["command"] != "admin" {
			t.Fatal(commands)
		}
		if tr.calls[1].payload["menu_button"].(map[string]any)["type"] != "commands" {
			t.Fatal(tr.calls[1])
		}
	}
}
func TestMenuButtonOpensH5WhenConfigured(t *testing.T) {
	tr := &commandTransport{t: t}
	c := NewClient("test")
	c.http = &http.Client{Transport: tr}
	if err := c.RegisterCommandMenu(context.Background(), false, MenuButton{Text: "商城", URL: "https://shop.example.com/"}); err != nil {
		t.Fatal(err)
	}
	button := tr.calls[1].payload["menu_button"].(map[string]any)
	if button["type"] != "web_app" || button["text"] != "商城" || button["web_app"].(map[string]any)["url"] != "https://shop.example.com/" {
		t.Fatal(button)
	}
}
func TestCommandRegistrationReportsFailureAndHonorsCancellation(t *testing.T) {
	tr := &commandTransport{t: t, fail: true}
	c := NewClient("test")
	c.http = &http.Client{Transport: tr}
	if err := c.RegisterCommandMenu(context.Background(), false, MenuButton{}); err == nil {
		t.Fatal("registration error lost")
	}
	if len(tr.calls) != 1 {
		t.Fatal("continued after failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &Router{client: c}
	r.SyncCommandMenu(ctx)
	if len(tr.calls) != 1 {
		t.Fatal("registered after cancellation")
	}
}
func TestCommandParsingDoesNotConfuseStartPrefixOrReplyLabels(t *testing.T) {
	for input, want := range map[string]string{"/start referral": "/start", "/start@ExampleBot invite": "/start", "/starter": "/starter", " /contactus@ExampleBot ": "/contactus", "/menu": "/menu", ReplyHomeText: ReplyHomeText, "火锅": "火锅", "": ""} {
		if got := commandName(input); got != want {
			t.Fatalf("%q: %q != %q", input, got, want)
		}
	}
}
