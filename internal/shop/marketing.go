package shop

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"telegram-business-bot/internal/model"
)

// Home is the H5 home page's promotional content.
type Home struct {
	Banners  []model.ShopBanner `json:"banners"`
	Featured []ProductCard      `json:"featured"`
	Promos   []ProductCard      `json:"promos"`
	// CategoriesFromMenu makes the home's tabs the bot's menus; otherwise they are the
	// console's shop categories.
	CategoriesFromMenu bool `json:"categories_from_menu"`
}

func (s *Service) Home() (*Home, error) {
	now := time.Now()
	h := &Home{Banners: []model.ShopBanner{}, CategoriesFromMenu: s.CategoriesFromMenu()}
	if err := s.st.DB.Where("enabled = ? AND (starts_at IS NULL OR starts_at <= ?) AND (ends_at IS NULL OR ends_at > ?)", true, now, now).Order("sort asc, id desc").Limit(10).Find(&h.Banners).Error; err != nil {
		return nil, err
	}
	var err error
	if h.Featured, err = s.Products(ProductQuery{Featured: true}); err != nil {
		return nil, err
	}
	if h.Promos, err = s.Products(ProductQuery{OnPromo: true}); err != nil {
		return nil, err
	}
	if len(h.Featured) > 10 {
		h.Featured = h.Featured[:10]
	}
	if len(h.Promos) > 10 {
		h.Promos = h.Promos[:10]
	}
	return h, nil
}

func (s *Service) AdminBanners() ([]model.ShopBanner, error) {
	rows := []model.ShopBanner{}
	err := s.st.DB.Order("sort asc, id desc").Find(&rows).Error
	return rows, err
}

func (s *Service) SaveBanner(b model.ShopBanner) error {
	b.Title, b.ImageURL, b.LinkValue = strings.TrimSpace(b.Title), strings.TrimSpace(b.ImageURL), strings.TrimSpace(b.LinkValue)
	if err := checkLen(b.Title, "标题", 64, false); err != nil {
		return err
	}
	if u, err := url.Parse(b.ImageURL); err != nil || u.Scheme != "https" || u.Host == "" {
		return userErr("图片地址需为 https 链接，可先上传图片")
	}
	switch b.LinkType {
	case "none":
		b.LinkValue = ""
	case "product", "category":
		id, err := strconv.ParseUint(b.LinkValue, 10, 64)
		if err != nil || id == 0 {
			return userErr("请填写有效的商品或分类 ID")
		}
		table := map[string]any{"product": &model.ShopProduct{}, "category": &model.ShopCategory{}}[b.LinkType]
		if err := s.st.DB.Select("id").First(table, id).Error; err != nil {
			return userErr("链接的商品或分类不存在")
		}
	case "url":
		if u, err := url.Parse(b.LinkValue); err != nil || u.Scheme != "https" || u.Host == "" {
			return userErr("外部链接需为 https 地址")
		}
	default:
		return userErr("链接类型无效")
	}
	if b.StartsAt != nil && b.EndsAt != nil && !b.EndsAt.After(*b.StartsAt) {
		return userErr("结束时间需晚于开始时间")
	}
	if b.ID == 0 {
		return s.st.DB.Create(&b).Error
	}
	res := s.st.DB.Model(&model.ShopBanner{}).Where("id = ?", b.ID).Updates(map[string]any{"title": b.Title, "image_url": b.ImageURL, "link_type": b.LinkType, "link_value": b.LinkValue, "sort": b.Sort, "enabled": b.Enabled, "starts_at": b.StartsAt, "ends_at": b.EndsAt})
	if res.Error == nil && res.RowsAffected == 0 {
		return userErr("轮播图不存在")
	}
	return res.Error
}

func (s *Service) DeleteBanner(id uint64) error {
	return s.st.DB.Delete(&model.ShopBanner{}, id).Error
}
