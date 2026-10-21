package model

import "time"

// Shop amounts are USDT strings with 6 decimal places, like the wallet ledger.

// ShopCategory groups products on the H5 shop home page.
type ShopCategory struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:64;not null" json:"name"`
	IconURL   string    `gorm:"size:1024" json:"icon_url"`
	Sort      int       `gorm:"not null;default:0;index" json:"sort"`
	Enabled   bool      `gorm:"not null;index" json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ShopProduct is sold by a Merchant. Physical products allow express and/or pickup;
// virtual products are fulfilled with codes (ShopVirtualCode) or a manual note.
type ShopProduct struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	MerchantID   uint64    `gorm:"not null;index" json:"merchant_id"`
	CategoryID   uint64    `gorm:"not null;index" json:"category_id"`
	Name         string    `gorm:"size:128;not null" json:"name"`
	Description  string    `gorm:"type:text" json:"description"`
	Images       string    `gorm:"type:text" json:"images"`                       // newline-separated URLs; the first is the cover
	Kind         string    `gorm:"size:16;not null;default:physical" json:"kind"` // physical | virtual
	AllowExpress bool      `gorm:"not null;default:false" json:"allow_express"`
	AllowPickup  bool      `gorm:"not null;default:false" json:"allow_pickup"`
	Sales        int64     `gorm:"not null;default:0" json:"sales"`
	Sort         int       `gorm:"not null;default:0;index" json:"sort"`
	Enabled      bool      `gorm:"not null;index" json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ShopSKU is a purchasable variant of a product; every product has at least one.
type ShopSKU struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	ProductID uint64    `gorm:"not null;index" json:"product_id"`
	Name      string    `gorm:"size:64;not null" json:"name"`
	Price     string    `gorm:"type:decimal(24,6);not null" json:"price"`
	Stock     int64     `gorm:"not null;default:0" json:"stock"`
	Sort      int       `gorm:"not null;default:0" json:"sort"`
	Enabled   bool      `gorm:"not null" json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ShopVirtualCode is one pre-loaded code for a virtual SKU, assigned to an order on payment.
type ShopVirtualCode struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	SKUID     uint64    `gorm:"column:sku_id;not null;index:idx_sku_order" json:"sku_id"`
	Code      string    `gorm:"type:text;not null" json:"code"`
	OrderID   uint64    `gorm:"not null;default:0;index:idx_sku_order" json:"order_id"`
	CreatedAt time.Time `json:"created_at"`
}

type ShopCartItem struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	UserID    uint64    `gorm:"not null;uniqueIndex:uk_cart_user_sku" json:"user_id"`
	SKUID     uint64    `gorm:"column:sku_id;not null;uniqueIndex:uk_cart_user_sku" json:"sku_id"`
	Quantity  int64     `gorm:"not null" json:"quantity"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ShopAddress struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	UserID    uint64    `gorm:"not null;index" json:"user_id"`
	Name      string    `gorm:"size:64;not null" json:"name"`
	Phone     string    `gorm:"size:32;not null" json:"phone"`
	Address   string    `gorm:"size:512;not null" json:"address"`
	IsDefault bool      `gorm:"not null;default:false" json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ShopOrder covers one merchant and one delivery type. It is paid from the wallet when created,
// so it starts as "paid"; see internal/shop for the status flow.
type ShopOrder struct {
	ID             uint64     `gorm:"primaryKey" json:"id"`
	OrderNo        string     `gorm:"size:32;uniqueIndex;not null" json:"order_no"`
	RequestID      string     `gorm:"size:64;uniqueIndex:uk_order_user_request;not null" json:"-"`
	UserID         uint64     `gorm:"not null;uniqueIndex:uk_order_user_request;index" json:"user_id"`
	TelegramUserID int64      `gorm:"not null;index" json:"telegram_user_id"`
	BotID          uint64     `gorm:"not null;default:0" json:"bot_id"` // bot the Mini App was opened from; used for notifications
	MerchantID     uint64     `gorm:"not null;index" json:"merchant_id"`
	MerchantName   string     `gorm:"size:128" json:"merchant_name"`
	DeliveryType   string     `gorm:"size:16;not null" json:"delivery_type"` // express | pickup | virtual
	Status         string     `gorm:"size:16;not null;index" json:"status"`
	GoodsAmount    string     `gorm:"type:decimal(24,6);not null" json:"goods_amount"`
	Discount       string     `gorm:"type:decimal(24,6);not null" json:"discount"`
	PayAmount      string     `gorm:"type:decimal(24,6);not null" json:"pay_amount"`
	UserCouponID   uint64     `gorm:"not null;default:0" json:"user_coupon_id"`
	ContactName    string     `gorm:"size:64" json:"contact_name"`
	ContactPhone   string     `gorm:"size:32" json:"contact_phone"`
	Address        string     `gorm:"size:512" json:"address"`
	Remark         string     `gorm:"size:255" json:"remark"`
	Carrier        string     `gorm:"size:64" json:"carrier"`
	TrackingNo     string     `gorm:"size:128" json:"tracking_no"`
	DeliveryNote   string     `gorm:"type:text" json:"delivery_note"` // virtual codes or pickup instructions shown to the buyer
	CancelReason   string     `gorm:"size:255" json:"cancel_reason"`
	HandledBy      string     `gorm:"size:128" json:"handled_by"`
	PaidAt         time.Time  `json:"paid_at"`
	ShippedAt      *time.Time `json:"shipped_at"`
	CompletedAt    *time.Time `json:"completed_at"`
	CanceledAt     *time.Time `json:"canceled_at"`
	CreatedAt      time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ShopOrderItem snapshots the product at purchase time.
type ShopOrderItem struct {
	ID          uint64 `gorm:"primaryKey" json:"id"`
	OrderID     uint64 `gorm:"not null;index" json:"order_id"`
	ProductID   uint64 `gorm:"not null" json:"product_id"`
	SKUID       uint64 `gorm:"column:sku_id;not null" json:"sku_id"`
	ProductName string `gorm:"size:128;not null" json:"product_name"`
	SKUName     string `gorm:"size:64;not null" json:"sku_name"`
	ImageURL    string `gorm:"size:1024" json:"image_url"`
	Price       string `gorm:"type:decimal(24,6);not null" json:"price"`
	Quantity    int64  `gorm:"not null" json:"quantity"`
}

// ShopCoupon is a "spend Threshold, save Discount" template. MerchantID 0 applies to every merchant.
type ShopCoupon struct {
	ID           uint64     `gorm:"primaryKey" json:"id"`
	Name         string     `gorm:"size:64;not null" json:"name"`
	MerchantID   uint64     `gorm:"not null;default:0" json:"merchant_id"`
	Threshold    string     `gorm:"type:decimal(24,6);not null" json:"threshold"`
	Discount     string     `gorm:"type:decimal(24,6);not null" json:"discount"`
	TotalQty     int64      `gorm:"not null;default:0" json:"total_qty"` // 0 = unlimited
	IssuedQty    int64      `gorm:"not null;default:0" json:"issued_qty"`
	PerUserLimit int64      `gorm:"not null;default:1" json:"per_user_limit"`
	ValidDays    int        `gorm:"not null;default:0" json:"valid_days"` // days after claim; 0 uses EndsAt only
	EndsAt       *time.Time `json:"ends_at"`
	Claimable    bool       `gorm:"not null" json:"claimable"` // listed in the H5 coupon center
	Enabled      bool       `gorm:"not null;index" json:"enabled"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ShopUserCoupon is a coupon held by a user; Status is unused, used or void.
type ShopUserCoupon struct {
	ID        uint64     `gorm:"primaryKey" json:"id"`
	CouponID  uint64     `gorm:"not null;index" json:"coupon_id"`
	UserID    uint64     `gorm:"not null;index" json:"user_id"`
	Status    string     `gorm:"size:16;not null;index" json:"status"`
	OrderID   uint64     `gorm:"not null;default:0" json:"order_id"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}
