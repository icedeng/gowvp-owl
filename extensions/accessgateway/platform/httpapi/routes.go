// Package httpapi 只处理管理 HTTP 契约；严格管理员身份由宿主显式注入。
package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/credentials"
	"github.com/gowvp/owl/extensions/accessgateway/platform/service"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

type API struct {
	service *service.Service
	actor   func(*gin.Context) string
}

func New(s *service.Service, actor func(*gin.Context) string) *API {
	return &API{service: s, actor: actor}
}
func (a *API) Register(router gin.IRouter, admin gin.HandlerFunc) {
	migrations := router.Group("/access_gateway_migrations", admin, a.ready)
	migrations.POST("", a.createMigration)
	migrations.GET("/:id", a.getMigration)
	migrations.POST("/:id/cancel", a.cancelMigration)
	r := router.Group("/access_gateways", admin, a.ready)
	r.GET("", a.list)
	r.POST("", a.create)
	r.POST("/connection-test", a.draftTest)
	r.GET("/:id", a.get)
	r.PUT("/:id", a.update)
	r.DELETE("/:id", a.remove)
	r.POST("/:id/test", a.test)
	r.POST("/:id/channel-diagnostic", a.channelDiagnostic)
	r.POST("/:id/release-reports", a.reportRelease)
	r.PUT("/:id/state", a.state)
	r.POST("/:id/reconnect", a.reconnect)
	r.GET("/:id/devices", a.devices)
	r.GET("/:id/platforms", a.platforms)
	r.GET("/:id/sessions", a.sessions)
	r.POST("/:id/sessions/:session/stop", a.stop)
	r.GET("/:id/metrics", a.metrics)
	r.GET("/:id/events", a.events)
	r.GET("/:id/operations/:operation", a.operation)
	r.POST("/:id/credentials", a.credentials)
	r.POST("/:id/certificate-rotation", a.rotate)
	r.GET("/:id/deployment-template", a.template)
}
func Failure(c *gin.Context, e error) {
	trace := uuid.NewString()
	status, reason, msg := 500, "INTERNAL_ERROR", "网关管理暂不可用"
	details := map[string]any{}
	var ce *core.Error
	if errors.As(e, &ce) {
		status, reason, msg = ce.Status, ce.Reason, ce.Message
		if ce.Details != nil {
			details = ce.Details
		}
	} else {
		var pe *protocol.Error
		if errors.As(e, &pe) {
			status, reason, msg = 503, string(pe.Code), pe.Message
		} else {
			slog.ErrorContext(c.Request.Context(), "网关管理请求失败", "trace_id", trace, "error", e)
		}
	}
	c.AbortWithStatusJSON(status, gin.H{"msg": msg, "reason": reason, "trace_id": trace, "details": details})
}
func (a *API) ready(c *gin.Context) {
	if a.service == nil {
		Failure(c, core.Fail(503, "MODULE_DISABLED", "公网接入网关模块未启用"))
		return
	}
	c.Header("Cache-Control", "no-store")
	c.Next()
}
func decode(c *gin.Context, out any) bool {
	contentType, _, e := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if e != nil || contentType != "application/json" {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "请求须为 application/json"))
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	data, e := io.ReadAll(c.Request.Body)
	if e != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(e, &tooLarge) {
			Failure(c, core.Fail(413, "BODY_TOO_LARGE", "JSON 请求超过 1 MiB"))
		} else {
			Failure(c, core.Fail(400, "INVALID_ARGUMENT", "无法读取请求"))
		}
		return false
	}
	if e = protocol.DecodeStrict(data, out); e != nil {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", e.Error()))
		return false
	}
	return true
}
func expected(c *gin.Context, required bool) (*int64, bool) {
	values := c.Request.Header.Values("If-Match")
	if len(values) == 0 && !required {
		return nil, true
	}
	if len(values) != 1 {
		Failure(c, core.Fail(400, "IF_MATCH_REQUIRED", "需要唯一 If-Match 版本"))
		return nil, false
	}
	s := values[0]
	if len(s) < 3 || s[0] != '"' || s[len(s)-1] != '"' {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "If-Match 必须为双引号括起的十进制版本"))
		return nil, false
	}
	n, e := protocol.Decimal(s[1 : len(s)-1]).Int64()
	if e != nil {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "If-Match 版本无效"))
		return nil, false
	}
	return &n, true
}
func (a *API) mutation(c *gin.Context, requireRevision bool) (core.Mutation, bool) {
	values := c.Request.Header.Values("Idempotency-Key")
	if len(values) != 1 {
		Failure(c, core.Fail(400, "INVALID_IDEMPOTENCY_KEY", "需要唯一 Idempotency-Key UUID"))
		return core.Mutation{}, false
	}
	if _, e := uuid.Parse(values[0]); e != nil {
		Failure(c, core.Fail(400, "INVALID_IDEMPOTENCY_KEY", "Idempotency-Key 不是 UUID"))
		return core.Mutation{}, false
	}
	revision, ok := expected(c, requireRevision)
	if !ok {
		return core.Mutation{}, false
	}
	return core.Mutation{Actor: a.actor(c), Route: c.Request.Method + " " + c.FullPath(), Key: values[0], ExpectedRevision: revision}, true
}
func respond(c *gin.Context, result core.Result, e error) {
	if e != nil {
		Failure(c, e)
		return
	}
	c.JSON(result.Status, result.Body)
}
func getResult(c *gin.Context, value any, e error) {
	if e != nil {
		Failure(c, e)
		return
	}
	c.JSON(200, value)
}
func (a *API) list(c *gin.Context) {
	q, ok := query(c, "q", "group", "admin_state", "connection_state", "health_state", "sort", "order")
	if !ok {
		return
	}
	if q.Sort != "" && q.Sort != "name" && q.Sort != "created_at" && q.Sort != "device_total" && q.Sort != "device_online" && q.Sort != "last_seen_at" {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "排序字段不受支持"))
		return
	}
	v, e := a.service.List(c.Request.Context(), q)
	getResult(c, v, e)
}
func (a *API) get(c *gin.Context) {
	if _, ok := query(c); !ok {
		return
	}
	v, e := a.service.Get(c.Request.Context(), c.Param("id"))
	if e == nil {
		c.Header("ETag", "\""+string(v.Revision)+"\"")
	}
	getResult(c, v, e)
}
func (a *API) create(c *gin.Context) {
	m, ok := a.mutation(c, false)
	if !ok {
		return
	}
	var req core.CreateRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.Create(c.Request.Context(), m, req)
	respond(c, v, e)
}
func (a *API) update(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	var req core.UpdateRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.Update(c.Request.Context(), m, c.Param("id"), req)
	respond(c, v, e)
}
func (a *API) remove(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	if c.Request.ContentLength > 0 {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "删除请求不接收 body"))
		return
	}
	v, e := a.service.Delete(c.Request.Context(), m, c.Param("id"))
	respond(c, v, e)
}
func (a *API) state(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	var req core.StateRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.State(c.Request.Context(), m, c.Param("id"), req)
	respond(c, v, e)
}
func (a *API) reconnect(c *gin.Context) {
	m, ok := a.mutation(c, false)
	if !ok {
		return
	}
	var req core.ReasonRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.Reconnect(c.Request.Context(), m, c.Param("id"), req)
	respond(c, v, e)
}
func (a *API) draftTest(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	var req core.DraftTestRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.Test(c.Request.Context(), m, req.DraftID)
	respond(c, v, e)
}
func (a *API) test(c *gin.Context) {
	m, ok := a.mutation(c, false)
	if !ok {
		return
	}
	var req struct{}
	if !decode(c, &req) {
		return
	}
	v, e := a.service.Test(c.Request.Context(), m, c.Param("id"))
	respond(c, v, e)
}
func (a *API) devices(c *gin.Context) {
	q, ok := query(c, "q", "online")
	if !ok {
		return
	}
	v, e := a.service.Devices(c.Request.Context(), c.Param("id"), q)
	getResult(c, v, e)
}
func (a *API) platforms(c *gin.Context) {
	q, ok := query(c, "q", "state")
	if !ok {
		return
	}
	v, e := a.service.Platforms(c.Request.Context(), c.Param("id"), q)
	getResult(c, v, e)
}
func (a *API) sessions(c *gin.Context) {
	q, ok := query(c, "purpose", "state", "subject_id")
	if !ok {
		return
	}
	v, e := a.service.Sessions(c.Request.Context(), c.Param("id"), q)
	getResult(c, v, e)
}
func (a *API) events(c *gin.Context) {
	q, ok := query(c, "start", "end", "type", "subject_id")
	if !ok {
		return
	}
	v, e := a.service.Events(c.Request.Context(), c.Param("id"), q)
	getResult(c, v, e)
}
func (a *API) stop(c *gin.Context) {
	m, ok := a.mutation(c, false)
	if !ok {
		return
	}
	var req core.ReasonRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.Stop(c.Request.Context(), m, c.Param("id"), c.Param("session"), req)
	respond(c, v, e)
}
func (a *API) operation(c *gin.Context) {
	if _, ok := query(c); !ok {
		return
	}
	v, e := a.service.GetOperation(c.Request.Context(), c.Param("id"), c.Param("operation"))
	getResult(c, v, e)
}
func (a *API) rotate(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	var req core.RotationRequest
	if !decode(c, &req) {
		return
	}
	v, e := a.service.Rotate(c.Request.Context(), m, c.Param("id"), req)
	respond(c, v, e)
}
func (a *API) metrics(c *gin.Context) {
	q, ok := query(c, "start", "end", "step_seconds")
	if !ok {
		return
	}
	now := time.Now().UTC()
	if q.End == nil {
		q.End = &now
	}
	start := q.End.Add(-time.Hour)
	if q.Start == nil {
		q.Start = &start
	}
	step := 60
	if raw := c.Query("step_seconds"); raw != "" {
		var e error
		step, e = strconv.Atoi(raw)
		if e != nil {
			Failure(c, core.Fail(400, "INVALID_ARGUMENT", "步长无效"))
			return
		}
	}
	v, e := a.service.Metrics(c.Request.Context(), c.Param("id"), *q.Start, *q.End, step)
	getResult(c, v, e)
}
func query(c *gin.Context, extra ...string) (core.PageQuery, bool) {
	q := core.PageQuery{Page: 1, Size: 20, Order: "asc"}
	allowed := map[string]bool{"page": true, "size": true}
	for _, key := range extra {
		allowed[key] = true
	}
	values, e := urlValues(c)
	if e != nil {
		Failure(c, e)
		return q, false
	}
	for k, v := range values {
		if !allowed[k] || len(v) != 1 {
			Failure(c, core.Fail(400, "INVALID_ARGUMENT", "未知或重复查询参数："+k))
			return q, false
		}
	}
	for _, field := range []struct {
		name  string
		value *int
		limit int
	}{{"page", &q.Page, 1000000}, {"size", &q.Size, 100}} {
		if raw := c.Query(field.name); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 || n > field.limit {
				Failure(c, core.Fail(400, "INVALID_ARGUMENT", "分页参数无效"))
				return q, false
			}
			*field.value = n
		}
	}
	q.Q = c.Query("q")
	q.Group = c.Query("group")
	q.AdminState = c.Query("admin_state")
	q.ConnectionState = c.Query("connection_state")
	q.HealthState = c.Query("health_state")
	q.Sort = c.Query("sort")
	q.Purpose = c.Query("purpose")
	q.State = c.Query("state")
	q.SubjectID = c.Query("subject_id")
	q.Type = c.Query("type")
	if len(q.Q) > 512 || len(q.SubjectID) > 64 {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "筛选参数过长"))
		return q, false
	}
	if raw := c.Query("order"); raw != "" {
		if raw != "asc" && raw != "desc" {
			Failure(c, core.Fail(400, "INVALID_ARGUMENT", "排序方向无效"))
			return q, false
		}
		q.Order = raw
	}
	if raw := c.Query("online"); raw != "" {
		if raw != "true" && raw != "false" {
			Failure(c, core.Fail(400, "INVALID_ARGUMENT", "online 须为 true 或 false"))
			return q, false
		}
		b := raw == "true"
		q.Online = &b
	}
	for _, f := range []struct {
		name   string
		target **time.Time
	}{{"start", &q.Start}, {"end", &q.End}} {
		if raw := c.Query(f.name); raw != "" {
			at, e := time.Parse(time.RFC3339Nano, raw)
			if e != nil {
				Failure(c, core.Fail(400, "INVALID_ARGUMENT", "时间须为 RFC3339"))
				return q, false
			}
			at = at.UTC()
			*f.target = &at
		}
	}
	return q, true
}
func urlValues(c *gin.Context) (map[string][]string, error) {
	v, e := url.ParseQuery(c.Request.URL.RawQuery)
	if e != nil {
		return nil, core.Fail(400, "INVALID_ARGUMENT", "查询参数编码无效")
	}
	return v, nil
}
func (a *API) credentials(c *gin.Context) {
	m, ok := a.mutation(c, false)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	reader, e := c.Request.MultipartReader()
	if e != nil {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "需要 multipart 凭据文件"))
		return
	}
	fields := map[string][]byte{}
	defer func() { clear(fields["client_key_pem"]) }()
	for {
		part, e := reader.NextPart()
		if e == io.EOF {
			break
		}
		if e != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(e, &tooLarge) {
				Failure(c, core.Fail(413, "BODY_TOO_LARGE", "上传超过 2 MiB"))
			} else {
				Failure(c, core.Fail(400, "INVALID_ARGUMENT", "multipart 数据无效"))
			}
			return
		}
		name := part.FormName()
		if name != "ca_pem" && name != "client_cert_pem" && name != "client_key_pem" || fields[name] != nil {
			part.Close()
			Failure(c, core.Fail(400, "INVALID_ARGUMENT", "未知或重复凭据字段"))
			return
		}
		data, e := io.ReadAll(io.LimitReader(part, credentials.MaxPEMBytes+1))
		part.Close()
		if e != nil || len(data) > credentials.MaxPEMBytes {
			clear(data)
			Failure(c, core.Fail(413, "BODY_TOO_LARGE", "每份 PEM 最多 512 KiB"))
			return
		}
		fields[name] = data
	}
	if len(fields) != 3 {
		Failure(c, core.Fail(400, "INVALID_ARGUMENT", "需要完整 CA、客户端证书和私钥"))
		return
	}
	v, e := a.service.ImportCredential(c.Request.Context(), m, c.Param("id"), fields["ca_pem"], fields["client_cert_pem"], fields["client_key_pem"])
	respond(c, v, e)
}
func (a *API) template(c *gin.Context) {
	if _, ok := query(c, "format"); !ok {
		return
	}
	v, e := a.service.DeploymentTemplate(c.Request.Context(), c.Param("id"), strings.TrimSpace(c.Query("format")))
	getResult(c, v, e)
}

func (a *API) channelDiagnostic(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	var in service.ChannelDiagnosticRequest
	if !decode(c, &in) {
		return
	}
	v, e := a.service.ChannelDiagnostic(c.Request.Context(), m, c.Param("id"), in)
	respond(c, v, e)
}

func (a *API) reportRelease(c *gin.Context) {
	m, ok := a.mutation(c, true)
	if !ok {
		return
	}
	var in service.ReleaseReport
	if !decode(c, &in) {
		return
	}
	v, e := a.service.ReportRelease(c.Request.Context(), m, c.Param("id"), in)
	respond(c, v, e)
}
