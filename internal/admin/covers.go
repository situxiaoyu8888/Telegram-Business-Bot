package admin

import (
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
)

// Red packet covers: images senders pick in the bot's red packet wizard. Packets keep the image
// URL they were created with, so editing or deleting a cover does not change packets already sent.

func (h *Handler) redPacketCovers(c *gin.Context) {
	list := []model.RedPacketCover{}
	if h.st.DB.Order("is_default desc, sort asc, id asc").Find(&list).Error != nil {
		failed(c, "封面加载失败")
		return
	}
	c.JSON(http.StatusOK, list)
}

func (h *Handler) saveRedPacketCover(c *gin.Context) {
	var b model.RedPacketCover
	if c.ShouldBindJSON(&b) != nil {
		bad(c, "参数错误")
		return
	}
	b.Name, b.ImageURL = strings.TrimSpace(b.Name), strings.TrimSpace(b.ImageURL)
	if b.Name == "" || utf8.RuneCountInString(b.Name) > 32 {
		bad(c, "请填写 32 字以内的封面名称")
		return
	}
	if u, err := url.Parse(b.ImageURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(b.ImageURL) > 512 {
		bad(c, "请上传封面图片或填写图片地址（http/https）")
		return
	}
	if b.IsDefault && !b.Enabled {
		bad(c, "默认封面必须启用")
		return
	}
	err := h.st.DB.Transaction(func(tx *gorm.DB) error {
		if b.IsDefault {
			if err := tx.Model(&model.RedPacketCover{}).Where("is_default = ? AND id <> ?", true, b.ID).Update("is_default", false).Error; err != nil {
				return err
			}
		}
		if b.ID == 0 {
			return tx.Create(&b).Error
		}
		return tx.Model(&model.RedPacketCover{}).Where("id = ?", b.ID).Updates(map[string]any{
			"name": b.Name, "image_url": b.ImageURL, "sort": b.Sort, "enabled": b.Enabled, "is_default": b.IsDefault}).Error
	})
	if err != nil {
		failed(c, "保存失败")
		return
	}
	c.JSON(http.StatusOK, b)
}

func (h *Handler) deleteRedPacketCover(c *gin.Context) {
	id, ok := idParam(c)
	if !ok {
		return
	}
	if h.st.DB.Delete(&model.RedPacketCover{}, id).Error != nil {
		failed(c, "删除失败")
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
