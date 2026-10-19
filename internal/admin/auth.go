package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
)

const sessionCookie = "admin_session"
const sessionTTL = 12 * time.Hour
const accountKey = "admin_account"
const permissionsKey = "admin_permissions"

//go:embed login.html
var loginHTML string

type sessionStore interface {
	Get(context.Context, string) *redis.StringCmd
	Set(context.Context, string, interface{}, time.Duration) *redis.StatusCmd
	Del(context.Context, ...string) *redis.IntCmd
}

// accountStore is the lookup the session layer needs; account management uses the DB directly.
type accountStore interface {
	ByUsername(ctx context.Context, username string) (*model.AdminUser, error)
	ByID(ctx context.Context, id uint64) (*model.AdminUser, error)
	TouchLogin(ctx context.Context, id uint64) error
	Role(ctx context.Context, id uint64) (*model.AdminRole, error)
}

type dbAccounts struct{ db *gorm.DB }

func (a dbAccounts) ByUsername(ctx context.Context, username string) (*model.AdminUser, error) {
	var u model.AdminUser
	if err := a.db.WithContext(ctx).Where("username=?", username).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}
func (a dbAccounts) ByID(ctx context.Context, id uint64) (*model.AdminUser, error) {
	var u model.AdminUser
	if err := a.db.WithContext(ctx).First(&u, id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}
func (a dbAccounts) TouchLogin(ctx context.Context, id uint64) error {
	return a.db.WithContext(ctx).Model(&model.AdminUser{}).Where("id=?", id).Update("last_login_at", time.Now()).Error
}

func (a dbAccounts) Role(ctx context.Context, id uint64) (*model.AdminRole, error) {
	var r model.AdminRole
	if err := a.db.WithContext(ctx).First(&r, id).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

func (h *Handler) sessions() sessionStore {
	if h.sessionBackend != nil {
		return h.sessionBackend
	}
	return h.st.Redis
}
func (h *Handler) accounts() accountStore {
	if h.accountBackend != nil {
		return h.accountBackend
	}
	return dbAccounts{h.st.DB}
}

// Bootstrap creates the first super administrator from .env when no account exists yet.
// Afterwards accounts are managed in the console and the .env credentials are ignored.
func Bootstrap(db *gorm.DB, username, password string) error {
	var n int64
	if err := db.Model(&model.AdminUser{}).Count(&n).Error; err != nil {
		return err
	}
	username = strings.TrimSpace(username)
	if n > 0 || username == "" || password == "" {
		return nil
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}
	// The unique username makes concurrent first starts create a single account.
	return db.Where(model.AdminUser{Username: username}).FirstOrCreate(&model.AdminUser{Username: username, PasswordHash: hash, Role: roleSuper, Enabled: true}).Error
}

func hashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(b), err
}

// A hash of an unknown password keeps failed lookups as slow as wrong passwords.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("dummy-password-for-timing"), bcrypt.DefaultCost)

// sessionValue binds a session to the account and its current password; changing the password revokes it.
func sessionValue(u *model.AdminUser) string {
	sum := sha256.Sum256([]byte(u.PasswordHash))
	return fmt.Sprintf("%d:%s", u.ID, hex.EncodeToString(sum[:]))
}
func sessionKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "admin:session:" + hex.EncodeToString(sum[:])
}
func validToken(token string) bool {
	if len(token) != 64 {
		return false
	}
	_, err := hex.DecodeString(token)
	return err == nil
}

// A custom header disallows cross-site form submissions. No cross-origin access
// is enabled; SameSite cookies provide an additional layer of protection.
func csrfOK(c *gin.Context) bool {
	if c.GetHeader("X-Requested-With") != "admin-console" {
		c.AbortWithStatusJSON(403, gin.H{"error": "请从后台页面操作"})
		return false
	}
	return true
}
func (h *Handler) cookie(c *gin.Context, token string, age int) {
	c.SetSameSite(http.SameSiteStrictMode)
	c.SetCookie(sessionCookie, token, age, "/admin", "", c.Request.TLS != nil || c.GetHeader("X-Forwarded-Proto") == "https", true)
}
func (h *Handler) loginPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Data(200, "text/html; charset=utf-8", []byte(loginHTML))
}
func (h *Handler) login(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !csrfOK(c) {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	var b struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if c.ShouldBindJSON(&b) != nil {
		c.JSON(400, gin.H{"error": "请填写用户名和密码"})
		return
	}
	u, err := h.accounts().ByUsername(c.Request.Context(), strings.TrimSpace(b.Username))
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		c.JSON(503, gin.H{"error": "账号服务暂不可用"})
		return
	}
	hash := dummyHash
	if u != nil {
		hash = []byte(u.PasswordHash)
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(b.Password)) != nil || u == nil {
		c.JSON(401, gin.H{"error": "用户名或密码错误"})
		return
	}
	if !u.Enabled {
		c.JSON(403, gin.H{"error": "账号已停用，请联系超级管理员"})
		return
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		c.JSON(500, gin.H{"error": "无法创建登录会话"})
		return
	}
	token := hex.EncodeToString(tokenBytes)
	// Rotate a previous cookie rather than leaving a second live session behind.
	if old, err := c.Cookie(sessionCookie); err == nil && validToken(old) {
		if err = h.sessions().Del(c.Request.Context(), sessionKey(old)).Err(); err != nil {
			c.JSON(503, gin.H{"error": "会话服务暂不可用"})
			return
		}
	}
	if err := h.sessions().Set(c.Request.Context(), sessionKey(token), sessionValue(u), sessionTTL).Err(); err != nil {
		c.JSON(503, gin.H{"error": "会话服务暂不可用"})
		return
	}
	_ = h.accounts().TouchLogin(c.Request.Context(), u.ID)
	h.cookie(c, token, int(sessionTTL.Seconds()))
	c.JSON(200, gin.H{"ok": true})
}
func (h *Handler) logout(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !csrfOK(c) {
		return
	}
	if token, err := c.Cookie(sessionCookie); err == nil && validToken(token) {
		if err = h.sessions().Del(c.Request.Context(), sessionKey(token)).Err(); err != nil {
			c.JSON(503, gin.H{"error": "退出失败，请稍后重试"})
			return
		}
	}
	h.cookie(c, "", -1)
	c.JSON(200, gin.H{"ok": true})
}

// sessionAccount resolves the session to an enabled account whose password has not changed since login.
func (h *Handler) sessionAccount(ctx context.Context, value string) (*model.AdminUser, error) {
	idText, _, ok := strings.Cut(value, ":")
	id, err := strconv.ParseUint(idText, 10, 64)
	if !ok || err != nil {
		return nil, nil
	}
	u, err := h.accounts().ByID(ctx, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !u.Enabled || subtle.ConstantTimeCompare([]byte(value), []byte(sessionValue(u))) != 1 {
		return nil, nil
	}
	return u, nil
}
func (h *Handler) requireSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	token, err := c.Cookie(sessionCookie)
	if err == nil && validToken(token) {
		var value string
		value, err = h.sessions().Get(c.Request.Context(), sessionKey(token)).Result()
		if err != nil && err != redis.Nil {
			c.AbortWithStatusJSON(503, gin.H{"error": "会话服务暂不可用"})
			return
		}
		if err == nil {
			u, err := h.sessionAccount(c.Request.Context(), value)
			if err != nil {
				c.AbortWithStatusJSON(503, gin.H{"error": "账号服务暂不可用"})
				return
			}
			if u != nil {
				if c.Request.Method != "GET" && c.Request.Method != "HEAD" && !csrfOK(c) {
					return
				}
				perms, err := h.permissionsOf(c.Request.Context(), u)
				if err != nil {
					c.AbortWithStatusJSON(503, gin.H{"error": "账号服务暂不可用"})
					return
				}
				c.Set(accountKey, u)
				c.Set(permissionsKey, perms)
				c.Next()
				return
			}
		}
	}
	h.cookie(c, "", -1)
	if strings.HasPrefix(c.Request.URL.Path, "/admin/api/") {
		c.AbortWithStatusJSON(401, gin.H{"error": "请重新登录"})
	} else {
		c.Redirect(http.StatusSeeOther, "/admin/login")
		c.Abort()
	}
}

// current returns the signed-in account; requireSession guarantees it on every /admin route.
func current(c *gin.Context) *model.AdminUser {
	if v, ok := c.Get(accountKey); ok {
		return v.(*model.AdminUser)
	}
	return &model.AdminUser{}
}

// operator names the signed-in account in audit fields such as reviewer and push creator.
func operator(c *gin.Context) string { return current(c).Username }

// permissionsOf loads the account's role on every request, so role edits apply immediately.
func (h *Handler) permissionsOf(ctx context.Context, u *model.AdminUser) (map[string]bool, error) {
	perms := map[string]bool{}
	if u.Role == roleSuper {
		for _, p := range permissionKeys {
			perms[p.Key] = true
		}
		return perms, nil
	}
	if u.RoleID == 0 {
		return perms, nil
	}
	role, err := h.accounts().Role(ctx, u.RoleID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return perms, nil
	}
	if err != nil {
		return nil, err
	}
	for _, p := range splitPermissions(role.Permissions) {
		perms[p] = true
	}
	return perms, nil
}

// requirePermission passes when the account may use any of the given pages.
func requirePermission(pages ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		perms, _ := c.Get(permissionsKey)
		granted, _ := perms.(map[string]bool)
		for _, p := range pages {
			if granted[p] {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(403, gin.H{"error": "没有该页面的访问权限，请联系超级管理员"})
	}
}

func requireSuper(c *gin.Context) {
	if current(c).Role != roleSuper {
		c.AbortWithStatusJSON(403, gin.H{"error": "仅超级管理员可操作"})
		return
	}
	c.Next()
}
