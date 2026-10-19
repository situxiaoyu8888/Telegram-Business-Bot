package menu

import (
	"database/sql/driver"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func TestHiddenOrAdminParentBlocksDirectChildCallback(t *testing.T) {
	for _, v := range []struct {
		enabled bool
		role    string
	}{{false, "user"}, {true, "admin"}} {
		db := testutil.DB(t, testutil.Step{Contains: "menu_items", Columns: []string{"id", "parent_id", "enabled", "role"}, Rows: [][]driver.Value{{int64(1), int64(0), v.enabled, v.role}}})
		s := New(&store.Store{DB: db})
		if err := s.checkAccess(model.MenuItem{ID: 2, ParentID: 1, Enabled: true, Role: "user"}, false); err == nil {
			t.Fatal("hidden parent bypassed")
		}
	}
}

func TestReplyMenusFilterRoleAndMatchExactTitle(t *testing.T) {
	for _, tt := range []struct {
		admin, only bool
		sql         string
	}{{false, false, "role<>'admin'"}, {true, true, "role='admin'"}, {true, false, "placement=?"}} {
		db := testutil.DB(t, testutil.Step{Contains: tt.sql, Columns: []string{"id", "title", "placement", "enabled"}, Rows: [][]driver.Value{{int64(1), "🍴 美食", "reply", true}}})
		item, err := New(&store.Store{DB: db}).ReplyByTitle("🍴 美食", tt.admin, tt.only)
		if err != nil || item == nil || item.ID != 1 {
			t.Fatal(item, err)
		}
	}
}
func TestReplyMenuRejectsAmbiguousTitles(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "placement=?", Columns: []string{"id", "title"}, Rows: [][]driver.Value{{int64(1), "菜单"}, {int64(2), "菜单"}}})
	if _, err := New(&store.Store{DB: db}).ReplyByTitle("菜单", true, false); err == nil {
		t.Fatal("ambiguous menu dispatched")
	}
}
func TestInlineRootExcludesReplyKeyboard(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "placement<>?", Columns: []string{"id"}})
	if _, err := New(&store.Store{DB: db}).Root(false); err != nil {
		t.Fatal(err)
	}
}

func TestNestedReplyTitleChecksAncestorVisibility(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		db := testutil.DB(t, testutil.Step{Contains: "title=?", Columns: []string{"id", "parent_id", "title", "enabled", "placement"}, Rows: [][]driver.Value{{int64(2), int64(1), "火锅", true, "reply"}}}, testutil.Step{Contains: "menu_items", Columns: []string{"id", "enabled", "role"}, Rows: [][]driver.Value{{int64(1), enabled, "user"}}})
		item, err := New(&store.Store{DB: db}).ReplyByTitle("火锅", false, false)
		if enabled && (err != nil || item == nil) {
			t.Fatal(item, err)
		}
		if !enabled && err == nil {
			t.Fatal("hidden ancestor bypassed")
		}
	}
}

func TestRecommendationCategoriesDoNotEscapeVisibleSubtree(t *testing.T) {
	items := []model.MenuItem{{ID: 1, ActionType: "submenu"}, {ID: 2, ParentID: 1, ActionType: "merchants"}, {ID: 3, ParentID: 1, ActionType: "submenu"}, {ID: 4, ParentID: 3, ActionType: "merchants"}, {ID: 5, ParentID: 99, ActionType: "merchants"}, {ID: 6, ParentID: 88, ActionType: "merchants"}, {ID: 7, ParentID: 1, ActionType: "text"}, {ID: 8, ParentID: 7, ActionType: "merchants"}}
	ids := merchantDescendants(items, 1)
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 4 {
		t.Fatal(ids)
	}
	if len(merchantDescendants(items, 5)) != 0 {
		t.Fatal("recommendations bypass missing ancestor")
	}
	if len(merchantDescendants(items, 99)) != 0 {
		t.Fatal("missing/invisible parent accepted")
	}
}

func TestSearchMerchantMenusMatchesDecoratedNamesAndVisibleBranches(t *testing.T) {
	items := []model.MenuItem{
		{ID: 1, Title: "美食", ActionType: "submenu"},
		{ID: 2, ParentID: 1, Title: "🍢 烧烤", ActionType: "merchants"},
		{ID: 3, ParentID: 1, Title: "韩式烧烤", ActionType: "merchants"},
		{ID: 4, ParentID: 99, Title: "烧烤", ActionType: "merchants"},
	}
	for _, tc := range []struct {
		keyword string
		want    []uint64
	}{
		{"烧烤", []uint64{2}}, {"韩式", []uint64{3}}, {"美食", []uint64{2, 3}}, {"不存在", nil}, {"🍢", nil},
	} {
		got := searchMerchantMenus(items, tc.keyword)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %v", tc.keyword, got)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: %v", tc.keyword, got)
			}
		}
	}
}
