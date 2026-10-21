package shop

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
)

// After-sale types and statuses.
const (
	AfterSaleRefund = "refund" // money only
	AfterSaleReturn = "return" // goods sent back, then refunded

	AfterSalePending  = "pending"
	AfterSaleApproved = "approved" // return accepted, waiting for the parcel
	AfterSaleRefunded = "refunded"
	AfterSaleRejected = "rejected"
	AfterSaleCanceled = "canceled"
)

// afterSaleAllowed explains why an order cannot take a new after-sale request, or returns nil.
func (s *Service) afterSaleAllowed(o model.ShopOrder, now time.Time) error {
	if o.AfterSaleOpen {
		return userErr("该订单已有处理中的售后申请")
	}
	switch o.Status {
	case StatusShipped, StatusReady:
	case StatusCompleted:
		days := s.configInt("shop_aftersale_days", 7)
		if o.CompletedAt == nil || days == 0 || now.After(o.CompletedAt.AddDate(0, 0, days)) {
			return userErr("已超过售后申请期限")
		}
	case StatusPaid:
		return userErr("订单尚未发货，可直接取消订单")
	default:
		return userErr("当前订单状态不能申请售后")
	}
	if !amount(o.PayAmount).Sub(amount(o.RefundedAmount)).IsPositive() {
		return userErr("该订单已全额退款")
	}
	return nil
}

type AfterSaleRequest struct {
	OrderID uint64 `json:"order_id"`
	Type    string `json:"type"`
	Reason  string `json:"reason"`
	Amount  string `json:"amount"`
}

// ApplyAfterSale opens a refund or return request on one of the buyer's orders.
func (s *Service) ApplyAfterSale(userID uint64, req AfterSaleRequest) (*model.ShopAfterSale, error) {
	if req.Type != AfterSaleRefund && req.Type != AfterSaleReturn {
		return nil, userErr("请选择售后类型")
	}
	if err := checkLen(req.Reason, "售后原因", 512, true); err != nil {
		return nil, err
	}
	want, err := parseAmount(req.Amount, "退款金额")
	if err != nil {
		return nil, err
	}
	if !want.IsPositive() {
		return nil, userErr("退款金额必须大于 0")
	}
	var a model.ShopAfterSale
	var o model.ShopOrder
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if o, err = lockOrder(tx, req.OrderID, Scope{UserID: userID}); err != nil {
			return err
		}
		if err := s.afterSaleAllowed(o, time.Now()); err != nil {
			return err
		}
		if req.Type == AfterSaleReturn && o.DeliveryType == DeliveryVirtual {
			return userErr("虚拟商品只能申请仅退款")
		}
		left := amount(o.PayAmount).Sub(amount(o.RefundedAmount))
		if want.GreaterThan(left) {
			return userErr("退款金额不能超过 %s USDT", left.String())
		}
		a = model.ShopAfterSale{OrderID: o.ID, OrderNo: o.OrderNo, UserID: o.UserID, TelegramUserID: o.TelegramUserID, MerchantID: o.MerchantID, Type: req.Type, Reason: strings.TrimSpace(req.Reason), Amount: want.StringFixed(6), RefundAmount: "0", Status: AfterSalePending}
		if err := tx.Create(&a).Error; err != nil {
			return err
		}
		return tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Update("after_sale_open", true).Error
	})
	if err != nil {
		return nil, err
	}
	s.notifyStaff(o.MerchantID, fmt.Sprintf("🧾 <b>新的售后申请</b>\n\n订单号：<code>%s</code>\n商家：%s\n类型：%s\n申请退款：<code>%s USDT</code>\n原因：%s", o.OrderNo, htmlEscape(o.MerchantName), afterSaleNames[a.Type], want.String(), htmlEscape(a.Reason)))
	return &a, nil
}

var afterSaleNames = map[string]string{AfterSaleRefund: "仅退款", AfterSaleReturn: "退货退款"}

// lockAfterSale loads an after-sale request for update within the scope.
func lockAfterSale(tx *gorm.DB, id uint64, scope Scope) (model.ShopAfterSale, error) {
	var a model.ShopAfterSale
	q := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id)
	if scope.UserID != 0 {
		q = q.Where("user_id = ?", scope.UserID)
	}
	if scope.MerchantID != 0 {
		q = q.Where("merchant_id = ?", scope.MerchantID)
	}
	if err := q.First(&a).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return a, userErr("售后申请不存在")
		}
		return a, err
	}
	return a, nil
}

// close ends a request and reopens the order for new requests.
func closeAfterSale(tx *gorm.DB, a model.ShopAfterSale, updates map[string]any) error {
	updates["closed_at"] = time.Now()
	if err := tx.Model(&model.ShopAfterSale{}).Where("id = ?", a.ID).Updates(updates).Error; err != nil {
		return err
	}
	return tx.Model(&model.ShopOrder{}).Where("id = ?", a.OrderID).Update("after_sale_open", false).Error
}

// CancelAfterSale lets the buyer withdraw a request that has not been refunded.
func (s *Service) CancelAfterSale(userID, id uint64) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		a, err := lockAfterSale(tx, id, Scope{UserID: userID})
		if err != nil {
			return err
		}
		if a.Status != AfterSalePending && a.Status != AfterSaleApproved {
			return userErr("该售后申请已结束")
		}
		return closeAfterSale(tx, a, map[string]any{"status": AfterSaleCanceled})
	})
}

// SubmitReturn records the buyer's return parcel for an approved return request.
func (s *Service) SubmitReturn(userID, id uint64, carrier, trackingNo string) error {
	if err := checkLen(carrier, "快递公司", 64, true); err != nil {
		return err
	}
	if err := checkLen(trackingNo, "快递单号", 128, true); err != nil {
		return err
	}
	var a model.ShopAfterSale
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if a, err = lockAfterSale(tx, id, Scope{UserID: userID}); err != nil {
			return err
		}
		if a.Type != AfterSaleReturn || a.Status != AfterSaleApproved {
			return userErr("当前售后状态不需要填写退货单号")
		}
		return tx.Model(&model.ShopAfterSale{}).Where("id = ?", a.ID).Updates(map[string]any{"return_carrier": strings.TrimSpace(carrier), "return_tracking": strings.TrimSpace(trackingNo)}).Error
	})
	if err == nil {
		s.notifyStaff(a.MerchantID, fmt.Sprintf("📦 <b>买家已寄回退货</b>\n\n订单号：<code>%s</code>\n快递：%s\n单号：<code>%s</code>\n收到货后请在后台确认退款。", a.OrderNo, htmlEscape(carrier), htmlEscape(trackingNo)))
	}
	return err
}

// MyAfterSales lists the buyer's requests, newest first.
func (s *Service) MyAfterSales(userID uint64) ([]model.ShopAfterSale, error) {
	rows := []model.ShopAfterSale{}
	err := s.st.DB.Where("user_id = ?", userID).Order("id desc").Limit(100).Find(&rows).Error
	return rows, err
}

// ---- Administration ----

// AdminAfterSales lists requests, optionally by status, limited to a merchant when scoped.
func (s *Service) AdminAfterSales(status string, merchantID uint64) ([]model.ShopAfterSale, error) {
	db := s.st.DB.Model(&model.ShopAfterSale{})
	if status != "" {
		db = db.Where("status = ?", status)
	}
	if merchantID != 0 {
		db = db.Where("merchant_id = ?", merchantID)
	}
	rows := []model.ShopAfterSale{}
	err := db.Order("id desc").Limit(500).Find(&rows).Error
	return rows, err
}

// ApproveReturn accepts a return request and tells the buyer where to send the goods.
func (s *Service) ApproveReturn(id uint64, scope Scope, address, note, operator string) error {
	if err := checkLen(note, "处理说明", 512, false); err != nil {
		return err
	}
	var a model.ShopAfterSale
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if a, err = lockAfterSale(tx, id, scope); err != nil {
			return err
		}
		if a.Type != AfterSaleReturn || a.Status != AfterSalePending {
			return userErr("只有待处理的退货申请可以同意退货")
		}
		address = strings.TrimSpace(address)
		if address == "" {
			address = s.configString("shop_return_address")
		}
		if address == "" {
			var m model.Merchant
			if tx.Select("address").First(&m, a.MerchantID).Error == nil {
				address = m.Address
			}
		}
		if err := checkLen(address, "退货地址", 512, true); err != nil {
			return err
		}
		a.ReturnAddress = address
		return tx.Model(&model.ShopAfterSale{}).Where("id = ?", a.ID).Updates(map[string]any{"status": AfterSaleApproved, "return_address": address, "admin_note": strings.TrimSpace(note), "handled_by": operator}).Error
	})
	if err == nil {
		s.notifyBuyerOf(a, fmt.Sprintf("📦 <b>退货申请已通过</b>\n\n订单号：<code>%s</code>\n请将商品寄回：%s\n寄出后在商城「订单详情」中填写快递单号。", a.OrderNo, htmlEscape(a.ReturnAddress)))
	}
	return err
}

// RefundAfterSale pays a refund for a pending refund-only request or an approved return, then closes
// it. The amount can be lower than requested but never more than what is left on the order.
func (s *Service) RefundAfterSale(id uint64, scope Scope, amountText, note, operator string) error {
	pay, err := parseAmount(amountText, "退款金额")
	if err != nil {
		return err
	}
	if !pay.IsPositive() {
		return userErr("退款金额必须大于 0")
	}
	if err := checkLen(note, "处理说明", 512, false); err != nil {
		return err
	}
	var a model.ShopAfterSale
	err = s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if a, err = lockAfterSale(tx, id, scope); err != nil {
			return err
		}
		ready := (a.Type == AfterSaleRefund && a.Status == AfterSalePending) || (a.Type == AfterSaleReturn && a.Status == AfterSaleApproved)
		if !ready {
			return userErr("当前售后状态不能退款")
		}
		o, err := lockOrder(tx, a.OrderID, Scope{})
		if err != nil {
			return err
		}
		refunded := amount(o.RefundedAmount)
		left := amount(o.PayAmount).Sub(refunded)
		if pay.GreaterThan(left) {
			return userErr("退款金额不能超过订单剩余可退 %s USDT", left.String())
		}
		if err := s.wallet.Refund(tx, o.UserID, pay, "shop_aftersale", a.ID, "商城售后退款 "+o.OrderNo); err != nil {
			return err
		}
		if err := tx.Model(&model.ShopOrder{}).Where("id = ?", o.ID).Update("refunded_amount", refunded.Add(pay).StringFixed(6)).Error; err != nil {
			return err
		}
		// Refunds before completion already lower the sale entry; later ones reverse it.
		if o.Status == StatusCompleted {
			if err := s.recordRefund(tx, o, a.ID, pay); err != nil {
				return err
			}
		}
		a.RefundAmount = pay.StringFixed(6)
		return closeAfterSale(tx, a, map[string]any{"status": AfterSaleRefunded, "refund_amount": a.RefundAmount, "admin_note": strings.TrimSpace(note), "handled_by": operator})
	})
	if err == nil {
		s.notifyBuyerOf(a, fmt.Sprintf("💰 <b>售后退款已到账</b>\n\n订单号：<code>%s</code>\n退款：<code>%s USDT</code> 已退回钱包余额。", a.OrderNo, pay.String()))
	}
	return err
}

// RejectAfterSale closes a pending or approved request without refunding.
func (s *Service) RejectAfterSale(id uint64, scope Scope, note, operator string) error {
	if err := checkLen(note, "拒绝原因", 512, true); err != nil {
		return err
	}
	var a model.ShopAfterSale
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if a, err = lockAfterSale(tx, id, scope); err != nil {
			return err
		}
		if a.Status != AfterSalePending && a.Status != AfterSaleApproved {
			return userErr("该售后申请已结束")
		}
		return closeAfterSale(tx, a, map[string]any{"status": AfterSaleRejected, "admin_note": strings.TrimSpace(note), "handled_by": operator})
	})
	if err == nil {
		s.notifyBuyerOf(a, fmt.Sprintf("❌ <b>售后申请未通过</b>\n\n订单号：<code>%s</code>\n原因：%s", a.OrderNo, htmlEscape(strings.TrimSpace(note))))
	}
	return err
}

// notifyBuyerOf messages the buyer through the bot used for the original order.
func (s *Service) notifyBuyerOf(a model.ShopAfterSale, html string) {
	var o model.ShopOrder
	if s.st.DB.Select("bot_id").First(&o, a.OrderID).Error != nil {
		return
	}
	s.notify(o.BotID, a.TelegramUserID, html)
}
