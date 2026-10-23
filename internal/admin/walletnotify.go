package admin

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/wallet"
)

// ---- Notices to users about their money ----

// notifyUser messages a user in the background through the bot they used (0 = primary) or any
// other bot that reaches them; a failure is only logged.
func (h *Handler) notifyUser(userID, botID uint64, html string) {
	if h.bots == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var u model.TelegramUser
		if err := h.st.DB.WithContext(ctx).Select("telegram_user_id").First(&u, userID).Error; err != nil {
			return
		}
		if err := h.bots.NotifyUser(ctx, botID, u.TelegramUserID, html); err != nil && h.logger != nil {
			h.logger.Warn("wallet notice not delivered", zap.Uint64("user_id", userID), zap.Error(err))
		}
	}()
}

func plainAmount(v string) string {
	d, err := decimal.NewFromString(v)
	if err != nil {
		return v
	}
	return d.String()
}

func (h *Handler) currencyName(code string) string {
	if c, err := h.wallet.Currency(code); err == nil {
		return c.Name
	}
	return code
}

// notifyWithdrawal tells the user their withdrawal was approved, paid out or rejected.
func (h *Handler) notifyWithdrawal(id uint64, kind, note string) {
	var o model.WithdrawalOrder
	if err := h.st.DB.First(&o, id).Error; err != nil {
		return
	}
	cur := htmlEscape(h.currencyName(o.Currency))
	var b strings.Builder
	switch kind {
	case "approved":
		b.WriteString("✅ <b>提现审核已通过</b>\n\n")
	case "completed":
		b.WriteString("💸 <b>提现已到账</b>\n\n")
	default:
		b.WriteString("❌ <b>提现未通过审核</b>\n\n")
	}
	fmt.Fprintf(&b, "订单编号：<code>%s</code>\n金额：<code>%s</code> %s\n", wallet.OrderNo("W", o.ID, o.CreatedAt), plainAmount(o.Amount), cur)
	if kind != "rejected" {
		fmt.Fprintf(&b, "实际到账：<code>%s</code> %s（手续费 %s）\n", plainAmount(o.Actual), cur, plainAmount(o.Fee))
	}
	fmt.Fprintf(&b, "%s：<code>%s</code>", map[bool]string{true: "收款账户", false: "收款地址"}[o.Network == "BANK"], htmlEscape(o.Address))
	switch kind {
	case "approved":
		b.WriteString("\n\n正在安排打款，到账后会再通知你。")
	case "completed":
		if note != "" {
			fmt.Fprintf(&b, "\n%s：<code>%s</code>", map[bool]string{true: "打款凭证", false: "交易哈希"}[o.Network == "BANK"], htmlEscape(note))
		}
		b.WriteString("\n\n请查收，如有疑问请联系客服。")
	default:
		if note != "" {
			b.WriteString("\n原因：" + htmlEscape(note))
		}
		b.WriteString("\n\n冻结的金额已退回你的钱包。")
	}
	h.notifyUser(o.UserID, o.BotID, b.String())
}

// NotifyDeposit tells the user a deposit has been credited (the wallet's deposit hook).
func (h *Handler) NotifyDeposit(o model.DepositOrder) {
	h.notifyUser(o.UserID, 0, fmt.Sprintf("💰 <b>充值到账</b>\n\n订单编号：<code>%s</code>\n金额：<code>%s</code> %s\n网络：%s\n交易哈希：<code>%s</code>\n\n已存入你的钱包。",
		wallet.OrderNo("D", o.ID, o.CreatedAt), plainAmount(o.Amount), htmlEscape(h.currencyName(o.Currency)), htmlEscape(o.Network), htmlEscape(o.TxID)))
}

// notifyCredit tells the user the console credited their wallet (e.g. a confirmed ERC20 or cash
// deposit).
func (h *Handler) notifyCredit(userID uint64, currency, amount, remark string) {
	text := fmt.Sprintf("💰 <b>充值到账</b>\n\n金额：<code>%s</code> %s\n", plainAmount(amount), htmlEscape(h.currencyName(currency)))
	if remark = strings.TrimSpace(remark); remark != "" {
		text += "备注：" + htmlEscape(remark) + "\n"
	}
	h.notifyUser(userID, 0, text+"\n已存入你的钱包。")
}

// ---- Operator address pool ----
//
// Deposit addresses come only from the operator's pool. One pool address may be given to several
// users; deposits to such a shared address are claimed by transaction hash (wallet.ClaimTRC20).

type poolRow struct {
	model.DepositPoolAddress
	Users int64 `json:"users"` // users it is assigned to
}

func (h *Handler) addressPool(c *gin.Context) {
	list := []model.DepositPoolAddress{}
	if err := h.st.DB.Order("status desc, id desc").Limit(2000).Find(&list).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "地址池加载失败"})
		return
	}
	var counts []struct {
		Address string
		N       int64
	}
	h.st.DB.Model(&model.DepositAddress{}).Select("address, COUNT(*) AS n").Group("address").Scan(&counts)
	used := map[string]int64{}
	for _, x := range counts {
		used[x.Address] = x.N
	}
	rows := make([]poolRow, 0, len(list))
	for _, a := range list {
		rows = append(rows, poolRow{DepositPoolAddress: a, Users: used[a.Address]})
	}
	c.JSON(http.StatusOK, rows)
}

// addPoolAddresses adds operator-owned addresses (one per line) to the pool.
func (h *Handler) addPoolAddresses(c *gin.Context) {
	var b struct {
		Network   string `json:"network"`
		Addresses string `json:"addresses"`
		Remark    string `json:"remark"`
	}
	if c.ShouldBindJSON(&b) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	check := map[string]func(string) bool{"TRC20": wallet.ValidTRONAddress, "ERC20": wallet.ValidERC20Address}[b.Network]
	if check == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择地址类型"})
		return
	}
	b.Remark = strings.TrimSpace(b.Remark)
	if len([]rune(b.Remark)) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "备注最多 64 字"})
		return
	}
	var rows []model.DepositPoolAddress
	seen := map[string]bool{}
	for i, line := range strings.Split(b.Addresses, "\n") {
		addr := strings.TrimSpace(line)
		if addr == "" || seen[addr] {
			continue
		}
		if !check(addr) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("第 %d 行不是有效的 %s 地址：%s", i+1, b.Network, addr)})
			return
		}
		seen[addr] = true
		rows = append(rows, model.DepositPoolAddress{Network: b.Network, Address: addr, Remark: b.Remark, Status: 1})
	}
	if len(rows) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请至少填写一个地址"})
		return
	}
	if len(rows) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "一次最多添加 1000 个地址"})
		return
	}
	addrs := make([]string, len(rows))
	for i, r := range rows {
		addrs[i] = r.Address
	}
	var existing []string
	h.st.DB.Model(&model.DepositPoolAddress{}).Where("address IN ?", addrs).Pluck("address", &existing)
	skip := map[string]bool{}
	for _, a := range existing {
		skip[a] = true
	}
	fresh := rows[:0]
	for _, r := range rows {
		if !skip[r.Address] {
			fresh = append(fresh, r)
		}
	}
	if len(fresh) > 0 {
		if err := h.st.DB.Create(&fresh).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "added": len(fresh), "skipped": len(skip)})
}

// deletePoolAddress removes a pool address no user has.
func (h *Handler) deletePoolAddress(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var a model.DepositPoolAddress
	if err := h.st.DB.First(&a, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "地址不存在"})
		return
	}
	var n int64
	h.st.DB.Model(&model.DepositAddress{}).Where("address = ?", a.Address).Count(&n)
	if n > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("该地址已分配给 %d 个用户，不能删除", n)})
		return
	}
	if err := h.st.DB.Delete(&a).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// poolAddresses lists the usable pool addresses of a network.
func (h *Handler) poolAddresses(db *gorm.DB, network string) ([]string, error) {
	var list []string
	err := db.Model(&model.DepositPoolAddress{}).Where("network = ? AND status = 1", network).Order("id asc").Pluck("address", &list).Error
	return list, err
}

// setUserAddress gives a user the address for a network, replacing the one they had (a new
// address is scanned from its start).
func setUserAddress(tx *gorm.DB, userID uint64, network, address string) error {
	var cur model.DepositAddress
	err := tx.Where("user_id = ? AND network = ?", userID, network).First(&cur).Error
	switch {
	case err == nil && cur.Address == address && cur.Status == 1:
		return nil
	case err == nil:
		return tx.Model(&cur).Updates(map[string]any{"address": address, "status": 1, "scan_timestamp": 0}).Error
	case errors.Is(err, gorm.ErrRecordNotFound):
		return tx.Create(&model.DepositAddress{UserID: userID, Network: network, Address: address, Status: 1}).Error
	}
	return err
}

// saveAddress gives one user a pool address of a network: the chosen one (pool_id) or a random
// one (pool_id 0). The address may be shared with other users.
func (h *Handler) saveAddress(c *gin.Context) {
	var b struct {
		UserID  uint64 `json:"user_id"`
		Network string `json:"network"`
		PoolID  uint64 `json:"pool_id"`
	}
	if c.ShouldBindJSON(&b) != nil || b.UserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择用户"})
		return
	}
	if b.Network != "TRC20" && b.Network != "ERC20" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择地址类型"})
		return
	}
	var user model.TelegramUser
	if err := h.st.DB.First(&user, b.UserID).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "用户不存在"})
		return
	}
	address := ""
	if b.PoolID != 0 {
		var a model.DepositPoolAddress
		if err := h.st.DB.Where("id = ? AND network = ? AND status = 1", b.PoolID, b.Network).First(&a).Error; err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "请选择地址池中的有效地址"})
			return
		}
		address = a.Address
	} else {
		list, err := h.poolAddresses(h.st.DB, b.Network)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "地址池加载失败"})
			return
		}
		if len(list) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "地址池中没有可用的 " + b.Network + " 地址，请先在「地址池」中添加"})
			return
		}
		address = list[rand.IntN(len(list))]
	}
	if err := setUserAddress(h.st.DB, b.UserID, b.Network, address); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "分配失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "address": address})
}

// bulkAddressPreview tells the console how many active users a bulk assignment covers and how
// many pool addresses it spreads them over.
func (h *Handler) bulkAddressPreview(c *gin.Context) {
	network := c.Query("network")
	if network != "TRC20" && network != "ERC20" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择地址类型"})
		return
	}
	var users, have int64
	h.st.DB.Model(&model.TelegramUser{}).Where("status = 1").Count(&users)
	h.st.DB.Model(&model.DepositAddress{}).Where("network = ?", network).Count(&have)
	list, err := h.poolAddresses(h.st.DB, network)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "统计失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users, "have": have, "free": len(list)})
}

// bulkAssignAddresses gives every active user a random pool address of the network, replacing
// the address they had; with fewer addresses than users, addresses are shared. Users are done in
// batches so locks stay short.
func (h *Handler) bulkAssignAddresses(c *gin.Context) {
	var b struct {
		Network string `json:"network"`
	}
	if c.ShouldBindJSON(&b) != nil || (b.Network != "TRC20" && b.Network != "ERC20") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择地址类型"})
		return
	}
	list, err := h.poolAddresses(h.st.DB, b.Network)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "地址池加载失败"})
		return
	}
	if len(list) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "地址池中没有可用的 " + b.Network + " 地址，请先在「地址池」中添加"})
		return
	}
	assigned := 0
	var last uint64
	for {
		var ids []uint64
		if err := h.st.DB.Model(&model.TelegramUser{}).Where("status = 1 AND id > ?", last).Order("id asc").Limit(500).Pluck("id", &ids).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("分配中断（已分配 %d 个用户）", assigned)})
			return
		}
		if len(ids) == 0 {
			break
		}
		err := h.st.DB.Transaction(func(tx *gorm.DB) error {
			for _, id := range ids {
				if err := setUserAddress(tx, id, b.Network, list[rand.IntN(len(list))]); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("分配中断（已分配 %d 个用户）", assigned)})
			return
		}
		assigned += len(ids)
		last = ids[len(ids)-1]
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "assigned": assigned, "addresses": len(list)})
}

// ---- Deposits waiting for a person ----

// assignDeposit credits an unclaimed deposit (to a shared address) to a chosen user.
func (h *Handler) assignDeposit(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		UserID uint64 `json:"user_id"`
	}
	if c.ShouldBindJSON(&b) != nil || b.UserID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择用户"})
		return
	}
	if err := h.st.DB.First(&model.TelegramUser{}, b.UserID).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "用户不存在"})
		return
	}
	_, err := h.wallet.AssignDeposit(id, b.UserID)
	walletReply(c, err)
}

// confirmDeposit credits a user's submitted ERC20 deposit with the verified amount.
func (h *Handler) confirmDeposit(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Amount string `json:"amount"`
	}
	_ = c.ShouldBindJSON(&b)
	_, err := h.wallet.ConfirmDeposit(id, b.Amount)
	walletReply(c, err)
}

// rejectDeposit turns down a submitted ERC20 deposit and tells the user.
func (h *Handler) rejectDeposit(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	o, err := h.wallet.RejectDeposit(id)
	if err == nil {
		text := fmt.Sprintf("❌ <b>充值未通过核对</b>\n\n订单编号：<code>%s</code>\n金额：<code>%s</code> %s\n交易哈希：<code>%s</code>",
			wallet.OrderNo("D", o.ID, o.CreatedAt), plainAmount(o.Amount), htmlEscape(h.currencyName(o.Currency)), htmlEscape(o.TxID))
		if note := strings.TrimSpace(b.Note); note != "" {
			text += "\n原因：" + htmlEscape(note)
		}
		h.notifyUser(o.UserID, 0, text+"\n\n如有疑问请联系客服。")
	}
	walletReply(c, err)
}

func walletReply(c *gin.Context, err error) {
	var ue wallet.UserError
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"ok": true})
	case errors.As(err, &ue):
		c.JSON(http.StatusBadRequest, gin.H{"error": ue.Msg})
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "记录不存在"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "操作失败"})
	}
}
