package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/redpacket"
	"telegram-business-bot/internal/session"
)

// Red packets are created in a private chat (menu "redpacket", /hb) or right in a group
// (/start there; /hb still works), then posted by the bot in that group or shared
// anywhere through inline mode ("@bot hb <code>"). The "hb:<code>" button claims a share;
// password packets are claimed by sending the password in the group or to the bot.

// WithRedPacketOnly makes this a red-packet bot: its private chat offers sending red packets,
// the wallet and (with shopURL) the exchange page, and nothing else.
func (r *Router) WithRedPacketOnly(shopURL string) *Router {
	r.packetOnly, r.menuButton = true, MenuButton{}
	if shopURL != "" {
		r.exchangeURL = shopURL
	}
	return r
}

const packetHomeText = "<b>🧧 红包机器人</b>\n\n" +
	"• 发红包：点「发红包」，或在群里发送 /start\n" +
	"• 抢红包：点群里红包下方的「抢红包」；口令红包发送口令即可\n" +
	"• 抢到的钱直接存入钱包，未领完的红包到期自动退回"

func (r *Router) packetHomeKeyboard() *InlineKeyboardMarkup {
	rows := [][]InlineKeyboardButton{{btn("🧧 发红包", "hbw:new"), btn("💰 我的钱包", "rpo:wallet")}}
	if r.exchangeURL != "" {
		rows = append(rows, []InlineKeyboardButton{ShopButton("💱 换汇", r.exchangeURL, "/exchange")})
	}
	if name, err := r.client.Username(context.Background()); err == nil && name != "" {
		rows = append(rows, []InlineKeyboardButton{{Text: "➕ 把机器人加入群聊", URL: "https://t.me/" + name + "?startgroup=hb"}})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// WithRedPackets enables red packets on this bot.
func (r *Router) WithRedPackets(s *redpacket.Service) *Router { r.packets = s; return r }

func displayName(u *model.TelegramUser) string {
	if n := strings.TrimSpace(u.FirstName + " " + u.LastName); n != "" {
		return n
	}
	if u.Username != "" {
		return u.Username
	}
	return fmt.Sprintf("用户%d", u.TelegramUserID)
}

func packetClaimer(u *model.TelegramUser) redpacket.Claimer {
	return redpacket.Claimer{UserID: u.ID, TelegramUserID: u.TelegramUserID, Username: u.Username, Name: displayName(u)}
}

// usernamer is the part of Client that knows the bot's @username.
type usernamer interface {
	Username(context.Context) (string, error)
}

// packetEditor is the part of Client that edits packet cards.
type packetEditor interface {
	EditMessage(context.Context, int64, int, string, *InlineKeyboardMarkup) error
	EditInlineMessage(context.Context, string, string, *InlineKeyboardMarkup) error
}

// packetKeyboardFor is the claim button (only while the packet can be claimed) and a link to
// send one's own packet.
func packetKeyboardFor(ctx context.Context, svc *redpacket.Service, p *model.RedPacket, bot usernamer) *InlineKeyboardMarkup {
	rows := [][]InlineKeyboardButton{}
	if svc.Open(p) {
		rows = append(rows, []InlineKeyboardButton{btn("🧧 抢红包", "hb:"+p.Code)})
	}
	if bot != nil {
		if name, err := bot.Username(ctx); err == nil && name != "" {
			rows = append(rows, []InlineKeyboardButton{{Text: "我也发红包", URL: "https://t.me/" + name + "?start=hb"}})
		}
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (r *Router) packetKeyboard(ctx context.Context, p *model.RedPacket) *InlineKeyboardMarkup {
	return packetKeyboardFor(ctx, r.packets, p, r.client)
}

// RefreshPacketMessages redraws a packet's cards through the bot that owns it: the one given by
// inlineMessageID (or the group message when it is empty) while the packet is open, and every
// known copy once it is finished or expired, so no copy keeps a claim button.
func RefreshPacketMessages(ctx context.Context, s Sender, svc *redpacket.Service, code, inlineMessageID string, logger *zap.Logger) {
	editor, ok := s.(packetEditor)
	if !ok {
		return
	}
	p, claims, err := svc.ByCode(code)
	if err != nil {
		return
	}
	bot, _ := s.(usernamer)
	text, kb := svc.Card(p, claims), packetKeyboardFor(ctx, svc, p, bot)
	_ = svc.RememberInline(p.ID, inlineMessageID)
	inline, group := []string{inlineMessageID}, inlineMessageID == ""
	if !svc.Open(p) {
		inline, _ = svc.InlineMessages(p.ID)
		group = true
	}
	edit := func(err error) {
		if err != nil && !strings.Contains(err.Error(), "message is not modified") && logger != nil {
			logger.Debug("red packet card update failed", zap.String("code", code), zap.Error(err))
		}
	}
	for _, id := range inline {
		if id != "" {
			edit(editor.EditInlineMessage(ctx, id, text, kb))
		}
	}
	if group && p.ChatID != 0 && p.MessageID != 0 {
		edit(editor.EditMessage(ctx, p.ChatID, p.MessageID, text, kb))
	}
}

// refreshPacket redraws the card that was clicked (inline mode) or posted in a group.
func (r *Router) refreshPacket(ctx context.Context, code, inlineMessageID string) {
	RefreshPacketMessages(ctx, r.client, r.packets, code, inlineMessageID, r.logger)
}

func (r *Router) packetCurrencyName(code string) string {
	if c, err := r.wallet.Currency(code); err == nil {
		return c.Name
	}
	return code
}

func (r *Router) answerClaim(claim *model.RedPacketClaim, p *model.RedPacket, err error) string {
	if err != nil {
		if redpacket.IsUserError(err) {
			if claim != nil && strings.Contains(err.Error(), "已经领过") {
				return fmt.Sprintf("你已经领过这个红包了（%s %s）", trimZeros(claim.Amount), r.packetCurrencyName(p.Currency))
			}
			return err.Error()
		}
		r.logger.Error("red packet claim failed", zap.Error(err))
		return "领取失败，请稍后再试"
	}
	return fmt.Sprintf("🧧 抢到 %s %s，已存入钱包", trimZeros(claim.Amount), r.packetCurrencyName(p.Currency))
}

// handlePacketButton claims a share from the "hb:<code>" button, in any chat.
func (r *Router) handlePacketButton(ctx context.Context, cb *CallbackQuery, u *model.TelegramUser) {
	code := strings.TrimPrefix(cb.Data, "hb:")
	if u == nil || u.Status != 1 {
		_ = r.client.AnswerCallbackAlert(ctx, cb.ID, "账号不可用", true)
		return
	}
	// A password packet is claimed by entering the password: the button opens the private chat,
	// remembering which message to update afterwards.
	if p, _, err := r.packets.ByCode(code); err == nil && p.Type == redpacket.TypePassword && r.packets.Open(p) {
		if name, err := r.client.Username(ctx); err == nil && name != "" {
			_ = r.sessions.Set(ctx, cb.From.ID, session.Session{Scene: "redpacket_pw", Step: "password", Data: map[string]any{"code": code, "inline": cb.InlineMessageID}})
			_ = r.client.AnswerCallbackURL(ctx, cb.ID, "https://t.me/"+name+"?start=hbp_"+code)
			return
		}
	}
	claim, p, err := r.packets.Claim(code, packetClaimer(u))
	_ = r.client.AnswerCallbackAlert(ctx, cb.ID, r.answerClaim(claim, p, err), true)
	// Also after a failure: a card still showing the button for a finished packet loses it.
	r.refreshPacket(ctx, code, cb.InlineMessageID)
}

// tryPasswordClaim treats text as a red packet password; it reports whether it was one.
// chatID is the group, or 0 in the private chat (any packet).
func (r *Router) tryPasswordClaim(ctx context.Context, chatID int64, m *Message, u *model.TelegramUser) bool {
	if r.packets == nil || strings.HasPrefix(m.Text, "/") {
		return false
	}
	claim, p, matched, err := r.packets.ClaimByPassword(chatID, m.Text, packetClaimer(u))
	if !matched {
		return false
	}
	msg := r.answerClaim(claim, p, err)
	if err == nil {
		msg = fmt.Sprintf("🧧 %s 抢到 <code>%s</code> %s", htmlEscape(displayName(u)), trimZeros(claim.Amount), htmlEscape(r.packetCurrencyName(p.Currency)))
		if chatID == 0 {
			msg += "，已存入钱包"
		}
		r.refreshPacket(ctx, p.Code, "")
	} else {
		msg = htmlEscape(msg)
	}
	_ = r.client.ReplyMessage(ctx, m.Chat.ID, m.MessageID, msg, nil)
	return true
}

// askPacketPassword asks for the password of a password packet in the private chat.
func (r *Router) askPacketPassword(ctx context.Context, chatID, userID int64, code string) {
	p, _, err := r.packets.ByCode(code)
	if err != nil {
		_ = r.client.SendMessage(ctx, chatID, "红包不存在。", nil)
		return
	}
	if !r.packets.Open(p) {
		_ = r.client.SendMessage(ctx, chatID, "这个红包已领完或已过期。", nil)
		return
	}
	// Keep the message the button was pressed on, so its card can be updated after the claim.
	inline := ""
	if s, _ := r.sessions.Get(ctx, userID); s != nil && s.Scene == "redpacket_pw" && sessionString(s, "code") == code {
		inline = sessionString(s, "inline")
	}
	_ = r.sessions.Set(ctx, userID, session.Session{Scene: "redpacket_pw", Step: "password", Data: map[string]any{"code": code, "inline": inline}})
	_ = r.client.SendMessage(ctx, chatID, fmt.Sprintf("🔑 <b>%s 的口令红包</b>\n\n请发送口令领取。发送 /cancel 可取消。", htmlEscape(p.SenderName)), nil)
}

// claimWithPassword claims the packet chosen with the button using the typed password.
func (r *Router) claimWithPassword(ctx context.Context, m *Message, u *model.TelegramUser, s *session.Session) {
	code := sessionString(s, "code")
	claim, p, err := r.packets.ClaimCode(code, m.Text, packetClaimer(u))
	if err != nil && redpacket.IsUserError(err) && strings.Contains(err.Error(), "口令不对") {
		_ = r.client.SendMessage(ctx, m.Chat.ID, "口令不对，请重新发送。发送 /cancel 可取消。", nil)
		return
	}
	_ = r.sessions.Clear(ctx, m.From.ID)
	_ = r.client.SendMessage(ctx, m.Chat.ID, htmlEscape(r.answerClaim(claim, p, err)), r.homeKeyboard(r.isAdmin(m.From.ID)))
	if err == nil {
		r.refreshPacket(ctx, code, sessionString(s, "inline"))
		if sessionString(s, "inline") != "" {
			r.refreshPacket(ctx, code, "") // the group message too, if any
		}
	}
}

// handleGroupMessage handles messages in groups: /start (or the older /hb) starts the wizard
// right in the group,
// replies to the wizard carry its answers, and other text may be a red packet password. With
// privacy mode on (the default) the bot sees only commands and replies to its own messages, so
// passwords typed in the group need privacy mode off or the bot made a group admin.
func (r *Router) handleGroupMessage(ctx context.Context, m *Message, u *model.TelegramUser) {
	command := commandName(m.Text)
	s, _ := r.sessions.Get(ctx, m.From.ID)
	inWizard := s != nil && s.Scene == "redpacket" && sessionString(s, "chat") == strconv.FormatInt(m.Chat.ID, 10)
	switch {
	case command == "/start" && len(strings.Fields(m.Text)) > 1:
		// "/start@bot <payload>" is sent by Telegram when the bot is added through a startgroup
		// link: introduce the bot instead of opening a wizard for whoever added it.
		_ = r.client.SendMessage(ctx, m.Chat.ID, "🧧 红包机器人已加入本群。\n\n发送 /start 即可在群里发红包，点红包下方的「抢红包」领取。", nil)
		return
	case command == "/start" || command == "/hb" || command == "/hongbao":
		if inWizard {
			r.deleteWizard(ctx, s)
		}
		r.startGroupPacketWizard(ctx, m, u)
		return
	case command == "/cancel" && inWizard:
		r.deleteWizard(ctx, s)
		_ = r.sessions.Clear(ctx, m.From.ID)
		_ = r.client.DeleteMessage(ctx, m.Chat.ID, m.MessageID)
		return
	case inWizard && m.ReplyToMessage != nil && strconv.Itoa(m.ReplyToMessage.MessageID) == sessionString(s, "msg") && !strings.HasPrefix(m.Text, "/"):
		r.handlePacketWizardInput(ctx, m, u, s)
		// Keep the group tidy; this only works when the bot is a group admin.
		_ = r.client.DeleteMessage(ctx, m.Chat.ID, m.MessageID)
		return
	}
	r.tryPasswordClaim(ctx, m.Chat.ID, m, u)
}

// handleInlineQuery lists the user's claimable packets so they can be sent into any chat.
func (r *Router) handleInlineQuery(ctx context.Context, q *InlineQuery) {
	u := r.saveUser(&q.From)
	if u == nil || u.Status != 1 {
		return
	}
	code := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(q.Query), "hb"))
	list, err := r.packets.Active(u.ID, r.botID, 20)
	if err != nil {
		return
	}
	var results []InlineArticle
	for i := range list {
		p := &list[i]
		if code != "" && p.Code != code {
			continue
		}
		_, claims, err := r.packets.ByCode(p.Code)
		if err != nil {
			continue
		}
		results = append(results, InlineArticle{ID: p.Code,
			Title:       fmt.Sprintf("🧧 %s红包 · %s %s", redpacket.TypeNames[p.Type], trimZeros(p.Total), r.packetCurrencyName(p.Currency)),
			Description: fmt.Sprintf("%s · 已领 %d/%d", p.Greeting, p.ClaimedCount, p.Count),
			Text:        r.packets.Card(p, claims), Keyboard: r.packetKeyboard(ctx, p)})
	}
	if err := r.client.AnswerInlineQuery(ctx, q.ID, results, "🧧 发一个新红包", "hb"); err != nil {
		r.logger.Warn("answer inline query failed", zap.Error(err))
	}
}

// ---- Wizard (session scene "redpacket") ----
//
// In a private chat each step is a new message. In a group (session data "chat"/"msg") the
// wizard is one message that is edited step by step; answers are replies to it, and only the
// user who started it can press its buttons.

func newRequestID() (string, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
}

func packetTypeKeyboard() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{btn("🎲 拼手气红包", "hbw:t:"+redpacket.TypeLucky), btn("💌 普通红包", "hbw:t:"+redpacket.TypeEqual)},
		{btn("🎯 专属红包", "hbw:t:"+redpacket.TypeExclusive), btn("🔑 口令红包", "hbw:t:"+redpacket.TypePassword)},
		{btn("取消", "hbw:x")},
	}}
}

func cancelKeyboard() *InlineKeyboardMarkup {
	return &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("取消", "hbw:x")}}}
}

// startPacketWizard starts the wizard in the private chat; groupID is a group to post the packet
// in (from old "/start hb_<group>" links), or 0.
func (r *Router) startPacketWizard(ctx context.Context, chatID, userID, groupID int64) {
	id, err := newRequestID()
	if err != nil {
		return
	}
	data := map[string]any{"request_id": id}
	if groupID != 0 {
		data["group"] = strconv.FormatInt(groupID, 10)
	}
	_ = r.sessions.Set(ctx, userID, session.Session{Scene: "redpacket", Step: "type", Data: data})
	text := "<b>🧧 发红包</b>\n\n请选择红包类型："
	if groupID != 0 {
		text = "<b>🧧 发红包到群</b>\n\n设置完成后，机器人会把红包发到你刚才所在的群。\n请选择红包类型："
	}
	_ = r.client.SendMessage(ctx, chatID, text, packetTypeKeyboard())
}

// startGroupPacketWizard starts the wizard as a message in the group.
func (r *Router) startGroupPacketWizard(ctx context.Context, m *Message, u *model.TelegramUser) {
	id, err := newRequestID()
	if err != nil {
		return
	}
	owner := displayName(u)
	msgID, err := r.client.SendMessageID(ctx, m.Chat.ID, wizardTitle(owner)+"请选择红包类型：", packetTypeKeyboard())
	if err != nil {
		r.logger.Warn("start group red packet wizard failed", zap.Int64("chat_id", m.Chat.ID), zap.Error(err))
		return
	}
	chat := strconv.FormatInt(m.Chat.ID, 10)
	_ = r.sessions.Set(ctx, m.From.ID, session.Session{Scene: "redpacket", Step: "type", Data: map[string]any{
		"request_id": id, "group": chat, "chat": chat, "msg": strconv.Itoa(msgID), "owner": owner}})
	_ = r.client.DeleteMessage(ctx, m.Chat.ID, m.MessageID) // the /start command; needs admin rights
}

func wizardTitle(owner string) string {
	return "🧧 <b>" + htmlEscape(owner) + " 正在发红包</b>\n\n"
}

func inGroupWizard(s *session.Session) bool { return sessionString(s, "chat") != "" }

// wizardSay shows a wizard step: an edit of the group message, or a new private message.
// prompt marks a step that waits for typed input.
func (r *Router) wizardSay(ctx context.Context, s *session.Session, chatID int64, text string, kb *InlineKeyboardMarkup, prompt bool) {
	if !inGroupWizard(s) {
		_ = r.client.SendMessage(ctx, chatID, text, kb)
		return
	}
	if prompt {
		text += "\n\n👉 请<b>回复这条消息</b>输入"
	}
	group, _ := strconv.ParseInt(sessionString(s, "chat"), 10, 64)
	msg, _ := strconv.Atoi(sessionString(s, "msg"))
	_ = r.client.EditMessage(ctx, group, msg, wizardTitle(sessionString(s, "owner"))+text, kb)
}

func (r *Router) deleteWizard(ctx context.Context, s *session.Session) {
	group, _ := strconv.ParseInt(sessionString(s, "chat"), 10, 64)
	msg, _ := strconv.Atoi(sessionString(s, "msg"))
	if group != 0 && msg != 0 {
		_ = r.client.DeleteMessage(ctx, group, msg)
	}
}

func sessionString(s *session.Session, key string) string {
	v, _ := s.Data[key].(string)
	return v
}

// handleGroupWizardCallback lets only the user who started a group wizard press its buttons.
func (r *Router) handleGroupWizardCallback(ctx context.Context, cb *CallbackQuery, u *model.TelegramUser) {
	s, _ := r.sessions.Get(ctx, cb.From.ID)
	if s == nil || s.Scene != "redpacket" || sessionString(s, "chat") != strconv.FormatInt(cb.Message.Chat.ID, 10) || sessionString(s, "msg") != strconv.Itoa(cb.Message.MessageID) {
		_ = r.client.AnswerCallbackAlert(ctx, cb.ID, "只有发红包的人可以操作；想发红包请发送 /start", true)
		return
	}
	_ = r.client.AnswerCallback(ctx, cb.ID, "")
	if u == nil || u.Status != 1 {
		return
	}
	r.handlePacketWizardCallback(ctx, cb, u)
}

// handlePacketWizardCallback handles the wizard's "hbw:" buttons.
func (r *Router) handlePacketWizardCallback(ctx context.Context, cb *CallbackQuery, u *model.TelegramUser) {
	chatID, msgID, userID := cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID
	if cb.Data == "hbw:new" {
		r.startPacketWizard(ctx, chatID, userID, 0)
		return
	}
	s, _ := r.sessions.Get(ctx, userID)
	if cb.Data == "hbw:x" {
		_ = r.sessions.Clear(ctx, userID)
		if s != nil && inGroupWizard(s) {
			r.deleteWizard(ctx, s)
		} else {
			r.editHome(ctx, chatID, msgID, userID)
		}
		return
	}
	if s == nil || s.Scene != "redpacket" {
		_ = r.client.EditMessage(ctx, chatID, msgID, "操作已过期，请重新发红包。", &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("🧧 发红包", "hbw:new")}}})
		return
	}
	switch {
	case strings.HasPrefix(cb.Data, "hbw:t:") && s.Step == "type":
		t := strings.TrimPrefix(cb.Data, "hbw:t:")
		if _, ok := redpacket.TypeNames[t]; !ok {
			return
		}
		s.Data["type"], s.Step = t, "currency"
		_ = r.sessions.Set(ctx, userID, *s)
		balances, err := r.wallet.Balances(u.ID)
		if err != nil {
			return
		}
		var rows [][]InlineKeyboardButton
		for _, b := range balances {
			icon := b.Icon
			if icon == "" {
				icon = "₮"
			}
			label := fmt.Sprintf("%s %s（可用 %s）", icon, b.Name, trimZeros(b.Available))
			if inGroupWizard(s) {
				label = icon + " " + b.Name // balances are private
			}
			rows = append(rows, []InlineKeyboardButton{btn(label, "hbw:c:"+b.Code)})
		}
		rows = append(rows, []InlineKeyboardButton{btn("取消", "hbw:x")})
		text := "<b>" + redpacket.TypeNames[t] + "红包</b>\n\n请选择用哪种币种发红包："
		if !inGroupWizard(s) {
			text = "<b>🧧 " + redpacket.TypeNames[t] + "红包</b>\n\n请选择用哪种币种发红包："
			_ = r.client.EditMessage(ctx, chatID, msgID, text, &InlineKeyboardMarkup{InlineKeyboard: rows})
			return
		}
		r.wizardSay(ctx, s, chatID, text, &InlineKeyboardMarkup{InlineKeyboard: rows}, false)
	case strings.HasPrefix(cb.Data, "hbw:c:") && s.Step == "currency":
		cur, err := r.wallet.Currency(strings.TrimPrefix(cb.Data, "hbw:c:"))
		if err != nil {
			return
		}
		s.Data["currency"], s.Data["currency_name"], s.Step = cur.Code, cur.Name, "amount"
		_ = r.sessions.Set(ctx, userID, *s)
		prompt := map[string]string{redpacket.TypeEqual: "请输入<b>每个红包</b>的金额", redpacket.TypeExclusive: "请输入红包金额"}[sessionString(s, "type")]
		if prompt == "" {
			prompt = "请输入红包<b>总金额</b>（随机分给领取的人）"
		}
		text := fmt.Sprintf("<b>%s红包 · %s</b>\n\n%s，例如 <code>10</code>。", redpacket.TypeNames[sessionString(s, "type")], htmlEscape(cur.Name), prompt)
		if !inGroupWizard(s) {
			_ = r.client.EditMessage(ctx, chatID, msgID, "🧧 "+text+"\n发送 /cancel 可取消。", nil)
			return
		}
		r.wizardSay(ctx, s, chatID, text, cancelKeyboard(), true)
	case cb.Data == "hbw:g" && s.Step == "greeting":
		s.Data["greeting"] = ""
		r.confirmPacket(ctx, chatID, userID, s)
	case strings.HasPrefix(cb.Data, "hbw:send:") && s.Step == "confirm" && strings.TrimPrefix(cb.Data, "hbw:send:") == sessionString(s, "request_id"):
		r.sendPacket(ctx, chatID, msgID, u, s)
	}
}

// handlePacketWizardInput handles typed answers in the wizard.
func (r *Router) handlePacketWizardInput(ctx context.Context, m *Message, u *model.TelegramUser, s *session.Session) {
	text := strings.TrimSpace(m.Text)
	ask := func(t string) { r.wizardSay(ctx, s, m.Chat.ID, t, cancelKeyboard(), true) }
	switch s.Step {
	case "amount":
		d, err := decimal.NewFromString(text)
		if err != nil || !d.IsPositive() {
			ask("⚠️ 金额格式不正确，请输入数字，例如 <code>10</code>。")
			return
		}
		if cur, err := r.wallet.Currency(sessionString(s, "currency")); err == nil && !d.Equal(d.Truncate(int32(cur.Decimals))) {
			ask(fmt.Sprintf("⚠️ %s 金额最多 %d 位小数，请重新输入。", htmlEscape(cur.Name), cur.Decimals))
			return
		}
		s.Data["amount"] = d.String()
		if sessionString(s, "type") == redpacket.TypeExclusive {
			s.Step = "target"
			ask("请输入领取人的 <b>@用户名</b> 或 <b>数字 Telegram ID</b>：")
		} else {
			s.Step = "count"
			ask(fmt.Sprintf("请输入红包个数（1–%d）：", r.packets.MaxCount()))
		}
	case "count":
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > r.packets.MaxCount() {
			ask(fmt.Sprintf("⚠️ 请输入 1–%d 之间的整数。", r.packets.MaxCount()))
			return
		}
		s.Data["count"] = strconv.Itoa(n)
		if sessionString(s, "type") == redpacket.TypePassword {
			s.Step = "password"
			ask("请设置口令（1–20 个字），群友发送口令即可领取：")
		} else {
			s.Step = "greeting"
			r.askGreeting(ctx, s, m.Chat.ID)
		}
	case "target":
		if _, _, err := redpacket.ParseTarget(text); err != nil {
			ask("⚠️ " + htmlEscape(err.Error()) + "。")
			return
		}
		s.Data["target"], s.Data["count"], s.Step = text, "1", "greeting"
		r.askGreeting(ctx, s, m.Chat.ID)
	case "password":
		if n := utf8.RuneCountInString(text); n < 1 || n > 20 || strings.HasPrefix(text, "/") {
			ask("⚠️ 口令需为 1–20 个字，且不能以 / 开头，请重新输入。")
			return
		}
		s.Data["password"], s.Step = text, "greeting"
		r.askGreeting(ctx, s, m.Chat.ID)
	case "greeting":
		if utf8.RuneCountInString(text) > 40 {
			ask("⚠️ 祝福语最多 40 个字，请重新输入。")
			return
		}
		s.Data["greeting"] = text
		r.confirmPacket(ctx, m.Chat.ID, m.From.ID, s)
		return
	default:
		return
	}
	_ = r.sessions.Set(ctx, m.From.ID, *s)
}

func (r *Router) askGreeting(ctx context.Context, s *session.Session, chatID int64) {
	kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("使用「"+redpacket.DefaultGreeting+"」", "hbw:g")}, {btn("取消", "hbw:x")}}}
	r.wizardSay(ctx, s, chatID, "请输入祝福语（最多 40 字），或使用默认祝福语：", kb, true)
}

func (r *Router) confirmPacket(ctx context.Context, chatID, userID int64, s *session.Session) {
	s.Step = "confirm"
	if err := r.sessions.Set(ctx, userID, *s); err != nil {
		return
	}
	t := sessionString(s, "type")
	amount, _ := decimal.NewFromString(sessionString(s, "amount"))
	count, _ := strconv.Atoi(sessionString(s, "count"))
	total := amount
	if t == redpacket.TypeEqual || t == redpacket.TypeExclusive {
		total = amount.Mul(decimal.NewFromInt(int64(count)))
	}
	cur := htmlEscape(sessionString(s, "currency_name"))
	greeting := sessionString(s, "greeting")
	if greeting == "" {
		greeting = redpacket.DefaultGreeting
	}
	var b strings.Builder
	fmt.Fprintf(&b, "<b>确认发红包</b>\n\n类型：%s红包\n金额：<code>%s</code> %s，共 %d 个", redpacket.TypeNames[t], total.String(), cur, count)
	if t == redpacket.TypeEqual {
		fmt.Fprintf(&b, "（每个 %s）", amount.String())
	}
	if v := sessionString(s, "target"); v != "" {
		fmt.Fprintf(&b, "\n领取人：%s", htmlEscape(v))
	}
	if v := sessionString(s, "password"); v != "" {
		fmt.Fprintf(&b, "\n口令：<code>%s</code>", htmlEscape(v))
	}
	fmt.Fprintf(&b, "\n祝福语：%s\n有效期：%d 小时，未领完自动退回", htmlEscape(greeting), r.packets.ExpireHours())
	switch {
	case inGroupWizard(s):
		b.WriteString("\n\n确认后将从你的钱包扣款，红包直接发在这里。")
	case sessionString(s, "group") != "":
		b.WriteString("\n\n确认后将从钱包扣款，并把红包发到群里。")
	default:
		b.WriteString("\n\n确认后将从钱包扣款，然后可以把红包转发到任意群聊或好友。")
	}
	kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("✅ 确认发红包", "hbw:send:"+sessionString(s, "request_id")), btn("❌ 取消", "hbw:x")}}}
	r.wizardSay(ctx, s, chatID, b.String(), kb, false)
}

func (r *Router) sendPacket(ctx context.Context, chatID int64, msgID int, u *model.TelegramUser, s *session.Session) {
	count, _ := strconv.Atoi(sessionString(s, "count"))
	group, _ := strconv.ParseInt(sessionString(s, "group"), 10, 64)
	p, err := r.packets.Create(redpacket.CreateInput{BotID: r.botID, SenderUserID: u.ID, SenderTelegramID: u.TelegramUserID, SenderName: displayName(u),
		Type: sessionString(s, "type"), Currency: sessionString(s, "currency"), Amount: sessionString(s, "amount"), Count: count,
		Target: sessionString(s, "target"), Password: sessionString(s, "password"), Greeting: sessionString(s, "greeting"), ChatID: group, RequestID: sessionString(s, "request_id")})
	_ = r.sessions.Clear(ctx, u.TelegramUserID)
	if err != nil {
		msg := "发红包失败，请稍后再试。"
		if redpacket.IsUserError(err) {
			msg = "发红包失败：" + htmlEscape(err.Error())
		} else {
			r.logger.Error("create red packet failed", zap.Error(err))
		}
		if inGroupWizard(s) {
			// The wizard message stays as the error; the sender can top up in the private chat.
			var kb *InlineKeyboardMarkup
			if name, err := r.client.Username(ctx); err == nil && name != "" {
				kb = &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "💰 去机器人查看钱包", URL: "https://t.me/" + name}}}}
			}
			_ = r.client.EditMessage(ctx, chatID, msgID, wizardTitle(sessionString(s, "owner"))+msg+"\n\n可以重新发送 /start", kb)
			return
		}
		again := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("🧧 重新发红包", "hbw:new")}, {btn("⬅ 返回首页", "home")}}}
		_ = r.client.EditMessage(ctx, chatID, msgID, msg, again)
		return
	}
	if inGroupWizard(s) {
		// The wizard message becomes the packet card.
		_ = r.packets.SetMessage(p.ID, chatID, msgID)
		p.ChatID, p.MessageID = chatID, msgID
		_ = r.client.EditMessage(ctx, chatID, msgID, r.packets.Card(p, nil), r.packetKeyboard(ctx, p))
		return
	}
	note := "✅ 红包已创建。点击「发送到群聊/好友」选择聊天即可发出。"
	if group != 0 {
		id, err := r.client.SendMessageID(ctx, group, r.packets.Card(p, nil), r.packetKeyboard(ctx, p))
		if err == nil {
			_ = r.packets.SetMessage(p.ID, group, id)
			p.ChatID, p.MessageID = group, id
			note = "✅ 红包已发到群里。也可以点击「发送到群聊/好友」转发到其他聊天。"
		} else {
			r.logger.Warn("post red packet to group failed", zap.Int64("chat_id", group), zap.Error(err))
			note = "⚠️ 红包已创建，但没能发到群里（请确认机器人在群内且可以发言）。可点击「发送到群聊/好友」手动发出。"
		}
	}
	_ = r.client.EditMessage(ctx, chatID, msgID, note, nil)
	kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "📤 发送到群聊/好友", SwitchInlineQuery: "hb " + p.Code}}, {btn("🧧 再发一个", "hbw:new")}}}
	_ = r.client.SendMessage(ctx, chatID, r.packets.Card(p, nil), kb)
}
