package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"github.com/redis/go-redis/v9"
	"time"
)

var ErrBusy = errors.New("operation already in progress")

const ttl = 2 * time.Minute

// Run owns a renewable lease. Cancellation or loss of ownership cancels the work.
type Client interface {
	SetNX(context.Context, string, interface{}, time.Duration) *redis.BoolCmd
	Eval(context.Context, string, []string, ...interface{}) *redis.Cmd
}

func Run(ctx context.Context, rdb Client, key string, work func(context.Context) error) error {
	return run(ctx, rdb, key, ttl, work)
}

// RunFor is Run with a custom lease time, for short work whose stale lease (after a crash) should
// not block others for long; it is renewed every third of it while the work runs.
func RunFor(ctx context.Context, rdb Client, key string, d time.Duration, work func(context.Context) error) error {
	return run(ctx, rdb, key, d, work)
}
func run(ctx context.Context, rdb Client, key string, ttl time.Duration, work func(context.Context) error) error {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	token := hex.EncodeToString(b)
	ok, err := rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return err
	}
	if !ok {
		return ErrBusy
	}
	child, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	stopped := make(chan struct{})
	lost := make(chan error, 1)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(ttl / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-child.Done():
				return
			case <-ticker.C:
				renewCtx, stop := context.WithTimeout(child, 5*time.Second)
				n, e := rdb.Eval(renewCtx, `if redis.call('get',KEYS[1]) == ARGV[1] then return redis.call('pexpire',KEYS[1],ARGV[2]) else return 0 end`, []string{key}, token, ttl.Milliseconds()).Int64()
				stop()
				if e != nil || n != 1 {
					if e == nil {
						e = errors.New("lease lost")
					}
					lost <- e
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		close(done)
		<-stopped
		releaseCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = rdb.Eval(releaseCtx, `if redis.call('get',KEYS[1]) == ARGV[1] then return redis.call('del',KEYS[1]) else return 0 end`, []string{key}, token).Err()
	}()
	err = work(child)
	select {
	case e := <-lost:
		return e
	default:
		return err
	}
}
