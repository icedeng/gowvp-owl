package api

import (
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/capture"
	"github.com/ixugo/goddd/pkg/reason"
	"gorm.io/gorm"
)

type captureScheduleListInput struct {
	Page int    `form:"page"`
	Size int    `form:"size"`
	Key  string `form:"key"`
}

// captureScheduleItem 每行对应一个已启用的通道计划，只投影展示所需字段。
type captureScheduleItem struct {
	capture.Schedule
	DeviceID    string `json:"device_id"`
	DeviceName  string `json:"device_name"`
	DeviceCode  string `json:"device_code"`
	ChannelName string `json:"channel_name"`
	ChannelCode string `json:"channel_code"`
	ChannelType string `json:"channel_type"`
	IsOnline    bool   `json:"is_online"`
}

type captureSchedulePage struct {
	Items           []captureScheduleItem `json:"items"`
	Total           int64                 `json:"total"`
	DeviceCount     int64                 `json:"device_count"`
	StandaloneCount int64                 `json:"standalone_count"`
}

// listCaptureSchedules godoc
// @Summary 分页查询已开启定时抓拍的设备与通道
// @Tags Capture
// @Security BearerAuth
// @Produce json
// @Param page query int false "页码，默认1"
// @Param size query int false "每页数量，默认20，最大100"
// @Param key query string false "设备或通道名称、编码"
// @Success 200 {object} captureSchedulePage
// @Router /capture_schedules [get]
func (a IPCAPI) listCaptureSchedules(c *gin.Context, in *captureScheduleListInput) (*captureSchedulePage, error) {
	if a.uc == nil || a.uc.DB == nil {
		return nil, reason.ErrServer.WithMsg("定时抓拍列表不可用")
	}
	if in.Page < 1 {
		in.Page = 1
	}
	if in.Size < 1 || in.Size > 100 {
		in.Size = 20
	}
	q := a.uc.DB.WithContext(c.Request.Context()).Table("channel_capture_schedules AS s").
		Joins("JOIN channels AS ch ON ch.id = s.channel_id").
		Joins("LEFT JOIN devices AS d ON d.id = ch.did").
		Where("s.enabled = ?", true)
	if key := strings.TrimSpace(in.Key); key != "" {
		// 使用绑定参数，并将通配符视为文字，避免搜索符号意外匹配全量结果。
		key = "%" + strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.ToLower(key)) + "%"
		q = q.Where(`(LOWER(ch.name) LIKE ? ESCAPE '!' OR LOWER(ch.channel_id) LIKE ? ESCAPE '!' OR LOWER(ch.id) LIKE ? ESCAPE '!' OR LOWER(COALESCE(d.name, '')) LIKE ? ESCAPE '!' OR LOWER(COALESCE(d.device_id, ch.device_id)) LIKE ? ESCAPE '!')`, key, key, key, key, key)
	}
	// 计数与分页各自克隆查询，避免 Scan 将联表条件重复写回同一 Statement。
	q = q.Session(&gorm.Session{})
	result := captureSchedulePage{Items: make([]captureScheduleItem, 0)}
	var totals struct{ Total, DeviceCount, StandaloneCount int64 }
	if err := q.Select("COUNT(*) AS total, COUNT(DISTINCT d.id) AS device_count, COALESCE(SUM(CASE WHEN d.id IS NULL THEN 1 ELSE 0 END), 0) AS standalone_count").Scan(&totals).Error; err != nil {
		return nil, captureError(err)
	}
	result.Total, result.DeviceCount, result.StandaloneCount = totals.Total, totals.DeviceCount, totals.StandaloneCount
	if err := q.Select(`s.*, COALESCE(d.id, '') AS device_id, COALESCE(d.name, '') AS device_name,
		COALESCE(d.device_id, ch.device_id) AS device_code, ch.name AS channel_name,
		ch.channel_id AS channel_code, ch.type AS channel_type, ch.is_online AS is_online`).
		Order("COALESCE(d.name, ''), COALESCE(d.id, ''), ch.name, ch.id").
		Offset((in.Page - 1) * in.Size).Limit(in.Size).Scan(&result.Items).Error; err != nil {
		return nil, captureError(err)
	}
	return &result, nil
}
