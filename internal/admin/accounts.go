package admin

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

const (
	roleSuper    = "super"
	roleAdmin    = "admin"
	roleMerchant = "merchant" // manages one merchant's shop products, orders and after-sales

)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.@-]{3,32}$`)

func validPassword(p string) error {
	if utf8.RuneCountInString(p) < 8 || len(p) > 72 {
		return errors.New("密码长度需为 8–72 位")
	}
	return nil
}

func (h *Handler) session(c *gin.Context) {
	u := current(c)
	perms, _ := c.Get(permissionsKey)
	granted, _ := perms.(map[string]bool)
	list := []string{}
	for _, p := range permissionKeys {
		if granted[p.Key] {
			list = append(list, p.Key)
		}
	}
	res := gin.H{"ok": true, "id": u.ID, "username": u.Username, "role": u.Role, "permissions": list}
	if u.Role == roleMerchant && u.MerchantID != 0 {
		var m model.Merchant
		if h.st.DB.Select("id", "name").First(&m, u.MerchantID).Error == nil {
			res["merchant_id"], res["merchant_name"] = m.ID, m.Name
		}
	}
	c.JSON(200, res)
}

func (h *Handler) listAccounts(c *gin.Context) {
	rows := []model.AdminUser{}
	if err := h.st.DB.Order("id asc").Find(&rows).Error; err != nil {
		c.JSON(500, gin.H{"error": "账号加载失败"})
		return
	}
	c.JSON(200, rows)
}

// errNoSuper is returned when a change would leave no enabled super administrator.
var errNoSuper = errors.New("至少需要保留一个启用的超级管理员")

// keepSuper locks the enabled super rows so concurrent changes cannot remove the last one.
func keepSuper(tx *gorm.DB, change func() error) error {
	var supers []model.AdminUser
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("role=? AND enabled=?", roleSuper, true).Find(&supers).Error; err != nil {
		return err
	}
	if err := change(); err != nil {
		return err
	}
	var n int64
	if err := tx.Model(&model.AdminUser{}).Where("role=? AND enabled=?", roleSuper, true).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return errNoSuper
	}
	return nil
}

func (h *Handler) saveAccount(c *gin.Context) {
	var b struct {
		ID         uint64 `json:"id"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		Role       string `json:"role"`
		RoleID     uint64 `json:"role_id"`
		MerchantID uint64 `json:"merchant_id"`
		Enabled    bool   `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(400, gin.H{"error": "参数错误"})
		return
	}
	if b.Role != roleSuper && b.Role != roleAdmin && b.Role != roleMerchant {
		c.JSON(400, gin.H{"error": "角色无效"})
		return
	}
	me := current(c)
	if b.ID == me.ID && (b.Role != me.Role || !b.Enabled) {
		c.JSON(400, gin.H{"error": "不能修改自己的角色或停用自己"})
		return
	}
	if b.ID == 0 || b.Password != "" {
		if err := validPassword(b.Password); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
	}
	if b.Role != roleAdmin {
		b.RoleID = 0
	}
	if b.Role != roleMerchant {
		b.MerchantID = 0
	} else if b.MerchantID == 0 {
		c.JSON(400, gin.H{"error": "请选择商家账号管理的商家"})
		return
	}
	err := h.st.DB.Transaction(func(tx *gorm.DB) error {
		if b.MerchantID != 0 {
			if err := tx.Select("id").First(&model.Merchant{}, b.MerchantID).Error; err != nil {
				return errors.New("商家不存在")
			}
		}
		if b.RoleID != 0 {
			if err := tx.Select("id").First(&model.AdminRole{}, b.RoleID).Error; err != nil {
				return errors.New("角色模板不存在")
			}
		}
		return keepSuper(tx, func() error {
			if b.ID == 0 {
				name := strings.TrimSpace(b.Username)
				if !usernamePattern.MatchString(name) {
					return errors.New("用户名需为 3–32 位字母、数字或 _ . @ -")
				}
				hash, err := hashPassword(b.Password)
				if err != nil {
					return err
				}
				res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.AdminUser{Username: name, PasswordHash: hash, Role: b.Role, RoleID: b.RoleID, MerchantID: b.MerchantID, Enabled: b.Enabled})
				if res.Error == nil && res.RowsAffected == 0 {
					return errors.New("用户名已存在")
				}
				return res.Error
			}
			updates := map[string]any{"role": b.Role, "role_id": b.RoleID, "merchant_id": b.MerchantID, "enabled": b.Enabled}
			if b.Password != "" {
				hash, err := hashPassword(b.Password)
				if err != nil {
					return err
				}
				updates["password_hash"] = hash
			}
			res := tx.Model(&model.AdminUser{}).Where("id=?", b.ID).Updates(updates)
			if res.Error == nil && res.RowsAffected == 0 {
				return gorm.ErrRecordNotFound
			}
			return res.Error
		})
	})
	respond(c, err)
}

func (h *Handler) deleteAccount(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if id == current(c).ID {
		c.JSON(400, gin.H{"error": "不能删除当前登录的账号"})
		return
	}
	respond(c, h.st.DB.Transaction(func(tx *gorm.DB) error {
		return keepSuper(tx, func() error {
			res := tx.Delete(&model.AdminUser{}, id)
			if res.Error == nil && res.RowsAffected == 0 {
				return gorm.ErrRecordNotFound
			}
			return res.Error
		})
	}))
}

// changePassword lets any account change its own password and keeps the current session valid.
func (h *Handler) changePassword(c *gin.Context) {
	var b struct {
		Old string `json:"old_password"`
		New string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(400, gin.H{"error": "参数错误"})
		return
	}
	me := current(c)
	if bcrypt.CompareHashAndPassword([]byte(me.PasswordHash), []byte(b.Old)) != nil {
		c.JSON(400, gin.H{"error": "原密码不正确"})
		return
	}
	if err := validPassword(b.New); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	hash, err := hashPassword(b.New)
	if err != nil {
		respond(c, err)
		return
	}
	if err := h.st.DB.Model(&model.AdminUser{}).Where("id=?", me.ID).Update("password_hash", hash).Error; err != nil {
		respond(c, err)
		return
	}
	// Other sessions of this account are revoked by the new hash; re-bind this one.
	me.PasswordHash = hash
	if token, err := c.Cookie(sessionCookie); err == nil && validToken(token) {
		_ = h.sessions().Set(c.Request.Context(), sessionKey(token), sessionValue(me), sessionTTL).Err()
	}
	c.JSON(200, gin.H{"ok": true})
}
