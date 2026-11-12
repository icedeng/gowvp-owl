package api

import (
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/ixugo/goddd/pkg/logger"
	"github.com/ixugo/goddd/pkg/web"
)

// 保留请求追踪和耗时，不读取请求体，也不将查询令牌、密钥或响应内容写入日志。
func requestLogger(ignore ...web.IngoreOption) gin.HandlerFunc {
	return func(c *gin.Context) {
		traceID := uuid.NewString()
		c.Request = c.Request.WithContext(logger.WithAttr(c.Request.Context(), slog.String("trace_id", traceID)))
		web.SetTraceID(c, traceID)
		for _, skip := range ignore {
			if skip(c) {
				c.Next()
				return
			}
		}
		started := time.Now()
		c.Next()
		fields := []any{
			"method", c.Request.Method, "path", c.Request.URL.Path,
			"remoteaddr", c.ClientIP(), "statuscode", c.Writer.Status(),
			"duration_ms", time.Since(started).Milliseconds(),
		}
		if status := c.Writer.Status(); status >= 200 && status < 400 {
			slog.InfoContext(c.Request.Context(), "OK", fields...)
		} else {
			slog.WarnContext(c.Request.Context(), "Bad", fields...)
		}
	}
}
