package openapi

import (
	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/capture"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"time"
)

type captureResult struct {
	capture.Record
	ImageURL       string     `gorm:"-" json:"image_url,omitempty"`
	ImageExpiresAt *time.Time `gorm:"-" json:"image_expires_at,omitempty"`
}

func (a *API) snapshotAddress(c *gin.Context, row *op.Snapshot) error {
	if row.State != "queued" && row.State != "running" && row.State != "succeeded" || !row.ExpiresAt.After(time.Now()) {
		return nil
	}
	if _, err := a.S.Current(row.AppID, row.CredentialID); err != nil {
		return err
	}
	raw, t, err := a.S.IssueTicket(principal(c), "snapshot", row.ID)
	if err != nil {
		return err
	}
	row.ImageURL = a.S.Config.PublicBaseURL + "/openmedia/v1/snapshot/" + row.ID + "/content?ticket=" + raw
	row.ImageExpiresAt = &t.ExpiresAt
	return nil
}
func (a *API) captureAddress(c *gin.Context, row *captureResult) error {
	raw, t, err := a.S.IssueTicket(principal(c), "capture", row.ID)
	if err != nil {
		return err
	}
	row.ImageURL = a.S.Config.PublicBaseURL + "/openmedia/v1/capture/" + row.ID + "/content?ticket=" + raw
	row.ImageExpiresAt = &t.ExpiresAt
	return nil
}
func (a *API) captureRoutes(g *gin.RouterGroup) {
	g.GET("/captures", a.listCaptures)
	g.GET("/captures/:capture", func(c *gin.Context) {
		if !require(c, a.S, "snapshot:capture") {
			return
		}
		var row captureResult
		err := a.S.ChannelQuery(a.S.DB.Model(&capture.Record{}), principal(c).App, "channel_captures.channel_id").Where("id = ? AND state = ?", c.Param("capture"), "succeeded").Take(&row).Error
		if err != nil {
			respond(c, nil, op.ErrNotFound)
			return
		}
		err = a.captureAddress(c, &row)
		respond(c, row, err)
	})
	g.POST("/captures/:capture/image-tickets", func(c *gin.Context) { a.ticket(c, "capture", c.Param("capture")) })
	device := "/devices/by-code/:device_code"
	g.GET(device+"/captures", a.resolveCodes("snapshot:capture", false), a.listCaptures)
	g.GET(device+"/channels/:channel_code/captures", a.resolveCodes("snapshot:capture", true), a.listCaptures)
	g.GET("/channels/:id/captures", func(c *gin.Context) {
		if !require(c, a.S, "snapshot:capture") {
			return
		}
		ch, err := a.S.Channel(principal(c).App, c.Param("id"))
		if err != nil {
			respond(c, nil, err)
			return
		}
		c.Set("open_resolved_device", ch.DID)
		c.Set("open_resolved_channel", ch.ID)
		a.listCaptures(c)
	})
}
func (a *API) listCaptures(c *gin.Context) {
	if !require(c, a.S, "snapshot:capture") {
		return
	}
	q := a.S.ChannelQuery(a.S.DB.Model(&capture.Record{}), principal(c).App, "channel_captures.channel_id").Where("channel_captures.state = ?", "succeeded")
	q, err := a.resourceFilter(c, q, "", "channel_captures.channel_id")
	// 永久图片默认列出全部历史；只有显式时间参数才限制区间。
	if err == nil && (c.Query("start") != "" || c.Query("end") != "") {
		q, err = timeFilter(c, q, "channel_captures.created_at")
	}
	if err != nil {
		respond(c, nil, err)
		return
	}
	resourcePage[captureResult](a, c, q, "channel_captures", func(v captureResult) string { return v.ID }, func(v *captureResult) error { return a.captureAddress(c, v) })
}
