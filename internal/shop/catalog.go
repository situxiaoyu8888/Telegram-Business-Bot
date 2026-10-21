package shop

import (
	"errors"
	"strings"
	"time"

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
	ListPrice    string `json:"list_price"` // regular price of the cheapest SKU, shown struck through during a promo
	OnPromo      bool   `json:"on_promo"`
	InStock      bool   `json:"in_stock"`
	Sales        int64  `json:"sales"`
	Kind         string `json:"kind"`
	MerchantID   uint64 `json:"merchant_id"`
	MerchantName string `json:"merchant_name"`
}

// SKUView is an SKU as sold right now: Price is the effective price, ListPrice the regular one.
type SKUView struct {
	ID        uint64 `json:"id"`
	Name      string `json:"name"`
	Price     string `json:"price"`
	ListPrice string `json:"list_price"`
	OnPromo   bool   `json:"on_promo"`
	Stock     int64  `json:"stock"`
	PromoLeft *int64 `json:"promo_left,omitempty"` // units left at the promo price, when limited
}

// ProductDetail adds the description, images, delivery options, enabled SKUs and rating summary.
type ProductDetail struct {
	ProductCard
	Description     string     `json:"description"`
	Images          []string   `json:"images"`
	AllowExpress    bool       `json:"allow_express"`
	AllowPickup     bool       `json:"allow_pickup"`
	SKUs            []SKUView  `json:"skus"`
	PromoEndsAt     *time.Time `json:"promo_ends_at,omitempty"`
	Rating          float64    `json:"rating"`
	RatingCount     int64      `json:"rating_count"`
	LimitPerUser    int64      `json:"limit_per_user"`
	GroupSize       int        `json:"group_size"`
	GroupHours      int        `json:"group_hours"`
	GroupRate       string     `json:"group_rate"`
	Favorited       bool       `json:"favorited"` // set by the H5 handler for the signed-in buyer
	MerchantAddress string     `json:"merchant_address"`
	MerchantHours   string     `json:"merchant_hours"`
	MerchantPhone   string     `json:"merchant_phone"`
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
	Featured   bool
	OnPromo    bool
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
	if q.Featured {
		db = db.Where("shop_products.featured = ?", true)
	}
	if q.OnPromo {
		now := time.Now()
		db = db.Where("shop_products.promo_starts_at <= ? AND shop_products.promo_ends_at > ?", now, now)
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
		min, list decimal.Decimal
		has       bool
		anyPromo  bool // some SKU sells at a promo price, even if it is not the cheapest
		inStock   bool
	}
	byID := map[uint64]model.ShopProduct{}
	for _, p := range products {
		byID[p.ID] = p
	}
	now := time.Now()
	byProduct := map[uint64]*agg{}
	for _, k := range skus {
		a := byProduct[k.ProductID]
		if a == nil {
			a = &agg{}
			byProduct[k.ProductID] = a
		}
		p := byID[k.ProductID]
		current, onPromo := effectivePrice(k.Price, k.PromoPrice, k.PromoStock, p.PromoStartsAt, p.PromoEndsAt, now)
		price := amount(current)
		if !a.has || price.LessThan(a.min) {
			a.min, a.list, a.has = price, amount(k.Price), true
		}
		a.anyPromo = a.anyPromo || onPromo
		if k.Stock > 0 {
			a.inStock = true
		}
	}
	for _, p := range products {
		a := byProduct[p.ID]
		if a == nil {
			continue
		}
		out = append(out, ProductCard{ID: p.ID, Name: p.Name, Cover: firstImage(p.Images), MinPrice: a.min.StringFixed(6), ListPrice: a.list.StringFixed(6), OnPromo: a.anyPromo, InStock: a.inStock, Sales: p.Sales, Kind: p.Kind, MerchantID: p.MerchantID, MerchantName: names[p.MerchantID]})
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
	now := time.Now()
	views := make([]SKUView, 0, len(skus))
	anyPromo := false
	for _, k := range skus {
		current, onPromo := effectivePrice(k.Price, k.PromoPrice, k.PromoStock, p.PromoStartsAt, p.PromoEndsAt, now)
		anyPromo = anyPromo || onPromo
		v := SKUView{ID: k.ID, Name: k.Name, Price: current, ListPrice: k.Price, OnPromo: onPromo, Stock: k.Stock}
		if onPromo {
			v.PromoLeft = k.PromoStock
		}
		views = append(views, v)
	}
	var rating struct {
		Avg float64
		N   int64
	}
	if err := s.st.DB.Model(&model.ShopReview{}).Select("COALESCE(AVG(rating), 0) AS avg, COUNT(*) AS n").Where("product_id = ? AND hidden = ?", id, false).Scan(&rating).Error; err != nil {
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
	d := &ProductDetail{ProductCard: cards[0], LimitPerUser: p.LimitPerUser, GroupSize: p.GroupSize, GroupHours: p.GroupHours, GroupRate: p.GroupRate, Description: p.Description, Images: images, AllowExpress: p.AllowExpress, AllowPickup: p.AllowPickup, SKUs: views, Rating: rating.Avg, RatingCount: rating.N, MerchantAddress: m.Address, MerchantHours: m.Hours, MerchantPhone: m.Phone}
	if anyPromo {
		d.PromoEndsAt = p.PromoEndsAt
	}
	return d, nil
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
	Name       string `json:"name"`
	Price      string `json:"price"`
	PromoPrice string `json:"promo_price"` // optional; used only inside the product's promo window
	PromoStock *int64 `json:"promo_stock"` // optional cap on units sold at the promo price
	Stock      int64  `json:"stock"`
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
	hasPromo := false
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
		promo := decimal.Zero
		if strings.TrimSpace(skus[i].PromoPrice) != "" {
			if promo, err = parseAmount(skus[i].PromoPrice, "活动价"); err != nil {
				return 0, err
			}
			if !promo.IsZero() && !promo.LessThan(price) {
				return 0, userErr("规格「%s」的活动价必须低于原价", skus[i].Name)
			}
		}
		skus[i].PromoPrice = promo.StringFixed(6)
		hasPromo = hasPromo || promo.IsPositive()
		if skus[i].PromoStock != nil && (*skus[i].PromoStock < 0 || promo.IsZero()) {
			return 0, userErr("规格「%s」：活动库存需为非负数，且需同时填写活动价", skus[i].Name)
		}
		if skus[i].Stock < 0 {
			return 0, userErr("库存不能为负数")
		}
	}
	if (p.PromoStartsAt == nil) != (p.PromoEndsAt == nil) || (p.PromoStartsAt != nil && !p.PromoEndsAt.After(*p.PromoStartsAt)) {
		return 0, userErr("限时活动需同时设置开始和结束时间，且结束晚于开始")
	}
	if p.LimitPerUser < 0 {
		return 0, userErr("每人限购数量不能为负数")
	}
	if p.GroupSize != 0 {
		rate, err := decimal.NewFromString(strings.TrimSpace(p.GroupRate))
		if p.GroupSize < 2 || p.GroupSize > 100 || p.GroupHours < 1 || p.GroupHours > 720 || err != nil || !rate.IsPositive() || !rate.LessThan(decimal.NewFromInt(1)) {
			return 0, userErr("拼团需 2–100 人、1–720 小时，拼团价比例在 0 到 1 之间（如 0.8）")
		}
		if p.Kind == "virtual" {
			return 0, userErr("虚拟商品不支持拼团")
		}
		p.GroupRate = rate.StringFixed(6)
	} else {
		p.GroupHours, p.GroupRate = 24, "1"
	}
	if hasPromo && p.PromoStartsAt == nil {
		return 0, userErr("填写了活动价，请设置限时活动的开始和结束时间")
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
		fields := map[string]any{"merchant_id": p.MerchantID, "category_id": p.CategoryID, "name": p.Name, "description": p.Description, "images": strings.TrimSpace(p.Images), "kind": p.Kind, "allow_express": p.AllowExpress, "allow_pickup": p.AllowPickup, "sort": p.Sort, "enabled": p.Enabled, "featured": p.Featured, "promo_starts_at": p.PromoStartsAt, "promo_ends_at": p.PromoEndsAt, "limit_per_user": p.LimitPerUser, "group_size": p.GroupSize, "group_hours": p.GroupHours, "group_rate": p.GroupRate}
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
				if err := tx.Model(&model.ShopSKU{}).Where("id = ?", old.ID).Updates(map[string]any{"price": in.Price, "promo_price": in.PromoPrice, "promo_stock": in.PromoStock, "stock": in.Stock, "sort": i, "enabled": true}).Error; err != nil {
					return err
				}
				delete(byName, in.Name)
				continue
			}
			if err := tx.Create(&model.ShopSKU{ProductID: p.ID, Name: in.Name, Price: in.Price, PromoPrice: in.PromoPrice, PromoStock: in.PromoStock, Stock: in.Stock, Sort: i, Enabled: true}).Error; err != nil {
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
