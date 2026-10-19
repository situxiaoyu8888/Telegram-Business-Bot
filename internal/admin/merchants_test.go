package admin

import (
	"database/sql/driver"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http/httptest"
	"strings"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func merchantRequest(h *Handler, path, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/merchants", h.saveMerchant)
	r.DELETE("/menus/:id", h.deleteMenu)
	r.POST("/menus", h.saveMenu)
	method := "POST"
	if strings.HasPrefix(path, "/menus/") {
		method = "DELETE"
	}
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}
func TestMerchantValidation(t *testing.T) {
	for _, m := range []model.Merchant{{Name: "店铺"}, {MenuID: 1}, {Name: "店铺", MenuID: 1, ContactURL: "javascript:alert(1)"}, {Name: "店铺", MenuID: 1, Description: strings.Repeat("字", 1001)}} {
		if validateMerchant(&m) == nil {
			t.Fatal("invalid merchant accepted", m)
		}
	}
	m := model.Merchant{Name: " 店铺 ", MenuID: 1, ContactURL: "https://t.me/shop"}
	if err := validateMerchant(&m); err != nil || m.Name != "店铺" {
		t.Fatal(err)
	}
}
func TestMerchantMustBelongToDirectoryChild(t *testing.T) {
	for _, v := range []struct {
		parent int64
		kind   string
	}{{0, "merchants"}, {1, "text"}} {
		db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "parent_id", "action_type"}, Rows: [][]driver.Value{{int64(2), v.parent, v.kind}}})
		w := merchantRequest(&Handler{st: &store.Store{DB: db}}, "/merchants", `{"name":"店铺","menu_id":2,"enabled":true}`)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
func TestMerchantUpdatePreservesDisabledStateAndAssociation(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "parent_id", "action_type"}, Rows: [][]driver.Value{{int64(2), int64(1), "merchants"}}}, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "menu_id", "pinned"}, Rows: [][]driver.Value{{int64(9), int64(3), true}}}, testutil.Step{Contains: "UPDATE `merchants`", Affected: 1})
	db.Callback().Update().Before("gorm:update").Register("test:merchant", func(tx *gorm.DB) {
		m := tx.Statement.Dest.(*model.Merchant)
		if m.Pinned || m.Enabled || m.MenuID != 2 || m.Sort != 0 {
			t.Fatal(m)
		}
	})
	w := merchantRequest(&Handler{st: &store.Store{DB: db}}, "/merchants", `{"id":9,"name":"店铺","menu_id":2,"enabled":false,"pinned":false,"sort":0}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestCannotDeleteCategoryWithMerchants(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "action_type"}, Rows: [][]driver.Value{{int64(2), "merchants"}}}, testutil.Step{Contains: "FROM `merchants`", Columns: []string{"count"}, Rows: [][]driver.Value{{int64(1)}}})
	w := merchantRequest(&Handler{st: &store.Store{DB: db}}, "/menus/2", "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "商家") {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestCannotChangeCategoryTypeWithMerchants(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "action_type"}, Rows: [][]driver.Value{{int64(2), "merchants"}}}, testutil.Step{Contains: "FROM `merchants`", Columns: []string{"count"}, Rows: [][]driver.Value{{int64(1)}}})
	w := merchantRequest(&Handler{st: &store.Store{DB: db}}, "/menus", `{"id":2,"code":"food","title":"美食","action_type":"text","parent_id":1}`)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestCannotChangeParentTypeWithChildren(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "action_type"}, Rows: [][]driver.Value{{int64(1), "submenu"}}}, testutil.Step{Contains: "parent_id=?", Columns: []string{"count"}, Rows: [][]driver.Value{{int64(1)}}})
	w := merchantRequest(&Handler{st: &store.Store{DB: db}}, "/menus", `{"id":1,"code":"food","title":"美食","action_type":"text"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "子菜单") {
		t.Fatal(w.Code, w.Body.String())
	}
}
