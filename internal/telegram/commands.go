package telegram

import (
	"context"
	"go.uber.org/zap"
	"strings"
	"time"
)

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

func defaultCommands(adminOnly, shop bool) []BotCommand {
	if adminOnly {
		return []BotCommand{{"start", "开始使用"}, {"admin", "管理入口"}, {"menu", "返回主菜单"}, {"cancel", "取消当前操作"}}
	}
	if shop {
		return []BotCommand{{"start", "开始使用"}, {"orders", "我的订单"}, {"hb", "发红包"}, {"contactus", "联系客服"}}
	}
	return []BotCommand{{"start", "开始使用"}, {"hb", "发红包"}, {"contactus", "联系客服"}}
}

// MenuButton is the button left of the input field. With a URL it opens that H5 page as a
// Telegram Web App; otherwise it shows the command list.
type MenuButton struct {
	Text string
	URL  string
}

func (b MenuButton) payload() any {
	if b.URL == "" {
		return map[string]string{"type": "commands"}
	}
	return map[string]any{"type": "web_app", "text": b.Text, "web_app": map[string]string{"url": b.URL}}
}

// Register the private-chat command list and the input field's menu button.
// Commands stay available by typing "/" when the button opens the H5 page.
func (c *Client) RegisterCommandMenu(ctx context.Context, adminOnly bool, button MenuButton) error {
	if err := c.call(ctx, "setMyCommands", map[string]any{"commands": defaultCommands(adminOnly, button.URL != ""), "scope": map[string]string{"type": "all_private_chats"}, "language_code": ""}, nil); err != nil {
		return err
	}
	return c.call(ctx, "setChatMenuButton", map[string]any{"menu_button": button.payload()}, nil)
}

// Registration failure must not stop message delivery. Retry transient failures
// within the bot worker's lifetime, with a bounded request timeout.
func (r *Router) SyncCommandMenu(ctx context.Context) {
	for ctx.Err() == nil {
		attempt, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := r.client.RegisterCommandMenu(attempt, r.adminOnly, r.menuButton)
		cancel()
		if err == nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
		if r.logger != nil {
			r.logger.Warn("command menu registration failed; retrying", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}
func commandName(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return text
	}
	first := strings.Fields(text)[0]
	return strings.SplitN(first, "@", 2)[0]
}
