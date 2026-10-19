package admin

import (
	"database/sql/driver"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http"
	"net/http/httptest"
	"strings"
	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"testing"
)

func TestCreateDisabledSubmenuChild(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "code=?", Columns: []string{"id"}},
		testutil.Step{Contains: "menu_items", Columns: []string{"id", "parent_id", "action_type"}, Rows: [][]driver.Value{{int64(5), int64(0), "submenu"}}},
		testutil.Step{Contains: "INSERT INTO `menu_items`", Affected: 1},
	)
	db.Callback().Create().Before("gorm:create").Register("test:child", func(tx *gorm.DB) {
		m := tx.Statement.Dest.(*model.MenuItem)
		if m.Enabled || m.ParentID != 5 {
			t.Errorf("lost child settings: %+v", m)
		}
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{st: &store.Store{DB: db}}
	r.POST("/menus", h.saveMenu)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/menus", strings.NewReader(`{"code":"child","title":"Child","parent_id":5,"enabled":false,"action_type":"builtin","action_value":"wallet"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestReplyMenuValidation(t *testing.T) {
	for _, body := range []string{`{"code":"food","title":"/start","placement":"reply"}`, `{"code":"food","title":"↩️ 返回主菜单","placement":"reply"}`, `{"code":"food","title":"美食","placement":"wrong"}`} {
		gin.SetMode(gin.TestMode)
		r := gin.New()
		h := &Handler{}
		r.POST("/menus", h.saveMenu)
		w := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/menus", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
func TestSaveReplyMenuPersistsUniqueTitleAndLayout(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "code=?", Columns: []string{"id"}}, testutil.Step{Contains: "INSERT INTO `menu_items`", Affected: 1})
	db.Callback().Create().Before("gorm:create").Register("test:reply-menu", func(tx *gorm.DB) {
		m := tx.Statement.Dest.(*model.MenuItem)
		if m.Placement != "reply" || m.ReplyTitle == nil || *m.ReplyTitle != "🍴 美食" || m.RowNo != 1 || m.ActionType != "text" {
			t.Fatalf("%+v", m)
		}
	})
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := &Handler{st: &store.Store{DB: db}}
	r.POST("/menus", h.saveMenu)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/menus", strings.NewReader(`{"code":"food","title":"🍴 美食","placement":"reply","row_no":1,"action_type":"text","description":"餐厅列表","enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestSaveReplyChildMerchantMenu(t *testing.T) {
	db := testutil.DB(t, testutil.Step{Contains: "code=?", Columns: []string{"id"}}, testutil.Step{Contains: "FOR UPDATE", Columns: []string{"id", "action_type"}, Rows: [][]driver.Value{{int64(12), "submenu"}}}, testutil.Step{Contains: "INSERT INTO `menu_items`", Affected: 1})
	db.Callback().Create().Before("gorm:create").Register("test:reply-child", func(tx *gorm.DB) {
		m := tx.Statement.Dest.(*model.MenuItem)
		if m.ParentID != 12 || m.Placement != "reply" || m.ActionType != "merchants" {
			t.Fatal(m)
		}
	})
	w := merchantRequest(&Handler{st: &store.Store{DB: db}}, "/menus", `{"code":"hotpot","title":"火锅","parent_id":12,"placement":"reply","action_type":"merchants","enabled":true}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPhotoMenuAcceptsUploadedHTTPAndHTTPSImages(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		db := testutil.DB(t, testutil.Step{Contains: "code=?", Columns: []string{"id"}}, testutil.Step{Contains: "INSERT INTO `menu_items`", Affected: 1})
		body := `{"code":"contact","title":"客服","action_type":"photo","image_url":"` + scheme + `://up.example.com/uploads/contact.jpg","button_text":"联系客服","action_value":"https://t.me/contact"}`
		w := merchantRequest(&Handler{st: &store.Store{DB: db}}, "/menus", body)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}
func TestPhotoMenuRejectsNonWebImageURLs(t *testing.T) {
	for _, imageURL := range []string{"file:///tmp/contact.jpg", "ftp://example.com/a.jpg", "https:///a.jpg"} {
		w := merchantRequest(&Handler{}, "/menus", `{"code":"contact","title":"客服","action_type":"photo","image_url":"`+imageURL+`","button_text":"联系客服","action_value":"https://t.me/contact"}`)
		if w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
}
