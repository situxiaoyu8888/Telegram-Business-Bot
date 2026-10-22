package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// APIBase is the Bot API server. Set it (via TELEGRAM_API_URL) before creating clients to use a
// self-hosted Bot API server, or a local fake one in end-to-end tests.
var APIBase = "https://api.telegram.org"

type Client struct {
	token string
	base  string
	http  *http.Client

	mu       sync.Mutex
	username string // cached from getMe
}

// Username returns this bot's @username, calling getMe once and caching the success.
func (c *Client) Username(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.username != "" {
		return c.username, nil
	}
	me, err := c.GetMe(ctx)
	if err != nil {
		return "", err
	}
	c.username = me.Username
	return c.username, nil
}

func NewClient(token string) *Client {
	return &Client{
		token: token,
		base:  APIBase + "/bot" + token,
		http: &http.Client{
			Timeout: 70 * time.Second,
		},
	}
}

func (c *Client) call(
	ctx context.Context,
	method string,
	reqBody any,
	out any,
) (err error) {
	defer func() {
		if err != nil && c.token != "" && strings.Contains(err.Error(), c.token) {
			err = errors.New(strings.ReplaceAll(err.Error(), c.token, "[redacted]"))
		}
	}()
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		c.base+"/"+method,
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 300 {
		return fmt.Errorf(
			"telegram http %d: %s",
			resp.StatusCode,
			string(raw),
		)
	}

	if out == nil {
		var r APIResponse[json.RawMessage]
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}

		if !r.OK {
			return fmt.Errorf(
				"telegram api error %d: %s",
				r.ErrorCode,
				r.Description,
			)
		}

		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return err
	}

	return nil
}

// allowedUpdates are the update types the bots handle (inline queries share red packets).
var allowedUpdates = []string{"message", "callback_query", "inline_query", "channel_post", "my_chat_member"}

// CopyMessage copies a message (e.g. a channel post) into another chat.
func (c *Client) CopyMessage(ctx context.Context, chatID, fromChatID int64, messageID int, kb *InlineKeyboardMarkup) error {
	payload := map[string]any{"chat_id": chatID, "from_chat_id": fromChatID, "message_id": messageID}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	return c.call(ctx, "copyMessage", payload, nil)
}

// SendMessageID sends an HTML message and returns its message ID.
func (c *Client) SendMessageID(ctx context.Context, chatID int64, text string, kb *InlineKeyboardMarkup) (int, error) {
	payload := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML"}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	var resp APIResponse[Message]
	if err := c.call(ctx, "sendMessage", payload, &resp); err != nil {
		return 0, err
	}
	if !resp.OK {
		return 0, fmt.Errorf("sendMessage: %s", resp.Description)
	}
	return resp.Result.MessageID, nil
}

// ReplyMessage sends an HTML message in reply to another one in the same chat.
func (c *Client) ReplyMessage(ctx context.Context, chatID int64, replyTo int, text string, kb *InlineKeyboardMarkup) error {
	payload := map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML", "reply_parameters": map[string]any{"message_id": replyTo, "allow_sending_without_reply": true}}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	return c.call(ctx, "sendMessage", payload, nil)
}

// EditInlineMessage edits a message sent through inline mode.
func (c *Client) EditInlineMessage(ctx context.Context, inlineMessageID, text string, kb *InlineKeyboardMarkup) error {
	payload := map[string]any{"inline_message_id": inlineMessageID, "text": text, "parse_mode": "HTML"}
	if kb != nil {
		payload["reply_markup"] = kb
	}
	return c.call(ctx, "editMessageText", payload, nil)
}

// AnswerCallbackURL answers a button press by opening a t.me/<bot>?start=... link.
func (c *Client) AnswerCallbackURL(ctx context.Context, callbackID, url string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "url": url}, nil)
}

// AnswerCallbackAlert answers a button press with a toast, or a dialog when alert is true.
func (c *Client) AnswerCallbackAlert(ctx context.Context, callbackID, text string, alert bool) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": callbackID, "text": text, "show_alert": alert}, nil)
}

// InlineArticle is one result of an inline query: a ready-made message the user can send.
type InlineArticle struct {
	ID          string
	Title       string
	Description string
	Text        string
	Keyboard    *InlineKeyboardMarkup
}

// AnswerInlineQuery answers with personal, uncached results. startText and startParam add a
// button above the results that opens the private chat with /start startParam.
func (c *Client) AnswerInlineQuery(ctx context.Context, queryID string, results []InlineArticle, startText, startParam string) error {
	list := make([]map[string]any, 0, len(results))
	for _, r := range results {
		item := map[string]any{"type": "article", "id": r.ID, "title": r.Title, "description": r.Description,
			"input_message_content": map[string]any{"message_text": r.Text, "parse_mode": "HTML"}}
		if r.Keyboard != nil {
			item["reply_markup"] = r.Keyboard
		}
		list = append(list, item)
	}
	payload := map[string]any{"inline_query_id": queryID, "results": list, "cache_time": 0, "is_personal": true}
	if startText != "" {
		payload["button"] = map[string]any{"text": startText, "start_parameter": startParam}
	}
	return c.call(ctx, "answerInlineQuery", payload, nil)
}

func (c *Client) GetUpdates(
	ctx context.Context,
	offset int,
) ([]Update, error) {
	var resp APIResponse[[]Update]

	err := c.call(
		ctx,
		"getUpdates",
		map[string]any{
			"offset":          offset,
			"timeout":         60,
			"allowed_updates": allowedUpdates,
		},
		&resp,
	)
	if err != nil {
		return nil, err
	}

	if !resp.OK {
		return nil, fmt.Errorf("getUpdates: %s", resp.Description)
	}

	return resp.Result, nil
}

func (c *Client) SendMessage(
	ctx context.Context,
	chatID int64,
	text string,
	kb *InlineKeyboardMarkup,
) error {
	return c.SendMessageWithParseMode(
		ctx,
		chatID,
		text,
		"HTML",
		kb,
	)
}

func (c *Client) SendMessageWithParseMode(
	ctx context.Context,
	chatID int64,
	text string,
	parseMode string,
	kb *InlineKeyboardMarkup,
) error {
	payload := map[string]any{
		"chat_id": chatID,
		"text":    text,
	}

	if strings.TrimSpace(parseMode) != "" {
		payload["parse_mode"] = parseMode
	}

	if kb != nil {
		payload["reply_markup"] = kb
	}

	return c.call(
		ctx,
		"sendMessage",
		payload,
		nil,
	)
}

func (c *Client) SendPhoto(
	ctx context.Context,
	chatID int64,
	photo string,
	caption string,
	parseMode string,
	kb *InlineKeyboardMarkup,
) error {
	photo = strings.TrimSpace(photo)

	if photo == "" {
		return fmt.Errorf("photo is required")
	}

	payload := map[string]any{
		"chat_id": chatID,
		"photo":   photo,
	}

	if strings.TrimSpace(caption) != "" {
		payload["caption"] = caption
	}

	if strings.TrimSpace(parseMode) != "" {
		payload["parse_mode"] = parseMode
	}

	if kb != nil {
		payload["reply_markup"] = kb
	}

	return c.call(
		ctx,
		"sendPhoto",
		payload,
		nil,
	)
}

func (c *Client) SendVideo(
	ctx context.Context,
	chatID int64,
	video string,
	caption string,
	parseMode string,
	kb *InlineKeyboardMarkup,
) error {
	video = strings.TrimSpace(video)

	if video == "" {
		return fmt.Errorf("video is required")
	}

	payload := map[string]any{
		"chat_id":            chatID,
		"video":              video,
		"supports_streaming": true,
	}

	if strings.TrimSpace(caption) != "" {
		payload["caption"] = caption
	}

	if strings.TrimSpace(parseMode) != "" {
		payload["parse_mode"] = parseMode
	}

	if kb != nil {
		payload["reply_markup"] = kb
	}

	return c.call(
		ctx,
		"sendVideo",
		payload,
		nil,
	)
}

func (c *Client) EditMessage(
	ctx context.Context,
	chatID int64,
	messageID int,
	text string,
	kb *InlineKeyboardMarkup,
) error {
	payload := map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
		"parse_mode": "HTML",
	}

	if kb != nil {
		payload["reply_markup"] = kb
	}

	return c.call(
		ctx,
		"editMessageText",
		payload,
		nil,
	)
}

func (c *Client) AnswerCallback(
	ctx context.Context,
	callbackID string,
	text string,
) error {
	payload := map[string]any{
		"callback_query_id": callbackID,
	}

	if text != "" {
		payload["text"] = text
	}

	return c.call(
		ctx,
		"answerCallbackQuery",
		payload,
		nil,
	)
}

func (c *Client) DeleteWebhook(
	ctx context.Context,
	dropPending bool,
) error {
	return c.call(
		ctx,
		"deleteWebhook",
		map[string]any{
			"drop_pending_updates": dropPending,
		},
		nil,
	)
}

func (c *Client) SetWebhook(
	ctx context.Context,
	webhookURL string,
	secret string,
) error {
	payload := map[string]any{
		"url":             webhookURL,
		"allowed_updates": allowedUpdates,
	}

	if secret != "" {
		payload["secret_token"] = secret
	}

	return c.call(
		ctx,
		"setWebhook",
		payload,
		nil,
	)
}

// WebhookInfo is the part of getWebhookInfo the program checks.
type WebhookInfo struct {
	URL                string `json:"url"`
	PendingUpdateCount int    `json:"pending_update_count"`
	LastErrorDate      int64  `json:"last_error_date"`
	LastErrorMessage   string `json:"last_error_message"`
}

func (c *Client) GetWebhookInfo(ctx context.Context) (*WebhookInfo, error) {
	var resp APIResponse[WebhookInfo]
	if err := c.call(ctx, "getWebhookInfo", map[string]any{}, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("getWebhookInfo: %s", resp.Description)
	}
	return &resp.Result, nil
}

func (c *Client) GetMe(
	ctx context.Context,
) (*User, error) {
	var resp APIResponse[User]

	if err := c.call(
		ctx,
		"getMe",
		map[string]any{},
		&resp,
	); err != nil {
		return nil, err
	}

	if !resp.OK {
		return nil, fmt.Errorf(
			"getMe: %s",
			resp.Description,
		)
	}

	return &resp.Result, nil
}

func IsTelegramURL(s string) bool {
	u, err := url.Parse(s)

	return err == nil &&
		u.Scheme == "https" &&
		u.Host != ""
}

// Sender is the delivery surface shared by all business bots.
type Sender interface {
	SendMessageWithParseMode(context.Context, int64, string, string, *InlineKeyboardMarkup) error
	SendPhoto(context.Context, int64, string, string, string, *InlineKeyboardMarkup) error
	SendVideo(context.Context, int64, string, string, string, *InlineKeyboardMarkup) error
}
