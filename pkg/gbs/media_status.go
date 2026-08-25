package gbs

import (
	"context"
	"log/slog"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
)

const mediaStatusHistoryFinished = "121"

// MediaStatusNotify 是 2011 附录 J 定义、后续版本延续的媒体通知。
type MediaStatusNotify struct {
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
	if strings.TrimSpace(notify.NotifyType) != mediaStatusHistoryFinished {
		ctx.String(200, "OK")
		return
	}

	callID := callIDFromRequest(ctx.Request)
	matched := false
	var ended *Streams
	endedDownload := false
	if callID != "" && g.directDownloads != nil && g.directDownloads.NotifySenderFinishedForDevice(callID, ctx.DeviceID) {
		matched = true
	}
	if !matched && g.streams != nil && callID != "" {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil || stream.DeviceID != ctx.DeviceID || normalizeStoredCallID(stream.CallID) != callID || !strings.HasPrefix(key, "history:") {
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

func normalizeStoredCallID(value string) string {
	value = strings.TrimSpace(value)
	return strings.TrimSpace(strings.TrimPrefix(value, "Call-ID:"))
}
