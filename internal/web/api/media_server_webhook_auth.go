package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/sms"
)

// mediaServerWebhookAuth 按节点独立密钥鉴权，并约束回调归属。
// 兼容旧 local 回调 URL；新 URL 显式携带节点 ID。
func (w WebHookAPI) mediaServerWebhookAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.Query("media_server_id"))
		if id == "" {
			id = sms.DefaultMediaServerID
		}
		expected := w.conf.Media.Secret
		if reader, ok := w.smsCore.(interface {
			GetMediaServer(context.Context, string) (*sms.MediaServer, error)
		}); ok {
			node, err := reader.GetMediaServer(c.Request.Context(), id)
			if err != nil || node.Disabled {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
			expected = node.Secret
		} else if id != sms.DefaultMediaServerID {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		provided := strings.TrimSpace(c.GetHeader("Secret"))
		if provided == "" {
			provided = strings.TrimSpace(c.Query("secret"))
		}
		if expected == "" || !constantTimeEqual(provided, expected) {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBodyBytes)
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatus(http.StatusRequestEntityTooLarge)
			return
		}
		var payload struct {
			MediaServerID string `json:"mediaServerId"`
		}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &payload); err != nil {
				c.AbortWithStatus(http.StatusBadRequest)
				return
			}
			if payload.MediaServerID != "" && payload.MediaServerID != id {
				c.AbortWithStatus(http.StatusUnauthorized)
				return
			}
		}
		if payload.MediaServerID == "" && len(body) > 0 {
			var fields map[string]json.RawMessage
			if json.Unmarshal(body, &fields) == nil && fields != nil {
				fields["mediaServerId"], _ = json.Marshal(id)
				body, _ = json.Marshal(fields)
			}
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Set("webhook_media_server_id", id)
		c.Next()
	}
}
