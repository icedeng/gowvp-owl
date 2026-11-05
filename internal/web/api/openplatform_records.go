package api

import (
	"context"
	"errors"

	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/pkg/gbs"
)

func (h openHost) QueryDeviceRecords(ctx context.Context, channel string, in *ipc.RecordQueryInput) (*op.DeviceRecords, error) {
	out, err := h.uc.GB28181API.ipc.QueryRecords(ctx, channel, in)
	return openDeviceRecordResult(out, err)
}

func openDeviceRecordResult(out *ipc.RecordQueryOutput, err error) (*op.DeviceRecords, error) {
	if out != nil && out.Data == nil {
		out.Data = []ipc.RecordDate{}
	}
	var partial *gbs.RecordInfoIncompleteError
	if out != nil && errors.As(err, &partial) {
		return &op.DeviceRecords{RecordQueryOutput: out, Complete: false, Incomplete: &op.DeviceRecordIncomplete{
			ReceivedCount: partial.Received, ExpectedCount: partial.Expected,
		}}, nil
	}
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded), errors.Is(err, gbs.ErrRecordQueryTimeout):
			return nil, op.Fail(504, "DEVICE_RECORD_TIMEOUT", "设备录像查询超时，请缩小时间范围后重试")
		case errors.Is(err, gbs.ErrDeviceOffline):
			return nil, op.Fail(409, "DEVICE_OFFLINE", "设备离线，无法查询本地录像")
		case errors.Is(err, gbs.ErrChannelNotExist):
			return nil, op.ErrNotFound
		default:
			// 原始设备错误可能包含信令或内网信息，仅返回固定业务错误。
			return nil, op.Fail(503, "DEVICE_RECORD_QUERY_FAILED", "设备录像查询失败，请确认设备支持录像目录查询")
		}
	}
	if out == nil {
		return nil, op.ErrUnavailable
	}
	return &op.DeviceRecords{RecordQueryOutput: out, Complete: true}, nil
}
