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
	ID           uint64 `gorm:"primaryKey" json:"id"`
	MerchantID   uint64 `gorm:"not null;index" json:"merchant_id"`
	CategoryID   uint64 `gorm:"not null;index" json:"category_id"`
	Name         string `gorm:"size:128;not null" json:"name"`
	Description  string `gorm:"type:text" json:"description"`
	Images       string `gorm:"type:text" json:"images"`                       // newline-separated URLs; the first is the cover
	Kind         string `gorm:"size:16;not null;default:physical" json:"kind"` // physical | virtual
	AllowExpress bool   `gorm:"not null;default:false" json:"allow_express"`
	AllowPickup  bool   `gorm:"not null;default:false" json:"allow_pickup"`
	Sales        int64  `gorm:"not null;default:0" json:"sales"`
	Featured     bool   `gorm:"not null;default:false;index" json:"featured"` // shown in the home page's recommended section
	// SKU promo prices apply only between PromoStartsAt and PromoEndsAt.
	PromoStartsAt *time.Time `json:"promo_starts_at"`
	PromoEndsAt   *time.Time `json:"promo_ends_at"`
	// LimitPerUser caps how many units one buyer may hold across non-canceled orders (0 = no limit).
	LimitPerUser int64     `gorm:"not null;default:0" json:"limit_per_user"`
	Sort         int       `gorm:"not null;default:0;index" json:"sort"`
	Enabled      bool      `gorm:"not null;index" json:"enabled"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ShopSKU is a purchasable variant of a product; every product has at least one.
type ShopSKU struct {
	ID         uint64 `gorm:"primaryKey" json:"id"`
	ProductID  uint64 `gorm:"not null;index" json:"product_id"`
	Name       string `gorm:"size:64;not null" json:"name"`
	Price      string `gorm:"type:decimal(24,6);not null" json:"price"`
	PromoPrice string `gorm:"type:decimal(24,6);not null;default:0" json:"promo_price"` // 0 = no promo price
	// PromoStock limits how many units sell at the promo price; nil means no separate limit.
	PromoStock *int64    `json:"promo_stock"`
	Stock      int64     `gorm:"not null;default:0" json:"stock"`
	Sort       int       `gorm:"not null;default:0" json:"sort"`
	Enabled    bool      `gorm:"not null" json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
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
	ID             uint64 `gorm:"primaryKey" json:"id"`
	OrderNo        string `gorm:"size:32;uniqueIndex;not null" json:"order_no"`
	RequestID      string `gorm:"size:64;uniqueIndex:uk_order_user_request;not null" json:"-"`
	UserID         uint64 `gorm:"not null;uniqueIndex:uk_order_user_request;index" json:"user_id"`
	TelegramUserID int64  `gorm:"not null;index" json:"telegram_user_id"`
	BotID          uint64 `gorm:"not null;default:0" json:"bot_id"` // bot the Mini App was opened from; used for notifications
	MerchantID     uint64 `gorm:"not null;index" json:"merchant_id"`
	MerchantName   string `gorm:"size:128" json:"merchant_name"`
	DeliveryType   string `gorm:"size:16;not null" json:"delivery_type"` // express | pickup | virtual
	Status         string `gorm:"size:16;not null;index" json:"status"`
	GoodsAmount    string `gorm:"type:decimal(24,6);not null" json:"goods_amount"`
	Discount       string `gorm:"type:decimal(24,6);not null" json:"discount"`
	PayAmount      string `gorm:"type:decimal(24,6);not null" json:"pay_amount"`
	RefundedAmount string `gorm:"type:decimal(24,6);not null;default:0" json:"refunded_amount"` // after-sale refunds so far
	AfterSaleOpen  bool   `gorm:"not null;default:false" json:"after_sale_open"`
	// CommissionDone marks that the buyer's referrer has been paid (or that none was due).
	CommissionDone bool       `gorm:"not null;default:false;index" json:"commission_done"`
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
	Promo       bool   `gorm:"not null;default:false" json:"promo"` // sold at the promo price (counts against promo stock)
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

// ShopAfterSale is a buyer's refund request on a shipped, ready or completed order.
// Type is refund (money only) or return (goods sent back first). Status flow:
// pending → approved (return only, waiting for the parcel) → refunded, or pending → rejected / canceled.
type ShopAfterSale struct {
	ID             uint64     `gorm:"primaryKey" json:"id"`
	OrderID        uint64     `gorm:"not null;index" json:"order_id"`
	OrderNo        string     `gorm:"size:32;not null" json:"order_no"`
	UserID         uint64     `gorm:"not null;index" json:"user_id"`
	TelegramUserID int64      `gorm:"not null" json:"telegram_user_id"`
	MerchantID     uint64     `gorm:"not null;index" json:"merchant_id"`
	Type           string     `gorm:"size:16;not null" json:"type"`
	Reason         string     `gorm:"size:512;not null" json:"reason"`
	Amount         string     `gorm:"type:decimal(24,6);not null" json:"amount"` // requested
	RefundAmount   string     `gorm:"type:decimal(24,6);not null;default:0" json:"refund_amount"`
	Status         string     `gorm:"size:16;not null;index" json:"status"`
	ReturnCarrier  string     `gorm:"size:64" json:"return_carrier"`
	ReturnTracking string     `gorm:"size:128" json:"return_tracking"`
	ReturnAddress  string     `gorm:"size:512" json:"return_address"`
	AdminNote      string     `gorm:"size:512" json:"admin_note"`
	HandledBy      string     `gorm:"size:128" json:"handled_by"`
	ClosedAt       *time.Time `json:"closed_at"`
	CreatedAt      time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// ShopBanner is a slide in the H5 home page carousel. LinkType is none, product, category or url.
type ShopBanner struct {
	ID        uint64     `gorm:"primaryKey" json:"id"`
	Title     string     `gorm:"size:64" json:"title"`
	ImageURL  string     `gorm:"size:1024;not null" json:"image_url"`
	LinkType  string     `gorm:"size:16;not null;default:none" json:"link_type"`
	LinkValue string     `gorm:"size:1024" json:"link_value"`
	Sort      int        `gorm:"not null;default:0" json:"sort"`
	Enabled   bool       `gorm:"not null;index" json:"enabled"`
	StartsAt  *time.Time `json:"starts_at"`
	EndsAt    *time.Time `json:"ends_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ShopReview is a buyer's rating of one purchased item from a completed order.
type ShopReview struct {
	ID          uint64     `gorm:"primaryKey" json:"id"`
	OrderItemID uint64     `gorm:"not null;uniqueIndex" json:"order_item_id"`
	OrderID     uint64     `gorm:"not null;index" json:"order_id"`
	ProductID   uint64     `gorm:"not null;index" json:"product_id"`
	MerchantID  uint64     `gorm:"not null;index" json:"merchant_id"`
	UserID      uint64     `gorm:"not null;index" json:"user_id"`
	BuyerName   string     `gorm:"size:64" json:"buyer_name"` // masked display name
	ProductName string     `gorm:"size:128" json:"product_name"`
	SKUName     string     `gorm:"size:64" json:"sku_name"`
	Rating      int        `gorm:"not null" json:"rating"` // 1–5
	Content     string     `gorm:"size:1000" json:"content"`
	Hidden      bool       `gorm:"not null;default:false;index" json:"hidden"`
	Reply       string     `gorm:"size:1000" json:"reply"`
	RepliedAt   *time.Time `json:"replied_at"`
	CreatedAt   time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ShopMerchantEntry is one line of a merchant's settlement ledger. A completed order adds a
// "sale" entry; a refund after completion adds a negative "refund" entry. EntryKey records each
// event once. SettlementID is 0 until the entry is included in a settlement.
type ShopMerchantEntry struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	EntryKey     string    `gorm:"size:64;uniqueIndex;not null" json:"entry_key"`
	MerchantID   uint64    `gorm:"not null;index:idx_merchant_settlement" json:"merchant_id"`
	SettlementID uint64    `gorm:"not null;default:0;index:idx_merchant_settlement" json:"settlement_id"`
	OrderID      uint64    `gorm:"not null;index" json:"order_id"`
	OrderNo      string    `gorm:"size:32" json:"order_no"`
	Type         string    `gorm:"size:16;not null" json:"type"`                  // sale | refund
	Amount       string    `gorm:"type:decimal(24,6);not null" json:"amount"`     // order money, negative for refunds
	Commission   string    `gorm:"type:decimal(24,6);not null" json:"commission"` // platform share, same sign
	Net          string    `gorm:"type:decimal(24,6);not null" json:"net"`        // Amount − Commission
	Rate         string    `gorm:"type:decimal(10,6);not null" json:"rate"`
	CreatedAt    time.Time `gorm:"index" json:"created_at"`
}

// ShopSettlement groups unsettled ledger entries into one payout to a merchant.
type ShopSettlement struct {
	ID         uint64     `gorm:"primaryKey" json:"id"`
	MerchantID uint64     `gorm:"not null;index" json:"merchant_id"`
	Amount     string     `gorm:"type:decimal(24,6);not null" json:"amount"`
	Commission string     `gorm:"type:decimal(24,6);not null" json:"commission"`
	Net        string     `gorm:"type:decimal(24,6);not null" json:"net"` // what the merchant is paid
	EntryCount int64      `gorm:"not null" json:"entry_count"`
	Status     string     `gorm:"size:16;not null;index" json:"status"` // pending | paid
	PayRef     string     `gorm:"size:255" json:"pay_ref"`
	Note       string     `gorm:"size:512" json:"note"`
	CreatedBy  string     `gorm:"size:128" json:"created_by"`
	PaidBy     string     `gorm:"size:128" json:"paid_by"`
	PaidAt     *time.Time `json:"paid_at"`
	CreatedAt  time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ShopFavorite is a product the buyer saved.
type ShopFavorite struct {
	UserID    uint64    `gorm:"primaryKey;autoIncrement:false" json:"user_id"`
	ProductID uint64    `gorm:"primaryKey;autoIncrement:false" json:"product_id"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// ShopView is the buyer's most recent visit to a product, for browsing history.
type ShopView struct {
	UserID    uint64    `gorm:"primaryKey;autoIncrement:false" json:"user_id"`
	ProductID uint64    `gorm:"primaryKey;autoIncrement:false" json:"product_id"`
	ViewedAt  time.Time `gorm:"index" json:"viewed_at"`
}
