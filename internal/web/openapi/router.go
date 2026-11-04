// Package openapi 将应用认证、管理入口和媒体票据与旧业务接口分开注册。
package openapi

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"gorm.io/gorm"
)

type API struct{ S *op.Service }

func respond(c *gin.Context, data any, err error) {
	if err != nil {
		e := op.AsError(err)
		c.Set("open_result", e.Code)
		c.AbortWithStatusJSON(e.Status, gin.H{"code": e.Code, "message": e.Message, "request_id": c.GetString("open_request_id"), "data": nil})
		return
	}
	c.JSON(200, gin.H{"code": "OK", "message": "success", "request_id": c.GetString("open_request_id"), "data": data})
}
func Body(c *gin.Context, out any) error {
	b, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if e != nil {
		return op.Fail(413, "BODY_TOO_LARGE", "请求体过大")
	}
	if len(b) == 0 {
		b = []byte("{}")
	}
	if e = uniqueJSON(b); e != nil {
		return op.Fail(400, "INVALID_JSON", "JSON包含重复属性或格式错误")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(out); e != nil {
		return op.Fail(400, "INVALID_JSON", "请求字段或JSON格式不正确")
	}
	if d.Decode(new(any)) != io.EOF {
		return op.Fail(400, "INVALID_JSON", "请求体必须为单个JSON对象")
	}
	return nil
}
func uniqueJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		switch t {
		case json.Delim('{'):
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return errors.New("duplicate key")
				}
				seen[s] = true
				if e = walk(); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		case json.Delim('['):
			for d.More() {
				if e = walk(); e != nil {
					return e
				}
			}
			_, e = d.Token()
			return e
		}
		return nil
	}
	return walk()
}
func principal(c *gin.Context) op.Principal { return c.MustGet("open_principal").(op.Principal) }
func require(c *gin.Context, s *op.Service, scope string) bool {
	if err := s.Scope(principal(c).App, scope); err != nil {
		respond(c, nil, err)
		return false
	}
	return true
}
func New(s *op.Service) *API { return &API{S: s} }

// Register 总是占用新路径；配置缺失时返回503，不能降级到旧鉴权。
func (a *API) Register(r *gin.Engine, admin gin.HandlerFunc) {
	// 开发者文档只公开接入元信息，不包含应用、凭据或设备数据。
	r.GET("/open-platform/docs/config", func(c *gin.Context) {
		c.Set("open_request_id", uuid.NewString())
		c.Header("Cache-Control", "no-store")
		data := gin.H{"enabled": false, "public_url": "", "audience": "", "allow_http": false}
		if a.S != nil && a.S.Config.Enabled {
			data = gin.H{"enabled": true, "public_url": a.S.Config.PublicBaseURL, "audience": a.S.Config.Audience, "allow_http": a.S.Config.AllowHTTP}
		}
		respond(c, data, nil)
	})
	common := func(c *gin.Context) {
		c.Set("open_request_id", uuid.NewString())
		c.Header("X-Request-ID", c.GetString("open_request_id"))
		c.Header("Cache-Control", "no-store")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Content-Type-Options", "nosniff")
		if a.S == nil || !a.S.Config.Enabled {
			respond(c, nil, op.ErrUnavailable)
			return
		}
		c.Next()
	}
	mg := r.Group("/admin/open-platform", common, func(c *gin.Context) {
		started := time.Now()
		c.Next()
		result := c.GetString("open_result")
		if result == "" {
			result = "completed"
		}
		a.S.Record(op.Audit{RequestID: c.GetString("open_request_id"), AppID: c.Param("app"), Actor: c.GetString("open_admin"), IP: a.IP(c), Route: c.FullPath(), Action: "admin." + c.Request.Method, ResourceID: c.Param("app"), Result: result, Status: c.Writer.Status(), DurationMS: time.Since(started).Milliseconds()})
	}, admin)
	a.admin(mg)
	g := r.Group("/openapi/v1", common, a.auth)
	a.business(g)
	r.GET("/openmedia/v1/:kind/:id/*path", common, a.media)
}
func (a *API) IP(c *gin.Context) string {
	host, _, e := net.SplitHostPort(c.Request.RemoteAddr)
	if e != nil {
		host = c.Request.RemoteAddr
	}
	ip := net.ParseIP(host)
	trusted := func(ip net.IP) bool {
		for _, n := range a.S.Config.TrustedProxies {
			if n.Contains(ip) {
				return true
			}
		}
		return false
	}
	if !trusted(ip) {
		return host
	}
	chain := strings.Split(c.GetHeader("X-Forwarded-For"), ",")
	for i := len(chain) - 1; i >= 0; i-- {
		candidate := net.ParseIP(strings.TrimSpace(chain[i]))
		if candidate == nil {
			return host
		}
		ip = candidate
		if !trusted(ip) {
			break
		}
	}
	return ip.String()
}
func allowedIP(app op.App, raw string) bool {
	if app.AllowAnyIP {
		return true
	}
	ip := net.ParseIP(raw)
	for _, s := range app.CIDRs {
		_, n, e := net.ParseCIDR(s)
		if e == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
func (a *API) auth(c *gin.Context) {
	started := time.Now()
	var p op.Principal
	result := "authentication_failed"
	defer func() {
		if value := c.GetString("open_result"); value != "" {
			result = value
		}
		a.S.Record(op.Audit{RequestID: c.GetString("open_request_id"), AppID: p.App.ID, CredentialID: p.Credential.ID, Actor: "application", IP: a.IP(c), Route: c.FullPath(), Action: c.Request.Method, Result: result, Status: c.Writer.Status(), DurationMS: time.Since(started).Milliseconds()})
	}()
	deny := func(e error) { respond(c, nil, e) }
	if !a.S.Limit("ip:"+a.IP(c), 50, 100) {
		deny(op.Fail(429, "RATE_LIMITED", "请求过于频繁"))
		return
	}
	for _, h := range []string{"Authorization", "X-Owl-Audience", "X-Owl-Timestamp", "X-Owl-Nonce", "X-Owl-Content-SHA256", "Content-Type", "Idempotency-Key", "Content-Encoding"} {
		if len(c.Request.Header.Values(h)) > 1 || len(c.GetHeader(h)) > 512 || strings.ContainsAny(c.GetHeader(h), "\r\n") {
			deny(op.ErrAuth)
			return
		}
	}
	path := c.Request.URL.EscapedPath()
	if !op.ValidPath(path) || c.Request.URL.RawPath != "" || strings.HasSuffix(path, "/") || c.GetHeader("Content-Encoding") != "" {
		deny(op.ErrAuth)
		return
	}
	audience, ts, nonce, digest := c.GetHeader("X-Owl-Audience"), c.GetHeader("X-Owl-Timestamp"), c.GetHeader("X-Owl-Nonce"), c.GetHeader("X-Owl-Content-SHA256")
	stamp, e := strconv.ParseInt(ts, 10, 64)
	now := time.Now().Unix()
	n, ne := base64.RawURLEncoding.DecodeString(nonce)
	if e != nil || strconv.FormatInt(stamp, 10) != ts || stamp < now-300 || stamp > now+300 || ne != nil || len(n) < 16 || len(nonce) > 128 || audience != a.S.Config.Audience {
		c.Header("X-Owl-Server-Time", strconv.FormatInt(now, 10))
		deny(op.ErrAuth)
		return
	}
	query, _, e := op.CanonicalQuery(c.Request.URL.RawQuery)
	if e != nil {
		deny(e)
		return
	}
	ct := c.GetHeader("Content-Type")
	if c.Request.Method == "GET" || c.Request.Method == "DELETE" {
		if ct != "" {
			deny(op.ErrAuth)
			return
		}
	} else if ct != "application/json" {
		deny(op.ErrAuth)
		return
	}
	body, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20))
	if e != nil {
		deny(op.Fail(413, "BODY_TOO_LARGE", "请求体过大"))
		return
	}
	if (c.Request.Method == "GET" || c.Request.Method == "DELETE") && len(body) > 0 || op.Hash(body) != digest {
		deny(op.ErrAuth)
		return
	}
	ak, sig, e := op.ParseAuthorization(c.GetHeader("Authorization"))
	if e != nil {
		deny(e)
		return
	}
	p, key, e := a.S.Lookup(ak)
	if e != nil {
		deny(e)
		return
	}
	if !op.Verify(key, op.Canonical(audience, c.Request.Method, path, query, ct, ts, nonce, c.GetHeader("Idempotency-Key"), digest), sig) {
		deny(op.ErrAuth)
		return
	}
	if !allowedIP(p.App, a.IP(c)) {
		result = "ip_denied"
		deny(op.Fail(403, "IP_DENIED", "来源IP不允许"))
		return
	}
	if e = a.S.UseNonce(ak, nonce, stamp); e != nil {
		result = "nonce_rejected"
		deny(e)
		return
	}
	if !a.S.Limit("app:"+p.App.ID, p.App.QPS, p.App.Burst) {
		result = "rate_limited"
		c.Header("Retry-After", "1")
		deny(op.Fail(429, "RATE_LIMITED", "应用请求超过配额"))
		return
	}
	c.Request.URL.RawQuery = query
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	c.Set("open_body", body)
	c.Set("open_principal", p)
	result = "completed"
	c.Next()
}
func (a *API) idem(c *gin.Context, operation string) (string, bool) {
	b, _ := c.Get("open_body")
	body, _ := b.([]byte)
	id, err := a.S.Begin(principal(c), operation, c.GetHeader("Idempotency-Key"), op.Hash(append([]byte(c.Request.URL.Path+"?"+c.Request.URL.RawQuery+"\n"), body...)))
	if err != nil {
		c.Header("Retry-After", "2")
		respond(c, nil, err)
		return "", false
	}
	return id, true
}
func (a *API) done(c *gin.Context, operation, id string) bool {
	if err := a.S.Finish(principal(c), operation, c.GetHeader("Idempotency-Key"), id); err != nil {
		respond(c, nil, err)
		return false
	}
	return true
}
func get[T any](db *gorm.DB, id string) (T, error) {
	var row T
	e := db.First(&row, "id = ?", id).Error
	if errors.Is(e, gorm.ErrRecordNotFound) {
		e = op.ErrNotFound
	}
	return row, e
}
func (a *API) list(c *gin.Context, db *gorm.DB, out any) {
	var total int64
	if err := db.Count(&total).Error; err != nil {
		respond(c, nil, err)
		return
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "50"))
	if page < 1 || page > 100000 || size < 1 || size > 100 {
		respond(c, nil, op.Fail(400, "INVALID_PAGE", "分页参数不合法"))
		return
	}
	if e := db.Order("created_at DESC").Offset((page - 1) * size).Limit(size).Find(out).Error; e != nil {
		respond(c, nil, e)
		return
	}
	respond(c, gin.H{"items": out, "total": total, "page": page, "size": size}, nil)
}

// requireSecretTransport 默认要求TLS；仅显式配置允许HTTP时兼容现有明文入口。
func (a *API) requireSecretTransport(c *gin.Context) bool {
	if a.S.Config.AllowHTTP {
		return true
	}
	if c.Request.TLS != nil {
		return true
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err == nil && len(c.Request.Header.Values("X-Forwarded-Proto")) == 1 && c.GetHeader("X-Forwarded-Proto") == "https" {
		ip := net.ParseIP(host)
		for _, network := range a.S.Config.TrustedProxies {
			if network.Contains(ip) {
				return true
			}
		}
	}
	respond(c, nil, op.Fail(403, "HTTPS_REQUIRED", "创建或轮换密钥需要HTTPS入口"))
	return false
}
