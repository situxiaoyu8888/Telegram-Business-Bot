package shop

import (
	"strings"

	"telegram-business-bot/internal/model"
)

// DirectoryMerchant is a merchant as listed on the shop home page.
type DirectoryMerchant struct {
	ID           uint64 `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Address      string `json:"address"`
	Hours        string `json:"hours"`
	Phone        string `json:"phone"`
	ContactURL   string `json:"contact_url"`
	Pinned       bool   `json:"pinned"`
	ProductCount int64  `json:"product_count"`
}

// DirectorySection is one merchant category (a "merchants" menu item) and its merchants. Title
// is the path below the top-level category, e.g. "中餐 · 川菜" for a nested category.
type DirectorySection struct {
	ID        uint64              `json:"id"`
	Title     string              `json:"title"`
	Merchants []DirectoryMerchant `json:"merchants"`
}

// DirectoryCategory is a top-level menu item that leads to at least one merchant.
type DirectoryCategory struct {
	ID       uint64             `json:"id"`
	Title    string             `json:"title"`
	Sections []DirectorySection `json:"sections"`
}

// Directory mirrors the bot's menus: each top-level menu item, the merchant categories reached
// through its submenus, and their enabled merchants. Visibility follows the bot for ordinary
// users: disabled items and admin-only items (with everything below them) are left out, and
// categories without merchants are dropped.
func (s *Service) Directory() ([]DirectoryCategory, error) {
	var items []model.MenuItem
	if err := s.st.DB.Where("enabled = ? AND role <> ?", true, "admin").Order("row_no asc, sort asc, id asc").Find(&items).Error; err != nil {
		return nil, err
	}
	children := map[uint64][]model.MenuItem{}
	for _, it := range items {
		children[it.ParentID] = append(children[it.ParentID], it)
	}
	// Walk each top-level item through submenus to its merchant categories.
	type leaf struct {
		root  int
		id    uint64
		title string
	}
	out := []DirectoryCategory{}
	leaves := []leaf{}
	for _, root := range children[0] {
		if root.ActionType != "submenu" {
			continue
		}
		out = append(out, DirectoryCategory{ID: root.ID, Title: root.Title, Sections: []DirectorySection{}})
		var walk func(parent uint64, path []string, seen map[uint64]bool)
		walk = func(parent uint64, path []string, seen map[uint64]bool) {
			for _, it := range children[parent] {
				if seen[it.ID] {
					continue
				}
				seen[it.ID] = true
				p := append(append([]string{}, path...), it.Title)
				switch it.ActionType {
				case "merchants":
					leaves = append(leaves, leaf{len(out) - 1, it.ID, strings.Join(p, " · ")})
				case "submenu":
					walk(it.ID, p, seen)
				}
			}
		}
		walk(root.ID, nil, map[uint64]bool{root.ID: true})
	}
	if len(leaves) == 0 {
		return []DirectoryCategory{}, nil
	}
	ids := make([]uint64, 0, len(leaves))
	for _, l := range leaves {
		ids = append(ids, l.id)
	}
	var merchants []model.Merchant
	if err := s.st.DB.Where("menu_id IN ? AND enabled = ?", ids, true).Order("pinned desc, sort asc, id asc").Find(&merchants).Error; err != nil {
		return nil, err
	}
	var counts []struct {
		MerchantID uint64
		N          int64
	}
	if err := s.st.DB.Model(&model.ShopProduct{}).Select("merchant_id, COUNT(*) AS n").Where("enabled = ?", true).Group("merchant_id").Scan(&counts).Error; err != nil {
		return nil, err
	}
	count := map[uint64]int64{}
	for _, c := range counts {
		count[c.MerchantID] = c.N
	}
	byMenu := map[uint64][]DirectoryMerchant{}
	for _, m := range merchants {
		byMenu[m.MenuID] = append(byMenu[m.MenuID], DirectoryMerchant{ID: m.ID, Name: m.Name, Description: m.Description, Address: m.Address,
			Hours: m.Hours, Phone: m.Phone, ContactURL: m.ContactURL, Pinned: m.Pinned, ProductCount: count[m.ID]})
	}
	for _, l := range leaves {
		if list := byMenu[l.id]; len(list) > 0 {
			out[l.root].Sections = append(out[l.root].Sections, DirectorySection{ID: l.id, Title: l.title, Merchants: list})
		}
	}
	result := out[:0]
	for _, c := range out {
		if len(c.Sections) > 0 {
			result = append(result, c)
		}
	}
	return result, nil
}

// Merchant returns one merchant listed in the directory, or a not-found user error.
func (s *Service) Merchant(id uint64) (*DirectoryMerchant, error) {
	dir, err := s.Directory()
	if err != nil {
		return nil, err
	}
	for _, c := range dir {
		for _, sec := range c.Sections {
			for _, m := range sec.Merchants {
				if m.ID == id {
					return &m, nil
				}
			}
		}
	}
	return nil, userErr("商家不存在或已下线")
}
