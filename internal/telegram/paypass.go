package telegram

import (
	"context"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/paypass"
	"telegram-business-bot/internal/session"
)

// Spending from the wallet in the bot (sending a red packet, withdrawing) asks for the payment
// password first, in the private chat. The wizard keeps its session scene and switches to the
// steps below; "pay_next" names what to do once the password is verified (or first set):
//
//	pay_set1 → pay_set2   set a password (asked when the user has none)
//	pay_pin               enter the password
//	pp_old → pp_new1 → pp_new2   change it (scene "paypass", from the wallet or /paypass)
//
// Every message carrying a password is deleted right away.

// WithPayPassword turns on payment passwords.
func (r *Router) WithPayPassword(s *paypass.Service) *Router { r.pay = s; return r }

const payPinPrompt = "🔐 请输入 6 位<b>支付密码</b>（发送后消息会自动删除）。\n发送 /cancel 可取消。"
const paySetPrompt = "🔐 你还没有设置支付密码，请先设置 6 位数字<b>支付密码</b>（以后发红包、提现、商城付款都要用到）："

// requirePayPassword moves the session to the password step; next runs once it is verified.
// It returns the prompt to show in the private chat.
func (r *Router) requirePayPassword(ctx context.Context, userID int64, u *model.TelegramUser, s *session.Session, next string) string {
	s.Data["pay_next"] = next
	set, err := r.pay.IsSet(u.ID)
	if err == nil && !set {
		s.Step = "pay_set1"
	} else {
		s.Step = "pay_pin"
	}
	_ = r.sessions.Set(ctx, userID, *s)
	return payPrompt(s)
}

func payPrompt(s *session.Session) string {
	switch s.Step {
	case "pay_set1":
		return paySetPrompt
	case "pay_set2", "pp_new2":
		return "🔐 请再输入一次，确认支付密码："
	case "pp_old":
		return "🔐 请输入<b>当前</b>支付密码："
	case "pp_new1":
		return "🔐 请输入<b>新的</b> 6 位数字支付密码："
	}
	return payPinPrompt
}

func isPayStep(step string) bool {
	return strings.HasPrefix(step, "pay_") || strings.HasPrefix(step, "pp_")
}

// startPayManage lets the user set or change the payment password.
func (r *Router) startPayManage(ctx context.Context, chatID int64, u *model.TelegramUser) {
	s := session.Session{Scene: "paypass", Data: map[string]any{"pay_next": "none"}}
	if set, _ := r.pay.IsSet(u.ID); set {
		s.Step = "pp_old"
	} else {
		s.Step = "pay_set1"
	}
	_ = r.sessions.Set(ctx, u.TelegramUserID, s)
	_ = r.client.SendMessage(ctx, chatID, payPrompt(&s), nil)
}

// handlePayInput handles a password typed in the private chat; it reports whether the session
// was at a password step.
func (r *Router) handlePayInput(ctx context.Context, m *Message, u *model.TelegramUser, s *session.Session) bool {
	if r.pay == nil || !isPayStep(s.Step) {
		return false
	}
	pin := strings.TrimSpace(m.Text)
	_ = r.client.DeleteMessage(ctx, m.Chat.ID, m.MessageID)
	say := func(text string) { _ = r.client.SendMessage(ctx, m.Chat.ID, text, nil) }
	save := func(step string) {
		s.Step = step
		_ = r.sessions.Set(ctx, m.From.ID, *s)
		say(payPrompt(s))
	}
	switch s.Step {
	case "pay_set1", "pp_new1":
		if err := paypass.Check(pin); err != nil {
			say("⚠️ " + htmlEscape(err.Error()) + "\n\n" + payPrompt(s))
			return true
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.MinCost)
		if err != nil {
			return true
		}
		s.Data["pay_first"] = string(hash)
		save(map[string]string{"pay_set1": "pay_set2", "pp_new1": "pp_new2"}[s.Step])
	case "pay_set2", "pp_new2":
		first, _ := s.Data["pay_first"].(string)
		if bcrypt.CompareHashAndPassword([]byte(first), []byte(pin)) != nil {
			say("⚠️ 两次输入不一致，请重新输入。")
			save(map[string]string{"pay_set2": "pay_set1", "pp_new2": "pp_new1"}[s.Step])
			return true
		}
		delete(s.Data, "pay_first")
		var err error
		if s.Step == "pay_set2" {
			err = r.pay.Set(u.ID, pin)
		} else {
			err = r.pay.Replace(u.ID, pin)
		}
		if err != nil {
			_ = r.sessions.Clear(ctx, m.From.ID)
			say("设置失败：" + htmlEscape(userMessage(err)))
			return true
		}
		if s.Step == "pp_new2" {
			_ = r.sessions.Clear(ctx, m.From.ID)
			_ = r.client.SendMessage(ctx, m.Chat.ID, "✅ 支付密码已修改。", r.homeKeyboard(r.isAdmin(m.From.ID)))
			return true
		}
		say("✅ 支付密码已设置。")
		r.continueAfterPay(ctx, m.Chat.ID, u, s)
	case "pay_pin", "pp_old":
		err := r.pay.Verify(u.ID, pin)
		switch {
		case err == paypass.ErrNotSet:
			save("pay_set1")
		case err != nil && strings.Contains(err.Error(), "还可以再试"):
			say("⚠️ " + htmlEscape(err.Error()) + "\n\n" + payPrompt(s))
		case err != nil:
			_ = r.sessions.Clear(ctx, m.From.ID)
			say("⚠️ " + htmlEscape(userMessage(err)))
		case s.Step == "pp_old":
			save("pp_new1")
		default:
			r.continueAfterPay(ctx, m.Chat.ID, u, s)
		}
	}
	return true
}

func userMessage(err error) string {
	if paypass.IsUserError(err) {
		return err.Error()
	}
	return "请稍后再试"
}

// continueAfterPay runs the action that waited for the password.
func (r *Router) continueAfterPay(ctx context.Context, chatID int64, u *model.TelegramUser, s *session.Session) {
	next, _ := s.Data["pay_next"].(string)
	s.Step = "confirm"
	switch next {
	case "redpacket":
		if inGroupWizard(s) {
			group, msg := wizardMessage(s)
			r.sendPacket(ctx, group, msg, u, s)
			_ = r.client.SendMessage(ctx, chatID, "✅ 支付成功，红包已发到群里。", nil)
			return
		}
		r.sendPacket(ctx, chatID, 0, u, s)
	case "withdraw":
		r.submitWithdrawal(ctx, chatID, 0, u, s)
	default:
		_ = r.sessions.Clear(ctx, u.TelegramUserID)
	}
}

// editOrSend edits msgID, or sends a new message when there is none.
func (r *Router) editOrSend(ctx context.Context, chatID int64, msgID int, text string, kb *InlineKeyboardMarkup) {
	if msgID != 0 {
		_ = r.client.EditMessage(ctx, chatID, msgID, text, kb)
		return
	}
	_ = r.client.SendMessage(ctx, chatID, text, kb)
}
