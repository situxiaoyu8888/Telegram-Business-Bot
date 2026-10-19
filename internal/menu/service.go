package menu

import (
	"fmt"
	"sort"
	"strings"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"unicode"
)

type Button struct {
	Text, CallbackData, URL string
	Row                     int
}
type Service struct{ st *store.Store }

func New(st *store.Store) *Service { return &Service{st: st} }
func (s *Service) Root(isAdmin bool) ([]Button, error) {
	var items []model.MenuItem
	q := s.st.DB.Where("parent_id=0 AND enabled=1 AND placement<>?", "reply")
	if !isAdmin {
		q = q.Where("role<>'admin'")
	}
	if err := q.Order("sort asc,id asc").Find(&items).Error; err != nil {
		return nil, err
	}
	return convert(items), nil
}

func (s *Service) AdminRoot() ([]Button, error) {
	var items []model.MenuItem
	if err := s.st.DB.Where("parent_id=0 AND enabled=1 AND role='admin' AND placement<>?", "reply").Order("sort asc,id asc").Find(&items).Error; err != nil {
		return nil, err
	}
	return convert(items), nil
}

func (s *Service) Children(parent uint64, isAdmin bool) ([]Button, error) {
	var items []model.MenuItem
	q := s.st.DB.Where("parent_id=? AND enabled=1 AND placement<>?", parent, "reply")
	if !isAdmin {
		q = q.Where("role<>'admin'")
	}
	if err := q.Order("sort asc,id asc").Find(&items).Error; err != nil {
		return nil, err
	}
	return convert(items), nil
}
func convert(items []model.MenuItem) []Button {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].RowNo == items[j].RowNo {
			return items[i].Sort < items[j].Sort
		}
		return items[i].RowNo < items[j].RowNo
	})
	out := make([]Button, 0, len(items))
	for _, it := range items {
		b := Button{Text: it.Title, Row: it.RowNo}
		if it.ActionType == "url" {
			b.URL = it.ActionValue
		} else {
			b.CallbackData = "menu:" + it.Code
		}
		out = append(out, b)
	}
	return out
}
func (s *Service) ByCode(code string, isAdmin bool) (model.MenuItem, error) {
	var m model.MenuItem
	err := s.st.DB.Where("code=? AND enabled=1", code).First(&m).Error
	if err != nil {
		return m, err
	}
	if err := s.checkAccess(m, isAdmin); err != nil {
		return m, err
	}
	return m, nil
}

func (s *Service) checkAccess(m model.MenuItem, isAdmin bool) error {
	visited := map[uint64]bool{}
	for {
		if !m.Enabled || (m.Role == "admin" && !isAdmin) || visited[m.ID] {
			return fmt.Errorf("menu unavailable")
		}
		visited[m.ID] = true
		if m.ParentID == 0 {
			return nil
		}
		parentID := m.ParentID
		m = model.MenuItem{}
		if err := s.st.DB.First(&m, parentID).Error; err != nil {
			return err
		}
	}
}
func (s *Service) Config(key, def string) string {
	var c model.SystemConfig
	if err := s.st.DB.Where("`key`=?", key).First(&c).Error; err != nil {
		return def
	}
	return c.Value
}

// Reply buttons send their visible text, so ambiguous titles must never dispatch.
func (s *Service) ReplyItems(isAdmin, adminOnly bool) ([]model.MenuItem, error) {
	var items []model.MenuItem
	q := s.st.DB.Where("parent_id=0 AND enabled=1 AND placement=?", "reply")
	if adminOnly {
		q = q.Where("role='admin'")
	} else if !isAdmin {
		q = q.Where("role<>'admin'")
	}
	err := q.Order("row_no asc,sort asc,id asc").Find(&items).Error
	return items, err
}
func (s *Service) ReplyByTitle(title string, isAdmin, adminOnly bool) (*model.MenuItem, error) {
	var items []model.MenuItem
	q := s.st.DB.Where("enabled=1 AND placement=?", "reply").Where("title=?", title)
	if adminOnly {
		q = q.Where("role='admin'")
	} else if !isAdmin {
		q = q.Where("role<>'admin'")
	}
	err := q.Find(&items).Error
	if err != nil {
		return nil, err
	}
	var found *model.MenuItem
	for i := range items {
		if items[i].Title == title {
			if found != nil {
				return nil, fmt.Errorf("ambiguous menu title")
			}
			found = &items[i]
		}
	}
	if found != nil {
		if err := s.checkAccess(*found, isAdmin); err != nil {
			return nil, err
		}
	}
	return found, nil
}

func (s *Service) MerchantMenu(id uint64, isAdmin bool) (model.MenuItem, error) {
	var item model.MenuItem
	if err := s.st.DB.First(&item, id).Error; err != nil {
		return item, err
	}
	if item.ActionType != "merchants" || item.ParentID == 0 {
		return item, fmt.Errorf("merchant menu unavailable")
	}
	return item, s.checkAccess(item, isAdmin)
}

func (s *Service) ReplyChildren(parent uint64, isAdmin, adminOnly bool) ([]model.MenuItem, error) {
	var items []model.MenuItem
	q := s.st.DB.Where("parent_id=? AND enabled=1 AND placement=?", parent, "reply")
	if adminOnly {
		q = q.Where("role='admin'")
	} else if !isAdmin {
		q = q.Where("role<>'admin'")
	}
	err := q.Order("row_no asc,sort asc,id asc").Find(&items).Error
	return items, err
}

// Recommendation candidates must remain inside the selected visible category.
// Walking through visible submenu nodes excludes disabled and restricted branches.
func (s *Service) MerchantDescendants(root uint64, isAdmin, adminOnly bool) ([]uint64, error) {
	var items []model.MenuItem
	q := s.st.DB.Where("enabled=1")
	if adminOnly {
		q = q.Where("role='admin'")
	} else if !isAdmin {
		q = q.Where("role<>'admin'")
	}
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return merchantDescendants(items, root), nil
}
func merchantDescendants(items []model.MenuItem, root uint64) []uint64 {
	byID := map[uint64]model.MenuItem{}
	children := map[uint64][]uint64{}
	for _, item := range items {
		byID[item.ID] = item
		children[item.ParentID] = append(children[item.ParentID], item.ID)
	}
	ancestors := map[uint64]bool{}
	for id := root; id != 0; {
		item, ok := byID[id]
		if !ok || ancestors[id] || (id != root && item.ActionType != "submenu") {
			return nil
		}
		ancestors[id] = true
		id = item.ParentID
	}
	result := []uint64{}
	queue := []uint64{root}
	seen := map[uint64]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		item, ok := byID[id]
		if !ok {
			continue
		}
		if item.ActionType == "merchants" && item.ParentID != 0 {
			result = append(result, id)
		} else if item.ActionType == "submenu" {
			queue = append(queue, children[id]...)
		}
	}
	return result
}

// SearchMerchantMenus matches category names, ignoring decorative emoji and spaces.
// Exact names take precedence over partial names; matching branches are merged.
func (s *Service) SearchMerchantMenus(keyword string, isAdmin, adminOnly bool) ([]uint64, error) {
	var items []model.MenuItem
	q := s.st.DB.Where("enabled=1")
	if adminOnly {
		q = q.Where("role='admin'")
	} else if !isAdmin {
		q = q.Where("role<>'admin'")
	}
	if err := q.Find(&items).Error; err != nil {
		return nil, err
	}
	return searchMerchantMenus(items, keyword), nil
}
func categoryKeyword(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, text)
}
func searchMerchantMenus(items []model.MenuItem, keyword string) []uint64 {
	keyword = categoryKeyword(keyword)
	if keyword == "" {
		return nil
	}
	exact, partial := []uint64{}, []uint64{}
	for _, item := range items {
		title := categoryKeyword(item.Title)
		if !strings.Contains(title, keyword) {
			continue
		}
		ids := merchantDescendants(items, item.ID)
		if title == keyword {
			exact = append(exact, ids...)
		} else {
			partial = append(partial, ids...)
		}
	}
	if len(exact) > 0 {
		partial = exact
	}
	result := []uint64{}
	seen := map[uint64]bool{}
	for _, id := range partial {
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}
