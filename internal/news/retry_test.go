package news

import "testing"

func TestOnlyUndeliveredSendsAreRetried(t *testing.T) {
	for msg, want := range map[string]bool{
		`telegram http 429: {"ok":false,"error_code":429,"description":"Too Many Requests: retry after 5"}`: true,
		`telegram http 502: Bad Gateway`: true,
		`Post "https://api.telegram.org/bot[redacted]/sendMessage": dial tcp: connection refused`:       true,
		`Post "https://api.telegram.org/bot[redacted]/sendMessage": context deadline exceeded`:          false,
		`telegram http 403: {"ok":false,"description":"Forbidden: bot was kicked from the group chat"}`: false,
	} {
		if got := retryableSendError(msg); got != want {
			t.Errorf("%q: retry %v, want %v", msg, got, want)
		}
	}
}
