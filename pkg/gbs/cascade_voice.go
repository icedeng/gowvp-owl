package gbs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	sdp "github.com/panjjo/gosdp"
)

type cascadeVoiceSourceSession struct {
	worker      *cascadeWorker
	server      *sms.MediaServer
	streamID    string
	ssrc        string
	ssrcRelease func()
	sourceID    string
	callID      string
	response    *sip.Response
	broadcast   *broadcastSession
	opened      bool
	upstreamEnd bool
	identity    *monitorUserIdentity
	done        chan struct{}

	mu         sync.Mutex
	stopping   bool
	stopMu     sync.Mutex
	doneOnce   sync.Once
	dialogDone bool
}

var errCascadeVoiceSourceStopped = errors.New("cascade Broadcast source stopped")

func cascadeBroadcastProfile(version GBProtocolVersion) (payload int, mapping string, allowed bool) {
	switch version {
	case GBVersion11:
		return broadcastPSPayload, "PS/90000", true
	case GBVersion20, GBVersion30:
		return broadcastPCMAPayload, "PCMA/8000", true
	default:
		return 0, "", false
	}
}

func cascadeBroadcastTargetAllowed(platform cascadePlatform, exposedID string) bool {
	return platform.exposedChannelMap[strings.TrimSpace(exposedID)] != ""
}

func resolveCascadeBroadcastChannel(g *GB28181API, platform cascadePlatform, exposedID string) (*ipc.Channel, *ipc.Device, error) {
	return resolveCascadeBroadcastChannelContext(context.Background(), g, platform, exposedID)
}

func resolveCascadeBroadcastChannelContext(ctx context.Context, g *GB28181API, platform cascadePlatform, exposedID string) (*ipc.Channel, *ipc.Device, error) {
	if g == nil || !cascadeBroadcastTargetAllowed(platform, exposedID) {
		return nil, nil, fmt.Errorf("cascade Broadcast target is not shared")
	}
	return g.resolveCascadeChannelContext(ctx, platform.exposedChannelMap[strings.TrimSpace(exposedID)], "", platform)
}

func (g *GB28181API) forwardCascadeBroadcast(ctx context.Context, worker *cascadeWorker, request cascadeQueryEnvelope) error {
	if _, _, allowed := cascadeBroadcastProfile(worker.protocolVersion()); !allowed {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	if err := filterUnknowDevices(strings.TrimSpace(request.SourceID)); err != nil {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	channel, _, err := resolveCascadeBroadcastChannelContext(ctx, g, worker.platform, request.TargetID)
	if err != nil {
		slog.Warn("resolve cascade Broadcast target failed", "upstream", worker.platform.name, "target", request.TargetID, "err", err)
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	if g.svr == nil || g.svr.memoryStorer == nil || g.svr.mediaService == nil || g.sms == nil {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	runtimeChannel, ok := g.svr.memoryStorer.GetChannel(channel.DeviceID, channel.ChannelID)
	if !ok || runtimeChannel == nil || runtimeChannel.device == nil || !runtimeChannel.device.IsOnlineNow() {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	unlock, err := runtimeChannel.device.lockMediaContext(ctx, runtimeChannel.ChannelID)
	if err != nil {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	defer unlock()
	if !runtimeChannel.device.IsOnlineNow() {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	channel, err = g.core.GetChannel(ctx, channel.ID)
	if err != nil {
		slog.Warn("reload cascade Broadcast target after media lock failed", "upstream", worker.platform.name, "target", request.TargetID, "err", err)
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	if err := g.requireGBFeature(channel.DeviceID, "voice_broadcast", "级联语音广播", func(c GBCapabilities) bool {
		return c.VoiceBroadcast
	}); err != nil {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	mediaServer, err := g.svr.mediaService.GetMediaServer(ctx, cascadeMediaServerID(channel))
	if err != nil {
		slog.Warn("resolve cascade Broadcast media server failed", "upstream", worker.platform.name, "err", err)
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	source, err := g.startCascadeVoiceSource(ctx, worker, mediaServer, request)
	if err != nil {
		slog.Warn("start cascade Broadcast upstream source failed", "upstream", worker.platform.name, "source", request.SourceID, "err", err)
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}

	input := &VoiceInput{
		Channel: channel, SMS: mediaServer, Mode: voiceModeBroadcast, Timeout: 8 * time.Second,
		SourceID: worker.platform.localID, SourceVHost: cascadeSourceVHost,
		SourceApp: cascadeSourceApp, SourceStream: source.streamID,
	}
	session, err := g.newBroadcastSessionContext(ctx, input)
	if err != nil {
		_ = g.stopCascadeVoiceSource(source, true)
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	version, ok := ParseGBProtocolVersion(runtimeChannel.GBVersion())
	if !ok {
		version = GBVersion10
	}
	session.Version = version
	session.Cascade = source
	if !g.attachCascadeVoiceBroadcast(source, session) {
		_ = g.stopCascadeVoiceSource(source, true)
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}

	fail := func(cause error) error {
		slog.Warn("cascade Broadcast downstream failed", "upstream", worker.platform.name, "target", request.TargetID, "err", cause)
		_ = g.stopBroadcastSession(session, true)
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	notify := g.startBroadcastNotification
	if g.cascadeBroadcastNotify != nil {
		notify = g.cascadeBroadcastNotify
	}
	if err := notify(ctx, runtimeChannel, input); err != nil {
		return fail(err)
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case err := <-session.ready:
		if err != nil {
			return fail(err)
		}
	case <-ctx.Done():
		return fail(ctx.Err())
	case <-g.serviceDone():
		return fail(ErrServiceStopped)
	case <-timer.C:
		return fail(fmt.Errorf("wait cascade Broadcast receiver INVITE timeout"))
	}

	key := voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID)
	g.streams.Store(key, session.Stream)
	published, err := g.commitChannelStreamStart(ctx, key, session.Stream)
	if err != nil {
		return fail(fmt.Errorf("persist cascade Broadcast playing state: %w", err))
	}
	if !published {
		return fail(fmt.Errorf("cascade Broadcast ended before start commit"))
	}
	return g.sendCascadeBroadcastResult(ctx, worker, request, "OK")
}

func (g *GB28181API) sendCascadeBroadcastResult(ctx context.Context, worker *cascadeWorker, request cascadeQueryEnvelope, result string) error {
	return sendCascadeXML(ctx, worker, broadcastResponse{
		CmdType: "Broadcast", SN: request.SN, DeviceID: strings.TrimSpace(request.TargetID), Result: result,
	})
}

func (g *GB28181API) startCascadeVoiceSource(ctx context.Context, worker *cascadeWorker, server *sms.MediaServer, request cascadeQueryEnvelope) (_ *cascadeVoiceSourceSession, resultErr error) {
	if g == nil || g.sms == nil || worker == nil || server == nil {
		return nil, fmt.Errorf("cascade Broadcast media service is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	operationCtx, operationCancel := context.WithCancel(ctx)
	stopWorkerCancel := func() bool { return false }
	if operationCtx := worker.operationContext(); operationCtx != nil {
		stopWorkerCancel = context.AfterFunc(operationCtx, operationCancel)
	}
	defer func() {
		stopWorkerCancel()
		operationCancel()
	}()
	ctx = operationCtx
	ssrc, releaseSSRC, err := g.reserveSSRC(0)
	if err != nil {
		return nil, err
	}
	ssrcNumber, err := strconv.ParseUint(ssrc, 10, 64)
	if err != nil {
		releaseSSRC()
		return nil, fmt.Errorf("invalid cascade Broadcast SSRC: %w", err)
	}
	streamID := cascadeSourceStreamID("voice\x00" + worker.platform.name + "\x00" + request.SourceID + "\x00" + request.TargetID + "\x00" + strconv.Itoa(request.SN) + "\x00" + sip.RandString(12))
	source := &cascadeVoiceSourceSession{
		worker: worker, server: server, streamID: streamID, ssrc: ssrc, ssrcRelease: releaseSSRC, sourceID: strings.TrimSpace(request.SourceID),
		identity: monitorUserIdentityFromContext(ctx), done: make(chan struct{}),
	}
	defer func() {
		if resultErr == nil {
			return
		}
		source.mu.Lock()
		sendBYE := source.response != nil
		source.mu.Unlock()
		resultErr = errors.Join(resultErr, g.stopCascadeVoiceSource(source, sendBYE))
	}()
	opened, err := openRTPServerContext(ctx, g.sms, server, zlm.OpenRTPServerRequest{TCPMode: 0, StreamID: streamID, SSRC: ssrcNumber})
	if err != nil {
		return nil, fmt.Errorf("open cascade Broadcast RTP receiver: %w", err)
	}
	source.opened = true
	if opened == nil || opened.Port <= 0 || opened.Port > 65535 {
		return nil, fmt.Errorf("media server returned invalid cascade Broadcast RTP port")
	}
	offer, err := buildCascadeVoiceReceiveSDP(worker.platform.localID, server, worker.protocolVersion(), opened.Port, ssrc)
	if err != nil {
		return nil, err
	}
	callID := sip.CallID("cascade-voice-" + sip.RandString(24))
	invite := newCascadeVoiceInvite(worker, request.SourceID, request.TargetID, offer, &callID, 1, nil, ssrc)
	if err := worker.platform.monitorUserIdentity.apply(ctx, invite); err != nil {
		return nil, err
	}
	response, err := worker.exchange(ctx, invite)
	if err != nil {
		return nil, fmt.Errorf("invite cascade Broadcast source: %w", err)
	}
	if response == nil {
		return nil, fmt.Errorf("invite cascade Broadcast source returned no SIP response")
	}
	if (response.StatusCode() == http.StatusUnauthorized || response.StatusCode() == http.StatusProxyAuthRequired) && worker.platform.password != "" {
		authHeader, auth, authErr := cascadeRequestDigestAuthorization(response, invite, worker.platform.localID, worker.platform.password)
		if authErr != nil {
			return nil, fmt.Errorf("cascade Broadcast source Digest challenge: %w", authErr)
		}
		invite = newCascadeVoiceInviteWithDigest(worker, request.SourceID, request.TargetID, offer, &callID, 2, authHeader, auth, ssrc)
		if err := worker.platform.monitorUserIdentity.apply(ctx, invite); err != nil {
			return nil, err
		}
		response, err = worker.exchange(ctx, invite)
		if err != nil {
			return nil, fmt.Errorf("authenticate cascade Broadcast source: %w", err)
		}
		if response == nil {
			return nil, fmt.Errorf("authenticate cascade Broadcast source returned no SIP response")
		}
	}
	if response.StatusCode() < http.StatusOK || response.StatusCode() >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("cascade Broadcast source rejected INVITE: %d %s", response.StatusCode(), response.Reason())
	}
	source.response = response
	ack, err := sip.NewRequestFromResponseChecked(sip.MethodACK, response)
	if err != nil {
		worker.discardInviteTransaction(response)
		return nil, fmt.Errorf("build cascade Broadcast ACK: %w", err)
	}
	prepareCascadeDialogRequest(worker, ack)
	if err := worker.platform.monitorUserIdentity.apply(ctx, ack); err != nil {
		worker.discardInviteTransaction(response)
		return nil, err
	}
	if err := worker.send(ack); err != nil {
		return nil, fmt.Errorf("ack cascade Broadcast source: %w", err)
	}
	if err := validateSIPContentType(response, string(sip.ContentTypeSDP)); err != nil {
		return nil, fmt.Errorf("cascade Broadcast source INVITE response %w", err)
	}
	if strings.TrimSpace(string(response.Body())) == "" {
		return nil, fmt.Errorf("cascade Broadcast source INVITE response SDP body is empty")
	}
	if err := validateCascadeVoiceAnswer(response.Body(), worker.protocolVersion(), ssrc); err != nil {
		return nil, err
	}
	responseCallID, ok := response.CallID()
	if !ok || normalizeCallID(responseCallID) == "" {
		return nil, fmt.Errorf("cascade Broadcast source response missing Call-ID")
	}
	source.callID = normalizeCallID(responseCallID)
	g.cascadeVoiceDialogs.Store(source.callID, source)
	if !g.cascadeWorkerAvailable(worker) {
		return nil, ErrServiceStopped
	}
	if err := g.waitCascadeVoiceSource(ctx, server, streamID, source.done); err != nil {
		return nil, err
	}
	return source, nil
}

func newCascadeVoiceInvite(worker *cascadeWorker, sourceID, receiverID string, body []byte, callID *sip.CallID, cseq uint32, auth *sip.Authorization, ssrc string) *sip.Request {
	return newCascadeVoiceInviteWithDigest(worker, sourceID, receiverID, body, callID, cseq, "Authorization", auth, ssrc)
}

func newCascadeVoiceInviteWithDigest(worker *cascadeWorker, sourceID, receiverID string, body []byte, callID *sip.CallID, cseq uint32, authHeader string, auth *sip.Authorization, ssrc string) *sip.Request {
	request := worker.newRequest(sip.MethodInvite, &sip.ContentTypeSDP, body, callID, cseq, -1, nil)
	if auth != nil {
		request.AppendHeader(&sip.GenericHeader{HeaderName: authHeader, Contents: auth.String()})
	}
	target := worker.targetURIForUser(sourceID)
	request.SetRecipient(target)
	request.RemoveHeader("To")
	request.AppendHeader(&sip.ToHeader{Address: target.Clone(), Params: sip.NewParams()})
	request.AppendHeader(&sip.GenericHeader{
		HeaderName: "Subject", Contents: buildGBInviteSubject(strings.TrimSpace(sourceID), ssrc, strings.TrimSpace(receiverID)),
	})
	return request
}

func prepareCascadeDialogRequest(worker *cascadeWorker, request *sip.Request) {
	if worker == nil || request == nil {
		return
	}
	request.RemoveHeader("X-GB-Ver")
	version := sip.XGBVer(worker.protocolVersion())
	request.AppendHeader(&version)
	remote := worker.remoteDestination()
	if request.Destination() == nil || (strings.EqualFold(request.Transport(), "TLS") && cascadeTransportForAddr(remote) == "tls") {
		// TLS 响应源会丢失 TLS/serverName 类型信息；此处恢复已验证的上级目标。
		request.SetDestination(remote)
	}
	if request.GetConnection() == nil && worker.server != nil && worker.server.Server != nil && worker.server.UDPConn() != nil {
		request.SetConnection(worker.server.UDPConn())
		request.SetSource(worker.server.UDPConn().LocalAddr())
	}
}

func buildCascadeVoiceReceiveSDP(localID string, server *sms.MediaServer, version GBProtocolVersion, port int, ssrc string) ([]byte, error) {
	payload, mapping, allowed := cascadeBroadcastProfile(version)
	if !allowed || server == nil || port <= 0 || port > 65535 || !validGBSSRC(ssrc) {
		return nil, fmt.Errorf("invalid cascade Broadcast SDP input")
	}
	if ssrc[0] != '0' {
		return nil, fmt.Errorf("cascade Broadcast Play SDP requires realtime SSRC starting with 0: %s", ssrc)
	}
	ipAddress, err := GetIP(server.GetSDPIP())
	if err != nil {
		return nil, err
	}
	address, err := parseSDPAddress(ipAddress)
	if err != nil {
		return nil, err
	}
	audio := sdp.Media{Description: sdp.MediaDescription{
		Type: "audio", Port: port, Formats: []string{strconv.Itoa(payload)}, Protocol: "RTP/AVP",
	}}
	audio.AddAttribute("recvonly")
	audio.AddAttribute("rtpmap", strconv.Itoa(payload), mapping)
	message := &sdp.Message{
		Origin:     sdp.Origin{Username: localID, NetworkType: "IN", AddressType: address.Type, Address: address.Canonical},
		Name:       historyModePlay,
		Connection: sdp.ConnectionData{NetworkType: "IN", AddressType: address.Type, IP: address.IP},
		Timing:     []sdp.Timing{{}}, Medias: []sdp.Media{audio}, SSRC: ssrc,
	}
	body := message.Append(nil).AppendTo(nil)
	return append(body, "f=v/////a/1/8/1\r\n"...), nil
}

func validateCascadeVoiceAnswer(body []byte, version GBProtocolVersion, expectedSSRC string) error {
	message, err := sdp.Decode(body)
	if err != nil {
		return fmt.Errorf("decode cascade Broadcast source SDP: %w", err)
	}
	ssrcValues := directTCPSDPLineValues(body, "y")
	if len(ssrcValues) > 1 {
		return fmt.Errorf("cascade Broadcast source SDP must not contain multiple y fields")
	}
	ssrc := ""
	if len(ssrcValues) == 1 {
		ssrc = ssrcValues[0]
	}
	if !validGBSSRC(ssrc) {
		return fmt.Errorf("cascade Broadcast source returned invalid SSRC %q", ssrc)
	}
	if expectedSSRC = strings.TrimSpace(expectedSSRC); expectedSSRC != "" && ssrc != expectedSSRC {
		return fmt.Errorf("cascade Broadcast source SSRC %s does not match offer %s", ssrc, expectedSSRC)
	}
	medias := sdpMediasByType(message, "audio")
	if len(medias) == 0 {
		return fmt.Errorf("cascade Broadcast source response does not contain audio media")
	}
	if len(medias) > 1 {
		return fmt.Errorf("cascade Broadcast source response must contain exactly one audio media description")
	}
	media := medias[0]
	if !strings.EqualFold(media.Description.Protocol, "RTP/AVP") || media.Description.Port <= 0 || media.Description.Port > 65535 {
		return fmt.Errorf("cascade Broadcast source returned invalid audio transport")
	}
	direction, err := effectiveSDPDirection(message, media)
	if err != nil {
		return fmt.Errorf("invalid cascade Broadcast source direction: %w", err)
	}
	if direction == "recvonly" || direction == "inactive" {
		return fmt.Errorf("cascade Broadcast source SDP must send media")
	}
	if _, _, _, err := parseBroadcastPayload(media, version); err != nil {
		return err
	}
	return nil
}

func (g *GB28181API) waitCascadeVoiceSource(ctx context.Context, server *sms.MediaServer, streamID string, stopped <-chan struct{}) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-stopped:
			return errCascadeVoiceSourceStopped
		default:
		}
		items, err := getMediaInfoContext(ctx, g.sms, server, cascadeSourceApp, streamID)
		if err == nil && hasReadyG711Audio(items) {
			select {
			case <-stopped:
				return errCascadeVoiceSourceStopped
			default:
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.serviceDone():
			return ErrServiceStopped
		case <-stopped:
			return errCascadeVoiceSourceStopped
		case <-timer.C:
			return fmt.Errorf("cascade Broadcast source stream timeout: %s", streamID)
		case <-ticker.C:
		}
	}
}

func (g *GB28181API) stopCascadeVoiceSource(source *cascadeVoiceSourceSession, sendBYE bool) (result error) {
	if source == nil {
		return nil
	}
	// 先登记终态所有权，再尝试外部清理。这样 INVITE 建立 Call-ID 之前的失败路径
	// 即使 CloseRTPServer 瞬时失败，也能由运行态清理器继续重试。
	g.pendingCascadeVoiceCleanups.Store(source, source)
	source.stopMu.Lock()
	defer source.stopMu.Unlock()
	cleanupCtx := g.mediaPersistenceContext()
	source.mu.Lock()
	source.stopping = true
	response := source.response
	ended := source.upstreamEnd
	opened := source.opened
	done := source.done
	if !sendBYE || ended || response == nil {
		source.dialogDone = true
	}
	dialogDone := source.dialogDone
	source.mu.Unlock()
	if done != nil {
		source.doneOnce.Do(func() { close(done) })
	}
	if !dialogDone {
		if source.worker == nil {
			result = errors.Join(result, fmt.Errorf("cascade Broadcast source worker is unavailable"))
		} else {
			identityCtx := withMonitorUserIdentity(cleanupCtx, source.identity)
			bye, err := sip.NewRequestFromResponsePreparedChecked(sip.MethodBYE, response, func(bye *sip.Request) error {
				prepareCascadeDialogRequest(source.worker, bye)
				return source.worker.platform.monitorUserIdentity.apply(identityCtx, bye)
			})
			if err == nil {
				ctx, cancel := context.WithTimeout(identityCtx, defaultCascadeRequestTimeout)
				var resp *sip.Response
				resp, err = source.worker.exchangeRequestWithDigest(ctx, bye)
				cancel()
				if err == nil && (resp == nil || resp.StatusCode() != http.StatusOK) {
					err = fmt.Errorf("cascade Broadcast source BYE failed")
				}
			}
			result = errors.Join(result, err)
			if err == nil {
				source.mu.Lock()
				source.dialogDone = true
				source.mu.Unlock()
			}
		}
	}
	if opened {
		if g.sms == nil || source.server == nil {
			result = errors.Join(result, fmt.Errorf("cascade Broadcast RTP receiver service is unavailable"))
		} else {
			_, err := closeRTPServerContext(cleanupCtx, g.sms, source.server, zlm.CloseRTPServerRequest{StreamID: source.streamID})
			result = errors.Join(result, err)
			if err == nil {
				source.mu.Lock()
				source.opened = false
				source.mu.Unlock()
			}
		}
	}
	if result != nil {
		return result
	}
	source.mu.Lock()
	complete := source.dialogDone && !source.opened
	releaseSSRC := source.ssrcRelease
	if complete {
		source.ssrcRelease = nil
	} else {
		releaseSSRC = nil
	}
	source.mu.Unlock()
	if releaseSSRC != nil {
		releaseSSRC()
	}
	if complete && source.callID != "" {
		g.cascadeVoiceDialogs.CompareAndDelete(source.callID, source)
	}
	if complete {
		g.pendingCascadeVoiceCleanups.CompareAndDelete(source, source)
	}
	return nil
}

func (g *GB28181API) attachCascadeVoiceBroadcast(source *cascadeVoiceSourceSession, session *broadcastSession) bool {
	if g == nil || source == nil || session == nil {
		return false
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.stopping || source.upstreamEnd {
		return false
	}
	if _, loaded := g.broadcastSessions.LoadOrStore(session.ChannelID, session); loaded {
		return false
	}
	source.broadcast = session
	return true
}

func (source *cascadeVoiceSourceSession) beginTermination(upstreamEnded bool) *broadcastSession {
	if source == nil {
		return nil
	}
	source.mu.Lock()
	source.stopping = true
	if upstreamEnded {
		source.upstreamEnd = true
	}
	session := source.broadcast
	source.mu.Unlock()
	return session
}

func (g *GB28181API) handleCascadeVoiceBYE(ctx *sip.Context, callID string) bool {
	value, ok := g.cascadeVoiceDialogs.Load(strings.TrimSpace(callID))
	if !ok {
		return false
	}
	source, _ := value.(*cascadeVoiceSourceSession)
	if source == nil || !g.authorizeCascadeVoiceSource(source, ctx) {
		ctx.String(http.StatusForbidden, "cascade voice dialog source mismatch")
		return true
	}
	if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
		slog.Error("respond cascade voice BYE", "err", err, "call_id", callID, "source_id", source.sourceID)
		return true
	}
	session := source.beginTermination(true)
	if session != nil {
		_ = g.stopBroadcastSession(session, true)
	} else {
		_ = g.stopCascadeVoiceSource(source, false)
	}
	return true
}

func (g *GB28181API) authorizeCascadeVoiceSource(source *cascadeVoiceSourceSession, ctx *sip.Context) bool {
	if source == nil || source.worker == nil || ctx == nil {
		return false
	}
	deviceID := strings.TrimSpace(ctx.DeviceID)
	if deviceID != source.sourceID && deviceID != source.worker.platform.serverID {
		return false
	}
	return source.worker.registrationActive(time.Now()) && source.worker.remoteAddressMatches(ctx.Source) && outboundDialogTagsMatch(source.response, ctx.Request)
}

func (g *GB28181API) terminateCascadeVoiceSource(streamID string) {
	g.terminateCascadeVoiceSourceForMediaServer(streamID, "")
}

func (g *GB28181API) terminateCascadeVoiceSourceForMediaServer(streamID, mediaServerID string) {
	g.cascadeVoiceDialogs.Range(func(_, value any) bool {
		source, _ := value.(*cascadeVoiceSourceSession)
		if source == nil || source.streamID != strings.TrimSpace(streamID) || !mediaServerEventMatches(source.server, mediaServerID) {
			return true
		}
		session := source.beginTermination(false)
		if session != nil {
			_ = g.stopBroadcastSession(session, true)
		} else {
			_ = g.stopCascadeVoiceSource(source, true)
		}
		return false
	})
}

func (g *GB28181API) closeCascadeVoiceSessions() {
	g.cascadeVoiceDialogs.Range(func(_, value any) bool {
		source, _ := value.(*cascadeVoiceSourceSession)
		if source == nil {
			return true
		}
		session := source.beginTermination(false)
		if session != nil {
			_ = g.stopBroadcastSession(session, true)
		} else {
			_ = g.stopCascadeVoiceSource(source, true)
		}
		return true
	})
}

func (g *GB28181API) removeCascadeVoiceSessions(worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	g.cascadeVoiceDialogs.Range(func(key, value any) bool {
		source, _ := value.(*cascadeVoiceSourceSession)
		if source == nil || source.worker != worker {
			return true
		}
		if current, ok := g.cascadeVoiceDialogs.Load(key); !ok || current != source {
			return true
		}
		session := source.beginTermination(false)
		if session != nil {
			_ = g.stopBroadcastSession(session, true)
		} else {
			_ = g.stopCascadeVoiceSource(source, true)
		}
		return true
	})
}
