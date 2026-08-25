package gbs

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	sdp "github.com/panjjo/gosdp"
)

type inboundInviteDialog struct {
	CallID       string
	DeviceID     string
	RemoteTag    string
	InitialToTag string
	LocalTag     string
	TagsBound    bool
	Established  bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LocalCSeq    uint32
	Request      *sip.Request
	Response     *sip.Response
	Broadcast    *broadcastSession
	Cascade      *cascadeMediaSession
	InviteTx     *sip.Transaction
	Cancelled    bool
	mu           sync.Mutex
}

const pendingInviteDialogTTL = 10 * time.Minute

// sipInviteGeneric 处理入向 INVITE。
// 2014+ 广播由接收者主动 INVITE 语音源；已注册上级的媒体请求进入级联 B2BUA。
// 其他未识别的入向媒体会话明确拒绝，避免回显 offer 使对端误判媒体已经建立。
func (g *GB28181API) sipInviteGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		ctx.String(400, "missing call-id")
		return
	}
	if g.svr != nil && g.svr.cascade != nil {
		if worker, ok := g.svr.cascade.matchRegistered(ctx.DeviceID, ctx.Source, ctx.Request.GetConnection()); ok {
			g.sipInviteCascade(ctx, callID, worker)
			return
		}
	}

	if session := g.findBroadcastSessionForInvite(ctx.DeviceID, ctx.GetHeader("Subject")); session != nil {
		g.sipInviteBroadcast(ctx, callID, session)
		return
	}

	ctx.String(501, "unrecognized inbound media session")
}

// sipCancelGeneric 取消尚未完成的入向级联 INVITE。
func (g *GB28181API) sipCancelGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		ctx.String(400, "missing call-id")
		return
	}
	value, ok := g.inviteDialogs.Load(callID)
	if !ok {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	dialog, _ := value.(*inboundInviteDialog)
	if dialog == nil || dialog.Cascade == nil && dialog.Broadcast == nil {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	if (dialog.Cascade != nil && !g.authorizeCascadeDialogRequest(dialog, ctx)) ||
		(dialog.Broadcast != nil && !g.authorizeBroadcastDialogRequest(dialog, ctx)) {
		ctx.String(http.StatusForbidden, "cascade dialog source mismatch")
		return
	}
	if !inboundDialogTagsMatch(dialog, ctx.Request, false) {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	dialog.mu.Lock()
	if dialog.Response != nil {
		dialog.mu.Unlock()
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	dialog.Cancelled = true
	inviteTx := dialog.InviteTx
	dialog.mu.Unlock()
	ctx.String(200, "OK")
	if inviteTx != nil && dialog.Request != nil {
		_ = inviteTx.Respond(sip.NewResponseFromRequest("", dialog.Request, 487, "Request Terminated", nil))
	}
	g.inviteDialogs.CompareAndDelete(callID, dialog)
	if dialog.Cascade != nil {
		g.stopCascadeMediaSession(dialog.Cascade, false, false)
	}
	if dialog.Broadcast != nil {
		dialog.Broadcast.complete(fmt.Errorf("Broadcast INVITE cancelled"))
		_ = g.stopBroadcastSession(dialog.Broadcast, false)
	}
}

type broadcastSDPOffer struct {
	RemoteIP net.IP
	Port     int
	Payload  int
	Mapping  string
	RTPType  int
}

func parseBroadcastSDPOffer(body []byte, version GBProtocolVersion) (*broadcastSDPOffer, error) {
	message, err := sdp.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("decode Broadcast SDP: %w", err)
	}
	for i := range message.Medias {
		media := &message.Medias[i]
		if !strings.EqualFold(media.Description.Type, "audio") {
			continue
		}
		if !strings.EqualFold(media.Description.Protocol, "RTP/AVP") {
			return nil, fmt.Errorf("Broadcast requires RTP/AVP audio")
		}
		if media.Description.Port <= 0 || media.Description.Port > 65535 {
			return nil, fmt.Errorf("invalid Broadcast RTP port")
		}
		if media.Flag("sendonly") || media.Flag("inactive") {
			return nil, fmt.Errorf("Broadcast receiver SDP must accept media")
		}
		remoteIP := media.Connection.IP
		if remoteIP == nil {
			remoteIP = message.Connection.IP
		}
		if remoteIP == nil || remoteIP.IsUnspecified() || remoteIP.IsMulticast() {
			return nil, fmt.Errorf("invalid Broadcast RTP address")
		}
		payload, mapping, rtpType, err := parseBroadcastPayload(media, version)
		if err != nil {
			return nil, err
		}
		return &broadcastSDPOffer{RemoteIP: remoteIP, Port: media.Description.Port, Payload: payload, Mapping: mapping, RTPType: rtpType}, nil
	}
	return nil, fmt.Errorf("Broadcast INVITE does not contain audio media")
}

func (g *GB28181API) sipInviteBroadcast(ctx *sip.Context, callID string, session *broadcastSession) {
	if recipient := ctx.Request.Recipient(); recipient != nil {
		if user := recipient.User(); user != nil && strings.TrimSpace(user.String()) != "" && strings.TrimSpace(user.String()) != session.SourceID {
			ctx.String(404, "Broadcast source_id mismatch")
			session.complete(fmt.Errorf("Broadcast INVITE source_id mismatch"))
			return
		}
	}
	if existing, ok := g.inviteDialogs.Load(callID); ok {
		if dialog, ok := existing.(*inboundInviteDialog); ok && dialog != nil && dialog.Broadcast == session {
			if !inboundDialogTagsMatch(dialog, ctx.Request, false) {
				ctx.String(491, "Call-ID already in use")
				return
			}
			dialog.mu.Lock()
			resp := dialog.Response
			dialog.UpdatedAt = time.Now()
			dialog.mu.Unlock()
			if resp != nil {
				_ = ctx.Tx.Respond(resp)
			} else {
				ctx.String(100, "Trying")
			}
			return
		}
		ctx.String(491, "Call-ID already in use")
		return
	}

	offer, err := parseBroadcastSDPOffer(ctx.Request.Body(), session.Version)
	if err != nil {
		ctx.String(488, err.Error())
		session.complete(err)
		return
	}
	session.mu.Lock()
	if session.stopped {
		session.mu.Unlock()
		ctx.String(487, "Broadcast session terminated")
		return
	}
	if session.inviteBusy || session.rtpStarted {
		session.mu.Unlock()
		ctx.String(491, "Broadcast INVITE already in progress")
		return
	}
	session.inviteBusy = true
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.inviteBusy = false
		session.mu.Unlock()
	}()
	dialog := &inboundInviteDialog{
		CallID: callID, DeviceID: strings.TrimSpace(ctx.DeviceID), RemoteTag: sipRequestFromTag(ctx.Request), InitialToTag: sipRequestToTag(ctx.Request), TagsBound: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		LocalCSeq: 1, Request: ctx.Request, Broadcast: session, InviteTx: ctx.Tx,
	}
	if _, loaded := g.inviteDialogs.LoadOrStore(callID, dialog); loaded {
		ctx.String(491, "Call-ID already in use")
		return
	}
	fail := func(status int, cause error) {
		dialog.mu.Lock()
		cancelled := dialog.Cancelled
		dialog.mu.Unlock()
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		if !cancelled {
			ctx.String(status, cause.Error())
		}
		session.complete(cause)
	}
	ssrc, err := g.getSSRC(0)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	started, err := g.sms.StartSendRTP(session.SMS, zlm.StartSendRTPRequest{
		Vhost:     session.SourceVHost,
		App:       session.SourceApp,
		Stream:    session.SourceStream,
		SSRC:      ssrc,
		DstURL:    offer.RemoteIP.String(),
		DstPort:   offer.Port,
		IsUDP:     true,
		Type:      offer.RTPType,
		PT:        offer.Payload,
		OnlyAudio: true,
	})
	if err != nil {
		fail(500, fmt.Errorf("start Broadcast RTP: %w", err))
		return
	}
	if started == nil || started.LocalPort <= 0 || started.LocalPort > 65535 {
		err = fmt.Errorf("media server returned invalid Broadcast RTP port")
		_, _ = g.sms.StopSendRTP(session.SMS, zlm.StopSendRTPRequest{Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc})
		fail(500, err)
		return
	}

	answer, err := buildBroadcastSDPAnswer(session, started.LocalPort, offer.Payload, offer.Mapping, ssrc)
	if err != nil {
		_, _ = g.sms.StopSendRTP(session.SMS, zlm.StopSendRTPRequest{Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc})
		fail(500, err)
		return
	}
	resp := sip.NewResponseFromRequest("", ctx.Request, 200, "OK", answer)
	resp.AppendHeader(&sip.ContentTypeSDP)
	if g.svr != nil {
		resp.AppendHeader(&sip.ContactHeader{
			DisplayName: g.svr.fromAddress.DisplayName,
			Address:     g.svr.fromAddress.URI.Clone(),
			Params:      g.svr.fromAddress.Params.Clone(),
		})
	}
	session.mu.Lock()
	if session.stopped {
		session.mu.Unlock()
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		_, _ = g.sms.StopSendRTP(session.SMS, zlm.StopSendRTPRequest{Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc})
		dialog.mu.Lock()
		cancelled := dialog.Cancelled
		dialog.mu.Unlock()
		if !cancelled {
			ctx.String(487, "Broadcast session terminated")
		}
		return
	}
	dialog.mu.Lock()
	if dialog.Cancelled {
		dialog.mu.Unlock()
		session.mu.Unlock()
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		_, _ = g.sms.StopSendRTP(session.SMS, zlm.StopSendRTPRequest{Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc})
		return
	}
	dialog.Response = resp
	dialog.LocalTag = sipResponseToTag(resp)
	dialog.UpdatedAt = time.Now()
	dialog.mu.Unlock()
	session.SSRC = ssrc
	session.Dialog = dialog
	session.rtpStarted = true
	session.Stream.CallID = callID
	session.Stream.ssrc = ssrc
	session.Stream.Status = 0
	session.mu.Unlock()
	if err := ctx.Tx.Respond(resp); err != nil {
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		_ = g.stopBroadcastSession(session, false)
		session.complete(fmt.Errorf("respond Broadcast INVITE: %w", err))
		return
	}
	session.complete(nil)
}

func buildBroadcastSDPAnswer(session *broadcastSession, port, payload int, mapping, ssrc string) ([]byte, error) {
	ip4str, err := GetIP(session.SMS.GetSDPIP())
	if err != nil {
		return nil, err
	}
	audio := sdp.Media{Description: sdp.MediaDescription{
		Type: "audio", Port: port, Formats: []string{strconv.Itoa(payload)}, Protocol: "RTP/AVP",
	}}
	audio.AddAttribute("sendonly")
	audio.AddAttribute("rtpmap", strconv.Itoa(payload), mapping)
	message := &sdp.Message{
		Origin: sdp.Origin{Username: session.SourceID, NetworkType: "IN", AddressType: "IP4", Address: ip4str},
		Name:   historyModePlay,
		Connection: sdp.ConnectionData{
			NetworkType: "IN", AddressType: "IP4", IP: net.ParseIP(ip4str),
		},
		Timing: []sdp.Timing{{}}, Medias: []sdp.Media{audio}, SSRC: ssrc,
	}
	body := message.Append(nil).AppendTo(nil)
	return append(body, "f=v/////a/1/8/1\r\n"...), nil
}

// sipByeGeneric 处理入向 BYE。
// 若会话不存在或未建立，返回 481（Call/Transaction Does Not Exist）。
func (g *GB28181API) sipByeGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		ctx.String(400, "missing call-id")
		return
	}
	v, ok := g.inviteDialogs.Load(callID)
	if !ok {
		if g.handleCascadeVoiceBYE(ctx, callID) {
			return
		}
		if g.handleOutboundBYE(ctx, callID) {
			return
		}
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	d, _ := v.(*inboundInviteDialog)
	if d == nil {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	if d.Cascade != nil && !g.authorizeCascadeDialogRequest(d, ctx) {
		ctx.String(http.StatusForbidden, "cascade dialog source mismatch")
		return
	}
	if d.Broadcast != nil && !g.authorizeBroadcastDialogRequest(d, ctx) {
		ctx.String(http.StatusForbidden, "broadcast dialog source mismatch")
		return
	}
	if !inboundDialogTagsMatch(d, ctx.Request, true) {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	d.mu.Lock()
	established := d.Established
	d.mu.Unlock()
	if !established {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	ctx.String(200, "OK")
	g.inviteDialogs.Delete(callID)
	if d.Broadcast != nil {
		_ = g.stopBroadcastSession(d.Broadcast, false)
	}
	if d.Cascade != nil {
		g.stopCascadeMediaSession(d.Cascade, false, false)
	}
}

func (g *GB28181API) handleOutboundBYE(ctx *sip.Context, callID string) bool {
	if g.streams == nil {
		return false
	}
	matched := false
	var endedStream *Streams
	endedDownload := false
	g.streams.Range(func(key string, stream *Streams) bool {
		if stream == nil || stream.DeviceID != ctx.DeviceID || normalizeStoredCallID(stream.CallID) != callID || !outboundDialogTagsMatch(stream.Resp, ctx.Request) {
			return true
		}
		if !g.streams.CompareAndDelete(key, stream) {
			return true
		}
		matched = true
		endedStream = stream
		stream.Stop = true
		stream.Status = 1
		stream.EndReason = "remote_bye"
		endedDownload = strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP
		return false
	})
	if matched {
		// 会话已从运行态移除后先确认 BYE，媒体服务器和数据库清理慢时不触发重复 BYE。
		ctx.String(200, "OK")
		if endedDownload {
			g.finishRTPDownload(endedStream, rtpDownloadStopped, "remote_bye")
		}
		if value, ok := g.talkSessions.Load(endedStream.StreamID); ok {
			if session, ok := value.(*talkSession); ok {
				_ = g.stopTalkSession(session, fmt.Errorf("Talk ended by remote BYE"))
			}
		} else if endedStream.mediaServer != nil && g.sms != nil {
			_, _ = g.sms.CloseRTPServer(endedStream.mediaServer, zlm.CloseRTPServerRequest{StreamID: endedStream.StreamID})
		}
		if g.core.Store() != nil {
			_ = g.core.EditPlaying(context.Background(), endedStream.DeviceID, endedStream.ChannelID, false)
		}
		g.terminateCascadeSessionsForStream(endedStream)
	}
	return matched
}

// sipAckGeneric 处理入向 ACK，标记会话为已建立。
func (g *GB28181API) sipAckGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		return
	}
	v, ok := g.inviteDialogs.Load(callID)
	if !ok {
		return
	}
	d, _ := v.(*inboundInviteDialog)
	if d == nil {
		return
	}
	if d.Cascade != nil && !g.authorizeCascadeDialogRequest(d, ctx) {
		return
	}
	if d.Broadcast != nil && !g.authorizeBroadcastDialogRequest(d, ctx) {
		return
	}
	if !inboundDialogTagsMatch(d, ctx.Request, true) {
		return
	}
	d.mu.Lock()
	d.Established = true
	d.UpdatedAt = time.Now()
	d.mu.Unlock()
}

type cascadeMANSRTSPRequest struct {
	method  string
	version string
	cseq    uint32
	headers []string
}

// sipInfoGeneric 将上级平台对级联历史会话的 MANSRTSP 控制转发给实际设备。
func (g *GB28181API) sipInfoGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		ctx.String(http.StatusBadRequest, "missing call-id")
		return
	}
	value, ok := g.inviteDialogs.Load(callID)
	if !ok {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	dialog, _ := value.(*inboundInviteDialog)
	if dialog == nil || dialog.Cascade == nil || !g.authorizeCascadeDialogRequest(dialog, ctx) {
		ctx.String(http.StatusForbidden, "cascade dialog source mismatch")
		return
	}
	if !inboundDialogTagsMatch(dialog, ctx.Request, true) {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	dialog.mu.Lock()
	established := dialog.Established
	dialog.UpdatedAt = time.Now()
	source := dialog.Cascade.source
	dialog.mu.Unlock()
	if !established || source == nil || source.stream == nil || source.channel == nil || source.mode == historyModePlay {
		ctx.String(481, "history dialog is not established")
		return
	}
	contentType := strings.ToLower(strings.TrimSpace(ctx.GetHeader("Content-Type")))
	if contentType != "application/mansrtsp" && !strings.HasPrefix(contentType, "application/mansrtsp;") {
		ctx.String(http.StatusUnsupportedMediaType, "Content-Type must be Application/MANSRTSP")
		return
	}
	command, err := parseCascadeMANSRTSP(ctx.Request.Body())
	if err != nil {
		ctx.String(http.StatusBadRequest, err.Error())
		return
	}

	source.controlMu.Lock()
	downstreamCSeq := source.stream.nextCSeq()
	downstreamVersion := GBVersion10
	if g.svr != nil && g.svr.memoryStorer != nil {
		downstreamVersion = g.getDeviceGBProtocolVersion(source.channel.DeviceID)
	}
	downstreamBody := command.body(downstreamCSeq, historyControlProtocolVersion(downstreamVersion))
	controlHistory := g.ControlHistory
	if g.cascadeControlHistory != nil {
		controlHistory = g.cascadeControlHistory
	}
	err = controlHistory(context.Background(), &ControlHistoryInput{
		Channel: source.channel, Mode: source.mode, Cmd: string(downstreamBody), sessionKey: source.key,
	})
	source.controlMu.Unlock()
	if err != nil {
		ctx.String(http.StatusBadGateway, err.Error())
		return
	}

	responseBody := []byte(fmt.Sprintf("%s 200 OK\r\nCSeq: %d\r\n\r\n", command.version, command.cseq))
	response := sip.NewResponseFromRequest("", ctx.Request, http.StatusOK, "OK", responseBody)
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	_ = ctx.Tx.Respond(response)
}

func (g *GB28181API) authorizeCascadeDialogRequest(dialog *inboundInviteDialog, ctx *sip.Context) bool {
	if g == nil || dialog == nil || dialog.Cascade == nil || dialog.Cascade.worker == nil || ctx == nil {
		return false
	}
	return g.authorizeCascadeWorker(dialog.Cascade.worker, ctx)
}

func (g *GB28181API) authorizeBroadcastDialogRequest(dialog *inboundInviteDialog, ctx *sip.Context) bool {
	if dialog == nil || dialog.Broadcast == nil || dialog.Request == nil || ctx == nil {
		return false
	}
	if strings.TrimSpace(ctx.DeviceID) != strings.TrimSpace(dialog.DeviceID) {
		return false
	}
	originalSource := dialog.Request.Source()
	currentSource := ctx.Source
	if currentSource == nil && ctx.Request != nil {
		currentSource = ctx.Request.Source()
	}
	return originalSource == nil || addressIP(originalSource).Equal(addressIP(currentSource))
}

func inboundDialogTagsMatch(dialog *inboundInviteDialog, request *sip.Request, established bool) bool {
	if dialog == nil || request == nil {
		return false
	}
	dialog.mu.Lock()
	remoteTag := dialog.RemoteTag
	initialToTag := dialog.InitialToTag
	localTag := dialog.LocalTag
	tagsBound := dialog.TagsBound
	dialogRequest := dialog.Request
	dialogResponse := dialog.Response
	if !tagsBound && remoteTag == "" {
		remoteTag = sipRequestFromTag(dialogRequest)
	}
	if !tagsBound && initialToTag == "" {
		initialToTag = sipRequestToTag(dialogRequest)
	}
	if !tagsBound && localTag == "" {
		localTag = sipResponseToTag(dialogResponse)
	}
	dialog.mu.Unlock()
	if !tagsBound && remoteTag == "" && initialToTag == "" && localTag == "" && dialogRequest == nil && dialogResponse == nil {
		// 兼容内部旧调用构造的无 SIP 报文会话；协议入口创建的会话始终保存标签。
		return true
	}
	expectedToTag := initialToTag
	if established {
		expectedToTag = localTag
	}
	return sipRequestFromTag(request) == remoteTag && sipRequestToTag(request) == expectedToTag
}

func sipRequestFromTag(request *sip.Request) string {
	if request == nil {
		return ""
	}
	from, ok := request.From()
	if !ok || from == nil {
		return ""
	}
	return sipParamsTag(from.Params)
}

func sipRequestToTag(request *sip.Request) string {
	if request == nil {
		return ""
	}
	to, ok := request.To()
	if !ok || to == nil {
		return ""
	}
	return sipParamsTag(to.Params)
}

func sipResponseToTag(response *sip.Response) string {
	if response == nil {
		return ""
	}
	to, ok := response.To()
	if !ok || to == nil {
		return ""
	}
	return sipParamsTag(to.Params)
}

func sipResponseFromTag(response *sip.Response) string {
	if response == nil {
		return ""
	}
	from, ok := response.From()
	if !ok || from == nil {
		return ""
	}
	return sipParamsTag(from.Params)
}

func outboundDialogTagsMatch(response *sip.Response, request *sip.Request) bool {
	if request == nil {
		return false
	}
	if response == nil {
		// 兼容内部旧调用构造的无响应流；协议入口建立的会话始终保存 2xx 响应。
		return true
	}
	return sipRequestFromTag(request) == sipResponseToTag(response) &&
		sipRequestToTag(request) == sipResponseFromTag(response)
}

func sipParamsTag(params sip.Params) string {
	if params == nil {
		return ""
	}
	tag, ok := params.Get("tag")
	if !ok || tag == nil {
		return ""
	}
	return strings.TrimSpace(tag.String())
}

func (g *GB28181API) authorizeCascadeWorker(worker *cascadeWorker, ctx *sip.Context) bool {
	if g == nil || worker == nil || ctx == nil {
		return false
	}
	if g.svr != nil && g.svr.cascade != nil {
		matched, ok := g.svr.cascade.matchRegistered(ctx.DeviceID, ctx.Source, ctx.Request.GetConnection())
		return ok && matched == worker
	}
	if strings.TrimSpace(ctx.DeviceID) != worker.platform.serverID {
		return false
	}
	state := worker.snapshot()
	return state.Registered && worker.remoteAddressMatches(ctx.Source)
}

func parseCascadeMANSRTSP(body []byte) (*cascadeMANSRTSPRequest, error) {
	if len(body) == 0 || len(body) > 4096 {
		return nil, fmt.Errorf("invalid MANSRTSP body length")
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("incomplete MANSRTSP request")
	}
	start := strings.Fields(strings.TrimSpace(lines[0]))
	if len(start) != 2 {
		return nil, fmt.Errorf("invalid MANSRTSP request line")
	}
	method := strings.ToUpper(start[0])
	if method != "PLAY" && method != "PAUSE" && method != "TEARDOWN" {
		return nil, fmt.Errorf("unsupported MANSRTSP method: %s", start[0])
	}
	version := strings.ToUpper(start[1])
	if version != "MANSRTSP/1.0" && version != "RTSP/1.0" {
		return nil, fmt.Errorf("unsupported MANSRTSP version: %s", start[1])
	}

	request := &cascadeMANSRTSPRequest{method: method, version: version}
	seen := make(map[string]struct{}, len(lines)-1)
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("invalid MANSRTSP header: %s", line)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate MANSRTSP header: %s", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "cseq":
			parsed, err := strconv.ParseUint(value, 10, 32)
			if err != nil || parsed == 0 {
				return nil, fmt.Errorf("invalid MANSRTSP CSeq: %s", value)
			}
			request.cseq = uint32(parsed)
		case "scale":
			if method != "PLAY" {
				return nil, fmt.Errorf("Scale is only valid for PLAY")
			}
			scale, err := strconv.ParseFloat(value, 64)
			if err != nil || scale == 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
				return nil, fmt.Errorf("invalid MANSRTSP Scale: %s", value)
			}
			request.headers = append(request.headers, "Scale: "+value)
		case "range":
			lower := strings.ToLower(value)
			if method != "PLAY" || !(strings.HasPrefix(lower, "npt=") || strings.HasPrefix(lower, "smpte=") || strings.HasPrefix(lower, "clock=")) {
				return nil, fmt.Errorf("invalid MANSRTSP Range: %s", value)
			}
			request.headers = append(request.headers, "Range: "+value)
		case "pausetime":
			if method != "PAUSE" || !strings.EqualFold(value, "now") {
				return nil, fmt.Errorf("invalid MANSRTSP PauseTime: %s", value)
			}
			request.headers = append(request.headers, "PauseTime: now")
		default:
			return nil, fmt.Errorf("unsupported MANSRTSP header: %s", name)
		}
	}
	if request.cseq == 0 {
		return nil, fmt.Errorf("MANSRTSP CSeq is required")
	}
	return request, nil
}

func (r *cascadeMANSRTSPRequest) body(cseq uint32, version string) []byte {
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s %s\r\nCSeq: %d\r\n", r.method, version, cseq)
	for _, header := range r.headers {
		builder.WriteString(header)
		builder.WriteString("\r\n")
	}
	builder.WriteString("\r\n")
	return []byte(builder.String())
}

func (g *GB28181API) sendInboundDialogBYE(dialog *inboundInviteDialog) error {
	if dialog == nil || g.svr == nil {
		return nil
	}
	dialog.mu.Lock()
	if !dialog.Established || dialog.Request == nil || dialog.Response == nil {
		dialog.mu.Unlock()
		return nil
	}
	request := dialog.Request
	response := dialog.Response
	dialog.LocalCSeq++
	cseq := dialog.LocalCSeq
	dialog.mu.Unlock()

	remote, ok := request.From()
	if !ok || remote == nil || remote.Address == nil {
		return fmt.Errorf("inbound dialog missing remote From")
	}
	local, ok := response.To()
	if !ok || local == nil || local.Address == nil {
		return fmt.Errorf("inbound dialog missing local To")
	}
	recipient := remote.Address.Clone()
	if contact, ok := request.Contact(); ok && contact != nil && contact.Address != nil {
		recipient = contact.Address.Clone()
	}
	callID, ok := request.CallID()
	if !ok || callID == nil {
		return fmt.Errorf("inbound dialog missing Call-ID")
	}
	fromParams := local.Params
	if fromParams == nil {
		fromParams = sip.NewParams()
	}
	toParams := remote.Params
	if toParams == nil {
		toParams = sip.NewParams()
	}
	contact := &g.svr.fromAddress
	version := ""
	if dialog.Cascade != nil && dialog.Cascade.worker != nil {
		if cascadeContact := dialog.Cascade.worker.contactAddress(); cascadeContact != nil {
			contact = cascadeContact
		}
		version = string(dialog.Cascade.worker.protocolVersion())
	} else if dialog.Broadcast != nil {
		version = string(dialog.Broadcast.Version)
	}
	hb := sip.NewHeaderBuilder().
		SetFrom(&sip.Address{DisplayName: local.DisplayName, URI: local.Address, Params: fromParams}).
		SetToWithParam(&sip.Address{DisplayName: remote.DisplayName, URI: remote.Address, Params: toParams}).
		SetContact(contact).
		SetMethod(sip.MethodBYE).
		SetSeqNo(uint(cseq)).
		SetCallID(callID).
		SetXGBVerValue(version).
		AddVia(&sip.ViaHop{Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})})
	bye := sip.NewRequest("", sip.MethodBYE, recipient, sip.DefaultSipVersion, hb.Build(), nil)
	bye.SetConnection(request.GetConnection())
	bye.SetSource(request.Destination())
	bye.SetDestination(request.Source())
	_, err := g.svr.Request(bye)
	return err
}

func callIDFromRequest(req *sip.Request) string {
	if req == nil {
		return ""
	}
	callID, ok := req.CallID()
	if !ok || callID == nil {
		return ""
	}
	return normalizeCallID(callID)
}

func normalizeCallID(callID *sip.CallID) string {
	if callID == nil {
		return ""
	}
	value := strings.TrimSpace(callID.String())
	return strings.TrimSpace(strings.TrimPrefix(value, "Call-ID:"))
}

// startInviteDialogCleaner 定时回收长期未更新会话，避免异常场景导致内存堆积。
func (g *GB28181API) startInviteDialogCleaner() {
	ticker := time.NewTicker(120 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-g.lifecycleDone:
			return
		case <-ticker.C:
		}
		g.cleanupInviteDialogs(time.Now())
	}
}

func (g *GB28181API) cleanupInviteDialogs(now time.Time) {
	if g == nil {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	expireBefore := now.Add(-pendingInviteDialogTTL)
	g.inviteDialogs.Range(func(key, value any) bool {
		d, ok := value.(*inboundInviteDialog)
		if !ok || d == nil {
			g.inviteDialogs.Delete(key)
			return true
		}
		d.mu.Lock()
		expired := !d.Established && d.UpdatedAt.Before(expireBefore)
		d.mu.Unlock()
		if expired {
			if d.Cascade != nil {
				g.stopCascadeMediaSession(d.Cascade, false, false)
			}
			g.inviteDialogs.CompareAndDelete(key, d)
			if d.Broadcast != nil {
				_ = g.stopBroadcastSession(d.Broadcast, false)
			}
		}
		return true
	})
}

func (g *GB28181API) close() {
	if g == nil {
		return
	}
	g.beginClose()
	g.lifecycleWG.Wait()
	g.requestWG.Wait()
	g.closeOnce.Do(func() {
		g.catalogResponses.Close()
		g.recordResponses.Close()
		g.pendingDeviceControl.Clear()
		g.pendingDeviceQuery.Clear()
		g.pendingDeviceConfig.Clear()
		g.pendingBroadcast.Clear()
		g.recordResponseAliases.Clear()
		g.eventSubscribers.Clear()
		g.outgoingSubscriptions.Clear()
		g.closeCascadeDownstreamSubscriptions()
		g.closeCascadeMediaSessions()
		g.closeCascadeVoiceSessions()
		g.closeVoiceSessions()
		if g.directDownloads != nil {
			g.directDownloads.Shutdown()
		}
		g.closeRemainingMediaSessions()
	})
}

func (g *GB28181API) beginClose() {
	if g == nil {
		return
	}
	g.closeBeginOnce.Do(func() {
		g.lifecycleMu.Lock()
		g.lifecycleClosed = true
		if g.lifecycleDone != nil {
			close(g.lifecycleDone)
		}
		if g.lifecycleCancel != nil {
			g.lifecycleCancel()
		}
		g.lifecycleMu.Unlock()
	})
}

func (g *GB28181API) closeVoiceSessions() {
	if g == nil {
		return
	}
	g.talkSessions.Range(func(key, value any) bool {
		session, ok := value.(*talkSession)
		if !ok || session == nil {
			g.talkSessions.Delete(key)
			return true
		}
		_ = g.stopTalkSession(session, fmt.Errorf("GB28181 service stopped"))
		if g.streams != nil && session.Stream != nil {
			g.streams.CompareAndDelete(voiceKey(voiceModeTalk, session.DeviceID, session.ChannelID), session.Stream)
		}
		return true
	})
	g.broadcastSessions.Range(func(key, value any) bool {
		session, ok := value.(*broadcastSession)
		if !ok || session == nil {
			g.broadcastSessions.Delete(key)
			return true
		}
		_ = g.stopBroadcastSession(session, true)
		session.mu.Lock()
		dialog := session.Dialog
		session.mu.Unlock()
		if dialog != nil {
			g.inviteDialogs.CompareAndDelete(dialog.CallID, dialog)
		}
		return true
	})
}

func (g *GB28181API) closeRemainingMediaSessions() {
	if g == nil {
		return
	}
	if g.streams != nil {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil {
				g.streams.Delete(key)
				return true
			}
			if !g.streams.CompareAndDelete(key, stream) {
				return true
			}
			if stream.DirectTCP && g.directDownloads != nil {
				g.directDownloads.Cancel(stream.DirectSessionID)
			}
			if strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP {
				g.finishRTPDownload(stream, rtpDownloadStopped, "service_stopped")
			}
			stream.Stop = true
			stream.Status = 1
			stream.EndReason = "service_stopped"
			g.sendStreamBYE(stream)
			if stream.mediaServer != nil && g.sms != nil {
				_, _ = g.sms.CloseRTPServer(stream.mediaServer, zlm.CloseRTPServerRequest{StreamID: stream.StreamID})
			}
			if g.core.Store() != nil {
				_ = g.core.EditPlaying(context.Background(), stream.DeviceID, stream.ChannelID, false)
			}
			return true
		})
	}
	g.inviteDialogs.Range(func(key, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog != nil {
			_ = g.sendInboundDialogBYE(dialog)
			if dialog.Broadcast != nil {
				_ = g.stopBroadcastSession(dialog.Broadcast, false)
			}
			if dialog.Cascade != nil {
				g.stopCascadeMediaSession(dialog.Cascade, false, false)
			}
		}
		g.inviteDialogs.CompareAndDelete(key, value)
		return true
	})
}

func (g *GB28181API) sendStreamBYE(stream *Streams) {
	if g == nil || stream == nil || stream.Resp == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return
	}
	ch, ok := g.svr.memoryStorer.GetChannel(stream.DeviceID, stream.ChannelID)
	if !ok || ch == nil || ch.Conn() == nil || ch.Source() == nil {
		return
	}
	req, err := sip.NewRequestFromResponseChecked(sip.MethodBYE, stream.Resp)
	if err != nil {
		return
	}
	req.SetDestination(ch.Source())
	req.SetConnection(ch.Conn())
	_, _ = g.svr.Request(req)
}

func (g *GB28181API) closeCascadeMediaSessions() {
	if g == nil {
		return
	}
	g.inviteDialogs.Range(func(key, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog == nil || dialog.Cascade == nil {
			return true
		}
		_ = g.sendInboundDialogBYE(dialog)
		g.inviteDialogs.CompareAndDelete(key, dialog)
		g.stopCascadeMediaSession(dialog.Cascade, false, false)
		return true
	})
}
