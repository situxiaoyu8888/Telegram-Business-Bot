package telegram

import (
	"strings"
	"testing"
)

func TestWithdrawalConfirmationBoundToRequest(t *testing.T) {
	a := ConfirmWithdrawKeyboard("first")
	b := ConfirmWithdrawKeyboard("second")
	if a.InlineKeyboard[0][0].CallbackData == b.InlineKeyboard[0][0].CallbackData {
		t.Fatal("confirmation not bound to request")
	}
}
func TestOnlySupportedWithdrawalNetworksOffered(t *testing.T) {
	for _, row := range WithdrawKeyboard().InlineKeyboard {
		for _, b := range row {
			if strings.Contains(b.CallbackData, "ERC20") {
				t.Fatal("unsupported network offered")
			}
		}
	}
}
