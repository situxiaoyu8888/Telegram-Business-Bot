package admin

import (
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/telegram"
	"unicode/utf8"
)

func (h *Handler) merchants(c *gin.Context) {
	rows := []model.Merchant{}
	q := h.st.DB.Order("sort asc,id desc")
	if id := merchantScope(c); id != 0 {
		q = q.Where("id = ?", id)
	}
	if err := q.Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "商家加载失败"})
		return
	}
	c.JSON(200, rows)
}
func validateMerchant(b *model.Merchant) error {
	b.Name = strings.TrimSpace(b.Name)
	b.Description = strings.TrimSpace(b.Description)
	b.Address = strings.TrimSpace(b.Address)
	b.Hours = strings.TrimSpace(b.Hours)
	b.Phone = strings.TrimSpace(b.Phone)
	b.ContactURL = strings.TrimSpace(b.ContactURL)
	if b.Name == "" || b.MenuID == 0 {
		return errors.New("请填写商家名称并关联子菜单")
	}
	for _, f := range []struct {
		v   string
		max int
	}{{b.Name, 64}, {b.Description, 1000}, {b.Address, 256}, {b.Hours, 100}, {b.Phone, 64}, {b.ContactURL, 1024}} {
		if utf8.RuneCountInString(f.v) > f.max {
			return errors.New("商家信息过长：名称 64 字、简介 1000 字、地址 256 字、营业时间 100 字、电话 64 字、链接 1024 字以内")
		}
	}
	if b.NotifyChatID < 0 {
		return errors.New("订单通知 Telegram ID 无效")
	}
	if strings.TrimSpace(b.CommissionRate) == "" {
		b.CommissionRate = "0"
	}
	rate, err := decimal.NewFromString(strings.TrimSpace(b.CommissionRate))
	if err != nil || rate.IsNegative() || !rate.LessThan(decimal.NewFromInt(1)) || !rate.Equal(rate.Truncate(6)) {
		return errors.New("平台佣金比例需在 0 到 1 之间（如 0.05 表示 5%），最多 6 位小数")
	}
	b.CommissionRate = rate.StringFixed(6)
	if b.ContactURL != "" && !telegram.IsTelegramURL(b.ContactURL) {
		return errors.New("联系链接必须是有效的 HTTPS 地址，例如 https://t.me/用户名")
	}
	return nil
}
func (h *Handler) saveMerchant(c *gin.Context) {
	var b model.Merchant
	if c.ShouldBindJSON(&b) != nil {
		c.JSON(400, gin.H{"error": "无效商家信息"})
		return
	}
	if err := validateMerchant(&b); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	err := h.st.DB.Transaction(func(tx *gorm.DB) error {
		var category model.MenuItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&category, b.MenuID).Error; err != nil {
			return errors.New("关联子菜单不存在")
		}
		if category.ParentID == 0 || category.ActionType != "merchants" {
			return errors.New("请选择入口类型为“商家列表”的子菜单")
		}
		if b.ID != 0 {
			var old model.Merchant
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&old, b.ID).Error; err != nil {
				return err
			}
			// The payout address is the merchant's own (set when it requests a payout); the console
			// form never carries it, so it is not overwritten here.
			return tx.Model(&old).Select("pinned", "menu_id", "name", "description", "address", "hours", "phone", "contact_url", "notify_chat_id", "commission_rate", "sort", "enabled").Updates(&b).Error
		}
		b.PayoutAddress = ""
		b.CreatedAt = model.Merchant{}.CreatedAt
		b.UpdatedAt = b.CreatedAt
		return tx.Create(&b).Error
	})
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, b)
}
func (h *Handler) deleteMerchant(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res := h.st.DB.Delete(&model.Merchant{}, id)
	if res.Error != nil {
		c.JSON(500, gin.H{"error": "删除失败"})
		return
	}
	if res.RowsAffected == 0 {
		c.JSON(404, gin.H{"error": "商家不存在"})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}
