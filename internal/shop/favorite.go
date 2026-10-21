package shop

import (
	"time"

	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

const maxHistory = 100

// SetFavorite saves or removes a product from the buyer's favorites.
func (s *Service) SetFavorite(userID, productID uint64, on bool) error {
	if !on {
		return s.st.DB.Where("user_id = ? AND product_id = ?", userID, productID).Delete(&model.ShopFavorite{}).Error
	}
	if _, err := s.Product(productID); err != nil {
		return err
	}
	return s.st.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ShopFavorite{UserID: userID, ProductID: productID}).Error
}

func (s *Service) IsFavorite(userID, productID uint64) bool {
	var n int64
	s.st.DB.Model(&model.ShopFavorite{}).Where("user_id = ? AND product_id = ?", userID, productID).Count(&n)
	return n > 0
}

// productsByIDs returns visible products as cards in the given order.
func (s *Service) productsByIDs(ids []uint64) ([]ProductCard, error) {
	if len(ids) == 0 {
		return []ProductCard{}, nil
	}
	var products []model.ShopProduct
	if err := s.visibleProducts().Where("shop_products.id IN ?", ids).Find(&products).Error; err != nil {
		return nil, err
	}
	byID := map[uint64]model.ShopProduct{}
	for _, p := range products {
		byID[p.ID] = p
	}
	ordered := make([]model.ShopProduct, 0, len(products))
	for _, id := range ids {
		if p, ok := byID[id]; ok {
			ordered = append(ordered, p)
		}
	}
	return s.cards(ordered)
}

// Favorites lists the buyer's saved products that are still on sale, newest first.
func (s *Service) Favorites(userID uint64) ([]ProductCard, error) {
	var ids []uint64
	if err := s.st.DB.Model(&model.ShopFavorite{}).Where("user_id = ?", userID).Order("created_at desc").Limit(200).Pluck("product_id", &ids).Error; err != nil {
		return nil, err
	}
	return s.productsByIDs(ids)
}

// RecordView remembers that the buyer opened a product, keeping the latest maxHistory entries.
func (s *Service) RecordView(userID, productID uint64) error {
	now := time.Now()
	if err := s.st.DB.Clauses(clause.OnConflict{DoUpdates: clause.Assignments(map[string]any{"viewed_at": now})}).Create(&model.ShopView{UserID: userID, ProductID: productID, ViewedAt: now}).Error; err != nil {
		return err
	}
	var cutoff []time.Time
	if err := s.st.DB.Model(&model.ShopView{}).Where("user_id = ?", userID).Order("viewed_at desc").Offset(maxHistory).Limit(1).Pluck("viewed_at", &cutoff).Error; err != nil || len(cutoff) == 0 {
		return err
	}
	return s.st.DB.Where("user_id = ? AND viewed_at <= ?", userID, cutoff[0]).Delete(&model.ShopView{}).Error
}

func (s *Service) History(userID uint64) ([]ProductCard, error) {
	var ids []uint64
	if err := s.st.DB.Model(&model.ShopView{}).Where("user_id = ?", userID).Order("viewed_at desc").Limit(maxHistory).Pluck("product_id", &ids).Error; err != nil {
		return nil, err
	}
	return s.productsByIDs(ids)
}

func (s *Service) ClearHistory(userID uint64) error {
	return s.st.DB.Where("user_id = ?", userID).Delete(&model.ShopView{}).Error
}
