package shop

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

const maxQuantity = 999

// CartLine is a cart entry with current product data. Unavailable lines stay in the cart
// (so the buyer sees what changed) but cannot be checked out.
type CartLine struct {
	SKUID        uint64 `json:"sku_id"`
	ProductID    uint64 `json:"product_id"`
	ProductName  string `json:"product_name"`
	SKUName      string `json:"sku_name"`
	Cover        string `json:"cover"`
	Price        string `json:"price"`
	ListPrice    string `json:"list_price"`
	Quantity     int64  `json:"quantity"`
	Stock        int64  `json:"stock"`
	Kind         string `json:"kind"`
	AllowExpress bool   `json:"allow_express"`
	AllowPickup  bool   `json:"allow_pickup"`
	MerchantID   uint64 `json:"merchant_id"`
	MerchantName string `json:"merchant_name"`
	Available    bool   `json:"available"`
}

// line is an SKU joined with its product and merchant.
type line struct {
	model.ShopSKU
	ProductName     string
	ProductEnabled  bool
	Images          string
	Kind            string
	AllowExpress    bool
	AllowPickup     bool
	MerchantID      uint64
	MerchantName    string
	MerchantEnabled bool
	MerchantAddress string
	PromoStartsAt   *time.Time
	PromoEndsAt     *time.Time
	// ListPrice is the regular price; Price is what the buyer pays now (see effectivePrice).
	ListPrice string
}

func (s *Service) lines(db *gorm.DB, skuIDs []uint64) (map[uint64]line, error) {
	out := map[uint64]line{}
	if len(skuIDs) == 0 {
		return out, nil
	}
	var rows []line
	err := db.Table("shop_skus").
		Select("shop_skus.*, shop_products.name AS product_name, shop_products.enabled AS product_enabled, shop_products.images, shop_products.kind, shop_products.allow_express, shop_products.allow_pickup, merchants.id AS merchant_id, merchants.name AS merchant_name, merchants.enabled AS merchant_enabled, merchants.address AS merchant_address, shop_products.promo_starts_at, shop_products.promo_ends_at").
		Joins("JOIN shop_products ON shop_products.id = shop_skus.product_id").
		Joins("JOIN merchants ON merchants.id = shop_products.merchant_id").
		Where("shop_skus.id IN ?", skuIDs).Scan(&rows).Error
	now := time.Now()
	for _, r := range rows {
		r.ListPrice = r.Price
		r.Price, _ = effectivePrice(r.Price, r.PromoPrice, r.PromoStartsAt, r.PromoEndsAt, now)
		out[r.ID] = r
	}
	return out, err
}

func (l line) available() bool { return l.Enabled && l.ProductEnabled && l.MerchantEnabled }

func (s *Service) Cart(userID uint64) ([]CartLine, error) {
	var items []model.ShopCartItem
	if err := s.st.DB.Where("user_id = ?", userID).Order("id desc").Find(&items).Error; err != nil {
		return nil, err
	}
	ids := make([]uint64, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.SKUID)
	}
	ls, err := s.lines(s.st.DB, ids)
	if err != nil {
		return nil, err
	}
	out := []CartLine{}
	for _, it := range items {
		l, ok := ls[it.SKUID]
		if !ok {
			continue
		}
		out = append(out, CartLine{SKUID: l.ID, ProductID: l.ProductID, ProductName: l.ProductName, SKUName: l.Name, Cover: firstImage(l.Images), Price: l.Price, ListPrice: l.ListPrice, Quantity: it.Quantity, Stock: l.Stock, Kind: l.Kind, AllowExpress: l.AllowExpress, AllowPickup: l.AllowPickup, MerchantID: l.MerchantID, MerchantName: l.MerchantName, Available: l.available() && l.Stock >= it.Quantity})
	}
	return out, nil
}

// SetCart adds to (add=true) or sets the quantity of a cart line; a resulting quantity of 0 removes it.
func (s *Service) SetCart(userID, skuID uint64, quantity int64, add bool) error {
	if quantity < 0 || quantity > maxQuantity {
		return userErr("数量需在 0–%d 之间", maxQuantity)
	}
	ls, err := s.lines(s.st.DB, []uint64{skuID})
	if err != nil {
		return err
	}
	l, ok := ls[skuID]
	if !ok || !l.available() {
		return userErr("商品不存在或已下架")
	}
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var item model.ShopCartItem
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND sku_id = ?", userID, skuID).First(&item).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		next := quantity
		if add {
			next += item.Quantity
		}
		if next > maxQuantity {
			next = maxQuantity
		}
		if next > l.Stock && next > item.Quantity {
			return userErr("库存不足，当前库存 %d", l.Stock)
		}
		if next == 0 {
			if item.ID == 0 {
				return nil
			}
			return tx.Delete(&item).Error
		}
		if item.ID == 0 {
			return tx.Clauses(clause.OnConflict{DoUpdates: clause.Assignments(map[string]any{"quantity": next})}).Create(&model.ShopCartItem{UserID: userID, SKUID: skuID, Quantity: next}).Error
		}
		return tx.Model(&item).Update("quantity", next).Error
	})
}

// ---- Addresses ----

const maxAddresses = 20

func (s *Service) Addresses(userID uint64) ([]model.ShopAddress, error) {
	rows := []model.ShopAddress{}
	err := s.st.DB.Where("user_id = ?", userID).Order("is_default desc, id desc").Find(&rows).Error
	return rows, err
}

func (s *Service) SaveAddress(userID uint64, a model.ShopAddress) (uint64, error) {
	a.Name, a.Phone, a.Address = strings.TrimSpace(a.Name), strings.TrimSpace(a.Phone), strings.TrimSpace(a.Address)
	if err := checkLen(a.Name, "收货人", 64, true); err != nil {
		return 0, err
	}
	if err := checkLen(a.Phone, "联系电话", 32, true); err != nil {
		return 0, err
	}
	if err := checkLen(a.Address, "详细地址", 512, true); err != nil {
		return 0, err
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.ShopAddress{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
			return err
		}
		if a.ID == 0 && count >= maxAddresses {
			return userErr("最多保存 %d 个地址", maxAddresses)
		}
		if count == 0 {
			a.IsDefault = true
		}
		if a.IsDefault {
			if err := tx.Model(&model.ShopAddress{}).Where("user_id = ?", userID).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		if a.ID == 0 {
			a.UserID = userID
			return tx.Create(&a).Error
		}
		res := tx.Model(&model.ShopAddress{}).Where("id = ? AND user_id = ?", a.ID, userID).Updates(map[string]any{"name": a.Name, "phone": a.Phone, "address": a.Address, "is_default": a.IsDefault})
		if res.Error == nil && res.RowsAffected == 0 {
			return userErr("地址不存在")
		}
		return res.Error
	})
	return a.ID, err
}

func (s *Service) DeleteAddress(userID, id uint64) error {
	return s.st.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.ShopAddress{}).Error
}
