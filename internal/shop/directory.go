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

// categoriesFromMenuKey switches the shop's categories between the bot's menus ("1", the
// default) and categories kept by hand in the console ("0").
const categoriesFromMenuKey = "shop_categories_from_menu"

// CategoriesFromMenu reports whether the shop's categories follow the bot's menus.
func (s *Service) CategoriesFromMenu() bool {
	switch strings.ToLower(strings.TrimSpace(s.configString(categoriesFromMenuKey))) {
	case "0", "false", "off", "no", "关", "关闭", "否":
		return false
	}
	return true
}

// menuLeaf is a merchant category ("merchants" menu item) below the top-level item roots[root].
type menuLeaf struct {
	root  int
	id    uint64
	title string
}

// menuTree walks the bot's menus as ordinary users see them: disabled items and admin-only
// items (with everything below them) are left out. It returns the top-level submenus and the
// merchant categories reached through each.
func (s *Service) menuTree() ([]model.MenuItem, []menuLeaf, error) {
	var items []model.MenuItem
	if err := s.st.DB.Where("enabled = ? AND role <> ?", true, "admin").Order("row_no asc, sort asc, id asc").Find(&items).Error; err != nil {
		return nil, nil, err
	}
	children := map[uint64][]model.MenuItem{}
	for _, it := range items {
		children[it.ParentID] = append(children[it.ParentID], it)
	}
	roots := []model.MenuItem{}
	leaves := []menuLeaf{}
	for _, root := range children[0] {
		if root.ActionType != "submenu" {
			continue
		}
		roots = append(roots, root)
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
					leaves = append(leaves, menuLeaf{len(roots) - 1, it.ID, strings.Join(p, " · ")})
				case "submenu":
					walk(it.ID, p, seen)
				}
			}
		}
		walk(root.ID, nil, map[uint64]bool{root.ID: true})
	}
	return roots, leaves, nil
}

// leafMerchants returns the enabled merchants of the given merchant categories, by category,
// with their enabled product counts.
func (s *Service) leafMerchants(leaves []menuLeaf) (map[uint64][]DirectoryMerchant, error) {
	byMenu := map[uint64][]DirectoryMerchant{}
	if len(leaves) == 0 {
		return byMenu, nil
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
	for _, m := range merchants {
		byMenu[m.MenuID] = append(byMenu[m.MenuID], DirectoryMerchant{ID: m.ID, Name: m.Name, Description: m.Description, Address: m.Address,
			Hours: m.Hours, Phone: m.Phone, ContactURL: m.ContactURL, Pinned: m.Pinned, ProductCount: count[m.ID]})
	}
	return byMenu, nil
}

// Directory mirrors the bot's menus: each top-level menu item, the merchant categories reached
// through its submenus, and their enabled merchants. Categories without merchants are dropped.
func (s *Service) Directory() ([]DirectoryCategory, error) {
	roots, leaves, err := s.menuTree()
	if err != nil {
		return nil, err
	}
	byMenu, err := s.leafMerchants(leaves)
	if err != nil {
		return nil, err
	}
	out := make([]DirectoryCategory, 0, len(roots))
	for _, r := range roots {
		out = append(out, DirectoryCategory{ID: r.ID, Title: r.Title, Sections: []DirectorySection{}})
	}
	for _, l := range leaves {
		if list := byMenu[l.id]; len(list) > 0 {
			out[l.root].Sections = append(out[l.root].Sections, DirectorySection{ID: l.id, Title: l.title, Merchants: list})
		}
	}
	result := []DirectoryCategory{}
	for _, c := range out {
		if len(c.Sections) > 0 {
			result = append(result, c)
		}
	}
	return result, nil
}

// MenuCategory is one row of the console's read-only view of the menu categories: a top-level
// menu item (Level 0, a tab on the shop home) or a merchant category below it (Level 1).
type MenuCategory struct {
	ID        uint64 `json:"id"`
	Name      string `json:"name"`
	Parent    string `json:"parent"`
	Level     int    `json:"level"`
	Merchants int    `json:"merchants"`
	Products  int64  `json:"products"`
	Shown     bool   `json:"shown"` // the shop home shows it (it has merchants)
}

// MenuCategories lists the menu categories as the shop home uses them, including empty ones
// (which the shop hides), for the console.
func (s *Service) MenuCategories() ([]MenuCategory, error) {
	roots, leaves, err := s.menuTree()
	if err != nil {
		return nil, err
	}
	byMenu, err := s.leafMerchants(leaves)
	if err != nil {
		return nil, err
	}
	out := []MenuCategory{}
	for i, r := range roots {
		head := len(out)
		out = append(out, MenuCategory{ID: r.ID, Name: r.Title, Level: 0})
		for _, l := range leaves {
			if l.root != i {
				continue
			}
			row := MenuCategory{ID: l.id, Name: l.title, Parent: r.Title, Level: 1, Merchants: len(byMenu[l.id])}
			for _, m := range byMenu[l.id] {
				row.Products += m.ProductCount
			}
			row.Shown = row.Merchants > 0
			out[head].Merchants += row.Merchants
			out[head].Products += row.Products
			out[head].Shown = out[head].Shown || row.Shown
			out = append(out, row)
		}
	}
	return out, nil
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
