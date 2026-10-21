package shop

import (
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
)

// Merchant application statuses.
const (
	ApplicationPending  = "pending"
	ApplicationApproved = "approved"
	ApplicationRejected = "rejected"
)

type ApplicationInput struct {
	ShopName    string `json:"shop_name"`
	Contact     string `json:"contact"`
	Category    string `json:"category"`
	Address     string `json:"address"`
	Description string `json:"description"`
}

// ApplyMerchant records a store application; a buyer can have one pending application at a time.
func (s *Service) ApplyMerchant(b Buyer, in ApplicationInput) (*model.ShopMerchantApplication, error) {
	for _, f := range []struct {
		v, name  string
		max      int
		required bool
	}{{in.ShopName, "店铺名称", 64, true}, {in.Contact, "联系方式", 128, true}, {in.Category, "经营类目", 64, false}, {in.Address, "地址", 256, false}, {in.Description, "店铺介绍", 1000, false}} {
		if err := checkLen(f.v, f.name, f.max, f.required); err != nil {
			return nil, err
		}
	}
	var a model.ShopMerchantApplication
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockBuyer(tx, b.UserID); err != nil {
			return err
		}
		var pending int64
		if err := tx.Model(&model.ShopMerchantApplication{}).Where("user_id = ? AND status = ?", b.UserID, ApplicationPending).Count(&pending).Error; err != nil {
			return err
		}
		if pending > 0 {
			return userErr("你已有一份入驻申请正在审核")
		}
		a = model.ShopMerchantApplication{UserID: b.UserID, TelegramUserID: b.TelegramUserID, BotID: b.BotID, ShopName: strings.TrimSpace(in.ShopName), Contact: strings.TrimSpace(in.Contact), Category: strings.TrimSpace(in.Category), Address: strings.TrimSpace(in.Address), Description: strings.TrimSpace(in.Description), Status: ApplicationPending}
		return tx.Create(&a).Error
	})
	if err != nil {
		return nil, err
	}
	s.notifyStaff(0, fmt.Sprintf("🏪 <b>新的商家入驻申请</b>\n\n店铺：%s\n类目：%s\n联系方式：%s\n申请人 Telegram ID：<code>%d</code>", htmlEscape(a.ShopName), htmlEscape(a.Category), htmlEscape(a.Contact), a.TelegramUserID))
	return &a, nil
}

// MyApplication returns the buyer's latest application, or nil.
func (s *Service) MyApplication(userID uint64) (*model.ShopMerchantApplication, error) {
	var a model.ShopMerchantApplication
	err := s.st.DB.Where("user_id = ?", userID).Order("id desc").First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &a, err
}

// lockBuyer serialises a buyer's own writes (checkout, applications) on their user row. It must be
// the first statement of a transaction so later reads see rows committed while it waited.
func lockBuyer(tx *gorm.DB, userID uint64) error {
	return tx.Exec("SELECT id FROM telegram_users WHERE id = ? FOR UPDATE", userID).Error
}
