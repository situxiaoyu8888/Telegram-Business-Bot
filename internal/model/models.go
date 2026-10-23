package model

import "time"

type TelegramUser struct {
	ID             uint64    `gorm:"primaryKey" json:"id"`
	TelegramUserID int64     `gorm:"uniqueIndex;not null" json:"telegram_user_id"`
	Username       string    `gorm:"size:128" json:"username"`
	FirstName      string    `gorm:"size:128" json:"first_name"`
	LastName       string    `gorm:"size:128" json:"last_name"`
	LanguageCode   string    `gorm:"size:16" json:"language_code"`
	ReferralCode   string    `gorm:"size:32;uniqueIndex" json:"referral_code"`
	ParentUserID   *uint64   `gorm:"index" json:"parent_user_id"`
	Status         int8      `gorm:"not null;default:1" json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type WalletAccount struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	UserID    uint64    `gorm:"uniqueIndex:uk_user_currency;not null" json:"user_id"`
	Currency  string    `gorm:"size:16;uniqueIndex:uk_user_currency;not null" json:"currency"`
	Available string    `gorm:"type:decimal(24,6);not null;default:0" json:"available"`
	Frozen    string    `gorm:"type:decimal(24,6);not null;default:0" json:"frozen"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type WalletTransaction struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	UserID    uint64    `gorm:"index;not null" json:"user_id"`
	Currency  string    `gorm:"size:16;index;not null" json:"currency"`
	Type      string    `gorm:"size:32;index;not null" json:"type"`
	Amount    string    `gorm:"type:decimal(24,6);not null" json:"amount"`
	Balance   string    `gorm:"type:decimal(24,6);not null" json:"balance"`
	RefType   string    `gorm:"size:32;index" json:"ref_type"`
	RefID     uint64    `gorm:"index" json:"ref_id"`
	Remark    string    `gorm:"size:255" json:"remark"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

type DepositAddress struct {
	ScanTimestamp int64     `gorm:"not null;default:0" json:"scan_timestamp"`
	ID            uint64    `gorm:"primaryKey" json:"id"`
	UserID        uint64    `gorm:"uniqueIndex:uk_user_network;not null" json:"user_id"`
	Network       string    `gorm:"size:16;uniqueIndex:uk_user_network;not null" json:"network"`
	Address       string    `gorm:"size:128;uniqueIndex;not null" json:"address"`
	Status        int8      `gorm:"default:1;index" json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type DepositOrder struct {
	ID             uint64    `gorm:"primaryKey" json:"id"`
	UserID         uint64    `gorm:"index;not null" json:"user_id"`
	Network        string    `gorm:"size:16;index;not null" json:"network"`
	Currency       string    `gorm:"size:16;index;not null" json:"currency"`
	Address        string    `gorm:"size:128;index;not null" json:"address"`
	FromAddress    string    `gorm:"size:128" json:"from_address"`
	Amount         string    `gorm:"type:decimal(24,6);not null" json:"amount"`
	TxID           string    `gorm:"size:128;uniqueIndex;not null" json:"txid"`
	BlockTimestamp int64     `gorm:"index" json:"block_timestamp"`
	Status         string    `gorm:"size:24;index;not null" json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type WithdrawalOrder struct {
	RequestID  *string    `gorm:"size:64;uniqueIndex" json:"request_id,omitempty"`
	ID         uint64     `gorm:"primaryKey" json:"id"`
	UserID     uint64     `gorm:"index;not null" json:"user_id"`
	Network    string     `gorm:"size:16;index;not null" json:"network"`
	Currency   string     `gorm:"size:16;index;not null" json:"currency"`
	Address    string     `gorm:"size:128;not null" json:"address"`
	Amount     string     `gorm:"type:decimal(24,6);not null" json:"amount"`
	Fee        string     `gorm:"type:decimal(24,6);not null" json:"fee"`
	Actual     string     `gorm:"type:decimal(24,6);not null" json:"actual"`
	Status     string     `gorm:"size:24;index;not null" json:"status"`
	TxID       string     `gorm:"size:128;index" json:"txid"`
	ReviewNote string     `gorm:"size:255" json:"review_note"`
	ReviewedBy string     `gorm:"size:128" json:"reviewed_by"`
	ReviewedAt *time.Time `json:"reviewed_at"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

type ReferralCommission struct {
	ID          uint64    `gorm:"primaryKey" json:"id"`
	AgentUserID uint64    `gorm:"index;not null" json:"agent_user_id"`
	FromUserID  uint64    `gorm:"index;not null" json:"from_user_id"`
	SourceType  string    `gorm:"size:32;index;not null" json:"source_type"`
	SourceID    uint64    `gorm:"index;not null" json:"source_id"`
	Rate        string    `gorm:"type:decimal(10,6);not null" json:"rate"`
	Amount      string    `gorm:"type:decimal(24,6);not null" json:"amount"`
	Status      string    `gorm:"size:16;index;not null" json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

type SupportTicket struct {
	BotID          uint64     `gorm:"not null;default:0;index" json:"bot_id"`
	DeliveryStatus string     `gorm:"size:24;not null;default:pending" json:"delivery_status"`
	DeliveryError  string     `gorm:"type:text" json:"delivery_error"`
	ID             uint64     `gorm:"primaryKey" json:"id"`
	UserID         uint64     `gorm:"index;not null" json:"user_id"`
	TelegramUserID int64      `gorm:"index;not null" json:"telegram_user_id"`
	Subject        string     `gorm:"size:128;not null" json:"subject"`
	Content        string     `gorm:"type:text;not null" json:"content"`
	Status         string     `gorm:"size:24;index;not null" json:"status"`
	AdminReply     string     `gorm:"type:text" json:"admin_reply"`
	RepliedBy      string     `gorm:"size:128" json:"replied_by"`
	RepliedAt      *time.Time `json:"replied_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type MenuItem struct {
	ImageURL    string    `gorm:"size:1024" json:"image_url"`
	ButtonText  string    `gorm:"size:64" json:"button_text"`
	ReplyTitle  *string   `gorm:"size:64;uniqueIndex" json:"-"`
	Placement   string    `gorm:"size:16;not null;default:inline;index" json:"placement"`
	ID          uint64    `gorm:"primaryKey" json:"id"`
	ParentID    uint64    `gorm:"index;not null;default:0" json:"parent_id"`
	Code        string    `gorm:"size:64;uniqueIndex;not null" json:"code"`
	Title       string    `gorm:"size:64;not null" json:"title"`
	ActionType  string    `gorm:"size:24;not null" json:"action_type"`
	ActionValue string    `gorm:"size:255" json:"action_value"`
	Role        string    `gorm:"size:32;default:user" json:"role"`
	Sort        int       `gorm:"index;default:0" json:"sort"`
	RowNo       int       `gorm:"default:0" json:"row_no"`
	Enabled     bool      `gorm:"index" json:"enabled"`
	Description string    `gorm:"type:text" json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AdminUser is a web console account. Role is "super" (manages accounts), "admin" (pages from
// its AdminRole), "merchant" (its own merchant's shop pages) or "merchant_staff" (a subset of
// those pages, created by the merchant account).
type AdminUser struct {
	ID           uint64 `gorm:"primaryKey" json:"id"`
	Username     string `gorm:"size:64;uniqueIndex;not null" json:"username"`
	PasswordHash string `gorm:"size:100;not null" json:"-"`
	Role         string `gorm:"size:16;not null" json:"role"`
	RoleID       uint64 `gorm:"not null;default:0;index" json:"role_id"`     // AdminRole for role "admin"; ignored for "super"
	MerchantID   uint64 `gorm:"not null;default:0;index" json:"merchant_id"` // the merchant a "merchant" account manages
	// Permissions are the pages a merchant_staff account may use (comma-separated), chosen by its owner.
	Permissions string     `gorm:"type:text" json:"permissions"`
	Enabled     bool       `gorm:"not null" json:"enabled"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// AdminRole is a permission template: the console pages its accounts may use.
type AdminRole struct {
	ID          uint64    `gorm:"primaryKey" json:"id"`
	Name        string    `gorm:"size:64;uniqueIndex;not null" json:"name"`
	Permissions string    `gorm:"type:text" json:"permissions"` // comma-separated page keys
	Remark      string    `gorm:"size:255" json:"remark"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DefaultWelcomeText is sent when a user activates a bot for the first time.
const DefaultWelcomeText = "👋 欢迎使用！点击下方菜单即可开始。"

type SystemConfig struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Key       string    `gorm:"size:128;uniqueIndex;not null" json:"key"`
	Value     string    `gorm:"type:text" json:"value"`
	Remark    string    `gorm:"size:255" json:"remark"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedAt time.Time `json:"created_at"`
}

type ActionLog struct {
	ID             uint64    `gorm:"primaryKey" json:"id"`
	TelegramUserID int64     `gorm:"index;not null" json:"telegram_user_id"`
	Action         string    `gorm:"size:64;index;not null" json:"action"`
	Payload        string    `gorm:"type:text" json:"payload"`
	CreatedAt      time.Time `gorm:"index" json:"created_at"`
}

type PushCampaign struct {
	RepeatSeconds int64  `gorm:"not null;default:0" json:"repeat_seconds"`
	ParentID      uint64 `gorm:"not null;default:0;index" json:"parent_id"`
	LastError     string `gorm:"type:text" json:"last_error"`
	BotID         uint64 `gorm:"not null;default:0;index" json:"bot_id"`
	Category      string `gorm:"size:64;index" json:"category"`
	Personalize   bool   `gorm:"not null;default:false" json:"personalize"`
	PausedFrom    string `gorm:"size:24" json:"paused_from"`
	ID            uint64 `gorm:"primaryKey" json:"id"`

	// 后台任务标题，仅管理后台显示
	Title string `gorm:"size:128;not null" json:"title"`

	// text / photo / video
	MessageType string `gorm:"size:16;index;not null;default:text" json:"message_type"`

	// 文本消息正文；photo/video 时作为 caption 或补充文本
	Content string `gorm:"type:text;not null" json:"content"`

	// HTML
	ParseMode string `gorm:"size:16;default:HTML" json:"parse_mode"`

	// 图片/视频来源
	// MediaURL 和 MediaFileID 二选一
	MediaURL    string `gorm:"size:2048" json:"media_url"`
	MediaFileID string `gorm:"size:512" json:"media_file_id"`

	// auto:
	// 内容较短 -> media caption
	// 内容过长 -> 先发 media，再发独立文本
	//
	// caption:
	// 强制作为 caption
	//
	// separate:
	// 强制 media + 独立文本
	ContentMode string `gorm:"size:16;not null;default:auto" json:"content_mode"`

	ButtonText string `gorm:"size:64" json:"button_text"`
	ButtonURL  string `gorm:"size:1024" json:"button_url"`

	TargetType  string `gorm:"size:32;index;not null" json:"target_type"`
	TargetValue string `gorm:"type:text" json:"target_value"`

	Status string `gorm:"size:24;index;not null" json:"status"`

	TotalCount  int64 `gorm:"not null;default:0" json:"total_count"`
	SentCount   int64 `gorm:"not null;default:0" json:"sent_count"`
	FailedCount int64 `gorm:"not null;default:0" json:"failed_count"`

	ScheduledAt *time.Time `gorm:"index" json:"scheduled_at"`
	StartedAt   *time.Time `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at"`

	CreatedBy string `gorm:"size:128" json:"created_by"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

type PushRecipient struct {
	MediaSent      bool       `gorm:"not null;default:false" json:"media_sent"`
	ID             uint64     `gorm:"primaryKey" json:"id"`
	CampaignID     uint64     `gorm:"uniqueIndex:uk_campaign_user;index;not null" json:"campaign_id"`
	TelegramUserID int64      `gorm:"uniqueIndex:uk_campaign_user;index;not null" json:"telegram_user_id"`
	Status         string     `gorm:"size:24;index;not null" json:"status"`
	Attempts       int        `gorm:"not null;default:0" json:"attempts"`
	LastError      string     `gorm:"type:text" json:"last_error"`
	SentAt         *time.Time `json:"sent_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// A separate claim table protects new grants without rewriting historical commissions.
type ReferralGrant struct {
	DepositID uint64 `gorm:"primaryKey;autoIncrement:false"`
	CreatedAt time.Time
}
type SeedMigration struct {
	Name      string `gorm:"size:128;primaryKey"`
	CreatedAt time.Time
}

// Bot ID zero denotes the original environment-configured business bot.
type BusinessBot struct {
	ID             uint64     `gorm:"primaryKey" json:"id"`
	Name           string     `gorm:"size:128;not null" json:"name"`
	TelegramBotID  int64      `gorm:"uniqueIndex;not null" json:"telegram_bot_id"`
	Username       string     `gorm:"size:128" json:"username"`
	TokenEncrypted string     `gorm:"type:text;not null" json:"-"`
	Enabled        bool       `gorm:"index" json:"enabled"`
	Purpose        string     `gorm:"size:16;not null;default:full" json:"purpose"` // full, redpacket (red packets and wallet only) or news
	Offset         int        `gorm:"not null;default:0" json:"-"`
	LastError      string     `gorm:"size:255" json:"last_error"`
	LastSeenAt     *time.Time `json:"last_seen_at"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}
type BotSubscription struct {
	BotID          uint64    `gorm:"primaryKey;autoIncrement:false" json:"bot_id"`
	TelegramUserID int64     `gorm:"primaryKey;autoIncrement:false" json:"telegram_user_id"`
	CreatedAt      time.Time `json:"created_at"`
}
type PushRoute struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Category  string    `gorm:"size:64;uniqueIndex;not null" json:"category"`
	BotID     uint64    `gorm:"not null;default:0" json:"bot_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Merchant belongs to one child menu configured as a merchant directory.
type Merchant struct {
	Pinned      bool   `gorm:"not null;default:false;index" json:"pinned"`
	ID          uint64 `gorm:"primaryKey" json:"id"`
	MenuID      uint64 `gorm:"not null;index" json:"menu_id"`
	Name        string `gorm:"size:128;not null" json:"name"`
	Description string `gorm:"type:text" json:"description"`
	Address     string `gorm:"size:512" json:"address"`
	Hours       string `gorm:"size:128" json:"hours"`
	Phone       string `gorm:"size:64" json:"phone"`
	ContactURL  string `gorm:"size:1024" json:"contact_url"`
	// NotifyChatID receives shop order and after-sale notices from the primary bot (0 = none).
	NotifyChatID int64 `gorm:"not null;default:0" json:"notify_chat_id"`
	// CommissionRate is the platform's share of each completed shop order (0.05 = 5%).
	CommissionRate string `gorm:"type:decimal(10,6);not null;default:0" json:"commission_rate"`
	// PayoutAddress is the merchant's last TRC20 payout address, offered again on the next request.
	PayoutAddress string    `gorm:"size:64" json:"payout_address"`
	Sort          int       `gorm:"not null;default:0" json:"sort"`
	Enabled       bool      `gorm:"not null;index" json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// AdminAuditLog records a console write, a failed console request or a login attempt.
type AdminAuditLog struct {
	ID         uint64    `gorm:"primaryKey" json:"id"`
	AdminID    uint64    `gorm:"not null;default:0;index" json:"admin_id"`
	Username   string    `gorm:"size:64;index" json:"username"`
	Method     string    `gorm:"size:8;not null" json:"method"`
	Path       string    `gorm:"size:255;not null" json:"path"`
	Status     int       `gorm:"not null" json:"status"`
	IP         string    `gorm:"size:64" json:"ip"`
	DurationMS int64     `gorm:"not null" json:"duration_ms"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

// WalletCurrency is a currency users can hold. Rate is how many units of it one USDT buys; it
// drives exchanges between currencies. Code "USDT" is the TRC20 token the deposit watcher and the
// shop use; other codes are credited by the console (or by exchange).
type WalletCurrency struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	Code         string    `gorm:"size:16;uniqueIndex;not null" json:"code"`
	Name         string    `gorm:"size:32;not null" json:"name"`
	Icon         string    `gorm:"size:16;not null;default:''" json:"icon"`   // flag emoji for fiat; empty shows the kind's symbol
	Kind         string    `gorm:"size:16;not null;default:fiat" json:"kind"` // trc20, erc20 or fiat
	Rate         string    `gorm:"type:decimal(24,8);not null;default:1" json:"rate"`
	Decimals     int       `gorm:"not null" json:"decimals"` // zero values must be written, so no defaults
	MinWithdraw  string    `gorm:"type:decimal(24,6);not null;default:0" json:"min_withdraw"`
	WithdrawFee  string    `gorm:"type:decimal(24,6);not null;default:0" json:"withdraw_fee"`
	Exchangeable bool      `gorm:"not null" json:"exchangeable"`
	Withdrawable bool      `gorm:"not null" json:"withdrawable"`
	Enabled      bool      `gorm:"not null" json:"enabled"`
	Sort         int       `gorm:"not null" json:"sort"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// WalletTransfer is one completed transfer between two users' balances of one currency.
type WalletTransfer struct {
	ID         uint64    `gorm:"primaryKey" json:"id"`
	RequestID  string    `gorm:"size:64;uniqueIndex;not null" json:"-"`
	FromUserID uint64    `gorm:"index;not null" json:"from_user_id"`
	ToUserID   uint64    `gorm:"index;not null" json:"to_user_id"`
	Currency   string    `gorm:"size:16;not null" json:"currency"`
	Amount     string    `gorm:"type:decimal(24,6);not null" json:"amount"`
	Remark     string    `gorm:"size:128;not null;default:''" json:"remark"`
	CreatedAt  time.Time `gorm:"index" json:"created_at"`
}

// WalletExchange is one completed exchange between two of a user's balances.
type WalletExchange struct {
	ID           uint64    `gorm:"primaryKey" json:"id"`
	UserID       uint64    `gorm:"index;not null" json:"user_id"`
	RequestID    string    `gorm:"size:64;uniqueIndex;not null" json:"-"`
	FromCurrency string    `gorm:"size:16;not null" json:"from_currency"`
	ToCurrency   string    `gorm:"size:16;not null" json:"to_currency"`
	Amount       string    `gorm:"type:decimal(24,6);not null" json:"amount"`
	Received     string    `gorm:"type:decimal(24,6);not null" json:"received"`
	Rate         string    `gorm:"type:decimal(30,12);not null" json:"rate"` // units of ToCurrency per FromCurrency, after the fee
	FeeRate      string    `gorm:"type:decimal(10,6);not null" json:"fee_rate"`
	CreatedAt    time.Time `gorm:"index" json:"created_at"`
}

// RedPacket is money a user puts up for others to claim, shared in a group or through inline
// mode. Shares are fixed at creation (JSON amounts, taken in claim order); whatever is left when
// it expires goes back to the sender.
type RedPacket struct {
	ID               uint64     `gorm:"primaryKey" json:"id"`
	Code             string     `gorm:"size:24;uniqueIndex;not null" json:"code"`
	RequestID        string     `gorm:"size:64;uniqueIndex;not null" json:"-"`
	BotID            uint64     `gorm:"not null;index" json:"bot_id"`
	SenderUserID     uint64     `gorm:"not null;index" json:"sender_user_id"`
	SenderTelegramID int64      `gorm:"not null" json:"sender_telegram_id"`
	SenderName       string     `gorm:"size:128;not null" json:"sender_name"`
	Type             string     `gorm:"size:16;not null" json:"type"` // lucky, equal, exclusive or password
	Currency         string     `gorm:"size:16;not null" json:"currency"`
	Total            string     `gorm:"type:decimal(24,6);not null" json:"total"`
	Count            int        `gorm:"not null" json:"count"`
	ClaimedCount     int        `gorm:"not null" json:"claimed_count"`
	ClaimedAmount    string     `gorm:"type:decimal(24,6);not null" json:"claimed_amount"`
	Shares           string     `gorm:"type:text;not null" json:"-"`
	TargetUsername   string     `gorm:"size:64;not null;default:''" json:"target_username"`
	TargetTelegramID int64      `gorm:"not null;default:0" json:"target_telegram_id"`
	Password         string     `gorm:"size:64;not null;default:'';index" json:"password"`
	Greeting         string     `gorm:"size:128;not null;default:''" json:"greeting"`
	CoverID          uint64     `gorm:"not null;default:0" json:"cover_id"`
	Cover            string     `gorm:"size:512;not null;default:''" json:"cover"` // image URL at creation; the card is a photo when set
	CoverFileID      string     `gorm:"size:255;not null;default:''" json:"-"`     // Telegram file ID of the cover, once the owning bot has sent it
	ChatID           int64      `gorm:"not null;default:0;index" json:"chat_id"`   // group the bot posted it in (0 = shared via inline mode)
	MessageID        int        `gorm:"not null;default:0" json:"message_id"`
	Status           string     `gorm:"size:16;not null;index" json:"status"` // active, finished or expired
	RefundAmount     string     `gorm:"type:decimal(24,6);not null;default:0" json:"refund_amount"`
	ExpiresAt        time.Time  `gorm:"not null;index" json:"expires_at"`
	FinishedAt       *time.Time `json:"finished_at"`
	CreatedAt        time.Time  `gorm:"index" json:"created_at"`
}

type RedPacketClaim struct {
	ID             uint64    `gorm:"primaryKey" json:"id"`
	PacketID       uint64    `gorm:"not null;uniqueIndex:uk_packet_user" json:"packet_id"`
	UserID         uint64    `gorm:"not null;uniqueIndex:uk_packet_user;index" json:"user_id"`
	TelegramUserID int64     `gorm:"not null" json:"telegram_user_id"`
	Name           string    `gorm:"size:128;not null" json:"name"`
	Amount         string    `gorm:"type:decimal(24,6);not null" json:"amount"`
	CreatedAt      time.Time `json:"created_at"`
}

// RedPacketCover is a cover image the console offers for red packets; senders pick one in the wizard.
type RedPacketCover struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:32;not null" json:"name"`
	ImageURL  string    `gorm:"size:512;not null" json:"image_url"`
	Sort      int       `gorm:"not null" json:"sort"`
	Enabled   bool      `gorm:"not null" json:"enabled"`
	IsDefault bool      `gorm:"not null" json:"is_default"`
	CreatedAt time.Time `json:"created_at"`
}

// NewsCategory groups news articles; users subscribe per category.
type NewsCategory struct {
	ID        uint64    `gorm:"primaryKey" json:"id"`
	Name      string    `gorm:"size:32;not null" json:"name"`
	Sort      int       `gorm:"not null" json:"sort"`
	Enabled   bool      `gorm:"not null" json:"enabled"`
	CreatedAt time.Time `json:"created_at"`
}

// NewsArticle is one news item, written in the console, fetched from an RSS feed or copied from
// a channel. Published articles are delivered once (DispatchedAt) to subscribers and chats.
type NewsArticle struct {
	ID           uint64     `gorm:"primaryKey" json:"id"`
	CategoryID   uint64     `gorm:"not null;index" json:"category_id"`
	Title        string     `gorm:"size:255;not null" json:"title"`
	Content      string     `gorm:"type:text;not null" json:"content"`
	ImageURL     string     `gorm:"size:512;not null;default:''" json:"image_url"`
	LinkURL      string     `gorm:"size:1024;not null;default:''" json:"link_url"`
	Source       string     `gorm:"size:16;not null;index" json:"source"` // manual, rss or channel
	SourceID     uint64     `gorm:"not null;default:0" json:"source_id"`  // news_feeds.id or news_chats.id
	ExternalKey  *string    `gorm:"size:191;uniqueIndex" json:"-"`        // feed item GUID or channel post, for de-duplication
	ChannelChat  int64      `gorm:"not null;default:0" json:"channel_chat"`
	ChannelMsgID int        `gorm:"not null;default:0" json:"channel_msg_id"`
	ChannelBotID uint64     `gorm:"not null;default:0" json:"channel_bot_id"`
	Status       string     `gorm:"size:16;not null;index" json:"status"` // draft, pending, scheduled, published
	PublishAt    *time.Time `gorm:"index" json:"publish_at"`
	PublishedAt  *time.Time `gorm:"index" json:"published_at"`
	DispatchedAt *time.Time `gorm:"index" json:"dispatched_at"`
	Push         bool       `gorm:"not null" json:"push"` // deliver to subscribers and chats when published
	CreatedBy    string     `gorm:"size:64;not null;default:''" json:"created_by"`
	CreatedAt    time.Time  `gorm:"index" json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// NewsFeed is an RSS or Atom feed fetched periodically.
type NewsFeed struct {
	ID            uint64     `gorm:"primaryKey" json:"id"`
	Name          string     `gorm:"size:64;not null" json:"name"`
	URL           string     `gorm:"size:1024;not null" json:"url"`
	CategoryID    uint64     `gorm:"not null" json:"category_id"`
	AutoPublish   bool       `gorm:"not null" json:"auto_publish"`
	Interval      int        `gorm:"not null" json:"interval"` // minutes
	Enabled       bool       `gorm:"not null" json:"enabled"`
	LastFetchedAt *time.Time `json:"last_fetched_at"`
	LastError     string     `gorm:"size:255;not null;default:''" json:"last_error"`
	CreatedAt     time.Time  `json:"created_at"`
}

// NewsChat is a group or channel a news bot was added to. It can receive pushed news and, for
// channels, be a source whose posts become articles.
type NewsChat struct {
	ID               uint64    `gorm:"primaryKey" json:"id"`
	BotID            uint64    `gorm:"not null;uniqueIndex:uk_news_chat" json:"bot_id"`
	ChatID           int64     `gorm:"not null;uniqueIndex:uk_news_chat" json:"chat_id"`
	Type             string    `gorm:"size:16;not null" json:"type"` // group, supergroup or channel
	Title            string    `gorm:"size:255;not null;default:''" json:"title"`
	Username         string    `gorm:"size:64;not null;default:''" json:"username"`
	Status           string    `gorm:"size:16;not null" json:"status"` // the bot's membership: member, administrator, left
	PushEnabled      bool      `gorm:"not null" json:"push_enabled"`
	PushCategories   string    `gorm:"size:255;not null;default:''" json:"push_categories"` // comma-separated IDs; empty = all
	SourceEnabled    bool      `gorm:"not null" json:"source_enabled"`
	SourceCategoryID uint64    `gorm:"not null;default:0" json:"source_category_id"`
	AutoPublish      bool      `gorm:"not null" json:"auto_publish"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// NewsSubscription is a user's subscription to a category (0 = every category) on a news bot.
type NewsSubscription struct {
	ID             uint64    `gorm:"primaryKey" json:"id"`
	BotID          uint64    `gorm:"not null;uniqueIndex:uk_news_sub" json:"bot_id"`
	TelegramUserID int64     `gorm:"not null;uniqueIndex:uk_news_sub;index" json:"telegram_user_id"`
	CategoryID     uint64    `gorm:"not null;uniqueIndex:uk_news_sub" json:"category_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// NewsDelivery is one article to one chat through one bot.
type NewsDelivery struct {
	ID        uint64     `gorm:"primaryKey" json:"id"`
	ArticleID uint64     `gorm:"not null;uniqueIndex:uk_news_delivery" json:"article_id"`
	BotID     uint64     `gorm:"not null;uniqueIndex:uk_news_delivery;index:idx_news_delivery_pending,priority:2" json:"bot_id"`
	ChatID    int64      `gorm:"not null;uniqueIndex:uk_news_delivery" json:"chat_id"`
	Status    string     `gorm:"size:16;not null;index:idx_news_delivery_pending,priority:1" json:"status"` // pending, sent, failed
	Error     string     `gorm:"size:255;not null;default:''" json:"error"`
	SentAt    *time.Time `json:"sent_at"`
	CreatedAt time.Time  `json:"created_at"`
}

// RedPacketMessage is a copy of a packet card sent through inline mode, remembered when someone
// presses its button so every copy can be updated once the packet is finished or expires.
type RedPacketMessage struct {
	ID              uint64    `gorm:"primaryKey" json:"id"`
	PacketID        uint64    `gorm:"not null;index" json:"packet_id"`
	InlineMessageID string    `gorm:"size:191;not null;uniqueIndex" json:"inline_message_id"`
	CreatedAt       time.Time `json:"created_at"`
}

// PaymentPassword is a user's 6-digit payment password (bcrypt). Wrong attempts are counted
// and lock it for a while.
type PaymentPassword struct {
	UserID      uint64     `gorm:"primaryKey;autoIncrement:false" json:"user_id"`
	Hash        string     `gorm:"size:100;not null" json:"-"`
	Failed      int        `gorm:"not null" json:"failed"`
	LockedUntil *time.Time `json:"locked_until"`
	UpdatedAt   time.Time  `json:"updated_at"`
}
