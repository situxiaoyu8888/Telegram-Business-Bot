package config

import (
	"github.com/joho/godotenv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setupEnv(t *testing.T) {
	t.Helper()
	values, err := godotenv.Read("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range values {
		t.Setenv(k, v)
	}
	t.Setenv("ADMIN_IDS", "123456789")
}
func TestEnvironmentOnlyStartup(t *testing.T) {
	setupEnv(t)
	if _, err := load(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Fatal(err)
	}
}
func TestEnvironmentOverridesDotEnv(t *testing.T) {
	setupEnv(t)
	t.Setenv("REDIS_ADDR", "redis:6379")
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("REDIS_ADDR=localhost:6379\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := load(p)
	if err != nil || c.RedisAddr != "redis:6379" {
		t.Fatalf("env override lost: %s %v", c.RedisAddr, err)
	}
}
func TestInvalidConfigReturnsError(t *testing.T) {
	for _, item := range []struct{ k, v string }{{"ADMIN_IDS", "bad"}, {"UPLOAD_DIR", ""}, {"WITHDRAW_FEE_USDT", "-1"}, {"WITHDRAW_MIN_USDT", "nonsense"}, {"REFERRAL_RATE", "1.1"}, {"SHOP_H5_URL", "http://shop.example.com"}, {"SHOP_H5_URL", "shop.example.com"}, {"SHOP_H5_URL", "https://你的H5商城地址/"}, {"SHOP_H5_URL", "https://localhost/shop/"}} {
		t.Run(item.k, func(t *testing.T) {
			setupEnv(t)
			t.Setenv(item.k, item.v)
			if _, err := load(filepath.Join(t.TempDir(), "missing")); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
func TestWebhookSecretsRequired(t *testing.T) {
	setupEnv(t)
	t.Setenv("TELEGRAM_MODE", "webhook")
	t.Setenv("TELEGRAM_WEBHOOK_URL", "https://example.com/webhook")
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", "")
	if _, err := load(filepath.Join(t.TempDir(), "missing")); err == nil || !strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("%v", err)
	}
	t.Setenv("TELEGRAM_WEBHOOK_SECRET", "test-secret")
	t.Setenv("ADMIN_TELEGRAM_BOT_TOKEN", "admin-token")
	if _, err := load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("admin secret absent")
	}
}

func TestShopH5ButtonDefaults(t *testing.T) {
	setupEnv(t)
	t.Setenv("SHOP_H5_URL", "https://shop.example.com/")
	t.Setenv("SHOP_H5_BUTTON_TEXT", "")
	c, err := load(filepath.Join(t.TempDir(), "missing"))
	if err != nil || c.ShopH5URL != "https://shop.example.com/" || c.ShopH5ButtonText != "商城" {
		t.Fatal(c.ShopH5URL, c.ShopH5ButtonText, err)
	}
}

func TestSplitListeners(t *testing.T) {
	split := map[string]string{
		"API_HTTP_ADDR": ":18082", "OPERATOR_HTTP_ADDR": ":18090", "MERCHANT_HTTP_ADDR": ":18091", "SHOP_HTTP_ADDR": ":18081",
		"API_PUBLIC_URL": "https://api.example.com/", "OPERATOR_PUBLIC_URL": "https://ops.example.com/admin/",
		"MERCHANT_PUBLIC_URL": "https://seller.example.com", "MAIN_PUBLIC_URL": "https://bot.example.com",
		"API_ALLOWED_ORIGINS": "https://other.example.com/x, not-a-url",
	}
	set := func(skip string, override map[string]string) {
		for k, v := range split {
			if k == skip {
				v = ""
			}
			if o, ok := override[k]; ok {
				v = o
			}
			t.Setenv(k, v)
		}
	}
	setupEnv(t)
	set("", nil)
	c, err := load(filepath.Join(t.TempDir(), "missing"))
	if err != nil || !c.Split() || c.APIPublicURL != "https://api.example.com" || c.OperatorPublicURL != "https://ops.example.com" {
		t.Fatal(c, err)
	}
	if got := c.AllowedOrigins(); len(got) != 3 || got[2] != "https://other.example.com" {
		t.Fatal(got)
	}
	cases := []map[string]string{
		{"SHOP_HTTP_ADDR": ":18082"},                              // same as API
		{"OPERATOR_HTTP_ADDR": os.Getenv("HTTP_ADDR")},            // same as main
		{"MERCHANT_PUBLIC_URL": "https://ops.example.com/admin/"}, // consoles indistinguishable
		{"API_PUBLIC_URL": "api.example.com"},                     // not a URL
	}
	for _, o := range cases {
		set("", o)
		if _, err := load(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatalf("accepted %v", o)
		}
	}
	for _, skip := range []string{"OPERATOR_HTTP_ADDR", "MERCHANT_PUBLIC_URL", "API_HTTP_ADDR"} {
		set(skip, nil)
		if _, err := load(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Fatalf("accepted without %s", skip)
		}
	}
	set("", map[string]string{"API_HTTP_ADDR": "", "OPERATOR_HTTP_ADDR": "", "MERCHANT_HTTP_ADDR": "", "SHOP_HTTP_ADDR": ""})
	if c, err := load(filepath.Join(t.TempDir(), "missing")); err != nil || c.Split() {
		t.Fatal("single listener rejected", err)
	}
}
