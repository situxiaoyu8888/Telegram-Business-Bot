package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
		if private && !r.adminOnly {
			if err := r.store.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.BotSubscription{BotID: r.botID, TelegramUserID: userID}).Error; err != nil {
				return err
			}
		}
		r.handle(ctx, u)
		return ctx.Err()
	})
}
func (r *Router) handle(ctx context.Context, u Update) {
	if u.Message != nil {
		r.handleMessage(ctx, u.Message)
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

func (r *Router) handleMessage(ctx context.Context, m *Message) {
	u := r.saveUser(m.From)
	if u == nil || u.Status != 1 || m.Chat.ID != m.From.ID {
		return
	}
	if r.adminOnly && !r.isAdmin(m.From.ID) {
		_ = r.client.SendMessage(ctx, m.Chat.ID, "无管理员权限。", nil)
		return
	}
	text := strings.TrimSpace(m.Text)
	command := commandName(text)
	if command == "/start" {
		_ = r.sessions.Clear(ctx, m.From.ID)
		parts := strings.Fields(text)
		if len(parts) > 1 {
			_ = r.referral.BindByCode(u.ID, parts[1])
		}
		r.sendHome(ctx, m.Chat.ID, m.From.ID)
		return
	}
	switch command {
	case "/menu", ReplyHomeText:
		_ = r.sessions.Clear(ctx, m.From.ID)
		r.sendHome(ctx, m.Chat.ID, m.From.ID)
		return
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
	case strings.HasPrefix(cb.Data, "withdraw:network:"):
		network := strings.TrimPrefix(cb.Data, "withdraw:network:")
		if network != "TRC20" {
			return
		}
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return
		}
		requestID := hex.EncodeToString(nonce)
		_ = r.sessions.Set(ctx, userID, session.Session{Scene: "withdraw", Step: "input_amount", Data: map[string]any{"network": network, "request_id": requestID}})
		_ = r.client.EditMessage(ctx, chatID, msgID, "<b>💸 提现 - "+htmlEscape(network)+"</b>\n\n请输入提现金额，例如 <code>100.5</code>。\n发送 /cancel 可取消。", ptrKB(BackHomeKeyboard()))
	case strings.HasPrefix(cb.Data, "withdraw:confirm:"):
		s, _ := r.sessions.Get(ctx, userID)
		if s == nil || s.Scene != "withdraw" || s.Step != "confirm" || fmt.Sprint(s.Data["request_id"]) != strings.TrimPrefix(cb.Data, "withdraw:confirm:") {
			return
		}
		amount := fmt.Sprint(s.Data["amount"])
		network := fmt.Sprint(s.Data["network"])
		address := fmt.Sprint(s.Data["address"])
		order, err := r.wallet.CreateWithdrawal(u.ID, network, address, amount, fmt.Sprint(s.Data["request_id"]))
		if err != nil {
			_ = r.client.EditMessage(ctx, chatID, msgID, "提现提交失败："+htmlEscape(err.Error()), ptrKB(BackHomeKeyboard()))
			return
		}
		_ = r.sessions.Clear(ctx, userID)
		_ = r.client.EditMessage(ctx, chatID, msgID, fmt.Sprintf("<b>✅ 提现申请已提交</b>\n\n订单：<code>#%d</code>\n状态：待审核\n金额：<code>%s USDT</code>\n\n审核通过后由独立钱包/签名服务执行链上转账。", order.ID, order.Amount), ptrKB(BackHomeKeyboard()))
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
		av, fr, err := r.wallet.Balance(u.ID)
		if err != nil {
			return
		}
		_ = r.menuResponse(ctx, chatID, msgID, fmt.Sprintf("<b>💰 我的钱包</b>\n\n可用：<code>%s USDT</code>\n冻结：<code>%s USDT</code>", av.StringFixed(6), fr.StringFixed(6)), ptrKB(BackHomeKeyboard()))
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
		_ = r.menuResponse(ctx, chatID, msgID, "<b>💸 USDT 提现</b>\n\n请选择网络：", ptrKB(WithdrawKeyboard()))
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
		_ = r.menuResponse(ctx, chatID, msgID, fmt.Sprintf("<b>🎁 推广代理</b>\n\n推广码：<code>%s</code>\n下级人数：<code>%d</code>\n累计佣金：<code>%s USDT</code>\n\n邀请链接格式：<code>https://t.me/&lt;bot_username&gt;?start=%s</code>", u.ReferralCode, count, total.StringFixed(6), u.ReferralCode), ptrKB(BackHomeKeyboard()))
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
				b.WriteString(fmt.Sprintf("#%d 用户%d <code>%s USDT</code>\n", x.ID, x.UserID, x.Amount))
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
			_ = r.client.SendMessage(ctx, m.Chat.ID, "请输入 TRON/TRC20 提现地址：", nil)
		case "input_address":
			addr := strings.TrimSpace(m.Text)
			if !wallet.ValidTRONAddress(addr) {
				_ = r.client.SendMessage(ctx, m.Chat.ID, "地址格式看起来不正确，请输入以 T 开头的 TRON 地址。", nil)
				return
			}
			s.Data["address"] = addr
			s.Step = "confirm"
			_ = r.sessions.Set(ctx, m.From.ID, *s)
			text := fmt.Sprintf("<b>确认提现</b>\n\n网络：<code>%s</code>\n金额：<code>%s USDT</code>\n地址：<code>%s</code>", htmlEscape(fmt.Sprint(s.Data["network"])), htmlEscape(fmt.Sprint(s.Data["amount"])), htmlEscape(addr))
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
func (r *Router) sendHome(ctx context.Context, chatID, userID int64) {
	name := r.menus.Config("site_name", "Telegram Business Bot")
	items, err := r.menus.ReplyItems(r.isAdmin(userID), r.adminOnly)
	if err != nil {
		_ = r.client.SendMessage(ctx, chatID, "菜单加载失败，请稍后发送 /menu 重试。", nil)
		return
	}
	if err = r.client.SendReplyKeyboard(ctx, chatID, "<b>"+htmlEscape(name)+"</b>\n\n请选择功能：", items); err != nil {
		return
	}
	kb := r.homeKeyboard(r.isAdmin(userID))
	if kb != nil && len(kb.InlineKeyboard) > 0 {
		_ = r.client.SendMessage(ctx, chatID, "更多功能：", kb)
	}
}

func (r *Router) editHome(ctx context.Context, chatID int64, msgID int, userID int64) {
	name := r.menus.Config("site_name", "Telegram Business Bot")
	_ = r.client.EditMessage(ctx, chatID, msgID, "<b>"+htmlEscape(name)+"</b>\n\n请选择功能：", r.homeKeyboard(r.isAdmin(userID)))
}

func (r *Router) WithBotID(id uint64) *Router { r.botID = id; return r }
