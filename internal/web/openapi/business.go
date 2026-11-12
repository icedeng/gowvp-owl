package openapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/internal/core/recording"
	"gorm.io/gorm"
)

type cursor struct {
	App   string
	Query string
	Last  string
	Exp   int64
}

func (a *API) page(c *gin.Context, q *gorm.DB, table string, decorate ...func([]map[string]any) error) {
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
	var items []map[string]any
	if e = q.Order(table + ".id ASC").Limit(size + 1).Find(&items).Error; e != nil {
		respond(c, nil, e)
		return
	}
	var next any
	if len(items) > size {
		items = items[:size]
		v := cursor{p.App.ID, identity, fmt.Sprint(items[len(items)-1]["id"]), time.Now().Add(time.Hour).Unix()}
		b, _ := json.Marshal(v)
		raw := base64.RawURLEncoding.EncodeToString(b)
		next = raw + "." + op.Sign(a.S.Config.Keys[a.S.Config.KeyID], raw)
	}
	if items == nil {
		items = []map[string]any{}
	}
	for _, fn := range decorate {
		if err := fn(items); err != nil {
			respond(c, nil, err)
			return
		}
	}
	respond(c, gin.H{"items": items, "next_cursor": next}, nil)
}
func (a *API) business(g *gin.RouterGroup) {
	a.codeRoutes(g)
	a.captureRoutes(g)
	a.deviceRecordRoutes(g)
	a.playbackRoutes(g)
	for _, resource := range []string{"live-sessions", "snapshot-tasks", "ptz-commands"} {
		name := resource
		g.GET("/"+name, func(c *gin.Context) { a.listChannelResources(c, name) })
	}
	for _, resource := range []string{"devices", "channels", "recordings", "events"} {
		name := resource
		g.GET("/"+name, func(c *gin.Context) { a.read(c, name, false) })
		g.GET("/"+name+"/:id", func(c *gin.Context) { a.read(c, name, true) })
	}
	g.POST("/live-sessions", a.createLive)
	g.GET("/live-sessions/:session", func(c *gin.Context) {
		if !require(c, a.S, "live:play") {
			return
		}
		row, e := owned[op.Session](a, c, c.Param("session"))
		if e == nil {
			_, e = a.S.Current(row.AppID, row.CredentialID)
		}
		if e == nil {
			_, e = a.S.Channel(principal(c).App, row.ChannelID)
		}
		respond(c, row, e)
	})
	g.DELETE("/live-sessions/:session", func(c *gin.Context) {
		if !require(c, a.S, "live:play") {
			return
		}
		respond(c, gin.H{"released": true}, a.S.ReleaseSession(principal(c).App.ID, c.Param("session")))
	})
	g.POST("/live-sessions/:session/tickets", func(c *gin.Context) { a.ticket(c, "live", c.Param("session")) })
	g.POST("/recordings/:id/tickets", func(c *gin.Context) {
		var in struct {
			Purpose string `json:"purpose"`
		}
		if e := Body(c, &in); e != nil {
			respond(c, nil, e)
			return
		}
		kind := "recording"
		if in.Purpose == "download" {
			kind = "download"
		} else if in.Purpose != "play" {
			respond(c, nil, op.Fail(400, "INVALID_PURPOSE", "purpose须为play或download"))
			return
		}
		a.ticket(c, kind, c.Param("id"))
	})
	g.POST("/events/:id/image-tickets", func(c *gin.Context) { a.ticket(c, "event", c.Param("id")) })
	g.POST("/snapshot-tasks", a.createSnapshot)
	g.GET("/snapshot-tasks/:task", func(c *gin.Context) {
		if !require(c, a.S, "snapshot:capture") {
			return
		}
		row, e := owned[op.Snapshot](a, c, c.Param("task"))
		if e == nil {
			_, e = a.S.Current(row.AppID, row.CredentialID)
		}
		if e == nil {
			_, e = a.S.Channel(principal(c).App, row.ChannelID)
		}
		if e == nil {
			e = a.snapshotAddress(c, &row)
		}
		respond(c, row, e)
	})
	g.POST("/snapshot-tasks/:task/image-tickets", func(c *gin.Context) { a.ticket(c, "snapshot", c.Param("task")) })
	g.GET("/channels/:id/ptz/capabilities", a.ptzCapabilities)
	g.POST("/channels/:id/ptz", a.ptz)
	g.GET("/ptz-commands/:command", func(c *gin.Context) {
		if !require(c, a.S, "ptz:control") {
			return
		}
		row, e := owned[op.Command](a, c, c.Param("command"))
		if e == nil {
			_, e = a.S.Channel(principal(c).App, row.ChannelID)
		}
		respond(c, row, e)
	})
	g.POST("/ptz-commands/:command/stop", func(c *gin.Context) {
		if !require(c, a.S, "ptz:control") {
			return
		}
		respond(c, gin.H{"stop_requested": true}, a.S.StopCommand(c.Request.Context(), principal(c).App.ID, c.Param("command")))
	})
	a.subscriptionRoutes(g)
	a.eventRoutes(g)
}
func (a *API) read(c *gin.Context, resource string, detail bool) {
	p := principal(c)
	scope := map[string]string{"devices": "device:read", "channels": "channel:read", "recordings": "recording:read", "events": "event:read"}[resource]
	if !require(c, a.S, scope) {
		return
	}
	var q *gorm.DB
	switch resource {
	case "devices":
		q = a.S.DeviceQuery(a.S.DB.Model(&ipc.Device{}), p.App, "devices.id").Select("devices.id, devices.device_id, devices.name, devices.type, devices.is_online")
		if v := c.Query("q"); v != "" {
			q = q.Where("name LIKE ? OR device_id LIKE ?", "%"+v+"%", "%"+v+"%")
		}
		if v := c.Query("device_code"); v != "" {
			q = q.Where("devices.device_id = ?", v)
		}
	case "channels":
		q = a.S.DeviceQuery(a.S.DB.Model(&ipc.Channel{}), p.App, "channels.did").Select("channels.id, channels.did, channels.channel_id, channels.name, channels.type, channels.is_online, channels.is_playing, channels.ptztype")
	case "recordings":
		q = a.S.ChannelQuery(a.S.DB.Model(&recording.Recording{}), p.App, "recordings.cid").Where("delete_flag = ?", false).Select("recordings.id, recordings.cid, recordings.started_at, recordings.ended_at, recordings.duration, recordings.size")
	case "events":
		q = a.S.EventQuery(p.App)
		if v := c.Query("label"); v != "" {
			q = q.Where("label = ?", v)
		}
	}
	if resource == "events" {
		if v := c.Query("status"); v != "" {
			if !op.Has([]string{"pending", "processing", "resolved", "ignored"}, v) {
				respond(c, nil, op.Fail(400, "INVALID_STATUS", "事件状态无效"))
				return
			}
			q = q.Where("events.status = ?", v)
		}
		if detail {
			id, err := strconv.ParseInt(c.Param("id"), 10, 64)
			if err != nil || id <= 0 {
				respond(c, nil, op.ErrNotFound)
				return
			}
			row, err := a.S.Event(c.Request.Context(), p.App, id)
			respond(c, row, err)
			return
		}
	}
	if detail {
		var row map[string]any
		e := q.Where(resource+".id = ?", c.Param("id")).Take(&row).Error
		if e == gorm.ErrRecordNotFound {
			e = op.ErrNotFound
		}
		respond(c, row, e)
		return
	}
	if resource != "devices" {
		deviceColumn := map[string]string{"channels": "channels.did", "events": "events.did"}[resource]
		channelColumn := map[string]string{"channels": "channels.id", "events": "events.cid", "recordings": "recordings.cid"}[resource]
		var e error
		q, e = a.resourceFilter(c, q, deviceColumn, channelColumn)
		if e != nil {
			respond(c, nil, e)
			return
		}
	}
	if resource == "events" || resource == "recordings" {
		var e error
		q, e = timeFilter(c, q, resource+".started_at")
		if e != nil {
			respond(c, nil, e)
			return
		}
	}
	if resource == "events" {
		a.page(c, q, resource, func(rows []map[string]any) error { return a.S.FillEventMaps(c.Request.Context(), p.App, rows) })
		return
	}
	a.page(c, q, resource)
}
func (a *API) ticket(c *gin.Context, kind, id string) {
	if kind != "recording" && kind != "download" && kind != "event-video" && kind != "event-download" {
		if e := Body(c, &struct{}{}); e != nil {
			respond(c, nil, e)
			return
		}
	}
	raw, t, e := a.S.IssueTicket(principal(c), kind, id)
	if e != nil {
		respond(c, nil, e)
		return
	}
	suffix := "content"
	if kind == "live" || kind == "playback" {
		var row struct{ Protocol string }
		table := "open_media_sessions"
		if kind == "playback" {
			table = "open_playback_sessions"
		}
		if e = a.S.DB.Table(table).First(&row, "id = ?", id).Error; e != nil {
			respond(c, nil, e)
			return
		}
		if row.Protocol == "hls" {
			suffix = "hls.fmp4.m3u8"
		} else {
			suffix = "stream.live.flv"
		}
	}
	respond(c, gin.H{"url": a.mediaBaseURL(c) + "/openmedia/v1/" + kind + "/" + id + "/" + suffix + "?ticket=" + raw, "expires_at": t.ExpiresAt}, nil)
}
func (a *API) createLive(c *gin.Context) {
	if !require(c, a.S, "live:play") {
		return
	}
	var in struct {
		channelSelector
		Protocol string `json:"protocol"`
	}
	if e := Body(c, &in); e != nil {
		respond(c, nil, e)
		return
	}
	channel, e := a.inputChannel(c, in.channelSelector)
	if e != nil {
		respond(c, nil, e)
		return
	}
	id, ok := a.idem(c, "live.create")
	if !ok {
		return
	}
	if id != "" {
		row, e := owned[op.Session](a, c, id)
		respond(c, row, e)
		return
	}
	row, e := a.S.CreateSession(c.Request.Context(), principal(c), channel, in.Protocol)
	if row.ID != "" {
		if !a.done(c, "live.create", row.ID) {
			return
		}
	}
	respond(c, row, e)
}
func (a *API) createSnapshot(c *gin.Context) {
	if !require(c, a.S, "snapshot:capture") {
		return
	}
	var in struct {
		channelSelector
		Mode           string `json:"mode"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if e := Body(c, &in); e != nil {
		respond(c, nil, e)
		return
	}
	if in.Mode == "" {
		in.Mode = "async"
	}
	if in.Mode != "sync" && in.Mode != "async" {
		respond(c, nil, op.Fail(400, "INVALID_SNAPSHOT_MODE", "mode 须为 sync 或 async"))
		return
	}
	if in.TimeoutSeconds == 0 {
		in.TimeoutSeconds = 60
	}
	if in.TimeoutSeconds < 1 || in.TimeoutSeconds > 120 {
		respond(c, nil, op.Fail(400, "INVALID_SNAPSHOT_TIMEOUT", "timeout_seconds 须为 1—120"))
		return
	}
	channel, e := a.inputChannel(c, in.channelSelector)
	if e != nil {
		respond(c, nil, e)
		return
	}
	id, ok := a.idem(c, "snapshot.create")
	if !ok {
		return
	}
	var row op.Snapshot
	if id != "" {
		row, e = owned[op.Snapshot](a, c, id)
	} else {
		row, e = a.S.CreateSnapshot(principal(c), channel)
		if e == nil {
			// 先启动持久化任务；即使幂等结果写入失败，也不留下永不执行的排队任务。
			taskID := row.ID
			a.S.Spawn(func() { a.S.Capture(taskID) })
			if !a.done(c, "snapshot.create", row.ID) {
				return
			}
		}
	}
	if e != nil {
		respond(c, nil, e)
		return
	}
	if in.Mode == "sync" {
		// 同步等待最长 120 秒，单独延长此入口写超时，不影响其他 API。
		_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(time.Duration(in.TimeoutSeconds+5) * time.Second))
		// 客户端断开只停止等待，已接受的任务继续；同幂等键重试不会再次抓拍。
		deadline := time.NewTimer(time.Duration(in.TimeoutSeconds) * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
	wait:
		for row.State == "queued" || row.State == "running" {
			select {
			case <-c.Request.Context().Done():
				return
			case <-deadline.C:
				respond(c, nil, op.Fail(504, "SNAPSHOT_TIMEOUT", "等待抓拍超时，任务仍在处理；请使用相同幂等键重试或查询任务列表"))
				return
			case <-tick.C:
				row, e = owned[op.Snapshot](a, c, row.ID)
				if e != nil {
					break wait
				}
			}
		}
		if e == nil && row.State != "succeeded" {
			e = op.Fail(503, "SNAPSHOT_FAILED", "抓拍失败或任务已失效，请查询任务状态")
		}
	}
	if e == nil {
		e = a.snapshotAddress(c, &row)
	}
	respond(c, row, e)
}
func (a *API) ptz(c *gin.Context) {
	if !require(c, a.S, "ptz:control") {
		return
	}
	var in op.PTZInput
	if e := Body(c, &in); e != nil {
		respond(c, nil, e)
		return
	}
	in.Action = cleanAction(in.Action)
	id, ok := a.idem(c, "ptz.execute")
	if !ok {
		return
	}
	if id != "" {
		row, e := owned[op.Command](a, c, id)
		respond(c, row, e)
		return
	}
	row, e := a.S.CreateCommand(c.Request.Context(), principal(c), c.Param("id"), in)
	if row.ID != "" {
		if !a.done(c, "ptz.execute", row.ID) {
			return
		}
	}
	respond(c, row, e)
}

func (a *API) ptzCapabilities(c *gin.Context) {
	if !require(c, a.S, "ptz:control") {
		return
	}
	if _, e := a.S.Channel(principal(c).App, c.Param("id")); e != nil {
		respond(c, nil, e)
		return
	}
	caps, e := a.S.Host.Capabilities(c.Request.Context(), c.Param("id"))
	allowed := []string{}
	for _, v := range caps {
		if op.Has(principal(c).App.AllowedActions, v) {
			allowed = append(allowed, v)
		}
	}
	respond(c, gin.H{"actions": allowed, "speed_min": 1, "speed_max": 100, "duration_min": 1, "duration_max": 3}, e)
}
