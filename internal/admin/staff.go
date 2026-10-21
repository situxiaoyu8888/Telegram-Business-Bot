package admin

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// staffPermissions keeps only pages a merchant account itself has, in canonical order.
func staffPermissions(in []string) (string, error) {
	want := map[string]bool{}
	for _, p := range in {
		want[strings.TrimSpace(p)] = true
	}
	out := []string{}
	for _, p := range merchantPermissions {
		if want[p] {
			out = append(out, p)
			delete(want, p)
		}
	}
	for p := range want {
		if p != "" {
			return "", errors.New("员工不能使用页面：" + p)
		}
	}
	if len(out) == 0 {
		return "", errors.New("请至少选择一个员工可使用的页面")
	}
	return strings.Join(out, ","), nil
}

func (h *Handler) listStaff(c *gin.Context) {
	rows := []model.AdminUser{}
	err := h.st.DB.Where("role = ? AND merchant_id = ?", roleStaff, merchantScope(c)).Order("id asc").Find(&rows).Error
	listOrError(c, rows, err)
}

// saveStaff creates or updates a staff account of the signed-in merchant.
func (h *Handler) saveStaff(c *gin.Context) {
	var b struct {
		ID          uint64   `json:"id"`
		Username    string   `json:"username"`
		Password    string   `json:"password"`
		Permissions []string `json:"permissions"`
		Enabled     bool     `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(400, gin.H{"error": "参数错误"})
		return
	}
	perms, err := staffPermissions(b.Permissions)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if b.ID == 0 || b.Password != "" {
		if err := validPassword(b.Password); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
	}
	merchantID := merchantScope(c)
	if b.ID == 0 {
		name := strings.TrimSpace(b.Username)
		if !usernamePattern.MatchString(name) {
			c.JSON(400, gin.H{"error": "用户名需为 3–32 位字母、数字或 _ . @ -"})
			return
		}
		hash, err := hashPassword(b.Password)
		if err != nil {
			respond(c, err)
			return
		}
		err = h.st.DB.Create(&model.AdminUser{Username: name, PasswordHash: hash, Role: roleStaff, MerchantID: merchantID, Permissions: perms, Enabled: b.Enabled}).Error
		if store.IsDuplicate(err) {
			c.JSON(400, gin.H{"error": "用户名已存在"})
			return
		}
		respond(c, err)
		return
	}
	updates := map[string]any{"permissions": perms, "enabled": b.Enabled}
	if b.Password != "" {
		hash, err := hashPassword(b.Password)
		if err != nil {
			respond(c, err)
			return
		}
		updates["password_hash"] = hash
	}
	res := h.st.DB.Model(&model.AdminUser{}).Where("id = ? AND role = ? AND merchant_id = ?", b.ID, roleStaff, merchantID).Updates(updates)
	if res.Error == nil && res.RowsAffected == 0 {
		respond(c, gorm.ErrRecordNotFound)
		return
	}
	respond(c, res.Error)
}

func (h *Handler) deleteStaff(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	res := h.st.DB.Where("id = ? AND role = ? AND merchant_id = ?", id, roleStaff, merchantScope(c)).Delete(&model.AdminUser{})
	if res.Error == nil && res.RowsAffected == 0 {
		respond(c, gorm.ErrRecordNotFound)
		return
	}
	respond(c, res.Error)
}
