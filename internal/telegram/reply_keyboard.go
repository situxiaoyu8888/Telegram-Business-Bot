package telegram

import (
	"context"
	"strings"
	"telegram-business-bot/internal/model"
)

const ReplyHomeText = "↩️ 返回主菜单"

func submenuReplyItems(items []model.MenuItem) []model.MenuItem {
	result := make([]model.MenuItem, 0, len(items)+1)
	result = append(result, model.MenuItem{Title: ReplyHomeText, RowNo: -1})
	return append(result, items...)
}

type ReplyKeyboardButton struct {
	Text string `json:"text"`
}
type ReplyKeyboardMarkup struct {
	Keyboard        [][]ReplyKeyboardButton `json:"keyboard"`
	ResizeKeyboard  bool                    `json:"resize_keyboard"`
	IsPersistent    bool                    `json:"is_persistent"`
	OneTimeKeyboard bool                    `json:"one_time_keyboard"`
}

func replyKeyboard(items []model.MenuItem) any {
	if len(items) == 0 {
		return map[string]bool{"remove_keyboard": true}
	}
	rows := [][]ReplyKeyboardButton{}
	last := -1
	for _, item := range items {
		if len(rows) == 0 || item.RowNo != last || len(rows[len(rows)-1]) >= 8 {
			rows = append(rows, []ReplyKeyboardButton{})
			last = item.RowNo
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], ReplyKeyboardButton{Text: item.Title})
	}
	// Allow clients to show the keyboard toggle instead of forcing it open.
	return ReplyKeyboardMarkup{Keyboard: rows, ResizeKeyboard: true, IsPersistent: false, OneTimeKeyboard: false}
}
func (c *Client) SendReplyKeyboard(ctx context.Context, chatID int64, text string, items []model.MenuItem) error {
	return c.call(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": text, "parse_mode": "HTML", "reply_markup": replyKeyboard(items)}, nil)
}

// Text keyboard presses are new user messages, never editable bot messages.
func (r *Router) menuResponse(ctx context.Context, chatID int64, msgID int, text string, kb *InlineKeyboardMarkup) error {
	if msgID == 0 {
		return r.client.SendMessage(ctx, chatID, text, kb)
	}
	err := r.client.EditMessage(ctx, chatID, msgID, text, kb)
	// A photo (e.g. a deposit QR code) has no text to edit: answer with a new message.
	if err != nil && strings.Contains(err.Error(), "no text in the message") {
		return r.client.SendMessage(ctx, chatID, text, kb)
	}
	return err
}
