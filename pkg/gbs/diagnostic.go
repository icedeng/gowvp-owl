package gbs

import (
	"context"
	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"time"
)

type diagnosticTraceKey struct{}
type DiagnosticTrace struct {
	StreamID      string
	SIPDurationMS *int64
	SIPError      bool
}

func traceSIP(ctx context.Context, started time.Time, failed bool) {
	if t, ok := ctx.Value(diagnosticTraceKey{}).(*DiagnosticTrace); ok {
		n := time.Since(started).Milliseconds()
		t.SIPDurationMS = &n
		t.SIPError = failed
	}
}

// DiagnosticPlay 使用独立会话/流，清理时不停止正在观看或取证的普通直播。
func (g *GB28181API) DiagnosticPlay(ctx context.Context, ch *ipc.Channel, ms *sms.MediaServer, mode int8) (*DiagnosticTrace, func(context.Context) error, error) {
	trace := &DiagnosticTrace{StreamID: "diag_" + uuid.NewString()}
	key := "diagnostic:" + trace.StreamID
	cleanup := func(ctx context.Context) error {
		return g.StopPlay(ctx, &StopPlayInput{Channel: ch, sessionKey: key, skipLinkedVoice: true})
	}
	err := g.PlayContext(context.WithValue(ctx, diagnosticTraceKey{}, trace), &PlayInput{Channel: ch, SMS: ms, StreamMode: mode, sessionKey: key, streamID: trace.StreamID})
	return trace, cleanup, err
}

func (s *Server) DiagnosticPlay(ctx context.Context, ch *ipc.Channel, ms *sms.MediaServer, mode int8) (*DiagnosticTrace, func(context.Context) error, error) {
	return s.gb.DiagnosticPlay(ctx, ch, ms, mode)
}
