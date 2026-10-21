// Package ratelimit counts events in fixed Redis windows, shared by every replica.
package ratelimit

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store is the subset of Redis the limiter needs; *redis.Client satisfies it.
type Store interface {
	Incr(ctx context.Context, key string) *redis.IntCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	Get(ctx context.Context, key string) *redis.StringCmd
}

// Hit records one event under key and returns the count in the current window. The window
// starts with the first event and lasts window.
func Hit(ctx context.Context, rdb Store, key string, window time.Duration) (int64, error) {
	n, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if n == 1 {
		if err := rdb.Expire(ctx, key, window).Err(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Count returns the events recorded under key in the current window.
func Count(ctx context.Context, rdb Store, key string) (int64, error) {
	n, err := rdb.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return n, err
}
