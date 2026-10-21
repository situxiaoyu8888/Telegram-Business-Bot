package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/joho/godotenv"
)

type Config struct {
	BusinessBotKey             string
	AppEnv                     string
	HTTPAddr                   string
	TelegramBotToken           string
	TelegramMode               string
	TelegramWebhookURL         string
	TelegramWebhookSecret      string
	AdminIDs                   map[int64]struct{}
	AdminTelegramBotToken      string
	AdminTelegramWebhookURL    string
	AdminTelegramWebhookSecret string
	AdminWebUser               string
	AdminWebPassword           string
	MySQLDSN                   string
	RedisAddr                  string
	RedisPassword              string
	RedisDB                    int
	SessionTTL                 time.Duration
	WalletCurrency             string
	WithdrawMinUSDT            string
	WithdrawFeeUSDT            string
	ReferralRate               string
	TronGridBaseURL            string
	TronGridAPIKey             string
	TronUSDTContract           string
	TronConfirmOnly            bool
	TronWatchEnabled           bool
	TronWatchInterval          time.Duration
	TronFallbackDepositAddress string

	UploadDir           string
	UploadPublicBaseURL string
	UploadMaxImageMB    int64
	UploadMaxVideoMB    int64

	// SHOP_H5_URL makes the user bots' menu button open the H5 shop; empty keeps the command list.
	ShopH5URL      string
	TelegramAPIURL string // optional Bot API server; empty uses https://api.telegram.org
	// Split listeners (all set, or none): HTTPAddr keeps webhooks, uploads and /uploads; the
	// operator and merchant consoles, the shop page and every API get their own addresses.
	// The public URLs are what browsers use, for cross-origin API calls and console checks.
	APIHTTPAddr       string
	OperatorHTTPAddr  string
	MerchantHTTPAddr  string
	ShopHTTPAddr      string
	APIPublicURL      string
	OperatorPublicURL string
	MerchantPublicURL string
	MainPublicURL     string   // public address of HTTPAddr, where consoles upload files
	ExtraOrigins      []string // more origins allowed to call the API (API_ALLOWED_ORIGINS)
	AdminPublicURL    string   // optional console address included in merchant approval messages
	ShopH5ButtonText  string

	// Daily database backups to BackupDir (empty disables), keeping BackupKeep files, at BackupHour.
	BackupDir  string
	BackupKeep int
	BackupHour int

	LogDir        string
	LogMaxSizeMB  int
	LogMaxBackups int
	LogMaxAgeDays int
}

func Load() (Config, error) {
	return load(".env")
}

func load(path string) (Config, error) {
	if err := godotenv.Load(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("load .env failed: %w", err)
	}

	required := []string{"APP_ENV", "HTTP_ADDR", "TELEGRAM_BOT_TOKEN", "TELEGRAM_MODE", "ADMIN_IDS", "ADMIN_WEB_USER", "ADMIN_WEB_PASSWORD", "MYSQL_DSN", "REDIS_ADDR", "WALLET_CURRENCY", "WITHDRAW_MIN_USDT", "WITHDRAW_FEE_USDT", "REFERRAL_RATE", "TRONGRID_BASE_URL", "UPLOAD_DIR", "UPLOAD_PUBLIC_BASE_URL", "REDIS_DB", "SESSION_TTL_MINUTES", "TRON_CONFIRM_ONLY", "TRON_WATCH_ENABLED", "TRON_WATCH_INTERVAL_SECONDS", "UPLOAD_MAX_IMAGE_MB", "UPLOAD_MAX_VIDEO_MB"}
	for _, key := range required {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			return Config{}, fmt.Errorf("required env %s is missing", key)
		}
	}
	ids, err := parseIDs(os.Getenv("ADMIN_IDS"))
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		BusinessBotKey:             strings.TrimSpace(os.Getenv("BUSINESS_BOT_KEY")),
		AppEnv:                     mustEnv("APP_ENV"),
		HTTPAddr:                   mustEnv("HTTP_ADDR"),
		TelegramBotToken:           mustEnv("TELEGRAM_BOT_TOKEN"),
		TelegramMode:               mustEnv("TELEGRAM_MODE"),
		TelegramWebhookURL:         strings.TrimSpace(os.Getenv("TELEGRAM_WEBHOOK_URL")),
		TelegramWebhookSecret:      strings.TrimSpace(os.Getenv("TELEGRAM_WEBHOOK_SECRET")),
		AdminIDs:                   ids,
		AdminTelegramBotToken:      strings.TrimSpace(os.Getenv("ADMIN_TELEGRAM_BOT_TOKEN")),
		AdminTelegramWebhookURL:    strings.TrimSpace(os.Getenv("ADMIN_TELEGRAM_WEBHOOK_URL")),
		AdminTelegramWebhookSecret: strings.TrimSpace(os.Getenv("ADMIN_TELEGRAM_WEBHOOK_SECRET")),
		AdminWebUser:               mustEnv("ADMIN_WEB_USER"),
		AdminWebPassword:           mustEnv("ADMIN_WEB_PASSWORD"),
		MySQLDSN:                   mustEnv("MYSQL_DSN"),
		RedisAddr:                  mustEnv("REDIS_ADDR"),
		RedisPassword:              os.Getenv("REDIS_PASSWORD"),
		WalletCurrency:             mustEnv("WALLET_CURRENCY"),
		WithdrawMinUSDT:            mustEnv("WITHDRAW_MIN_USDT"),
		WithdrawFeeUSDT:            mustEnv("WITHDRAW_FEE_USDT"),
		ReferralRate:               mustEnv("REFERRAL_RATE"),
		TronGridBaseURL:            mustEnv("TRONGRID_BASE_URL"),
		TronGridAPIKey:             strings.TrimSpace(os.Getenv("TRONGRID_API_KEY")),
		TronUSDTContract:           strings.TrimSpace(os.Getenv("TRON_USDT_CONTRACT")),
		TronFallbackDepositAddress: strings.TrimSpace(os.Getenv("TRON_FALLBACK_DEPOSIT_ADDRESS")),
		UploadDir:                  mustEnv("UPLOAD_DIR"),
		UploadPublicBaseURL:        strings.TrimRight(mustEnv("UPLOAD_PUBLIC_BASE_URL"), "/"),
	}

	cfg.RedisDB, err = strconv.Atoi(mustEnv("REDIS_DB"))
	if err != nil {
		return Config{}, fmt.Errorf("REDIS_DB invalid: %w", err)
	}

	sessionTTLMinutes, err := strconv.Atoi(mustEnv("SESSION_TTL_MINUTES"))
	if err != nil || sessionTTLMinutes <= 0 {
		return Config{}, fmt.Errorf("SESSION_TTL_MINUTES must be positive")
	}
	cfg.SessionTTL = time.Duration(sessionTTLMinutes) * time.Minute

	cfg.TronConfirmOnly, err = strconv.ParseBool(mustEnv("TRON_CONFIRM_ONLY"))
	if err != nil {
		return Config{}, fmt.Errorf("TRON_CONFIRM_ONLY invalid: %w", err)
	}

	cfg.TronWatchEnabled, err = strconv.ParseBool(mustEnv("TRON_WATCH_ENABLED"))
	if err != nil {
		return Config{}, fmt.Errorf("TRON_WATCH_ENABLED invalid: %w", err)
	}

	interval, err := strconv.Atoi(mustEnv("TRON_WATCH_INTERVAL_SECONDS"))
	if err != nil || interval < 10 {
		return Config{}, fmt.Errorf("TRON_WATCH_INTERVAL_SECONDS must be >= 10")
	}
	cfg.TronWatchInterval = time.Duration(interval) * time.Second

	cfg.UploadMaxImageMB, err = strconv.ParseInt(mustEnv("UPLOAD_MAX_IMAGE_MB"), 10, 64)
	if err != nil || cfg.UploadMaxImageMB <= 0 {
		return Config{}, fmt.Errorf("UPLOAD_MAX_IMAGE_MB invalid")
	}

	cfg.UploadMaxVideoMB, err = strconv.ParseInt(mustEnv("UPLOAD_MAX_VIDEO_MB"), 10, 64)
	if err != nil || cfg.UploadMaxVideoMB <= 0 {
		return Config{}, fmt.Errorf("UPLOAD_MAX_VIDEO_MB invalid")
	}

	cfg.TelegramAPIURL = strings.TrimRight(mustEnv("TELEGRAM_API_URL"), "/")
	cfg.APIHTTPAddr, cfg.OperatorHTTPAddr, cfg.MerchantHTTPAddr, cfg.ShopHTTPAddr = mustEnv("API_HTTP_ADDR"), mustEnv("OPERATOR_HTTP_ADDR"), mustEnv("MERCHANT_HTTP_ADDR"), mustEnv("SHOP_HTTP_ADDR")
	cfg.APIPublicURL, cfg.OperatorPublicURL, cfg.MerchantPublicURL = origin(mustEnv("API_PUBLIC_URL")), origin(mustEnv("OPERATOR_PUBLIC_URL")), origin(mustEnv("MERCHANT_PUBLIC_URL"))
	cfg.MainPublicURL = origin(mustEnv("MAIN_PUBLIC_URL"))
	if cfg.MainPublicURL == "" {
		cfg.MainPublicURL = origin(strings.TrimSuffix(cfg.UploadPublicBaseURL, "/uploads"))
	}
	for _, o := range strings.Split(mustEnv("API_ALLOWED_ORIGINS"), ",") {
		if o = origin(o); o != "" {
			cfg.ExtraOrigins = append(cfg.ExtraOrigins, o)
		}
	}
	cfg.BackupDir, cfg.BackupKeep, cfg.BackupHour = mustEnv("BACKUP_DIR"), 7, 3
	if v := mustEnv("BACKUP_KEEP"); v != "" {
		if cfg.BackupKeep, err = strconv.Atoi(v); err != nil || cfg.BackupKeep < 1 {
			return Config{}, fmt.Errorf("BACKUP_KEEP must be a positive number")
		}
	}
	if v := mustEnv("BACKUP_HOUR"); v != "" {
		if cfg.BackupHour, err = strconv.Atoi(v); err != nil || cfg.BackupHour < 0 || cfg.BackupHour > 23 {
			return Config{}, fmt.Errorf("BACKUP_HOUR must be 0-23")
		}
	}
	cfg.AdminPublicURL = mustEnv("ADMIN_PUBLIC_URL")
	cfg.ShopH5URL = mustEnv("SHOP_H5_URL")
	cfg.ShopH5ButtonText = mustEnv("SHOP_H5_BUTTON_TEXT")
	if cfg.ShopH5ButtonText == "" {
		cfg.ShopH5ButtonText = "商城"
	}

	// Log settings are optional so existing .env files keep working; LOG_DIR=off disables file output.
	cfg.LogDir = mustEnv("LOG_DIR")
	if cfg.LogDir == "" {
		cfg.LogDir = "./logs"
	} else if cfg.LogDir == "off" {
		cfg.LogDir = ""
	}
	for _, v := range []struct {
		key string
		def int
		dst *int
	}{{"LOG_MAX_SIZE_MB", 100, &cfg.LogMaxSizeMB}, {"LOG_MAX_BACKUPS", 30, &cfg.LogMaxBackups}, {"LOG_MAX_AGE_DAYS", 30, &cfg.LogMaxAgeDays}} {
		*v.dst = v.def
		if raw := mustEnv(v.key); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				return Config{}, fmt.Errorf("%s must be positive", v.key)
			}
			*v.dst = n
		}
	}

	if cfg.TelegramMode != "polling" && cfg.TelegramMode != "webhook" {
		return Config{}, fmt.Errorf("TELEGRAM_MODE must be polling or webhook")
	}

	if cfg.TelegramMode == "webhook" && cfg.TelegramWebhookURL == "" {
		return Config{}, fmt.Errorf("TELEGRAM_WEBHOOK_URL is required in webhook mode")
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Split reports whether the console, merchant console, shop page and API listen separately.
func (c Config) Split() bool { return c.APIHTTPAddr != "" }

// validateSplit checks the split-listener settings: all of them or none.
func (c Config) validateSplit() error {
	addrs := map[string]string{"OPERATOR_HTTP_ADDR": c.OperatorHTTPAddr, "MERCHANT_HTTP_ADDR": c.MerchantHTTPAddr, "SHOP_HTTP_ADDR": c.ShopHTTPAddr}
	if !c.Split() {
		for name, v := range addrs {
			if v != "" {
				return fmt.Errorf("%s needs API_HTTP_ADDR as well: set all split listeners or none", name)
			}
		}
		return nil
	}
	addrs["API_HTTP_ADDR"], addrs["HTTP_ADDR"] = c.APIHTTPAddr, c.HTTPAddr
	seen := map[string]string{}
	for name, v := range addrs {
		if v == "" {
			return fmt.Errorf("%s is required when API_HTTP_ADDR is set", name)
		}
		if other, dup := seen[v]; dup {
			return fmt.Errorf("%s and %s must use different addresses", name, other)
		}
		seen[v] = name
	}
	for name, v := range map[string]string{"API_PUBLIC_URL": c.APIPublicURL, "OPERATOR_PUBLIC_URL": c.OperatorPublicURL, "MERCHANT_PUBLIC_URL": c.MerchantPublicURL, "MAIN_PUBLIC_URL": c.MainPublicURL} {
		if v == "" {
			return fmt.Errorf("%s must be the http(s) address browsers use when API_HTTP_ADDR is set", name)
		}
	}
	if c.OperatorPublicURL == c.MerchantPublicURL {
		return fmt.Errorf("OPERATOR_PUBLIC_URL and MERCHANT_PUBLIC_URL must differ: the API tells the consoles apart by address")
	}
	return nil
}

// origin reduces an http(s) URL to scheme://host[:port]; anything else becomes "".
func origin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// AllowedOrigins are the browser origins that may call the API in split mode.
func (c Config) AllowedOrigins() []string {
	out := []string{c.OperatorPublicURL, c.MerchantPublicURL}
	if o := origin(c.ShopH5URL); o != "" {
		out = append(out, o)
	}
	return append(out, c.ExtraOrigins...)
}

// publicHost matches a DNS name with a dot (internationalised names must be in punycode).
var publicHost = regexp.MustCompile(`^(?i)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

func mustEnv(key string) string { return strings.TrimSpace(os.Getenv(key)) }

func parseIDs(s string) (map[int64]struct{}, error) {
	out := make(map[int64]struct{})
	for _, p := range strings.Split(s, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("invalid ADMIN_IDS value")
		}
		out[id] = struct{}{}
	}
	return out, nil
}

func (c Config) Validate() error {
	if c.TelegramMode == "webhook" {
		valid := regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
		if !valid.MatchString(c.TelegramWebhookSecret) {
			return fmt.Errorf("TELEGRAM_WEBHOOK_SECRET required (1-256 letters, digits, _ or -)")
		}
		if c.AdminTelegramBotToken != "" && (c.AdminTelegramWebhookURL == "" || !valid.MatchString(c.AdminTelegramWebhookSecret)) {
			return fmt.Errorf("admin webhook URL and secret required")
		}
	}
	min, e1 := decimal.NewFromString(c.WithdrawMinUSDT)
	fee, e2 := decimal.NewFromString(c.WithdrawFeeUSDT)
	rate, e3 := decimal.NewFromString(c.ReferralRate)
	if e1 != nil || e2 != nil || e3 != nil || !min.IsPositive() || fee.IsNegative() || rate.IsNegative() || rate.GreaterThan(decimal.NewFromInt(1)) || !min.Equal(min.Truncate(6)) || !fee.Equal(fee.Truncate(6)) {
		return fmt.Errorf("invalid withdrawal minimum, fee or referral rate")
	}
	if err := c.validateSplit(); err != nil {
		return err
	}
	if c.TelegramAPIURL != "" {
		if u, err := url.Parse(c.TelegramAPIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("TELEGRAM_API_URL must be an http(s) URL")
		}
	}
	if c.ShopH5URL != "" {
		u, err := url.Parse(c.ShopH5URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return fmt.Errorf("SHOP_H5_URL must be an https URL")
		}
		// Telegram rejects such menu buttons only at runtime (and the bot then retries forever), so
		// catch placeholders such as "https://你的H5商城地址/" at startup.
		if !publicHost.MatchString(u.Hostname()) {
			return fmt.Errorf("SHOP_H5_URL %q is not a real public address; use your domain, e.g. https://bot.example.com/shop/", c.ShopH5URL)
		}
		if n := utf8.RuneCountInString(c.ShopH5ButtonText); n == 0 || n > 64 {
			return fmt.Errorf("SHOP_H5_BUTTON_TEXT must be 1-64 characters")
		}
	}
	if c.WalletCurrency != "USDT" {
		return fmt.Errorf("only USDT wallets are supported")
	}
	if c.TronWatchEnabled && (!c.TronConfirmOnly || c.TronUSDTContract == "") {
		return fmt.Errorf("TRON watcher requires confirmed transactions and a token contract")
	}
	return nil
}
