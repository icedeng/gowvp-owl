package ipc

import (
	"context"
	"strings"

	"github.com/ixugo/goddd/pkg/reason"
)

func (c Core) PTZControl(ctx context.Context, channelID string, in *PTZControlInput) error {
	if in == nil {
		return reason.ErrBadRequest.SetMsg("invalid ptz request")
	}
	ch, err := c.GetChannel(ctx, channelID)
	if err != nil {
		return err
	}
	dev, err := c.GetDevice(ctx, ch.DID)
	if err != nil {
		return err
	}

	protocol, ok := c.protocols[dev.GetType()]
	if !ok {
		return reason.ErrBadRequest.SetMsg("unsupported protocol")
	}
	ptz, ok := protocol.(PTZCapable)
	if !ok {
		return reason.ErrBadRequest.SetMsg("protocol does not support ptz")
	}

	in.Action = strings.ToLower(strings.TrimSpace(in.Action))
	if in.Action == "" {
		return reason.ErrBadRequest.SetMsg("action is required")
	}
	if strings.Contains(in.Action, "preset") && (in.Preset < 1 || in.Preset > 255) {
		return reason.ErrBadRequest.SetMsg("preset must be in [1,255]")
	}
	if in.Speed == 0 {
		in.Speed = 40
	}
	if in.Timeout <= 0 {
		in.Timeout = 6
	}

	return ptz.PTZControl(ctx, dev, ch, in)
}
