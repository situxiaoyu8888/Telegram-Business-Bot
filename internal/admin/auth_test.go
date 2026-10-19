package admin

import (
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memorySessions struct {
	values map[string]string
	ttl    time.Duration
	err    error
}

func (s *memorySessions) Get(ctx context.Context, key string) *redis.StringCmd {
	v, ok := s.values[key]
	err := s.err
	if !ok && err == nil {
		err = redis.Nil
	}
	return redis.NewStringResult(v, err)
}
func (s *memorySessions) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) *redis.StatusCmd {
	if s.err == nil {
		s.values[key] = value.(string)
		s.ttl = ttl
	}
	return redis.NewStatusResult("OK", s.err)
}
func (s *memorySessions) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	if s.err == nil {
		for _, key := range keys {
			delete(s.values, key)
		}
	}
	return redis.NewIntResult(1, s.err)
}
func authFixture() (*Handler, *gin.Engine, *memorySessions) {
	gin.SetMode(gin.TestMode)
	backend := &memorySessions{values: map[string]string{}}
	h := &Handler{user: "admin", pass: "test-password", sessionBackend: backend}
	r := gin.New()
	h.Register(r)
	return h, r, backend
}
func authRequest(r *gin.Engine, method, path, body string, cookie *http.Cookie, header bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if header {
		req.Header.Set("X-Requested-With", "admin-console")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func loginCookie(t *testing.T, r *gin.Engine) *http.Cookie {
	t.Helper()
	w := authRequest(r, "POST", "/admin/login", `{"username":"admin","password":"test-password"}`, nil, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return w.Result().Cookies()[0]
}
func TestLogoutRevokesCookieAndOldBasicCredentials(t *testing.T) {
	_, r, s := authFixture()
	cookie := loginCookie(t, r)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/admin" || cookie.MaxAge != 43200 || s.ttl != sessionTTL {
		t.Fatalf("unsafe cookie/session: %+v", cookie)
	}
	if _, exists := s.values[sessionKey(cookie.Value)]; !exists {
		t.Fatal("session missing")
	}
	if w := authRequest(r, "GET", "/admin/api/session", "", cookie, false); w.Code != 200 {
		t.Fatal(w.Code)
	}
	w := authRequest(r, "POST", "/admin/logout", "", cookie, true)
	if w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout failed", w.Code)
	}
	if w = authRequest(r, "GET", "/admin/api/session", "", cookie, false); w.Code != 401 {
		t.Fatal("revoked session still accepted")
	}
	req := httptest.NewRequest("GET", "/admin/", nil)
	req.SetBasicAuth("admin", "test-password")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 303 || w.Header().Get("Location") != "/admin/login" || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("Basic auth bypassed logout")
	}
}
func TestLoginCSRFAndBadCredentials(t *testing.T) {
	_, r, _ := authFixture()
	for _, tt := range []struct {
		body   string
		header bool
		code   int
	}{{`{"username":"admin","password":"test-password"}`, false, 403}, {`{"username":"admin","password":"wrong"}`, true, 401}, {`{`, true, 400}} {
		w := authRequest(r, "POST", "/admin/login", tt.body, nil, tt.header)
		if w.Code != tt.code {
			t.Fatal(w.Code, tt.code)
		}
	}
}
func TestSessionRotationExpiryAndCredentialChange(t *testing.T) {
	h, r, s := authFixture()
	first := loginCookie(t, r)
	w := authRequest(r, "POST", "/admin/login", `{"username":"admin","password":"test-password"}`, first, true)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	second := w.Result().Cookies()[0]
	if second.Value == first.Value {
		t.Fatal("not rotated")
	}
	if w = authRequest(r, "GET", "/admin/api/session", "", first, false); w.Code != 401 {
		t.Fatal("old session valid")
	}
	h.pass = "changed"
	if w = authRequest(r, "GET", "/admin/api/session", "", second, false); w.Code != 401 {
		t.Fatal("credential change retained session")
	}
	h.pass = "test-password"
	delete(s.values, sessionKey(second.Value))
	if w = authRequest(r, "GET", "/admin/api/session", "", second, false); w.Code != 401 {
		t.Fatal("expired session valid")
	}
}
func TestLogoutFailsClosedOnRedisFailureAndRequiresHeader(t *testing.T) {
	_, r, s := authFixture()
	cookie := loginCookie(t, r)
	if w := authRequest(r, "POST", "/admin/logout", "", cookie, false); w.Code != 403 {
		t.Fatal(w.Code)
	}
	s.err = errors.New("offline")
	w := authRequest(r, "POST", "/admin/logout", "", cookie, true)
	if w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatal("reported success without revocation")
	}
	if w = authRequest(r, "GET", "/admin/api/session", "", cookie, false); w.Code != 503 {
		t.Fatal("failed open")
	}
}
func TestAuthenticatedWritesRequireCSRFHeader(t *testing.T) {
	_, r, _ := authFixture()
	cookie := loginCookie(t, r)
	w := authRequest(r, "POST", "/admin/api/pushes", `{}`, cookie, false)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestHTTPSLoginSetsSecureCookieAndNoStore(t *testing.T) {
	_, r, _ := authFixture()
	req := httptest.NewRequest("POST", "https://example.test/admin/login", strings.NewReader(`{"username":"admin","password":"test-password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "admin-console")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("HTTPS session missing protections")
	}
}
