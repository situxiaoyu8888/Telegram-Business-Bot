package telegram

import (
	"context"
	"errors"
	"strings"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/session"
	"telegram-business-bot/internal/wallet"
)

// Deposits to an address shared by several users, and all ERC20 deposits, are claimed by
// transaction hash (session scene "deposit_claim": hash → amount for ERC20).

const depositClaimPrompt = "🧾 请发送这笔充值的<b>交易哈希</b>（TxID）：\n• USDT-TRC20：64 位字符\n• USDT-ERC20：0x 开头的 66 位字符\n\n发送 /cancel 可取消。"

// depositNeedsClaim reports whether the user has an address whose deposits must be claimed:
// an ERC20 one, or a TRC20 one shared with other users.
func (r *Router) depositNeedsClaim(list []model.DepositAddress) bool {
	for _, a := range list {
		if a.Network != "TRC20" {
			return true
		}
		var n int64
		r.store.DB.Model(&model.DepositAddress{}).Where("network = 'TRC20' AND address = ?", a.Address).Count(&n)
		if n > 1 {
			return true
		}
	}
	return false
}

func (r *Router) startDepositClaim(ctx context.Context, chatID int64, u *model.TelegramUser) {
	_ = r.sessions.Set(ctx, u.TelegramUserID, session.Session{Scene: "deposit_claim", Step: "hash", Data: map[string]any{}})
	_ = r.client.SendMessage(ctx, chatID, depositClaimPrompt, nil)
}

func (r *Router) handleDepositClaimInput(ctx context.Context, m *Message, u *model.TelegramUser, s *session.Session) {
	text := strings.TrimSpace(m.Text)
	if s.Data == nil { // an empty map does not survive the session store
		s.Data = map[string]any{}
	}
	say := func(t string) { _ = r.client.SendMessage(ctx, m.Chat.ID, t, nil) }
	done := func(t string) {
		_ = r.sessions.Clear(ctx, m.From.ID)
		_ = r.client.SendMessage(ctx, m.Chat.ID, t, ptrKB(BackHomeKeyboard()))
	}
	switch s.Step {
	case "hash":
		switch wallet.DepositHashNetwork(text) {
		case "TRC20":
			o, err := r.wallet.ClaimTRC20(u.ID, text)
			if err != nil {
				done("⚠️ " + htmlEscape(userText(err)))
				return
			}
			done("✅ <b>认领成功</b>\n\n<code>" + trimZeros(o.Amount) + "</code> USDT-TRC20 已存入你的钱包。\n订单编号：<code>" + wallet.OrderNo("D", o.ID, o.CreatedAt) + "</code>")
		case "ERC20":
			s.Data["hash"], s.Step = strings.ToLower(text), "amount"
			_ = r.sessions.Set(ctx, m.From.ID, *s)
			say("请输入这笔转账的 <b>USDT 数量</b>，例如 <code>100</code>：")
		default:
			say("⚠️ 交易哈希格式不正确。\n\n" + depositClaimPrompt)
		}
	case "amount":
		o, err := r.wallet.SubmitERC20(u.ID, sessionString(s, "hash"), text)
		if err != nil {
			if strings.Contains(err.Error(), "金额") {
				say("⚠️ " + htmlEscape(err.Error()))
				return
			}
			done("⚠️ " + htmlEscape(userText(err)))
			return
		}
		done("📨 <b>已提交</b>\n\n订单编号：<code>" + wallet.OrderNo("D", o.ID, o.CreatedAt) + "</code>\n金额：<code>" + trimZeros(o.Amount) + "</code> USDT-ERC20\n\n客服核对链上到账后入账，届时会通知你。")
	}
}

// userText is an error's message when it is meant for people, or a generic one.
func userText(err error) string {
	var ue wallet.UserError
	if errors.As(err, &ue) {
		return ue.Msg
	}
	return "处理失败，请稍后再试"
}
