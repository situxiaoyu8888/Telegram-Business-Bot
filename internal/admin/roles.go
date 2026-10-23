package admin

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

// permissionKeys are the console pages a role can grant, in navigation order.
// Account and role management are reserved for super administrators.
var permissionKeys = []struct{ Key, Label string }{
	{"home", "首页"}, {"pushes", "消息推送"}, {"bots", "业务机器人"}, {"routes", "消息路由"},
	{"merchants", "商家管理"}, {"users", "用户管理"}, {"withdrawals", "提现审核"}, {"deposits", "充值记录"},
	{"currencies", "币种汇率"}, {"exchanges", "换汇记录"}, {"redpackets", "红包记录"}, {"redpacket_covers", "红包封面"},
	{"news", "新闻管理"}, {"news_categories", "新闻分类"}, {"news_feeds", "RSS 订阅源"}, {"news_chats", "群和频道"},
	{"addresses", "充值地址"}, {"tickets", "客服工单"}, {"menus", "动态菜单"}, {"configs", "系统配置"},
	{"shop_products", "商城商品"}, {"shop_orders", "商城订单"}, {"shop_aftersales", "商城售后"}, {"shop_coupons", "商城优惠券"},
	// funds is not a page: it allows creating money (balance adjustments, crediting deposits).
	{"funds", "资金操作"},
	{"shop_reviews", "商品评价"}, {"shop_marketing", "商城运营"}, {"shop_reports", "销售报表"}, {"shop_settlements", "商家结算"}, {"shop_members", "会员积分"}, {"shop_messages", "商家消息"}, {"shop_applications", "入驻申请"},
}

func splitPermissions(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// normalizePermissions keeps known keys in canonical order and rejects unknown ones.
func normalizePermissions(in []string) (string, error) {
	want := map[string]bool{}
	for _, p := range in {
		want[strings.TrimSpace(p)] = true
	}
	out := []string{}
	for _, p := range permissionKeys {
		if want[p.Key] {
			out = append(out, p.Key)
			delete(want, p.Key)
		}
	}
	for p := range want {
		return "", fmt.Errorf("未知权限：%s", p)
	}
	return strings.Join(out, ","), nil
}

type roleRow struct {
	model.AdminRole
	AccountCount int64 `json:"account_count"`
}

func (h *Handler) listRoles(c *gin.Context) {
	roles := []model.AdminRole{}
	if err := h.st.DB.Order("id asc").Find(&roles).Error; err != nil {
		c.JSON(500, gin.H{"error": "角色加载失败"})
		return
	}
	var counts []struct {
		RoleID uint64
		N      int64
	}
	if err := h.st.DB.Model(&model.AdminUser{}).Select("role_id, count(*) as n").Where("role=? AND role_id<>0", roleAdmin).Group("role_id").Scan(&counts).Error; err != nil {
		c.JSON(500, gin.H{"error": "角色加载失败"})
		return
	}
	byRole := map[uint64]int64{}
	for _, n := range counts {
		byRole[n.RoleID] = n.N
	}
	rows := make([]roleRow, 0, len(roles))
	for _, r := range roles {
		rows = append(rows, roleRow{r, byRole[r.ID]})
	}
	c.JSON(200, rows)
}

func (h *Handler) saveRole(c *gin.Context) {
	var b struct {
		ID          uint64   `json:"id"`
		Name        string   `json:"name"`
		Permissions []string `json:"permissions"`
		Remark      string   `json:"remark"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		c.JSON(400, gin.H{"error": "参数错误"})
		return
	}
	name := strings.TrimSpace(b.Name)
	if name == "" || utf8.RuneCountInString(name) > 32 || utf8.RuneCountInString(b.Remark) > 255 {
		c.JSON(400, gin.H{"error": "角色名称需为 1–32 个字符，说明不超过 255 个字符"})
		return
	}
	perms, err := normalizePermissions(b.Permissions)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var res *gorm.DB
	if b.ID == 0 {
		res = h.st.DB.Create(&model.AdminRole{Name: name, Permissions: perms, Remark: b.Remark})
	} else {
		res = h.st.DB.Model(&model.AdminRole{}).Where("id=?", b.ID).Updates(map[string]any{"name": name, "permissions": perms, "remark": b.Remark})
	}
	if res.Error != nil {
		if store.IsDuplicate(res.Error) {
			c.JSON(400, gin.H{"error": "角色名称已存在"})
			return
		}
		respond(c, res.Error)
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (h *Handler) deleteRole(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	respond(c, h.st.DB.Transaction(func(tx *gorm.DB) error {
		var users int64
		// Lock the referencing accounts so none is assigned to the role while it is deleted.
		if err := tx.Model(&model.AdminUser{}).Clauses(clause.Locking{Strength: "UPDATE"}).Where("role_id=?", id).Count(&users).Error; err != nil {
			return err
		}
		if users > 0 {
			return fmt.Errorf("仍有 %d 个账号使用该角色，请先调整这些账号", users)
		}
		res := tx.Delete(&model.AdminRole{}, id)
		if res.Error == nil && res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return res.Error
	}))
}
