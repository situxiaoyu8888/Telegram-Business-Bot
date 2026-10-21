package ratelimit

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// Memory is an in-process Store for tests.
type Memory struct {
	n   map[string]int64
	ttl map[string]time.Duration
}

func (m *Memory) Incr(_ context.Context, key string) *redis.IntCmd {
	m.n[key]++
	return redis.NewIntResult(m.n[key], nil)
}
func (m *Memory) Expire(_ context.Context, key string, d time.Duration) *redis.BoolCmd {
	m.ttl[key] = d
	return redis.NewBoolResult(true, nil)
}
func (m *Memory) Get(_ context.Context, key string) *redis.StringCmd {
	v, ok := m.n[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(strconv.FormatInt(v, 10), nil)
}

func TestHitSetsWindowOnFirstEvent(t *testing.T) {
	m := &Memory{n: map[string]int64{}, ttl: map[string]time.Duration{}}
	ctx := context.Background()
	if n, _ := Count(ctx, m, "k"); n != 0 {
		t.Fatal(n)
	}
	for i := 1; i <= 3; i++ {
		if n, err := Hit(ctx, m, "k", time.Minute); err != nil || n != int64(i) {
			t.Fatal(n, err)
		}
	}
	if m.ttl["k"] != time.Minute {
		t.Fatal("window not set")
	}
	if n, _ := Count(ctx, m, "k"); n != 3 {
		t.Fatal(n)
	}
}
