package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

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

// NotifyDeposit tells the user an on-chain deposit has been credited (the wallet's deposit hook).
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

func (h *Handler) addressPool(c *gin.Context) {
	rows := []model.DepositPoolAddress{}
	if err := h.st.DB.Order("status desc, user_id = 0 desc, id desc").Limit(2000).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "地址池加载失败"})
		return
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
	// Addresses already in the pool or bound to a user are skipped.
	var existing []string
	addrs := make([]string, len(rows))
	for i, r := range rows {
		addrs[i] = r.Address
	}
	h.st.DB.Model(&model.DepositPoolAddress{}).Where("address IN ?", addrs).Pluck("address", &existing)
	var bound []string
	h.st.DB.Model(&model.DepositAddress{}).Where("address IN ?", addrs).Pluck("address", &bound)
	skip := map[string]bool{}
	for _, a := range append(existing, bound...) {
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

// deletePoolAddress removes a free address from the pool; assigned ones stay for the record.
func (h *Handler) deletePoolAddress(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res := h.st.DB.Where("id = ? AND user_id = 0", id).Delete(&model.DepositPoolAddress{})
	if res.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除失败"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "已分配给用户的地址不能删除"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// saveAddress gives a user a deposit address from the operator's pool: the chosen free address,
// or the oldest free one of the network (pool_id 0). A user's previous address of that network is
// retired, never reassigned. TRC20 addresses are scanned by the TRON watcher; ERC20 ones are only
// shown to the user, who is credited from the console.
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
	var picked model.DepositPoolAddress
	err := h.st.DB.Transaction(func(tx *gorm.DB) error {
		q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("network = ? AND user_id = 0 AND status = 1", b.Network)
		if b.PoolID != 0 {
			q = q.Where("id = ?", b.PoolID)
		}
		if err := q.Order("id asc").First(&picked).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return userError("地址池中没有可用的 " + b.Network + " 地址，请先在「地址池」中添加")
			}
			return err
		}
		now := time.Now()
		var old model.DepositAddress
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND network = ?", b.UserID, b.Network).First(&old).Error
		switch {
		case err == nil:
			if err := tx.Model(&model.DepositPoolAddress{}).Where("address = ?", old.Address).Update("status", 0).Error; err != nil {
				return err
			}
			if err := tx.Model(&old).Updates(map[string]any{"address": picked.Address, "status": 1, "scan_timestamp": 0}).Error; err != nil {
				return err
			}
		case errors.Is(err, gorm.ErrRecordNotFound):
			if err := tx.Create(&model.DepositAddress{UserID: b.UserID, Network: b.Network, Address: picked.Address, Status: 1}).Error; err != nil {
				return err
			}
		default:
			return err
		}
		return tx.Model(&picked).Updates(map[string]any{"user_id": b.UserID, "assigned_at": now}).Error
	})
	if err != nil {
		var ue userError
		if errors.As(err, &ue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": string(ue)})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "绑定失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "address": picked.Address})
}

type userError string

func (e userError) Error() string { return string(e) }
