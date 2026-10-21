package telegram

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var ErrInvalidInitData = errors.New("invalid Telegram Web App init data")

// VerifyInitData checks a Mini App's initData against a bot token, as documented at
// https://core.telegram.org/bots/webapps#validating-data-received-via-the-mini-app,
// and returns the signed-in user. Data older than maxAge is rejected.
func VerifyInitData(initData, botToken string, maxAge time.Duration, now time.Time) (*User, error) {
	values, err := url.ParseQuery(initData)
	if err != nil {
		return nil, ErrInvalidInitData
	}
	hash := values.Get("hash")
	if hash == "" {
		return nil, ErrInvalidInitData
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if k != "hash" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+"="+values.Get(k))
	}
	secret := hmac.New(sha256.New, []byte("WebAppData"))
	secret.Write([]byte(botToken))
	mac := hmac.New(sha256.New, secret.Sum(nil))
	mac.Write([]byte(strings.Join(lines, "\n")))
	want, err := hex.DecodeString(hash)
	if err != nil || !hmac.Equal(mac.Sum(nil), want) {
		return nil, ErrInvalidInitData
	}
	authDate, err := strconv.ParseInt(values.Get("auth_date"), 10, 64)
	if err != nil {
		return nil, ErrInvalidInitData
	}
	if age := now.Sub(time.Unix(authDate, 0)); age > maxAge || age < -time.Minute {
		return nil, errors.New("Telegram sign-in expired, reopen the shop")
	}
	var u User
	if err := json.Unmarshal([]byte(values.Get("user")), &u); err != nil || u.ID <= 0 {
		return nil, ErrInvalidInitData
	}
	return &u, nil
}
