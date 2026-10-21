package shop

import (
	"errors"
	"strings"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
)

const pageSize = 20

// ProductCard is a product as listed in the shop: cover, lowest price and whether anything is in stock.
type ProductCard struct {
	ID           uint64 `json:"id"`
	Name         string `json:"name"`
	Cover        string `json:"cover"`
	MinPrice     string `json:"min_price"`
	InStock      bool   `json:"in_stock"`
	Sales        int64  `json:"sales"`
	Kind         string `json:"kind"`
	MerchantID   uint64 `json:"merchant_id"`
	MerchantName string `json:"merchant_name"`
}

// ProductDetail adds the description, images, delivery options and enabled SKUs.
type ProductDetail struct {
	ProductCard
	Description     string          `json:"description"`
	Images          []string        `json:"images"`
	AllowExpress    bool            `json:"allow_express"`
	AllowPickup     bool            `json:"allow_pickup"`
	SKUs            []model.ShopSKU `json:"skus"`
	MerchantAddress string          `json:"merchant_address"`
	MerchantHours   string          `json:"merchant_hours"`
	MerchantPhone   string          `json:"merchant_phone"`
}

func (s *Service) Categories() ([]model.ShopCategory, error) {
	rows := []model.ShopCategory{}
	err := s.st.DB.Where("enabled=?", true).Order("sort asc, id asc").Find(&rows).Error
	return rows, err
}

// visibleProducts selects enabled products of enabled merchants.
func (s *Service) visibleProducts() *gorm.DB {
	return s.st.DB.Model(&model.ShopProduct{}).
		Joins("JOIN merchants ON merchants.id = shop_products.merchant_id AND merchants.enabled = ?", true).
		Where("shop_products.enabled = ?", true)
}

type ProductQuery struct {
	CategoryID uint64
	MerchantID uint64
	Keyword    string
	Page       int
}

func (s *Service) Products(q ProductQuery) ([]ProductCard, error) {
	db := s.visibleProducts()
	if q.CategoryID > 0 {
		db = db.Where("shop_products.category_id = ?", q.CategoryID)
	}
	if q.MerchantID > 0 {
		db = db.Where("shop_products.merchant_id = ?", q.MerchantID)
	}
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(kw) + "%"
		db = db.Where("shop_products.name LIKE ? OR merchants.name LIKE ?", like, like)
	}
	if q.Page < 1 {
		q.Page = 1
	}
	var products []model.ShopProduct
	if err := db.Order("shop_products.sort asc, shop_products.id desc").Offset((q.Page - 1) * pageSize).Limit(pageSize).Find(&products).Error; err != nil {
		return nil, err
	}
	return s.cards(products)
}

// cards attaches prices, stock and merchant names; products without an enabled SKU are left out.
func (s *Service) cards(products []model.ShopProduct) ([]ProductCard, error) {
	out := []ProductCard{}
	if len(products) == 0 {
		return out, nil
	}
	ids := make([]uint64, 0, len(products))
	merchantIDs := []uint64{}
	for _, p := range products {
		ids = append(ids, p.ID)
		merchantIDs = append(merchantIDs, p.MerchantID)
	}
	var skus []model.ShopSKU
	if err := s.st.DB.Where("product_id IN ? AND enabled = ?", ids, true).Find(&skus).Error; err != nil {
		return nil, err
	}
	var merchants []model.Merchant
	if err := s.st.DB.Where("id IN ?", merchantIDs).Find(&merchants).Error; err != nil {
		return nil, err
	}
	names := map[uint64]string{}
	for _, m := range merchants {
		names[m.ID] = m.Name
	}
	type agg struct {
		min     decimal.Decimal
		has     bool
		inStock bool
	}
	byProduct := map[uint64]*agg{}
	for _, k := range skus {
		a := byProduct[k.ProductID]
		if a == nil {
			a = &agg{}
			byProduct[k.ProductID] = a
		}
		price := amount(k.Price)
		if !a.has || price.LessThan(a.min) {
			a.min, a.has = price, true
		}
		if k.Stock > 0 {
			a.inStock = true
		}
	}
	for _, p := range products {
		a := byProduct[p.ID]
		if a == nil {
			continue
		}
		out = append(out, ProductCard{ID: p.ID, Name: p.Name, Cover: firstImage(p.Images), MinPrice: a.min.StringFixed(6), InStock: a.inStock, Sales: p.Sales, Kind: p.Kind, MerchantID: p.MerchantID, MerchantName: names[p.MerchantID]})
	}
	return out, nil
}

func (s *Service) Product(id uint64) (*ProductDetail, error) {
	var p model.ShopProduct
	if err := s.visibleProducts().Where("shop_products.id = ?", id).First(&p).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, userErr("商品不存在或已下架")
		}
		return nil, err
	}
	cards, err := s.cards([]model.ShopProduct{p})
	if err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return nil, userErr("商品不存在或已下架")
	}
	var skus []model.ShopSKU
	if err := s.st.DB.Where("product_id = ? AND enabled = ?", id, true).Order("sort asc, id asc").Find(&skus).Error; err != nil {
		return nil, err
	}
	var m model.Merchant
	if err := s.st.DB.First(&m, p.MerchantID).Error; err != nil {
		return nil, err
	}
	images := []string{}
	for _, line := range strings.Split(p.Images, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			images = append(images, line)
		}
	}
	return &ProductDetail{ProductCard: cards[0], Description: p.Description, Images: images, AllowExpress: p.AllowExpress, AllowPickup: p.AllowPickup, SKUs: skus, MerchantAddress: m.Address, MerchantHours: m.Hours, MerchantPhone: m.Phone}, nil
}

// ---- Administration ----

func (s *Service) AdminCategories() ([]model.ShopCategory, error) {
	rows := []model.ShopCategory{}
	err := s.st.DB.Order("sort asc, id asc").Find(&rows).Error
	return rows, err
}

func (s *Service) SaveCategory(c model.ShopCategory) error {
	c.Name = strings.TrimSpace(c.Name)
	if err := checkLen(c.Name, "分类名称", 64, true); err != nil {
		return err
	}
	if c.ID == 0 {
		return s.st.DB.Create(&c).Error
	}
	res := s.st.DB.Model(&model.ShopCategory{}).Where("id = ?", c.ID).Updates(map[string]any{"name": c.Name, "icon_url": strings.TrimSpace(c.IconURL), "sort": c.Sort, "enabled": c.Enabled})
	if res.Error == nil && res.RowsAffected == 0 {
		return userErr("分类不存在")
	}
	return res.Error
}

func (s *Service) DeleteCategory(id uint64) error {
	var n int64
	if err := s.st.DB.Model(&model.ShopProduct{}).Where("category_id = ?", id).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return userErr("该分类下还有 %d 个商品，请先调整商品分类", n)
	}
	return s.st.DB.Delete(&model.ShopCategory{}, id).Error
}

// AdminProduct is a product with all of its SKUs and the number of unused virtual codes per SKU.
type AdminProduct struct {
	model.ShopProduct
	SKUs  []model.ShopSKU  `json:"skus"`
	Codes map[uint64]int64 `json:"codes"`
}

// AdminProducts lists products for the console; merchantID limits it to one merchant (0 = all).
func (s *Service) AdminProducts(merchantID uint64) ([]AdminProduct, error) {
	var products []model.ShopProduct
	q := s.st.DB.Order("sort asc, id desc")
	if merchantID != 0 {
		q = q.Where("merchant_id = ?", merchantID)
	}
	if err := q.Find(&products).Error; err != nil {
		return nil, err
	}
	ids := []uint64{0}
	for _, p := range products {
		ids = append(ids, p.ID)
	}
	var skus []model.ShopSKU
	if err := s.st.DB.Where("product_id IN ?", ids).Order("sort asc, id asc").Find(&skus).Error; err != nil {
		return nil, err
	}
	var counts []struct {
		SKUID uint64 `gorm:"column:sku_id"`
		N     int64
	}
	if err := s.st.DB.Model(&model.ShopVirtualCode{}).Select("sku_id, count(*) as n").Where("order_id = 0").Group("sku_id").Scan(&counts).Error; err != nil {
		return nil, err
	}
	codes := map[uint64]int64{}
	for _, c := range counts {
		codes[c.SKUID] = c.N
	}
	bySKU := map[uint64][]model.ShopSKU{}
	for _, k := range skus {
		bySKU[k.ProductID] = append(bySKU[k.ProductID], k)
	}
	out := make([]AdminProduct, 0, len(products))
	for _, p := range products {
		ap := AdminProduct{ShopProduct: p, SKUs: bySKU[p.ID], Codes: map[uint64]int64{}}
		if ap.SKUs == nil {
			ap.SKUs = []model.ShopSKU{}
		}
		for _, k := range ap.SKUs {
			if n := codes[k.ID]; n > 0 {
				ap.Codes[k.ID] = n
			}
		}
		out = append(out, ap)
	}
	return out, nil
}

// SKUInput is one SKU row from the product form. SKUs are matched to existing ones by name,
// so carts and order history keep pointing at the same SKU; rows left out are disabled.
type SKUInput struct {
	Name  string `json:"name"`
	Price string `json:"price"`
	Stock int64  `json:"stock"`
}

// SaveProduct creates or updates a product with its SKUs. A non-zero merchantScope (a merchant
// account) pins the product to that merchant and refuses other merchants' products.
func (s *Service) SaveProduct(p model.ShopProduct, skus []SKUInput, merchantScope uint64) (uint64, error) {
	if merchantScope != 0 {
		p.MerchantID = merchantScope
	}
	p.Name = strings.TrimSpace(p.Name)
	if err := checkLen(p.Name, "商品名称", 128, true); err != nil {
		return 0, err
	}
	if p.Kind != "physical" && p.Kind != "virtual" {
		return 0, userErr("商品类型无效")
	}
	if p.Kind == "virtual" {
		p.AllowExpress, p.AllowPickup = false, false
	} else if !p.AllowExpress && !p.AllowPickup {
		return 0, userErr("实物商品至少选择一种配送方式：快递或自提")
	}
	if len(skus) == 0 {
		return 0, userErr("至少需要一个规格")
	}
	seen := map[string]bool{}
	for i := range skus {
		skus[i].Name = strings.TrimSpace(skus[i].Name)
		if err := checkLen(skus[i].Name, "规格名称", 64, true); err != nil {
			return 0, err
		}
		if seen[skus[i].Name] {
			return 0, userErr("规格名称重复：%s", skus[i].Name)
		}
		seen[skus[i].Name] = true
		price, err := parseAmount(skus[i].Price, "价格")
		if err != nil {
			return 0, err
		}
		if !price.IsPositive() {
			return 0, userErr("规格「%s」的价格必须大于 0", skus[i].Name)
		}
		skus[i].Price = price.StringFixed(6)
		if skus[i].Stock < 0 {
			return 0, userErr("库存不能为负数")
		}
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var m model.Merchant
		if err := tx.Select("id").First(&m, p.MerchantID).Error; err != nil {
			return userErr("请选择有效的商家")
		}
		var c model.ShopCategory
		if err := tx.Select("id").First(&c, p.CategoryID).Error; err != nil {
			return userErr("请选择有效的分类")
		}
		fields := map[string]any{"merchant_id": p.MerchantID, "category_id": p.CategoryID, "name": p.Name, "description": p.Description, "images": strings.TrimSpace(p.Images), "kind": p.Kind, "allow_express": p.AllowExpress, "allow_pickup": p.AllowPickup, "sort": p.Sort, "enabled": p.Enabled}
		if p.ID == 0 {
			p.Images = strings.TrimSpace(p.Images)
			if err := tx.Create(&p).Error; err != nil {
				return err
			}
		} else {
			var existing model.ShopProduct
			if err := tx.Select("id", "kind", "merchant_id").First(&existing, p.ID).Error; err != nil || (merchantScope != 0 && existing.MerchantID != merchantScope) {
				return userErr("商品不存在")
			}
			if existing.Kind != p.Kind {
				return userErr("已创建的商品不能修改商品类型")
			}
			if err := tx.Model(&model.ShopProduct{}).Where("id = ?", p.ID).Updates(fields).Error; err != nil {
				return err
			}
		}
		var current []model.ShopSKU
		if err := tx.Where("product_id = ?", p.ID).Find(&current).Error; err != nil {
			return err
		}
		byName := map[string]model.ShopSKU{}
		for _, k := range current {
			byName[k.Name] = k
		}
		for i, in := range skus {
			if old, ok := byName[in.Name]; ok {
				if err := tx.Model(&model.ShopSKU{}).Where("id = ?", old.ID).Updates(map[string]any{"price": in.Price, "stock": in.Stock, "sort": i, "enabled": true}).Error; err != nil {
					return err
				}
				delete(byName, in.Name)
				continue
			}
			if err := tx.Create(&model.ShopSKU{ProductID: p.ID, Name: in.Name, Price: in.Price, Stock: in.Stock, Sort: i, Enabled: true}).Error; err != nil {
				return err
			}
		}
		for _, gone := range byName {
			if err := tx.Model(&model.ShopSKU{}).Where("id = ?", gone.ID).Update("enabled", false).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return p.ID, err
}

// ImportCodes adds virtual codes (one per non-empty line) to a virtual SKU and raises its stock to match.
// A non-zero merchantScope only allows SKUs of that merchant's products.
func (s *Service) ImportCodes(skuID uint64, text string, merchantScope uint64) (int, error) {
	codes := []model.ShopVirtualCode{}
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 2000 {
				return 0, userErr("单条卡密不能超过 2000 个字符")
			}
			codes = append(codes, model.ShopVirtualCode{SKUID: skuID, Code: line})
		}
	}
	if len(codes) == 0 {
		return 0, userErr("请粘贴卡密，每行一条")
	}
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var sku model.ShopSKU
		if err := tx.First(&sku, skuID).Error; err != nil {
			return userErr("规格不存在")
		}
		var p model.ShopProduct
		if err := tx.Select("id", "kind", "merchant_id").First(&p, sku.ProductID).Error; err != nil {
			return err
		}
		if merchantScope != 0 && p.MerchantID != merchantScope {
			return userErr("规格不存在")
		}
		if p.Kind != "virtual" {
			return userErr("只有虚拟商品可以导入卡密")
		}
		if err := tx.CreateInBatches(&codes, 200).Error; err != nil {
			return err
		}
		return tx.Model(&model.ShopSKU{}).Where("id = ?", skuID).Update("stock", gorm.Expr("stock + ?", len(codes))).Error
	})
	return len(codes), err
}
