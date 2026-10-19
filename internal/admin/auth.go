package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const sessionCookie = "admin_session"
const sessionTTL = 12 * time.Hour

//go:embed login.html
var loginHTML string

type sessionStore interface {
	Get(context.Context, string) *redis.StringCmd
	Set(context.Context, string, interface{}, time.Duration) *redis.StatusCmd
	Del(context.Context, ...string) *redis.IntCmd
}

func (h *Handler) sessions() sessionStore {
	if h.sessionBackend != nil {
		return h.sessionBackend
	}
	return h.st.Redis
}
func (h *Handler) credentialHash() string {
	sum := sha256.Sum256([]byte(h.user + "\x00" + h.pass))
	return hex.EncodeToString(sum[:])
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
	supplied := sha256.Sum256([]byte(b.Username + "\x00" + b.Password))
	expected := sha256.Sum256([]byte(h.user + "\x00" + h.pass))
	if h.user == "" || h.pass == "" || subtle.ConstantTimeCompare(supplied[:], expected[:]) != 1 {
		c.JSON(401, gin.H{"error": "用户名或密码错误"})
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
	if err := h.sessions().Set(c.Request.Context(), sessionKey(token), h.credentialHash(), sessionTTL).Err(); err != nil {
		c.JSON(503, gin.H{"error": "会话服务暂不可用"})
		return
	}
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
func (h *Handler) requireSession(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	token, err := c.Cookie(sessionCookie)
	if err == nil && validToken(token) {
		var digest string
		digest, err = h.sessions().Get(c.Request.Context(), sessionKey(token)).Result()
		if err != nil && err != redis.Nil {
			c.AbortWithStatusJSON(503, gin.H{"error": "会话服务暂不可用"})
			return
		}
		if err == nil && subtle.ConstantTimeCompare([]byte(digest), []byte(h.credentialHash())) == 1 {
			if c.Request.Method != "GET" && c.Request.Method != "HEAD" && !csrfOK(c) {
				return
			}
			c.Next()
			return
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
