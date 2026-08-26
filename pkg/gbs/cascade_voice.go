package gbs

import (
	"context"
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
	sourceID    string
	callID      string
	response    *sip.Response
	broadcast   *broadcastSession
	opened      bool
	upstreamEnd bool
	identity    *monitorUserIdentity

	mu       sync.Mutex
	stopOnce sync.Once
}

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
	if g == nil || !cascadeBroadcastTargetAllowed(platform, exposedID) {
		return nil, nil, fmt.Errorf("cascade Broadcast target is not shared")
	}
	return g.resolveCascadeChannel(platform.exposedChannelMap[strings.TrimSpace(exposedID)])
}

func (g *GB28181API) forwardCascadeBroadcast(ctx context.Context, worker *cascadeWorker, request cascadeQueryEnvelope) error {
	if _, _, allowed := cascadeBroadcastProfile(worker.protocolVersion()); !allowed {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	if err := filterUnknowDevices(strings.TrimSpace(request.SourceID)); err != nil {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	channel, _, err := resolveCascadeBroadcastChannel(g, worker.platform, request.TargetID)
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
	if err := g.requireGBFeature(channel.DeviceID, "voice_broadcast", "级联语音广播", func(c GBCapabilities) bool {
		return c.VoiceBroadcast
	}); err != nil {
		return g.sendCascadeBroadcastResult(ctx, worker, request, "ERROR")
	}
	mediaServer, err := g.svr.mediaService.GetMediaServer(ctx, sms.DefaultMediaServerID)
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
	session, err := g.newBroadcastSession(input)
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
	source.mu.Lock()
	source.broadcast = session
	source.mu.Unlock()
	if _, loaded := g.broadcastSessions.LoadOrStore(session.ChannelID, session); loaded {
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

	g.streams.Store(voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID), session.Stream)
	if g.core.Store() != nil {
		_ = g.core.EditPlaying(ctx, session.DeviceID, session.ChannelID, true)
	}
	return g.sendCascadeBroadcastResult(ctx, worker, request, "OK")
}

func (g *GB28181API) sendCascadeBroadcastResult(ctx context.Context, worker *cascadeWorker, request cascadeQueryEnvelope, result string) error {
	return sendCascadeXML(ctx, worker, broadcastResponse{
		CmdType: "Broadcast", SN: request.SN, DeviceID: strings.TrimSpace(request.TargetID), Result: result,
	})
}

func (g *GB28181API) startCascadeVoiceSource(ctx context.Context, worker *cascadeWorker, server *sms.MediaServer, request cascadeQueryEnvelope) (*cascadeVoiceSourceSession, error) {
	if g == nil || g.sms == nil || worker == nil || server == nil {
		return nil, fmt.Errorf("cascade Broadcast media service is unavailable")
	}
	ssrc, err := g.getSSRC(0)
	if err != nil {
		return nil, err
	}
	ssrcNumber, err := strconv.ParseUint(ssrc, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid cascade Broadcast SSRC: %w", err)
	}
	streamID := cascadeSourceStreamID("voice\x00" + worker.platform.name + "\x00" + request.SourceID + "\x00" + request.TargetID + "\x00" + strconv.Itoa(request.SN) + "\x00" + sip.RandString(12))
	source := &cascadeVoiceSourceSession{
		worker: worker, server: server, streamID: streamID, ssrc: ssrc, sourceID: strings.TrimSpace(request.SourceID),
		identity: monitorUserIdentityFromContext(ctx),
	}
	opened, err := g.sms.OpenRTPServer(server, zlm.OpenRTPServerRequest{TCPMode: 0, StreamID: streamID, SSRC: ssrcNumber})
	if err != nil {
		return nil, fmt.Errorf("open cascade Broadcast RTP receiver: %w", err)
	}
	source.opened = true
	if opened == nil || opened.Port <= 0 || opened.Port > 65535 {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, fmt.Errorf("media server returned invalid cascade Broadcast RTP port")
	}
	offer, err := buildCascadeVoiceReceiveSDP(worker.platform.localID, server, worker.protocolVersion(), opened.Port, ssrc)
	if err != nil {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, err
	}
	callID := sip.CallID("cascade-voice-" + sip.RandString(24))
	invite := newCascadeVoiceInvite(worker, request.SourceID, request.TargetID, offer, &callID, 1, nil, ssrc)
	if err := worker.platform.monitorUserIdentity.apply(ctx, invite); err != nil {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, err
	}
	response, err := worker.exchange(ctx, invite)
	if err != nil {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, fmt.Errorf("invite cascade Broadcast source: %w", err)
	}
	if response.StatusCode() == http.StatusUnauthorized && strings.TrimSpace(worker.platform.password) != "" {
		auth, authErr := cascadeDigestAuthorization(response, invite, worker.platform.localID, worker.platform.password)
		if authErr != nil {
			_ = g.stopCascadeVoiceSource(source, false)
			return nil, authErr
		}
		invite = newCascadeVoiceInvite(worker, request.SourceID, request.TargetID, offer, &callID, 2, auth, ssrc)
		if err := worker.platform.monitorUserIdentity.apply(ctx, invite); err != nil {
			_ = g.stopCascadeVoiceSource(source, false)
			return nil, err
		}
		response, err = worker.exchange(ctx, invite)
		if err != nil {
			_ = g.stopCascadeVoiceSource(source, false)
			return nil, fmt.Errorf("authenticate cascade Broadcast source: %w", err)
		}
	}
	if response.StatusCode() != http.StatusOK {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, fmt.Errorf("cascade Broadcast source rejected INVITE: %d %s", response.StatusCode(), response.Reason())
	}
	if err := validateCascadeVoiceAnswer(response.Body(), worker.protocolVersion()); err != nil {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, err
	}
	responseCallID, ok := response.CallID()
	if !ok || normalizeCallID(responseCallID) == "" {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, fmt.Errorf("cascade Broadcast source response missing Call-ID")
	}
	source.callID = normalizeCallID(responseCallID)
	source.response = response
	g.cascadeVoiceDialogs.Store(source.callID, source)
	ack, err := sip.NewRequestFromResponseChecked(sip.MethodACK, response)
	if err != nil {
		_ = g.stopCascadeVoiceSource(source, false)
		return nil, fmt.Errorf("build cascade Broadcast ACK: %w", err)
	}
	prepareCascadeDialogRequest(worker, ack)
	if err := worker.platform.monitorUserIdentity.apply(ctx, ack); err != nil {
		_ = g.stopCascadeVoiceSource(source, true)
		return nil, err
	}
	if err := worker.send(ack); err != nil {
		_ = g.stopCascadeVoiceSource(source, true)
		return nil, fmt.Errorf("ack cascade Broadcast source: %w", err)
	}
	if err := g.waitCascadeVoiceSource(ctx, server, streamID); err != nil {
		_ = g.stopCascadeVoiceSource(source, true)
		return nil, err
	}
	return source, nil
}

func newCascadeVoiceInvite(worker *cascadeWorker, sourceID, receiverID string, body []byte, callID *sip.CallID, cseq uint32, auth *sip.Authorization, ssrc string) *sip.Request {
	request := worker.newRequest(sip.MethodInvite, &sip.ContentTypeSDP, body, callID, cseq, -1, auth)
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

func validateCascadeVoiceAnswer(body []byte, version GBProtocolVersion) error {
	message, err := sdp.Decode(body)
	if err != nil {
		return fmt.Errorf("decode cascade Broadcast source SDP: %w", err)
	}
	for index := range message.Medias {
		media := &message.Medias[index]
		if !strings.EqualFold(media.Description.Type, "audio") {
			continue
		}
		if !strings.EqualFold(media.Description.Protocol, "RTP/AVP") || media.Description.Port <= 0 || media.Description.Port > 65535 {
			return fmt.Errorf("cascade Broadcast source returned invalid audio transport")
		}
		if media.Flag("recvonly") || media.Flag("inactive") {
			return fmt.Errorf("cascade Broadcast source SDP must send media")
		}
		if _, _, _, err := parseBroadcastPayload(media, version); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("cascade Broadcast source response does not contain audio media")
}

func (g *GB28181API) waitCascadeVoiceSource(ctx context.Context, server *sms.MediaServer, streamID string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		items, err := g.sms.GetMediaInfo(server, cascadeSourceApp, streamID)
		if err == nil && hasReadyG711Audio(items) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.serviceDone():
			return ErrServiceStopped
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
	source.stopOnce.Do(func() {
		source.mu.Lock()
		response := source.response
		ended := source.upstreamEnd
		opened := source.opened
		source.opened = false
		source.mu.Unlock()
		if source.callID != "" {
			g.cascadeVoiceDialogs.CompareAndDelete(source.callID, source)
		}
		if sendBYE && !ended && response != nil && source.worker != nil {
			bye, err := sip.NewRequestFromResponseChecked(sip.MethodBYE, response)
			if err != nil {
				result = err
			} else {
				prepareCascadeDialogRequest(source.worker, bye)
				identityCtx := withMonitorUserIdentity(context.Background(), source.identity)
				if err := source.worker.platform.monitorUserIdentity.apply(identityCtx, bye); err != nil {
					result = err
					return
				}
				ctx, cancel := context.WithTimeout(identityCtx, defaultCascadeRequestTimeout)
				resp, err := source.worker.exchange(ctx, bye)
				cancel()
				if err != nil {
					result = err
				} else if resp == nil || resp.StatusCode() != http.StatusOK {
					result = fmt.Errorf("cascade Broadcast source BYE failed")
				}
			}
		}
		if opened && g.sms != nil && source.server != nil {
			_, err := g.sms.CloseRTPServer(source.server, zlm.CloseRTPServerRequest{StreamID: source.streamID})
			if result == nil {
				result = err
			}
		}
	})
	return result
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
	source.mu.Lock()
	source.upstreamEnd = true
	session := source.broadcast
	source.mu.Unlock()
	ctx.String(http.StatusOK, "OK")
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
	state := source.worker.snapshot()
	return state.Registered && source.worker.remoteAddressMatches(ctx.Source) && outboundDialogTagsMatch(source.response, ctx.Request)
}

func (g *GB28181API) terminateCascadeVoiceSource(streamID string) {
	g.cascadeVoiceDialogs.Range(func(_, value any) bool {
		source, _ := value.(*cascadeVoiceSourceSession)
		if source == nil || source.streamID != strings.TrimSpace(streamID) {
			return true
		}
		source.mu.Lock()
		session := source.broadcast
		source.mu.Unlock()
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
		source.mu.Lock()
		session := source.broadcast
		source.mu.Unlock()
		if session != nil {
			_ = g.stopBroadcastSession(session, true)
		} else {
			_ = g.stopCascadeVoiceSource(source, true)
		}
		return true
	})
}
