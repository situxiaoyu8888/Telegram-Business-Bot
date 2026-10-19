package telegram

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type errorTransport struct{}

func (errorTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("cannot reach %s", r.URL.String())
}
func TestTransportErrorsDoNotExposeBotToken(t *testing.T) {
	token := "12345:private-secret"
	c := NewClient(token)
	c.http = &http.Client{Transport: errorTransport{}}
	err := c.SendMessage(context.Background(), 1, "hello", nil)
	if err == nil || strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("unsafe error: %v", err)
	}
}
