package openapi

import (
	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/ipc"
)

func (a *API) deviceRecordRoutes(g *gin.RouterGroup) {
	g.POST("/devices/by-code/:device_code/channels/:channel_code/records/query", a.resolveCodes("recording:read", true), a.queryDeviceRecords)
	g.POST("/channels/:id/records/query", a.queryDeviceRecords)
}

func (a *API) queryDeviceRecords(c *gin.Context) {
	if !require(c, a.S, "recording:read") {
		return
	}
	var in struct {
		StartAt int64  `json:"start_at"`
		EndAt   int64  `json:"end_at"`
		Timeout *int   `json:"timeout"`
		Type    string `json:"type"`
	}
	if err := Body(c, &in); err != nil {
		respond(c, nil, err)
		return
	}
	timeout := 10
	if in.Timeout != nil {
		timeout = *in.Timeout
	}
	out, err := a.S.QueryDeviceRecords(c.Request.Context(), principal(c), c.Param("id"), ipc.RecordQueryInput{
		StartAt: in.StartAt, EndAt: in.EndAt, Timeout: timeout, Type: in.Type,
	})
	respond(c, out, err)
}
