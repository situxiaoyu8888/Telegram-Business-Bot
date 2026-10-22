package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"telegram-business-bot/internal/lease"
	"telegram-business-bot/internal/menu"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/referral"
	"telegram-business-bot/internal/session"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/support"
	"telegram-business-bot/internal/wallet"

	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm/clause"
)

type Router struct {
	botID           uint64
	client          *Client
	store           *store.Store
	sessions        *session.Manager
	wallet          *wallet.Service
	menus           *menu.Service
	support         *support.Service
	referral        *referral.Service
	adminIDs        map[int64]struct{}
	fallbackDeposit string
	adminOnly       bool
	menuButton      MenuButton
	logger          *zap.Logger
}

func NewRouter(c *Client, st *store.Store, sessions *session.Manager, w *wallet.Service, menus *menu.Service, sup *support.Service, ref *referral.Service, adminIDs map[int64]struct{}, fallback string, adminOnly bool, logger *zap.Logger) *Router {
	return &Router{client: c, store: st, sessions: sessions, wallet: w, menus: menus, support: sup, referral: ref, adminIDs: adminIDs, fallbackDeposit: fallback, adminOnly: adminOnly, logger: logger}
}
func (r *Router) Handle(ctx context.Context, u Update) error {
	var userID int64
	if u.Message != nil && u.Message.From != nil {
		userID = u.Message.From.ID
	} else if u.CallbackQuery != nil {
		userID = u.CallbackQuery.From.ID
	}
	if userID <= 0 {
		return nil
	}
	return lease.Run(ctx, r.store.Redis, fmt.Sprintf("tg:lock:%t:%d:%d", r.adminOnly, r.botID, userID), func(ctx context.Context) error {
		private := u.Message != nil && u.Message.Chat.ID == userID || u.CallbackQuery != nil && u.CallbackQuery.Message != nil && u.CallbackQuery.Message.Chat.ID == userID
		first := false
		if private && !r.adminOnly {
			res := r.store.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.BotSubscription{BotID: r.botID, TelegramUserID: userID})
			if res.Error != nil {
				return res.Error
			}
			// A new subscription row means this user is activating this bot for the first time.
			first = res.RowsAffected == 1
		}
		r.handle(ctx, u, first)
		return ctx.Err()
	})
}
func (r *Router) handle(ctx context.Context, u Update, first bool) {
	if u.Message != nil {
		r.handleMessage(ctx, u.Message, first)
	} else if u.CallbackQuery != nil {
		r.handleCallback(ctx, u.CallbackQuery)
	}
}
func (r *Router) isAdmin(id int64) bool { _, ok := r.adminIDs[id]; return ok }
func (r *Router) saveUser(u *User) *model.TelegramUser {
	if u == nil {
		return nil
	}
	m, err := r.store.UpsertTelegramUser(u.ID, u.Username, u.FirstName, u.LastName, u.LanguageCode)
	if err != nil {
		r.logger.Error("upsert user", zap.Error(err))
		return nil
	}
	return m
}
func (r *Router) homeKeyboard(isAdmin bool) *InlineKeyboardMarkup {
	var buttons []menu.Button
	var err error
	if r.adminOnly {
		buttons, err = r.menus.AdminRoot()
	} else {
		buttons, err = r.menus.Root(isAdmin)
	}
	if err != nil {
		return ptrKB(HomeKeyboard(isAdmin))
	}
	return buttonsToKB(buttons, false)
}
func buttonsToKB(bs []menu.Button, back bool) *InlineKeyboardMarkup {
	rows := [][]InlineKeyboardButton{}
	last := -999
	for _, b := range bs {
		btn := InlineKeyboardButton{Text: b.Text, CallbackData: b.CallbackData, URL: b.URL}
		if len(rows) == 0 || b.Row != last {
			rows = append(rows, []InlineKeyboardButton{})
			last = b.Row
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], btn)
	}
	if back {
		rows = append(rows, []InlineKeyboardButton{{Text: "⬅ 返回首页", CallbackData: "home"}})
	}
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (r *Router) handleMessage(ctx context.Context, m *Message, first bool) {
	u := r.saveUser(m.From)
	if u == nil || u.Status != 1 || m.Chat.ID != m.From.ID {
		return
	}
	if r.adminOnly && !r.isAdmin(m.From.ID) {
		_ = r.client.SendMessage(ctx, m.Chat.ID, "无管理员权限。", nil)
		return
	}
	if first {
		r.sendWelcome(ctx, m.Chat.ID)
	}
	text := strings.TrimSpace(m.Text)
	command := commandName(text)
	if command == "/start" {
		_ = r.sessions.Clear(ctx, m.From.ID)
		parts := strings.Fields(text)
		var shared []string
		if len(parts) > 1 {
			code := parts[1]
			// Shop share links carry "p<productID>_<code>" (a product) or "g<groupID>_<code>" (a group buy).
			if m := sharePayload.FindStringSubmatch(code); m != nil {
				shared, code = m[1:3], m[3]
			}
			_ = r.referral.BindByCode(u.ID, code)
		}
		r.sendHome(ctx, m.Chat.ID, m.From.ID)
		if shared != nil && r.menuButton.URL != "" && !r.adminOnly {
			text, button, fragment := "好友给你分享了一个商品，点击下方按钮查看：", "🛍 查看商品", "/product/"+shared[1]
			if shared[0] == "g" {
				text, button, fragment = "好友邀请你一起拼团，点击下方按钮参团：", "👥 去参团", "/group/"+shared[1]
			}
			kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{ShopButton(button, r.menuButton.URL, fragment)}}}
			_ = r.client.SendMessage(ctx, m.Chat.ID, text, kb)
		}
		return
	}
	switch command {
	case "/menu", ReplyHomeText:
		_ = r.sessions.Clear(ctx, m.From.ID)
		r.sendHome(ctx, m.Chat.ID, m.From.ID)
		return
	case "/orders":
		if !r.adminOnly {
			r.sendShopOrders(ctx, m.Chat.ID, u.ID)
			return
		}
	case "/cancel":
		_ = r.sessions.Clear(ctx, m.From.ID)
		_ = r.client.SendMessage(ctx, m.Chat.ID, "已取消当前操作。", r.homeKeyboard(r.isAdmin(m.From.ID)))
		return
	case "/contactus":
		r.dispatchMenu(ctx, &CallbackQuery{From: *m.From, Message: &Message{Chat: m.Chat}}, u, model.MenuItem{ActionType: "builtin", ActionValue: "support"})
		return
	case "/admin":
		if r.isAdmin(m.From.ID) {
			_ = r.client.SendMessage(ctx, m.Chat.ID, "<b>🛡 管理入口</b>\n请选择管理功能；完整配置请使用 Web 管理后台 <code>/admin/</code>。", r.homeKeyboard(true))
		} else {
			_ = r.client.SendMessage(ctx, m.Chat.ID, "无管理员权限。", r.homeKeyboard(false))
		}
		return
	}
	if item, err := r.menus.ReplyByTitle(text, r.isAdmin(m.From.ID), r.adminOnly); err != nil {
		_ = r.client.SendMessage(ctx, m.Chat.ID, "菜单暂不可用，请稍后重试。", nil)
		return
	} else if item != nil {
		if err := r.sessions.Clear(ctx, m.From.ID); err != nil {
			return
		}
		if item.ActionType == "merchants" {
			if !r.searchMerchants(ctx, &CallbackQuery{From: *m.From, Message: &Message{Chat: m.Chat}}, text, false) {
				r.dispatchMenu(ctx, &CallbackQuery{From: *m.From, Message: &Message{Chat: m.Chat}}, u, *item)
			}
		} else {
			r.dispatchMenu(ctx, &CallbackQuery{From: *m.From, Message: &Message{Chat: m.Chat}}, u, *item)
		}
		return
	}

	s, err := r.sessions.Get(ctx, m.From.ID)
	if err == nil && s != nil {
		r.handleSessionInput(ctx, m, u, s)
		return
	}
	if err != nil {
		_ = r.client.SendMessage(ctx, m.Chat.ID, "当前操作状态暂不可用，请稍后重试。", nil)
		return
	}
	if r.searchMerchants(ctx, &CallbackQuery{From: *m.From, Message: &Message{Chat: m.Chat}}, text, false) {
		return
	}
	_ = r.client.SendMessage(ctx, m.Chat.ID, "请选择菜单操作，或发送分类关键词（如：烧烤）搜索商家。", r.homeKeyboard(r.isAdmin(m.From.ID)))
}

func (r *Router) handleCallback(ctx context.Context, cb *CallbackQuery) {
	u := r.saveUser(&cb.From)
	if r.adminOnly && !r.isAdmin(cb.From.ID) {
		_ = r.client.AnswerCallback(ctx, cb.ID, "无管理员权限")
		return
	}
	_ = r.client.AnswerCallback(ctx, cb.ID, "")
	if cb.Message == nil || u == nil || u.Status != 1 || cb.Message.Chat.ID != cb.From.ID {
		return
	}
	chatID, msgID, userID := cb.Message.Chat.ID, cb.Message.MessageID, cb.From.ID
	r.store.LogAction(userID, "callback", map[string]any{"data": cb.Data})
	if cb.Data == "home" {
		_ = r.sessions.Clear(ctx, userID)
		r.sendHome(ctx, chatID, userID)
		return
	}
	if strings.HasPrefix(cb.Data, "search:") {
		r.searchMerchants(ctx, cb, strings.TrimPrefix(cb.Data, "search:"), true)
		return
	}
	if strings.HasPrefix(cb.Data, "recommend:") {
		r.handleRecommendationCallback(ctx, cb)
		return
	}
	if strings.HasPrefix(cb.Data, "shops:") || strings.HasPrefix(cb.Data, "shop:") {
		r.handleMerchantCallback(ctx, cb)
		return
	}

	if strings.HasPrefix(cb.Data, "menu:") {
		code := strings.TrimPrefix(cb.Data, "menu:")
		item, err := r.menus.ByCode(code, r.isAdmin(userID))
		if err != nil {
			return
		}
		if item.Role == "admin" && !r.isAdmin(userID) {
			return
		}
		r.dispatchMenu(ctx, cb, u, item)
		return
	}
	switch {
	case strings.HasPrefix(cb.Data, "withdraw:cur:"), cb.Data == "withdraw:network:TRC20":
		// "withdraw:network:TRC20" comes from buttons sent before withdrawals had currencies.
		code := strings.TrimPrefix(cb.Data, "withdraw:cur:")
		if cb.Data == "withdraw:network:TRC20" {
			code = "USDT"
		}
		cur, err := r.wallet.Currency(code)
		if err != nil || !cur.Withdrawable {
			_ = r.client.EditMessage(ctx, chatID, msgID, "该币种暂不支持提现。", ptrKB(BackHomeKeyboard()))
			return
		}
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return
		}
		requestID := hex.EncodeToString(nonce)
		_ = r.sessions.Set(ctx, userID, session.Session{Scene: "withdraw", Step: "input_amount", Data: map[string]any{"currency": cur.Code, "name": cur.Name, "kind": cur.Kind, "request_id": requestID}})
		rule := ""
		if cur.Code != "USDT" {
			rule = fmt.Sprintf("\n最低提现：<code>%s</code>，每笔手续费：<code>%s</code>", trimZeros(cur.MinWithdraw), trimZeros(cur.WithdrawFee))
		}
		_ = r.client.EditMessage(ctx, chatID, msgID, "<b>💸 提现 - "+htmlEscape(cur.Name)+"</b>\n\n请输入提现金额，例如 <code>100.5</code>。"+rule+"\n发送 /cancel 可取消。", ptrKB(BackHomeKeyboard()))
	case strings.HasPrefix(cb.Data, "withdraw:confirm:"):
		s, _ := r.sessions.Get(ctx, userID)
		if s == nil || s.Scene != "withdraw" || s.Step != "confirm" || fmt.Sprint(s.Data["request_id"]) != strings.TrimPrefix(cb.Data, "withdraw:confirm:") {
			return
		}
		amount := fmt.Sprint(s.Data["amount"])
		address := fmt.Sprint(s.Data["address"])
		order, err := r.wallet.CreateCurrencyWithdrawal(u.ID, withdrawCurrency(s), address, amount, fmt.Sprint(s.Data["request_id"]))
		if err != nil {
			_ = r.client.EditMessage(ctx, chatID, msgID, "提现提交失败："+htmlEscape(err.Error()), ptrKB(BackHomeKeyboard()))
			return
		}
		_ = r.sessions.Clear(ctx, userID)
		_ = r.client.EditMessage(ctx, chatID, msgID, fmt.Sprintf("<b>✅ 提现申请已提交</b>\n\n订单：<code>#%d</code>\n状态：待审核\n金额：<code>%s %s</code>\n\n%s", order.ID, trimZeros(order.Amount), htmlEscape(withdrawName(s)), map[bool]string{true: "审核通过后由财务人工打款。", false: "审核通过后由独立钱包/签名服务执行链上转账。"}[order.Network == "BANK"]), ptrKB(BackHomeKeyboard()))
	case cb.Data == "withdraw:cancel":
		_ = r.sessions.Clear(ctx, userID)
		r.sendHome(ctx, chatID, userID)

	}
}

func (r *Router) dispatchMenu(ctx context.Context, cb *CallbackQuery, u *model.TelegramUser, item model.MenuItem) {
	chatID, msgID := cb.Message.Chat.ID, cb.Message.MessageID
	action := item.ActionValue
	if item.ActionType == "photo" {
		kb := &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: item.ButtonText, URL: item.ActionValue}}}}
		if err := r.client.SendPhoto(ctx, chatID, item.ImageURL, item.Description, "", kb); err != nil {
			_ = r.client.SendMessage(ctx, chatID, "图片暂时发送失败，请稍后重试。", nil)
		}
		return
	}
	if item.ActionType == "merchants" {
		r.showMerchants(ctx, cb, item.ID, 0)
		return
	}
	if item.ActionType == "url" {
		_ = r.menuResponse(ctx, chatID, msgID, "<b>"+htmlEscape(item.Title)+"</b>\n\n"+htmlEscape(item.Description), &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{{Text: "打开链接", URL: item.ActionValue}}}})
		return
	}
	if item.ActionType == "text" {
		content := item.Description
		if content == "" {
			content = item.ActionValue
		}
		if content == "" {
			content = item.Title
		}
		_ = r.menuResponse(ctx, chatID, msgID, htmlEscape(content), ptrKB(BackHomeKeyboard()))
		return
	}

	if item.ActionType == "submenu" {
		replyItems, err := r.menus.ReplyChildren(item.ID, r.isAdmin(cb.From.ID), r.adminOnly)
		if err != nil {
			_ = r.menuResponse(ctx, chatID, msgID, "菜单加载失败，请稍后重试。", ptrKB(BackHomeKeyboard()))
			return
		}
		bs, err := r.menus.Children(item.ID, r.isAdmin(cb.From.ID))
		if err != nil {
			_ = r.menuResponse(ctx, chatID, msgID, "菜单加载失败，请稍后重试。", ptrKB(BackHomeKeyboard()))
			return
		}
		text := "<b>" + htmlEscape(item.Title) + "</b>\n\n" + htmlEscape(item.Description)
		if len(replyItems) > 0 || item.Placement == "reply" {
			prompt := "请点击 👇"
			if item.Description != "" {
				prompt = htmlEscape(item.Description) + "\n\n" + prompt
			}
			if err := r.client.SendReplyKeyboard(ctx, chatID, prompt, submenuReplyItems(replyItems)); err != nil {
				return
			}
			r.sendMerchantRecommendations(ctx, cb, item.ID)
			if len(bs) > 0 {
				_ = r.client.SendMessage(ctx, chatID, "更多分类：", buttonsToKB(bs, true))
			}
		} else {
			_ = r.menuResponse(ctx, chatID, msgID, text, buttonsToKB(bs, true))
		}
		return
	}

	switch action {
	case "profile":
		parent := "-"
		if u.ParentUserID != nil {
			parent = fmt.Sprint(*u.ParentUserID)
		}
		text := fmt.Sprintf("<b>👤 个人中心</b>\n\nTelegram ID：<code>%d</code>\n用户名：@%s\n推广码：<code>%s</code>\n上级用户ID：<code>%s</code>", u.TelegramUserID, empty(u.Username, "-"), u.ReferralCode, parent)
		_ = r.menuResponse(ctx, chatID, msgID, text, ptrKB(BackHomeKeyboard()))
	case "wallet":
		balances, err := r.wallet.Balances(u.ID)
		if err != nil {
			return
		}
		var b strings.Builder
		b.WriteString("<b>💰 我的钱包</b>\n")
		for _, x := range balances {
			// USDT (the shop currency) is always listed; other currencies once they hold money.
			if x.Code != "USDT" && isZero(x.Available) && isZero(x.Frozen) {
				continue
			}
			icon := x.Icon
			if icon == "" && x.Kind != wallet.KindFiat {
				icon = "₮"
			}
			fmt.Fprintf(&b, "\n<b>%s %s</b>\n可用：<code>%s</code>", htmlEscape(icon), htmlEscape(x.Name), x.Available)
			if !isZero(x.Frozen) {
				fmt.Fprintf(&b, "　冻结：<code>%s</code>", x.Frozen)
			}
		}
		_ = r.menuResponse(ctx, chatID, msgID, b.String(), ptrKB(BackHomeKeyboard()))
	case "deposit":
		var a model.DepositAddress
		err := r.store.DB.Where("user_id=? AND network='TRC20' AND status=1", u.ID).First(&a).Error
		addr := a.Address
		if err != nil {
			addr = ""
		}
		notice := r.menus.Config("deposit_notice", "仅支持 USDT TRC20")
		if addr == "" {
			_ = r.menuResponse(ctx, chatID, msgID, "<b>💵 USDT TRC20 充值</b>\n\n尚未为你的账号分配充值地址，请联系管理员在后台绑定。", ptrKB(BackHomeKeyboard()))
			return
		}
		_ = r.menuResponse(ctx, chatID, msgID, "<b>💵 USDT TRC20 充值</b>\n\n地址：\n<code>"+htmlEscape(addr)+"</code>\n\n⚠️ "+htmlEscape(notice), ptrKB(BackHomeKeyboard()))
	case "withdraw":
		_ = r.sessions.Set(ctx, u.TelegramUserID, session.Session{Scene: "withdraw", Step: "select_network", Data: map[string]any{}})
		list, err := r.wallet.Currencies(true)
		if err != nil {
			return
		}
		_ = r.menuResponse(ctx, chatID, msgID, "<b>💸 提现</b>\n\n请选择提现币种：", ptrKB(WithdrawKeyboard(list)))
	case "records":
		var rows []model.WalletTransaction
		_ = r.store.DB.Where("user_id=?", u.ID).Order("id desc").Limit(10).Find(&rows).Error
		var b strings.Builder
		b.WriteString("<b>📜 最近资金记录</b>\n\n")
		for _, x := range rows {
			b.WriteString(fmt.Sprintf("#%d  %s  <code>%s</code>\n", x.ID, htmlEscape(x.Type), x.Amount))
		}
		if len(rows) == 0 {
			b.WriteString("暂无记录")
		}
		_ = r.menuResponse(ctx, chatID, msgID, b.String(), ptrKB(BackHomeKeyboard()))
	case "referral":
		count, total, _ := r.referral.Stats(u.ID)
		// The link names the bot the user is talking to, so business bots invite to themselves.
		link := "邀请链接格式：<code>https://t.me/&lt;bot_username&gt;?start=" + htmlEscape(u.ReferralCode) + "</code>"
		if name, err := r.client.Username(ctx); err == nil && name != "" {
			link = "邀请链接：" + htmlEscape("https://t.me/"+name+"?start="+u.ReferralCode)
		} else if r.logger != nil {
			r.logger.Warn("referral link: bot username unavailable", zap.Error(err))
		}
		_ = r.menuResponse(ctx, chatID, msgID, fmt.Sprintf("<b>🎁 推广代理</b>\n\n推广码：<code>%s</code>\n下级人数：<code>%d</code>\n累计佣金：<code>%s USDT</code>\n\n%s", u.ReferralCode, count, total.StringFixed(6), link), ptrKB(BackHomeKeyboard()))
	case "support":
		if err := r.sessions.Set(ctx, u.TelegramUserID, session.Session{Scene: "support", Step: "input_content"}); err != nil {
			_ = r.menuResponse(ctx, chatID, msgID, "客服暂不可用，请稍后重试。", nil)
			return
		}
		_ = r.menuResponse(ctx, chatID, msgID, "<b>🎧 在线客服</b>\n\n"+htmlEscape(r.menus.Config("support_text", "请描述你的问题。"))+"\n\n发送 /cancel 可取消。", ptrKB(BackHomeKeyboard()))
	case "admin_dashboard":
		if r.isAdmin(u.TelegramUserID) {
			var users, pending, tickets int64
			_ = r.store.DB.Model(&model.TelegramUser{}).Count(&users).Error
			_ = r.store.DB.Model(&model.WithdrawalOrder{}).Where("status='pending'").Count(&pending).Error
			_ = r.store.DB.Model(&model.SupportTicket{}).Where("status='open'").Count(&tickets).Error
			_ = r.menuResponse(ctx, chatID, msgID, fmt.Sprintf("<b>📊 管理概览</b>\n\n用户：<code>%d</code>\n待审提现：<code>%d</code>\n待处理工单：<code>%d</code>", users, pending, tickets), ptrKB(BackHomeKeyboard()))
		}
	case "admin_withdrawals":
		if r.isAdmin(u.TelegramUserID) {
			var rows []model.WithdrawalOrder
			_ = r.store.DB.Where("status='pending'").Order("id asc").Limit(10).Find(&rows).Error
			var b strings.Builder
			b.WriteString("<b>💸 待审提现</b>\n\n")
			for _, x := range rows {
				b.WriteString(fmt.Sprintf("#%d 用户%d <code>%s %s</code>\n", x.ID, x.UserID, trimZeros(x.Amount), x.Currency))
			}
			if len(rows) == 0 {
				b.WriteString("暂无待审提现")
			}
			b.WriteString("\n\n请在 Web 后台审核。")
			_ = r.menuResponse(ctx, chatID, msgID, b.String(), ptrKB(BackHomeKeyboard()))
		}
	case "admin_tickets":
		if r.isAdmin(u.TelegramUserID) {
			var rows []model.SupportTicket
			_ = r.store.DB.Where("status='open'").Order("id asc").Limit(10).Find(&rows).Error
			var b strings.Builder
			b.WriteString("<b>🎧 待处理工单</b>\n\n")
			for _, x := range rows {
				b.WriteString(fmt.Sprintf("#%d TG:%d %s\n", x.ID, x.TelegramUserID, htmlEscape(x.Content)))
			}
			if len(rows) == 0 {
				b.WriteString("暂无待处理工单")
			}
			b.WriteString("\n\n请在 Web 后台回复。")
			_ = r.menuResponse(ctx, chatID, msgID, b.String(), ptrKB(BackHomeKeyboard()))
		}
	case "admin":
		if r.isAdmin(u.TelegramUserID) {
			_ = r.menuResponse(ctx, chatID, msgID, "<b>🛡 Web 管理后台</b>\n\n浏览器访问服务地址的 <code>/admin/</code>。\n支持用户、钱包、提现审核、充值地址、工单、菜单和系统配置管理。", ptrKB(BackHomeKeyboard()))
		}
	default:
		_ = r.menuResponse(ctx, chatID, msgID, "功能正在配置中。", ptrKB(BackHomeKeyboard()))
	}
}

func (r *Router) handleSessionInput(ctx context.Context, m *Message, u *model.TelegramUser, s *session.Session) {
	switch s.Scene {
	case "withdraw":
		switch s.Step {
		case "input_amount":
			amount, err := decimal.NewFromString(strings.TrimSpace(m.Text))
			if err != nil || amount.LessThanOrEqual(decimal.Zero) || !amount.Equal(amount.Truncate(6)) {
				_ = r.client.SendMessage(ctx, m.Chat.ID, "金额格式不正确，请重新输入。", nil)
				return
			}
			s.Data["amount"] = amount.Truncate(6).StringFixed(6)
			s.Step = "input_address"
			_ = r.sessions.Set(ctx, m.From.ID, *s)
			_ = r.client.SendMessage(ctx, m.Chat.ID, "请输入"+wallet.WithdrawTarget(withdrawKind(s))+"：", nil)
		case "input_address":
			addr := strings.TrimSpace(m.Text)
			if err := wallet.CheckWithdrawAddress(withdrawKind(s), addr); err != nil {
				_ = r.client.SendMessage(ctx, m.Chat.ID, err.Error()+"。", nil)
				return
			}
			s.Data["address"] = addr
			s.Step = "confirm"
			_ = r.sessions.Set(ctx, m.From.ID, *s)
			text := fmt.Sprintf("<b>确认提现</b>\n\n币种：<code>%s</code>\n金额：<code>%s</code>\n%s：<code>%s</code>", htmlEscape(withdrawName(s)), htmlEscape(trimZeros(fmt.Sprint(s.Data["amount"]))), map[bool]string{true: "收款信息", false: "地址"}[withdrawKind(s) == wallet.KindFiat], htmlEscape(addr))
			_ = r.client.SendMessage(ctx, m.Chat.ID, text, ptrKB(ConfirmWithdrawKeyboard(fmt.Sprint(s.Data["request_id"]))))
		}
	case "support":
		if s.Step == "input_content" {
			content := strings.TrimSpace(m.Text)
			if len(content) < 2 {
				_ = r.client.SendMessage(ctx, m.Chat.ID, "请补充问题描述。", nil)
				return
			}
			ticket, err := r.support.Create(u.ID, u.TelegramUserID, content, r.botID)
			if err != nil {
				return
			}
			_ = r.sessions.Clear(ctx, m.From.ID)
			_ = r.client.SendMessage(ctx, m.Chat.ID, fmt.Sprintf("客服工单已创建：<code>#%d</code>\n请等待客服处理。", ticket.ID), r.homeKeyboard(r.isAdmin(m.From.ID)))
		}
	}
}

var sharePayload = regexp.MustCompile(`^([pg])(\d{1,18})_([A-Za-z0-9]{1,40})$`)

var shopStatusNames = map[string]string{"grouping": "拼团中", "paid": "待处理", "shipped": "已发货", "ready": "待自提", "completed": "已完成", "canceled": "已取消"}

// sendShopOrders lists the user's latest shop orders, with buttons that open each in the shop.
func (r *Router) sendShopOrders(ctx context.Context, chatID int64, userID uint64) {
	var orders []model.ShopOrder
	if err := r.store.DB.Where("user_id = ?", userID).Order("id desc").Limit(5).Find(&orders).Error; err != nil {
		_ = r.client.SendMessage(ctx, chatID, "订单暂时无法查询，请稍后重试。", nil)
		return
	}
	if len(orders) == 0 {
		var kb *InlineKeyboardMarkup
		if r.menuButton.URL != "" {
			kb = &InlineKeyboardMarkup{InlineKeyboard: [][]InlineKeyboardButton{{ShopButton("🛍 去逛逛", r.menuButton.URL, "/")}}}
		}
		_ = r.client.SendMessage(ctx, chatID, "你还没有商城订单。", kb)
		return
	}
	var b strings.Builder
	b.WriteString("<b>📦 最近的商城订单</b>\n")
	rows := [][]InlineKeyboardButton{}
	for _, o := range orders {
		pay, _ := decimal.NewFromString(o.PayAmount)
		b.WriteString(fmt.Sprintf("\n<code>%s</code>  %s\n%s · %s USDT · %s\n", o.OrderNo, shopStatusNames[o.Status], htmlEscape(o.MerchantName), pay.String(), o.CreatedAt.Format("01-02 15:04")))
		if r.menuButton.URL != "" {
			rows = append(rows, []InlineKeyboardButton{ShopButton("查看 "+o.OrderNo, r.menuButton.URL, fmt.Sprintf("/order/%d", o.ID))})
		}
	}
	if r.menuButton.URL != "" {
		rows = append(rows, []InlineKeyboardButton{ShopButton("全部订单", r.menuButton.URL, "/orders")})
	}
	var kb *InlineKeyboardMarkup
	if len(rows) > 0 {
		kb = &InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	_ = r.client.SendMessage(ctx, chatID, b.String(), kb)
}

// sendWelcome greets a user on first activation; an empty welcome_text disables it.
func (r *Router) sendWelcome(ctx context.Context, chatID int64) {
	text := strings.TrimSpace(r.menus.Config("welcome_text", model.DefaultWelcomeText))
	if text == "" {
		return
	}
	_ = r.client.SendMessage(ctx, chatID, htmlEscape(text), nil)
}
func (r *Router) sendHome(ctx context.Context, chatID, userID int64) {
	name := r.menus.Config("site_name", "Telegram Business Bot")
	items, err := r.menus.ReplyItems(r.isAdmin(userID), r.adminOnly)
	if err != nil {
		_ = r.client.SendMessage(ctx, chatID, "菜单加载失败，请稍后发送 /menu 重试。", nil)
		return
	}
	title := "<b>" + htmlEscape(name) + "</b>"
	kb := r.homeKeyboard(r.isAdmin(userID))
	hasInline := kb != nil && len(kb.InlineKeyboard) > 0
	switch {
	case len(items) == 0 && hasInline:
		// Inline buttons alone fit in one message.
		_ = r.client.SendMessage(ctx, chatID, title+"\n\n请选择功能：", kb)
	case !hasInline:
		_ = r.client.SendReplyKeyboard(ctx, chatID, title+"\n\n请选择功能：", items)
	default:
		// A message carries one reply_markup, so the reply keyboard and inline buttons need separate messages.
		if r.client.SendReplyKeyboard(ctx, chatID, title, items) == nil {
			_ = r.client.SendMessage(ctx, chatID, "请选择功能：", kb)
		}
	}
}

func (r *Router) editHome(ctx context.Context, chatID int64, msgID int, userID int64) {
	name := r.menus.Config("site_name", "Telegram Business Bot")
	_ = r.client.EditMessage(ctx, chatID, msgID, "<b>"+htmlEscape(name)+"</b>\n\n请选择功能：", r.homeKeyboard(r.isAdmin(userID)))
}

func (r *Router) WithBotID(id uint64) *Router { r.botID = id; return r }

// WithMenuButton sets the input field's menu button registered by SyncCommandMenu.
func (r *Router) WithMenuButton(b MenuButton) *Router { r.menuButton = b; return r }

// Withdrawal sessions started before currencies existed carry only network TRC20 (USDT).
func withdrawCurrency(s *session.Session) string {
	if c, ok := s.Data["currency"].(string); ok && c != "" {
		return c
	}
	return "USDT"
}

func withdrawName(s *session.Session) string {
	if n, ok := s.Data["name"].(string); ok && n != "" {
		return n
	}
	return "USDT-TRC20"
}

func withdrawKind(s *session.Session) string {
	if k, ok := s.Data["kind"].(string); ok && k != "" {
		return k
	}
	return wallet.KindTRC20
}

func isZero(v string) bool {
	d, err := decimal.NewFromString(v)
	return err == nil && d.IsZero()
}

// trimZeros shows a decimal without trailing zeros ("10.500000" → "10.5").
func trimZeros(v string) string {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return v
	}
	return d.String()
}
