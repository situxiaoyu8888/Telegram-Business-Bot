package integration

import (
	"testing"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/shop"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/wallet"
)

func TestShopDirectoryMirrorsMenus(t *testing.T) {
	db := testDatabase(t)
	st := &store.Store{DB: db}
	svc := shop.New(st, wallet.New(st, "USDT", "1", "0"), nil, nil)
	menu := func(parent uint64, code, title, action, role string, enabled bool, sort int) model.MenuItem {
		m := model.MenuItem{ParentID: parent, Code: code, Title: title, ActionType: action, Role: role, Enabled: true, Sort: sort, Placement: "inline"}
		must(t, db.Create(&m).Error)
		if !enabled {
			must(t, db.Model(&m).Update("enabled", false).Error)
		}
		return m
	}
	food := menu(0, "food", "美食", "submenu", "user", true, 1)
	chinese := menu(food.ID, "chinese", "中餐", "merchants", "user", true, 1)
	western := menu(food.ID, "western", "西餐", "submenu", "user", true, 2)
	steak := menu(western.ID, "steak", "牛排", "merchants", "user", true, 1)
	life := menu(0, "life", "生活", "submenu", "user", true, 2)
	emptyCat := menu(life.ID, "laundry", "洗衣", "merchants", "user", true, 1)
	hidden := menu(0, "staff", "内部", "submenu", "admin", true, 3)
	hiddenCat := menu(hidden.ID, "staffcat", "内部商家", "merchants", "user", true, 1)
	off := menu(food.ID, "closed", "停用分类", "merchants", "user", false, 3)
	menu(0, "wallet", "钱包", "builtin", "user", true, 4)

	merchant := func(menuID uint64, name string, enabled, pinned bool, sort int) model.Merchant {
		m := model.Merchant{MenuID: menuID, Name: name, Enabled: true, Pinned: pinned, Sort: sort}
		must(t, db.Create(&m).Error)
		if !enabled {
			must(t, db.Model(&m).Update("enabled", false).Error)
		}
		return m
	}
	a := merchant(chinese.ID, "川味馆", true, false, 1)
	b := merchant(chinese.ID, "粤菜馆", true, true, 2)
	merchant(chinese.ID, "已下线", false, false, 0)
	c := merchant(steak.ID, "牛排屋", true, false, 0)
	merchant(emptyCat.ID, "停业洗衣", false, false, 0)
	secret := merchant(hiddenCat.ID, "内部商家", true, false, 0)
	merchant(off.ID, "停用分类商家", true, false, 0)
	must(t, db.Create(&model.ShopProduct{MerchantID: a.ID, Name: "宫保鸡丁", Kind: "physical", Enabled: true}).Error)

	dir, err := svc.Directory()
	must(t, err)
	// Only 美食 has visible merchants: 生活 has none enabled, 内部 is admin-only, 钱包 is not a submenu.
	if len(dir) != 1 || dir[0].Title != "美食" || len(dir[0].Sections) != 2 {
		t.Fatalf("%+v", dir)
	}
	cn, st2 := dir[0].Sections[0], dir[0].Sections[1]
	if cn.Title != "中餐" || len(cn.Merchants) != 2 || cn.Merchants[0].ID != b.ID || cn.Merchants[1].ID != a.ID || cn.Merchants[1].ProductCount != 1 {
		t.Fatalf("pinned first, disabled left out, products counted: %+v", cn)
	}
	if st2.Title != "西餐 · 牛排" || len(st2.Merchants) != 1 || st2.Merchants[0].ID != c.ID {
		t.Fatalf("nested category: %+v", st2)
	}
	if m, err := svc.Merchant(a.ID); err != nil || m.Name != "川味馆" {
		t.Fatal(m, err)
	}
	if _, err := svc.Merchant(secret.ID); err == nil {
		t.Fatal("merchant under an admin-only menu is visible")
	}
}
