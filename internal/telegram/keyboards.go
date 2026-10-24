package telegram

import "telegram-business-bot/internal/model"

func btn(text, data string) InlineKeyboardButton {
	return InlineKeyboardButton{Text: text, CallbackData: data}
}

func HomeKeyboard(isAdmin bool) InlineKeyboardMarkup {
	rows := [][]InlineKeyboardButton{
		{btn("👤 个人中心", "menu:profile"), btn("💰 我的钱包", "menu:wallet")},
		{btn("💵 充值", "menu:deposit"), btn("💸 提现", "menu:withdraw")},
		{btn("📜 交易记录", "menu:records"), btn("🎧 联系客服", "menu:support")},
	}
	if isAdmin {
		rows = append(rows, []InlineKeyboardButton{btn("🛡 管理后台", "menu:admin")})
	}
	return InlineKeyboardMarkup{InlineKeyboard: rows}
}

func BackHomeKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{btn("⬅ 返回首页", "home")},
		},
	}
}

// WithdrawKeyboard offers the withdrawable currencies, two per row.
// usdtNetworkNames label the two USDT wallets by network in the bot's deposit and withdrawal menus.
var usdtNetworkNames = map[string]string{"USDT": "TRC20-USDT", "USDT_ERC20": "ERC20-USDT"}

// DepositNetworkKeyboard lets the user pick the network to deposit USDT on.
func DepositNetworkKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{btn("TRC20-USDT", "dep:net:TRC20"), btn("ERC20-USDT", "dep:net:ERC20")},
		{btn("🏠 返回首页", "home")},
	}}
}

// WithdrawKeyboard offers the withdrawable currencies: the USDT networks first, then the rest.
func WithdrawKeyboard(currencies []model.WalletCurrency) InlineKeyboardMarkup {
	var rows [][]InlineKeyboardButton
	var usdt []InlineKeyboardButton
	for _, c := range currencies {
		if name, ok := usdtNetworkNames[c.Code]; ok && c.Enabled && c.Withdrawable {
			usdt = append(usdt, btn(name, "withdraw:cur:"+c.Code))
		}
	}
	if len(usdt) > 0 {
		rows = append(rows, usdt)
	}
	for _, c := range currencies {
		if _, ok := usdtNetworkNames[c.Code]; ok || !c.Enabled || !c.Withdrawable {
			continue
		}
		b := btn(c.Name, "withdraw:cur:"+c.Code)
		if n := len(rows); n > 0 && len(rows[n-1]) < 2 {
			rows[n-1] = append(rows[n-1], b)
		} else {
			rows = append(rows, []InlineKeyboardButton{b})
		}
	}
	rows = append(rows, []InlineKeyboardButton{btn("取消", "home")})
	return InlineKeyboardMarkup{InlineKeyboard: rows}
}

func ConfirmWithdrawKeyboard(requestID string) InlineKeyboardMarkup {
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{btn("✅ 确认提交", "withdraw:confirm:"+requestID), btn("❌ 取消", "withdraw:cancel")},
		},
	}
}

func AdminKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{btn("👥 用户统计", "admin:users"), btn("🧾 最近操作", "admin:logs")},
			{btn("⬅ 返回首页", "home")},
		},
	}
}
