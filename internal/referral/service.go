package referral

import (
	"errors"
	"fmt"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	st   *store.Store
	rate decimal.Decimal
}

func New(st *store.Store, rate string) *Service {
	r, err := decimal.NewFromString(rate)
	if err != nil {
		r = decimal.Zero
	}
	return &Service{st: st, rate: r}
}
func (s *Service) BindByCode(userID uint64, code string) error {
	var u model.TelegramUser
	if err := s.st.DB.First(&u, userID).Error; err != nil {
		return err
	}
	if u.ParentUserID != nil {
		return fmt.Errorf("referrer already bound")
	}
	var p model.TelegramUser
	if err := s.st.DB.Where("referral_code=?", code).First(&p).Error; err != nil {
		return fmt.Errorf("invalid referral code")
	}
	if p.ID == u.ID {
		return fmt.Errorf("cannot refer yourself")
	}
	res := s.st.DB.Model(&model.TelegramUser{}).Where("id=? AND parent_user_id IS NULL", userID).Update("parent_user_id", p.ID)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return fmt.Errorf("referrer already bound")
	}
	return nil
}
func (s *Service) Stats(userID uint64) (count int64, total decimal.Decimal, err error) {
	if err = s.st.DB.Model(&model.TelegramUser{}).Where("parent_user_id=?", userID).Count(&count).Error; err != nil {
		return
	}
	var rows []model.ReferralCommission
	if err = s.st.DB.Where("agent_user_id=? AND status='credited'", userID).Find(&rows).Error; err != nil {
		return
	}
	total = decimal.Zero
	for _, r := range rows {
		d, _ := decimal.NewFromString(r.Amount)
		total = total.Add(d)
	}
	return
}
func (s *Service) AwardFromDeposit(sourceID, fromUserID uint64, depositAmount, currency string) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		// Lock the immutable source before inspecting either historical grants or current parent.
		var source model.DepositOrder
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&source, sourceID).Error; err != nil {
			return err
		}
		if source.UserID != fromUserID || source.Status != "credited" {
			return fmt.Errorf("invalid commission source")
		}
		depositAmount, currency = source.Amount, source.Currency
		var child model.TelegramUser
		if err := tx.First(&child, fromUserID).Error; err != nil {
			return err
		}
		if child.ParentUserID == nil || s.rate.LessThanOrEqual(decimal.Zero) {
			return nil
		}
		var n int64
		if err := tx.Model(&model.ReferralCommission{}).Where("source_type='deposit' AND source_id=?", sourceID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		amt, err := decimal.NewFromString(depositAmount)
		if err != nil {
			return err
		}
		commission := amt.Mul(s.rate).Truncate(6)
		if commission.LessThanOrEqual(decimal.Zero) {
			return nil
		}
		claim := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.ReferralGrant{DepositID: sourceID})
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return nil
		}
		var acc model.WalletAccount
		err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id=? AND currency=?", *child.ParentUserID, currency).First(&acc).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			acc = model.WalletAccount{UserID: *child.ParentUserID, Currency: currency, Available: "0", Frozen: "0"}
			if err = tx.Create(&acc).Error; err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		av, _ := decimal.NewFromString(acc.Available)
		av = av.Add(commission)
		acc.Available = av.StringFixed(6)
		if err := tx.Save(&acc).Error; err != nil {
			return err
		}
		rc := model.ReferralCommission{AgentUserID: *child.ParentUserID, FromUserID: fromUserID, SourceType: "deposit", SourceID: sourceID, Rate: s.rate.StringFixed(6), Amount: commission.StringFixed(6), Status: "credited"}
		if err := tx.Create(&rc).Error; err != nil {
			return err
		}
		return tx.Create(&model.WalletTransaction{UserID: *child.ParentUserID, Currency: currency, Type: "referral_commission", Amount: commission.StringFixed(6), Balance: av.StringFixed(6), RefType: "referral_commission", RefID: rc.ID, Remark: "deposit referral commission"}).Error
	})
}
