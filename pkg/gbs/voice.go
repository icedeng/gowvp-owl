package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"net"
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

const (
	voiceModeTalk      = "Talk"
	voiceModeBroadcast = "Broadcast"

	defaultBroadcastVHost = "__defaultVhost__"
	defaultBroadcastApp   = "live"
	broadcastPSPayload    = 96
	broadcastPCMAPayload  = 8
	broadcastRTPTypeES    = 0
	broadcastRTPTypePS    = 1
)

type rtpMediaService interface {
	OpenRTPServer(*sms.MediaServer, zlm.OpenRTPServerRequest) (*zlm.OpenRTPServerResponse, error)
	CloseRTPServer(*sms.MediaServer, zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error)
	GetMediaInfo(*sms.MediaServer, string, string) ([]zlm.MediaItem, error)
	StartSendRTP(*sms.MediaServer, zlm.StartSendRTPRequest) (*zlm.StartSendRTPResponse, error)
	StartSendRTPTalk(*sms.MediaServer, zlm.StartSendRTPTalkRequest) (*zlm.StartSendRTPResponse, error)
	StopSendRTP(*sms.MediaServer, zlm.StopSendRTPRequest) (*zlm.StopSendRTPResponse, error)
}

type VoiceInput struct {
	Channel      *ipc.Channel
	SMS          *sms.MediaServer
	StreamMode   int8
	Mode         string // Talk/Broadcast
	Timeout      time.Duration
	SourceID     string
	SourceVHost  string
	SourceApp    string
	SourceStream string
}

type broadcastNotify struct {
	XMLName  xml.Name `xml:"Notify"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	SourceID string   `xml:"SourceID"`
	TargetID string   `xml:"TargetID"`
}

type broadcastResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Result   string   `xml:"Result"`
}

type pendingBroadcastResponse struct {
	wait chan *broadcastResponse
}

type broadcastSession struct {
	DeviceID     string
	ChannelID    string
	SourceID     string
	SourceVHost  string
	SourceApp    string
	SourceStream string
	SMS          *sms.MediaServer
	SSRC         string
	Stream       *Streams
	Dialog       *inboundInviteDialog
	CreatedAt    time.Time
	Version      GBProtocolVersion
	Cascade      *cascadeVoiceSourceSession

	mu         sync.Mutex
	rtpStarted bool
	inviteBusy bool
	stopped    bool
	ready      chan error
	readyOnce  sync.Once
	stopOnce   sync.Once
}

type talkSession struct {
	DeviceID      string
	ChannelID     string
	ReceiveStream string
	SourceVHost   string
	SourceApp     string
	SourceStream  string
	SMS           *sms.MediaServer
	SSRC          string
	Stream        *Streams

	mu             sync.Mutex
	receiverOpened bool
	rtpStarted     bool
	startBusy      bool
	stopped        bool
	ready          chan error
	readyOnce      sync.Once
	stopOnce       sync.Once
}

func (s *talkSession) complete(err error) {
	s.readyOnce.Do(func() { s.ready <- err })
}

type voiceMediaSource struct {
	ID     string
	VHost  string
	App    string
	Stream string
}

func (s *broadcastSession) complete(err error) {
	s.readyOnce.Do(func() { s.ready <- err })
}

type StopVoiceInput struct {
	Channel *ipc.Channel
	Mode    string
}

func voiceKey(mode, deviceID, channelID string) string {
	return "voice:" + mode + ":" + deviceID + ":" + channelID
}

// StartVoice 启动语音会话（9.12），支持 Talk/Broadcast 信令流程。
func (g *GB28181API) StartVoice(ctx context.Context, in *VoiceInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid voice input")
	}
	if in.Mode != voiceModeTalk && in.Mode != voiceModeBroadcast {
		return fmt.Errorf("invalid voice mode: %s", in.Mode)
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return ErrChannelNotExist
	}
	if !ch.device.IsOnlineNow() {
		return ErrDeviceOffline
	}
	switch in.Mode {
	case voiceModeBroadcast:
		if err := g.requireGBFeature(in.Channel.DeviceID, "voice_broadcast", "语音广播", func(c GBCapabilities) bool {
			return c.VoiceBroadcast
		}); err != nil {
			return err
		}
	case voiceModeTalk:
		if err := g.requireGBFeature(in.Channel.DeviceID, "voice_intercom", "语音对讲", func(c GBCapabilities) bool {
			return c.VoiceIntercom
		}); err != nil {
			return err
		}
	}
	if in.Mode == voiceModeTalk {
		if err := g.requireMediaTransport(in.Channel.DeviceID, in.StreamMode, "语音会话"); err != nil {
			return err
		}
	}

	unlock, err := ch.device.lockMediaContext(ctx, ch.ChannelID)
	if err != nil {
		return err
	}
	defer unlock()
	if in.Mode == voiceModeBroadcast {
		return g.startBroadcast(ctx, ch, in)
	}
	return g.startTalk(ctx, ch, in)
}

func (g *GB28181API) startTalk(ctx context.Context, ch *Channel, in *VoiceInput) (err error) {
	source, err := g.resolveVoiceMediaSource(in)
	if err != nil {
		return err
	}
	key := voiceKey(voiceModeTalk, in.Channel.DeviceID, in.Channel.ChannelID)
	if _, exists := g.streams.Load(key); exists {
		_ = g.stopVoiceNoLock(ch, &StopVoiceInput{Channel: in.Channel, Mode: voiceModeTalk})
	}
	stream := &Streams{DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID, StreamID: in.Channel.ID, Status: -1}
	session := &talkSession{
		DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID, ReceiveStream: in.Channel.ID,
		SourceVHost: source.VHost, SourceApp: source.App, SourceStream: source.Stream,
		SMS: in.SMS, Stream: stream, ready: make(chan error, 1),
	}
	if previous, loaded := g.talkSessions.LoadOrStore(session.ReceiveStream, session); loaded {
		if old, ok := previous.(*talkSession); ok {
			_ = g.stopTalkSession(old, fmt.Errorf("Talk session replaced"))
		}
		g.talkSessions.Store(session.ReceiveStream, session)
	}
	g.streams.Store(key, stream)
	defer func() {
		if err == nil {
			return
		}
		if stream.Resp != nil {
			if req, requestErr := sip.NewRequestFromResponseChecked(sip.MethodBYE, stream.Resp); requestErr == nil {
				req.SetDestination(ch.Source())
				req.SetConnection(ch.Conn())
				_, _ = g.svr.Request(req)
			}
		}
		_ = g.stopTalkSession(session, err)
		g.streams.CompareAndDelete(key, stream)
	}()

	resp, err := g.sms.OpenRTPServer(in.SMS, zlm.OpenRTPServerRequest{TCPMode: in.StreamMode, StreamID: in.Channel.ID})
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.receiverOpened = true
	session.mu.Unlock()
	if err = g.sipInviteVoice(ctx, ch, in, resp.Port, stream); err != nil {
		return err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err = <-session.ready:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-g.serviceDone():
		return ErrServiceStopped
	case <-timer.C:
		return fmt.Errorf("wait Talk media stream timeout")
	}
	if g.core.Store() != nil {
		_ = g.core.EditPlaying(ctx, session.DeviceID, session.ChannelID, true)
	}
	return nil
}

func (g *GB28181API) startBroadcast(ctx context.Context, ch *Channel, in *VoiceInput) (err error) {
	session, err := g.newBroadcastSession(in)
	if err != nil {
		return err
	}
	if version, ok := ParseGBProtocolVersion(ch.GBVersion()); ok {
		session.Version = version
	} else {
		session.Version = GBVersion11
	}
	if existing, loaded := g.broadcastSessions.LoadOrStore(session.ChannelID, session); loaded {
		if previous, ok := existing.(*broadcastSession); ok {
			_ = g.stopBroadcastSession(previous, true)
		}
		g.broadcastSessions.Store(session.ChannelID, session)
	}
	defer func() {
		if err != nil {
			_ = g.stopBroadcastSession(session, true)
		}
	}()

	if err = g.startBroadcastNotification(ctx, ch, in); err != nil {
		return err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err = <-session.ready:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-g.serviceDone():
		return ErrServiceStopped
	case <-timer.C:
		return fmt.Errorf("wait Broadcast INVITE timeout")
	}

	g.streams.Store(voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID), session.Stream)
	if g.core.Store() != nil {
		_ = g.core.EditPlaying(ctx, session.DeviceID, session.ChannelID, true)
	}
	return nil
}

func (g *GB28181API) newBroadcastSession(in *VoiceInput) (*broadcastSession, error) {
	source, err := g.resolveVoiceMediaSource(in)
	if err != nil {
		return nil, err
	}
	return &broadcastSession{
		DeviceID:     in.Channel.DeviceID,
		ChannelID:    in.Channel.ChannelID,
		SourceID:     source.ID,
		SourceVHost:  source.VHost,
		SourceApp:    source.App,
		SourceStream: source.Stream,
		SMS:          in.SMS,
		Stream: &Streams{
			T: 0, DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
			StreamID: in.Channel.ID, Status: -1,
		},
		CreatedAt: time.Now(),
		ready:     make(chan error, 1),
	}, nil
}

func (g *GB28181API) resolveVoiceMediaSource(in *VoiceInput) (*voiceMediaSource, error) {
	cfg := g.configSnapshot()
	if cfg == nil {
		return nil, fmt.Errorf("SIP configuration is unavailable")
	}
	if g.sms == nil || in.SMS == nil {
		return nil, fmt.Errorf("media server is required for voice")
	}
	if in.SMS.Type != "" && in.SMS.Type != sms.ProtocolZLMediaKit {
		return nil, fmt.Errorf("voice RTP sending requires ZLMediaKit")
	}
	sourceStream := strings.TrimSpace(in.SourceStream)
	if sourceStream == "" {
		return nil, fmt.Errorf("source_stream is required for voice")
	}
	sourceApp := strings.TrimSpace(in.SourceApp)
	if sourceApp == "" {
		sourceApp = defaultBroadcastApp
	}
	sourceVHost := strings.TrimSpace(in.SourceVHost)
	if sourceVHost == "" {
		sourceVHost = defaultBroadcastVHost
	}
	sourceID := strings.TrimSpace(in.SourceID)
	if sourceID == "" {
		sourceID = cfg.ID
	}
	if err := filterUnknowDevices(sourceID); err != nil {
		return nil, fmt.Errorf("invalid voice source_id: %w", err)
	}
	items, err := g.sms.GetMediaInfo(in.SMS, sourceApp, sourceStream)
	if err != nil {
		return nil, fmt.Errorf("query voice source stream: %w", err)
	}
	if !hasReadyG711Audio(items) {
		return nil, fmt.Errorf("voice source stream must contain a ready G.711 A-law audio track")
	}
	return &voiceMediaSource{ID: sourceID, VHost: sourceVHost, App: sourceApp, Stream: sourceStream}, nil
}

func hasReadyG711Audio(items []zlm.MediaItem) bool {
	for _, item := range items {
		for _, track := range item.Tracks {
			if track.CodecType != 1 || !track.Ready {
				continue
			}
			name := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(track.CodecIDName), ".", ""))
			if name == "G711A" || name == "PCMA" || track.CodecID == 3 {
				return true
			}
		}
	}
	return false
}

func (g *GB28181API) startBroadcastNotification(ctx context.Context, ch *Channel, in *VoiceInput) error {
	cfg := g.configSnapshot()
	if cfg == nil {
		return fmt.Errorf("SIP configuration is unavailable")
	}
	sn := g.nextControlSN()
	body, err := sip.XMLEncode(broadcastNotify{
		CmdType:  "Broadcast",
		SN:       sn,
		SourceID: broadcastSourceID(cfg.ID, in.SourceID),
		TargetID: ch.ChannelID,
	})
	if err != nil {
		return err
	}
	key := buildPendingBroadcastKey(ch.ChannelID, sn)
	pending := &pendingBroadcastResponse{wait: make(chan *broadcastResponse, 1)}
	g.pendingBroadcast.Store(key, pending)
	defer g.pendingBroadcast.Delete(key)
	tx, err := g.svr.wrapRequest(ch, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		return err
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-pending.wait:
		if response.Result != "" && !strings.EqualFold(strings.TrimSpace(response.Result), "OK") {
			return fmt.Errorf("broadcast rejected: %s", response.Result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-g.serviceDone():
		return ErrServiceStopped
	case <-timer.C:
		return fmt.Errorf("wait Broadcast response timeout")
	}
}

func broadcastSourceID(platformID, sourceID string) string {
	if sourceID = strings.TrimSpace(sourceID); sourceID != "" {
		return sourceID
	}
	return strings.TrimSpace(platformID)
}

func (g *GB28181API) sipMessageBroadcastResponse(ctx *sip.Context) {
	var response broadcastResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &response); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}
	key := buildPendingBroadcastKey(response.DeviceID, response.SN)
	if value, ok := g.pendingBroadcast.Load(key); ok {
		select {
		case value.(*pendingBroadcastResponse).wait <- &response:
		default:
		}
	}
	ctx.String(200, "OK")
}

func buildPendingBroadcastKey(targetID string, sn int) string {
	return strings.TrimSpace(targetID) + ":" + fmt.Sprintf("%d", sn)
}

// StopVoice 停止语音会话。
func (g *GB28181API) StopVoice(ctx context.Context, in *StopVoiceInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid stop voice input")
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return ErrChannelNotExist
	}
	unlock, err := ch.device.lockMediaContext(ctx, ch.ChannelID)
	if err != nil {
		return err
	}
	defer unlock()
	defer func() {
		_ = g.svr.gb.core.EditPlaying(ctx, in.Channel.DeviceID, in.Channel.ChannelID, false)
	}()
	return g.stopVoiceNoLock(ch, in)
}

func (g *GB28181API) stopVoiceNoLock(ch *Channel, in *StopVoiceInput) error {
	if in.Mode == voiceModeBroadcast {
		if value, ok := g.broadcastSessions.Load(in.Channel.ChannelID); ok {
			if session, ok := value.(*broadcastSession); ok {
				return g.stopBroadcastSession(session, true)
			}
		}
		g.streams.Delete(voiceKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID))
		return nil
	}
	key := voiceKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	stream, ok := g.streams.LoadAndDelete(key)
	if !ok {
		return nil
	}
	var result error
	if stream.Resp != nil {
		req, requestErr := sip.NewRequestFromResponseChecked(sip.MethodBYE, stream.Resp)
		if requestErr != nil {
			result = requestErr
		} else {
			req.SetDestination(ch.Source())
			req.SetConnection(ch.Conn())
			_, result = g.svr.Request(req)
		}
	}
	if value, ok := g.talkSessions.Load(stream.StreamID); ok {
		if session, ok := value.(*talkSession); ok {
			if err := g.stopTalkSession(session, nil); result == nil {
				result = err
			}
		}
	}
	return result
}

func (g *GB28181API) startTalkRTP(streamID string) error {
	value, ok := g.talkSessions.Load(strings.TrimSpace(streamID))
	if !ok {
		return nil
	}
	session, ok := value.(*talkSession)
	if !ok || session == nil {
		return nil
	}
	session.mu.Lock()
	if session.stopped || session.rtpStarted || session.startBusy {
		session.mu.Unlock()
		return nil
	}
	session.startBusy = true
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.startBusy = false
		session.mu.Unlock()
	}()
	ssrc, err := g.getSSRC(0)
	if err != nil {
		session.complete(err)
		return err
	}
	_, err = g.sms.StartSendRTPTalk(session.SMS, zlm.StartSendRTPTalkRequest{
		Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream,
		SSRC: ssrc, RecvStreamID: session.ReceiveStream, Type: broadcastRTPTypeES, PT: broadcastPCMAPayload, OnlyAudio: true,
	})
	if err != nil {
		session.complete(fmt.Errorf("start Talk RTP: %w", err))
		return err
	}
	session.mu.Lock()
	if session.stopped {
		session.mu.Unlock()
		_, _ = g.sms.StopSendRTP(session.SMS, zlm.StopSendRTPRequest{Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc})
		return nil
	}
	session.SSRC = ssrc
	session.rtpStarted = true
	session.mu.Unlock()
	session.complete(nil)
	return nil
}

func (g *GB28181API) stopTalkSession(session *talkSession, cause error) (result error) {
	if session == nil {
		return nil
	}
	session.stopOnce.Do(func() {
		session.mu.Lock()
		started := session.rtpStarted
		opened := session.receiverOpened
		ssrc := session.SSRC
		session.rtpStarted = false
		session.receiverOpened = false
		session.stopped = true
		session.mu.Unlock()
		if started {
			_, result = g.sms.StopSendRTP(session.SMS, zlm.StopSendRTPRequest{
				Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc,
			})
		}
		if opened {
			_, err := g.sms.CloseRTPServer(session.SMS, zlm.CloseRTPServerRequest{StreamID: session.ReceiveStream})
			if result == nil {
				result = err
			}
		}
		g.talkSessions.CompareAndDelete(session.ReceiveStream, session)
		if cause != nil {
			session.complete(cause)
		}
		if g.core.Store() != nil {
			_ = g.core.EditPlaying(context.Background(), session.DeviceID, session.ChannelID, false)
		}
	})
	return result
}

func (g *GB28181API) stopBroadcastSession(session *broadcastSession, sendBYE bool) (result error) {
	if session == nil {
		return nil
	}
	session.stopOnce.Do(func() {
		session.mu.Lock()
		dialog := session.Dialog
		session.mu.Unlock()
		if sendBYE && dialog != nil {
			if err := g.sendInboundDialogBYE(dialog); err != nil {
				result = err
			}
		}
		session.mu.Lock()
		started := session.rtpStarted
		ssrc := session.SSRC
		session.rtpStarted = false
		session.stopped = true
		session.mu.Unlock()
		if started && g.sms != nil && session.SMS != nil {
			_, err := g.sms.StopSendRTP(session.SMS, zlm.StopSendRTPRequest{
				Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc,
			})
			if result == nil && err != nil {
				result = err
			}
		}
		g.broadcastSessions.CompareAndDelete(session.ChannelID, session)
		if session.Stream != nil {
			g.streams.CompareAndDelete(voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID), session.Stream)
		}
		if g.core.Store() != nil {
			_ = g.core.EditPlaying(context.Background(), session.DeviceID, session.ChannelID, false)
		}
		session.complete(fmt.Errorf("Broadcast session stopped"))
		if err := g.stopCascadeVoiceSource(session.Cascade, true); result == nil && err != nil {
			result = err
		}
	})
	return result
}

func (g *GB28181API) findBroadcastSession(deviceID string) *broadcastSession {
	deviceID = strings.TrimSpace(deviceID)
	if value, ok := g.broadcastSessions.Load(deviceID); ok {
		session, _ := value.(*broadcastSession)
		return session
	}
	var matched *broadcastSession
	g.broadcastSessions.Range(func(_, value any) bool {
		session, ok := value.(*broadcastSession)
		if !ok || session == nil || session.DeviceID != deviceID {
			return true
		}
		if matched != nil {
			matched = nil
			return false
		}
		matched = session
		return true
	})
	return matched
}

func (g *GB28181API) findBroadcastSessionForInvite(deviceID, subject string) *broadcastSession {
	receiverID := broadcastReceiverIDFromSubject(subject)
	if receiverID != "" {
		value, ok := g.broadcastSessions.Load(receiverID)
		if !ok {
			return nil
		}
		session, _ := value.(*broadcastSession)
		if session == nil || strings.TrimSpace(session.DeviceID) != strings.TrimSpace(deviceID) {
			return nil
		}
		return session
	}
	return g.findBroadcastSession(deviceID)
}

func broadcastReceiverIDFromSubject(subject string) string {
	_, receiver, ok := strings.Cut(strings.TrimSpace(subject), ",")
	if !ok {
		return ""
	}
	receiverID, _, _ := strings.Cut(strings.TrimSpace(receiver), ":")
	receiverID = strings.TrimSpace(receiverID)
	if filterUnknowDevices(receiverID) != nil {
		return ""
	}
	return receiverID
}

func parseBroadcastPayload(media *sdp.Media, version GBProtocolVersion) (payload int, mapping string, rtpType int, err error) {
	if version != GBVersion20 && version != GBVersion30 {
		version = GBVersion11
	}
	for _, format := range media.Description.Formats {
		value, parseErr := strconv.Atoi(format)
		if parseErr != nil || value < 0 || value > 127 {
			continue
		}
		formatName := strings.ToUpper(strings.TrimSpace(media.PayloadFormat(format)))
		if version == GBVersion11 && (formatName == "PS/90000" || formatName == "" && value == broadcastPSPayload) {
			return value, "PS/90000", broadcastRTPTypePS, nil
		}
		if version.AtLeast(GBVersion20) && (formatName == "PCMA/8000" || formatName == "PCMA/8000/1" || formatName == "" && value == broadcastPCMAPayload) {
			return value, "PCMA/8000", broadcastRTPTypeES, nil
		}
	}
	if version == GBVersion11 {
		return 0, "", 0, fmt.Errorf("Broadcast INVITE does not offer PS/90000 audio for protocol 1.1")
	}
	return 0, "", 0, fmt.Errorf("Broadcast INVITE does not offer PCMA/8000 audio for protocol %s", version)
}

func (g *GB28181API) sipInviteVoice(ctx context.Context, ch *Channel, in *VoiceInput, port int, stream *Streams) error {
	cfg := g.configSnapshot()
	if cfg == nil {
		return fmt.Errorf("SIP configuration is unavailable")
	}
	protocol := "TCP/RTP/AVP"
	if in.StreamMode == 0 {
		protocol = "RTP/AVP"
	}
	audio := sdp.Media{
		Description: sdp.MediaDescription{
			Type:     "audio",
			Port:     port,
			Formats:  []string{"8", "0", "9"},
			Protocol: protocol,
		},
	}
	if in.StreamMode == 1 {
		audio.AddAttribute("setup", "passive")
		audio.AddAttribute("connection", "new")
	}
	if in.StreamMode == 2 {
		audio.AddAttribute("setup", "active")
		audio.AddAttribute("connection", "new")
	}
	audio.AddAttribute("sendrecv")
	audio.AddAttribute("rtpmap", "8", "PCMA/8000")
	audio.AddAttribute("rtpmap", "0", "PCMU/8000")
	audio.AddAttribute("rtpmap", "9", "G722/8000")

	ip4str, err := GetIP(in.SMS.GetSDPIP())
	if err != nil {
		return err
	}
	ssrc, err := g.getSSRC(0)
	if err != nil {
		return err
	}
	msg := &sdp.Message{
		Origin: sdp.Origin{
			Username:    ch.ChannelID,
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     ip4str,
		},
		Name: historyModePlay,
		Connection: sdp.ConnectionData{
			NetworkType: "IN",
			AddressType: "IP4",
			IP:          net.ParseIP(ip4str),
		},
		Timing: []sdp.Timing{{}},
		Medias: []sdp.Media{audio},
		SSRC:   ssrc,
	}
	body := msg.Append(nil).AppendTo(nil)
	body = append(body, "f=v/////a/1/8/1\r\n"...)
	tx, err := g.svr.wrapRequest(ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(ch.ChannelID, ssrc, cfg.ID)})
	})
	if err != nil {
		return err
	}
	resp, err := sipResponseContext(ctx, tx)
	if err != nil {
		return err
	}
	stream.Resp = resp
	stream.T = 0
	stream.DeviceID = in.Channel.DeviceID
	stream.ChannelID = in.Channel.ChannelID
	stream.StreamID = in.Channel.ID
	stream.Status = 0
	stream.ssrc = ssrc
	if callID, ok := resp.CallID(); ok {
		stream.CallID = normalizeCallID(callID)
	}
	ack, err := sip.NewRequestFromResponseChecked(sip.MethodACK, resp)
	if err != nil {
		return err
	}
	return tx.Request(ack)
}
