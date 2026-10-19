package app

import (
	"github.com/gin-gonic/gin"
	"net/http"
	"net/http/httptest"
	"strings"
	"telegram-business-bot/internal/config"
	"testing"
)

func TestWebhookFailsClosedWithoutSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, secret := range []string{"", "expected"} {
		a := &App{cfg: config.Config{TelegramMode: "webhook", TelegramWebhookSecret: secret}}
		r := gin.New()
		r.POST("/webhook", a.webhookHandler(false))
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{"update_id":1}`))
		r.ServeHTTP(w, req)
		if w.Code != 401 {
			t.Fatalf("secret=%q code=%d", secret, w.Code)
		}
	}
}
