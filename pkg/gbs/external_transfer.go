package gbs

import (
	"context"
	"fmt"
	"net"
	"time"
)

type ExternalTransferLease interface {
	AdvertisedEndpoint() (string, int)
	PublicURL() string
	Connect(context.Context, string) (net.Conn, error)
	Commit(context.Context, int64, string) error
	Close(context.Context) error
}
type ExternalTransferSpec struct {
	SubjectType, SubjectID, GatewayID, Direction, WireMode string
	DeviceID, ChannelID, TaskID, Kind, CoverKey, SourceURL string
	MaxBytes                                               int64
}
type ExternalTransferPort interface {
	PrepareTransfer(context.Context, ExternalTransferSpec) (ExternalTransferLease, error)
}

func (g *GB28181API) prepareExternalTransfer(ctx context.Context, in ExternalTransferSpec) (ExternalTransferLease, error) {
	p, ok := g.externalMediaPort().(ExternalTransferPort)
	if !ok {
		return nil, nil
	}
	return p.PrepareTransfer(ctx, in)
}

// FailExternalTransfer 只将尚未提交的原任务收敛为失败；已完成任务及设备最终升级结果保持原状态。
func (s *Server) FailExternalTransfer(ctx context.Context, spec ExternalTransferSpec) error {
	if s == nil || s.gb == nil {
		return fmt.Errorf("国标任务服务未就绪")
	}
	g := s.gb
	switch spec.Kind {
	case "snapshot_upload":
		return g.failSnapshotTransfer(ctx, spec.DeviceID, spec.TaskID)
	case "upgrade_download":
		state, found, err := g.UpgradeStateContext(ctx, spec.DeviceID, spec.TaskID)
		if err != nil || !found || isUpgradeFinal(state.Status) {
			return err
		}
		state.Status = "failed"
		state.Result = "ERROR"
		state.FailedReason = "公网文件传输未提交，已中止且不自动续传"
		state.UpdatedAt = time.Now()
		return g.storeUpgradeStateContext(ctx, state)
	}
	return nil
}

// failSnapshotTransfer 与图片计数/完成通知共用同一状态锁，迟到清理不能覆盖已收齐或最终结果。
func (g *GB28181API) failSnapshotTransfer(ctx context.Context, deviceID, taskID string) error {
	if _, found, err := g.loadSnapshotState(ctx, deviceID, taskID); err != nil || !found {
		return err
	}
	g.snapshotStateMu.Lock()
	defer g.snapshotStateMu.Unlock()
	state, found := g.snapshotStates[snapshotStateKey(deviceID, taskID)]
	if !found || isSnapshotTerminal(state.Status) || state.Status == "cancelled" || state.ExpectedCount > 0 && state.ReceivedCount >= state.ExpectedCount {
		return nil
	}
	state.Status = "failed"
	if state.ReceivedCount > 0 {
		state.Status = "partial_failed"
	}
	state.UpdatedAt = time.Now()
	if err := g.persistSnapshotState(ctx, state); err != nil {
		return err
	}
	g.storeSnapshotStateMemoryLocked(state)
	return nil
}
