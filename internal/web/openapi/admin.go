package openapi

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"gorm.io/gorm"
)

func (a *API) admin(g *gin.RouterGroup) {
	g.GET("/devices", func(c *gin.Context) {
		q := a.S.DB.Model(&ipc.Device{}).Select("id, name, device_id, type, is_online, created_at")
		if v := c.Query("q"); v != "" {
			q = q.Where("name LIKE ? OR id LIKE ? OR device_id LIKE ?", "%"+v+"%", "%"+v+"%", "%"+v+"%")
		}
		a.list(c, q, &[]map[string]any{})
	})
	g.GET("/apps/:app/authorized-devices", func(c *gin.Context) {
		app, e := get[op.App](a.S.DB, c.Param("app"))
		if e != nil {
			respond(c, nil, e)
			return
		}
		items := []gin.H{}
		var rows []ipc.Device
		if len(app.DeviceIDs) > 0 {
			if e = a.S.DB.Where("id IN ?", app.DeviceIDs).Find(&rows).Error; e != nil {
				respond(c, nil, e)
				return
			}
		}
		byID := map[string]ipc.Device{}
		for _, v := range rows {
			byID[v.ID] = v
		}
		for _, id := range app.DeviceIDs {
			v, ok := byID[id]
			items = append(items, gin.H{"id": id, "name": v.Name, "device_id": v.DeviceID, "type": v.Type, "is_online": v.IsOnline, "deleted": !ok})
		}
		respond(c, gin.H{"mode": app.DeviceAccessMode, "items": items}, nil)
	})
	g.GET("/status", func(c *gin.Context) {
		respond(c, gin.H{"enabled": true, "allow_http": a.S.Config.AllowHTTP, "audience": a.S.Config.Audience, "public_url": a.S.Config.PublicBaseURL, "scopes": op.Scopes, "default_actions": op.BasicActions, "deployment": "single-instance"}, nil)
	})
	g.GET("/apps", func(c *gin.Context) {
		q := a.S.DB.Model(&op.App{})
		if v := c.Query("q"); v != "" {
			q = q.Where("name LIKE ? OR organization LIKE ?", "%"+v+"%", "%"+v+"%")
		}
		a.list(c, q, &[]op.App{})
	})
	g.POST("/apps", func(c *gin.Context) { a.saveApp(c, true) })
	g.GET("/apps/:app", func(c *gin.Context) { row, e := get[op.App](a.S.DB, c.Param("app")); respond(c, row, e) })
	g.PATCH("/apps/:app", func(c *gin.Context) { a.saveApp(c, false) })
	g.PUT("/apps/:app/authorization", func(c *gin.Context) { a.saveApp(c, false) })
	g.PUT("/apps/:app/access-policy", func(c *gin.Context) { a.saveApp(c, false) })
	for _, name := range []string{"enable", "disable", "archive"} {
		action := name
		g.POST("/apps/:app/"+action, func(c *gin.Context) {
			states := map[string]string{"enable": "enabled", "disable": "disabled", "archive": "archived"}
			e := a.S.SetStatus(c.Param("app"), states[action], c.GetString("open_admin"))
			respond(c, gin.H{"status": states[action]}, e)
		})
	}
	g.GET("/apps/:app/credentials", func(c *gin.Context) {
		a.list(c, a.S.DB.Model(&op.Credential{}).Where("app_id = ?", c.Param("app")), &[]op.Credential{})
	})
	g.POST("/apps/:app/credentials", func(c *gin.Context) {
		if !a.requireSecretTransport(c) {
			return
		}
		var in struct {
			ExpiresAt *time.Time `json:"expires_at"`
		}
		if e := Body(c, &in); e != nil {
			respond(c, nil, e)
			return
		}
		row, secret, e := a.S.CreateCredential(c.Param("app"), in.ExpiresAt, c.GetString("open_admin"))
		respond(c, gin.H{"credential": row, "secret_key": secret}, e)
	})
	g.POST("/apps/:app/credentials/:credential/revoke", func(c *gin.Context) {
		respond(c, gin.H{"revoked": true}, a.S.Revoke(c.Param("app"), c.Param("credential"), c.GetString("open_admin")))
	})
	g.GET("/audit-logs", func(c *gin.Context) {
		q := a.S.DB.Model(&op.Audit{})
		if v := c.Query("app_id"); v != "" {
			q = q.Where("app_id = ?", v)
		}
		if v := c.Query("action"); v != "" {
			q = q.Where("action = ?", v)
		}
		if c.Query("start") != "" || c.Query("end") != "" {
			var e error
			q, e = timeFilter(c, q, "created_at")
			if e != nil {
				respond(c, nil, e)
				return
			}
		}
		if v := c.Query("status"); v != "" {
			status, e := strconv.Atoi(v)
			if e != nil || status < 100 || status > 599 {
				respond(c, nil, op.Fail(400, "INVALID_STATUS", "HTTP状态不合法"))
				return
			}
			q = q.Where("status = ?", status)
		}
		a.list(c, q, &[]op.Audit{})
	})
	g.GET("/apps/:app/callback-targets", func(c *gin.Context) {
		a.list(c, a.S.DB.Model(&op.CallbackTarget{}).Where("app_id = ?", c.Param("app")), &[]op.CallbackTarget{})
	})
	g.POST("/apps/:app/callback-targets", a.saveTarget)
	g.PATCH("/apps/:app/callback-targets/:target", a.saveTarget)
	g.GET("/apps/:app/subscriptions", func(c *gin.Context) {
		a.list(c, a.S.DB.Model(&op.Subscription{}).Where("app_id = ?", c.Param("app")), &[]op.Subscription{})
	})
	g.GET("/apps/:app/deliveries", func(c *gin.Context) {
		q := a.S.DB.Model(&op.Delivery{}).Where("app_id = ?", c.Param("app"))
		if v := c.Query("state"); v != "" {
			q = q.Where("state = ?", v)
		}
		a.list(c, q, &[]op.Delivery{})
	})
	g.POST("/apps/:app/deliveries/:delivery/retry", func(c *gin.Context) {
		respond(c, gin.H{"queued": true}, a.S.Retry(c.Param("app"), "", c.Param("delivery"), c.GetString("open_admin")))
	})
	g.GET("/apps/:app/snapshot-tasks", func(c *gin.Context) {
		a.list(c, a.S.DB.Model(&op.Snapshot{}).Where("app_id = ?", c.Param("app")), &[]op.Snapshot{})
	})
	g.GET("/apps/:app/ptz-commands", func(c *gin.Context) {
		a.list(c, a.S.DB.Model(&op.Command{}).Where("app_id = ?", c.Param("app")), &[]op.Command{})
	})
	g.POST("/apps/:app/ptz-commands/:command/stop", func(c *gin.Context) {
		respond(c, gin.H{"stop_requested": true}, a.S.StopCommand(c.Request.Context(), c.Param("app"), c.Param("command")))
	})
}
func (a *API) saveApp(c *gin.Context, create bool) {
	row := op.DefaultApp()
	if !create {
		var e error
		row, e = get[op.App](a.S.DB, c.Param("app"))
		if e != nil {
			respond(c, nil, e)
			return
		}
	}
	var values map[string]json.RawMessage
	if e := Body(c, &values); e != nil {
		respond(c, nil, e)
		return
	}
	allowed := []string{"name", "organization", "owner", "contact", "purpose", "expires_at", "scopes", "device_access_mode", "device_ids", "allowed_actions", "allow_any_ip", "cidrs", "qps", "burst", "live_limit", "channel_limit", "download_limit", "snapshot_limit", "subscription_limit", "delivery_limit", "revision"}
	for k := range values {
		if !op.Has(allowed, k) {
			respond(c, nil, op.Fail(400, "UNKNOWN_FIELD", "不支持的应用字段"))
			return
		}
	}
	if !create {
		if _, ok := values["revision"]; !ok {
			respond(c, nil, op.Fail(400, "REVISION_REQUIRED", "修改应用需要revision"))
			return
		}
	}
	raw, _ := json.Marshal(values)
	if e := json.Unmarshal(raw, &row); e != nil {
		respond(c, nil, op.Fail(400, "INVALID_APP", "应用字段格式错误"))
		return
	}
	saved, e := a.S.SaveApp(row, create, c.GetString("open_admin"))
	respond(c, saved, e)
}
func (a *API) saveTarget(c *gin.Context) {
	var in struct {
		URL      string   `json:"url"`
		CIDRs    []string `json:"cidrs"`
		Status   string   `json:"status"`
		Revision int64    `json:"revision"`
	}
	if e := Body(c, &in); e != nil {
		respond(c, nil, e)
		return
	}
	if in.Status == "" {
		in.Status = "pending"
	}
	if !op.Has([]string{"pending", "approved", "disabled"}, in.Status) {
		respond(c, nil, op.Fail(400, "INVALID_STATUS", "回调目标状态不合法"))
		return
	}
	row, e := a.S.SaveTarget(c.Param("app"), c.Param("target"), in.URL, in.CIDRs, in.Status, in.Revision, c.GetString("open_admin"))
	respond(c, row, e)
}
func owned[T any](a *API, c *gin.Context, id string) (T, error) {
	return get[T](a.S.DB.Where("app_id = ?", principal(c).App.ID), id)
}
func timeFilter(c *gin.Context, q *gorm.DB, column string) (*gorm.DB, error) {
	end := time.Now()
	start := end.Add(-24 * time.Hour)
	var e error
	if v := c.Query("end"); v != "" {
		end, e = time.Parse(time.RFC3339, v)
		if e != nil {
			return q, op.Fail(400, "INVALID_TIME", "end必须为RFC3339时间")
		}
		start = end.Add(-24 * time.Hour)
	}
	if v := c.Query("start"); v != "" {
		start, e = time.Parse(time.RFC3339, v)
		if e != nil {
			return q, op.Fail(400, "INVALID_TIME", "start必须为RFC3339时间")
		}
	}
	if !end.After(start) || end.Sub(start) > 7*24*time.Hour {
		return q, op.Fail(400, "INVALID_TIME", "查询跨度须大于0且不超过7天")
	}
	return q.Where(column+" >= ? AND "+column+" <= ?", start, end), nil
}
func redactError(e error) string {
	if e == nil {
		return ""
	}
	if v, ok := e.(*op.Error); ok {
		return v.Message
	}
	return "设备或依赖服务操作失败"
}
func cleanAction(v string) string { return strings.ToLower(strings.TrimSpace(v)) }
