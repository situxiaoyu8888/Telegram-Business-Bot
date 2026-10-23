package trongrid

import (
	"context"
	"database/sql/driver"
	"fmt"
	"go.uber.org/zap"
	"io"
	"net/http"
	"strings"
	"telegram-business-bot/internal/store"
	"telegram-business-bot/internal/testutil"
	"telegram-business-bot/internal/wallet"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestAccountTRC20ReadsAllPages(t *testing.T) {
	calls := 0
	max := ""
	c := New("https://example.invalid", "")
	c.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		q := r.URL.Query()
		if q.Get("min_timestamp") != "123" || q.Get("only_confirmed") != "true" {
			t.Errorf("lost scan filters %v", q)
		}
		if calls == 1 {
			max = q.Get("max_timestamp")
			return response(`{"success":true,"data":[{"transaction_id":"first"}],"meta":{"fingerprint":"next"}}`), nil
		}
		if q.Get("fingerprint") != "next" || q.Get("max_timestamp") != max {
			t.Errorf("bad pagination %v", q)
		}
		return response(`{"success":true,"data":[{"transaction_id":"second"}],"meta":{}}`), nil
	})}
	rows, err := c.AccountTRC20(context.Background(), "address", "contract", true, 123)
	if err != nil || len(rows) != 2 || calls != 2 {
		t.Fatalf("%+v %v calls=%d", rows, err, calls)
	}
}
func TestPaginationFailureNeverReturnsPartialSuccess(t *testing.T) {
	for _, mode := range []string{"failure", "repeat", "http"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			c := New("https://example.invalid", "")
			c.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 || mode == "repeat" {
					return response(`{"success":true,"data":[{"transaction_id":"first"}],"meta":{"fingerprint":"next"}}`), nil
				}
				if mode == "http" {
					return nil, fmt.Errorf("connection lost")
				}
				return response(`{"success":false}`), nil
			})}
			rows, err := c.AccountTRC20(context.Background(), "address", "contract", true, 0)
			if err == nil || rows != nil {
				t.Fatalf("partial success: %v %v", rows, err)
			}
		})
	}
}
func TestRawAmountValidation(t *testing.T) {
	amount, ok := rawToDecimal("1234567", 6)
	if !ok || amount.String() != "1.234567" {
		t.Fatal(amount, ok)
	}
	for _, v := range []struct {
		raw      string
		decimals int
	}{{"-100", 6}, {"bad", 6}, {"10", -1}, {"10", 100}} {
		if _, ok := rawToDecimal(v.raw, v.decimals); ok {
			t.Fatalf("accepted %+v", v)
		}
	}
}

func TestWatcherResumesFromPersistedCursorAfterLongDowntime(t *testing.T) {
	cursor := time.Now().Add(-10 * 24 * time.Hour).UnixMilli()
	db := testutil.DB(t,
		testutil.Step{Contains: "deposit_addresses", Columns: []string{"user_id", "address", "users", "scan_timestamp"}, Rows: [][]driver.Value{{int64(2), "address", int64(1), cursor}}},
		testutil.Step{Contains: "deposit_address_holders", Columns: []string{"id"}},
		testutil.Step{Contains: "deposit_pool_addresses", Columns: []string{"id"}},
	)
	c := New("https://example.invalid", "")
	called := false
	c.http = &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		want := fmt.Sprint(cursor - (48 * time.Hour).Milliseconds())
		if r.URL.Query().Get("min_timestamp") != want {
			t.Errorf("lost historical scan window: %s, want %s", r.URL.Query().Get("min_timestamp"), want)
		}
		return response(`{"success":true,"data":[],"meta":{}}`), nil
	})}
	w := NewWatcher(&store.Store{DB: db}, nil, nil, c, "contract", true, time.Minute, zap.NewNop())
	w.scan(context.Background())
	if !called {
		t.Fatal("no scan")
	}
}

func TestFailedCreditDoesNotAdvanceScanCursor(t *testing.T) {
	db := testutil.DB(t,
		testutil.Step{Contains: "deposit_addresses", Columns: []string{"user_id", "address", "users", "scan_timestamp"}, Rows: [][]driver.Value{{int64(2), "address", int64(1), int64(123)}}},
		testutil.Step{Contains: "deposit_address_holders", Columns: []string{"id"}},
		testutil.Step{Contains: "deposit_orders", Err: fmt.Errorf("database unavailable")},
		testutil.Step{Contains: "deposit_pool_addresses", Columns: []string{"id"}},
	)
	st := &store.Store{DB: db}
	c := New("https://example.invalid", "")
	c.http = &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"success":true,"data":[{"transaction_id":"tx","to":"address","type":"Transfer","value":"1000000","block_timestamp":999,"token_info":{"address":"contract","decimals":6}}],"meta":{}}`), nil
	})}
	w := NewWatcher(st, wallet.New(st, "USDT", "1", "0"), nil, c, "contract", true, time.Minute, zap.NewNop())
	w.scan(context.Background()) // Any checkpoint UPDATE is an unexpected SQL statement.
}
