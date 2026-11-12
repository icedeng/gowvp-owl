package openapi

import (
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/event"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

func (a *API) eventRoutes(g *gin.RouterGroup) {
	g.POST("/events/:id/thumbnail-tickets", func(c *gin.Context) { a.ticket(c, "event-thumbnail", c.Param("id")) })
	g.POST("/events/:id/video-tickets", func(c *gin.Context) {
		var in struct {
			Purpose string `json:"purpose"`
		}
		if err := Body(c, &in); err != nil {
			respond(c, nil, err)
			return
		}
		kind := "event-video"
		if in.Purpose == "download" {
			kind = "event-download"
		} else if in.Purpose != "play" {
			respond(c, nil, op.Fail(400, "INVALID_PURPOSE", "purpose须为play或download"))
			return
		}
		a.ticket(c, kind, c.Param("id"))
	})
	g.GET("/events/:id/actions", func(c *gin.Context) {
		if !require(c, a.S, "event:read") {
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			respond(c, nil, op.ErrNotFound)
			return
		}
		if _, err = a.S.Event(c.Request.Context(), principal(c).App, id); err != nil {
			respond(c, nil, err)
			return
		}
		rows, err := event.NewWorkflow(a.S.DB).Actions(c.Request.Context(), id)
		respond(c, gin.H{"items": rows}, err)
	})
	g.POST("/events/:id/process", func(c *gin.Context) {
		if !require(c, a.S, "event:process") {
			return
		}
		id, err := strconv.ParseInt(c.Param("id"), 10, 64)
		if err != nil || id <= 0 {
			respond(c, nil, op.ErrNotFound)
			return
		}
		var in struct {
			Revision int    `json:"revision"`
			Status   string `json:"status"`
			Note     string `json:"note"`
		}
		if err = Body(c, &in); err != nil {
			respond(c, nil, err)
			return
		}
		if in.Revision <= 0 {
			respond(c, nil, op.Fail(400, "REVISION_REQUIRED", "处理事件需要正整数revision"))
			return
		}
		body, _ := c.Get("open_body")
		raw, _ := body.([]byte)
		fingerprint := op.Hash(append([]byte(c.Request.URL.Path+"?"+c.Request.URL.RawQuery+"\n"), raw...))
		action, err := a.S.ProcessEvent(c.Request.Context(), principal(c), id, in.Revision, in.Status, in.Note, c.GetHeader("Idempotency-Key"), fingerprint)
		respond(c, gin.H{"event_id": id, "action_id": action}, err)
	})
}
