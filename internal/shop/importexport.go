package shop

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
	"gorm.io/gorm"

	"telegram-business-bot/internal/model"
)

// Product spreadsheets have one row per SKU. Product columns are read from a product's first row;
// rows belong to the same product when they share 商品ID, or, for new products, 商家 and 商品名称.
var productColumns = []string{"商品ID", "商家", "分类", "商品名称", "类型", "配送方式", "规格", "价格", "库存", "活动价", "活动库存", "每人限购", "上架", "首页推荐", "排序", "图片", "商品详情"}

const maxImportRows = 2000

// ExportProducts writes products (one row per enabled SKU) as CSV with a UTF-8 BOM for Excel.
func (s *Service) ExportProducts(w io.Writer, merchantID uint64) error {
	products, err := s.AdminProducts(merchantID)
	if err != nil {
		return err
	}
	merchants := map[uint64]string{}
	var ms []model.Merchant
	if err := s.st.DB.Select("id", "name").Find(&ms).Error; err != nil {
		return err
	}
	for _, m := range ms {
		merchants[m.ID] = m.Name
	}
	cats := map[uint64]string{}
	var cs []model.ShopCategory
	if err := s.st.DB.Select("id", "name").Find(&cs).Error; err != nil {
		return err
	}
	for _, c := range cs {
		cats[c.ID] = c.Name
	}
	if _, err := w.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	_ = cw.Write(productColumns)
	yes := func(b bool) string {
		if b {
			return "是"
		}
		return "否"
	}
	for _, p := range products {
		kind, delivery := "实物", []string{}
		if p.Kind == "virtual" {
			kind = "虚拟"
		}
		if p.AllowExpress {
			delivery = append(delivery, "快递")
		}
		if p.AllowPickup {
			delivery = append(delivery, "自提")
		}
		images := strings.Join(strings.Fields(strings.ReplaceAll(p.Images, "\n", " ")), "|")
		for _, k := range p.SKUs {
			if !k.Enabled {
				continue
			}
			promo, promoStock := "", ""
			if amount(k.PromoPrice).IsPositive() {
				promo = amount(k.PromoPrice).String()
				if k.PromoStock != nil {
					promoStock = strconv.FormatInt(*k.PromoStock, 10)
				}
			}
			_ = cw.Write([]string{strconv.FormatUint(p.ID, 10), merchants[p.MerchantID], cats[p.CategoryID], p.Name, kind, strings.Join(delivery, ","), k.Name, amount(k.Price).String(), strconv.FormatInt(k.Stock, 10), promo, promoStock, strconv.FormatInt(p.LimitPerUser, 10), yes(p.Enabled), yes(p.Featured), strconv.Itoa(p.Sort), images, p.Description})
		}
	}
	cw.Flush()
	return cw.Error()
}

// ImportRow is one product found in an import, for the preview.
type ImportRow struct {
	Row      int    `json:"row"` // first spreadsheet row of the product (header is row 1)
	ID       uint64 `json:"id"`  // 0 for a new product
	Name     string `json:"name"`
	Merchant string `json:"merchant"`
	SKUs     int    `json:"skus"`
	Action   string `json:"action"` // create | update
	Error    string `json:"error,omitempty"`
}

type ImportResult struct {
	Rows    []ImportRow `json:"rows"`
	Creates int         `json:"creates"`
	Updates int         `json:"updates"`
	Errors  int         `json:"errors"`
	Applied bool        `json:"applied"`
}

type importItem struct {
	row     ImportRow
	product model.ShopProduct
	skus    []SKUInput
}

// decodeSpreadsheet accepts UTF-8 (with or without BOM) and the GBK/GB18030 encoding Excel uses
// for "CSV" on Chinese Windows.
func decodeSpreadsheet(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	if utf8.Valid(data) {
		return data, nil
	}
	return simplifiedchinese.GB18030.NewDecoder().Bytes(data)
}

func yesNo(v string, def bool) (bool, error) {
	switch strings.TrimSpace(v) {
	case "":
		return def, nil
	case "是", "Y", "y", "yes", "Yes", "1", "true", "TRUE":
		return true, nil
	case "否", "N", "n", "no", "No", "0", "false", "FALSE":
		return false, nil
	}
	return false, fmt.Errorf("「%s」应为 是 或 否", v)
}

// ImportProducts reads a product spreadsheet. With apply false it only validates and returns the
// preview; with apply true it saves every product in one transaction, or nothing if any row fails.
func (s *Service) ImportProducts(data []byte, merchantScope uint64, apply bool) (*ImportResult, error) {
	text, err := decodeSpreadsheet(data)
	if err != nil {
		return nil, userErr("无法识别文件编码，请另存为 CSV UTF-8 后重试")
	}
	r := csv.NewReader(bytes.NewReader(text))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, userErr("CSV 格式错误：%s", err.Error())
	}
	if len(records) < 2 {
		return nil, userErr("文件中没有商品数据")
	}
	if len(records)-1 > maxImportRows {
		return nil, userErr("一次最多导入 %d 行", maxImportRows)
	}
	col := map[string]int{}
	for i, h := range records[0] {
		col[strings.TrimSpace(h)] = i
	}
	for _, need := range []string{"商品名称", "分类", "规格", "价格", "库存"} {
		if _, ok := col[need]; !ok {
			return nil, userErr("缺少「%s」列，请先导出商品获取模板", need)
		}
	}
	if _, ok := col["商家"]; !ok && merchantScope == 0 {
		return nil, userErr("缺少「商家」列")
	}
	cell := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}

	var merchants []model.Merchant
	if err := s.st.DB.Select("id", "name").Find(&merchants).Error; err != nil {
		return nil, err
	}
	var cats []model.ShopCategory
	if err := s.st.DB.Select("id", "name").Find(&cats).Error; err != nil {
		return nil, err
	}
	findMerchant := func(v string) (uint64, string, error) {
		for _, m := range merchants {
			if m.Name == v || strconv.FormatUint(m.ID, 10) == v {
				return m.ID, m.Name, nil
			}
		}
		return 0, "", fmt.Errorf("商家「%s」不存在", v)
	}
	findCategory := func(v string) (uint64, error) {
		for _, c := range cats {
			if c.Name == v || strconv.FormatUint(c.ID, 10) == v {
				return c.ID, nil
			}
		}
		return 0, fmt.Errorf("分类「%s」不存在", v)
	}

	items := []*importItem{}
	byKey := map[string]*importItem{}
	fail := func(it *importItem, rowNo int, msg string) {
		if it.row.Error == "" {
			it.row.Error = fmt.Sprintf("第 %d 行：%s", rowNo, msg)
		}
	}
	for i, rec := range records[1:] {
		rowNo := i + 2
		if strings.Join(rec, "") == "" {
			continue
		}
		idText, merchantText, name := cell(rec, "商品ID"), cell(rec, "商家"), cell(rec, "商品名称")
		key := "id:" + idText
		if idText == "" {
			key = "new:" + merchantText + "\x00" + name
		}
		it := byKey[key]
		if it == nil {
			it = &importItem{row: ImportRow{Row: rowNo, Name: name, Action: "create"}}
			byKey[key] = it
			items = append(items, it)
			if err := s.importProductFields(it, rec, cell, idText, merchantText, merchantScope, findMerchant, findCategory); err != nil {
				fail(it, rowNo, err.Error())
			}
		}
		stock, err := strconv.ParseInt(cell(rec, "库存"), 10, 64)
		if err != nil {
			fail(it, rowNo, "库存需为整数")
		}
		sku := SKUInput{Name: cell(rec, "规格"), Price: cell(rec, "价格"), PromoPrice: cell(rec, "活动价"), Stock: stock}
		if v := cell(rec, "活动库存"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				fail(it, rowNo, "活动库存需为整数")
			}
			sku.PromoStock = &n
		}
		it.skus = append(it.skus, sku)
	}

	res := &ImportResult{Rows: []ImportRow{}}
	for _, it := range items {
		it.row.SKUs = len(it.skus)
		if it.row.Error == "" {
			if err := s.validateProduct(&it.product, it.skus, merchantScope); err != nil {
				fail(it, it.row.Row, err.Error())
			}
		}
		if it.row.Error != "" {
			res.Errors++
		} else if it.row.Action == "update" {
			res.Updates++
		} else {
			res.Creates++
		}
		res.Rows = append(res.Rows, it.row)
	}
	if !apply || res.Errors > 0 {
		return res, nil
	}
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		for _, it := range items {
			if err := s.saveProductTx(tx, &it.product, it.skus, merchantScope); err != nil {
				return userErr("第 %d 行（%s）：%s", it.row.Row, it.row.Name, err.Error())
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res.Applied = true
	return res, nil
}

// importProductFields fills the product of an import item from its first row. Updates keep the
// fields the spreadsheet does not carry (promo window, group buying).
func (s *Service) importProductFields(it *importItem, rec []string, cell func([]string, string) string, idText, merchantText string, merchantScope uint64, findMerchant func(string) (uint64, string, error), findCategory func(string) (uint64, error)) error {
	p := &it.product
	if idText != "" {
		id, err := strconv.ParseUint(idText, 10, 64)
		if err != nil {
			return fmt.Errorf("商品ID「%s」无效", idText)
		}
		q := s.st.DB.Where("id = ?", id)
		if merchantScope != 0 {
			q = q.Where("merchant_id = ?", merchantScope)
		}
		if err := q.First(p).Error; err != nil {
			return fmt.Errorf("商品ID %d 不存在", id)
		}
		it.row.ID, it.row.Action = id, "update"
	} else {
		p.Enabled, p.GroupHours, p.GroupRate = true, 24, "1"
	}
	if merchantScope != 0 {
		p.MerchantID = merchantScope
	} else if merchantText != "" || p.ID == 0 {
		id, name, err := findMerchant(merchantText)
		if err != nil {
			return err
		}
		p.MerchantID, it.row.Merchant = id, name
	}
	catID, err := findCategory(cell(rec, "分类"))
	if err != nil {
		return err
	}
	p.CategoryID = catID
	p.Name = cell(rec, "商品名称")
	origKind := p.Kind
	switch cell(rec, "类型") {
	case "":
		if p.ID == 0 {
			p.Kind = "physical"
		}
	case "实物":
		p.Kind = "physical"
	case "虚拟":
		p.Kind = "virtual"
	default:
		return fmt.Errorf("类型应为 实物 或 虚拟")
	}
	if p.ID != 0 && p.Kind != origKind {
		return fmt.Errorf("已创建的商品不能修改类型")
	}
	if d := cell(rec, "配送方式"); d != "" || p.ID == 0 {
		p.AllowExpress, p.AllowPickup = strings.Contains(d, "快递"), strings.Contains(d, "自提")
	}
	if v := cell(rec, "每人限购"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("每人限购需为整数")
		}
		p.LimitPerUser = n
	}
	if v := cell(rec, "排序"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("排序需为整数")
		}
		p.Sort = n
	}
	if p.Enabled, err = yesNo(cell(rec, "上架"), p.Enabled); err != nil {
		return fmt.Errorf("上架%s", err.Error())
	}
	if p.Featured, err = yesNo(cell(rec, "首页推荐"), p.Featured); err != nil {
		return fmt.Errorf("首页推荐%s", err.Error())
	}
	if v := cell(rec, "图片"); v != "" {
		p.Images = strings.Join(strings.FieldsFunc(v, func(r rune) bool { return r == '|' || r == '\n' }), "\n")
	}
	if v := cell(rec, "商品详情"); v != "" {
		p.Description = v
	}
	if p.Kind == "virtual" {
		p.AllowExpress, p.AllowPickup = false, false
	}
	it.row.Name = p.Name
	return nil
}
