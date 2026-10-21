package shop

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// reviewWindowDays is how long after completion a buyer can review an order.
const reviewWindowDays = 90

type ReviewInput struct {
	OrderItemID uint64 `json:"order_item_id"`
	Rating      int    `json:"rating"`
	Content     string `json:"content"`
}

// maskName shows only the first character of the buyer's name.
func maskName(u model.TelegramUser) string {
	name := strings.TrimSpace(u.FirstName)
	if name == "" {
		name = u.Username
	}
	if name == "" {
		return "匿名用户"
	}
	r, _ := utf8.DecodeRuneInString(name)
	return string(r) + "**"
}

// Review records ratings for items of the buyer's completed order; each item can be reviewed once.
func (s *Service) Review(userID, orderID uint64, inputs []ReviewInput) error {
	if len(inputs) == 0 {
		return userErr("请为商品打分")
	}
	for _, in := range inputs {
		if in.Rating < 1 || in.Rating > 5 {
			return userErr("评分需在 1–5 星之间")
		}
		if err := checkLen(in.Content, "评价内容", 1000, false); err != nil {
			return err
		}
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		o, err := lockOrder(tx, orderID, Scope{UserID: userID})
		if err != nil {
			return err
		}
		if o.Status != StatusCompleted || o.CompletedAt == nil {
			return userErr("订单完成后才能评价")
		}
		if time.Since(*o.CompletedAt) > reviewWindowDays*24*time.Hour {
			return userErr("已超过评价期限")
		}
		var u model.TelegramUser
		if err := tx.First(&u, userID).Error; err != nil {
			return err
		}
		for _, in := range inputs {
			var item model.ShopOrderItem
			if err := tx.Where("id = ? AND order_id = ?", in.OrderItemID, o.ID).First(&item).Error; err != nil {
				return userErr("商品不属于该订单")
			}
			err := tx.Create(&model.ShopReview{OrderItemID: item.ID, OrderID: o.ID, ProductID: item.ProductID, MerchantID: o.MerchantID, UserID: userID, BuyerName: maskName(u), ProductName: item.ProductName, SKUName: item.SKUName, Rating: in.Rating, Content: strings.TrimSpace(in.Content)}).Error
			if store.IsDuplicate(err) {
				return userErr("「%s」已评价过", item.ProductName)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// ReviewedItems returns which items of an order the buyer has already reviewed.
func (s *Service) reviewedItems(orderID uint64) ([]uint64, error) {
	ids := []uint64{}
	err := s.st.DB.Model(&model.ShopReview{}).Where("order_id = ?", orderID).Pluck("order_item_id", &ids).Error
	return ids, err
}

// ProductReviews lists visible reviews of a product, newest first.
func (s *Service) ProductReviews(productID uint64, page int) ([]model.ShopReview, error) {
	if page < 1 {
		page = 1
	}
	rows := []model.ShopReview{}
	err := s.st.DB.Where("product_id = ? AND hidden = ?", productID, false).Order("id desc").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error
	return rows, err
}

// ---- Administration ----

func (s *Service) AdminReviews(merchantID uint64) ([]model.ShopReview, error) {
	q := s.st.DB.Order("id desc").Limit(500)
	if merchantID != 0 {
		q = q.Where("merchant_id = ?", merchantID)
	}
	rows := []model.ShopReview{}
	err := q.Find(&rows).Error
	return rows, err
}

func (s *Service) lockReview(tx *gorm.DB, id uint64, scope Scope) (model.ShopReview, error) {
	var r model.ShopReview
	q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id)
	if scope.MerchantID != 0 {
		q = q.Where("merchant_id = ?", scope.MerchantID)
	}
	err := q.First(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return r, userErr("评价不存在")
	}
	return r, err
}

// ReplyReview stores the merchant's public reply (empty clears it).
func (s *Service) ReplyReview(id uint64, scope Scope, reply string) error {
	if err := checkLen(reply, "回复", 1000, false); err != nil {
		return err
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		r, err := s.lockReview(tx, id, scope)
		if err != nil {
			return err
		}
		reply = strings.TrimSpace(reply)
		var at *time.Time
		if reply != "" {
			now := time.Now()
			at = &now
		}
		return tx.Model(&model.ShopReview{}).Where("id = ?", r.ID).Updates(map[string]any{"reply": reply, "replied_at": at}).Error
	})
}

// SetReviewHidden hides or shows a review in the shop (platform accounts only).
func (s *Service) SetReviewHidden(id uint64, hidden bool) error {
	res := s.st.DB.Model(&model.ShopReview{}).Where("id = ?", id).Update("hidden", hidden)
	if res.Error == nil && res.RowsAffected == 0 {
		var n int64
		if s.st.DB.Model(&model.ShopReview{}).Where("id = ?", id).Count(&n); n == 0 {
			return userErr("评价不存在")
		}
	}
	return res.Error
}
