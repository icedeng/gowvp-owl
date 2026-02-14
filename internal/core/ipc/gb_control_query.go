package ipc

import (
	"context"
	"strings"

	"github.com/ixugo/goddd/pkg/reason"
)

// GBDeviceControl 执行 GB 附录 A.2.3 统一设备控制。
func (c Core) GBDeviceControl(ctx context.Context, deviceID string, in *GBDeviceControlInput) (*GBDeviceControlOutput, error) {
	if in == nil {
		return nil, reason.ErrBadRequest.SetMsg("invalid gb device control request")
	}
	dev, err := c.GetDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	protocol, ok := c.protocols[dev.GetType()]
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	ctrl, ok := protocol.(GBDeviceControlCapable)
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("protocol does not support gb device control")
	}

	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	if in.Action == "" {
		return nil, reason.ErrBadRequest.SetMsg("action is required")
	}
	if in.Timeout <= 0 {
		in.Timeout = 6
	}
	if strings.TrimSpace(in.TargetID) == "" {
		in.TargetID = dev.DeviceID
	}
	return ctrl.DeviceControl(ctx, dev, in)
}

// GBDeviceQuery 执行 GB 附录 A.2.4 统一设备查询。
func (c Core) GBDeviceQuery(ctx context.Context, deviceID string, in *GBDeviceQueryInput) (*GBDeviceQueryOutput, error) {
	if in == nil {
		return nil, reason.ErrBadRequest.SetMsg("invalid gb device query request")
	}
	dev, err := c.GetDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	protocol, ok := c.protocols[dev.GetType()]
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	query, ok := protocol.(GBDeviceQueryCapable)
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("protocol does not support gb device query")
	}

	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	if in.Action == "" {
		return nil, reason.ErrBadRequest.SetMsg("action is required")
	}
	if in.Action == "record_info" || in.Action == "record_query" || in.Action == "file_query" {
		if strings.TrimSpace(in.TargetID) == "" {
			return nil, reason.ErrBadRequest.SetMsg("record_info requires target_id(channel id)")
		}
		if in.Start <= 0 || in.End <= in.Start {
			return nil, reason.ErrBadRequest.SetMsg("record_info requires valid start/end")
		}
	}
	if in.Timeout <= 0 {
		in.Timeout = 6
	}
	if strings.TrimSpace(in.TargetID) == "" {
		in.TargetID = dev.DeviceID
	}
	return query.DeviceQuery(ctx, dev, in)
}
