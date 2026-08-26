package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
)

const mediaStatusHistoryFinished = "121"

// MediaStatusNotify 是 2011 附录 J 定义、后续版本延续的媒体通知。
type MediaStatusNotify struct {
	XMLName    xml.Name
	CmdType    string `xml:"CmdType" json:"cmd_type"`
	SN         int    `xml:"SN" json:"sn"`
	DeviceID   string `xml:"DeviceID" json:"device_id"`
	NotifyType string `xml:"NotifyType" json:"notify_type"`
}

// sipMessageMediaStatus 处理历史媒体文件发送结束通知。
// 未知或已清理会话仍返回 200，保证设备重传能够幂等收敛。
func (g *GB28181API) sipMessageMediaStatus(ctx *sip.Context) {
	var notify MediaStatusNotify
	if err := sip.XMLDecode(ctx.Request.Body(), &notify); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	notify.CmdType = strings.TrimSpace(notify.CmdType)
	notify.DeviceID = strings.TrimSpace(notify.DeviceID)
	notify.NotifyType = strings.TrimSpace(notify.NotifyType)
	if err := g.validateMediaStatusEnvelope(ctx, notify); err != nil {
		ctx.String(400, err.Error())
		return
	}
	callID := callIDFromRequest(ctx.Request)
	if err := g.validateMediaStatusTarget(ctx, notify.DeviceID, callID); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if notify.NotifyType != mediaStatusHistoryFinished {
		ctx.String(200, "OK")
		return
	}

	matched := false
	var ended *Streams
	endedDownload := false
	if callID != "" && g.directDownloads != nil {
		if state, ok := g.directDownloads.State(callID); ok && mediaStatusTargetMatches(notify.DeviceID, state.DeviceID, state.ChannelID) &&
			g.directDownloads.NotifySenderFinishedForDevice(callID, ctx.DeviceID) {
			matched = true
		}
	}
	if !matched && g.streams != nil && callID != "" {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil || stream.DeviceID != ctx.DeviceID || normalizeStoredCallID(stream.CallID) != callID || !strings.HasPrefix(key, "history:") {
				return true
			}
			if !mediaStatusTargetMatches(notify.DeviceID, stream.DeviceID, stream.ChannelID) {
				return true
			}
			if !g.streams.CompareAndDelete(key, stream) {
				return true
			}
			matched = true
			ended = stream
			endedDownload = strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP
			stream.Status = 1
			stream.Stop = true
			stream.EndReason = "media_status"
			return false
		})
	}
	if !matched {
		slog.Debug("MediaStatus session not found", "device_id", ctx.DeviceID, "call_id", callID, "notify_type", notify.NotifyType)
	}
	// 先确认设备并保留已收敛的会话终态，媒体服务器/数据库清理慢时不触发 MediaStatus 重传。
	ctx.String(200, "OK")
	if ended == nil {
		return
	}
	if endedDownload {
		g.finishRTPDownload(ended, rtpDownloadCompleted, "media_status")
	}
	if ended.mediaServer != nil && g.sms != nil {
		_, _ = g.sms.CloseRTPServer(ended.mediaServer, zlm.CloseRTPServerRequest{StreamID: ended.StreamID})
	}
	if g.core.Store() != nil {
		_ = g.core.EditPlaying(context.Background(), ended.DeviceID, ended.ChannelID, false)
	}
}

func (g *GB28181API) validateMediaStatusEnvelope(ctx *sip.Context, notify MediaStatusNotify) error {
	if notify.XMLName.Local != "Notify" || !strings.EqualFold(notify.CmdType, "MediaStatus") || notify.SN <= 0 {
		return fmt.Errorf("invalid MediaStatus envelope")
	}
	if !isGBDeviceIdentifier(notify.DeviceID) || notify.NotifyType == "" {
		return fmt.Errorf("MediaStatus requires DeviceID and NotifyType")
	}
	if ctx == nil || !isGBDeviceIdentifier(strings.TrimSpace(ctx.DeviceID)) {
		return fmt.Errorf("MediaStatus requires authenticated GB28181 device")
	}
	return nil
}

func (g *GB28181API) validateMediaStatusTarget(ctx *sip.Context, targetID, callID string) error {
	if ctx == nil {
		return fmt.Errorf("MediaStatus target mismatch")
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	targetID = strings.TrimSpace(targetID)
	if targetID == deviceID {
		return nil
	}
	if g != nil && g.directDownloads != nil && callID != "" {
		if state, ok := g.directDownloads.State(callID); ok {
			if state.DeviceID == deviceID && mediaStatusTargetMatches(targetID, state.DeviceID, state.ChannelID) {
				return nil
			}
			return fmt.Errorf("MediaStatus target mismatch")
		}
	}
	if g != nil && g.streams != nil && callID != "" {
		matchedCall := false
		matchedTarget := false
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil || !strings.HasPrefix(key, "history:") || normalizeStoredCallID(stream.CallID) != callID {
				return true
			}
			matchedCall = true
			matchedTarget = stream.DeviceID == deviceID && mediaStatusTargetMatches(targetID, stream.DeviceID, stream.ChannelID)
			return !matchedTarget
		})
		if matchedTarget {
			return nil
		}
		if matchedCall {
			return fmt.Errorf("MediaStatus target mismatch")
		}
	}
	if g != nil && g.svr != nil && g.svr.memoryStorer != nil {
		if _, ok := g.svr.memoryStorer.GetChannel(deviceID, targetID); ok {
			return nil
		}
	}
	// 已清理或进程重启后无会话现场时保持通知幂等；此时不会触发任何状态变更。
	return nil
}

func mediaStatusTargetMatches(targetID, deviceID, channelID string) bool {
	targetID = strings.TrimSpace(targetID)
	return targetID == strings.TrimSpace(deviceID) || targetID == strings.TrimSpace(channelID)
}

func normalizeStoredCallID(value string) string {
	value = strings.TrimSpace(value)
	return strings.TrimSpace(strings.TrimPrefix(value, "Call-ID:"))
}
