package gbs

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type pendingInboundDialogCleanup struct {
	dialog *inboundInviteDialog
	mu     sync.Mutex
}

// requestInboundDialogCleanup 在尝试 BYE 前先保存终态所有权。
// 网络或身份处理瞬时失败时，运行态清理器会继续重试；成功后才删除待清理记录。
func (g *GB28181API) requestInboundDialogCleanup(ctx context.Context, dialog *inboundInviteDialog) error {
	if g == nil || dialog == nil {
		return nil
	}
	candidate := &pendingInboundDialogCleanup{dialog: dialog}
	actual, _ := g.pendingInboundDialogCleanups.LoadOrStore(dialog, candidate)
	pending, ok := actual.(*pendingInboundDialogCleanup)
	if !ok || pending == nil {
		g.pendingInboundDialogCleanups.CompareAndDelete(dialog, actual)
		pending = candidate
		g.pendingInboundDialogCleanups.Store(dialog, pending)
	}
	pending.mu.Lock()
	defer pending.mu.Unlock()
	if current, exists := g.pendingInboundDialogCleanups.Load(dialog); !exists || current != pending {
		return nil
	}
	err := g.sendInboundDialogBYEContext(ctx, dialog)
	if err == nil {
		g.pendingInboundDialogCleanups.CompareAndDelete(dialog, pending)
	}
	return err
}

// cleanupPendingInboundDialogCleanups 只扫描已经提交本地终态的入向媒体对话。
func (g *GB28181API) cleanupPendingInboundDialogCleanups() (pending bool) {
	if g == nil {
		return false
	}
	g.pendingInboundDialogCleanups.Range(func(key, value any) bool {
		dialog, _ := key.(*inboundInviteDialog)
		cleanup, ok := value.(*pendingInboundDialogCleanup)
		if !ok || cleanup == nil || dialog == nil || cleanup.dialog != dialog {
			g.pendingInboundDialogCleanups.CompareAndDelete(key, value)
			return true
		}
		cleanupCtx := g.mediaPersistenceContext()
		if err := g.requestInboundDialogCleanup(cleanupCtx, dialog); err != nil {
			slog.WarnContext(cleanupCtx, "retry inbound media dialog BYE failed",
				"call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
		}
		if current, exists := g.pendingInboundDialogCleanups.Load(dialog); exists && current == cleanup {
			pending = true
		}
		return true
	})
	return pending
}

func (g *GB28181API) retryPendingInboundDialogCleanups(ctx context.Context, interval time.Duration) error {
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
		if !g.cleanupPendingInboundDialogCleanups() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
