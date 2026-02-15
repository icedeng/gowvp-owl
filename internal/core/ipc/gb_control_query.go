package ipc

import (
	"context"
	"sort"
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

// GBAppendixA4Snapshot 查询已落库的附录 A.4 扩展对象快照（只读）。
// 过滤规则：
// 1. 设备通过路径参数指定；
// 2. cmd_type 可选，支持逗号分隔多值，大小写不敏感。
func (c Core) GBAppendixA4Snapshot(ctx context.Context, deviceID string, in *GBAppendixA4SnapshotInput) (*GBAppendixA4SnapshotOutput, error) {
	dev, err := c.GetDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	if !dev.IsGB28181() {
		return nil, reason.ErrBadRequest.SetMsg("protocol does not support gb appendix a4 snapshot")
	}
	if in == nil {
		in = &GBAppendixA4SnapshotInput{}
	}

	cmdFilter := parseCmdTypeFilter(in.CmdType)
	limit := in.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}

	items := make([]GBAppendixA4Object, 0, len(dev.Ext.GBAppendixA4))
	for _, item := range dev.Ext.GBAppendixA4 {
		if len(cmdFilter) > 0 {
			cmd := strings.ToUpper(strings.TrimSpace(item.CmdType))
			if _, ok := cmdFilter[cmd]; !ok {
				continue
			}
		}
		items = append(items, item)
	}

	// 按更新时间降序，便于优先查看最新扩展对象。
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].UpdatedAt == items[j].UpdatedAt {
			return items[i].Type < items[j].Type
		}
		return items[i].UpdatedAt > items[j].UpdatedAt
	})
	if len(items) > limit {
		items = items[:limit]
	}

	return &GBAppendixA4SnapshotOutput{
		DeviceID: dev.DeviceID,
		Filter:   strings.TrimSpace(in.CmdType),
		Total:    len(items),
		Items:    items,
	}, nil
}

func parseCmdTypeFilter(in string) map[string]struct{} {
	in = strings.TrimSpace(in)
	if in == "" {
		return nil
	}
	out := make(map[string]struct{}, 4)
	for _, part := range strings.Split(in, ",") {
		v := strings.ToUpper(strings.TrimSpace(part))
		if v == "" {
			continue
		}
		out[v] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
