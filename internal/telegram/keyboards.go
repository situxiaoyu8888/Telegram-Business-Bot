package telegram

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

func WithdrawKeyboard() InlineKeyboardMarkup {
	return InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{btn("TRC20", "withdraw:network:TRC20")},
			{btn("取消", "home")},
		},
	}
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
