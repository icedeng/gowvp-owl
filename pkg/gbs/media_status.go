package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const mediaStatusHistoryFinished = "121"

// MediaStatusNotify 是四版本历史回放/下载结束流程使用的媒体通知。
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
	if err := validateMediaStatusStructure(ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
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
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		ctx.String(200, "OK")
		return
	}
	defer unlockCommit()
	if err := g.validateMediaStatusTarget(ctx, notify.DeviceID, callID); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if notify.NotifyType != mediaStatusHistoryFinished {
		_ = ctx.RespondString(200, "OK")
		ctx.Abort()
		return
	}

	matched := false
	directCandidate := false
	cascadeForwarded := false
	var ended *Streams
	endedKey := ""
	endedDownload := false
	if callID != "" && g.directDownloads != nil {
		if state, ok := g.directDownloads.State(callID); ok && state.DeviceID == ctx.DeviceID &&
			mediaStatusTargetMatches(notify.DeviceID, state.DeviceID, state.ChannelID) {
			directCandidate = true
		}
	}
	if !directCandidate && g.streams != nil && callID != "" {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil || stream.DeviceID != ctx.DeviceID || normalizeStoredCallID(stream.CallID) != callID || !strings.HasPrefix(key, "history:") {
				return true
			}
			if !mediaStatusTargetMatches(notify.DeviceID, stream.DeviceID, stream.ChannelID) {
				return true
			}
			matched = true
			ended = stream
			endedKey = key
			endedDownload = strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP
			return false
		})
	}
	if ended != nil {
		forwarded, status, reason := g.forwardCascadeMediaStatus(ended, notify)
		cascadeForwarded = forwarded
		if forwarded && (status < 200 || status >= 300) {
			ctx.String(status, reason)
			return
		}
		if forwarded {
			// 上级已确认媒体结束后立即禁止复用旧源；其后到达的上级 BYE 负责逐级释放对话。
			g.markCascadeSourcesMediaStatusFinished(ended)
		}
	}
	// 上级转发必须在本级响应前完成，才能把 4xx/5xx 反馈给设备；
	// 本地流索引和下载终态则只在 200 OK 实际写出后提交。
	respondErr := ctx.RespondString(200, "OK")
	ctx.Abort()
	if respondErr != nil {
		slog.Error("acknowledge MediaStatus failed", "device_id", ctx.DeviceID, "call_id", callID, "notify_type", notify.NotifyType, "err", respondErr)
		return
	}
	directMatched := false
	if directCandidate && g.directDownloads.NotifySenderFinishedForDevice(callID, ctx.DeviceID) {
		matched = true
		directMatched = true
	}
	if ended != nil {
		current, exists := g.streams.Load(endedKey)
		if endedKey == "" || !exists || current != ended {
			ended = nil
		} else {
			if cascadeForwarded {
				g.deferMediaStreamDialogCleanup(ended)
				if ended.DirectTCP {
					g.notifyCascadeDirectTCPRelaySenderFinished(ended)
				}
			}
			g.markMediaStreamStopped(ended, "media_status", false)
		}
	}
	if ended == nil && !directMatched && callID != "" &&
		g.finishRTPDownloadByCallID(ctx.DeviceID, notify.DeviceID, callID, rtpDownloadCompleted, "media_status") {
		matched = true
	}
	if ended != nil {
		matched = true
	}
	if !matched {
		slog.Debug("MediaStatus session not found", "device_id", ctx.DeviceID, "call_id", callID, "notify_type", notify.NotifyType)
	}
	// SIP 已确认后再执行慢速媒体服务器/数据库清理，避免拖延设备事务。
	if ended == nil {
		return
	}
	// MediaStatus/121 是下载自然完成的更强证据；即使远端 BYE 先到，也允许把 stopped 升级为 completed。
	if endedDownload {
		g.finishRTPDownload(ended, rtpDownloadCompleted, "media_status")
	}
	// 级联已把 MediaStatus 转发给上级时，下级 SIP 对话由随后到达的上级 BYE 收敛；
	// markMediaStreamStopped 已将该步视为由级联持有，本地这里只释放 RTP 接收端口。
	cleanupCtx := g.mediaPersistenceContext()
	if _, err := g.cleanupMediaStreamContext(cleanupCtx, endedKey, ended); err != nil {
		slog.WarnContext(cleanupCtx, "cleanup media after MediaStatus failed", "device_id", ended.DeviceID, "channel_id", ended.ChannelID, "stream_id", ended.StreamID, "err", err)
	}
	if err := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), ended.DeviceID, ended.ChannelID); err != nil {
		slog.Warn("persist MediaStatus channel state", "device_id", ended.DeviceID, "channel_id", ended.ChannelID, "err", err)
	}
}

// forwardCascadeMediaStatus 将下级历史媒体结束通知按各自 SIP 对话转发给上级媒体接收者。
// 同一下级媒体源可被多个上级复用；成功项会记忆，设备重传时只重试尚未成功的会话。
func (g *GB28181API) forwardCascadeMediaStatus(stream *Streams, notify MediaStatusNotify) (bool, int, string) {
	if g == nil || stream == nil || notify.NotifyType != mediaStatusHistoryFinished {
		return false, http.StatusOK, "OK"
	}
	type target struct {
		dialog  *inboundInviteDialog
		session *cascadeMediaSession
	}
	targets := make([]target, 0, 1)
	g.inviteDialogs.Range(func(_, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if !cascadeMediaStatusDialogMatches(dialog, stream) {
			return true
		}
		targets = append(targets, target{dialog: dialog, session: dialog.Cascade})
		return true
	})
	if len(targets) == 0 {
		return false, http.StatusOK, "OK"
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].dialog.CallID < targets[j].dialog.CallID })

	failedStatus, failedReason := http.StatusOK, "OK"
	for _, item := range targets {
		status, reason := g.forwardCascadeMediaStatusDialog(item.dialog, item.session, notify)
		if (status < 200 || status >= 300) && failedStatus == http.StatusOK {
			failedStatus, failedReason = status, reason
		}
	}
	return true, failedStatus, failedReason
}

func cascadeMediaStatusDialogMatches(dialog *inboundInviteDialog, stream *Streams) bool {
	if dialog == nil || dialog.Cascade == nil || stream == nil {
		return false
	}
	sourceRef := dialog.Cascade.sourceSnapshot()
	if sourceRef == nil || sourceRef.stream == nil {
		return false
	}
	dialog.mu.Lock()
	established := dialog.Established && dialog.Request != nil && dialog.Response != nil
	dialog.mu.Unlock()
	if !established || sourceRef.mode != historyModePlayback && sourceRef.mode != historyModeDownload {
		return false
	}
	source := sourceRef.stream
	return source == stream || source.DeviceID == stream.DeviceID && source.ChannelID == stream.ChannelID &&
		normalizeStoredCallID(source.CallID) == normalizeStoredCallID(stream.CallID)
}

func (g *GB28181API) forwardCascadeMediaStatusDialog(dialog *inboundInviteDialog, session *cascadeMediaSession, notify MediaStatusNotify) (int, string) {
	if g == nil || g.svr == nil || dialog == nil || session == nil || session.worker == nil {
		return http.StatusBadGateway, "cascade MediaStatus target is unavailable"
	}
	source := session.sourceSnapshot()
	if source == nil || source.stream == nil {
		return http.StatusBadGateway, "cascade MediaStatus target is unavailable"
	}
	if session.worker.exchange == nil {
		return http.StatusBadGateway, "cascade MediaStatus exchange is unavailable"
	}
	session.mediaStatusMu.Lock()
	defer session.mediaStatusMu.Unlock()
	if session.mediaStatusForwarded {
		return http.StatusOK, "OK"
	}

	dialog.mu.Lock()
	if !dialog.Established || dialog.Request == nil || dialog.Response == nil {
		dialog.mu.Unlock()
		return http.StatusConflict, "cascade media dialog is not established"
	}
	request := dialog.Request
	response := dialog.Response
	dialog.mu.Unlock()

	exposedID := cascadeMediaStatusExposedID(session.worker.platform, request, source.stream.ChannelID)
	if exposedID == "" {
		return http.StatusBadGateway, "cascade MediaStatus channel mapping is unavailable"
	}
	notify.SN = g.nextQuerySN()
	notify.DeviceID = exposedID
	body, err := sip.XMLEncode(notify)
	if err != nil {
		return http.StatusBadGateway, "encode cascade MediaStatus failed"
	}
	dialog.mu.Lock()
	if !dialog.Established || dialog.Request != request || dialog.Response != response {
		dialog.mu.Unlock()
		return http.StatusConflict, "cascade media dialog changed before MediaStatus forwarding"
	}
	baseCSeq := dialog.LocalCSeq
	cseq, cseqErr := nextLocalCSeqLocked(dialog)
	dialog.mu.Unlock()
	if cseqErr != nil {
		return http.StatusConflict, cseqErr.Error()
	}
	forwarded, err := sip.NewRequestFromServerDialogChecked(sip.MethodMessage, request, response, cseq)
	if err != nil {
		return http.StatusBadGateway, "build cascade MediaStatus dialog request failed"
	}
	forwarded.AppendHeader(&sip.ContentTypeXML)
	version := sip.XGBVer(session.worker.protocolVersion())
	forwarded.AppendHeader(&version)
	transport := cascadeTransportForAddr(forwarded.Destination())
	forwarded.AppendHeader(sip.ViaHeader{&sip.ViaHop{
		ProtocolName: "SIP", ProtocolVersion: "2.0",
		Host: session.worker.platform.localHost, Port: sip.NewPort(session.worker.platform.contactPort(transport)),
		Transport: strings.ToUpper(transport), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
	}})
	forwarded.SetBody(body, true)
	identityCtx := session.identityCtx
	if identityCtx == nil {
		identityCtx = context.Background()
	}
	if err := session.worker.platform.monitorUserIdentity.apply(identityCtx, forwarded); err != nil {
		return http.StatusForbidden, "cascade MediaStatus identity forwarding failed"
	}
	if err := identityCtx.Err(); err != nil {
		return http.StatusBadGateway, "cascade MediaStatus upstream is unavailable"
	}
	dialog.mu.Lock()
	if !dialog.Established || dialog.Request != request || dialog.Response != response || dialog.LocalCSeq != baseCSeq {
		dialog.mu.Unlock()
		return http.StatusConflict, "cascade media dialog changed before MediaStatus forwarding"
	}
	dialog.LocalCSeq = cseq
	dialog.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(identityCtx, 10*time.Second)
	defer cancel()
	upstream, err := session.worker.exchangeMessageWithDigestPrepared(waitCtx, forwarded, func(retry *sip.Request) error {
		if err := waitCtx.Err(); err != nil {
			return err
		}
		return reserveCascadeDialogMessageCSeq(dialog, request, response, retry)
	})
	if err != nil || upstream == nil {
		return http.StatusBadGateway, "cascade MediaStatus upstream is unavailable"
	}
	status := upstream.StatusCode()
	reason := strings.TrimSpace(upstream.Reason())
	if reason == "" {
		reason = http.StatusText(status)
	}
	if status >= 200 && status < 300 {
		session.mediaStatusForwarded = true
		dialog.mu.Lock()
		dialog.UpdatedAt = time.Now()
		dialog.mu.Unlock()
	}
	return status, reason
}

func reserveCascadeDialogMessageCSeq(dialog *inboundInviteDialog, dialogRequest *sip.Request, dialogResponse *sip.Response, request *sip.Request) error {
	if dialog == nil || dialogRequest == nil || dialogResponse == nil || request == nil {
		return fmt.Errorf("cascade dialog MESSAGE retry is unavailable")
	}
	cseq, ok := request.CSeq()
	if !ok || cseq == nil || cseq.MethodName != sip.MethodMessage {
		return fmt.Errorf("cascade dialog MESSAGE retry CSeq is invalid")
	}
	dialog.mu.Lock()
	defer dialog.mu.Unlock()
	if !dialog.Established || dialog.Request != dialogRequest || dialog.Response != dialogResponse {
		return fmt.Errorf("cascade dialog changed before MESSAGE retry")
	}
	next, err := nextLocalCSeqLocked(dialog)
	if err != nil {
		return fmt.Errorf("cascade dialog MESSAGE retry: %w", err)
	}
	cseq.SeqNo = next
	dialog.LocalCSeq = next
	return nil
}

func cascadeMediaStatusExposedID(platform cascadePlatform, request *sip.Request, localChannelID string) string {
	if request != nil && request.Recipient() != nil && request.Recipient().User() != nil {
		exposedID := strings.TrimSpace(request.Recipient().User().String())
		if platform.exposedChannelMap[exposedID] == strings.TrimSpace(localChannelID) {
			return exposedID
		}
	}
	return strings.TrimSpace(platform.channelIDMap[strings.TrimSpace(localChannelID)])
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
