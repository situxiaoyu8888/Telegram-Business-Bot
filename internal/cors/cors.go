// Package cors lets the configured browser origins call the API on another listener.
package cors

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Allow answers CORS for the given origins, with credentials (the console session cookie).
// Requests from other origins get no CORS headers, so browsers do not expose the response; their
// preflights are refused. Same-origin and non-browser requests (no Origin) pass unchanged.
func Allow(origins []string) gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, o := range origins {
		if o != "" {
			allowed[o] = true
		}
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && allowed[origin] {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			if c.Request.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE")
				h.Set("Access-Control-Allow-Headers", "Content-Type, X-Requested-With, X-Telegram-Init-Data, X-Upload-Token")
				h.Set("Access-Control-Max-Age", "600")
				c.AbortWithStatus(http.StatusNoContent)
				return
			}
		} else if c.Request.Method == http.MethodOptions && origin != "" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}
		c.Next()
	}
}
