package owl

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
)

func (h *Host) DiagnoseChannel(ctx context.Context, pk, cid string) (result core.DiagnosticResult, err error) {
	result = core.DiagnosticResult{CheckedAt: time.Now().UTC(), Checks: []core.DiagnosticCheck{}, Capabilities: []string{}}
	var ch ipc.Channel
	if err = h.db.WithContext(ctx).Where("id = ?", cid).First(&ch).Error; err != nil {
		return result, err
	}
	binding, err := h.repo.Binding(ctx, ch.DID)
	if err != nil {
		return result, err
	}
	if binding.GatewayID == nil || *binding.GatewayID != pk {
		return result, core.Fail(403, "INGRESS_CONFLICT", "指定通道不属于此网关")
	}
	mh, ok := h.media.(*mediaHost)
	if !ok || h.sip == nil {
		return result, fmt.Errorf("media not ready")
	}
	serverID := ch.Config.MediaServerID
	if serverID == "" {
		serverID = sms.DefaultMediaServerID
	}
	ms, err := mh.media.GetMediaServer(ctx, serverID)
	if err != nil {
		return result, err
	}
	start := time.Now()
	var device ipc.Device
	if err = h.db.WithContext(ctx).Where("id = ?", ch.DID).First(&device).Error; err != nil {
		return result, err
	}
	trace, cleanup, playErr := h.sip.DiagnosticPlay(ctx, &ch, ms, device.StreamMode)
	defer func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cleanErr := cleanup(cleanCtx)
		check := core.DiagnosticCheck{Name: "cleanup", Status: "passed", Reason: "OK", Msg: "诊断会话已回收"}
		if cleanErr != nil {
			check.Status = "failed"
			check.Reason = "CLEANUP_PENDING"
			check.Msg = "诊断会话清理待重试，已保留资源记录"
		}
		result.Checks = append(result.Checks, check)
	}()
	checks := map[string]*core.DiagnosticCheck{}
	for _, name := range []string{"sip", "b_first_packet", "wss", "a_zlm", "play_ready"} {
		checks[name] = &core.DiagnosticCheck{Name: name, Status: "skipped", Reason: "NO_OBSERVATION", Msg: "尚未观测到此阶段"}
	}
	if trace.SIPDurationMS != nil {
		c := checks["sip"]
		c.DurationMS = int(*trace.SIPDurationMS)
		c.Status = "passed"
		c.Reason = "OK"
		c.Msg = "设备已接受 SIP 点播"
		if trace.SIPError {
			c.Status = "failed"
			c.Reason = "SIP_FAILED"
			c.Msg = "设备 SIP 点播未成功"
		}
	}
	finish := func() {
		for _, name := range []string{"sip", "b_first_packet", "wss", "a_zlm", "play_ready"} {
			result.Checks = append(result.Checks, *checks[name])
		}
	}
	if playErr != nil {
		finish()
		return result, nil
	}
	pass := func(name, msg string, ms int) {
		c := checks[name]
		c.Status = "passed"
		c.Reason = "OK"
		c.Msg = msg
		c.DurationMS = ms
	}
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		var session store.Session
		e := h.db.WithContext(ctx).Where("gateway_id = ? AND stream_id = ?", pk, trace.StreamID).First(&session).Error
		if e == nil {
			if session.Timings != nil {
				if at := session.Timings.WSSAttachedAt; at != nil {
					pass("wss", "媒体 WSS 已附着", int(at.Sub(start).Milliseconds()))
				}
				if at := session.Timings.AForwardedAt; at != nil {
					pass("a_zlm", "A 已收到有效媒体并转发原 ZLM 节点", int(at.Sub(start).Milliseconds()))
				}
			}
			snapshot := h.manager().Snapshot(pk)
			if slices.Contains(snapshot.Capabilities, "media_inspect_v1") {
				reply, e := h.manager().CallExact(ctx, pk, protocol.ResourceRef{ResourceType: "media", ID: session.RemoteID, Generation: session.Generation, ConnectionEpoch: protocol.Number(session.ConnectionEpoch)}, "media.inspect", protocol.MediaInspect{SessionID: session.RemoteID})
				if e == nil {
					if n := reply.(*protocol.MediaInspection).FirstPacketMS; n != nil {
						pass("b_first_packet", "B 已接收有效媒体首包（B 本地耗时）", int(*n))
					}
				}
			} else {
				checks["b_first_packet"].Reason = "UPGRADE_REQUIRED"
				checks["b_first_packet"].Msg = "此 B 版本不提供首包测量"
			}
		}
		items, mediaErr := mh.media.GetMediaInfoContext(ctx, ms, "rtp", trace.StreamID)
		readyTrack := false
		for _, item := range items {
			for _, track := range item.Tracks {
				readyTrack = readyTrack || track.Ready
			}
		}
		if mediaErr == nil && readyTrack {
			pass("play_ready", "ZLM 已提供可播放媒体流", int(time.Since(start).Milliseconds()))
			result.MediaPathVerified = true
			finish()
			return result, nil
		}
		select {
		case <-ctx.Done():
			finish()
			return result, nil
		case <-deadline.C:
			finish()
			return result, nil
		case <-tick.C:
		}
	}
}
