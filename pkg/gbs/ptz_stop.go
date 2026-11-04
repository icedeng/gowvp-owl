package gbs

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type ptzSession struct {
	sync.Mutex
	stop *ptzScheduledStop
}

type ptzScheduledStop struct {
	cancel context.CancelFunc
}

func (s *ptzSession) cancelPendingStop() {
	if s.stop != nil {
		s.stop.cancel()
		s.stop = nil
	}
}

// 自动停止与新指令共用通道锁，检查代次后直接下发，避免接管新的开放平台租约。
func (g *GB28181API) withPTZSession(ctx context.Context, deviceID, channelID string, fn func(*ptzSession) error) error {
	value, _ := g.ptzSessions.LoadOrStore(deviceID+":"+channelID, &ptzSession{})
	session := value.(*ptzSession)
	session.Lock()
	defer session.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := g.serviceContext().Err(); err != nil {
		return ErrServiceStopped
	}
	return fn(session)
}

// 只对连续方向、变倍、聚焦和光圈动作计时，预置位设置等一次性命令无需停止。
func timedPTZStopAction(action PTZAction) PTZAction {
	switch PTZAction(strings.ToLower(strings.TrimSpace(string(action)))) {
	case PTZActionLeft, PTZActionRight, PTZActionUp, PTZActionDown,
		PTZActionLeftUp, PTZActionLeftDown, PTZActionRightUp, PTZActionRightDown,
		PTZActionZoomIn, PTZActionZoomOut:
		return PTZActionStop
	case PTZActionFocusNear, PTZActionFocusFar, PTZActionFocusAdd, PTZActionFocusSub,
		PTZActionIrisOpen, PTZActionIrisClose, PTZActionIrisAdd, PTZActionIrisSub,
		"focus_plus", "focus_minus", "aperture_add", "aperture_sub":
		return "fi_stop"
	}
	return ""
}

func replacesPTZMotion(action PTZAction) bool {
	switch PTZAction(strings.ToLower(strings.TrimSpace(string(action)))) {
	case PTZActionStop, "ptz_stop", "fi_stop", "cruise_stop", "scan_stop",
		PTZActionPresetCall, "goto_preset", PTZActionCruiseStart, PTZActionScanStart:
		return true
	}
	return false
}

// 级联/附录控制兼容原始十六进制命令；设置预置位等配置不能取消正在执行的安全停止。
func ptzCommandReplacesMotion(command string) bool {
	if len(strings.TrimSpace(command)) != 16 {
		action := PTZAction(command)
		return timedPTZStopAction(action) != "" || replacesPTZMotion(action)
	}
	bytes, err := parsePTZCommand(command)
	if err != nil {
		return false
	}
	return bytes[3] < 0x50 || bytes[3] == 0x82 || bytes[3] == 0x88 || bytes[3] == 0x89
}

func (g *GB28181API) schedulePTZStop(ctx context.Context, session *ptzSession, input PTZInput, action PTZAction) {
	// HTTP 请求返回后仍须停止；保留身份上下文，并由服务生命周期负责取消任务。
	parent, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := &ptzScheduledStop{cancel: cancel}
	session.stop = stop
	started := g.startLifecycleTask(parent, func(taskCtx context.Context) {
		defer cancel()
		timer := time.NewTimer(input.Timeout)
		defer timer.Stop()
		select {
		case <-taskCtx.Done():
			return
		case <-timer.C:
		}
		session.Lock()
		defer session.Unlock()
		if session.stop != stop || taskCtx.Err() != nil {
			return
		}
		session.stop = nil
		stopCtx, stopCancel := context.WithTimeout(taskCtx, 5*time.Second)
		defer stopCancel()
		_, err := g.ptzRaw(stopCtx, &PTZInput{DeviceID: input.DeviceID, ChannelID: input.ChannelID, Action: action})
		if err != nil {
			slog.WarnContext(stopCtx, "GB28181 云台自动停止失败", "device_id", input.DeviceID, "channel_id", input.ChannelID, "err", err)
		}
	})
	if !started {
		session.cancelPendingStop()
	}
}
