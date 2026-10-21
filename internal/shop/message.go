package shop

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

const maxMessageLen = 1000

func preview(content string) string {
	content = strings.Join(strings.Fields(content), " ")
	if utf8.RuneCountInString(content) > 60 {
		return string([]rune(content)[:60]) + "…"
	}
	return content
}

type MessageInput struct {
	Content   string `json:"content"`
	ProductID uint64 `json:"product_id"`
	OrderID   uint64 `json:"order_id"`
}

// SendToMerchant adds a buyer message to their thread with a merchant, creating it if needed.
// A referenced order must be the buyer's own order with that merchant.
func (s *Service) SendToMerchant(b Buyer, buyerName string, merchantID uint64, in MessageInput) (*model.ShopMessage, error) {
	in.Content = strings.TrimSpace(in.Content)
	if err := checkLen(in.Content, "消息", maxMessageLen, true); err != nil {
		return nil, err
	}
	var m model.Merchant
	if err := s.st.DB.Select("id", "name", "enabled").First(&m, merchantID).Error; err != nil || !m.Enabled {
		return nil, userErr("商家不存在或已停止营业")
	}
	if in.OrderID != 0 {
		var n int64
		if err := s.st.DB.Model(&model.ShopOrder{}).Where("id = ? AND user_id = ? AND merchant_id = ?", in.OrderID, b.UserID, merchantID).Count(&n).Error; err != nil || n == 0 {
			in.OrderID = 0
		}
	}
	var msg model.ShopMessage
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		c := model.ShopConversation{UserID: b.UserID, MerchantID: merchantID, TelegramUserID: b.TelegramUserID, BotID: b.BotID, BuyerName: buyerName, LastAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&c).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND merchant_id = ?", b.UserID, merchantID).First(&c).Error; err != nil {
			return err
		}
		msg = model.ShopMessage{ConversationID: c.ID, Content: in.Content, ProductID: in.ProductID, OrderID: in.OrderID}
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}
		return tx.Model(&model.ShopConversation{}).Where("id = ?", c.ID).Updates(map[string]any{"bot_id": b.BotID, "buyer_name": buyerName, "last_message": preview(in.Content), "last_at": now, "merchant_unread": gorm.Expr("merchant_unread + 1")}).Error
	})
	if err != nil {
		return nil, err
	}
	s.notifyStaff(merchantID, fmt.Sprintf("💬 <b>买家消息</b> · %s\n\n%s：%s\n\n请在后台「商家消息」中回复。", htmlEscape(m.Name), htmlEscape(buyerName), htmlEscape(preview(in.Content))))
	return &msg, nil
}

// Thread is a conversation with its latest messages, oldest first.
type Thread struct {
	Conversation model.ShopConversation `json:"conversation"`
	MerchantName string                 `json:"merchant_name"`
	Messages     []model.ShopMessage    `json:"messages"`
}

func (s *Service) thread(c model.ShopConversation) (*Thread, error) {
	t := &Thread{Conversation: c, Messages: []model.ShopMessage{}}
	var m model.Merchant
	if s.st.DB.Select("name").First(&m, c.MerchantID).Error == nil {
		t.MerchantName = m.Name
	}
	if err := s.st.DB.Where("conversation_id = ?", c.ID).Order("id desc").Limit(100).Find(&t.Messages).Error; err != nil {
		return nil, err
	}
	for i, j := 0, len(t.Messages)-1; i < j; i, j = i+1, j-1 {
		t.Messages[i], t.Messages[j] = t.Messages[j], t.Messages[i]
	}
	return t, nil
}

// BuyerThread returns the buyer's thread with a merchant (empty if none yet) and marks it read.
func (s *Service) BuyerThread(userID, merchantID uint64) (*Thread, error) {
	var c model.ShopConversation
	err := s.st.DB.Where("user_id = ? AND merchant_id = ?", userID, merchantID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		t := &Thread{Conversation: model.ShopConversation{UserID: userID, MerchantID: merchantID}, Messages: []model.ShopMessage{}}
		var m model.Merchant
		if s.st.DB.Select("name").First(&m, merchantID).Error != nil {
			return nil, userErr("商家不存在")
		}
		t.MerchantName = m.Name
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	if c.BuyerUnread > 0 {
		s.st.DB.Model(&model.ShopConversation{}).Where("id = ?", c.ID).Update("buyer_unread", 0)
		c.BuyerUnread = 0
	}
	return s.thread(c)
}

// ConversationRow is a thread in a list, with the merchant's name.
type ConversationRow struct {
	model.ShopConversation
	MerchantName string `json:"merchant_name"`
}

func (s *Service) conversations(q *gorm.DB) ([]ConversationRow, error) {
	rows := []ConversationRow{}
	err := q.Table("shop_conversations").Select("shop_conversations.*, merchants.name AS merchant_name").
		Joins("LEFT JOIN merchants ON merchants.id = shop_conversations.merchant_id").Order("shop_conversations.last_at desc").Limit(200).Scan(&rows).Error
	return rows, err
}

func (s *Service) BuyerConversations(userID uint64) ([]ConversationRow, error) {
	return s.conversations(s.st.DB.Where("shop_conversations.user_id = ?", userID))
}

// ---- Administration ----

// AdminConversations lists threads, limited to one merchant when merchantID is set.
func (s *Service) AdminConversations(merchantID uint64) ([]ConversationRow, error) {
	q := s.st.DB
	if merchantID != 0 {
		q = q.Where("shop_conversations.merchant_id = ?", merchantID)
	}
	return s.conversations(q)
}

func lockConversation(tx *gorm.DB, id uint64, scope Scope) (model.ShopConversation, error) {
	var c model.ShopConversation
	q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id)
	if scope.MerchantID != 0 {
		q = q.Where("merchant_id = ?", scope.MerchantID)
	}
	err := q.First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return c, userErr("会话不存在")
	}
	return c, err
}

// AdminThread returns a thread for the console and marks it read by the merchant.
func (s *Service) AdminThread(id uint64, scope Scope) (*Thread, error) {
	var c model.ShopConversation
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if c, err = lockConversation(tx, id, scope); err != nil {
			return err
		}
		c.MerchantUnread = 0
		return tx.Model(&model.ShopConversation{}).Where("id = ?", id).Update("merchant_unread", 0).Error
	})
	if err != nil {
		return nil, err
	}
	return s.thread(c)
}

// ReplyToBuyer adds a merchant reply and tells the buyer through the bot they last wrote from.
func (s *Service) ReplyToBuyer(id uint64, scope Scope, content, operator string) error {
	content = strings.TrimSpace(content)
	if err := checkLen(content, "回复", maxMessageLen, true); err != nil {
		return err
	}
	var c model.ShopConversation
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if c, err = lockConversation(tx, id, scope); err != nil {
			return err
		}
		if err := tx.Create(&model.ShopMessage{ConversationID: c.ID, FromMerchant: true, Content: content, Operator: operator}).Error; err != nil {
			return err
		}
		return tx.Model(&model.ShopConversation{}).Where("id = ?", c.ID).Updates(map[string]any{"last_message": preview(content), "last_at": time.Now(), "buyer_unread": gorm.Expr("buyer_unread + 1"), "merchant_unread": 0}).Error
	})
	if err == nil && s.notifier != nil {
		var m model.Merchant
		s.st.DB.Select("name").First(&m, c.MerchantID)
		s.notifyChat(c.BotID, c.TelegramUserID, fmt.Sprintf("💬 <b>%s 回复了你</b>\n\n%s", htmlEscape(m.Name), htmlEscape(content)), fmt.Sprintf("/chat/%d", c.MerchantID))
	}
	return err
}
