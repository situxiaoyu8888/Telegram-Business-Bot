package telegram

import (
	"strings"
	"testing"

	"telegram-business-bot/internal/model"
)

func TestWithdrawalConfirmationBoundToRequest(t *testing.T) {
	a := ConfirmWithdrawKeyboard("first")
	b := ConfirmWithdrawKeyboard("second")
	if a.InlineKeyboard[0][0].CallbackData == b.InlineKeyboard[0][0].CallbackData {
		t.Fatal("confirmation not bound to request")
	}
}
func TestOnlyWithdrawableCurrenciesOffered(t *testing.T) {
	kb := WithdrawKeyboard([]model.WalletCurrency{{Code: "USDT", Name: "USDT-TRC20", Enabled: true, Withdrawable: true}, {Code: "CNY", Name: "人民币", Enabled: true}, {Code: "THB", Name: "泰铢", Withdrawable: true}})
	var data []string
	for _, row := range kb.InlineKeyboard {
		for _, b := range row {
			data = append(data, b.CallbackData)
		}
	}
	if got := strings.Join(data, ","); got != "withdraw:cur:USDT,home" {
		t.Fatalf("offered %s", got)
	}
}

func TestNewsMessageEscapesAndTrims(t *testing.T) {
	a := &model.NewsArticle{Title: "<b>标题</b>", Content: strings.Repeat("字", 900), ImageURL: "https://e.com/a.jpg", LinkURL: "https://e.com/a"}
	text, photo, kb := NewsMessage(a, "本地")
	if photo == "" || kb == nil || kb.InlineKeyboard[0][0].URL != "https://e.com/a" {
		t.Fatal(photo, kb)
	}
	if strings.Contains(text, "<b><b>") || !strings.Contains(text, "&lt;b&gt;") || len([]rune(text)) > 1024 {
		t.Fatalf("caption %d runes: %.80s", len([]rune(text)), text)
	}
}
