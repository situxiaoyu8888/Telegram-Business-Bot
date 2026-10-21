package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"telegram-business-bot/internal/business"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
)

type staticTokens []business.BotToken

func (s staticTokens) Tokens(context.Context) ([]business.BotToken, error) { return s, nil }

// signedInitData builds Mini App initData for a user, signed like Telegram does.
func signedInitData(token string, userID int64) string {
	v := url.Values{"auth_date": {fmt.Sprint(time.Now().Unix())}, "user": {fmt.Sprintf(`{"id":%d,"first_name":"买家"}`, userID)}}
	keys := []string{}
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := []string{}
	for _, k := range keys {
		lines = append(lines, k+"="+v.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	v.Set("hash", hex.EncodeToString(mac.Sum(nil)))
	return v.Encode()
}

func TestShopHTTPFlowSignedByBusinessBot(t *testing.T) {
	f := newShopFixture(t, 10, "physical")
	gin.SetMode(gin.TestMode)
	r := gin.New()
	shop.NewHandler(f.svc, &store.Store{DB: f.db}, staticTokens{{ID: 0, Token: "1:primary"}, {ID: 7, Token: "2:business"}}, nil).Register(r)
	call := func(method, path, initData, body string) (int, map[string]any, []any) {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if initData != "" {
			req.Header.Set("X-Telegram-Init-Data", initData)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var obj map[string]any
		var arr []any
		_ = json.Unmarshal(w.Body.Bytes(), &obj)
		_ = json.Unmarshal(w.Body.Bytes(), &arr)
		return w.Code, obj, arr
	}

	if code, _, _ := call("GET", "/shop/", "", ""); code != 200 {
		t.Fatal("page", code)
	}
	if code, _, _ := call("GET", "/shop/api/me", "", ""); code != 401 {
		t.Fatal("unsigned request accepted", code)
	}
	if code, _, _ := call("GET", "/shop/api/me", signedInitData("3:unknown", 4001), ""); code != 401 {
		t.Fatal("unknown bot accepted", code)
	}

	init := signedInitData("2:business", 4001)
	code, me, _ := call("GET", "/shop/api/me", init, "")
	if code != 200 || me["telegram_user_id"].(float64) != 4001 {
		t.Fatal(code, me)
	}
	var u model.TelegramUser
	must(t, f.db.Where("telegram_user_id = ?", 4001).First(&u).Error)
	must(t, f.w.CreditDeposit(model.DepositOrder{UserID: u.ID, Network: "TRC20", Address: "a", Amount: "50", TxID: "http-dep"}))

	if code, _, list := call("GET", "/shop/api/products", init, ""); code != 200 || len(list) != 1 {
		t.Fatal("products", code, list)
	}
	body := fmt.Sprintf(`{"request_id":"h1","items":[{"sku_id":%d,"quantity":2}],"delivery_type":"pickup"}`, f.sku.ID)
	code, order, _ := call("POST", "/shop/api/orders", init, body)
	if code != 200 || order["pay_amount"] != "20.000000" || order["status"] != "paid" {
		t.Fatal(code, order)
	}
	var stored model.ShopOrder
	must(t, f.db.First(&stored, uint64(order["id"].(float64))).Error)
	if stored.BotID != 7 || stored.Address != "曼谷 1 号" {
		t.Fatalf("order bot=%d address=%q", stored.BotID, stored.Address)
	}
	// Another buyer cannot read or cancel it.
	other := signedInitData("1:primary", 4002)
	if code, _, _ := call("GET", fmt.Sprintf("/shop/api/orders/%d", stored.ID), other, ""); code != 400 {
		t.Fatal("foreign order readable", code)
	}
	if code, _, _ := call("POST", fmt.Sprintf("/shop/api/orders/%d/cancel", stored.ID), other, ""); code != 400 {
		t.Fatal("foreign order cancelable", code)
	}
	if code, res, _ := call("POST", "/shop/api/orders", init, strings.NewReplacer("h1", "h2", `"quantity":2`, `"quantity":4`).Replace(body)); code != 400 || !strings.Contains(res["error"].(string), "余额不足") {
		t.Fatal("expected insufficient balance", code, res)
	}
}
