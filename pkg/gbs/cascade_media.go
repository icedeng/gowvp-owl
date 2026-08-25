package gbs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	RemoteIP      net.IP
	Port          int
	Payload       int
	Protocol      string
	SSRC          string
	IsUDP         bool
	Mode          string
	URI           string
	StartAt       time.Time
	EndAt         time.Time
	DownloadSpeed int
	FileSize      int64
	FileSizeKnown bool
	PreferredPath string
}

type cascadeMediaServerResolver interface {
	GetMediaServer(context.Context, string) (*sms.MediaServer, error)
}

type cascadeSourceRef struct {
	key       string
	refs      int
	owned     bool
	stopping  bool
	stopDone  chan struct{}
	channel   *ipc.Channel
	device    *ipc.Device
	server    *sms.MediaServer
	stream    *Streams
	mode      string
	controlMu sync.Mutex
}

type cascadeMediaSession struct {
	worker   *cascadeWorker
	source   *cascadeSourceRef
	server   *sms.MediaServer
	ssrc     string
	vhost    string
	app      string
	stream   string
	cancel   context.CancelFunc
	stopOnce sync.Once
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
	startAt, endAt := time.Time{}, time.Time{}
	uri := ""
	if mode != historyModePlay {
		startAt, endAt, err = cascadeTimingFromBody(body)
		if err != nil {
			return nil, err
		}
		uri = cascadeSDPLineValue(body, "u=")
		if uri == "" {
			return nil, fmt.Errorf("cascade %s SDP URI is required", mode)
		}
	}
	for index := range message.Medias {
		media := &message.Medias[index]
		if !strings.EqualFold(media.Description.Type, "video") {
			continue
		}
		protocol := strings.ToUpper(strings.TrimSpace(media.Description.Protocol))
		isUDP := false
		switch protocol {
		case "RTP/AVP":
			isUDP = true
		case "TCP/RTP/AVP":
			if !version.Capabilities().RTPOverTCP {
				return nil, fmt.Errorf("RTP over TCP is not supported by protocol %s", version)
			}
			setup := strings.ToLower(strings.TrimSpace(media.Attribute("setup")))
			if setup != "passive" {
				return nil, fmt.Errorf("cascade TCP receiver must use setup:passive")
			}
		default:
			return nil, fmt.Errorf("unsupported cascade media protocol: %s", media.Description.Protocol)
		}
		if media.Description.Port <= 0 || media.Description.Port > 65535 {
			return nil, fmt.Errorf("invalid cascade RTP port")
		}
		if media.Flag("sendonly") || media.Flag("inactive") {
			return nil, fmt.Errorf("cascade SDP receiver must accept media")
		}
		remoteIP := media.Connection.IP
		if remoteIP == nil {
			remoteIP = message.Connection.IP
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
			mapping := strings.ToUpper(strings.TrimSpace(media.PayloadFormat(format)))
			if mapping == "PS/90000" || mapping == "" && value == 96 {
				payload = value
				break
			}
		}
		if payload < 0 {
			return nil, fmt.Errorf("cascade SDP does not offer PS/90000")
		}
		ssrc := strings.TrimSpace(message.SSRC)
		if ssrc == "" {
			ssrc = cascadeSSRCFromBody(body)
		}
		if ssrc != "" && !validGBSSRC(ssrc) {
			return nil, fmt.Errorf("invalid cascade SSRC: %s", ssrc)
		}
		downloadSpeed := 0
		if value := strings.TrimSpace(media.Attribute("downloadspeed")); value != "" {
			downloadSpeed, err = strconv.Atoi(value)
			if err != nil || downloadSpeed < 0 {
				return nil, fmt.Errorf("invalid cascade download speed: %s", value)
			}
		}
		if downloadSpeed > 0 && mode != historyModeDownload {
			return nil, fmt.Errorf("downloadspeed is only valid for Download")
		}
		if downloadSpeed > 0 && !version.Capabilities().DownloadSpeed {
			return nil, fmt.Errorf("downloadspeed is not supported by protocol %s", version)
		}
		return &cascadeVideoOffer{
			RemoteIP: remoteIP, Port: media.Description.Port, Payload: payload, Protocol: protocol,
			SSRC: ssrc, IsUDP: isUDP, Mode: mode, URI: uri, StartAt: startAt, EndAt: endAt, DownloadSpeed: downloadSpeed,
		}, nil
	}
	return nil, fmt.Errorf("cascade INVITE does not contain video media")
}

func cascadeSSRCFromBody(body []byte) string {
	for line := range strings.SplitSeq(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "y=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "y="))
		}
	}
	return ""
}

func cascadeSDPLineValue(body []byte, prefix string) string {
	for line := range strings.SplitSeq(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
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
			dialog.mu.Lock()
			response := dialog.Response
			dialog.UpdatedAt = time.Now()
			dialog.mu.Unlock()
			if response != nil {
				_ = ctx.Tx.Respond(response)
			} else {
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
	offer.PreferredPath = preferredPath
	if offer.Mode != historyModePlay && !cascadeHistoryURIMatches(offer.URI, exposedID) {
		respondCascadeInviteStatus(ctx, worker, http.StatusNotAcceptable, "cascade history SDP URI does not match requested channel")
		return
	}
	if offer.SSRC == "" {
		offer.SSRC = g.getSSRC(0)
	}

	sessionCtx, cancel := context.WithCancel(context.Background())
	session := &cascadeMediaSession{worker: worker, ssrc: offer.SSRC, vhost: cascadeSourceVHost, app: cascadeSourceApp, cancel: cancel}
	dialog := &inboundInviteDialog{
		CallID: callID, DeviceID: ctx.DeviceID, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		LocalCSeq: 1, Request: ctx.Request, Cascade: session, InviteTx: ctx.Tx,
	}
	actual, loaded := g.inviteDialogs.LoadOrStore(callID, dialog)
	if loaded {
		cancel()
		if previous, ok := actual.(*inboundInviteDialog); ok && previous != nil && previous.Response != nil {
			_ = ctx.Tx.Respond(previous.Response)
		} else {
			ctx.String(100, "Trying")
		}
		return
	}

	fail := func(status int, cause error) {
		cancelled := sessionCtx.Err() != nil
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		g.stopCascadeMediaSession(session, false, false)
		if cancelled {
			return
		}
		respondCascadeInviteStatus(ctx, worker, status, cause.Error())
	}

	channel, device, err := g.resolveCascadeChannel(localChannelID)
	if err != nil {
		fail(http.StatusNotFound, err)
		return
	}
	mediaServer, err := g.svr.mediaService.GetMediaServer(sessionCtx, sms.DefaultMediaServerID)
	if err != nil {
		fail(http.StatusServiceUnavailable, err)
		return
	}
	source, err := g.acquireCascadeSource(sessionCtx, mediaServer, device, channel, offer)
	if err != nil {
		fail(http.StatusBadGateway, err)
		return
	}
	session.source = source
	session.server = mediaServer
	session.stream = source.stream.StreamID
	if offer.Mode == historyModeDownload && source.stream.FileSizeKnown {
		offer.FileSize = source.stream.FileSize
		offer.FileSizeKnown = true
	}
	if err := g.waitCascadeSource(sessionCtx, mediaServer, source.stream.StreamID); err != nil {
		fail(http.StatusGatewayTimeout, err)
		return
	}
	started, err := g.sms.StartSendRTP(mediaServer, zlm.StartSendRTPRequest{
		Vhost: session.vhost, App: session.app, Stream: session.stream, SSRC: session.ssrc,
		DstURL: offer.RemoteIP.String(), DstPort: offer.Port, IsUDP: offer.IsUDP, Type: 1, PT: offer.Payload,
	})
	if err != nil {
		fail(http.StatusBadGateway, fmt.Errorf("start cascade RTP: %w", err))
		return
	}
	if started == nil || started.LocalPort <= 0 || started.LocalPort > 65535 {
		fail(http.StatusBadGateway, fmt.Errorf("media server returned invalid cascade RTP port"))
		return
	}
	answer, err := buildCascadeSDPAnswer(worker.platform.localID, mediaServer, offer, started.LocalPort)
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
	dialog.UpdatedAt = time.Now()
	dialog.mu.Unlock()
	if err := ctx.Tx.Respond(response); err != nil {
		g.inviteDialogs.CompareAndDelete(callID, dialog)
		g.stopCascadeMediaSession(session, false, false)
	}
}

func (g *GB28181API) resolveCascadeChannel(localChannelID string) (*ipc.Channel, *ipc.Device, error) {
	if g == nil || g.core.Store() == nil {
		return nil, nil, fmt.Errorf("channel store is unavailable")
	}
	var channel ipc.Channel
	if err := g.core.Store().Channel().Get(context.Background(), &channel, orm.Where("channel_id = ?", localChannelID)); err != nil {
		return nil, nil, fmt.Errorf("shared channel not found: %w", err)
	}
	var device ipc.Device
	if err := g.core.Store().Device().Get(context.Background(), &device, orm.Where("device_id = ?", channel.DeviceID)); err != nil {
		return nil, nil, fmt.Errorf("shared channel device not found: %w", err)
	}
	if !channel.IsOnline || !device.IsOnline {
		return nil, nil, ErrDeviceOffline
	}
	return &channel, &device, nil
}

func (g *GB28181API) acquireCascadeSource(ctx context.Context, server *sms.MediaServer, device *ipc.Device, channel *ipc.Channel, offer *cascadeVideoOffer) (*cascadeSourceRef, error) {
	mode := offer.Mode
	key := cascadeSourceKey(channel, offer)
	for {
		g.cascadeMediaMu.Lock()
		if source := g.cascadeSources[key]; source != nil {
			if source.stopping {
				done := source.stopDone
				g.cascadeMediaMu.Unlock()
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-done:
					continue
				}
			}
			source.refs++
			g.cascadeMediaMu.Unlock()
			return source, nil
		}
		break
	}
	stream, exists := g.streams.Load(key)
	owned := false
	if !exists || stream == nil || stream.Stop || stream.Resp == nil {
		if mode == historyModePlay {
			play := g.Play
			if g.cascadePlay != nil {
				play = g.cascadePlay
			}
			playInput := &PlayInput{
				Channel: channel, SMS: server, StreamMode: device.StreamMode,
				preferredPath: offer.PreferredPath,
			}
			if offer.PreferredPath != "" {
				playInput.sessionKey = key
				playInput.streamID = cascadeSourceStreamID(key)
			}
			if err := play(playInput); err != nil {
				g.cascadeMediaMu.Unlock()
				return nil, err
			}
		} else {
			startHistory := g.StartHistory
			if g.cascadeHistory != nil {
				startHistory = g.cascadeHistory
			}
			if err := startHistory(ctx, &HistoryInput{
				Channel: channel, SMS: server, StreamMode: device.StreamMode,
				StartAt: offer.StartAt, EndAt: offer.EndAt, Mode: mode,
				Transport: historyTransportRTP, DownloadSpeed: offer.DownloadSpeed,
				sessionKey: key, streamID: cascadeSourceStreamID(key), preferredPath: offer.PreferredPath,
			}); err != nil {
				g.cascadeMediaMu.Unlock()
				return nil, err
			}
		}
		stream, exists = g.streams.Load(key)
		if !exists || stream == nil {
			g.cascadeMediaMu.Unlock()
			return nil, fmt.Errorf("cascade source stream was not created")
		}
		owned = true
	}
	source := &cascadeSourceRef{
		key: key, refs: 1, owned: owned, stopDone: make(chan struct{}),
		channel: channel, device: device, server: server, stream: stream, mode: mode,
	}
	g.cascadeSources[key] = source
	g.cascadeMediaMu.Unlock()
	select {
	case <-ctx.Done():
		g.releaseCascadeSource(source, false)
		return nil, ctx.Err()
	default:
		return source, nil
	}
}

func (g *GB28181API) waitCascadeSource(ctx context.Context, server *sms.MediaServer, streamID string) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(8 * time.Second)
	defer timeout.Stop()
	for {
		items, err := g.sms.GetMediaInfo(server, cascadeSourceApp, streamID)
		if err == nil && len(items) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
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
	g.cascadeMediaMu.Unlock()
	if source.owned && !sourceEnded {
		if source.mode == historyModePlay {
			if g.cascadeStop != nil {
				_ = g.cascadeStop(context.Background(), &StopPlayInput{Channel: source.channel, sessionKey: source.key})
			} else {
				_ = g.StopPlay(context.Background(), &StopPlayInput{Channel: source.channel, sessionKey: source.key})
			}
		} else {
			if g.cascadeStopHistory != nil {
				_ = g.cascadeStopHistory(context.Background(), &StopHistoryInput{Channel: source.channel, Mode: source.mode, sessionKey: source.key})
			} else {
				_ = g.StopHistory(context.Background(), &StopHistoryInput{Channel: source.channel, Mode: source.mode, sessionKey: source.key})
			}
		}
		if g.sms != nil && source.server != nil {
			_, _ = g.sms.CloseRTPServer(source.server, zlm.CloseRTPServerRequest{StreamID: source.stream.StreamID})
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

func cascadeHistoryURIMatches(uri, exposedID string) bool {
	value := strings.TrimSpace(uri)
	return value == strings.TrimSpace(exposedID)+":0"
}

func (g *GB28181API) stopCascadeMediaSession(session *cascadeMediaSession, sendUpstreamBYE, sourceEnded bool) {
	if session == nil {
		return
	}
	session.stopOnce.Do(func() {
		if session.cancel != nil {
			session.cancel()
		}
		if g.sms != nil && session.server != nil && session.stream != "" && session.ssrc != "" {
			_, _ = g.sms.StopSendRTP(session.server, zlm.StopSendRTPRequest{Vhost: session.vhost, App: session.app, Stream: session.stream, SSRC: session.ssrc})
		}
		g.releaseCascadeSource(session.source, sourceEnded)
		if sendUpstreamBYE {
			g.inviteDialogs.Range(func(_, value any) bool {
				dialog, _ := value.(*inboundInviteDialog)
				if dialog != nil && dialog.Cascade == session {
					_ = g.sendInboundDialogBYE(dialog)
					return false
				}
				return true
			})
		}
	})
}

func (g *GB28181API) terminateCascadeSessionsForStream(stream *Streams) {
	if stream == nil {
		return
	}
	g.inviteDialogs.Range(func(key, value any) bool {
		dialog, _ := value.(*inboundInviteDialog)
		if dialog == nil || dialog.Cascade == nil || dialog.Cascade.source == nil || dialog.Cascade.source.stream == nil {
			return true
		}
		sourceStream := dialog.Cascade.source.stream
		if sourceStream != stream && (sourceStream.StreamID != stream.StreamID || sourceStream.DeviceID != stream.DeviceID || sourceStream.ChannelID != stream.ChannelID) {
			return true
		}
		_ = g.sendInboundDialogBYE(dialog)
		g.inviteDialogs.Delete(key)
		g.stopCascadeMediaSession(dialog.Cascade, false, true)
		return true
	})
}

func buildCascadeSDPAnswer(localID string, server *sms.MediaServer, offer *cascadeVideoOffer, localPort int) ([]byte, error) {
	if offer == nil || server == nil || localPort <= 0 || localPort > 65535 {
		return nil, fmt.Errorf("invalid cascade SDP answer input")
	}
	ipAddress, err := GetIP(server.GetSDPIP())
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
		Origin:     sdp.Origin{Username: localID, NetworkType: "IN", AddressType: "IP4", Address: ipAddress},
		Name:       mode,
		Connection: sdp.ConnectionData{NetworkType: "IN", AddressType: "IP4", IP: net.ParseIP(ipAddress)},
		Timing:     []sdp.Timing{{}}, Medias: []sdp.Media{video}, SSRC: offer.SSRC,
	}
	if mode != historyModePlay {
		if strings.TrimSpace(offer.URI) == "" || offer.StartAt.IsZero() || offer.EndAt.IsZero() || !offer.EndAt.After(offer.StartAt) {
			return nil, fmt.Errorf("invalid cascade %s SDP answer input", mode)
		}
		message.URI = strings.TrimSpace(offer.URI)
		message.Timing = []sdp.Timing{{Start: offer.StartAt, End: offer.EndAt}}
	}
	body := message.Append(nil).AppendTo(nil)
	return append(body, "f=v/////a/1/8/1\r\n"...), nil
}
