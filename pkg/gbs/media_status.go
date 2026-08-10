package gbs

import (
	"context"
	"log/slog"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const mediaStatusHistoryFinished = "121"

// MediaStatusNotify 是 2014 修改补充文件 A.2.5 定义的媒体通知。
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
	if callID != "" && g.directDownloads != nil && g.directDownloads.NotifySenderFinished(callID) {
		matched = true
	}
	if !matched && g.streams != nil && callID != "" {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil || normalizeStoredCallID(stream.CallID) != callID || !strings.HasPrefix(key, "history:") {
				return true
			}
			if !g.streams.CompareAndDelete(key, stream) {
				return true
			}
			matched = true
			stream.Status = 1
			stream.Stop = true
			stream.EndReason = "media_status"
			if g.core.Store() != nil {
				_ = g.core.EditPlaying(context.Background(), stream.DeviceID, stream.ChannelID, false)
			}
			return false
		})
	}
	if !matched {
		slog.Debug("MediaStatus session not found", "device_id", ctx.DeviceID, "call_id", callID, "notify_type", notify.NotifyType)
	}
	ctx.String(200, "OK")
}

func normalizeStoredCallID(value string) string {
	value = strings.TrimSpace(value)
	return strings.TrimSpace(strings.TrimPrefix(value, "Call-ID:"))
}
