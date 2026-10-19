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
		base:  "https://api.telegram.org/bot" + token,
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
			"allowed_updates": []string{"message", "callback_query"},
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
		"allowed_updates": []string{"message", "callback_query"},
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
