package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"telegram-business-bot/internal/telegram"
)

type fakeWebhook struct {
	mu       sync.Mutex
	failures int    // SetWebhook calls that fail before one succeeds
	sets     int    // successful SetWebhook calls
	current  string // URL Telegram reports
}

func (f *fakeWebhook) SetWebhook(_ context.Context, url, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures > 0 {
		f.failures--
		return errors.New("bad webhook: Failed to resolve host")
	}
	f.sets++
	f.current = url
	return nil
}

func (f *fakeWebhook) GetWebhookInfo(context.Context) (*telegram.WebhookInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &telegram.WebhookInfo{URL: f.current}, nil
}

func (f *fakeWebhook) state() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets, f.current
}

func shortIntervals(t *testing.T) {
	oldMin, oldMax, oldCheck := retryMin, retryMax, webhookCheckEvery
	retryMin, retryMax, webhookCheckEvery = time.Millisecond, 4*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { retryMin, retryMax, webhookCheckEvery = oldMin, oldMax, oldCheck })
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestKeepWebhookRetriesAndReregisters(t *testing.T) {
	shortIntervals(t)
	const url = "https://bot.example.com/telegram/webhook"
	f := &fakeWebhook{failures: 3}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- keepWebhook(ctx, f, url, "secret", zap.NewNop()) }()

	// Registration keeps retrying through failures instead of giving up.
	waitFor(t, func() bool { sets, cur := f.state(); return sets == 1 && cur == url })

	// Someone removes the webhook: the periodic check registers it again.
	f.mu.Lock()
	f.current = ""
	f.mu.Unlock()
	waitFor(t, func() bool { sets, cur := f.state(); return sets == 2 && cur == url })

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRetryStopsWithContext(t *testing.T) {
	shortIntervals(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := retry(ctx, func() error { calls++; return errors.New("down") }, func(error, time.Duration) {
		if calls == 3 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || calls != 3 {
		t.Fatal(err, calls)
	}
}
