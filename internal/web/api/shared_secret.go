package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	maxWebhookBodyBytes = 10 << 20
	maxSnapshotBytes    = 5 << 20
)

func sharedSecretAuth(secret func() string) gin.HandlerFunc {
	return func(c *gin.Context) {
		expected := strings.TrimSpace(secret())
		provided := strings.TrimSpace(c.GetHeader("Secret"))
		if provided == "" {
			provided = strings.TrimSpace(c.Query("secret"))
		}
		if expected == "" || !constantTimeEqual(provided, expected) {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 1, "msg": "unauthorized"})
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBodyBytes)
		c.Next()
	}
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
