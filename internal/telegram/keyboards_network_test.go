package telegram

import (
	"testing"

	"telegram-business-bot/internal/model"
)

func TestWithdrawKeyboardOffersUSDTNetworksFirst(t *testing.T) {
	kb := WithdrawKeyboard([]model.WalletCurrency{
		{Code: "CNY", Name: "人民币", Enabled: true, Withdrawable: true},
		{Code: "USDT", Name: "USDT-TRC20", Enabled: true, Withdrawable: true},
		{Code: "USDT_ERC20", Name: "USDT-ERC20", Enabled: true, Withdrawable: true},
		{Code: "THB", Name: "泰铢", Enabled: true},
	})
	first := kb.InlineKeyboard[0]
	if len(first) != 2 || first[0].Text != "TRC20-USDT" || first[0].CallbackData != "withdraw:cur:USDT" || first[1].Text != "ERC20-USDT" || first[1].CallbackData != "withdraw:cur:USDT_ERC20" {
		t.Fatalf("first row %+v", first)
	}
	if len(kb.InlineKeyboard) != 3 || kb.InlineKeyboard[1][0].Text != "人民币" {
		t.Fatalf("rows %+v", kb.InlineKeyboard)
	}
	dep := DepositNetworkKeyboard().InlineKeyboard[0]
	if dep[0].CallbackData != "dep:net:TRC20" || dep[1].CallbackData != "dep:net:ERC20" {
		t.Fatalf("deposit row %+v", dep)
	}
}
