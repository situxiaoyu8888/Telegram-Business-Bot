package admin

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
)

func (h *Handler) shopApplications(c *gin.Context) {
	rows := []model.ShopMerchantApplication{}
	q := h.st.DB.Order("id desc").Limit(500)
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	err := q.Find(&rows).Error
	listOrError(c, rows, err)
}

// randomPassword returns a 12-character initial password without look-alike characters.
func randomPassword() (string, error) {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 12)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		b[i] = alphabet[n.Int64()]
	}
	return string(b), nil
}

// approveShopApplication creates the merchant and its console account in one transaction and
// sends the login to the applicant through the bot they applied from.
func (h *Handler) approveShopApplication(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		MenuID         uint64 `json:"menu_id"`
		CommissionRate string `json:"commission_rate"`
		Note           string `json:"note"`
	}
	if err := c.ShouldBindJSON(&b); err != nil || b.MenuID == 0 {
		c.JSON(400, gin.H{"error": "请选择商家所属的分类菜单"})
		return
	}
	rate, err := decimal.NewFromString(strings.TrimSpace(b.CommissionRate))
	if strings.TrimSpace(b.CommissionRate) == "" {
		rate, err = decimal.Zero, nil
	}
	if err != nil || rate.IsNegative() || !rate.LessThan(decimal.NewFromInt(1)) || !rate.Equal(rate.Truncate(6)) {
		c.JSON(400, gin.H{"error": "平台佣金比例需在 0 到 1 之间（如 0.05）"})
		return
	}
	password, err := randomPassword()
	if err != nil {
		respond(c, err)
		return
	}
	hash, err := hashPassword(password)
	if err != nil {
		respond(c, err)
		return
	}
	var app model.ShopMerchantApplication
	var username string
	err = h.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&app, id).Error; err != nil {
			return errors.New("申请不存在")
		}
		if app.Status != shop.ApplicationPending {
			return errors.New("该申请已处理")
		}
		var menu model.MenuItem
		if err := tx.First(&menu, b.MenuID).Error; err != nil || menu.ParentID == 0 || menu.ActionType != "merchants" {
			return errors.New("请选择入口类型为“商家列表”的子菜单")
		}
		m := model.Merchant{MenuID: b.MenuID, Name: app.ShopName, Description: app.Description, Address: app.Address, Phone: app.Contact, Enabled: true, NotifyChatID: app.TelegramUserID, CommissionRate: rate.StringFixed(6)}
		if utfLen(m.Name) > 64 {
			m.Name = string([]rune(m.Name)[:64])
		}
		if err := tx.Create(&m).Error; err != nil {
			return err
		}
		username = fmt.Sprintf("shop%d", m.ID)
		err := tx.Create(&model.AdminUser{Username: username, PasswordHash: hash, Role: roleMerchant, MerchantID: m.ID, Enabled: true}).Error
		if store.IsDuplicate(err) {
			suffix, rerr := secureRandomName(3) // never derived from the password
			if rerr != nil {
				return rerr
			}
			username = fmt.Sprintf("shop%d_%s", m.ID, suffix)
			err = tx.Create(&model.AdminUser{Username: username, PasswordHash: hash, Role: roleMerchant, MerchantID: m.ID, Enabled: true}).Error
		}
		if err != nil {
			return err
		}
		now := time.Now()
		app.Status, app.MerchantID, app.Username = shop.ApplicationApproved, m.ID, username
		return tx.Model(&model.ShopMerchantApplication{}).Where("id = ?", app.ID).Updates(map[string]any{"status": shop.ApplicationApproved, "merchant_id": m.ID, "username": username, "admin_note": strings.TrimSpace(b.Note), "handled_by": operator(c), "handled_at": now}).Error
	})
	if err != nil {
		respond(c, err)
		return
	}
	text := fmt.Sprintf("🎉 <b>商家入驻申请已通过</b>\n\n店铺：%s\n后台账号：<code>%s</code>\n初始密码：<code>%s</code>\n", htmlEscape(app.ShopName), username, password)
	if h.publicURL != "" {
		text += "后台地址：" + htmlEscape(h.publicURL) + "\n"
	}
	text += "\n请登录后立即点击左下角「改密」修改密码。"
	delivered := h.sendToApplicant(c.Request.Context(), app, text)
	// The password is shown once to the reviewer only if the applicant could not be messaged.
	res := gin.H{"ok": true, "username": username, "merchant_id": app.MerchantID, "delivered": delivered}
	if !delivered {
		res["password"] = password
	}
	c.JSON(200, res)
}

func (h *Handler) rejectShopApplication(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	var b struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&b)
	note := strings.TrimSpace(b.Note)
	if note == "" || utfLen(note) > 512 {
		c.JSON(400, gin.H{"error": "请填写拒绝原因（512 字以内）"})
		return
	}
	var app model.ShopMerchantApplication
	err := h.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&app, id).Error; err != nil {
			return errors.New("申请不存在")
		}
		if app.Status != shop.ApplicationPending {
			return errors.New("该申请已处理")
		}
		return tx.Model(&model.ShopMerchantApplication{}).Where("id = ?", app.ID).Updates(map[string]any{"status": shop.ApplicationRejected, "admin_note": note, "handled_by": operator(c), "handled_at": time.Now()}).Error
	})
	if err != nil {
		respond(c, err)
		return
	}
	h.sendToApplicant(c.Request.Context(), app, fmt.Sprintf("😔 <b>商家入驻申请未通过</b>\n\n店铺：%s\n原因：%s", htmlEscape(app.ShopName), htmlEscape(note)))
	c.JSON(200, gin.H{"ok": true})
}

// sendToApplicant messages the applicant through the bot they applied from and reports success.
func (h *Handler) sendToApplicant(ctx context.Context, app model.ShopMerchantApplication, html string) bool {
	if h.bots == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sender, err := h.bots.Sender(ctx, app.BotID)
	if err == nil {
		err = sender.SendMessageWithParseMode(ctx, app.TelegramUserID, html, "HTML", nil)
	}
	if err != nil && h.logger != nil {
		h.logger.Warn("notify merchant applicant", zap.Uint64("application", app.ID), zap.Error(err))
	}
	return err == nil
}

func utfLen(s string) int { return len([]rune(s)) }
