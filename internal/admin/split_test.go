package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

const (
	testOperatorOrigin = "https://ops.example.com"
	testMerchantOrigin = "https://seller.example.com"
)

// splitFixture serves the API and uploads on separate engines, as with split listeners.
func splitFixture(t *testing.T) (api, main *gin.Engine, sessions *memorySessions) {
	h, _, sessions, accounts := accountFixture(t)
	owner := testAccount(t, 3, "owner", "owner-password", roleMerchant)
	owner.MerchantID = 7
	accounts[3] = owner
	h.WithSplit("https://api.example.com", "https://bot.example.com", testOperatorOrigin, testMerchantOrigin)
	api, main = gin.New(), gin.New()
	h.RegisterAPI(api)
	h.RegisterUpload(main)
	return api, main, sessions
}

func originRequest(r *gin.Engine, method, path, origin, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "admin-console")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestSplitLoginIsBoundToItsConsole(t *testing.T) {
	api, _, _ := splitFixture(t)
	admin := `{"username":"admin","password":"test-password"}`
	owner := `{"username":"owner","password":"owner-password"}`
	for _, c := range []struct {
		origin, body string
		code         int
	}{
		{testMerchantOrigin, admin, 403}, {testOperatorOrigin, owner, 403}, {"", admin, 403}, {"https://evil.example", admin, 403},
		{testOperatorOrigin, admin, 200}, {testMerchantOrigin, owner, 200},
	} {
		if w := originRequest(api, "POST", "/admin/login", c.origin, c.body, nil); w.Code != c.code {
			t.Fatalf("%s %s: %d %s", c.origin, c.body, w.Code, w.Body.String())
		}
	}

	w := originRequest(api, "POST", "/admin/login", testOperatorOrigin, admin, nil)
	cookie := w.Result().Cookies()[0]
	if cookie.Name != sessionCookie+"_"+portalOperator {
		t.Fatal(cookie.Name)
	}
	if w := originRequest(api, "GET", "/admin/api/session", testOperatorOrigin, "", cookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// The operator session is not accepted from the merchant console, even under its cookie name.
	moved := &http.Cookie{Name: sessionCookie + "_" + portalMerchant, Value: cookie.Value}
	if w := originRequest(api, "GET", "/admin/api/session", testMerchantOrigin, "", moved); w.Code != 401 {
		t.Fatal(w.Code)
	}
	// Downloads are navigations without Origin; they name their console.
	if w := originRequest(api, "GET", "/admin/api/session?portal=operator", "", "", cookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := originRequest(api, "GET", "/admin/api/session?portal=merchant", "", "", moved); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestUploadTokenIsSingleUse(t *testing.T) {
	api, main, _ := splitFixture(t)
	cookie := originRequest(api, "POST", "/admin/login", testOperatorOrigin, `{"username":"admin","password":"test-password"}`, nil).Result().Cookies()[0]
	w := originRequest(api, "POST", "/admin/api/upload-token", testOperatorOrigin, "{}", cookie)
	var got struct{ Token, URL string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.URL != "https://bot.example.com/admin/api/upload" {
		t.Fatal(w.Code, w.Body.String())
	}
	upload := func(token string) int {
		req := httptest.NewRequest("POST", "/admin/api/upload", strings.NewReader(""))
		req.Header.Set("X-Upload-Token", token)
		w := httptest.NewRecorder()
		main.ServeHTTP(w, req)
		return w.Code
	}
	// No file: the request gets past authentication and fails validation.
	if code := upload(got.Token); code != 400 {
		t.Fatal(code)
	}
	if code := upload(got.Token); code != 401 {
		t.Fatal("token reused", code)
	}
	if code := upload(strings.Repeat("a", 64)); code != 401 {
		t.Fatal(code)
	}
}
