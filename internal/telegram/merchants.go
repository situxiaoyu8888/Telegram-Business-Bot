package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"telegram-business-bot/internal/model"
)

const merchantPageSize = 10

func (r *Router) merchantUnavailable(ctx context.Context, cb *CallbackQuery) {
	_ = r.menuResponse(ctx, cb.Message.Chat.ID, cb.Message.MessageID, "商家或分类暂不可用，请返回菜单重新选择。", ptrKB(BackHomeKeyboard()))
}
func (r *Router) handleMerchantCallback(ctx context.Context, cb *CallbackQuery) {
	parts := strings.Split(cb.Data, ":")
	if len(parts) != 3 {
		r.merchantUnavailable(ctx, cb)
		return
	}
	id, err := strconv.ParseUint(parts[1], 10, 64)
	page, pageErr := strconv.Atoi(parts[2])
	if err != nil || id == 0 || pageErr != nil || page < 0 || page > 100000 {
		r.merchantUnavailable(ctx, cb)
		return
	}
	if parts[0] == "shops" {
		r.showMerchants(ctx, cb, id, page)
		return
	}
	if parts[0] != "shop" {
		r.merchantUnavailable(ctx, cb)
		return
	}
	var merchant model.Merchant
	if err = r.store.DB.Where("id=? AND enabled=1", id).First(&merchant).Error; err != nil {
		r.merchantUnavailable(ctx, cb)
		return
	}
	if _, err = r.menus.MerchantMenu(merchant.MenuID, r.isAdmin(cb.From.ID)); err != nil {
		r.merchantUnavailable(ctx, cb)
		return
	}
	kb := [][]InlineKeyboardButton{}
	if merchant.ContactURL != "" && IsTelegramURL(merchant.ContactURL) {
		kb = append(kb, []InlineKeyboardButton{{Text: "联系商家", URL: merchant.ContactURL}})
	}
	kb = append(kb, []InlineKeyboardButton{{Text: "⬅ 返回商家列表", CallbackData: fmt.Sprintf("shops:%d:%d", merchant.MenuID, page)}})
	_ = r.menuResponse(ctx, cb.Message.Chat.ID, cb.Message.MessageID, merchantText(merchant), &InlineKeyboardMarkup{InlineKeyboard: kb})
}
func merchantText(m model.Merchant) string {
	text := "<b>" + htmlEscape(m.Name) + "</b>"
	if m.Description != "" {
		text += "\n\n" + htmlEscape(m.Description)
	}
	for _, field := range []struct{ label, value string }{{"📍 地址", m.Address}, {"🕒 营业时间", m.Hours}, {"☎️ 电话", m.Phone}} {
		if field.value != "" {
			text += "\n\n" + field.label + "：" + htmlEscape(field.value)
		}
	}
	return text
}
func (r *Router) showMerchants(ctx context.Context, cb *CallbackQuery, menuID uint64, page int) {
	if page < 0 || page > 100000 {
		r.merchantUnavailable(ctx, cb)
		return
	}
	category, err := r.menus.MerchantMenu(menuID, r.isAdmin(cb.From.ID))
	if err != nil {
		r.merchantUnavailable(ctx, cb)
		return
	}
	var merchants []model.Merchant
	if err = r.store.DB.Where("menu_id=? AND enabled=1", menuID).Order("sort asc,id asc").Offset(page * merchantPageSize).Limit(merchantPageSize + 1).Find(&merchants).Error; err != nil {
		r.merchantUnavailable(ctx, cb)
		return
	}
	hasNext := len(merchants) > merchantPageSize
	if hasNext {
		merchants = merchants[:merchantPageSize]
	}
	kb := [][]InlineKeyboardButton{}
	for _, m := range merchants {
		kb = append(kb, []InlineKeyboardButton{{Text: m.Name, CallbackData: fmt.Sprintf("shop:%d:%d", m.ID, page)}})
	}
	nav := []InlineKeyboardButton{}
	if page > 0 {
		nav = append(nav, InlineKeyboardButton{Text: "上一页", CallbackData: fmt.Sprintf("shops:%d:%d", menuID, page-1)})
	}
	if hasNext {
		nav = append(nav, InlineKeyboardButton{Text: "下一页", CallbackData: fmt.Sprintf("shops:%d:%d", menuID, page+1)})
	}
	if len(nav) > 0 {
		kb = append(kb, nav)
	}
	// The menu was checked above, including its ancestors. Resolve its parent's
	// code for navigation; callback authorization is checked again when clicked.
	var parent model.MenuItem
	if r.store.DB.First(&parent, category.ParentID).Error == nil {
		kb = append(kb, []InlineKeyboardButton{{Text: "⬅ 返回分类", CallbackData: "menu:" + parent.Code}})
	}
	kb = append(kb, []InlineKeyboardButton{{Text: "🏠 返回首页", CallbackData: "home"}})
	text := "<b>" + htmlEscape(category.Title) + "</b>\n\n请选择商家："
	if len(merchants) == 0 {
		text = "<b>" + htmlEscape(category.Title) + "</b>\n\n当前页暂无可展示的商家。"
	} else {
		text += fmt.Sprintf("\n第 %d 页", page+1)
	}
	_ = r.menuResponse(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, &InlineKeyboardMarkup{InlineKeyboard: kb})
}

// The first merchant occupies its own row; the remaining twelve use two columns.
func recommendationKeyboard(merchants []model.Merchant, root uint64) *InlineKeyboardMarkup {
	rows := [][]InlineKeyboardButton{}
	for i, m := range merchants {
		button := InlineKeyboardButton{Text: "🟢 " + m.Name, CallbackData: fmt.Sprintf("shop:%d:0", m.ID)}
		if i == 0 || i%2 == 1 {
			rows = append(rows, []InlineKeyboardButton{button})
		} else {
			rows[len(rows)-1] = append(rows[len(rows)-1], button)
		}
	}
	rows = append(rows, []InlineKeyboardButton{{Text: "换一批", CallbackData: fmt.Sprintf("recommend:%d", root)}})
	return &InlineKeyboardMarkup{InlineKeyboard: rows}
}
func (r *Router) recommendedMerchants(ids []uint64) ([]model.Merchant, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// Randomize within the highest-priority group: pinned first, otherwise all.
	var first []model.Merchant
	if err := r.store.DB.Where("menu_id IN ? AND enabled=1", ids).Order("pinned DESC,RAND()").Limit(1).Find(&first).Error; err != nil {
		return nil, err
	}
	if len(first) == 0 {
		return nil, nil
	}
	var rest []model.Merchant
	if err := r.store.DB.Where("menu_id IN ? AND enabled=1 AND id<>?", ids, first[0].ID).Order("RAND()").Limit(12).Find(&rest).Error; err != nil {
		return nil, err
	}
	return append(first, rest...), nil
}
func (r *Router) handleRecommendationCallback(ctx context.Context, cb *CallbackQuery) {
	parts := strings.Split(cb.Data, ":")
	if len(parts) != 2 {
		r.merchantUnavailable(ctx, cb)
		return
	}
	root, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil || root == 0 {
		r.merchantUnavailable(ctx, cb)
		return
	}
	r.showMerchantRecommendations(ctx, cb, root, true)
}
func (r *Router) sendMerchantRecommendations(ctx context.Context, cb *CallbackQuery, root uint64) {
	r.showMerchantRecommendations(ctx, cb, root, false)
}
func (r *Router) showMerchantRecommendations(ctx context.Context, cb *CallbackQuery, root uint64, refresh bool) {
	ids, err := r.menus.MerchantDescendants(root, r.isAdmin(cb.From.ID), r.adminOnly)
	if err != nil {
		if refresh {
			r.merchantUnavailable(ctx, cb)
		}
		if r.logger != nil {
			r.logger.Warn("merchant recommendation categories unavailable")
		}
		return
	}
	if len(ids) == 0 {
		if refresh {
			r.merchantUnavailable(ctx, cb)
		}
		return
	}
	merchants, err := r.recommendedMerchants(ids)
	if err != nil {
		if refresh {
			r.merchantUnavailable(ctx, cb)
		}
		if r.logger != nil {
			r.logger.Warn("merchant recommendations unavailable")
		}
		return
	}
	text := "随机给您推荐的商铺"
	if len(merchants) == 0 {
		if !refresh {
			return
		}
		text = "当前分类暂无可推荐的商家。"
	}
	kb := recommendationKeyboard(merchants, root)
	if refresh {
		_ = r.menuResponse(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, kb)
	} else {
		_ = r.client.SendMessage(ctx, cb.Message.Chat.ID, text, kb)
	}
}

// Keep callback_data within Telegram's 64-byte limit, including the prefix.
func (r *Router) searchMerchants(ctx context.Context, cb *CallbackQuery, keyword string, refresh bool) bool {
	keyword = strings.TrimSpace(keyword)
	if keyword == "" || len(keyword) > 56 || strings.HasPrefix(keyword, "/") {
		if refresh {
			r.merchantUnavailable(ctx, cb)
		}
		return false
	}
	ids, err := r.menus.SearchMerchantMenus(keyword, r.isAdmin(cb.From.ID), r.adminOnly)
	if err != nil {
		_ = r.client.SendMessage(ctx, cb.Message.Chat.ID, "商家搜索暂不可用，请稍后重试。", nil)
		return true
	}
	if len(ids) == 0 {
		if refresh {
			r.merchantUnavailable(ctx, cb)
			return true
		}
		_ = r.client.SendMessage(ctx, cb.Message.Chat.ID, "未找到相关商家分类，请换个关键词，例如：烧烤。", nil)
		return true
	}
	merchants, err := r.recommendedMerchants(ids)
	if err != nil {
		_ = r.client.SendMessage(ctx, cb.Message.Chat.ID, "商家搜索暂不可用，请稍后重试。", nil)
		return true
	}
	text := "随机给您推荐的商铺"
	if len(merchants) == 0 {
		text = "该分类暂无可推荐的商家。"
	}
	kb := recommendationKeyboard(merchants, 0)
	kb.InlineKeyboard[len(kb.InlineKeyboard)-1][0].CallbackData = "search:" + keyword
	if refresh {
		_ = r.menuResponse(ctx, cb.Message.Chat.ID, cb.Message.MessageID, text, kb)
	} else {
		_ = r.client.SendMessage(ctx, cb.Message.Chat.ID, text, kb)
	}
	return true
}
