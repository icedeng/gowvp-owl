package openapi

import (
	"encoding/base64"
	"encoding/json"
	"github.com/gin-gonic/gin"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"gorm.io/gorm"
	"strconv"
	"strings"
	"time"
)

// resourcePage 使用实体序列化，保证订阅密文等隐藏字段不会被map查询意外输出。
func resourcePage[T any](a *API, c *gin.Context, q *gorm.DB, table string, id func(T) string) {
	size, e := strconv.Atoi(c.DefaultQuery("size", "50"))
	if e != nil || size < 1 || size > 100 {
		respond(c, nil, op.Fail(400, "INVALID_PAGE", "size须为1至100"))
		return
	}
	query := c.Request.URL.Query()
	query.Del("cursor")
	identity := op.Hash([]byte(c.Request.URL.Path + "?" + query.Encode()))
	p := principal(c)
	if raw := c.Query("cursor"); raw != "" {
		body, sig, ok := strings.Cut(raw, ".")
		b, e := base64.RawURLEncoding.DecodeString(body)
		var v cursor
		if !ok || e != nil || json.Unmarshal(b, &v) != nil || v.App != p.App.ID || v.Query != identity || v.Exp < time.Now().Unix() || !op.Verify(a.S.Config.Keys[a.S.Config.KeyID], body, sig) {
			respond(c, nil, op.Fail(400, "INVALID_CURSOR", "游标无效或已过期"))
			return
		}
		q = q.Where(table+".id > ?", v.Last)
	}
	items := []T{}
	if e = q.Order(table + ".id ASC").Limit(size + 1).Find(&items).Error; e != nil {
		respond(c, nil, e)
		return
	}
	var next any
	if len(items) > size {
		items = items[:size]
		b, _ := json.Marshal(cursor{p.App.ID, identity, id(items[size-1]), time.Now().Add(time.Hour).Unix()})
		raw := base64.RawURLEncoding.EncodeToString(b)
		next = raw + "." + op.Sign(a.S.Config.Keys[a.S.Config.KeyID], raw)
	}
	respond(c, gin.H{"items": items, "next_cursor": next}, nil)
}
