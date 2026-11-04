package openapi

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"gorm.io/gorm"
)

// 编码入口只解析当前应用可访问的资源，再复用内部 ID 入口的业务流程。
func (a *API) codeRoutes(g *gin.RouterGroup) {
	device := "/devices/by-code/:device_code"
	channel := device + "/channels/:channel_code"
	g.GET(device, a.resolveCodes("device:read", false), func(c *gin.Context) { a.read(c, "devices", true) })
	g.GET(device+"/channels", a.resolveCodes("channel:read", false), func(c *gin.Context) { a.read(c, "channels", false) })
	g.GET(channel, a.resolveCodes("channel:read", true), func(c *gin.Context) { a.read(c, "channels", true) })
	for _, resource := range []string{"recordings", "events", "live-sessions", "snapshot-tasks", "ptz-commands"} {
		name := resource
		scope := map[string]string{"recordings": "recording:read", "events": "event:read", "live-sessions": "live:play", "snapshot-tasks": "snapshot:capture", "ptz-commands": "ptz:control"}[name]
		handler := func(c *gin.Context) {
			if name == "recordings" || name == "events" {
				a.read(c, name, false)
			} else {
				a.listChannelResources(c, name)
			}
		}
		g.GET(device+"/"+name, a.resolveCodes(scope, false), handler)
		g.GET(channel+"/"+name, a.resolveCodes(scope, true), handler)
	}
	g.GET(channel+"/ptz/capabilities", a.resolveCodes("ptz:control", true), a.ptzCapabilities)
	g.POST(channel+"/ptz", a.resolveCodes("ptz:control", true), a.ptz)
	g.POST(channel+"/live-sessions", a.resolveCodes("live:play", true), a.createLive)
	g.POST(channel+"/snapshot-tasks", a.resolveCodes("snapshot:capture", true), a.createSnapshot)
}

func validResourceCode(value string) bool {
	return value != "" && len(value) <= 256 && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func (a *API) resolveCodes(scope string, channel bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !require(c, a.S, scope) {
			return
		}
		deviceCode, channelCode := c.Param("device_code"), c.Param("channel_code")
		deviceID, channelID, err := a.resourceByCodes(principal(c).App, deviceCode, channelCode, channel)
		if err != nil {
			respond(c, nil, err)
			return
		}
		c.Set("open_resolved_device", deviceID)
		id := deviceID
		if channel {
			id = channelID
			c.Set("open_resolved_channel", id)
		}
		c.Params = append(c.Params, gin.Param{Key: "id", Value: id})
		c.Next()
	}
}

func (a *API) resourceByCodes(app op.App, deviceCode, channelCode string, channel bool) (string, string, error) {
	if !validResourceCode(deviceCode) || channel && !validResourceCode(channelCode) {
		return "", "", op.Fail(400, "INVALID_RESOURCE_CODE", "设备编码或通道编码格式不正确")
	}
	var device ipc.Device
	err := a.S.DeviceQuery(a.S.DB.Model(&ipc.Device{}), app, "devices.id").Select("devices.id").Where("devices.device_id = ?", deviceCode).Take(&device).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			err = op.ErrNotFound
		}
		return "", "", err
	}
	if !channel {
		return device.ID, "", nil
	}
	var matches []ipc.Channel
	// 以父设备归属为准，不信任通道上冗余的 device_id，也不按排序猜测通道号。
	if err = a.S.DB.Select("id").Where("did = ? AND channel_id = ?", device.ID, channelCode).Limit(2).Find(&matches).Error; err != nil {
		return "", "", err
	}
	if len(matches) == 0 {
		return "", "", op.ErrNotFound
	}
	if len(matches) > 1 {
		return "", "", op.Fail(409, "AMBIGUOUS_RESOURCE_CODE", "同一设备下的通道编码不唯一，请联系管理员核对")
	}
	return device.ID, matches[0].ID, nil
}

type channelSelector struct {
	ChannelID   string `json:"channel_id"`
	DeviceCode  string `json:"device_code"`
	ChannelCode string `json:"channel_code"`
}

func (a *API) inputChannel(c *gin.Context, in channelSelector) (string, error) {
	if resolved := c.GetString("open_resolved_channel"); resolved != "" {
		if in.ChannelID != "" || in.DeviceCode != "" || in.ChannelCode != "" {
			return "", op.Fail(400, "INVALID_RESOURCE_SELECTOR", "编码路径已确定通道，请勿同时传入其他资源定位字段")
		}
		return resolved, nil
	}
	if in.DeviceCode != "" || in.ChannelCode != "" {
		if in.ChannelID != "" || in.DeviceCode == "" || in.ChannelCode == "" {
			return "", op.Fail(400, "INVALID_RESOURCE_SELECTOR", "请使用内部 channel_id 或完整的 device_code、channel_code，不能混用")
		}
		_, id, err := a.resourceByCodes(principal(c).App, in.DeviceCode, in.ChannelCode, true)
		return id, err
	}
	return in.ChannelID, nil
}

// 列表筛选使用子查询，未知或未授权编码返回空列表；编码路径则严格解析并返回 404。
// 所有筛选条件取交集，绝不将未知编码忽略后退化成全量查询。
func (a *API) resourceFilter(c *gin.Context, q *gorm.DB, deviceColumn, channelColumn string) (*gorm.DB, error) {
	deviceID, channelID := c.Query("device_id"), c.Query("channel_id")
	deviceCode, channelCode := c.Query("device_code"), c.Query("channel_code")
	if resolved := c.GetString("open_resolved_device"); resolved != "" {
		for _, key := range []string{"device_id", "channel_id", "device_code", "channel_code"} {
			if _, exists := c.GetQuery(key); exists {
				return q, op.Fail(400, "INVALID_RESOURCE_SELECTOR", "编码路径已确定资源范围，请勿同时传入资源筛选参数")
			}
		}
		deviceID, channelID = resolved, c.GetString("open_resolved_channel")
	}
	for _, key := range []string{"device_code", "channel_code"} {
		if value, exists := c.GetQuery(key); exists && !validResourceCode(value) {
			return q, op.Fail(400, "INVALID_RESOURCE_CODE", "设备编码或通道编码格式不正确")
		}
	}
	channels := func() *gorm.DB { return a.S.DB.Model(&ipc.Channel{}) }
	byDevice := func(sub *gorm.DB) {
		if deviceColumn == "" {
			q = q.Where(channelColumn+" IN (?)", channels().Select("id").Where("did IN (?)", sub))
		} else if deviceColumn == "events.did" {
			q = q.Where("(events.did IN (?) OR events.cid IN (?))", sub, channels().Select("id").Where("did IN (?)", sub))
		} else {
			q = q.Where(deviceColumn+" IN (?)", sub)
		}
	}
	if deviceID != "" {
		byDevice(a.S.DB.Model(&ipc.Device{}).Select("id").Where("id = ?", deviceID))
	}
	if deviceCode != "" {
		byDevice(a.S.DB.Model(&ipc.Device{}).Select("id").Where("device_id = ?", deviceCode))
	}
	if channelID != "" {
		q = q.Where(channelColumn+" = ?", channelID)
	}
	if channelCode != "" {
		q = q.Where(channelColumn+" IN (?)", channels().Select("id").Where("channel_id = ?", channelCode))
	}
	return q, nil
}

func (a *API) listChannelResources(c *gin.Context, resource string) {
	scope := map[string]string{"live-sessions": "live:play", "snapshot-tasks": "snapshot:capture", "ptz-commands": "ptz:control"}[resource]
	if !require(c, a.S, scope) {
		return
	}
	table := map[string]string{"live-sessions": "open_media_sessions", "snapshot-tasks": "open_snapshot_tasks", "ptz-commands": "open_ptz_commands"}[resource]
	q := a.S.ChannelQuery(a.S.DB.Table(table), principal(c).App, table+".channel_id").Where(table+".app_id = ?", principal(c).App.ID)
	q, err := a.resourceFilter(c, q, "", table+".channel_id")
	if err == nil {
		q, err = timeFilter(c, q, table+".created_at")
	}
	if err != nil {
		respond(c, nil, err)
		return
	}
	if state := c.Query("state"); state != "" {
		q = q.Where(table+".state = ?", state)
	}
	switch resource {
	case "live-sessions":
		resourcePage[op.Session](a, c, q, table, func(v op.Session) string { return v.ID })
	case "snapshot-tasks":
		resourcePage[op.Snapshot](a, c, q, table, func(v op.Snapshot) string { return v.ID })
	case "ptz-commands":
		resourcePage[op.Command](a, c, q, table, func(v op.Command) string { return v.ID })
	}
}
