package telegram

import "strings"

func ptrKB(k InlineKeyboardMarkup) *InlineKeyboardMarkup { return &k }
func empty(v, f string) string {
	if v == "" {
		return f
	}
	return v
}
func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
