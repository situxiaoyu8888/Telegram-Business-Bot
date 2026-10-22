package admin

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

func (h *Handler) currencies(c *gin.Context) {
	rows, err := h.wallet.Currencies(false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "币种加载失败"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// saveCurrency creates a currency (id 0) or updates one. The code and kind of an existing
// currency are fixed because balances and withdrawals refer to them.
func (h *Handler) saveCurrency(c *gin.Context) {
	var b model.WalletCurrency
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || utf8.RuneCountInString(b.Name) > 32 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请填写 32 字以内的币种名称"})
		return
	}
	rate, err := decimal.NewFromString(strings.TrimSpace(b.Rate))
	if err != nil || !rate.IsPositive() || !rate.Equal(rate.Truncate(8)) || rate.GreaterThan(decimal.New(1, 12)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "汇率需为大于 0 的数字（最多 8 位小数），表示 1 USDT 可兑换的数量"})
		return
	}
	if b.Decimals < 0 || b.Decimals > 6 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "小数位需在 0–6 之间"})
		return
	}
	for _, v := range []struct{ val *string }{{&b.MinWithdraw}, {&b.WithdrawFee}} {
		d, err := decimal.NewFromString(strings.TrimSpace(*v.val))
		if strings.TrimSpace(*v.val) == "" {
			d, err = decimal.Zero, nil
		}
		if err != nil || d.IsNegative() || !d.Equal(d.Truncate(6)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "最低提现金额和提现手续费需为不小于 0 的数字"})
			return
		}
		*v.val = d.StringFixed(6)
	}
	b.Icon = strings.TrimSpace(b.Icon)
	if utf8.RuneCountInString(b.Icon) > 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "图标请填写一个国旗 emoji 或符号"})
		return
	}
	fields := map[string]any{"name": b.Name, "icon": b.Icon, "rate": rate.StringFixed(8), "decimals": b.Decimals, "min_withdraw": b.MinWithdraw, "withdraw_fee": b.WithdrawFee,
		"exchangeable": b.Exchangeable, "withdrawable": b.Withdrawable, "enabled": b.Enabled, "sort": b.Sort}
	if b.ID == 0 {
		b.Code = strings.ToUpper(strings.TrimSpace(b.Code))
		if !wallet.ValidCurrencyCode(b.Code) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "币种代码需为 2–16 位大写字母、数字或下划线，例如 VND"})
			return
		}
		row := model.WalletCurrency{Code: b.Code, Kind: wallet.KindFiat, Rate: rate.StringFixed(8)}
		if err := h.st.DB.Create(&row).Error; err != nil {
			if store.IsDuplicate(err) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "币种代码已存在"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
			return
		}
		b.ID = row.ID
	} else {
		var cur model.WalletCurrency
		if err := h.st.DB.First(&cur, b.ID).Error; err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "币种不存在"})
			return
		}
		// The USDT wallet pays for the shop and receives TRC20 deposits; it cannot be switched off.
		if cur.Code == "USDT" && !b.Enabled {
			c.JSON(http.StatusBadRequest, gin.H{"error": "USDT-TRC20 是商城支付币种，不能停用"})
			return
		}
	}
	if err := h.st.DB.Model(&model.WalletCurrency{}).Where("id = ?", b.ID).Updates(fields).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "保存失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "id": b.ID})
}

type exchangeRow struct {
	model.WalletExchange
	TelegramUserID int64 `json:"telegram_user_id"`
}

func (h *Handler) exchanges(c *gin.Context) {
	rows := []exchangeRow{}
	err := h.st.DB.Table("wallet_exchanges e").Select("e.*, u.telegram_user_id").Joins("LEFT JOIN telegram_users u ON u.id = e.user_id").Order("e.id desc").Limit(500).Scan(&rows).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "换汇记录加载失败"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

func (h *Handler) userBalances(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	rows, err := h.wallet.Balances(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "余额加载失败"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// adjustBalance credits or debits one currency of a user's wallet, e.g. after an ERC20 or cash
// deposit that the program cannot see.
func (h *Handler) adjustBalance(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Currency string `json:"currency"`
		Amount   string `json:"amount"`
		Remark   string `json:"remark"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误"})
		return
	}
	var u model.TelegramUser
	if err := h.st.DB.First(&u, id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "用户不存在"})
		return
	}
	if err := h.wallet.AdminAdjust(u.ID, b.Currency, b.Amount, operator(c), b.Remark); err != nil {
		var ue wallet.UserError
		if errors.As(err, &ue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": ue.Msg})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "调整失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
