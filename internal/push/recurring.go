package push

import (
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"telegram-business-bot/internal/model"
	"time"
)

func validateRepeat(seconds int64, start *time.Time) error {
	if seconds == 0 {
		return nil
	}
	if seconds < 60 || seconds > 366*86400 {
		return errors.New("重复周期须为 60 秒至 366 天")
	}
	if start == nil || !start.After(time.Now()) {
		return errors.New("周期推送需要设置未来的首次发送时间")
	}
	return nil
}

// Preserve the original cadence, skipping missed occurrences instead of flooding users.
func nextOccurrence(start, now time.Time, seconds int64) time.Time {
	step := time.Duration(seconds) * time.Second
	if start.After(now) {
		return start
	}
	return start.Add((now.Sub(start)/step + 1) * step)
}

// Archive and rearm in one transaction: a crash can never reset delivery state
// without preserving the completed round, or enqueue the same round twice.
func (s *Service) finishRecurring(id uint64, now time.Time) error {
	return s.st.DB.Transaction(func(tx *gorm.DB) error {
		var c model.PushCampaign
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&c, id).Error; err != nil {
			return err
		}
		if c.Status != "processing" || c.RepeatSeconds == 0 {
			return nil
		}
		archive := c
		archive.ID = 0
		archive.ParentID = c.ID
		archive.RepeatSeconds = 0
		archive.Status = "completed"
		archive.CompletedAt = &now
		archive.CreatedAt = now
		archive.UpdatedAt = now
		if err := tx.Create(&archive).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.PushRecipient{}).Where("campaign_id=?", c.ID).Update("campaign_id", archive.ID).Error; err != nil {
			return err
		}
		anchor := now
		if c.ScheduledAt != nil {
			anchor = *c.ScheduledAt
		}
		next := nextOccurrence(anchor, now, c.RepeatSeconds)
		return tx.Model(&c).Updates(map[string]any{"status": "scheduled", "scheduled_at": next, "started_at": nil, "completed_at": nil, "total_count": 0, "sent_count": 0, "failed_count": 0, "last_error": ""}).Error
	})
}

func resumeUpdates(c model.PushCampaign, now time.Time) map[string]any {
	values := map[string]any{"status": resumeStatus(c, now), "paused_from": "", "last_error": ""}
	if c.RepeatSeconds > 0 && c.PausedFrom != "processing" && c.ScheduledAt != nil && !c.ScheduledAt.After(now) {
		values["scheduled_at"] = nextOccurrence(*c.ScheduledAt, now, c.RepeatSeconds)
		values["status"] = "scheduled"
	}
	return values
}
