package admin

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
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

type memoryAccounts map[uint64]*model.AdminUser

func (m memoryAccounts) ByUsername(_ context.Context, name string) (*model.AdminUser, error) {
	for _, u := range m {
		if u.Username == name {
			copy := *u
			return &copy, nil
		}
	}
	return nil, gorm.ErrRecordNotFound
}
func (m memoryAccounts) ByID(_ context.Context, id uint64) (*model.AdminUser, error) {
	if u, ok := m[id]; ok {
		copy := *u
		return &copy, nil
	}
	return nil, gorm.ErrRecordNotFound
}
func (m memoryAccounts) TouchLogin(context.Context, uint64) error { return nil }

// MerchantActive treats every merchant as existing; staff need an enabled owner account.
func (m memoryAccounts) MerchantActive(_ context.Context, merchantID uint64, staff bool) (bool, error) {
	if !staff {
		return true, nil
	}
	for _, u := range m {
		if u.Role == roleMerchant && u.MerchantID == merchantID && u.Enabled {
			return true, nil
		}
	}
	return false, nil
}

// testAudit collects audit records written by the fixture's handler.
var testAudit []model.AdminAuditLog

// testRoles backs memoryAccounts.Role; accountFixture resets it (tests in this package are not parallel).
var testRoles = map[uint64]*model.AdminRole{}

func (m memoryAccounts) Role(_ context.Context, id uint64) (*model.AdminRole, error) {
	if r, ok := testRoles[id]; ok {
		return r, nil
	}
	return nil, gorm.ErrRecordNotFound
}

func testAccount(t testing.TB, id uint64, name, password, role string) *model.AdminUser {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return &model.AdminUser{ID: id, Username: name, PasswordHash: string(hash), Role: role, Enabled: true}
}
func authFixture(t testing.TB) (*Handler, *gin.Engine, *memorySessions) {
	h, r, s, _ := accountFixture(t)
	return h, r, s
}
func accountFixture(t testing.TB) (*Handler, *gin.Engine, *memorySessions, memoryAccounts) {
	gin.SetMode(gin.TestMode)
	backend := &memorySessions{values: map[string]string{}}
	testRoles = map[uint64]*model.AdminRole{}
	accounts := memoryAccounts{1: testAccount(t, 1, "admin", "test-password", roleSuper), 2: testAccount(t, 2, "staff", "staff-password", roleAdmin)}
	h := &Handler{sessionBackend: backend, accountBackend: accounts, auditSink: func(r model.AdminAuditLog) error { testAudit = append(testAudit, r); return nil }}
	testAudit = nil
	r := gin.New()
	h.Register(r)
	return h, r, backend, accounts
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
	_, r, s := authFixture(t)
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
	_, r, _ := authFixture(t)
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
	_, r, s, accounts := accountFixture(t)
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
	original := accounts[1].PasswordHash
	accounts[1].PasswordHash = testAccount(t, 1, "admin", "changed-password", roleSuper).PasswordHash
	if w = authRequest(r, "GET", "/admin/api/session", "", second, false); w.Code != 401 {
		t.Fatal("credential change retained session")
	}
	accounts[1].PasswordHash = original
	delete(s.values, sessionKey(second.Value))
	if w = authRequest(r, "GET", "/admin/api/session", "", second, false); w.Code != 401 {
		t.Fatal("expired session valid")
	}
}
func TestLogoutFailsClosedOnRedisFailureAndRequiresHeader(t *testing.T) {
	_, r, s := authFixture(t)
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
	_, r, _ := authFixture(t)
	cookie := loginCookie(t, r)
	w := authRequest(r, "POST", "/admin/api/pushes", `{}`, cookie, false)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestHTTPSLoginSetsSecureCookieAndNoStore(t *testing.T) {
	_, r, _ := authFixture(t)
	req := httptest.NewRequest("POST", "https://example.test/admin/login", strings.NewReader(`{"username":"admin","password":"test-password"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "admin-console")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 || !w.Result().Cookies()[0].Secure || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("HTTPS session missing protections")
	}
}

func login(t *testing.T, r *gin.Engine, user, password string) *http.Cookie {
	t.Helper()
	w := authRequest(r, "POST", "/admin/login", `{"username":"`+user+`","password":"`+password+`"}`, nil, true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	return w.Result().Cookies()[0]
}
func TestSessionReportsAccountAndRole(t *testing.T) {
	_, r, _, _ := accountFixture(t)
	w := authRequest(r, "GET", "/admin/api/session", "", login(t, r, "staff", "staff-password"), false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"username":"staff"`) || !strings.Contains(w.Body.String(), `"role":"admin"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestUnknownUserAndDisabledAccountCannotLogIn(t *testing.T) {
	_, r, _, accounts := accountFixture(t)
	if w := authRequest(r, "POST", "/admin/login", `{"username":"nobody","password":"test-password"}`, nil, true); w.Code != 401 {
		t.Fatal(w.Code)
	}
	accounts[2].Enabled = false
	if w := authRequest(r, "POST", "/admin/login", `{"username":"staff","password":"staff-password"}`, nil, true); w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestDisablingAccountRevokesLiveSession(t *testing.T) {
	_, r, _, accounts := accountFixture(t)
	cookie := login(t, r, "staff", "staff-password")
	accounts[2].Enabled = false
	if w := authRequest(r, "GET", "/admin/api/session", "", cookie, false); w.Code != 401 {
		t.Fatal("disabled account kept access", w.Code)
	}
}
func TestAccountManagementIsSuperOnly(t *testing.T) {
	_, r, _, _ := accountFixture(t)
	staff := login(t, r, "staff", "staff-password")
	for _, tt := range []struct{ method, path, body string }{{"GET", "/admin/api/accounts", ""}, {"POST", "/admin/api/accounts", `{"username":"x","password":"12345678","role":"super","enabled":true}`}, {"DELETE", "/admin/api/accounts/1", ""}} {
		if w := authRequest(r, tt.method, tt.path, tt.body, staff, true); w.Code != 403 {
			t.Fatal(tt.method, tt.path, w.Code)
		}
	}
}
func TestSuperCannotDemoteDisableOrDeleteSelf(t *testing.T) {
	_, r, _, _ := accountFixture(t)
	super := login(t, r, "admin", "test-password")
	for _, body := range []string{`{"id":1,"role":"admin","enabled":true}`, `{"id":1,"role":"super","enabled":false}`} {
		if w := authRequest(r, "POST", "/admin/api/accounts", body, super, true); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
	if w := authRequest(r, "DELETE", "/admin/api/accounts/1", "", super, true); w.Code != 400 {
		t.Fatal(w.Code)
	}
}
func TestChangePasswordRequiresOldPassword(t *testing.T) {
	_, r, _, _ := accountFixture(t)
	staff := login(t, r, "staff", "staff-password")
	if w := authRequest(r, "POST", "/admin/api/account/password", `{"old_password":"wrong","new_password":"new-password"}`, staff, true); w.Code != 400 || !strings.Contains(w.Body.String(), "原密码") {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := authRequest(r, "POST", "/admin/api/account/password", `{"old_password":"staff-password","new_password":"short"}`, staff, true); w.Code != 400 {
		t.Fatal(w.Code)
	}
}
func TestLastEnabledSuperCannotBeRemoved(t *testing.T) {
	h, r, _, _ := accountFixture(t)
	h.st = &store.Store{DB: testutil.DB(t,
		testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id"}, Rows: [][]driver.Value{{int64(3)}}},
		testutil.Step{Contains: "UPDATE `admin_users`", Affected: 1},
		testutil.Step{Contains: "count(*)", Columns: []string{"count"}, Rows: [][]driver.Value{{int64(0)}}},
	)}
	super := login(t, r, "admin", "test-password")
	w := authRequest(r, "POST", "/admin/api/accounts", `{"id":3,"role":"admin","enabled":true}`, super, true)
	if w.Code != 400 || !strings.Contains(w.Body.String(), errNoSuper.Error()) {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRolePermissionsGuardPagesAndApi(t *testing.T) {
	_, r, _, accounts := accountFixture(t)
	testRoles[7] = &model.AdminRole{ID: 7, Name: "财务", Permissions: "withdrawals,deposits"}
	accounts[2].RoleID = 7
	staff := login(t, r, "staff", "staff-password")
	w := authRequest(r, "GET", "/admin/api/session", "", staff, false)
	if !strings.Contains(w.Body.String(), `"permissions":["withdrawals","deposits"]`) {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"/admin/api/pushes", "/admin/api/configs", "/admin/api/roles", "/admin/api/accounts"} {
		if w := authRequest(r, "GET", path, "", staff, false); w.Code != 403 {
			t.Fatal(path, w.Code)
		}
	}
	if w := authRequest(r, "POST", "/admin/api/configs", `{"key":"x","value":"y"}`, staff, true); w.Code != 403 {
		t.Fatal("write without permission", w.Code)
	}
	// Role edits apply to live sessions without logging in again.
	testRoles[7].Permissions = "configs"
	w = authRequest(r, "GET", "/admin/api/session", "", staff, false)
	if !strings.Contains(w.Body.String(), `"permissions":["configs"]`) {
		t.Fatal(w.Body.String())
	}
}
func TestAccountWithoutRoleHasNoPages(t *testing.T) {
	_, r, _, _ := accountFixture(t)
	staff := login(t, r, "staff", "staff-password")
	if w := authRequest(r, "GET", "/admin/api/session", "", staff, false); !strings.Contains(w.Body.String(), `"permissions":[]`) {
		t.Fatal(w.Body.String())
	}
	if w := authRequest(r, "GET", "/admin/api/withdrawals", "", staff, false); w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestSuperHasEveryPermission(t *testing.T) {
	_, r, _, _ := accountFixture(t)
	w := authRequest(r, "GET", "/admin/api/session", "", login(t, r, "admin", "test-password"), false)
	for _, p := range permissionKeys {
		if !strings.Contains(w.Body.String(), `"`+p.Key+`"`) {
			t.Fatal(p.Key, w.Body.String())
		}
	}
}
func TestNormalizePermissionsRejectsUnknownKeys(t *testing.T) {
	if got, err := normalizePermissions([]string{"configs", "pushes", "pushes"}); err != nil || got != "pushes,configs" {
		t.Fatal(got, err)
	}
	if _, err := normalizePermissions([]string{"accounts"}); err == nil {
		t.Fatal("account management must not be grantable")
	}
}
func TestMerchantAccountGetsOnlyScopedShopPages(t *testing.T) {
	h, r, _, accounts := accountFixture(t)
	h.shop = &shop.Service{} // registers shop routes; the guards below reject before any handler runs
	h.st = &store.Store{DB: testutil.DB(t, testutil.Step{Contains: "merchants", Columns: []string{"id", "name"}, Rows: [][]driver.Value{{int64(9), "甲"}}})}
	r = gin.New()
	h.Register(r)
	m := testAccount(t, 3, "shopowner", "owner-password", roleMerchant)
	m.MerchantID = 9
	accounts[3] = m
	cookie := login(t, r, "shopowner", "owner-password")
	w := authRequest(r, "GET", "/admin/api/session", "", cookie, false)
	if !strings.Contains(w.Body.String(), `"permissions":["shop_products","shop_orders","shop_aftersales","shop_reviews","shop_reports","shop_settlements","shop_messages"]`) || !strings.Contains(w.Body.String(), `"merchant_name":"甲"`) {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"/admin/api/configs", "/admin/api/withdrawals", "/admin/api/shop-coupons", "/admin/api/accounts"} {
		if w := authRequest(r, "GET", path, "", cookie, false); w.Code != 403 {
			t.Fatal(path, w.Code)
		}
	}
	if w := authRequest(r, "POST", "/admin/api/shop-categories", `{"name":"x"}`, cookie, true); w.Code != 403 {
		t.Fatal("merchant changed shared categories", w.Code)
	}
	if w := authRequest(r, "POST", "/admin/api/shop-settlements", `{"merchant_id":9}`, cookie, true); w.Code != 403 {
		t.Fatal("merchant created its own settlement", w.Code)
	}
	if w := authRequest(r, "POST", "/admin/api/shop-reviews/1/hidden", `{"hidden":true}`, cookie, true); w.Code != 403 {
		t.Fatal("merchant hid a review", w.Code)
	}
	if w := authRequest(r, "GET", "/admin/api/shop-banners", "", cookie, false); w.Code != 403 {
		t.Fatal("merchant opened marketing", w.Code)
	}
	if w := authRequest(r, "GET", "/admin/api/dashboard", "", cookie, false); w.Code != 200 || w.Body.String() != "{}" {
		t.Fatal("platform figures shown to merchant", w.Body.String())
	}
	// A merchant account without a merchant has no pages at all.
	m.MerchantID = 0
	if w := authRequest(r, "GET", "/admin/api/session", "", cookie, false); !strings.Contains(w.Body.String(), `"permissions":[]`) {
		t.Fatal(w.Body.String())
	}
}

type memoryLimiter struct{ n map[string]int64 }

func (m *memoryLimiter) Incr(_ context.Context, key string) *redis.IntCmd {
	m.n[key]++
	return redis.NewIntResult(m.n[key], nil)
}
func (m *memoryLimiter) Expire(context.Context, string, time.Duration) *redis.BoolCmd {
	return redis.NewBoolResult(true, nil)
}
func (m *memoryLimiter) Get(_ context.Context, key string) *redis.StringCmd {
	v, ok := m.n[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(fmt.Sprint(v), nil)
}

func TestRepeatedLoginFailuresAreThrottled(t *testing.T) {
	h, _, _, _ := accountFixture(t)
	h.limiter = &memoryLimiter{n: map[string]int64{}}
	r := gin.New()
	h.Register(r)
	for i := 0; i < maxLoginFailuresPerUser; i++ {
		if w := authRequest(r, "POST", "/admin/login", `{"username":"admin","password":"wrong"}`, nil, true); w.Code != 401 {
			t.Fatal(i, w.Code)
		}
	}
	// Even the right password is refused while the account is throttled.
	if w := authRequest(r, "POST", "/admin/login", `{"username":"admin","password":"test-password"}`, nil, true); w.Code != 429 {
		t.Fatal("not throttled", w.Code)
	}
	// Another account from the same address is still allowed (under the per-IP limit).
	if w := authRequest(r, "POST", "/admin/login", `{"username":"staff","password":"staff-password"}`, nil, true); w.Code != 200 {
		t.Fatal(w.Code)
	}
}

func TestAuditRecordsLoginsWritesAndFailuresButNotReads(t *testing.T) {
	_, r, _, _ := accountFixture(t)
	authRequest(r, "POST", "/admin/login", `{"username":"staff","password":"nope"}`, nil, true)
	staff := login(t, r, "staff", "staff-password")
	authRequest(r, "GET", "/admin/api/session", "", staff, false)
	authRequest(r, "GET", "/admin/api/configs", "", staff, false)            // 403: no permission
	authRequest(r, "POST", "/admin/api/account/password", `{}`, staff, true) // write
	got := []string{}
	for _, a := range testAudit {
		got = append(got, fmt.Sprintf("%s %s %s %d", a.Username, a.Method, a.Path, a.Status))
	}
	want := []string{"staff LOGIN /admin/login (失败) 401", "staff LOGIN /admin/login 200", "staff GET /admin/api/configs 403", "staff POST /admin/api/account/password 400"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit:\n%s", strings.Join(got, "\n"))
	}
}
func TestMerchantStaffGetsChosenPagesOnly(t *testing.T) {
	h, r, _, accounts := accountFixture(t)
	h.shop = &shop.Service{}
	r = gin.New()
	h.Register(r)
	owner := testAccount(t, 3, "owner", "owner-password", roleMerchant)
	owner.MerchantID = 9
	staff := testAccount(t, 4, "packer", "packer-password", roleStaff)
	staff.MerchantID, staff.Permissions = 9, "shop_orders,configs"
	accounts[3], accounts[4] = owner, staff
	cookie := login(t, r, "packer", "packer-password")
	h.st = &store.Store{DB: testutil.DB(t, testutil.Step{Contains: "merchants", Columns: []string{"id", "name"}, Rows: [][]driver.Value{{int64(9), "甲"}}})}
	w := authRequest(r, "GET", "/admin/api/session", "", cookie, false)
	// "configs" is not a merchant page, so it is ignored even if stored.
	if !strings.Contains(w.Body.String(), `"permissions":["shop_orders"]`) || !strings.Contains(w.Body.String(), `"role":"merchant_staff"`) {
		t.Fatal(w.Body.String())
	}
	for _, tt := range []struct{ method, path, body string }{
		{"GET", "/admin/api/shop-products", ""},
		{"GET", "/admin/api/merchant-staff", ""},
		{"POST", "/admin/api/shop-settlements/payout", `{"address":"T"}`},
		{"POST", "/admin/api/shop-categories", `{}`},
	} {
		if w := authRequest(r, tt.method, tt.path, tt.body, cookie, true); w.Code != 403 {
			t.Fatal(tt.path, w.Code)
		}
	}
}

func TestMerchantStaffLoseAccessWhenOwnerDisabled(t *testing.T) {
	h, r, _, accounts := accountFixture(t)
	h.shop = &shop.Service{}
	r = gin.New()
	h.Register(r)
	owner := testAccount(t, 3, "owner", "owner-password", roleMerchant)
	owner.MerchantID = 9
	staff := testAccount(t, 4, "packer", "packer-password", roleStaff)
	staff.MerchantID, staff.Permissions = 9, "shop_orders"
	accounts[3], accounts[4] = owner, staff
	cookie := login(t, r, "packer", "packer-password")
	owner.Enabled = false
	h.st = &store.Store{DB: testutil.DB(t, testutil.Step{Contains: "merchants", Columns: []string{"id", "name"}, Rows: [][]driver.Value{{int64(9), "甲"}}})}
	if w := authRequest(r, "GET", "/admin/api/session", "", cookie, false); !strings.Contains(w.Body.String(), `"permissions":[]`) {
		t.Fatal("staff kept pages after the owner was disabled:", w.Body.String())
	}
}
