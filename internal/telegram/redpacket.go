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

// Red packets are created in a private chat (menu "redpacket", /hb, or the deep link
// /start hb_<groupID> offered by /hb in a group), then posted by the bot in that group or shared
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
	"• 发红包：点「发红包」，或在群里发送 /hb\n" +
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

// packetKeyboard is the claim button (while claimable) and a link to send one's own packet.
func (r *Router) packetKeyboard(ctx context.Context, p *model.RedPacket) *InlineKeyboardMarkup {
	var rows [][]InlineKeyboardButton
	if r.packets.Open(p) {
		label := "🧧 抢红包"
		if p.Type == redpacket.TypePassword {
			label = "🔑 发送口令领取"
		}
		rows = append(rows, []InlineKeyboardButton{btn(label, "hb:"+p.Code)})
	}
	if name, err := r.client.Username(ctx); err == nil && name != "" {
		rows = append(rows, []InlineKeyboardButton{{Text: "我也发红包", URL: "https://t.me/" + name + "?start=hb"}})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

// refreshPacket redraws the packet message that was clicked (inline mode) or posted in a group.
func (r *Router) refreshPacket(ctx context.Context, code, inlineMessageID string) {
	p, claims, err := r.packets.ByCode(code)
	if err != nil {
		return
	}
	text, kb := r.packets.Card(p, claims), r.packetKeyboard(ctx, p)
	if inlineMessageID != "" {
		err = r.client.EditInlineMessage(ctx, inlineMessageID, text, kb)
	} else if p.ChatID != 0 && p.MessageID != 0 {
		err = r.client.EditMessage(ctx, p.ChatID, p.MessageID, text, kb)
	}
	if err != nil && !strings.Contains(err.Error(), "message is not modified") {
		r.logger.Debug("red packet card update failed", zap.String("code", code), zap.Error(err))
	}
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
	claim, p, err := r.packets.Claim(code, packetClaimer(u))
	_ = r.client.AnswerCallbackAlert(ctx, cb.ID, r.answerClaim(claim, p, err), true)
	if err == nil {
		r.refreshPacket(ctx, code, cb.InlineMessageID)
	}
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

// handleGroupMessage handles messages in groups: /hb and red packet passwords. With privacy
// mode on (the default) the bot only sees commands, so passwords need privacy mode off or the
// bot made a group admin.
func (r *Router) handleGroupMessage(ctx context.Context, m *Message, u *model.TelegramUser) {
	switch commandName(m.Text) {
	case "/hb", "/hongbao":
		name, err := r.client.Username(ctx)
		if err != nil || name == "" {
			return
		}
		kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "🧧 去私聊发红包", URL: fmt.Sprintf("https://t.me/%s?start=hb_%d", name, m.Chat.ID)}}}}
		_ = r.client.ReplyMessage(ctx, m.Chat.ID, m.MessageID, "点击下方按钮，在私聊里设置红包，发出后会自动出现在本群。", kb)
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

// ---- Private-chat wizard (session scene "redpacket") ----

func (r *Router) startPacketWizard(ctx context.Context, chatID, userID, groupID int64) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	data := map[string]any{"request_id": hex.EncodeToString(nonce)}
	if groupID != 0 {
		data["group"] = strconv.FormatInt(groupID, 10)
	}
	_ = r.sessions.Set(ctx, userID, session.Session{Scene: "redpacket", Step: "type", Data: data})
	text := "<b>🧧 发红包</b>\n\n请选择红包类型："
	if groupID != 0 {
		text = "<b>🧧 发红包到群</b>\n\n设置完成后，机器人会把红包发到你刚才所在的群。\n请选择红包类型："
	}
	kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{
		{btn("🎲 拼手气红包", "hbw:t:"+redpacket.TypeLucky), btn("💌 普通红包", "hbw:t:"+redpacket.TypeEqual)},
		{btn("🎯 专属红包", "hbw:t:"+redpacket.TypeExclusive), btn("🔑 口令红包", "hbw:t:"+redpacket.TypePassword)},
		{btn("取消", "home")},
	}}
	_ = r.client.SendMessage(ctx, chatID, text, kb)
}

func sessionString(s *session.Session, key string) string {
	v, _ := s.Data[key].(string)
	return v
}

// handlePacketWizardCallback handles the wizard's "hbw:" buttons.
func (r *Router) handlePacketWizardCallback(ctx context.Context, cb *CallbackQuery, u *model.TelegramUser) {
	chatID, msgID, userID := cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID
	if cb.Data == "hbw:new" {
		r.startPacketWizard(ctx, chatID, userID, 0)
		return
	}
	s, _ := r.sessions.Get(ctx, userID)
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
			rows = append(rows, []InlineKeyboardButton{btn(fmt.Sprintf("%s %s（可用 %s）", icon, b.Name, trimZeros(b.Available)), "hbw:c:"+b.Code)})
		}
		rows = append(rows, []InlineKeyboardButton{btn("取消", "home")})
		_ = r.client.EditMessage(ctx, chatID, msgID, "<b>🧧 "+redpacket.TypeNames[t]+"红包</b>\n\n请选择用哪种币种发红包：", &InlineKeyboardMarkup{InlineKeyboard: rows})
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
		_ = r.client.EditMessage(ctx, chatID, msgID, fmt.Sprintf("<b>🧧 %s红包 · %s</b>\n\n%s，例如 <code>10</code>。\n发送 /cancel 可取消。", redpacket.TypeNames[sessionString(s, "type")], htmlEscape(cur.Name), prompt), nil)
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
	reply := func(t string, kb *InlineKeyboardMarkup) { _ = r.client.SendMessage(ctx, m.Chat.ID, t, kb) }
	switch s.Step {
	case "amount":
		d, err := decimal.NewFromString(text)
		if err != nil || !d.IsPositive() {
			reply("金额格式不正确，请输入数字，例如 <code>10</code>。", nil)
			return
		}
		if cur, err := r.wallet.Currency(sessionString(s, "currency")); err == nil && !d.Equal(d.Truncate(int32(cur.Decimals))) {
			reply(fmt.Sprintf("%s 金额最多 %d 位小数，请重新输入。", htmlEscape(cur.Name), cur.Decimals), nil)
			return
		}
		s.Data["amount"] = d.String()
		if sessionString(s, "type") == redpacket.TypeExclusive {
			s.Step = "target"
			reply("请输入领取人的 <b>@用户名</b> 或 <b>数字 Telegram ID</b>：", nil)
		} else {
			s.Step = "count"
			reply(fmt.Sprintf("请输入红包个数（1–%d）：", r.packets.MaxCount()), nil)
		}
	case "count":
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 || n > r.packets.MaxCount() {
			reply(fmt.Sprintf("请输入 1–%d 之间的整数。", r.packets.MaxCount()), nil)
			return
		}
		s.Data["count"] = strconv.Itoa(n)
		if sessionString(s, "type") == redpacket.TypePassword {
			s.Step = "password"
			reply("请设置口令（1–20 个字），群友发送口令即可领取：", nil)
		} else {
			s.Step = "greeting"
			r.askGreeting(ctx, m.Chat.ID)
		}
	case "target":
		if _, _, err := redpacket.ParseTarget(text); err != nil {
			reply(htmlEscape(err.Error())+"。", nil)
			return
		}
		s.Data["target"], s.Data["count"], s.Step = text, "1", "greeting"
		r.askGreeting(ctx, m.Chat.ID)
	case "password":
		if n := utf8.RuneCountInString(text); n < 1 || n > 20 || strings.HasPrefix(text, "/") {
			reply("口令需为 1–20 个字，且不能以 / 开头，请重新输入。", nil)
			return
		}
		s.Data["password"], s.Step = text, "greeting"
		r.askGreeting(ctx, m.Chat.ID)
	case "greeting":
		if utf8.RuneCountInString(text) > 40 {
			reply("祝福语最多 40 个字，请重新输入。", nil)
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

func (r *Router) askGreeting(ctx context.Context, chatID int64) {
	kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("使用「"+redpacket.DefaultGreeting+"」", "hbw:g")}, {btn("取消", "home")}}}
	_ = r.client.SendMessage(ctx, chatID, "请输入祝福语（最多 40 字），或使用默认祝福语：", kb)
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
	if sessionString(s, "group") != "" {
		b.WriteString("\n\n确认后将从钱包扣款，并把红包发到群里。")
	} else {
		b.WriteString("\n\n确认后将从钱包扣款，然后可以把红包转发到任意群聊或好友。")
	}
	kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("✅ 确认发红包", "hbw:send:"+sessionString(s, "request_id")), btn("❌ 取消", "home")}}}
	_ = r.client.SendMessage(ctx, chatID, b.String(), kb)
}

func (r *Router) sendPacket(ctx context.Context, chatID int64, msgID int, u *model.TelegramUser, s *session.Session) {
	count, _ := strconv.Atoi(sessionString(s, "count"))
	group, _ := strconv.ParseInt(sessionString(s, "group"), 10, 64)
	p, err := r.packets.Create(redpacket.CreateInput{BotID: r.botID, SenderUserID: u.ID, SenderTelegramID: u.TelegramUserID, SenderName: displayName(u),
		Type: sessionString(s, "type"), Currency: sessionString(s, "currency"), Amount: sessionString(s, "amount"), Count: count,
		Target: sessionString(s, "target"), Password: sessionString(s, "password"), Greeting: sessionString(s, "greeting"), RequestID: sessionString(s, "request_id")})
	again := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{btn("🧧 重新发红包", "hbw:new")}, {btn("⬅ 返回首页", "home")}}}
	if err != nil {
		_ = r.sessions.Clear(ctx, u.TelegramUserID)
		msg := "发红包失败，请稍后再试。"
		if redpacket.IsUserError(err) {
			msg = "发红包失败：" + htmlEscape(err.Error())
		} else {
			r.logger.Error("create red packet failed", zap.Error(err))
		}
		_ = r.client.EditMessage(ctx, chatID, msgID, msg, again)
		return
	}
	_ = r.sessions.Clear(ctx, u.TelegramUserID)
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
