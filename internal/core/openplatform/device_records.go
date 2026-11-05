package openplatform

import (
	"context"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
)

type DeviceRecordIncomplete struct {
	ReceivedCount int `json:"received_count"`
	ExpectedCount int `json:"expected_count"`
}

// DeviceRecords 仅返回录像时间目录，不暴露设备文件路径或原始 SIP/XML。
type DeviceRecords struct {
	*ipc.RecordQueryOutput
	Complete   bool                    `json:"complete"`
	Incomplete *DeviceRecordIncomplete `json:"incomplete,omitempty"`
}

// DeviceRecordQuerier 为宿主可选能力，保持现有 Host 适配器兼容。
type DeviceRecordQuerier interface {
	QueryDeviceRecords(context.Context, string, *ipc.RecordQueryInput) (*DeviceRecords, error)
}

func (s *Service) QueryDeviceRecords(ctx context.Context, p Principal, channel string, in ipc.RecordQueryInput) (*DeviceRecords, error) {
	p, err := s.Validate(p, "recording:read", channel)
	if err != nil {
		return nil, err
	}
	if channel == "" {
		return nil, ErrNotFound
	}
	if in.StartAt <= 0 || in.EndAt <= in.StartAt || in.EndAt-in.StartAt > 7*24*60*60 {
		return nil, Fail(400, "INVALID_TIME_RANGE", "start_at/end_at须为Unix秒，结束时间大于开始时间，跨度不超过7天")
	}
	if in.Timeout < 1 || in.Timeout > 60 {
		return nil, Fail(400, "INVALID_RECORD_TIMEOUT", "timeout须为1至60秒")
	}
	if in.Type == "" {
		in.Type = "all"
	}
	if !Has([]string{"time", "alarm", "manual", "all"}, in.Type) {
		return nil, Fail(400, "INVALID_RECORD_TYPE", "type须为time、alarm、manual或all")
	}
	ch, err := s.Channel(p.App, channel)
	if err != nil {
		return nil, err
	}
	var device ipc.Device
	if err = s.DB.WithContext(ctx).First(&device, "id = ?", ch.DID).Error; err != nil {
		return nil, ErrUnavailable
	}
	if device.GetType() != ipc.TypeGB28181 {
		return nil, Fail(400, "UNSUPPORTED_RECORD_QUERY", "当前仅支持GB28181设备的本地录像目录查询")
	}
	if !device.IsOnline {
		return nil, Fail(409, "DEVICE_OFFLINE", "设备离线，无法查询本地录像")
	}
	host, ok := s.Host.(DeviceRecordQuerier)
	if !ok {
		return nil, Fail(503, "RECORD_QUERY_UNAVAILABLE", "当前服务未提供设备录像查询能力")
	}
	release, err := s.AcquireConnection(p, "device-record-query")
	if err != nil {
		return nil, err
	}
	defer release()
	// 保留设备分包等待期限，另给 SIP 信令阶段少量余量；客户端断开会取消查询。
	ctx, cancel := context.WithTimeout(ctx, time.Duration(in.Timeout+5)*time.Second)
	defer cancel()
	out, queryErr := host.QueryDeviceRecords(ctx, channel, &in)
	// 查询等待期间的停用、凭据撤销和设备撤权不能继续返回录像目录。
	if _, err = s.Validate(p, "recording:read", channel); err != nil {
		return nil, err
	}
	return out, queryErr
}
