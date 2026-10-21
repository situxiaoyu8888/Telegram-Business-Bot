package shop

import (
	"context"
	"time"

	"go.uber.org/zap"

	"telegram-business-bot/internal/lease"
	"telegram-business-bot/internal/model"
)

// workerInterval is short enough that expired group-buying groups are refunded promptly.
const workerInterval = 2 * time.Minute

// Run auto-completes express orders that were shipped shop_auto_complete_days ago and then pays
// due referral commissions (see GrantCommissions). Pickup orders
// are left alone because only the merchant knows whether the goods were collected. A Redis
// lease keeps several replicas from running the same pass.
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(workerInterval)
	defer ticker.Stop()
	for {
		if err := lease.Run(ctx, s.st.Redis, "shop:auto-complete", func(ctx context.Context) error {
			if err := s.AutoComplete(ctx, time.Now()); err != nil {
				return err
			}
			if err := s.ExpireGroups(ctx, time.Now()); err != nil {
				return err
			}
			if err := s.maybeSendDigest(ctx, time.Now()); err != nil {
				return err
			}
			return s.GrantCommissions(ctx, time.Now())
		}); err != nil && err != lease.ErrBusy && ctx.Err() == nil && s.logger != nil {
			s.logger.Warn("shop background pass failed", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// AutoComplete completes shipped express orders older than the configured number of days.
// Orders with an open after-sale request wait until it is resolved.
func (s *Service) AutoComplete(ctx context.Context, now time.Time) error {
	days := s.configInt("shop_auto_complete_days", 7)
	if days == 0 {
		return nil
	}
	var ids []uint64
	if err := s.st.DB.WithContext(ctx).Model(&model.ShopOrder{}).
		Where("status = ? AND delivery_type = ? AND after_sale_open = ? AND shipped_at < ?", StatusShipped, DeliveryExpress, false, now.AddDate(0, 0, -days)).
		Order("id asc").Limit(500).Pluck("id", &ids).Error; err != nil {
		return err
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A buyer may confirm or open an after-sale concurrently; complete re-checks under a lock.
		if err := s.complete(id, Scope{}, autoOperator); err != nil && !IsUserError(err) {
			return err
		}
	}
	return nil
}
