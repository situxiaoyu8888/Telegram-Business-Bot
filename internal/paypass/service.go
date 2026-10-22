// Package paypass keeps users' payment passwords: 6 digits, bcrypt-hashed, required before any
// spending from the wallet (red packets, withdrawals, shop orders, exchanges). Five wrong
// attempts in a row lock the password for 30 minutes; administrators can reset it.
package paypass

import (
	"errors"
	"fmt"
	"regexp"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"telegram-business-bot/internal/model"
	"telegram-business-bot/internal/store"
)

const (
	MaxAttempts = 5
	LockFor     = 30 * time.Minute
)

// UserError carries a message meant for the person who made the request.
type UserError struct{ Msg string }

func (e UserError) Error() string { return e.Msg }

func userErr(format string, args ...any) error { return UserError{fmt.Sprintf(format, args...)} }

// ErrNotSet means the user has no payment password yet.
var ErrNotSet = UserError{"请先设置支付密码"}

var sixDigits = regexp.MustCompile(`^\d{6}$`)

type Service struct {
	st  *store.Store
	now func() time.Time
}

func New(st *store.Store) *Service { return &Service{st: st, now: time.Now} }

// Check validates a new password: six digits, not all the same and not a plain sequence.
func Check(pin string) error {
	if !sixDigits.MatchString(pin) {
		return userErr("支付密码必须是 6 位数字")
	}
	same, up, down := true, true, true
	for i := 1; i < len(pin); i++ {
		d := int(pin[i]) - int(pin[i-1])
		same, up, down = same && d == 0, up && d == 1, down && d == -1
	}
	if same || up || down {
		return userErr("支付密码太简单（不能是相同或连续的数字），请换一个")
	}
	return nil
}

// IsSet reports whether the user has a payment password.
func (s *Service) IsSet(userID uint64) (bool, error) {
	var n int64
	err := s.st.DB.Model(&model.PaymentPassword{}).Where("user_id = ?", userID).Count(&n).Error
	return n > 0, err
}

// Set creates the user's first payment password; it fails when one exists (use Change).
func (s *Service) Set(userID uint64, pin string) error {
	if err := Check(pin); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	err = s.st.DB.Create(&model.PaymentPassword{UserID: userID, Hash: string(hash)}).Error
	if store.IsDuplicate(err) {
		return userErr("已设置过支付密码；如需修改，请先验证原密码")
	}
	return err
}

// Change replaces the password after verifying the old one.
func (s *Service) Change(userID uint64, old, pin string) error {
	if err := Check(pin); err != nil {
		return err
	}
	if err := s.Verify(userID, old); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.st.DB.Model(&model.PaymentPassword{}).Where("user_id = ?", userID).Updates(map[string]any{"hash": string(hash), "failed": 0, "locked_until": nil}).Error
}

// Replace sets a new password for a user who has just verified the current one.
func (s *Service) Replace(userID uint64, pin string) error {
	if err := Check(pin); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(pin), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.st.DB.Model(&model.PaymentPassword{}).Where("user_id = ?", userID).Updates(map[string]any{"hash": string(hash), "failed": 0, "locked_until": nil}).Error
}

// Verify checks a password, counting wrong attempts and enforcing the lock. The attempt
// counter is committed even when the password is wrong.
func (s *Service) Verify(userID uint64, pin string) error {
	var result error
	err := s.st.DB.Transaction(func(tx *gorm.DB) error {
		var p model.PaymentPassword
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", userID).First(&p).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = ErrNotSet
			return nil
		}
		if err != nil {
			return err
		}
		now := s.now()
		if p.LockedUntil != nil && now.Before(*p.LockedUntil) {
			result = userErr("支付密码输错次数过多，已锁定，请 %d 分钟后再试", int(p.LockedUntil.Sub(now).Minutes())+1)
			return nil
		}
		if sixDigits.MatchString(pin) && bcrypt.CompareHashAndPassword([]byte(p.Hash), []byte(pin)) == nil {
			if p.Failed != 0 || p.LockedUntil != nil {
				return tx.Model(&model.PaymentPassword{}).Where("user_id = ?", userID).Updates(map[string]any{"failed": 0, "locked_until": nil}).Error
			}
			return nil
		}
		failed := p.Failed + 1
		updates := map[string]any{"failed": failed}
		result = userErr("支付密码不正确，还可以再试 %d 次", MaxAttempts-failed)
		if failed >= MaxAttempts {
			updates["failed"], updates["locked_until"] = 0, now.Add(LockFor)
			result = errLocked
		}
		return tx.Model(&model.PaymentPassword{}).Where("user_id = ?", userID).Updates(updates).Error
	})
	if err != nil {
		return err
	}
	return result
}

var errLocked = UserError{fmt.Sprintf("支付密码连续输错 %d 次，已锁定 %d 分钟", MaxAttempts, int(LockFor.Minutes()))}

// Reset removes the user's password (console); they set a new one on their next payment.
func (s *Service) Reset(userID uint64) error {
	return s.st.DB.Where("user_id = ?", userID).Delete(&model.PaymentPassword{}).Error
}

// IsUserError reports whether err carries a message meant for people.
func IsUserError(err error) bool {
	var u UserError
	return errors.As(err, &u)
}
