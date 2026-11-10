package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 仅供精确 /web 路由使用，保持相对静态资源基址和原查询参数。
func redirectWebRoot(c *gin.Context) {
	target := "/web/"
	if c.Request.URL.RawQuery != "" {
		target += "?" + c.Request.URL.RawQuery
	}
	c.Redirect(http.StatusPermanentRedirect, target)
}
