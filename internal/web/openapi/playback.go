package openapi

import (
	"github.com/gin-gonic/gin"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

func (a *API) playbackRoutes(g *gin.RouterGroup) {
	g.POST("/playback-sessions", a.createPlayback)
	channel := "/devices/by-code/:device_code/channels/:channel_code"
	g.POST(channel+"/playback-sessions", a.resolveCodes("recording:play", true), a.createPlayback)
	for _, prefix := range []string{"/devices/by-code/:device_code", channel} {
		g.GET(prefix+"/playback-sessions", a.resolveCodes("recording:play", prefix == channel), func(c *gin.Context) { a.listPlaybacks(c) })
	}
	g.GET("/playback-sessions", a.listPlaybacks)
	g.GET("/playback-sessions/:session", func(c *gin.Context) {
		if !require(c, a.S, "recording:play") {
			return
		}
		row, err := a.S.Playback(principal(c), c.Param("session"))
		respond(c, row, err)
	})
	g.POST("/playback-sessions/:session/tickets", func(c *gin.Context) { a.ticket(c, "playback", c.Param("session")) })
	g.POST("/playback-sessions/:session/control", a.controlPlayback)
	g.DELETE("/playback-sessions/:session", func(c *gin.Context) {
		if !require(c, a.S, "recording:play") {
			return
		}
		err := a.S.StopPlayback(principal(c), c.Param("session"))
		respond(c, gin.H{"stopped": true}, err)
	})
}

func (a *API) createPlayback(c *gin.Context) {
	if !require(c, a.S, "recording:play") {
		return
	}
	var in struct {
		channelSelector
		op.PlaybackInput
	}
	if err := Body(c, &in); err != nil {
		respond(c, nil, err)
		return
	}
	channel, err := a.inputChannel(c, in.channelSelector)
	if err == nil {
		err = op.ValidatePlaybackInput(&in.PlaybackInput)
	}
	if err != nil {
		respond(c, nil, err)
		return
	}
	id, ok := a.idem(c, "playback.create")
	if !ok {
		return
	}
	if id != "" {
		row, err := a.S.Playback(principal(c), id)
		respond(c, row, err)
		return
	}
	row, err := a.S.CreatePlayback(c.Request.Context(), principal(c), channel, in.PlaybackInput)
	if err != nil {
		respond(c, nil, err)
		return
	}
	// 先绑定幂等结果，后台启动只执行一次；即使响应丢失也能使用原键查到会话。
	done := a.done(c, "playback.create", row.ID)
	a.S.Spawn(func() { a.S.StartPlayback(row.ID) })
	if done {
		respond(c, row, nil)
	}
}

func (a *API) listPlaybacks(c *gin.Context) {
	if !require(c, a.S, "recording:play") {
		return
	}
	const table = "open_playback_sessions"
	q := a.S.ChannelQuery(a.S.DB.Model(&op.Playback{}), principal(c).App, table+".channel_id").Where("app_id = ?", principal(c).App.ID)
	q, err := a.resourceFilter(c, q, "", table+".channel_id")
	if err == nil {
		q, err = timeFilter(c, q, table+".created_at")
	}
	if err != nil {
		respond(c, nil, err)
		return
	}
	if state := c.Query("state"); state != "" {
		q = q.Where("state = ?", state)
	}
	resourcePage[op.Playback](a, c, q, table, func(v op.Playback) string { return v.ID })
}

func (a *API) controlPlayback(c *gin.Context) {
	if !require(c, a.S, "recording:play") {
		return
	}
	var in op.PlaybackControl
	if err := Body(c, &in); err != nil {
		respond(c, nil, err)
		return
	}
	operation := "playback.control:" + c.Param("session")
	id, ok := a.idem(c, operation)
	if !ok {
		return
	}
	if id != "" {
		row, err := a.S.Playback(principal(c), id)
		respond(c, row, err)
		return
	}
	row, err := a.S.ControlPlayback(c.Request.Context(), principal(c), c.Param("session"), in)
	if err == nil && !a.done(c, operation, row.ID) {
		return
	}
	respond(c, row, err)
}
