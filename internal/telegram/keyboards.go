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
func WithdrawKeyboard(currencies []model.WalletCurrency) InlineKeyboardMarkup {
	var rows [][]InlineKeyboardButton
	for _, c := range currencies {
		if !c.Enabled || !c.Withdrawable {
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
