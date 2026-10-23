package shop

import (
	"context"
	"encoding/base64"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/qrcode"
	"telegram-business-bot/internal/redpacket"
	"telegram-business-bot/internal/telegram"
	"telegram-business-bot/internal/wallet"
)

// The 我的 page's wallet functions: deposit address, withdrawals, transfers between users and
// sending red packets. Every spend needs the payment password (X-Pay-Password).

// SenderSource returns the bot that talks to users for a bot ID (0 = the primary bot).
type SenderSource func(ctx context.Context, botID uint64) (telegram.Sender, error)

// WithWallet enables the wallet functions; packets may be nil (no red packets).
func (h *Handler) WithWallet(packets *redpacket.Service, senders SenderSource) *Handler {
	h.packets, h.senders = packets, senders
	return h
}

func (h *Handler) registerWallet(api *gin.RouterGroup) {
	api.GET("/wallet/deposit", h.depositInfo)
	api.POST("/wallet/deposit/claim", h.throttle("rl:shop-order:", maxOrdersPerMinute), h.claimDeposit)
	api.GET("/wallet/withdraw", h.withdrawInfo)
	api.POST("/wallet/withdraw", h.throttle("rl:shop-order:", maxOrdersPerMinute), h.withdraw)
	api.GET("/wallet/transfer", h.transferInfo)
	api.GET("/wallet/recipient", h.throttle("rl:shop-lookup:", maxOrdersPerMinute), h.recipient)
	api.POST("/wallet/transfer", h.throttle("rl:shop-order:", maxOrdersPerMinute), h.transfer)
	api.GET("/redpackets/options", h.packetOptions)
	api.POST("/redpackets", h.throttle("rl:shop-order:", maxOrdersPerMinute), h.sendPacket)
}

// notify sends a message through the buyer's bot, or the primary bot when that one fails (the
// recipient of a transfer may never have opened it).
func (h *Handler) notify(ctx context.Context, botID uint64, telegramUserID int64, html string) {
	if h.senders == nil {
		return
	}
	for _, id := range []uint64{botID, 0} {
		s, err := h.senders(ctx, id)
		if err == nil {
			if err = s.SendMessageWithParseMode(ctx, telegramUserID, html, "HTML", nil); err == nil {
				return
			}
		}
		if h.logger != nil {
			h.logger.Debug("wallet notice failed", zap.Uint64("bot_id", id), zap.Error(err))
		}
		if id == 0 {
			return
		}
	}
}

// depositInfo returns the buyer's deposit addresses: TRC20 (credited automatically) first, then
// ERC20 (credited from the console once the deposit is confirmed).
func (h *Handler) depositInfo(c *gin.Context) {
	list := []model.DepositAddress{}
	err := h.st.DB.Where("user_id = ? AND status = 1 AND network IN ?", buyer(c).UserID, []string{"TRC20", "ERC20"}).Order("network desc").Find(&list).Error
	if err != nil {
		h.fail(c, err)
		return
	}
	notice := h.svc.configString("deposit_notice")
	if notice == "" {
		notice = "仅支持 USDT TRC20"
	}
	type address struct {
		Network string `json:"network"`
		Address string `json:"address"`
		Auto    bool   `json:"auto"`
		Notice  string `json:"notice"`
		QR      string `json:"qr"`    // PNG data URI
		Claim   bool   `json:"claim"` // deposits are claimed by transaction hash (shared or ERC20)
	}
	out := []address{}
	for _, a := range list {
		row := address{a.Network, a.Address, true, notice, "", false}
		if a.Network != "TRC20" {
			row.Auto, row.Claim, row.Notice = false, true, "仅支持以太坊网络的 USDT（ERC20）"
		} else {
			var n int64
			h.st.DB.Model(&model.DepositAddress{}).Where("network = 'TRC20' AND address = ?", a.Address).Count(&n)
			row.Auto, row.Claim = n <= 1, n > 1
		}
		if png, err := qrcode.PNG(a.Address); err == nil {
			row.QR = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
		}
		out = append(out, row)
	}
	type record struct {
		OrderNo  string `json:"order_no"`
		Network  string `json:"network"`
		Currency string `json:"currency"`
		Amount   string `json:"amount"`
		TxID     string `json:"txid"`
		Status   string `json:"status"`
		Created  string `json:"created_at"`
	}
	var orders []model.DepositOrder
	h.st.DB.Where("user_id = ?", buyer(c).UserID).Order("id desc").Limit(10).Find(&orders)
	records := []record{}
	for _, o := range orders {
		records = append(records, record{wallet.OrderNo("D", o.ID, o.CreatedAt), o.Network, o.Currency, o.Amount, o.TxID, o.Status, o.CreatedAt.Format(time.RFC3339)})
	}
	h.reply(c, gin.H{"addresses": out, "records": records}, nil)
}

// claimDeposit records the buyer's claim of an on-chain deposit by its transaction hash (and, for
// ERC20, the amount) for the console to confirm.
func (h *Handler) claimDeposit(c *gin.Context) {
	var in struct {
		TxID   string `json:"txid"`
		Amount string `json:"amount"`
	}
	if c.ShouldBindJSON(&in) != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	o, err := h.svc.wallet.SubmitClaim(buyer(c).UserID, in.TxID, in.Amount)
	if err != nil {
		h.fail(c, err)
		return
	}
	h.reply(c, gin.H{"pending": true, "order_no": wallet.OrderNo("D", o.ID, o.CreatedAt)}, nil)
}

func (h *Handler) withdrawInfo(c *gin.Context) {
	b := buyer(c)
	rules, err := h.svc.wallet.WithdrawRules()
	if err != nil {
		h.fail(c, err)
		return
	}
	balances, err := h.svc.wallet.Balances(b.UserID)
	if err != nil {
		h.fail(c, err)
		return
	}
	records := []model.WithdrawalOrder{}
	err = h.st.DB.Where("user_id = ?", b.UserID).Order("id desc").Limit(10).Find(&records).Error
	h.reply(c, gin.H{"rules": rules, "balances": balances, "records": records}, err)
}

func (h *Handler) withdraw(c *gin.Context) {
	var in struct {
		Currency  string `json:"currency"`
		Address   string `json:"address"`
		Amount    string `json:"amount"`
		RequestID string `json:"request_id"`
	}
	if c.ShouldBindJSON(&in) != nil || in.RequestID == "" || len(in.RequestID) > 60 {
		h.fail(c, userErr("参数错误"))
		return
	}
	if err := h.checkPayPassword(c); err != nil {
		h.fail(c, err)
		return
	}
	amount, err := decimal.NewFromString(strings.TrimSpace(in.Amount))
	if err != nil || !amount.IsPositive() {
		h.fail(c, userErr("请输入正确的提现金额"))
		return
	}
	order, err := h.svc.wallet.CreateCurrencyWithdrawal(buyer(c).UserID, in.Currency, strings.TrimSpace(in.Address), amount.StringFixed(6), "h5:"+in.RequestID)
	if err == nil {
		_ = h.svc.wallet.SetWithdrawalBot(order.ID, buyer(c).BotID)
	}
	h.reply(c, order, err)
}

func (h *Handler) transferInfo(c *gin.Context) {
	b := buyer(c)
	balances, err := h.svc.wallet.Balances(b.UserID)
	if err != nil {
		h.fail(c, err)
		return
	}
	list, err := h.svc.wallet.Transfers(b.UserID, 20)
	if err != nil {
		h.fail(c, err)
		return
	}
	ids := []uint64{}
	for _, t := range list {
		ids = append(ids, t.FromUserID, t.ToUserID)
	}
	var users []model.TelegramUser
	if len(ids) > 0 {
		if err := h.st.DB.Where("id IN ?", ids).Find(&users).Error; err != nil {
			h.fail(c, err)
			return
		}
	}
	names := map[uint64]string{}
	for i := range users {
		names[users[i].ID] = wallet.UserName(&users[i])
	}
	type record struct {
		model.WalletTransfer
		Out  bool   `json:"out"`
		Peer string `json:"peer"`
	}
	records := make([]record, 0, len(list))
	for _, t := range list {
		r := record{WalletTransfer: t, Out: t.FromUserID == b.UserID, Peer: names[t.FromUserID]}
		if r.Out {
			r.Peer = names[t.ToUserID]
		}
		records = append(records, r)
	}
	h.reply(c, gin.H{"balances": balances, "records": records}, nil)
}

// recipient shows who a transfer would go to before the buyer confirms it.
func (h *Handler) recipient(c *gin.Context) {
	u, err := h.svc.wallet.FindRecipient(c.Query("to"))
	if err == nil && u.ID == buyer(c).UserID {
		err = userErr("不能转账给自己")
	}
	if err != nil {
		if IsUserError(err) {
			// Typing a recipient is not a failed request; the page shows the reason.
			h.reply(c, gin.H{"found": false, "error": err.Error()}, nil)
			return
		}
		h.fail(c, err)
		return
	}
	h.reply(c, gin.H{"found": true, "name": wallet.UserName(u), "username": u.Username, "telegram_user_id": u.TelegramUserID}, nil)
}

func (h *Handler) transfer(c *gin.Context) {
	var in wallet.TransferRequest
	if c.ShouldBindJSON(&in) != nil {
		h.fail(c, userErr("参数错误"))
		return
	}
	if err := h.checkPayPassword(c); err != nil {
		h.fail(c, err)
		return
	}
	b := buyer(c)
	t, to, err := h.svc.wallet.Transfer(b.UserID, in)
	if err != nil {
		h.fail(c, err)
		return
	}
	if cur, err := h.svc.wallet.Currency(t.Currency); err == nil {
		var from model.TelegramUser
		if h.st.DB.First(&from, b.UserID).Error == nil {
			text := fmt.Sprintf("💸 <b>收到转账</b>\n\n%s 向你转账 <code>%s</code> %s，已存入钱包。", html.EscapeString(wallet.UserName(&from)), decimalText(t.Amount), html.EscapeString(cur.Name))
			if t.Remark != "" {
				text += "\n备注：" + html.EscapeString(t.Remark)
			}
			h.notify(c.Request.Context(), b.BotID, to.TelegramUserID, text)
		}
	}
	h.reply(c, gin.H{"transfer": t, "to": wallet.UserName(to)}, nil)
}

func decimalText(v string) string {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return v
	}
	return d.String()
}

func (h *Handler) packetOptions(c *gin.Context) {
	if h.packets == nil {
		h.fail(c, userErr("暂未开放发红包"))
		return
	}
	balances, err := h.svc.wallet.Balances(buyer(c).UserID)
	if err != nil {
		h.fail(c, err)
		return
	}
	covers, err := h.packets.Covers()
	if err != nil {
		h.fail(c, err)
		return
	}
	h.reply(c, gin.H{"balances": balances, "covers": covers, "max_count": h.packets.MaxCount(), "expire_hours": h.packets.ExpireHours(),
		"default_greeting": redpacket.DefaultGreeting}, nil)
}

// sendPacket creates a red packet for the bot the buyer opened the shop from; its card is sent
// to the buyer's private chat with that bot, from where it is shared into any chat.
func (h *Handler) sendPacket(c *gin.Context) {
	if h.packets == nil {
		h.fail(c, userErr("暂未开放发红包"))
		return
	}
	var in struct {
		Type      string `json:"type"`
		Currency  string `json:"currency"`
		Amount    string `json:"amount"`
		Count     int    `json:"count"`
		Target    string `json:"target"`
		Password  string `json:"password"`
		Greeting  string `json:"greeting"`
		CoverID   uint64 `json:"cover_id"`
		RequestID string `json:"request_id"`
	}
	if c.ShouldBindJSON(&in) != nil || in.RequestID == "" || len(in.RequestID) > 60 {
		h.fail(c, userErr("参数错误"))
		return
	}
	if err := h.checkPayPassword(c); err != nil {
		h.fail(c, err)
		return
	}
	b := buyer(c)
	var u model.TelegramUser
	if err := h.st.DB.First(&u, b.UserID).Error; err != nil {
		h.fail(c, err)
		return
	}
	p, err := h.packets.Create(redpacket.CreateInput{BotID: b.BotID, SenderUserID: u.ID, SenderTelegramID: u.TelegramUserID, SenderName: strings.TrimPrefix(wallet.UserName(&u), "@"),
		Type: in.Type, Currency: in.Currency, Amount: in.Amount, Count: in.Count, Target: in.Target, Password: in.Password, Greeting: in.Greeting,
		CoverID: in.CoverID, RequestID: "h5:" + in.RequestID})
	if err != nil {
		h.fail(c, err)
		return
	}
	shared := false
	if h.senders != nil {
		if s, err := h.senders(c.Request.Context(), p.BotID); err == nil {
			if err := telegram.SharePacketToSender(c.Request.Context(), s, h.packets, p, h.logger); err == nil {
				shared = true
			} else if h.logger != nil {
				h.logger.Warn("send red packet card to sender failed", zap.String("code", p.Code), zap.Error(err))
			}
		}
	}
	bot := ""
	if h.botUsername != nil {
		bot = h.botUsername(c.Request.Context(), p.BotID)
	}
	h.reply(c, gin.H{"code": p.Code, "bot": bot, "sent_to_chat": shared, "total": p.Total, "currency": p.Currency, "count": p.Count}, nil)
}
