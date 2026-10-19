package lease

import (
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"sync"
	"testing"
	"time"
)

type fakeRedis struct {
	mu    sync.Mutex
	value string
}

func (f *fakeRedis) SetNX(_ context.Context, key string, v interface{}, ttl time.Duration) *redis.BoolCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.value != "" {
		return redis.NewBoolResult(false, nil)
	}
	f.value = v.(string)
	return redis.NewBoolResult(true, nil)
}
func (f *fakeRedis) Eval(_ context.Context, script string, keys []string, args ...interface{}) *redis.Cmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.value != args[0].(string) {
		return redis.NewCmdResult(int64(0), nil)
	}
	if len(args) == 1 {
		f.value = ""
	}
	return redis.NewCmdResult(int64(1), nil)
}
func TestLeaseExcludesConcurrentWorkAndReleasesOnError(t *testing.T) {
	f := &fakeRedis{}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), f, "key", time.Second, func(context.Context) error { close(entered); <-release; return errors.New("work failed") })
	}()
	<-entered
	err := run(context.Background(), f, "key", time.Second, func(context.Context) error { t.Error("concurrent worker entered"); return nil })
	if !errors.Is(err, ErrBusy) {
		t.Errorf("expected busy, got %v", err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Error("work failure lost")
	}
	if err := run(context.Background(), f, "key", time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("lease not released: %v", err)
	}
}
func TestLostLeaseCancelsWorkAndPreservesNewOwner(t *testing.T) {
	f := &fakeRedis{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := run(ctx, f, "key", 30*time.Millisecond, func(ctx context.Context) error {
		f.mu.Lock()
		f.value = "new-owner"
		f.mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	})
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ownership loss not detected: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.value != "new-owner" {
		t.Fatal("deleted someone else's lease")
	}
}
