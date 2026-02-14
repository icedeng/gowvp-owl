package ipc

import (
	"context"

	"github.com/ixugo/goddd/pkg/reason"
)

// QueryRecords 按通道查询录像目录。
// 由 Core 根据设备协议分发给支持 RecordQueryable 的适配器实现。
func (c Core) QueryRecords(ctx context.Context, channelID string, in *RecordQueryInput) (*RecordQueryOutput, error) {
	if in == nil {
		return nil, reason.ErrBadRequest.SetMsg("invalid record query request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return nil, err
	}

	protocol, ok := c.protocols[dev.GetType()]
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	queryable, ok := protocol.(RecordQueryable)
	if !ok {
		return nil, reason.ErrBadRequest.SetMsg("protocol does not support record query")
	}

	if in.StartAt <= 0 || in.EndAt <= in.StartAt {
		return nil, reason.ErrBadRequest.SetMsg("invalid record query time range")
	}
	if in.Timeout <= 0 {
		in.Timeout = 10
	}

	return queryable.QueryRecords(ctx, dev, ch, in)
}
