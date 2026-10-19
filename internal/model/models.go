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
	Pinned      bool      `gorm:"not null;default:false;index" json:"pinned"`
	ID          uint64    `gorm:"primaryKey" json:"id"`
	MenuID      uint64    `gorm:"not null;index" json:"menu_id"`
	Name        string    `gorm:"size:128;not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	Address     string    `gorm:"size:512" json:"address"`
	Hours       string    `gorm:"size:128" json:"hours"`
	Phone       string    `gorm:"size:64" json:"phone"`
	ContactURL  string    `gorm:"size:1024" json:"contact_url"`
	Sort        int       `gorm:"not null;default:0" json:"sort"`
	Enabled     bool      `gorm:"not null;index" json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}
