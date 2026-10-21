package telegram

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"
)

// signInitData builds initData the way Telegram does, for tests in this and other packages.
func signInitData(token string, values url.Values) string {
	keys := []string{}
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := []string{}
	for _, k := range keys {
		lines = append(lines, k+"="+values.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(token))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	values.Set("hash", hex.EncodeToString(mac.Sum(nil)))
	return values.Encode()
}

func TestVerifyInitData(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	data := signInitData("123:abc", url.Values{"auth_date": {"1800000000"}, "query_id": {"q"}, "user": {`{"id":42,"first_name":"Li","username":"li"}`}})
	u, err := VerifyInitData(data, "123:abc", time.Hour, now)
	if err != nil || u.ID != 42 || u.Username != "li" {
		t.Fatal(u, err)
	}
	if _, err := VerifyInitData(data, "999:other", time.Hour, now); err == nil {
		t.Fatal("accepted another bot's signature")
	}
	if _, err := VerifyInitData(data, "123:abc", time.Hour, now.Add(2*time.Hour)); err == nil {
		t.Fatal("accepted expired data")
	}
	tampered := strings.Replace(data, "li", "lx", 1)
	if _, err := VerifyInitData(tampered, "123:abc", time.Hour, now); err == nil {
		t.Fatal("accepted tampered data")
	}
}
