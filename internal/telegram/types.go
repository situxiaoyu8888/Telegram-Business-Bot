package telegram

import "strings"

type Update struct {
	UpdateID      int            `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	InlineQuery   *InlineQuery   `json:"inline_query,omitempty"`
}

// InlineQuery is "@bot text" typed in any chat (needs inline mode enabled in @BotFather).
type InlineQuery struct {
	ID    string `json:"id"`
	From  User   `json:"from"`
	Query string `json:"query"`
}

type Message struct {
	MessageID int    `json:"message_id"`
	From      *User  `json:"from,omitempty"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text,omitempty"`
}

type User struct {
	ID           int64  `json:"id"`
	IsBot        bool   `json:"is_bot"`
	FirstName    string `json:"first_name"`
	LastName     string `json:"last_name,omitempty"`
	Username     string `json:"username,omitempty"`
	LanguageCode string `json:"language_code,omitempty"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type CallbackQuery struct {
	ID              string   `json:"id"`
	From            User     `json:"from"`
	Message         *Message `json:"message,omitempty"`
	InlineMessageID string   `json:"inline_message_id,omitempty"` // set instead of Message for inline-mode messages
	Data            string   `json:"data,omitempty"`
}

type APIResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description,omitempty"`
	ErrorCode   int    `json:"error_code,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type InlineKeyboardButton struct {
	Text         string      `json:"text"`
	CallbackData string      `json:"callback_data,omitempty"`
	URL          string      `json:"url,omitempty"`
	WebApp       *WebAppInfo `json:"web_app,omitempty"` // opens a Mini App; private chats only
	// SwitchInlineQuery lets the user pick a chat and starts "@bot <query>" there.
	SwitchInlineQuery string `json:"switch_inline_query,omitempty"`
}

type WebAppInfo struct {
	URL string `json:"url"`
}

// ShopButton opens a page of the H5 shop (fragment such as "/order/12") in the Mini App.
func ShopButton(text, shopURL, fragment string) InlineKeyboardButton {
	return InlineKeyboardButton{Text: text, WebApp: &WebAppInfo{URL: strings.SplitN(shopURL, "#", 2)[0] + "#" + fragment}}
}
