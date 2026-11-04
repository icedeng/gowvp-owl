package openapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/event"
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

func (a *API) page(c *gin.Context, q *gorm.DB, table string) {
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
	respond(c, gin.H{"items": items, "next_cursor": next}, nil)
}
func (a *API) business(g *gin.RouterGroup) {
	a.codeRoutes(g)
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
		q = a.S.DB.Model(&event.Event{})
		visibleChannels := a.S.DeviceQuery(a.S.DB.Model(&ipc.Channel{}).Select("id"), p.App, "did")
		visibleDevices := a.S.DeviceQuery(a.S.DB.Model(&ipc.Device{}).Select("id"), p.App, "id")
		q = q.Where("(cid IN (?) AND (did = '' OR EXISTS (SELECT 1 FROM channels owner_channel WHERE owner_channel.id = events.cid AND owner_channel.did = events.did))) OR (cid = '' AND did IN (?))", visibleChannels, visibleDevices).Select("events.id, events.did, events.cid, events.started_at, events.ended_at, events.label, events.score, events.model")
		if v := c.Query("label"); v != "" {
			q = q.Where("label = ?", v)
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
	a.page(c, q, resource)
}
func (a *API) ticket(c *gin.Context, kind, id string) {
	if kind != "recording" && kind != "download" {
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
	if kind == "live" {
		var row op.Session
		if e = a.S.DB.First(&row, "id = ?", id).Error; e != nil {
			respond(c, nil, e)
			return
		}
		if row.Protocol == "hls" {
			suffix = "hls.fmp4.m3u8"
		} else {
			suffix = "stream.live.flv"
		}
	}
	respond(c, gin.H{"url": a.S.Config.PublicBaseURL + "/openmedia/v1/" + kind + "/" + id + "/" + suffix + "?ticket=" + raw, "expires_at": t.ExpiresAt}, nil)
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
	var in channelSelector
	if e := Body(c, &in); e != nil {
		respond(c, nil, e)
		return
	}
	channel, e := a.inputChannel(c, in)
	if e != nil {
		respond(c, nil, e)
		return
	}
	id, ok := a.idem(c, "snapshot.create")
	if !ok {
		return
	}
	if id != "" {
		row, e := owned[op.Snapshot](a, c, id)
		respond(c, row, e)
		return
	}
	row, e := a.S.CreateSnapshot(principal(c), channel)
	if e != nil {
		respond(c, nil, e)
		return
	}
	if !a.done(c, "snapshot.create", row.ID) {
		return
	}
	a.S.Spawn(func() { a.S.Capture(row.ID) })
	respond(c, row, nil)
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
