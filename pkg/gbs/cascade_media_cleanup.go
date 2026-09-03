package gbs

import (
	"context"
	"time"
)

// cleanupStoppedCascadeMediaSessions 重试已经进入终态、但 RTP 发送端尚未关闭的级联媒体会话。
func (g *GB28181API) cleanupStoppedCascadeMediaSessions() (pending bool) {
	if g == nil {
		return false
	}
	g.pendingCascadeMediaCleanups.Range(func(key, value any) bool {
		session, _ := key.(*cascadeMediaSession)
		current, ok := value.(*cascadeMediaSession)
		if !ok || session == nil || current != session {
			g.pendingCascadeMediaCleanups.CompareAndDelete(key, value)
			return true
		}
		g.stopCascadeMediaSession(session, false, false)
		if actual, exists := g.pendingCascadeMediaCleanups.Load(session); exists && actual == session {
			pending = true
		}
		return true
	})
	return pending
}

func (g *GB28181API) retryStoppedCascadeMediaSessions(ctx context.Context, interval time.Duration) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = voiceShutdownRetryInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if !g.cleanupStoppedCascadeMediaSessions() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
