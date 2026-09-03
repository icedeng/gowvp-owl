package gbs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	// InitialRemoteCSeq 为远端 INVITE 序号；RemoteCSeq 为远端最近一次已接纳的对话内请求序号。
	InitialRemoteCSeq    uint32
	InitialRemoteCSeqSet bool
	RemoteCSeq           uint32
	RemoteCSeqSet        bool
	RemoteMethod         string
	RemoteFingerprint    [sha256.Size]byte
	RemoteResponse       *sip.Response
	Request              *sip.Request
	Response             *sip.Response
	TerminationResponse  *sip.Response
	Subject              *gbInviteSubject
	Broadcast            *broadcastSession
	Cascade              *cascadeMediaSession
	InviteTx             *sip.Transaction
	Cancelled            bool
	superseded           bool
	registration         inboundRegistrationBinding
	historyState         historyControlState
	remoteMu             sync.Mutex
	mu                   sync.Mutex
}

// nextLocalCSeqLocked 在持有 dialog.mu 时校验下一个本端对话序号，
// 但不提交状态。请求完成本地构造和安全头校验后才能真正推进序号。
func nextLocalCSeqLocked(dialog *inboundInviteDialog) (uint32, error) {
	if dialog == nil {
		return 0, fmt.Errorf("inbound dialog is unavailable")
	}
	next, err := sip.NextCSeq(dialog.LocalCSeq)
	if err != nil {
		return 0, fmt.Errorf("inbound dialog CSeq: %w", err)
	}
	return next, nil
}

// reserveLocalCSeqLocked 在持有 dialog.mu 时预留下一个本端对话序号。
// 达到 SIP 上界后应结束并重建对话，不能回绕。
func reserveLocalCSeqLocked(dialog *inboundInviteDialog) (uint32, error) {
	next, err := nextLocalCSeqLocked(dialog)
	if err != nil {
		return 0, err
	}
	dialog.LocalCSeq = next
	return next, nil
}

// respondInboundInviteTermination 只在终止响应真实写出后删除待处理 INVITE。
// 写失败时保留同一个响应（包括稳定的 To-tag），让原 INVITE 重传只重放终态，
// 不能在 CANCEL 已确认后重新进入媒体建链。
func (g *GB28181API) respondInboundInviteTermination(callID string, dialog *inboundInviteDialog, tx *sip.Transaction, response *sip.Response) error {
	if dialog == nil || tx == nil || response == nil {
		return fmt.Errorf("inbound INVITE termination response is unavailable")
	}
	if err := tx.Respond(response); err != nil {
		return err
	}
	if g != nil {
		g.inviteDialogs.CompareAndDelete(callID, dialog)
	}
	return nil
}

// replayInboundInviteFinalResponse 处理同一 INVITE 的业务层重传。
// 普通成功响应继续按既有语义重放；待确认终止响应写出成功后提交对话删除。
func (g *GB28181API) replayInboundInviteFinalResponse(ctx *sip.Context, callID string, dialog *inboundInviteDialog) bool {
	if ctx == nil || dialog == nil {
		return false
	}
	dialog.mu.Lock()
	termination := dialog.TerminationResponse
	response := dialog.Response
	if termination == nil && response != nil {
		dialog.UpdatedAt = time.Now()
	}
	dialog.mu.Unlock()
	if termination != nil {
		if err := g.respondInboundInviteTermination(callID, dialog, ctx.Tx, termination); err != nil {
			slog.Error("replay inbound INVITE termination", "err", err, "call_id", callID)
		}
		return true
	}
	if response == nil {
		return false
	}
	if err := ctx.Tx.Respond(response); err != nil {
		slog.Error("replay inbound INVITE response", "err", err, "call_id", callID)
	}
	return true
}

const (
	pendingInviteDialogTTL           = 10 * time.Minute
	mediaStatusCascadeDialogGraceTTL = 10 * time.Minute
)

type gbInviteSubject struct {
	SenderID         string
	SenderSequence   string
	ReceiverID       string
	ReceiverSequence string
}

func optionalGBInviteSubject(request *sip.Request) (*gbInviteSubject, error) {
	if request == nil {
		return nil, fmt.Errorf("INVITE request is nil")
	}
	headers := request.GetHeaders("Subject")
	if len(headers) == 0 {
		// 兼容未携带 Subject 的存量设备和上级平台；一旦携带则按附录 K/L 严格校验。
		return nil, nil
	}
	if len(headers) != 1 {
		return nil, fmt.Errorf("multiple Subject headers")
	}
	value := headers[0].String()
	_, value, ok := strings.Cut(value, ":")
	if !ok {
		return nil, fmt.Errorf("invalid Subject header")
	}
	return parseGBInviteSubject(strings.TrimSpace(value))
}

func requiredGBInviteSubject(request *sip.Request) (*gbInviteSubject, error) {
	subject, err := optionalGBInviteSubject(request)
	if err != nil {
		return nil, err
	}
	if subject == nil {
		return nil, fmt.Errorf("Subject header is required")
	}
	return subject, nil
}

func parseGBInviteSubject(value string) (*gbInviteSubject, error) {
	parts := strings.Split(value, ",")
	if len(parts) != 2 {
		return nil, fmt.Errorf("Subject must use senderID:senderSequence,receiverID:receiverSequence")
	}
	sender := strings.Split(parts[0], ":")
	receiver := strings.Split(parts[1], ":")
	if len(sender) != 2 || len(receiver) != 2 {
		return nil, fmt.Errorf("Subject must use senderID:senderSequence,receiverID:receiverSequence")
	}
	return &gbInviteSubject{
		SenderID:         strings.TrimSpace(sender[0]),
		SenderSequence:   strings.TrimSpace(sender[1]),
		ReceiverID:       strings.TrimSpace(receiver[0]),
		ReceiverSequence: strings.TrimSpace(receiver[1]),
	}, nil
}

func validateGBInviteSubject(subject *gbInviteSubject, senderID, receiverID string, senderSequencePrefix byte) error {
	if subject == nil {
		return nil
	}
	if !isGBDeviceIdentifier(subject.SenderID) {
		return fmt.Errorf("Subject sender id must be 20 ASCII digits")
	}
	if subject.SenderSequence == "" || !utf8.ValidString(subject.SenderSequence) || utf8.RuneCountInString(subject.SenderSequence) > 20 {
		return fmt.Errorf("Subject sender sequence must contain 1 to 20 characters")
	}
	if !isGBDeviceIdentifier(subject.ReceiverID) {
		return fmt.Errorf("Subject receiver id must be 20 ASCII digits")
	}
	if subject.ReceiverSequence == "" || !utf8.ValidString(subject.ReceiverSequence) {
		return fmt.Errorf("Subject receiver sequence is required")
	}
	if senderID = strings.TrimSpace(senderID); senderID != "" && subject.SenderID != senderID {
		return fmt.Errorf("Subject sender id does not match media source %s", senderID)
	}
	if receiverID = strings.TrimSpace(receiverID); receiverID != "" && subject.ReceiverID != receiverID {
		return fmt.Errorf("Subject receiver id does not match media receiver %s", receiverID)
	}
	if senderSequencePrefix != 0 && subject.SenderSequence[0] != senderSequencePrefix {
		return fmt.Errorf("Subject sender sequence must start with %c", senderSequencePrefix)
	}
	return nil
}

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

	session, err := g.findBroadcastSessionForInvite(ctx.DeviceID, ctx.Request)
	if session != nil {
		if authErr := g.authorizeInitialBroadcastInvite(session, ctx); authErr != nil {
			ctx.String(http.StatusForbidden, authErr.Error())
			return
		}
	}
	if err != nil {
		ctx.String(http.StatusBadRequest, err.Error())
		if session != nil {
			session.complete(err)
		}
		return
	}
	if session != nil {
		g.sipInviteBroadcast(ctx, callID, session)
		return
	}

	ctx.String(501, "unrecognized inbound media session")
}

func (g *GB28181API) authorizeInitialBroadcastInvite(session *broadcastSession, ctx *sip.Context) error {
	if session == nil || ctx == nil || strings.TrimSpace(ctx.DeviceID) != strings.TrimSpace(session.DeviceID) {
		return fmt.Errorf("broadcast receiver identity mismatch")
	}
	_, current, err := g.ensureRegisteredInboundDeviceWithBinding(ctx.DeviceID)
	if err != nil {
		if errors.Is(err, errInboundDeviceNotRegistered) {
			return fmt.Errorf("unregistered GB28181 device")
		}
		return err
	}
	if admitted, ok := admittedInboundRegistrationBinding(ctx); ok && admitted.device != nil {
		if admitted.device != current.device || admitted.expires != current.expires ||
			!admitted.lastRegisterAt.Equal(current.lastRegisterAt) {
			return errInboundDeviceGenerationChanged
		}
	}
	if _, ok := admittedInboundRegistrationBinding(ctx); !ok {
		ctx.Set(inboundRegistrationBindingContextKey, current)
	}
	if err := g.checkSourceAddress(ctx); err != nil {
		return err
	}
	return nil
}

// sipMediaRegistrationBindingMiddleware 将媒体请求入口绑定到当时的设备注册代次。
// 业务 handler 在 SIP 应答成功后再次复核该绑定，避免删除并同编码重建设备时，
// 已准入的旧请求误操作新代次复用的 Call-ID 会话。
func (g *GB28181API) sipMediaRegistrationBindingMiddleware(ctx *sip.Context) {
	if ctx == nil {
		return
	}
	if _, exists := admittedInboundRegistrationBinding(ctx); !exists {
		if _, binding, err := g.ensureRegisteredInboundDeviceWithBinding(ctx.DeviceID); err == nil {
			ctx.Set(inboundRegistrationBindingContextKey, binding)
		}
	}
	ctx.Next()
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
	if !inboundInviteTransactionMatches(dialog, ctx.Request) {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	dialog.mu.Lock()
	if dialog.Response != nil {
		dialog.mu.Unlock()
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	inviteTx := dialog.InviteTx
	// 将 CANCEL 的成功应答与原 INVITE 的最终响应提交串行化。否则在 200 OK
	// 写出期间 INVITE 可能先设置 Response，形成 CANCEL 成功但会话仍建立的竞态。
	if err := ctx.RespondString(200, "OK"); err != nil {
		dialog.mu.Unlock()
		slog.Error("respond CANCEL", "err", err, "call_id", callID)
		return
	}
	dialog.Cancelled = true
	var termination *sip.Response
	if dialog.Request != nil {
		termination = sip.NewResponseFromRequest("", dialog.Request, 487, "Request Terminated", nil)
	}
	dialog.TerminationResponse = termination
	dialog.mu.Unlock()
	if err := g.respondInboundInviteTermination(callID, dialog, inviteTx, termination); err != nil {
		slog.Error("respond cancelled INVITE", "err", err, "call_id", callID)
	}
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
	medias := sdpMediasByType(message, "audio")
	if len(medias) == 0 {
		return nil, fmt.Errorf("Broadcast INVITE does not contain audio media")
	}
	if len(medias) > 1 {
		return nil, fmt.Errorf("Broadcast INVITE must contain exactly one audio media description")
	}
	for _, media := range medias {
		if !strings.EqualFold(media.Description.Protocol, "RTP/AVP") {
			return nil, fmt.Errorf("Broadcast requires RTP/AVP audio")
		}
		if media.Description.Port <= 0 || media.Description.Port > 65535 {
			return nil, fmt.Errorf("invalid Broadcast RTP port")
		}
		direction, directionErr := effectiveSDPDirection(message, media)
		if directionErr != nil {
			return nil, fmt.Errorf("invalid Broadcast SDP direction: %w", directionErr)
		}
		if direction == "sendonly" || direction == "inactive" {
			return nil, fmt.Errorf("Broadcast receiver SDP must accept media")
		}
		remoteIP := media.Connection.IP
		networkType, addressType := media.Connection.NetworkType, media.Connection.AddressType
		if remoteIP == nil {
			remoteIP = message.Connection.IP
			networkType, addressType = message.Connection.NetworkType, message.Connection.AddressType
		}
		if err := validateSDPConnectionAddress(networkType, addressType, remoteIP); err != nil {
			return nil, fmt.Errorf("invalid Broadcast RTP address: %w", err)
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
	return nil, fmt.Errorf("Broadcast INVITE does not contain a usable audio media description")
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
			if !inboundInviteTransactionMatches(dialog, ctx.Request) {
				ctx.String(491, "Call-ID already in use")
				return
			}
			if !g.replayInboundInviteFinalResponse(ctx, callID, dialog) {
				ctx.String(100, "Trying")
			}
			return
		}
		ctx.String(491, "Call-ID already in use")
		return
	}
	if err := validateSIPContentType(ctx.Request, string(sip.ContentTypeSDP)); err != nil {
		ctx.String(http.StatusUnsupportedMediaType, "Content-Type must be application/sdp")
		session.complete(err)
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
	cleanup := func(sendBYE bool) {
		if cleanupErr := g.stopBroadcastSession(session, sendBYE); cleanupErr != nil {
			slog.WarnContext(g.mediaPersistenceContext(), "cleanup failed Broadcast INVITE", "device_id", session.DeviceID, "channel_id", session.ChannelID, "call_id", callID, "err", cleanupErr)
		}
	}
	defer func() {
		session.mu.Lock()
		session.inviteBusy = false
		stopped := session.stopped
		session.mu.Unlock()
		if stopped {
			cleanup(true)
		}
	}()
	remoteCSeq, remoteCSeqSet := sipRequestCSeq(ctx.Request, sip.MethodInvite)
	dialog := &inboundInviteDialog{
		CallID: callID, DeviceID: strings.TrimSpace(ctx.DeviceID), RemoteTag: sipRequestFromTag(ctx.Request), InitialToTag: sipRequestToTag(ctx.Request), TagsBound: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		LocalCSeq: 1, InitialRemoteCSeq: remoteCSeq, InitialRemoteCSeqSet: remoteCSeqSet,
		RemoteCSeq: remoteCSeq, RemoteCSeqSet: remoteCSeqSet, RemoteMethod: sip.MethodInvite,
		Request: ctx.Request, Broadcast: session, InviteTx: ctx.Tx,
	}
	dialog.registration, _ = admittedInboundRegistrationBinding(ctx)
	if _, loaded := g.inviteDialogs.LoadOrStore(callID, dialog); loaded {
		ctx.String(491, "Call-ID already in use")
		return
	}
	fail := func(status int, cause error) {
		dialog.mu.Lock()
		cancelled := dialog.Cancelled
		retainTermination := cancelled && dialog.TerminationResponse != nil
		dialog.mu.Unlock()
		if !retainTermination {
			g.inviteDialogs.CompareAndDelete(callID, dialog)
		}
		if !cancelled {
			ctx.String(status, cause.Error())
		}
		session.complete(cause)
		cleanup(false)
	}
	ssrc, releaseSSRC, err := g.reserveSSRC(0)
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	if err := session.Stream.bindSSRCReservation(ssrc, releaseSSRC); err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	started, err := startSendRTPContext(g.serviceContext(), g.sms, session.SMS, zlm.StartSendRTPRequest{
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
	// StartSendRTP 成功即登记发送端所有权。后续端口、SDP、设备代次或 SIP 应答失败时，
	// 统一停止状态机才能在媒体节点瞬时失败后保留对象并由后台清理器重试。
	session.mu.Lock()
	session.SSRC = ssrc
	session.rtpStarted = true
	session.mu.Unlock()
	if started == nil || started.LocalPort <= 0 || started.LocalPort > 65535 {
		err = fmt.Errorf("media server returned invalid Broadcast RTP port")
		fail(500, err)
		return
	}

	answer, err := buildBroadcastSDPAnswer(session, started.LocalPort, offer.Payload, offer.Mapping, ssrc)
	if err != nil {
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
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		fail(487, err)
		return
	}
	defer unlockCommit()
	session.mu.Lock()
	if session.stopped {
		session.mu.Unlock()
		cleanup(false)
		dialog.mu.Lock()
		cancelled := dialog.Cancelled
		retainTermination := cancelled && dialog.TerminationResponse != nil
		dialog.mu.Unlock()
		if !retainTermination {
			g.inviteDialogs.CompareAndDelete(callID, dialog)
		}
		if !cancelled {
			ctx.String(487, "Broadcast session terminated")
		}
		return
	}
	dialog.mu.Lock()
	if dialog.Cancelled {
		retainTermination := dialog.TerminationResponse != nil
		dialog.mu.Unlock()
		session.mu.Unlock()
		if !retainTermination {
			g.inviteDialogs.CompareAndDelete(callID, dialog)
		}
		cleanup(false)
		return
	}
	dialog.Response = resp
	dialog.LocalTag = sipResponseToTag(resp)
	dialog.UpdatedAt = time.Now()
	dialog.mu.Unlock()
	session.Dialog = dialog
	session.Stream.CallID = callID
	session.Stream.Status = 0
	session.mu.Unlock()
	if err := ctx.Tx.Respond(resp); err != nil {
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		cleanup(false)
		session.complete(fmt.Errorf("respond Broadcast INVITE: %w", err))
		return
	}
}

func buildBroadcastSDPAnswer(session *broadcastSession, port, payload int, mapping, ssrc string) ([]byte, error) {
	ip4str, err := GetIP(session.SMS.GetSDPIP())
	if err != nil {
		return nil, err
	}
	address, err := parseSDPAddress(ip4str)
	if err != nil {
		return nil, err
	}
	audio := sdp.Media{Description: sdp.MediaDescription{
		Type: "audio", Port: port, Formats: []string{strconv.Itoa(payload)}, Protocol: "RTP/AVP",
	}}
	audio.AddAttribute("sendonly")
	audio.AddAttribute("rtpmap", strconv.Itoa(payload), mapping)
	message := &sdp.Message{
		Origin: sdp.Origin{Username: session.SourceID, NetworkType: "IN", AddressType: address.Type, Address: address.Canonical},
		Name:   historyModePlay,
		Connection: sdp.ConnectionData{
			NetworkType: "IN", AddressType: address.Type, IP: address.IP,
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
	d.remoteMu.Lock()
	defer d.remoteMu.Unlock()
	response, duplicate, accepted := acceptInboundDialogRequest(d, ctx.Request)
	if duplicate {
		if response != nil {
			if err := ctx.Tx.Respond(response); err == nil && response.StatusCode() >= 200 && response.StatusCode() < 300 {
				g.commitInboundDialogBYE(callID, d)
			}
		} else {
			respondInboundDialogCSeqError(ctx)
		}
		return
	}
	if !accepted {
		respondInboundDialogCSeqError(ctx)
		return
	}
	response = sip.NewResponseFromRequest("", ctx.Request, http.StatusOK, "OK", nil)
	cacheInboundDialogResponse(d, response)
	if err := ctx.Tx.Respond(response); err != nil {
		slog.Error("respond inbound BYE", "err", err, "call_id", callID)
		return
	}
	g.commitInboundDialogBYE(callID, d)
}

func (g *GB28181API) commitInboundDialogBYE(callID string, dialog *inboundInviteDialog) {
	if g == nil || dialog == nil || !g.inviteDialogs.CompareAndDelete(callID, dialog) {
		return
	}
	if dialog.Broadcast != nil {
		_ = g.stopBroadcastSession(dialog.Broadcast, false)
	}
	if dialog.Cascade != nil {
		g.stopCascadeMediaSession(dialog.Cascade, false, false)
	}
}

func (g *GB28181API) handleOutboundBYE(ctx *sip.Context, callID string) bool {
	if g.streams == nil {
		return false
	}
	matched := false
	var endedStream *Streams
	endedKey := ""
	endedDownload := false
	g.streams.Range(func(key string, stream *Streams) bool {
		if stream == nil || stream.DeviceID != ctx.DeviceID || normalizeStoredCallID(stream.CallID) != callID ||
			!outboundDialogTagsMatch(stream.Resp, ctx.Request) || !outboundDialogSourceMatches(stream.Resp, ctx) {
			return true
		}
		matched = true
		endedStream = stream
		endedKey = key
		endedDownload = strings.HasPrefix(key, "history:"+historyModeDownload+":") && !stream.DirectTCP
		return false
	})
	if matched {
		if err := ctx.RespondString(200, "OK"); err != nil {
			slog.Error("respond remote BYE", "err", err, "device_id", ctx.DeviceID, "call_id", callID)
			return true
		}
		unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
		if err != nil {
			return true
		}
		defer unlockCommit()
		firstStop := g.markMediaStreamStopped(endedStream, "remote_bye", true)
		if firstStop && endedDownload {
			g.finishRTPDownload(endedStream, rtpDownloadStopped, "remote_bye")
		}
		if value, ok := g.talkSessions.Load(endedStream.StreamID); ok {
			if session, ok := value.(*talkSession); ok {
				_ = g.stopTalkSession(session, fmt.Errorf("Talk ended by remote BYE"))
			}
		} else if _, err := g.cleanupMediaStreamContext(g.mediaPersistenceContext(), endedKey, endedStream); err != nil {
			slog.Warn("cleanup media after remote BYE failed", "device_id", endedStream.DeviceID, "channel_id", endedStream.ChannelID, "stream_id", endedStream.StreamID, "err", err)
		}
		if err := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), endedStream.DeviceID, endedStream.ChannelID); err != nil {
			slog.Warn("persist remote BYE channel state", "device_id", endedStream.DeviceID, "channel_id", endedStream.ChannelID, "err", err)
		}
		if firstStop {
			g.terminateCascadeSessionsForStream(endedStream)
			g.stopStandardTalkForPlayKey(endedKey)
		}
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
	if !inboundInviteCSeqMatches(d, ctx.Request, sip.MethodACK) {
		return
	}
	d.mu.Lock()
	d.Established = true
	d.UpdatedAt = time.Now()
	superseded := d.superseded
	d.mu.Unlock()
	if superseded && d.Cascade != nil {
		g.terminateSupersededCascadeDialog(d)
		return
	}
	if d.Cascade != nil && d.Cascade.directRelaySnapshot() != nil {
		g.startCascadeDirectTCPRelay(d.Cascade)
	}
	if d.Broadcast != nil {
		d.Broadcast.complete(nil)
	}
}

type cascadeMANSRTSPRequest struct {
	method     string
	version    string
	cseq       uint32
	scale      float64
	hasScale   bool
	rangeValue string
	headers    []string
}

type historyControlResponse struct {
	version  string
	status   int
	reason   string
	cseq     uint32
	scale    float64
	hasScale bool
	headers  []string
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
	dialog.mu.Unlock()
	source := dialog.Cascade.sourceSnapshot()
	if !established || source == nil || source.stream == nil || source.channel == nil || source.mode == historyModePlay {
		ctx.String(481, "history dialog is not established")
		return
	}
	dialog.remoteMu.Lock()
	defer dialog.remoteMu.Unlock()
	if response, duplicate, accepted := acceptInboundDialogRequest(dialog, ctx.Request); duplicate {
		if response != nil {
			if err := ctx.Tx.Respond(response); err == nil && response.StatusCode() >= 200 && response.StatusCode() < 300 {
				dialog.mu.Lock()
				dialog.UpdatedAt = time.Now()
				dialog.mu.Unlock()
				if command, parseErr := parseCascadeMANSRTSP(ctx.Request.Body()); parseErr == nil && command.method == "TEARDOWN" {
					g.commitInboundHistoryTeardown(callID, dialog)
				}
			}
		} else {
			respondInboundDialogCSeqError(ctx)
		}
		return
	} else if !accepted {
		respondInboundDialogCSeqError(ctx)
		return
	}
	if err := validateSIPContentType(ctx.Request, "Application/MANSRTSP"); err != nil {
		respondAndCacheInboundDialog(dialog, ctx, http.StatusUnsupportedMediaType, "Content-Type must be Application/MANSRTSP")
		return
	}
	command, err := parseCascadeMANSRTSP(ctx.Request.Body())
	if err != nil {
		respondAndCacheInboundDialog(dialog, ctx, http.StatusBadRequest, err.Error())
		return
	}
	upstreamVersion := GBVersion10
	if dialog.Cascade.worker != nil {
		upstreamVersion = dialog.Cascade.worker.protocolVersion()
	}
	if expected := historyControlProtocolVersion(upstreamVersion); command.version != expected {
		respondAndCacheInboundDialog(dialog, ctx, http.StatusBadRequest, "MANSRTSP version does not match negotiated GB version")
		return
	}
	if err := validateHistoryControlCommandWithState(command, upstreamVersion, source.stream, &dialog.historyState); err != nil {
		respondAndCacheInboundDialog(dialog, ctx, http.StatusBadRequest, err.Error())
		return
	}

	var businessResponse *historyControlResponse
	requestCtx, cancelRequest := withCascadeWorkerOperation(g.serviceContext(), dialog.Cascade.worker)
	defer cancelRequest()
	source.controlMu.Lock()
	if err = requestCtx.Err(); err == nil {
		downstreamVersion := GBVersion10
		if err = g.validateCascadeRuntimeDeviceTarget(source.channel.DeviceID); err == nil && g.svr != nil && g.svr.memoryStorer != nil {
			downstreamVersion = g.getDeviceGBProtocolVersion(source.channel.DeviceID)
		}
		if err == nil {
			err = validateHistoryControlCommandWithState(command, downstreamVersion, source.stream, &source.stream.historyState)
		}
		if err == nil {
			downstreamCSeq, cseqErr := source.stream.nextCSeq()
			if cseqErr != nil {
				err = cseqErr
			} else {
				downstreamBody := command.body(downstreamCSeq, historyControlProtocolVersion(downstreamVersion))
				if g.cascadeControlHistory != nil {
					err = g.cascadeControlHistory(requestCtx, &ControlHistoryInput{
						Channel: source.channel, Mode: source.mode, Cmd: string(downstreamBody), sessionKey: source.key,
					})
				} else {
					businessResponse, err = g.controlHistory(requestCtx, &ControlHistoryInput{
						Channel: source.channel, Mode: source.mode, Cmd: string(downstreamBody), sessionKey: source.key,
					})
				}
			}
		}
	}
	source.controlMu.Unlock()
	if err != nil && businessResponse == nil {
		response := sip.NewResponseFromRequest("", ctx.Request, http.StatusBadGateway, err.Error(), nil)
		cacheInboundDialogResponse(dialog, response)
		_ = ctx.Tx.Respond(response)
		return
	}
	source.stream.historyState.commitResult(command, businessResponse, err)
	dialog.historyState.commitResult(command, businessResponse, err)

	responseBody := []byte(fmt.Sprintf("%s 200 OK\r\nCSeq: %d\r\n\r\n", command.version, command.cseq))
	if businessResponse != nil {
		responseBody = businessResponse.body(command.cseq, command.version)
	}
	response := sip.NewResponseFromRequest("", ctx.Request, http.StatusOK, "OK", responseBody)
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	cacheInboundDialogResponse(dialog, response)
	if err := ctx.Tx.Respond(response); err != nil {
		slog.Error("respond cascade INFO", "err", err, "call_id", callID, "method", command.method)
		return
	}
	dialog.mu.Lock()
	dialog.UpdatedAt = time.Now()
	dialog.mu.Unlock()
	if command.method == "TEARDOWN" && err == nil {
		g.commitInboundHistoryTeardown(callID, dialog)
	}
}

func (g *GB28181API) commitInboundHistoryTeardown(callID string, dialog *inboundInviteDialog) {
	if g == nil || dialog == nil || !g.inviteDialogs.CompareAndDelete(callID, dialog) {
		return
	}
	g.stopCascadeMediaSession(dialog.Cascade, false, true)
}

func sipRequestCSeq(request *sip.Request, method string) (uint32, bool) {
	if request == nil {
		return 0, false
	}
	cseq, ok := request.CSeq()
	if !ok || cseq == nil || !strings.EqualFold(strings.TrimSpace(cseq.MethodName), strings.TrimSpace(method)) {
		return 0, false
	}
	return cseq.SeqNo, true
}

func inboundInviteCSeqMatches(dialog *inboundInviteDialog, request *sip.Request, method string) bool {
	incoming, ok := sipRequestCSeq(request, method)
	if !ok || dialog == nil {
		return false
	}
	dialog.mu.Lock()
	initial := dialog.InitialRemoteCSeq
	initialSet := dialog.InitialRemoteCSeqSet
	dialogRequest := dialog.Request
	dialog.mu.Unlock()
	if initialSet {
		return incoming == initial
	}
	if cseq, found := sipRequestCSeq(dialogRequest, sip.MethodInvite); found {
		return incoming == cseq
	}
	// 兼容内部旧测试/清理状态；协议入口创建的会话始终记录初始 CSeq。
	return dialogRequest == nil
}

func inboundInviteTransactionMatches(dialog *inboundInviteDialog, request *sip.Request) bool {
	if dialog == nil || request == nil || !inboundDialogTagsMatch(dialog, request, false) || !inboundInviteCSeqMatches(dialog, request, request.Method()) {
		return false
	}
	dialog.mu.Lock()
	initial := dialog.Request
	dialog.mu.Unlock()
	if initial == nil {
		// 兼容内部旧测试/清理状态；协议入口创建的会话始终保存初始 INVITE。
		return true
	}
	if initial.Recipient() == nil || request.Recipient() == nil || initial.Recipient().String() != request.Recipient().String() {
		return false
	}
	initialVia, initialOK := initial.ViaHop()
	requestVia, requestOK := request.ViaHop()
	if !initialOK || !requestOK || initialVia == nil || requestVia == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(initialVia.ProtocolName), strings.TrimSpace(requestVia.ProtocolName)) &&
		strings.EqualFold(strings.TrimSpace(initialVia.ProtocolVersion), strings.TrimSpace(requestVia.ProtocolVersion)) &&
		strings.EqualFold(strings.TrimSpace(initialVia.Transport), strings.TrimSpace(requestVia.Transport)) &&
		strings.EqualFold(strings.TrimSpace(initialVia.SentBy()), strings.TrimSpace(requestVia.SentBy())) &&
		sipViaBranch(initialVia) != "" && sipViaBranch(initialVia) == sipViaBranch(requestVia)
}

func sipViaBranch(via *sip.ViaHop) string {
	if via == nil || via.Params == nil {
		return ""
	}
	branch, ok := via.Params.Get("branch")
	if !ok || branch == nil {
		return ""
	}
	return strings.TrimSpace(branch.String())
}

func acceptInboundDialogRequest(dialog *inboundInviteDialog, request *sip.Request) (*sip.Response, bool, bool) {
	incoming, ok := sipRequestCSeq(request, request.Method())
	if !ok || dialog == nil {
		return nil, false, false
	}
	dialog.mu.Lock()
	defer dialog.mu.Unlock()
	last := dialog.RemoteCSeq
	lastSet := dialog.RemoteCSeqSet
	if !lastSet {
		last = dialog.InitialRemoteCSeq
		lastSet = dialog.InitialRemoteCSeqSet
		if !lastSet {
			if initial, found := sipRequestCSeq(dialog.Request, sip.MethodInvite); found {
				last = initial
				lastSet = true
			}
		}
	}
	fingerprint := sha256.Sum256([]byte(request.String()))
	if lastSet && incoming == last && strings.EqualFold(dialog.RemoteMethod, request.Method()) {
		if dialog.RemoteFingerprint == fingerprint {
			return dialog.RemoteResponse, true, false
		}
		return nil, false, false
	}
	if lastSet && incoming <= last {
		return nil, false, false
	}
	dialog.RemoteCSeq = incoming
	dialog.RemoteCSeqSet = true
	dialog.RemoteMethod = request.Method()
	dialog.RemoteFingerprint = fingerprint
	dialog.RemoteResponse = nil
	return nil, false, true
}

func cacheInboundDialogResponse(dialog *inboundInviteDialog, response *sip.Response) {
	if dialog == nil {
		return
	}
	dialog.mu.Lock()
	dialog.RemoteResponse = response
	dialog.mu.Unlock()
}

func respondAndCacheInboundDialog(dialog *inboundInviteDialog, ctx *sip.Context, status int, reason string) {
	if ctx == nil || ctx.Request == nil || ctx.Tx == nil {
		return
	}
	response := sip.NewResponseFromRequest("", ctx.Request, status, reason, nil)
	cacheInboundDialogResponse(dialog, response)
	_ = ctx.Tx.Respond(response)
}

func respondInboundDialogCSeqError(ctx *sip.Context) {
	if ctx == nil || ctx.Request == nil || ctx.Tx == nil {
		return
	}
	response := sip.NewResponseFromRequest("", ctx.Request, http.StatusInternalServerError, "CSeq out of order", nil)
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Retry-After", Contents: "0"})
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
	if admitted, ok := admittedInboundRegistrationBinding(ctx); ok && admitted.device != nil &&
		dialog.registration.device != nil && admitted.device != dialog.registration.device {
		return false
	}
	if dialog.registration.device != nil {
		unlock, err := g.lockInboundDeviceStateCommit(dialog.DeviceID, dialog.registration)
		if err != nil {
			return false
		}
		unlock()
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
		// 2xx ACK 必须携带最终响应分配的 To-tag。最终响应尚未生成时本地 tag 为空，
		// 不能把提前到达的无 tag ACK 当成合法确认并建立媒体对话。
		if tagsBound && strings.TrimSpace(localTag) == "" {
			return false
		}
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

func outboundDialogSourceMatches(response *sip.Response, ctx *sip.Context) bool {
	if ctx == nil {
		return false
	}
	if response == nil || response.Source() == nil {
		// 兼容内部旧调用构造的无响应来源流；生产会话保存收到 2xx 的来源。
		return true
	}
	currentSource := ctx.Source
	if currentSource == nil && ctx.Request != nil {
		currentSource = ctx.Request.Source()
	}
	return currentSource != nil && addressIP(response.Source()).Equal(addressIP(currentSource))
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
	return cascadeRegistrationActive(state, time.Now()) && worker.remoteAddressMatches(ctx.Source)
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
			request.scale = scale
			request.hasScale = true
			request.headers = append(request.headers, "Scale: "+value)
		case "range":
			if method != "PLAY" || !validMANSRTSPRange(value) {
				return nil, fmt.Errorf("invalid MANSRTSP Range: %s", value)
			}
			request.rangeValue = value
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
	if request.method == "PAUSE" && request.version == "RTSP/1.0" {
		if _, ok := seen["pausetime"]; !ok {
			return nil, fmt.Errorf("RTSP PAUSE requires PauseTime: now")
		}
	}
	return request, nil
}

func validateHistoryControlCommand(request *cascadeMANSRTSPRequest, version GBProtocolVersion, stream *Streams) error {
	var state *historyControlState
	if stream != nil {
		state = &stream.historyState
	}
	return validateHistoryControlCommandWithState(request, version, stream, state)
}

func validateHistoryControlCommandWithState(request *cascadeMANSRTSPRequest, version GBProtocolVersion, stream *Streams, state *historyControlState) error {
	if request == nil {
		return fmt.Errorf("history control command is unavailable")
	}
	if request.hasScale && request.rangeValue != "" && version.AtLeast(GBVersion11) {
		if !version.AtLeast(GBVersion30) || request.scale >= 0 {
			return fmt.Errorf("GB/T 28181-%s does not allow this PLAY command to combine Scale and Range", version.StandardYear())
		}
	}
	if request.rangeValue == "" || stream == nil || stream.S.IsZero() || stream.E.IsZero() || stream.E.Before(stream.S) {
		return nil
	}
	start, end, startNow, endPresent, ok := parseMANSRTSPRange(request.rangeValue)
	if !ok || startNow {
		return nil
	}
	if endPresent {
		reverseRange := version.AtLeast(GBVersion30) && state.effectiveScale(request) < 0
		if reverseRange {
			if end >= start {
				return fmt.Errorf("GB/T 28181-2022 reverse Range end must be less than start")
			}
		} else if end < start {
			return fmt.Errorf("MANSRTSP Range end must not precede start")
		}
	}
	duration := stream.E.Sub(stream.S).Seconds()
	if start > duration || endPresent && end > duration {
		return fmt.Errorf("MANSRTSP Range exceeds history session duration")
	}
	return nil
}

func validMANSRTSPRange(value string) bool {
	_, _, _, _, ok := parseMANSRTSPRange(value)
	return ok
}

func parseMANSRTSPRange(value string) (start, end float64, startNow, endPresent, ok bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	prefix := ""
	switch {
	case strings.HasPrefix(value, "npt="):
		prefix, value = "npt", strings.TrimPrefix(value, "npt=")
	case strings.HasPrefix(value, "smpte="):
		prefix, value = "smpte", strings.TrimPrefix(value, "smpte=")
	default:
		return 0, 0, false, false, false
	}
	startValue, endValue, found := strings.Cut(value, "-")
	if !found || strings.Contains(endValue, "-") || startValue == "" {
		return 0, 0, false, false, false
	}
	if prefix == "npt" {
		if startValue == "now" {
			// RFC 2326/GB/T 28181 only defines the open-ended now form.
			return 0, 0, true, false, endValue == ""
		}
		start, ok = parseMANSRTSPSeconds(startValue)
		if !ok {
			return 0, 0, false, false, false
		}
		if endValue == "" {
			return start, 0, false, false, true
		}
		end, ok = parseMANSRTSPSeconds(endValue)
		return start, end, false, true, ok
	}
	start, ok = parseMANSRTSPSMPTETime(startValue)
	if !ok {
		return 0, 0, false, false, false
	}
	if endValue == "" {
		return start, 0, false, false, true
	}
	end, ok = parseMANSRTSPSMPTETime(endValue)
	return start, end, false, true, ok
}

func validMANSRTSPSeconds(value string) bool {
	_, ok := parseMANSRTSPSeconds(value)
	return ok
}

func parseMANSRTSPSeconds(value string) (float64, bool) {
	if value == "" || value == "." || strings.Count(value, ".") > 1 {
		return 0, false
	}
	for _, char := range value {
		if char != '.' && (char < '0' || char > '9') {
			return 0, false
		}
	}
	seconds, err := strconv.ParseFloat(value, 64)
	return seconds, err == nil && seconds >= 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0)
}

func validMANSRTSPSMPTETime(value string) bool {
	_, ok := parseMANSRTSPSMPTETime(value)
	return ok
}

func parseMANSRTSPSMPTETime(value string) (float64, bool) {
	main, subframes, hasSubframes := strings.Cut(value, ".")
	if hasSubframes && (subframes == "" || len(subframes) > 2 || !allDecimalDigits(subframes) || strings.Contains(subframes, ".")) {
		return 0, false
	}
	parts := strings.Split(main, ":")
	if len(parts) != 3 && len(parts) != 4 {
		return 0, false
	}
	if hasSubframes && len(parts) != 4 {
		return 0, false
	}
	values := make([]uint64, len(parts))
	for index, part := range parts {
		if part == "" || len(part) > 2 {
			return 0, false
		}
		parsed, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return 0, false
		}
		values[index] = parsed
	}
	if values[1] > 59 || values[2] > 59 {
		return 0, false
	}
	if len(values) == 4 && values[3] > 29 {
		return 0, false
	}
	seconds := float64(values[0]*3600 + values[1]*60 + values[2])
	if len(values) == 4 {
		frames := float64(values[3])
		if hasSubframes {
			fraction, err := strconv.ParseFloat("0."+subframes, 64)
			if err != nil {
				return 0, false
			}
			frames += fraction
		}
		seconds += frames / 30
	}
	return seconds, true
}

func parseHistoryControlSIPResponse(response *sip.Response, expectedVersion string, expectedCSeq uint32) (*historyControlResponse, error) {
	if response == nil {
		return nil, fmt.Errorf("history control SIP response is unavailable")
	}
	body := response.Body()
	if strings.TrimSpace(string(body)) == "" {
		return nil, nil
	}
	if err := validateSIPContentType(response, "Application/MANSRTSP"); err != nil {
		return nil, fmt.Errorf("history control response %w", err)
	}
	business, err := parseHistoryControlResponse(body)
	if err != nil {
		return nil, err
	}
	if business.version != strings.ToUpper(strings.TrimSpace(expectedVersion)) {
		return nil, fmt.Errorf("history control response version %s does not match request %s", business.version, expectedVersion)
	}
	if business.cseq != expectedCSeq {
		return nil, fmt.Errorf("history control response CSeq %d does not match request %d", business.cseq, expectedCSeq)
	}
	if business.status != http.StatusOK {
		return business, fmt.Errorf("history control failed: %d %s", business.status, business.reason)
	}
	return business, nil
}

func validateSIPContentType(message sip.Message, expected string) error {
	if message == nil {
		return fmt.Errorf("Content-Type is unavailable")
	}
	headers := message.GetHeaders("Content-Type")
	if len(headers) != 1 {
		return fmt.Errorf("must contain exactly one Content-Type")
	}
	value := ""
	switch header := headers[0].(type) {
	case *sip.ContentType:
		if header != nil {
			value = string(*header)
		}
	case *sip.GenericHeader:
		if header != nil {
			value = header.Contents
		}
	default:
		if header != nil {
			value = header.String()
			if _, after, ok := strings.Cut(value, ":"); ok {
				value = after
			}
		}
	}
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil || !strings.EqualFold(mediaType, expected) {
		return fmt.Errorf("Content-Type must be %s", expected)
	}
	return nil
}

func parseHistoryControlResponse(body []byte) (*historyControlResponse, error) {
	if len(body) == 0 || len(body) > 4096 {
		return nil, fmt.Errorf("invalid MANSRTSP response body length")
	}
	text := strings.ReplaceAll(string(body), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("incomplete MANSRTSP response")
	}
	start := strings.Fields(strings.TrimSpace(lines[0]))
	if len(start) < 2 {
		return nil, fmt.Errorf("invalid MANSRTSP response status line")
	}
	version := strings.ToUpper(start[0])
	if version != "MANSRTSP/1.0" && version != "RTSP/1.0" {
		return nil, fmt.Errorf("unsupported MANSRTSP response version: %s", start[0])
	}
	status, err := strconv.Atoi(start[1])
	if err != nil || status != http.StatusOK && (status < 400 || status > 599) {
		return nil, fmt.Errorf("invalid MANSRTSP response status: %s", start[1])
	}
	response := &historyControlResponse{version: version, status: status, reason: strings.TrimSpace(strings.Join(start[2:], " "))}
	seen := make(map[string]struct{}, len(lines)-1)
	for _, raw := range lines[1:] {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("invalid MANSRTSP response header: %s", line)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		value = strings.TrimSpace(value)
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate MANSRTSP response header: %s", name)
		}
		seen[name] = struct{}{}
		switch name {
		case "cseq":
			parsed, err := strconv.ParseUint(value, 10, 32)
			if err != nil || parsed == 0 {
				return nil, fmt.Errorf("invalid MANSRTSP response CSeq: %s", value)
			}
			response.cseq = uint32(parsed)
		case "range":
			if !validMANSRTSPRange(value) {
				return nil, fmt.Errorf("invalid MANSRTSP response Range: %s", value)
			}
			response.headers = append(response.headers, "Range: "+value)
		case "scale":
			scale, err := strconv.ParseFloat(value, 64)
			if err != nil || scale == 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
				return nil, fmt.Errorf("invalid MANSRTSP response Scale: %s", value)
			}
			response.scale = scale
			response.hasScale = true
			response.headers = append(response.headers, "Scale: "+value)
		case "rtp-info":
			if !validMANSRTSPRTPInfo(value) {
				return nil, fmt.Errorf("invalid MANSRTSP response RTP-Info: %s", value)
			}
			response.headers = append(response.headers, "RTP-Info: "+value)
		default:
			return nil, fmt.Errorf("unsupported MANSRTSP response header: %s", name)
		}
	}
	if response.cseq == 0 {
		return nil, fmt.Errorf("MANSRTSP response CSeq is required")
	}
	return response, nil
}

func validMANSRTSPRTPInfo(value string) bool {
	for _, entry := range strings.Split(value, ",") {
		seenURL, seenSeq, seenRTPTime := false, false, false
		for _, parameter := range strings.Split(strings.TrimSpace(entry), ";") {
			name, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || strings.TrimSpace(raw) == "" {
				return false
			}
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "url":
				// URL 由下级设备生成，只要求非空且保持为单行文本。
				if seenURL {
					return false
				}
				seenURL = true
			case "seq":
				if _, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 16); err != nil || seenSeq {
					return false
				}
				seenSeq = true
			case "rtptime":
				if _, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32); err != nil || seenRTPTime {
					return false
				}
				seenRTPTime = true
			default:
				return false
			}
		}
		// RFC 2326 将 seq/rtptime 定义为可选参数，GB/T 28181-2022 也仅要求“宜携带”。
		// 至少保留一个已知参数，拒绝空条目和重复参数。
		if !seenURL && !seenSeq && !seenRTPTime {
			return false
		}
	}
	return strings.TrimSpace(value) != ""
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

func (r *historyControlResponse) body(cseq uint32, version string) []byte {
	if r == nil {
		return nil
	}
	var builder strings.Builder
	reason := strings.TrimSpace(r.reason)
	if reason == "" {
		reason = http.StatusText(r.status)
	}
	fmt.Fprintf(&builder, "%s %d %s\r\nCSeq: %d\r\n", version, r.status, reason, cseq)
	for _, header := range r.headers {
		builder.WriteString(header)
		builder.WriteString("\r\n")
	}
	builder.WriteString("\r\n")
	return []byte(builder.String())
}

func (g *GB28181API) sendInboundDialogBYE(dialog *inboundInviteDialog) error {
	return g.sendInboundDialogBYEContext(context.Background(), dialog)
}

func (g *GB28181API) sendInboundDialogBYEContext(ctx context.Context, dialog *inboundInviteDialog) error {
	if dialog == nil {
		return nil
	}
	if g == nil || g.svr == nil {
		return fmt.Errorf("SIP server is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dialog.mu.Lock()
	if !dialog.Established || dialog.Request == nil || dialog.Response == nil {
		dialog.mu.Unlock()
		return nil
	}
	request := dialog.Request
	response := dialog.Response
	cascade := dialog.Cascade
	broadcast := dialog.Broadcast
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
	dialog.mu.Lock()
	if !dialog.Established || dialog.Request != request || dialog.Response != response ||
		dialog.Cascade != cascade || dialog.Broadcast != broadcast {
		dialog.mu.Unlock()
		return fmt.Errorf("inbound dialog changed before BYE")
	}
	baseCSeq := dialog.LocalCSeq
	cseq, cseqErr := nextLocalCSeqLocked(dialog)
	dialog.mu.Unlock()
	if cseqErr != nil {
		return cseqErr
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
	if cascade != nil && cascade.worker != nil {
		if cascadeContact := cascade.worker.contactAddress(); cascadeContact != nil {
			contact = cascadeContact
		}
		version = string(cascade.worker.protocolVersion())
	} else if broadcast != nil {
		version = string(broadcast.Version)
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
	if cascade != nil && cascade.worker != nil {
		identityCtx := ctx
		if cascade.identityCtx != nil {
			identity := monitorUserIdentityFromContext(cascade.identityCtx)
			localGatewayID, _ := cascade.identityCtx.Value(monitorUserIdentityGatewayContextKey{}).(string)
			identityCtx = withMonitorUserIdentityRoute(ctx, identity, localGatewayID)
		}
		if err := cascade.worker.platform.monitorUserIdentity.apply(identityCtx, bye); err != nil {
			return err
		}
	}
	deviceID, channelID := dialog.DeviceID, ""
	if broadcast != nil {
		deviceID, channelID = broadcast.DeviceID, broadcast.ChannelID
	}
	target := g.svr.dialogTarget(deviceID, channelID)
	if err := ctx.Err(); err != nil {
		return err
	}
	dialog.mu.Lock()
	if !dialog.Established || dialog.Request != request || dialog.Response != response ||
		dialog.Cascade != cascade || dialog.Broadcast != broadcast || dialog.LocalCSeq != baseCSeq {
		dialog.mu.Unlock()
		return fmt.Errorf("inbound dialog changed before BYE")
	}
	dialog.LocalCSeq = cseq
	dialog.mu.Unlock()
	if cascade != nil && cascade.worker != nil {
		return cascade.worker.sendRequestWithDigestAsyncPreparedContext(ctx, bye, func(retry *sip.Request) error {
			return commitInboundDialogBYERetryCSeq(dialog, request, response, cascade, broadcast, retry)
		})
	}
	tx, err := g.svr.requestDialogCleanupContext(ctx, target, bye)
	if err == nil {
		g.consumeDialogResponseAsync(tx)
	}
	return err
}

func commitInboundDialogBYERetryCSeq(
	dialog *inboundInviteDialog,
	dialogRequest *sip.Request,
	dialogResponse *sip.Response,
	cascade *cascadeMediaSession,
	broadcast *broadcastSession,
	retry *sip.Request,
) error {
	if dialog == nil || dialogRequest == nil || dialogResponse == nil || retry == nil {
		return fmt.Errorf("inbound dialog BYE retry is unavailable")
	}
	cseq, ok := retry.CSeq()
	if !ok || cseq == nil || cseq.MethodName != sip.MethodBYE {
		return fmt.Errorf("inbound dialog BYE retry CSeq is invalid")
	}
	dialog.mu.Lock()
	defer dialog.mu.Unlock()
	if dialog.Request != dialogRequest || dialog.Response != dialogResponse ||
		dialog.Cascade != cascade || dialog.Broadcast != broadcast {
		return fmt.Errorf("inbound dialog changed before BYE retry")
	}
	next, err := nextLocalCSeqLocked(dialog)
	if err != nil {
		return fmt.Errorf("inbound dialog BYE retry: %w", err)
	}
	if cseq.SeqNo != next {
		return fmt.Errorf("inbound dialog BYE retry CSeq is not contiguous")
	}
	dialog.LocalCSeq = next
	return nil
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
	mediaStatusExpireBefore := now.Add(-mediaStatusCascadeDialogGraceTTL)
	mediaStatusExpired := make([]*inboundInviteDialog, 0, 1)
	g.inviteDialogs.Range(func(key, value any) bool {
		d, ok := value.(*inboundInviteDialog)
		if !ok || d == nil {
			g.inviteDialogs.CompareAndDelete(key, value)
			return true
		}
		d.mu.Lock()
		pendingExpired := !d.Established && d.UpdatedAt.Before(expireBefore)
		established := d.Established
		updatedAt := d.UpdatedAt
		d.mu.Unlock()
		terminalExpired := established && d.Cascade != nil && updatedAt.Before(mediaStatusExpireBefore) &&
			g.cascadeSourceMediaStatusFinished(d.Cascade.sourceSnapshot())
		if pendingExpired || terminalExpired {
			if !g.inviteDialogs.CompareAndDelete(key, d) {
				return true
			}
			if d.Cascade != nil {
				g.stopCascadeMediaSession(d.Cascade, false, false)
			}
			if d.Broadcast != nil {
				_ = g.stopBroadcastSession(d.Broadcast, false)
			}
			if terminalExpired {
				mediaStatusExpired = append(mediaStatusExpired, d)
			}
		}
		return true
	})
	// 先摘除所有本地终态和共享源引用；异常上级网络写阻塞不能留下假在线状态。
	for _, dialog := range mediaStatusExpired {
		cleanupCtx := g.mediaPersistenceContext()
		if err := g.requestInboundDialogCleanup(cleanupCtx, dialog); err != nil {
			slog.WarnContext(cleanupCtx, "send expired cascade dialog BYE failed", "call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
		}
	}
}

func (g *GB28181API) close() {
	if g == nil {
		return
	}
	g.beginClose()
	g.lifecycleWG.Wait()
	g.requestWG.Wait()
	g.closeOnce.Do(func() {
		defer func() {
			g.lifecycleMu.Lock()
			cancel := g.shutdownPersistenceCancel
			g.lifecycleMu.Unlock()
			if cancel != nil {
				cancel()
			}
		}()
		if g.annexG != nil {
			g.annexG.close()
		}
		g.catalogResponses.Close()
		g.recordResponses.Close()
		g.pendingDeviceControl.Clear()
		g.pendingDeviceQuery.Clear()
		g.pendingMultiResponse.Clear()
		g.pendingDeviceRequests.Clear()
		g.cascadeMobilePositionQueries.Clear()
		g.pendingAlarmDispatch.Clear()
		g.pendingLocalAlarmDispatch.Clear()
		g.pendingDeviceConfig.Clear()
		g.pendingBroadcast.Clear()
		g.cascadeTaskRoutes.Clear()
		g.recordResponseAliases.Clear()
		g.clearAllRecordResponseExtra()
		g.eventSubscribers.Clear()
		g.closeCascadeDownstreamSubscriptions()
		g.outgoingSubscriptions.Clear()
		g.closeCascadeMediaSessions()
		g.closeCascadeVoiceSessions()
		g.closeVoiceSessions()
		if err := g.retryStoppedVoiceSessions(g.mediaPersistenceContext(), voiceShutdownRetryInterval); err != nil {
			slog.WarnContext(g.mediaPersistenceContext(), "retry GB28181 voice cleanup during shutdown failed", "err", err)
		}
		if g.directDownloads != nil {
			g.directDownloads.Shutdown()
		}
		g.retryPendingDirectTCPDownloadStates()
		g.closeRemainingMediaSessions()
		// 停服清理刚产生的 RTP 下载终态可能遇到一次瞬时存储失败；
		// 在收尾窗口关闭前再刷新一次待持久状态，避免只能等下次进程启动。
		g.cleanupRTPDownloads(time.Now())
	})
}

func (g *GB28181API) beginClose() {
	if g == nil {
		return
	}
	g.closeBeginOnce.Do(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gbShutdownPersistenceTimeout)
		g.lifecycleMu.Lock()
		g.lifecycleClosed = true
		g.shutdownPersistenceCtx = shutdownCtx
		g.shutdownPersistenceCancel = shutdownCancel
		if g.lifecycleDone != nil {
			close(g.lifecycleDone)
		}
		if g.lifecycleCancel != nil {
			g.lifecycleCancel()
		}
		g.lifecycleMu.Unlock()
		g.cancelAllPendingDeviceRequests(ErrServiceStopped)
	})
}

func (g *GB28181API) closeVoiceSessions() {
	if g == nil {
		return
	}
	g.talkSessions.Range(func(key, value any) bool {
		session, ok := value.(*talkSession)
		if !ok || session == nil {
			g.talkSessions.CompareAndDelete(key, value)
			return true
		}
		if err := g.stopTalkSession(session, fmt.Errorf("GB28181 service stopped")); err != nil {
			slog.WarnContext(g.mediaPersistenceContext(), "stop GB28181 Talk during shutdown failed", "device_id", session.DeviceID, "channel_id", session.ChannelID, "err", err)
		}
		return true
	})
	g.broadcastSessions.Range(func(key, value any) bool {
		session, ok := value.(*broadcastSession)
		if !ok || session == nil {
			g.broadcastSessions.CompareAndDelete(key, value)
			return true
		}
		if err := g.stopBroadcastSession(session, true); err != nil {
			slog.WarnContext(g.mediaPersistenceContext(), "stop GB28181 Broadcast during shutdown failed", "device_id", session.DeviceID, "channel_id", session.ChannelID, "err", err)
		}
		return true
	})
}

func (g *GB28181API) closeRemainingMediaSessions() {
	if g == nil {
		return
	}
	cleanupCtx := g.mediaPersistenceContext()
	if g.streams != nil {
		g.streams.Range(func(key string, stream *Streams) bool {
			if stream == nil {
				g.streams.CompareAndDelete(key, nil)
				return true
			}
			if stream.DirectTCP && g.directDownloads != nil {
				g.markMediaStreamStopped(stream, "service_stopped", true)
				if g.directDownloads.Cancel(stream.DirectSessionID) {
					return true
				}
			}
			g.resumeMediaStreamDialogCleanup(stream)
			firstStop := g.markMediaStreamStopped(stream, "service_stopped", false)
			if firstStop && strings.HasPrefix(key, "history:"+historyModeDownload+":") {
				g.finishRTPDownload(stream, rtpDownloadStopped, "service_stopped")
			}
			if _, err := g.cleanupMediaStreamContext(cleanupCtx, key, stream); err != nil {
				slog.WarnContext(cleanupCtx, "cleanup GB28181 media during shutdown failed", "key", key, "stream_id", stream.StreamID, "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "err", err)
			}
			if firstStop && g.core.Store() != nil {
				if err := g.core.EditPlaying(cleanupCtx, stream.DeviceID, stream.ChannelID, false); err != nil {
					slog.WarnContext(cleanupCtx, "persist GB28181 stopped playing state during shutdown failed", "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "err", err)
				}
			}
			return true
		})
		if err := g.retryStoppedMediaSessions(cleanupCtx, voiceShutdownRetryInterval); err != nil {
			slog.WarnContext(cleanupCtx, "retry GB28181 media cleanup during shutdown failed", "err", err)
		}
	}
	g.inviteDialogs.Range(func(key, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog != nil {
			cleanupCtx := g.mediaPersistenceContext()
			if err := g.requestInboundDialogCleanup(cleanupCtx, dialog); err != nil {
				slog.WarnContext(cleanupCtx, "send inbound media dialog BYE during shutdown failed", "call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
			}
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
	if err := g.retryPendingInboundDialogCleanups(cleanupCtx, voiceShutdownRetryInterval); err != nil {
		slog.WarnContext(cleanupCtx, "retry inbound media dialog cleanup during shutdown failed", "err", err)
	}
	if err := g.retryStoppedCascadeMediaSessions(cleanupCtx, voiceShutdownRetryInterval); err != nil {
		slog.WarnContext(cleanupCtx, "retry cascade media cleanup during shutdown failed", "err", err)
	}
}

func (g *GB28181API) sendStreamBYE(stream *Streams) error {
	return g.sendStreamBYEContext(context.Background(), stream)
}

func (g *GB28181API) sendStreamBYEContext(ctx context.Context, stream *Streams) error {
	if g == nil || stream == nil || stream.Resp == nil {
		return nil
	}
	tx, err := g.svr.requestFromResponseCleanupContext(ctx, g.svr.dialogTarget(stream.DeviceID, stream.ChannelID), sip.MethodBYE, stream.Resp)
	if err == nil {
		g.consumeDialogResponseAsync(tx)
	}
	return err
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
		cleanupCtx := g.mediaPersistenceContext()
		if err := g.requestInboundDialogCleanup(cleanupCtx, dialog); err != nil {
			slog.WarnContext(cleanupCtx, "send cascade dialog BYE during shutdown failed", "call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
		}
		g.inviteDialogs.CompareAndDelete(key, dialog)
		g.stopCascadeMediaSession(dialog.Cascade, false, false)
		return true
	})
}
