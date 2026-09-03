package gbs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/orm"
	sdp "github.com/panjjo/gosdp"
)

const (
	cascadeSourceVHost = "__defaultVhost__"
	cascadeSourceApp   = "rtp"
)

type cascadeVideoOffer struct {
	Version         GBProtocolVersion
	RemoteIP        net.IP
	Port            int
	Payload         int
	Protocol        string
	SSRC            string
	IsUDP           bool
	Mode            string
	URI             string
	HistorySourceID string
	RecordType      int
	StartAt         time.Time
	EndAt           time.Time
	DownloadSpeed   int
	FileSize        int64
	FileSizeKnown   bool
	PreferredPath   string
	DirectTCP       bool
	SessionKey      string
}

type cascadeMediaServerResolver interface {
	GetMediaServer(context.Context, string) (*sms.MediaServer, error)
}

type cascadeSourceRef struct {
	key                 string
	refs                int
	owned               bool
	ended               bool
	mediaStatusFinished bool
	starting            bool
	stopping            bool
	startDone           chan struct{}
	stopDone            chan struct{}
	channel             *ipc.Channel
	device              *ipc.Device
	server              *sms.MediaServer
	stream              *Streams
	mode                string
	directOffer         directTCPDownloadOffer
	controlMu           sync.Mutex
}

type cascadeMediaSession struct {
	worker               *cascadeWorker
	sourceMu             sync.RWMutex
	source               *cascadeSourceRef
	server               *sms.MediaServer
	ssrc                 string
	ssrcRelease          func()
	vhost                string
	app                  string
	stream               string
	directRelay          *directTCPRelay
	identityCtx          context.Context
	cancel               context.CancelFunc
	mediaStatusMu        sync.Mutex
	mediaStatusForwarded bool
	stopMu               sync.Mutex
	stopRequested        bool
	cancelled            bool
	startBusy            bool
	rtpStopped           bool
	sourceReleased       bool
	upstreamBYERequested bool
	upstreamDetached     bool
	sourceEnded          bool
}

func (p cascadePlatform) allowsMediaDestination(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if remoteIP := addressIP(p.remote); remoteIP != nil && remoteIP.Equal(ip) {
		return true
	}
	for index := range p.mediaAllowedCIDRs {
		if p.mediaAllowedCIDRs[index].Contains(ip) {
			return true
		}
	}
	return false
}

func parseCascadeVideoOffer(body []byte, version GBProtocolVersion, platform cascadePlatform) (*cascadeVideoOffer, error) {
	message, err := sdp.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("decode cascade SDP: %w", err)
	}
	mode := strings.TrimSpace(message.Name)
	switch strings.ToLower(mode) {
	case strings.ToLower(historyModePlay):
		mode = historyModePlay
	case strings.ToLower(historyModePlayback):
		mode = historyModePlayback
	case strings.ToLower(historyModeDownload):
		mode = historyModeDownload
	default:
		return nil, fmt.Errorf("unsupported cascade session: %s", message.Name)
	}
	mediaDescriptions := cascadeSDPLineValues(body, "f=")
	if len(mediaDescriptions) > 1 {
		return nil, fmt.Errorf("cascade SDP must not contain multiple f fields")
	}
	if len(mediaDescriptions) == 1 {
		if mediaDescriptions[0] == "" {
			return nil, fmt.Errorf("cascade SDP f field must not be empty")
		}
		if err := validateGBMediaDescriptionForVersion(mediaDescriptions[0], version); err != nil {
			return nil, fmt.Errorf("invalid cascade media description: %w", err)
		}
	}
	startAt, endAt := time.Time{}, time.Time{}
	uri, historySourceID, recordType := "", "", 3
	if mode != historyModePlay {
		startAt, endAt, err = cascadeTimingFromBody(body)
		if err != nil {
			return nil, err
		}
		uris := cascadeSDPLineValues(body, "u=")
		if len(uris) != 1 || uris[0] == "" {
			return nil, fmt.Errorf("cascade %s SDP requires exactly one URI", mode)
		}
		uri = uris[0]
		historySourceID, recordType, err = parseCascadeHistoryURI(uri)
		if err != nil {
			return nil, err
		}
	}
	medias := sdpMediasByType(message, "video")
	if len(medias) == 0 {
		return nil, fmt.Errorf("cascade INVITE does not contain video media")
	}
	if len(medias) > 1 {
		return nil, fmt.Errorf("cascade INVITE must contain exactly one video media description")
	}
	for _, media := range medias {
		protocol := strings.ToUpper(strings.TrimSpace(media.Description.Protocol))
		isUDP := false
		directTCP := false
		setupValues, setupErr := effectiveSDPAttributeValues(message, media, "setup")
		if setupErr != nil {
			return nil, fmt.Errorf("invalid cascade TCP setup: %w", setupErr)
		}
		connectionValues, connectionErr := effectiveSDPAttributeValues(message, media, "connection")
		if connectionErr != nil {
			return nil, fmt.Errorf("invalid cascade TCP connection: %w", connectionErr)
		}
		switch protocol {
		case "RTP/AVP":
			if len(setupValues) != 0 || len(connectionValues) != 0 {
				return nil, fmt.Errorf("cascade UDP offer must not contain TCP setup/connection attributes")
			}
			isUDP = true
		case "TCP/RTP/AVP":
			if !version.Capabilities().RTPOverTCP {
				return nil, fmt.Errorf("RTP over TCP is not supported by protocol %s", version)
			}
			if len(setupValues) != 1 || !strings.EqualFold(strings.TrimSpace(setupValues[0]), "passive") {
				return nil, fmt.Errorf("cascade TCP receiver must use setup:passive")
			}
			if len(connectionValues) > 1 || len(connectionValues) == 1 && !strings.EqualFold(strings.TrimSpace(connectionValues[0]), "new") {
				return nil, fmt.Errorf("cascade TCP receiver only supports connection:new")
			}
		case "TCP":
			if version != GBVersion11 || mode != historyModeDownload {
				return nil, fmt.Errorf("raw TCP cascade download is only supported by GB/T 28181-2014 Download")
			}
			if len(setupValues) != 0 || len(connectionValues) != 0 {
				return nil, fmt.Errorf("raw TCP cascade offer must not contain RTP TCP setup/connection attributes")
			}
			directTCP = true
		default:
			return nil, fmt.Errorf("unsupported cascade media protocol: %s", media.Description.Protocol)
		}
		if media.Description.Port <= 0 || media.Description.Port > 65535 {
			return nil, fmt.Errorf("invalid cascade RTP port")
		}
		direction, directionErr := effectiveSDPDirection(message, media)
		if directionErr != nil {
			return nil, fmt.Errorf("invalid cascade SDP direction: %w", directionErr)
		}
		if directTCP && direction != "recvonly" {
			return nil, fmt.Errorf("raw TCP cascade receiver must use recvonly")
		}
		if !directTCP && (direction == "sendonly" || direction == "inactive") {
			return nil, fmt.Errorf("cascade SDP receiver must accept media")
		}
		remoteIP := media.Connection.IP
		networkType, addressType := media.Connection.NetworkType, media.Connection.AddressType
		if remoteIP == nil {
			remoteIP = message.Connection.IP
			networkType, addressType = message.Connection.NetworkType, message.Connection.AddressType
		}
		if err := validateSDPConnectionAddress(networkType, addressType, remoteIP); err != nil {
			return nil, fmt.Errorf("invalid cascade RTP address: %w", err)
		}
		if remoteIP == nil || remoteIP.IsUnspecified() || remoteIP.IsMulticast() || !platform.allowsMediaDestination(remoteIP) {
			return nil, fmt.Errorf("cascade RTP destination is not allowed: %v", remoteIP)
		}
		payload := -1
		for _, format := range media.Description.Formats {
			value, parseErr := strconv.Atoi(format)
			if parseErr != nil || value < 0 || value > 127 {
				continue
			}
			mapping, mappingErr := sdpPayloadFormat(media, value)
			if mappingErr != nil {
				return nil, mappingErr
			}
			mapping = strings.ToUpper(mapping)
			if mapping == "PS/90000" || mapping == "" && value == 96 {
				payload = value
				break
			}
		}
		if payload < 0 {
			return nil, fmt.Errorf("cascade SDP does not offer PS/90000")
		}
		ssrcValues := cascadeSDPLineValues(body, "y=")
		if len(ssrcValues) > 1 {
			return nil, fmt.Errorf("cascade SDP must not contain multiple y fields")
		}
		ssrc := ""
		if len(ssrcValues) == 1 {
			ssrc = ssrcValues[0]
		}
		if ssrc != "" && !validGBSSRC(ssrc) {
			return nil, fmt.Errorf("invalid cascade SSRC: %s", ssrc)
		}
		ssrcType, ssrcKind := cascadeSSRCStreamType(mode)
		if ssrc != "" && int(ssrc[0]-'0') != ssrcType {
			return nil, fmt.Errorf("cascade %s stream requires %s SSRC", mode, ssrcKind)
		}
		downloadSpeed := 0
		downloadSpeedValues, downloadSpeedErr := effectiveSDPAttributeValues(message, media, "downloadspeed")
		if downloadSpeedErr != nil {
			return nil, fmt.Errorf("invalid cascade download speed: %w", downloadSpeedErr)
		}
		if len(downloadSpeedValues) == 1 {
			value := strings.TrimSpace(downloadSpeedValues[0])
			downloadSpeed, err = strconv.Atoi(value)
			if err != nil || downloadSpeed <= 0 {
				return nil, fmt.Errorf("invalid cascade download speed: %s", value)
			}
			if mode != historyModeDownload {
				return nil, fmt.Errorf("downloadspeed is only valid for Download")
			}
			if !version.Capabilities().DownloadSpeed {
				return nil, fmt.Errorf("downloadspeed is not supported by protocol %s", version)
			}
		}
		return &cascadeVideoOffer{
			Version:  version,
			RemoteIP: remoteIP, Port: media.Description.Port, Payload: payload, Protocol: protocol,
			SSRC: ssrc, IsUDP: isUDP, Mode: mode, URI: uri, HistorySourceID: historySourceID, RecordType: recordType,
			StartAt: startAt, EndAt: endAt, DownloadSpeed: downloadSpeed, DirectTCP: directTCP,
		}, nil
	}
	return nil, fmt.Errorf("cascade INVITE does not contain a usable video media description")
}

func cascadeSSRCStreamType(mode string) (int, string) {
	if mode == historyModePlay {
		return 0, "realtime"
	}
	return 1, "history"
}

func parseCascadeHistoryURI(uri string) (string, int, error) {
	value := strings.TrimSpace(uri)
	parts := strings.Split(value, ":")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[0]) != parts[0] || len(parts[1]) != 1 {
		return "", 0, fmt.Errorf("cascade history SDP URI must use <source-id>:<record-type>")
	}
	recordType, err := strconv.Atoi(parts[1])
	if err != nil || recordType < 0 || recordType > 3 {
		return "", 0, fmt.Errorf("invalid cascade history record type: %s", parts[1])
	}
	return parts[0], recordType, nil
}

func cascadeSDPLineValue(body []byte, prefix string) string {
	values := cascadeSDPLineValues(body, prefix)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func cascadeSDPLineValues(body []byte, prefix string) []string {
	values := make([]string, 0, 1)
	for line := range strings.SplitSeq(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			values = append(values, strings.TrimSpace(strings.TrimPrefix(line, prefix)))
		}
	}
	return values
}

func cascadeTimingFromBody(body []byte) (time.Time, time.Time, error) {
	value := cascadeSDPLineValue(body, "t=")
	parts := strings.Fields(value)
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, fmt.Errorf("cascade history SDP requires start/end time")
	}
	start, startErr := strconv.ParseInt(parts[0], 10, 64)
	end, endErr := strconv.ParseInt(parts[1], 10, 64)
	if startErr != nil || endErr != nil || start <= 0 || end <= start {
		return time.Time{}, time.Time{}, fmt.Errorf("invalid cascade history time range: %s", value)
	}
	return time.Unix(start, 0), time.Unix(end, 0), nil
}

func (g *GB28181API) sipInviteCascade(ctx *sip.Context, callID string, worker *cascadeWorker) {
	if existing, loaded := g.inviteDialogs.Load(callID); loaded {
		dialog, _ := existing.(*inboundInviteDialog)
		if dialog != nil && dialog.Cascade != nil && dialog.Cascade.worker == worker {
			if !inboundInviteTransactionMatches(dialog, ctx.Request) {
				respondCascadeInviteStatus(ctx, worker, 491, "Call-ID already in use")
				return
			}
			if !g.replayInboundInviteFinalResponse(ctx, callID, dialog) {
				ctx.String(100, "Trying")
			}
			return
		}
		respondCascadeInviteStatus(ctx, worker, 491, "Call-ID already in use")
		return
	}

	recipient := ctx.Request.Recipient()
	if recipient == nil || recipient.User() == nil {
		respondCascadeInviteStatus(ctx, worker, http.StatusBadRequest, "missing cascade channel id")
		return
	}
	exposedID := strings.TrimSpace(recipient.User().String())
	localChannelID := worker.platform.exposedChannelMap[exposedID]
	if localChannelID == "" {
		respondCascadeInviteStatus(ctx, worker, http.StatusNotFound, "cascade channel not shared")
		return
	}
	if err := validateSIPContentType(ctx.Request, string(sip.ContentTypeSDP)); err != nil {
		respondCascadeInviteStatus(ctx, worker, http.StatusUnsupportedMediaType, "Content-Type must be application/sdp")
		return
	}
	preferredPath, err := consumeCascadePreferredPath(ctx.Request, worker)
	if err != nil {
		respondCascadeInviteStatus(ctx, worker, http.StatusNotFound, err.Error())
		return
	}
	offer, err := parseCascadeVideoOffer(ctx.Request.Body(), worker.protocolVersion(), worker.platform)
	if err != nil {
		respondCascadeInviteStatus(ctx, worker, http.StatusNotAcceptable, err.Error())
		return
	}
	var subject *gbInviteSubject
	if worker.protocolVersion().AtLeast(GBVersion11) {
		subject, err = requiredGBInviteSubject(ctx.Request)
	} else {
		subject, err = optionalGBInviteSubject(ctx.Request)
	}
	if err == nil {
		prefix := byte('0')
		if offer.Mode != historyModePlay {
			prefix = '1'
		}
		err = validateGBInviteSubject(subject, exposedID, worker.platform.serverID, prefix)
	}
	if err != nil {
		respondCascadeInviteStatus(ctx, worker, http.StatusBadRequest, err.Error())
		return
	}
	offer.PreferredPath = preferredPath
	offer.SessionKey = callID
	if offer.Mode != historyModePlay && offer.HistorySourceID != exposedID {
		respondCascadeInviteStatus(ctx, worker, http.StatusNotAcceptable, "cascade history SDP URI does not match requested channel")
		return
	}
	var releaseSSRC func()
	if offer.SSRC == "" {
		ssrcType, _ := cascadeSSRCStreamType(offer.Mode)
		offer.SSRC, releaseSSRC, err = g.reserveSSRC(ssrcType)
		if err != nil {
			respondCascadeInviteStatus(ctx, worker, http.StatusInternalServerError, err.Error())
			return
		}
	}

	sessionCtx, cancel := g.newCascadeMediaSessionContext(ctx, worker)
	session := &cascadeMediaSession{
		worker: worker, ssrc: offer.SSRC, ssrcRelease: releaseSSRC, vhost: cascadeSourceVHost, app: cascadeSourceApp,
		identityCtx: sessionCtx, cancel: cancel,
	}
	remoteCSeq, remoteCSeqSet := sipRequestCSeq(ctx.Request, sip.MethodInvite)
	dialog := &inboundInviteDialog{
		CallID: callID, DeviceID: ctx.DeviceID, RemoteTag: sipRequestFromTag(ctx.Request), InitialToTag: sipRequestToTag(ctx.Request), TagsBound: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		LocalCSeq: 1, InitialRemoteCSeq: remoteCSeq, InitialRemoteCSeqSet: remoteCSeqSet,
		RemoteCSeq: remoteCSeq, RemoteCSeqSet: remoteCSeqSet, RemoteMethod: sip.MethodInvite,
		Request: ctx.Request, Subject: subject, Cascade: session, InviteTx: ctx.Tx,
	}
	actual, loaded, replaced := g.storeCascadeInviteDialog(callID, dialog)
	if loaded {
		g.stopCascadeMediaSession(session, false, false)
		previous, ok := actual.(*inboundInviteDialog)
		if !ok || previous == nil || previous.Cascade == nil || previous.Cascade.worker != worker || !inboundInviteTransactionMatches(previous, ctx.Request) {
			respondCascadeInviteStatus(ctx, worker, 491, "Call-ID already in use")
			return
		}
		previous.mu.Lock()
		previousResponse := previous.Response
		previous.UpdatedAt = time.Now()
		previous.mu.Unlock()
		if previousResponse != nil {
			_ = ctx.Tx.Respond(previousResponse)
		} else {
			ctx.String(100, "Trying")
		}
		return
	}
	for _, previous := range replaced {
		g.terminateSupersededCascadeDialog(previous)
	}

	fail := func(status int, cause error) {
		cancelled := sessionCtx.Err() != nil
		dialog.mu.Lock()
		retainTermination := dialog.Cancelled && dialog.TerminationResponse != nil
		dialog.mu.Unlock()
		if !retainTermination {
			g.inviteDialogs.CompareAndDelete(callID, dialog)
		}
		g.stopCascadeMediaSession(session, false, false)
		if cancelled {
			return
		}
		respondCascadeInviteStatusWithRoute(ctx, worker, status, cause.Error(), cascadeDownstreamInviteResponse(cause), offer.PreferredPath)
	}
	if !g.cascadeWorkerAvailable(worker) {
		fail(http.StatusServiceUnavailable, ErrServiceStopped)
		return
	}

	channel, device, err := g.resolveCascadeChannelContext(sessionCtx, localChannelID, offer.PreferredPath, worker.platform)
	if err != nil {
		fail(http.StatusNotFound, err)
		return
	}
	var mediaServer *sms.MediaServer
	if !offer.DirectTCP {
		mediaServer, err = g.svr.mediaService.GetMediaServer(sessionCtx, cascadeMediaServerID(channel))
		if err != nil {
			fail(http.StatusServiceUnavailable, err)
			return
		}
	}
	source, err := g.acquireCascadeSource(sessionCtx, mediaServer, device, channel, offer)
	if err != nil {
		fail(http.StatusBadGateway, err)
		return
	}
	if !offer.DirectTCP {
		mediaServer = source.server
		if mediaServer == nil {
			fail(http.StatusServiceUnavailable, fmt.Errorf("cascade source media server is unavailable"))
			return
		}
	}
	if !g.attachCascadeSource(session, source, mediaServer) {
		return
	}
	if !g.cascadeSourceUsable(source) {
		fail(http.StatusBadGateway, fmt.Errorf("cascade source stream ended during setup"))
		return
	}
	if offer.Mode == historyModeDownload && source.stream.FileSizeKnown {
		offer.FileSize = source.stream.FileSize
		offer.FileSizeKnown = true
	}
	mediaDescription := ""
	if source.stream.Resp != nil {
		mediaDescription = cascadeSDPLineValue(source.stream.Resp.Body(), "f=")
	}
	var answer []byte
	if offer.DirectTCP {
		downstream := source.directOffer
		if downstream.Address == "" || downstream.SSRC == "" {
			fail(http.StatusBadGateway, fmt.Errorf("cascade direct TCP source is incomplete"))
			return
		}
		if session.ssrcRelease != nil {
			session.ssrcRelease()
			session.ssrcRelease = nil
		}
		session.ssrc = downstream.SSRC
		offer.SSRC = downstream.SSRC
		offer.FileSize = downstream.FileSize
		offer.FileSizeKnown = downstream.FileSizeKnown
		registeredIP := net.IP(nil)
		if runtimeChannel, ok := g.svr.memoryStorer.GetChannel(channel.DeviceID, channel.ChannelID); ok {
			registeredIP = addressIP(runtimeChannel.Source())
		}
		relay, advertiseIP, relayPort, relayErr := g.prepareCascadeDirectTCPRelay(
			sessionCtx, g.directTCPPolicySnapshot(), worker.platform, channel.DeviceID, registeredIP, downstream,
		)
		if relayErr != nil {
			fail(http.StatusServiceUnavailable, relayErr)
			return
		}
		if !g.attachCascadeDirectTCPRelay(session, relay) {
			return
		}
		answer, err = buildCascadeDirectTCPSDPAnswer(worker.platform.localID, offer, advertiseIP, relayPort, mediaDescription)
	} else {
		if err := g.waitCascadeSource(sessionCtx, mediaServer, source.stream.StreamID); err != nil {
			fail(http.StatusGatewayTimeout, err)
			return
		}
		started, startErr := g.startCascadeSessionRTP(sessionCtx, session, worker.protocolVersion(), mediaServer, offer, zlm.StartSendRTPRequest{
			Vhost: session.vhost, App: session.app, Stream: session.stream, SSRC: session.ssrc,
			DstURL: offer.RemoteIP.String(), DstPort: offer.Port, IsUDP: offer.IsUDP, Type: 1, PT: offer.Payload,
			TCPRTCP: shouldEnableTCPRTCP(worker.protocolVersion(), !offer.IsUDP),
		})
		if startErr != nil {
			fail(http.StatusBadGateway, fmt.Errorf("start cascade RTP: %w", startErr))
			return
		}
		if started == nil || started.LocalPort <= 0 || started.LocalPort > 65535 {
			fail(http.StatusBadGateway, fmt.Errorf("media server returned invalid cascade RTP port"))
			return
		}
		answer, err = buildCascadeSDPAnswer(worker.platform.localID, mediaServer, offer, started.LocalPort, mediaDescription)
	}
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	response := sip.NewResponseFromRequest("", ctx.Request, http.StatusOK, "OK", answer)
	response.AppendHeader(&sip.ContentTypeSDP)
	if contact := worker.contactAddress(); contact != nil {
		response.AppendHeader(&sip.ContactHeader{DisplayName: contact.DisplayName, Address: contact.URI.Clone(), Params: contact.Params.Clone()})
	}
	version := sip.XGBVer(worker.protocolVersion())
	response.AppendHeader(&version)
	if err := appendCascadeRoutePath(response, worker, source.stream.Resp, offer.PreferredPath); err != nil {
		fail(http.StatusBadGateway, err)
		return
	}
	dialog.mu.Lock()
	if dialog.Cancelled || sessionCtx.Err() != nil {
		dialog.mu.Unlock()
		g.stopCascadeMediaSession(session, false, false)
		return
	}
	dialog.Response = response
	dialog.LocalTag = sipResponseToTag(response)
	dialog.UpdatedAt = time.Now()
	dialog.mu.Unlock()
	if err := ctx.Tx.Respond(response); err != nil {
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		g.stopCascadeMediaSession(session, false, false)
	}
}

func cascadeMediaServerID(channel *ipc.Channel) string {
	if channel != nil {
		if id := strings.TrimSpace(channel.Config.MediaServerID); id != "" {
			return id
		}
	}
	return sms.DefaultMediaServerID
}

func (g *GB28181API) resolveCascadeChannelMediaServer(ctx context.Context, channelID string) (*sms.MediaServer, error) {
	if g == nil || g.core.Store() == nil || g.svr == nil || g.svr.mediaService == nil {
		return nil, fmt.Errorf("cascade media server resolver is unavailable")
	}
	channel, err := g.core.GetChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	return g.svr.mediaService.GetMediaServer(ctx, cascadeMediaServerID(channel))
}

// attachCascadeSource 原子转移 acquireCascadeSource 返回的引用所有权。
// 如果终态已经先提交，引用仍属于当前调用并在这里立即释放。
func (g *GB28181API) attachCascadeSource(session *cascadeMediaSession, source *cascadeSourceRef, mediaServer *sms.MediaServer) bool {
	if g == nil || source == nil {
		return false
	}
	if session == nil || source.stream == nil {
		g.releaseCascadeSource(source, false)
		return false
	}
	session.stopMu.Lock()
	stopped := session.stopRequested
	if !stopped {
		session.sourceMu.Lock()
		session.source = source
		session.sourceMu.Unlock()
		session.server = mediaServer
		session.stream = source.stream.StreamID
	}
	session.stopMu.Unlock()
	if stopped {
		g.releaseCascadeSource(source, false)
		return false
	}
	return true
}

// attachCascadeDirectTCPRelay 原子转移已监听端口的所有权。
// CANCEL、停服或替换可能在监听建立后先提交终态，此时立即关闭中继。
func (g *GB28181API) attachCascadeDirectTCPRelay(session *cascadeMediaSession, relay *directTCPRelay) bool {
	if session == nil || relay == nil {
		if relay != nil {
			relay.Close()
		}
		return false
	}
	session.stopMu.Lock()
	if session.stopRequested {
		session.stopMu.Unlock()
		relay.Close()
		return false
	}
	session.directRelay = relay
	session.stopMu.Unlock()
	return true
}

// startCascadeSessionRTP 将正在启动的 RTP 发送端纳入 session 的终态所有权。
// 对不支持 context 的媒体适配器，CANCEL/停服可能先返回而 StartSendRTP 随后成功；
// startBusy 会阻止清理器提前释放 session，启动返回后再统一停止或交给后台重试。
func (g *GB28181API) startCascadeSessionRTP(
	ctx context.Context,
	session *cascadeMediaSession,
	version GBProtocolVersion,
	mediaServer *sms.MediaServer,
	offer *cascadeVideoOffer,
	request zlm.StartSendRTPRequest,
) (*zlm.StartSendRTPResponse, error) {
	if session == nil {
		return nil, fmt.Errorf("cascade media session is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	session.stopMu.Lock()
	if session.stopRequested {
		session.stopMu.Unlock()
		return nil, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		session.stopMu.Unlock()
		return nil, err
	}
	session.startBusy = true
	session.stopMu.Unlock()

	response, err := g.startCascadeRTP(ctx, version, mediaServer, offer, request)
	session.stopMu.Lock()
	session.startBusy = false
	stopped := session.stopRequested
	session.stopMu.Unlock()
	if stopped {
		g.stopCascadeMediaSession(session, false, false)
		if err == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				err = ctxErr
			} else {
				err = context.Canceled
			}
		}
	}
	return response, err
}

func (g *GB28181API) startCascadeRTP(
	ctx context.Context,
	version GBProtocolVersion,
	mediaServer *sms.MediaServer,
	offer *cascadeVideoOffer,
	request zlm.StartSendRTPRequest,
) (*zlm.StartSendRTPResponse, error) {
	if g == nil || g.sms == nil || offer == nil {
		return nil, fmt.Errorf("cascade RTP sender is unavailable")
	}
	attempts, delay := 1, time.Duration(0)
	if !offer.IsUDP {
		attempts, delay = activeRTPConnectPolicy(version, mediaServer)
	}
	contextSender, contextAware := g.sms.(rtpMediaSenderContext)
	return retryMediaOperationWithDelay(ctx, attempts, delay, func(operationCtx context.Context) (*zlm.StartSendRTPResponse, error) {
		var response *zlm.StartSendRTPResponse
		var err error
		if contextAware {
			response, err = contextSender.StartSendRTPContext(operationCtx, mediaServer, request)
		} else {
			response, err = g.sms.StartSendRTP(mediaServer, request)
		}
		if err != nil {
			return nil, err
		}
		if response == nil || response.LocalPort <= 0 || response.LocalPort > 65535 {
			return nil, fmt.Errorf("media server returned invalid cascade RTP port")
		}
		return response, nil
	})
}

func (g *GB28181API) newCascadeMediaSessionContext(ctx *sip.Context, worker *cascadeWorker) (context.Context, context.CancelFunc) {
	sessionCtx, sessionCancel := context.WithCancel(monitorUserIdentityContext(ctx))
	stopLifecycleCancel := context.AfterFunc(g.serviceContext(), sessionCancel)
	stopWorkerCancel := func() bool { return false }
	if worker != nil {
		stopWorkerCancel = context.AfterFunc(worker.operationContext(), sessionCancel)
	}
	var once sync.Once
	return sessionCtx, func() {
		once.Do(func() {
			stopLifecycleCancel()
			stopWorkerCancel()
			sessionCancel()
		})
	}
}

func (g *GB28181API) storeCascadeInviteDialog(callID string, dialog *inboundInviteDialog) (any, bool, []*inboundInviteDialog) {
	if g == nil {
		return nil, false, nil
	}
	g.cascadeInviteMu.Lock()
	defer g.cascadeInviteMu.Unlock()
	actual, loaded := g.inviteDialogs.LoadOrStore(callID, dialog)
	if loaded || dialog == nil || dialog.Subject == nil {
		return actual, loaded, nil
	}
	replaced := make([]*inboundInviteDialog, 0, 1)
	g.inviteDialogs.Range(func(key, value any) bool {
		previous, _ := value.(*inboundInviteDialog)
		if previous == nil || previous == dialog || previous.Cascade == nil || !sameGBInviteSubjectSender(previous.Subject, dialog.Subject) {
			return true
		}
		previous.mu.Lock()
		if previous.superseded {
			previous.mu.Unlock()
			return true
		}
		previous.superseded = true
		if previous.Response == nil {
			// 阻止仍在处理的旧 INVITE 提交 2xx；统一终止入口随后返回 487。
			previous.Cancelled = true
			if previous.TerminationResponse == nil && previous.Request != nil {
				previous.TerminationResponse = sip.NewResponseFromRequest("", previous.Request, 487, "Request Terminated", nil)
			}
		}
		previous.UpdatedAt = time.Now()
		previous.mu.Unlock()
		// 暂时保留旧 Call-ID：pending 状态需要先停止本地处理再回 487，
		// 已回 2xx 的状态则必须等待合法 ACK 后才能发送 BYE。
		if actual, exists := g.inviteDialogs.Load(key); exists && actual == previous {
			replaced = append(replaced, previous)
		}
		return true
	})
	return dialog, false, replaced
}

// terminateSupersededCascadeDialog 终止被相同 Subject 媒体源标识替换的旧会话。
// SIP UAS 在 2xx ACK 到达前不能主动发送 BYE，因此该状态会保留旧对话索引等待 ACK。
func (g *GB28181API) terminateSupersededCascadeDialog(dialog *inboundInviteDialog) {
	if g == nil || dialog == nil || dialog.Cascade == nil {
		return
	}
	dialog.mu.Lock()
	superseded := dialog.superseded
	pending := dialog.Response == nil
	established := dialog.Established
	inviteTx := dialog.InviteTx
	request := dialog.Request
	termination := dialog.TerminationResponse
	if superseded && pending && request != nil && termination == nil {
		termination = sip.NewResponseFromRequest("", request, 487, "Request Terminated", nil)
		dialog.TerminationResponse = termination
	}
	dialog.mu.Unlock()
	// 先保存 pending 终态，再提交本地媒体终态。这样取消 context 唤醒旧 handler 时，
	// 它能识别必须保留的 487，而不会在首次写失败前删除对话。
	g.stopCascadeMediaSession(dialog.Cascade, false, false)
	if !superseded || !pending && !established {
		return
	}
	if pending {
		if request == nil {
			g.inviteDialogs.CompareAndDelete(dialog.CallID, dialog)
			return
		}
		if err := g.respondInboundInviteTermination(dialog.CallID, dialog, inviteTx, termination); err != nil {
			slog.Warn("send superseded cascade INVITE termination failed",
				"call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
		}
		return
	}
	if !g.inviteDialogs.CompareAndDelete(dialog.CallID, dialog) {
		return
	}
	cleanupCtx := g.mediaPersistenceContext()
	sendBYE := func(ctx context.Context) {
		if err := g.requestInboundDialogCleanup(ctx, dialog); err != nil {
			slog.WarnContext(ctx, "send superseded cascade dialog BYE failed",
				"call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
		}
	}
	// 旧对话 BYE 是已经提交本地终态后的最佳努力收尾，不能阻塞新 INVITE 建链。
	// 生命周期已关闭时无法登记异步任务，退化为共享收尾 context 下的同步发送。
	if !g.startLifecycleTask(cleanupCtx, sendBYE) {
		sendBYE(cleanupCtx)
	}
}

func sameGBInviteSubjectSender(left, right *gbInviteSubject) bool {
	return left != nil && right != nil &&
		strings.TrimSpace(left.SenderID) == strings.TrimSpace(right.SenderID) &&
		strings.TrimSpace(left.SenderSequence) == strings.TrimSpace(right.SenderSequence)
}

func (g *GB28181API) resolveCascadeChannel(localChannelID, preferredPath string, platform cascadePlatform) (*ipc.Channel, *ipc.Device, error) {
	return g.resolveCascadeChannelContext(context.Background(), localChannelID, preferredPath, platform)
}

func (g *GB28181API) resolveCascadeChannelContext(ctx context.Context, localChannelID, preferredPath string, platform cascadePlatform) (*ipc.Channel, *ipc.Device, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if g == nil || g.core.Store() == nil {
		return nil, nil, fmt.Errorf("channel store is unavailable")
	}
	queryOptions := []orm.QueryOption{orm.Where("channel_id = ?", localChannelID)}
	if strings.TrimSpace(preferredPath) != "" {
		path, err := parseCascadePlatformPath(preferredPath)
		if err != nil {
			return nil, nil, err
		}
		if len(path) > 0 {
			nextHop := path[0]
			if localNextHop := platform.exposedChannelMap[nextHop]; localNextHop != "" {
				nextHop = localNextHop
			}
			queryOptions = append(queryOptions, orm.Where("device_id = ?", nextHop))
		}
	}
	var channel ipc.Channel
	if err := g.core.Store().Channel().Get(ctx, &channel, queryOptions...); err != nil {
		if strings.TrimSpace(preferredPath) != "" {
			return nil, nil, fmt.Errorf("shared channel is unavailable through preferred path %s: %w", preferredPath, err)
		}
		return nil, nil, fmt.Errorf("shared channel not found: %w", err)
	}
	var device ipc.Device
	if err := g.core.Store().Device().Get(ctx, &device, orm.Where("device_id = ?", channel.DeviceID)); err != nil {
		return nil, nil, fmt.Errorf("shared channel device not found: %w", err)
	}
	if !channel.IsOnline || !device.IsOnline {
		return nil, nil, ErrDeviceOffline
	}
	if g.svr != nil && g.svr.memoryStorer != nil {
		runtime, ok := g.svr.memoryStorer.Load(channel.DeviceID)
		if !ok || runtime == nil || !runtime.IsOnlineNow() {
			return nil, nil, ErrDeviceOffline
		}
	}
	return &channel, &device, nil
}

func (g *GB28181API) acquireCascadeSource(ctx context.Context, server *sms.MediaServer, device *ipc.Device, channel *ipc.Channel, offer *cascadeVideoOffer) (*cascadeSourceRef, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := g.validateCascadeMediaSourceTarget(device, channel, offer); err != nil {
		return nil, err
	}
	mode := offer.Mode
	key := cascadeSourceKey(channel, offer)
	var source *cascadeSourceRef
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		select {
		case <-g.serviceDone():
			return nil, ErrServiceStopped
		default:
		}
		g.cascadeMediaMu.Lock()
		if current := g.cascadeSources[key]; current != nil {
			if current.starting {
				done := current.startDone
				g.cascadeMediaMu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-g.serviceDone():
					return nil, ErrServiceStopped
				case <-done:
					continue
				}
			}
			if current.ended || current.stopping {
				done := current.stopDone
				g.cascadeMediaMu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-g.serviceDone():
					return nil, ErrServiceStopped
				case <-done:
					continue
				}
			}
			current.refs++
			g.cascadeMediaMu.Unlock()
			return current, nil
		}
		source = &cascadeSourceRef{
			key: key, refs: 1, starting: true, startDone: make(chan struct{}), stopDone: make(chan struct{}),
		}
		g.cascadeSources[key] = source
		g.cascadeMediaMu.Unlock()
		break
	}
	stream, exists := g.streams.Load(key)
	owned := false
	if !exists || stream == nil || g.mediaStreamStopping(stream) || stream.Resp == nil {
		if mode == historyModePlay {
			play := g.PlayContext
			if g.cascadePlay != nil {
				play = g.cascadePlay
			}
			playInput := &PlayInput{
				Channel: channel, SMS: server, StreamMode: device.StreamMode,
				ResolveMediaServer: func(ctx context.Context) (*sms.MediaServer, error) {
					return g.resolveCascadeChannelMediaServer(ctx, channel.ID)
				},
				preferredPath: offer.PreferredPath,
			}
			if offer.PreferredPath != "" {
				playInput.sessionKey = key
				playInput.streamID = cascadeSourceStreamID(key)
			}
			if err := play(ctx, playInput); err != nil {
				g.abortCascadeSourceStart(source)
				return nil, wrapCascadeDownstreamInviteError(err, playInput.routeResponse)
			}
		} else if offer.DirectTCP {
			recordType := offer.RecordType
			historyInput := &HistoryInput{
				Channel: channel, StartAt: offer.StartAt, EndAt: offer.EndAt, Mode: mode,
				Transport: historyTransportDirectTCP, DownloadSpeed: offer.DownloadSpeed, RecordType: &recordType,
				sessionKey: key, streamID: cascadeSourceStreamID(key), preferredPath: offer.PreferredPath,
			}
			directOffer, directErr := g.startCascadeDirectTCPSource(ctx, historyInput, key)
			if directErr != nil {
				g.abortCascadeSourceStart(source)
				return nil, wrapCascadeDownstreamInviteError(directErr, historyInput.routeResponse)
			}
			source.directOffer = directOffer
		} else {
			startHistory := g.StartHistory
			if g.cascadeHistory != nil {
				startHistory = g.cascadeHistory
			}
			recordType := offer.RecordType
			historyInput := &HistoryInput{
				Channel: channel, SMS: server, StreamMode: device.StreamMode,
				ResolveMediaServer: func(ctx context.Context) (*sms.MediaServer, error) {
					return g.resolveCascadeChannelMediaServer(ctx, channel.ID)
				},
				StartAt: offer.StartAt, EndAt: offer.EndAt, Mode: mode,
				Transport: historyTransportRTP, DownloadSpeed: offer.DownloadSpeed, RecordType: &recordType,
				sessionKey: key, streamID: cascadeSourceStreamID(key), preferredPath: offer.PreferredPath,
			}
			if err := startHistory(ctx, historyInput); err != nil {
				g.abortCascadeSourceStart(source)
				return nil, wrapCascadeDownstreamInviteError(err, historyInput.routeResponse)
			}
		}
		stream, exists = g.streams.Load(key)
		if !exists || stream == nil {
			g.abortCascadeSourceStart(source)
			return nil, fmt.Errorf("cascade source stream was not created")
		}
		owned = true
	}
	g.cascadeMediaMu.Lock()
	if g.cascadeSources[key] != source {
		g.cascadeMediaMu.Unlock()
		return nil, fmt.Errorf("cascade source start was superseded")
	}
	source.owned = owned
	source.channel = channel
	source.device = device
	source.server = server
	if stream != nil && stream.mediaServer != nil {
		source.server = stream.mediaServer
	}
	source.stream = stream
	source.mode = mode
	source.starting = false
	if source.startDone != nil {
		close(source.startDone)
		source.startDone = nil
	}
	g.cascadeMediaMu.Unlock()
	select {
	case <-ctx.Done():
		g.releaseCascadeSource(source, false)
		return nil, ctx.Err()
	case <-g.serviceDone():
		g.releaseCascadeSource(source, false)
		return nil, ErrServiceStopped
	default:
		return source, nil
	}
}

// validateCascadeMediaSourceTarget 在自定义级联媒体钩子和源引用登记前，
// 复用直连播放/历史会话的设备在线、传输和下载倍速能力门禁。
func (g *GB28181API) validateCascadeMediaSourceTarget(device *ipc.Device, channel *ipc.Channel, offer *cascadeVideoOffer) error {
	if device == nil || channel == nil || offer == nil {
		return fmt.Errorf("invalid cascade media source")
	}
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		// 保留未装配运行态存储的协议单元测试和嵌入调用兼容。
		return nil
	}
	runtime, ok := g.svr.memoryStorer.Load(channel.DeviceID)
	if !ok || runtime == nil || !runtime.IsOnlineNow() {
		return ErrDeviceOffline
	}
	if offer.Mode == historyModePlayback || offer.Mode == historyModeDownload {
		if offer.StartAt.IsZero() || offer.EndAt.IsZero() || !offer.EndAt.After(offer.StartAt) {
			return fmt.Errorf("invalid history range")
		}
		if _, err := historyURI(channel.ChannelID, &offer.RecordType); err != nil {
			return err
		}
		advertised := ""
		if channel.Ext.GBCatalog != nil {
			advertised = channel.Ext.GBCatalog.DownloadSpeed
		}
		if err := g.requireHistoryDownloadSpeed(channel.DeviceID, offer.DownloadSpeed, advertised); err != nil {
			return err
		}
	}
	if offer.DirectTCP {
		return nil
	}
	return g.requireMediaTransport(channel.DeviceID, device.StreamMode, "级联媒体")
}

func (g *GB28181API) abortCascadeSourceStart(source *cascadeSourceRef) {
	if g == nil || source == nil {
		return
	}
	g.cascadeMediaMu.Lock()
	if g.cascadeSources[source.key] == source {
		delete(g.cascadeSources, source.key)
	}
	if source.starting {
		source.starting = false
		if source.startDone != nil {
			close(source.startDone)
			source.startDone = nil
		}
	}
	g.cascadeMediaMu.Unlock()
}

func (g *GB28181API) waitCascadeSource(ctx context.Context, server *sms.MediaServer, streamID string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(8 * time.Second)
	defer timeout.Stop()
	for {
		items, err := getMediaInfoContext(ctx, g.sms, server, cascadeSourceApp, streamID)
		if err == nil && len(items) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.serviceDone():
			return ErrServiceStopped
		case <-timeout.C:
			return fmt.Errorf("cascade source stream timeout: %s", streamID)
		case <-ticker.C:
		}
	}
}

func (g *GB28181API) releaseCascadeSource(source *cascadeSourceRef, sourceEnded bool) {
	if source == nil {
		return
	}
	g.cascadeMediaMu.Lock()
	current := g.cascadeSources[source.key]
	if current != source {
		g.cascadeMediaMu.Unlock()
		return
	}
	if source.refs <= 0 || source.stopping {
		g.cascadeMediaMu.Unlock()
		return
	}
	source.refs--
	if source.refs > 0 {
		g.cascadeMediaMu.Unlock()
		return
	}
	source.stopping = true
	sourceEnded = sourceEnded || source.ended
	mediaStatusFinished := source.mediaStatusFinished
	g.cascadeMediaMu.Unlock()
	cleanupCtx := g.mediaPersistenceContext()
	deviceID, channelID := "", ""
	if source.channel != nil {
		deviceID = source.channel.DeviceID
		channelID = source.channel.ChannelID
	}
	if source.owned && (!sourceEnded || mediaStatusFinished) {
		if mediaStatusFinished {
			// MediaStatus 先关闭 RTP 并保留终态所有权；最后一个上级 BYE 到达后再允许结束下级对话。
			if source.stream != nil {
				source.stream.cleanupMu.Lock()
				if source.stream.mediaServer == nil {
					source.stream.mediaServer = source.server
				}
				source.stream.cleanupMu.Unlock()
			}
			g.resumeMediaStreamDialogCleanup(source.stream)
			g.markMediaStreamStopped(source.stream, "media_status", false)
			if _, err := g.cleanupMediaStreamContext(cleanupCtx, source.key, source.stream); err != nil {
				slog.WarnContext(cleanupCtx, "cleanup cascade source after MediaStatus failed", "key", source.key, "device_id", deviceID, "channel_id", channelID, "err", err)
			}
		} else if source.mode == historyModePlay {
			var err error
			if g.cascadeStop != nil {
				err = g.cascadeStop(cleanupCtx, &StopPlayInput{Channel: source.channel, sessionKey: source.key})
			} else {
				err = g.StopPlay(cleanupCtx, &StopPlayInput{Channel: source.channel, sessionKey: source.key})
			}
			if err != nil {
				slog.ErrorContext(cleanupCtx, "stop cascade realtime source", "key", source.key, "device_id", deviceID, "channel_id", channelID, "err", err)
			}
		} else {
			var err error
			if g.cascadeStopHistory != nil {
				err = g.cascadeStopHistory(cleanupCtx, &StopHistoryInput{Channel: source.channel, Mode: source.mode, sessionKey: source.key})
			} else {
				err = g.StopHistory(cleanupCtx, &StopHistoryInput{Channel: source.channel, Mode: source.mode, sessionKey: source.key})
			}
			if err != nil {
				slog.ErrorContext(cleanupCtx, "stop cascade history source", "key", source.key, "device_id", deviceID, "channel_id", channelID, "mode", source.mode, "err", err)
			}
		}
	}
	g.cascadeMediaMu.Lock()
	if g.cascadeSources[source.key] == source {
		delete(g.cascadeSources, source.key)
	}
	if source.stopDone != nil {
		close(source.stopDone)
		source.stopDone = nil
	}
	g.cascadeMediaMu.Unlock()
}

func cascadeSourceKey(channel *ipc.Channel, offer *cascadeVideoOffer) string {
	if channel == nil || offer == nil {
		return ""
	}
	if offer.Mode == historyModePlay {
		base := "play:" + channel.DeviceID + ":" + channel.ChannelID
		if strings.TrimSpace(offer.PreferredPath) == "" {
			return base
		}
		sum := sha256.Sum256([]byte(offer.PreferredPath))
		return base + ":cascade:" + hex.EncodeToString(sum[:8])
	}
	if offer.DirectTCP {
		identity := fmt.Sprintf("direct_tcp\x00%s\x00%s\x00%s", channel.DeviceID, channel.ChannelID, strings.TrimSpace(offer.SessionKey))
		sum := sha256.Sum256([]byte(identity))
		return historyKey(offer.Mode, channel.DeviceID, channel.ChannelID) + ":cascade-direct:" + hex.EncodeToString(sum[:8])
	}
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d\x00%d\x00%s\x00%s",
		offer.Mode, channel.DeviceID, channel.ChannelID, offer.StartAt.Unix(), offer.EndAt.Unix(), offer.DownloadSpeed,
		strings.TrimSpace(offer.URI), strings.TrimSpace(offer.PreferredPath))
	sum := sha256.Sum256([]byte(identity))
	return historyKey(offer.Mode, channel.DeviceID, channel.ChannelID) + ":cascade:" + hex.EncodeToString(sum[:8])
}

func cascadeSourceStreamID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "cascade-" + hex.EncodeToString(sum[:8])
}

func (g *GB28181API) stopCascadeMediaSession(session *cascadeMediaSession, sendUpstreamBYE, sourceEnded bool) {
	if g == nil || session == nil {
		return
	}
	session.stopMu.Lock()
	defer session.stopMu.Unlock()
	session.stopRequested = true
	session.upstreamBYERequested = session.upstreamBYERequested || sendUpstreamBYE
	session.sourceEnded = session.sourceEnded || sourceEnded
	g.pendingCascadeMediaCleanups.Store(session, session)
	if !session.cancelled {
		if session.cancel != nil {
			session.cancel()
		}
		session.cancelled = true
	}
	if session.startBusy {
		// StartSendRTP 尚未给出确定结果，不能提前停止一个可能尚未创建的发送端，
		// 也不能释放来源。启动返回后会再次进入本状态机。
		return
	}
	cleanupCtx := g.mediaPersistenceContext()
	if session.upstreamBYERequested && !session.upstreamDetached {
		g.inviteDialogs.Range(func(key, value any) bool {
			dialog, _ := value.(*inboundInviteDialog)
			if dialog == nil || dialog.Cascade != session {
				return true
			}
			if !g.inviteDialogs.CompareAndDelete(key, dialog) {
				return false
			}
			if err := g.requestInboundDialogCleanup(cleanupCtx, dialog); err != nil {
				slog.WarnContext(cleanupCtx, "send cascade upstream BYE failed",
					"call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
			}
			return false
		})
		session.upstreamDetached = true
	}
	if !session.rtpStopped {
		if session.directRelay != nil {
			session.directRelay.Close()
			session.rtpStopped = true
		} else if session.server == nil || session.stream == "" || session.ssrc == "" {
			session.rtpStopped = true
		} else if g.sms == nil {
			slog.WarnContext(cleanupCtx, "stop cascade RTP sender failed",
				"device_id", session.sourceDeviceID(), "channel_id", session.sourceChannelID(),
				"stream", session.stream, "ssrc", session.ssrc, "err", "RTP media service is unavailable")
			return
		} else if _, err := stopSendRTPContext(cleanupCtx, g.sms, session.server, zlm.StopSendRTPRequest{
			Vhost: session.vhost, App: session.app, Stream: session.stream, SSRC: session.ssrc,
		}); err != nil {
			slog.WarnContext(cleanupCtx, "stop cascade RTP sender failed",
				"device_id", session.sourceDeviceID(), "channel_id", session.sourceChannelID(),
				"stream", session.stream, "ssrc", session.ssrc, "err", err)
			return
		} else {
			session.rtpStopped = true
		}
	}
	if session.rtpStopped && session.ssrcRelease != nil {
		session.ssrcRelease()
		session.ssrcRelease = nil
	}
	if !session.sourceReleased {
		g.releaseCascadeSource(session.sourceSnapshot(), session.sourceEnded)
		session.sourceReleased = true
	}
	if session.rtpStopped && session.sourceReleased && (!session.upstreamBYERequested || session.upstreamDetached) {
		g.pendingCascadeMediaCleanups.CompareAndDelete(session, session)
	}
}

func (g *GB28181API) removeCascadeMediaSessions(worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	dialogs := make([]*inboundInviteDialog, 0, 1)
	g.inviteDialogs.Range(func(key, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog == nil || dialog.Cascade == nil || dialog.Cascade.worker != worker {
			return true
		}
		if g.inviteDialogs.CompareAndDelete(key, dialog) {
			dialogs = append(dialogs, dialog)
		}
		return true
	})
	// 先摘除本地对话并释放媒体；上级 BYE 写失败不能留下孤儿 RTP 会话。
	for _, dialog := range dialogs {
		g.stopCascadeMediaSession(dialog.Cascade, false, false)
		cleanupCtx := g.mediaPersistenceContext()
		if err := g.requestInboundDialogCleanup(cleanupCtx, dialog); err != nil {
			slog.WarnContext(cleanupCtx, "send cascade dialog BYE after upstream removal failed",
				"call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
		}
	}
}

func (s *cascadeMediaSession) sourceDeviceID() string {
	source := s.sourceSnapshot()
	if source == nil || source.channel == nil {
		return ""
	}
	return source.channel.DeviceID
}

func (s *cascadeMediaSession) sourceChannelID() string {
	source := s.sourceSnapshot()
	if source == nil || source.channel == nil {
		return ""
	}
	return source.channel.ChannelID
}

func (s *cascadeMediaSession) sourceSnapshot() *cascadeSourceRef {
	if s == nil {
		return nil
	}
	s.sourceMu.RLock()
	defer s.sourceMu.RUnlock()
	return s.source
}

func (s *cascadeMediaSession) directRelaySnapshot() *directTCPRelay {
	if s == nil {
		return nil
	}
	s.stopMu.Lock()
	defer s.stopMu.Unlock()
	return s.directRelay
}

func (g *GB28181API) terminateCascadeSessionsForStream(stream *Streams) {
	if stream == nil {
		return
	}
	g.markCascadeSourcesEnded(stream)
	dialogs := make([]*inboundInviteDialog, 0, 1)
	g.inviteDialogs.Range(func(key, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog == nil || dialog.Cascade == nil || !cascadeSourceReferencesStream(dialog.Cascade.sourceSnapshot(), stream) {
			return true
		}
		if g.inviteDialogs.CompareAndDelete(key, dialog) {
			g.stopCascadeMediaSession(dialog.Cascade, false, true)
			dialogs = append(dialogs, dialog)
		}
		return true
	})
	// 本地会话必须先进入终态；上级网络写阻塞或失败不能让已丢失媒体的对话继续显示为活动。
	for _, dialog := range dialogs {
		cleanupCtx := g.mediaPersistenceContext()
		if err := g.requestInboundDialogCleanup(cleanupCtx, dialog); err != nil {
			slog.WarnContext(cleanupCtx, "send cascade BYE after source termination failed", "call_id", dialog.CallID, "device_id", dialog.DeviceID, "err", err)
		}
	}
}

func (g *GB28181API) markCascadeSourcesEnded(stream *Streams) {
	if g == nil || stream == nil {
		return
	}
	g.cascadeMediaMu.Lock()
	for _, source := range g.cascadeSources {
		if cascadeSourceReferencesStream(source, stream) {
			source.ended = true
		}
	}
	g.cascadeMediaMu.Unlock()
}

func (g *GB28181API) markCascadeSourcesMediaStatusFinished(stream *Streams) {
	if g == nil || stream == nil {
		return
	}
	g.cascadeMediaMu.Lock()
	for _, source := range g.cascadeSources {
		// 必须按对象身份匹配，不能把同键并发创建的新媒体源误标为已结束。
		if source != nil && source.stream == stream {
			source.ended = true
			source.mediaStatusFinished = true
		}
	}
	g.cascadeMediaMu.Unlock()
}

func (g *GB28181API) cascadeSourceMediaStatusFinished(source *cascadeSourceRef) bool {
	if g == nil || source == nil {
		return false
	}
	g.cascadeMediaMu.Lock()
	finished := g.cascadeSources[source.key] == source && source.mediaStatusFinished
	g.cascadeMediaMu.Unlock()
	return finished
}

func (g *GB28181API) cascadeSourceUsable(source *cascadeSourceRef) bool {
	if g == nil || source == nil {
		return false
	}
	g.cascadeMediaMu.Lock()
	usable := g.cascadeSources[source.key] == source && !source.starting && !source.ended && !source.stopping
	g.cascadeMediaMu.Unlock()
	return usable
}

func cascadeSourceReferencesStream(source *cascadeSourceRef, stream *Streams) bool {
	if source == nil || source.stream == nil || stream == nil {
		return false
	}
	// Streams 可在相同设备、通道和 StreamID 下重建；旧代次的迟到清理只能作用于
	// 原对象，不能按可复用的业务标识终止当前级联源。
	return source.stream == stream
}

func buildCascadeSDPAnswer(localID string, server *sms.MediaServer, offer *cascadeVideoOffer, localPort int, mediaDescription string) ([]byte, error) {
	if offer == nil || server == nil || localPort <= 0 || localPort > 65535 {
		return nil, fmt.Errorf("invalid cascade SDP answer input")
	}
	ipAddress, err := GetIP(server.GetSDPIP())
	if err != nil {
		return nil, err
	}
	address, err := parseSDPAddress(ipAddress)
	if err != nil {
		return nil, err
	}
	video := sdp.Media{Description: sdp.MediaDescription{
		Type: "video", Port: localPort, Formats: []string{strconv.Itoa(offer.Payload)}, Protocol: offer.Protocol,
	}}
	video.AddAttribute("sendonly")
	video.AddAttribute("rtpmap", strconv.Itoa(offer.Payload), "PS/90000")
	if offer.DownloadSpeed > 0 {
		video.AddAttribute("downloadspeed", strconv.Itoa(offer.DownloadSpeed))
	}
	if offer.FileSizeKnown {
		if offer.FileSize < 0 {
			return nil, fmt.Errorf("invalid cascade download file size")
		}
		video.AddAttribute("filesize", strconv.FormatInt(offer.FileSize, 10))
	}
	if !offer.IsUDP {
		video.AddAttribute("setup", "active")
		video.AddAttribute("connection", "new")
	}
	mode := strings.TrimSpace(offer.Mode)
	if mode == "" {
		mode = historyModePlay
	}
	message := &sdp.Message{
		Origin:     sdp.Origin{Username: localID, NetworkType: "IN", AddressType: address.Type, Address: address.Canonical},
		Name:       mode,
		Connection: sdp.ConnectionData{NetworkType: "IN", AddressType: address.Type, IP: address.IP},
		Timing:     []sdp.Timing{{}}, Medias: []sdp.Media{video}, SSRC: offer.SSRC,
	}
	if mode != historyModePlay {
		if strings.TrimSpace(offer.URI) == "" || offer.StartAt.IsZero() || offer.EndAt.IsZero() || !offer.EndAt.After(offer.StartAt) {
			return nil, fmt.Errorf("invalid cascade %s SDP answer input", mode)
		}
		message.URI = strings.TrimSpace(offer.URI)
		message.Timing = []sdp.Timing{{Start: offer.StartAt, End: offer.EndAt}}
	}
	if mediaDescription == "" {
		// 下级未返回 f 字段时保持结构完整但不虚构具体音频编码。
		mediaDescription = "v/////a///"
	}
	if err := validateGBMediaDescriptionForVersion(mediaDescription, offer.Version); err != nil {
		return nil, fmt.Errorf("invalid cascade source media description: %w", err)
	}
	body := message.Append(nil).AppendTo(nil)
	body = append(body, "f="...)
	body = append(body, mediaDescription...)
	return append(body, '\r', '\n'), nil
}

// buildCascadeDirectTCPSDPAnswer 构造 2014 附录 O 媒体发送方应答。
// 应答只暴露 Owl 的中继监听地址，不泄漏下级设备的 TCP 服务地址。
func buildCascadeDirectTCPSDPAnswer(localID string, offer *cascadeVideoOffer, advertiseIP string, localPort int, mediaDescription string) ([]byte, error) {
	if offer == nil || !offer.DirectTCP || offer.Version != GBVersion11 || offer.Mode != historyModeDownload ||
		localPort <= 0 || localPort > 65535 || !validGBSSRC(offer.SSRC) {
		return nil, fmt.Errorf("invalid cascade direct TCP SDP answer input")
	}
	address, err := parseSDPAddress(advertiseIP)
	if err != nil || address.IP == nil || address.IP.IsUnspecified() || address.IP.IsMulticast() {
		return nil, fmt.Errorf("invalid cascade direct TCP SDP advertise address")
	}
	video := sdp.Media{Description: sdp.MediaDescription{
		Type: "video", Port: localPort, Formats: []string{strconv.Itoa(offer.Payload)}, Protocol: "tcp",
	}}
	video.AddAttribute("sendonly")
	video.AddAttribute("rtpmap", strconv.Itoa(offer.Payload), "PS/90000")
	if offer.DownloadSpeed > 0 {
		video.AddAttribute("downloadspeed", strconv.Itoa(offer.DownloadSpeed))
	}
	if offer.FileSizeKnown {
		if offer.FileSize < 0 {
			return nil, fmt.Errorf("invalid cascade direct TCP download file size")
		}
		video.AddAttribute("filesize", strconv.FormatInt(offer.FileSize, 10))
	}
	if strings.TrimSpace(offer.URI) == "" || offer.StartAt.IsZero() || offer.EndAt.IsZero() || !offer.EndAt.After(offer.StartAt) {
		return nil, fmt.Errorf("invalid cascade direct TCP Download SDP answer input")
	}
	message := &sdp.Message{
		Origin:     sdp.Origin{Username: localID, NetworkType: "IN", AddressType: address.Type, Address: address.Canonical},
		Name:       historyModeDownload,
		URI:        strings.TrimSpace(offer.URI),
		Connection: sdp.ConnectionData{NetworkType: "IN", AddressType: address.Type, IP: address.IP},
		Timing:     []sdp.Timing{{Start: offer.StartAt, End: offer.EndAt}},
		Medias:     []sdp.Media{video},
		SSRC:       offer.SSRC,
	}
	if mediaDescription == "" {
		mediaDescription = "v/////a///"
	}
	if err := validateGBMediaDescriptionForVersion(mediaDescription, offer.Version); err != nil {
		return nil, fmt.Errorf("invalid cascade direct TCP source media description: %w", err)
	}
	body := message.Append(nil).AppendTo(nil)
	body = append(body, "f="...)
	body = append(body, mediaDescription...)
	return append(body, '\r', '\n'), nil
}
