package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
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
	voiceModeTalk              = "Talk"
	voiceModeTalkStandard      = "TalkStandard"
	voiceModeBroadcast         = "Broadcast"
	voiceCleanupRetryInterval  = time.Second
	voiceShutdownRetryInterval = 100 * time.Millisecond

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

type rtpMediaCloserContext interface {
	CloseRTPServerContext(context.Context, *sms.MediaServer, zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error)
}

type rtpMediaOpenerContext interface {
	OpenRTPServerContext(context.Context, *sms.MediaServer, zlm.OpenRTPServerRequest) (*zlm.OpenRTPServerResponse, error)
}

type rtpMediaSenderContext interface {
	StartSendRTPContext(context.Context, *sms.MediaServer, zlm.StartSendRTPRequest) (*zlm.StartSendRTPResponse, error)
}

type rtpMediaTalkSenderContext interface {
	StartSendRTPTalkContext(context.Context, *sms.MediaServer, zlm.StartSendRTPTalkRequest) (*zlm.StartSendRTPResponse, error)
}

type rtpMediaStopperContext interface {
	StopSendRTPContext(context.Context, *sms.MediaServer, zlm.StopSendRTPRequest) (*zlm.StopSendRTPResponse, error)
}

type rtpMediaInfoContext interface {
	GetMediaInfoContext(context.Context, *sms.MediaServer, string, string) ([]zlm.MediaItem, error)
}

func openRTPServerContext(ctx context.Context, media rtpMediaService, server *sms.MediaServer, in zlm.OpenRTPServerRequest) (*zlm.OpenRTPServerResponse, error) {
	if service, ok := media.(rtpMediaOpenerContext); ok {
		return service.OpenRTPServerContext(ctx, server, in)
	}
	return media.OpenRTPServer(server, in)
}

func closeRTPServerContext(ctx context.Context, media rtpMediaService, server *sms.MediaServer, in zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error) {
	if service, ok := media.(rtpMediaCloserContext); ok {
		return service.CloseRTPServerContext(ctx, server, in)
	}
	return media.CloseRTPServer(server, in)
}

func stopSendRTPContext(ctx context.Context, media rtpMediaService, server *sms.MediaServer, in zlm.StopSendRTPRequest) (*zlm.StopSendRTPResponse, error) {
	if service, ok := media.(rtpMediaStopperContext); ok {
		return service.StopSendRTPContext(ctx, server, in)
	}
	return media.StopSendRTP(server, in)
}

func startSendRTPContext(ctx context.Context, media rtpMediaService, server *sms.MediaServer, in zlm.StartSendRTPRequest) (*zlm.StartSendRTPResponse, error) {
	if service, ok := media.(rtpMediaSenderContext); ok {
		return service.StartSendRTPContext(ctx, server, in)
	}
	return media.StartSendRTP(server, in)
}

func startSendRTPTalkContext(ctx context.Context, media rtpMediaService, server *sms.MediaServer, in zlm.StartSendRTPTalkRequest) (*zlm.StartSendRTPResponse, error) {
	if service, ok := media.(rtpMediaTalkSenderContext); ok {
		return service.StartSendRTPTalkContext(ctx, server, in)
	}
	return media.StartSendRTPTalk(server, in)
}

func getMediaInfoContext(ctx context.Context, media rtpMediaService, server *sms.MediaServer, app, stream string) ([]zlm.MediaItem, error) {
	if service, ok := media.(rtpMediaInfoContext); ok {
		return service.GetMediaInfoContext(ctx, server, app, stream)
	}
	return media.GetMediaInfo(server, app, stream)
}

type VoiceInput struct {
	Channel            *ipc.Channel
	SMS                *sms.MediaServer
	ResolveMediaServer MediaServerResolver
	StreamMode         int8
	Mode               string // Talk/Broadcast
	Timeout            time.Duration
	SourceID           string
	SourceVHost        string
	SourceApp          string
	SourceStream       string

	standardTalkPlayKey string
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
	wait      chan *broadcastResponse
	operation *pendingDeviceOperation
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
	// StandardTalkPlayKey 非空时表示该广播会话是标准语音对讲的下行半链路。
	StandardTalkPlayKey string

	mu         sync.Mutex
	rtpStarted bool
	inviteBusy bool
	stopped    bool
	ready      chan error
	readyOnce  sync.Once
	stopMu     sync.Mutex
	dialogDone bool
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
	ssrcRelease   func()

	mu             sync.Mutex
	receiverOpened bool
	rtpStarted     bool
	startBusy      bool
	stopped        bool
	ready          chan error
	readyOnce      sync.Once
	stopMu         sync.Mutex
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

func standardTalkPlayKey(deviceID, channelID string) string {
	return voiceKey("TalkUpstream", deviceID, channelID)
}

func standardTalkStreamID(channel *ipc.Channel) string {
	if channel == nil {
		return ""
	}
	base := strings.TrimSpace(channel.ID)
	if base == "" {
		base = strings.TrimSpace(channel.ChannelID)
	}
	return base + "-talk-upstream"
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
	if in.Mode != voiceModeTalk && in.Mode != voiceModeTalkStandard && in.Mode != voiceModeBroadcast {
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
	case voiceModeTalk, voiceModeTalkStandard:
		if err := g.requireGBFeature(in.Channel.DeviceID, "voice_intercom", "语音对讲", func(c GBCapabilities) bool {
			return c.VoiceIntercom
		}); err != nil {
			return err
		}
		if in.Mode == voiceModeTalkStandard {
			if err := g.requireGBVersionAtLeast(in.Channel.DeviceID, string(GBVersion20), "标准双流程语音对讲"); err != nil {
				return err
			}
			if err := g.requireGBFeature(in.Channel.DeviceID, "voice_broadcast", "标准语音对讲下行广播", func(c GBCapabilities) bool {
				return c.VoiceBroadcast
			}); err != nil {
				return err
			}
		}
	}
	if in.Mode == voiceModeTalk || in.Mode == voiceModeTalkStandard {
		if err := g.requireMediaTransport(in.Channel.DeviceID, in.StreamMode, "语音会话"); err != nil {
			return err
		}
	}
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)

	unlock, err := ch.device.lockMediaContext(requestCtx, ch.ChannelID)
	if err != nil {
		return operation.ErrorOr(err)
	}
	defer unlock()
	in.SMS, err = resolveMediaServerAfterLock(requestCtx, in.SMS, in.ResolveMediaServer)
	if err != nil {
		return operation.ErrorOr(err)
	}
	var startErr error
	if in.Mode == voiceModeBroadcast {
		startErr = g.startBroadcast(requestCtx, ch, in)
	} else if in.Mode == voiceModeTalkStandard {
		startErr = g.startStandardTalk(requestCtx, ch, in)
	} else {
		startErr = g.startTalk(requestCtx, ch, in)
	}
	if startErr != nil {
		return operation.ErrorOr(startErr)
	}
	if !operation.Deliver(func() {}) {
		_ = g.stopVoiceNoLock(g.mediaPersistenceContext(), ch, &StopVoiceInput{
			Channel: in.Channel, Mode: in.Mode,
		})
		return operation.Cause()
	}
	return nil
}

func (g *GB28181API) startStandardTalk(ctx context.Context, ch *Channel, in *VoiceInput) (err error) {
	return g.startStandardTalkWith(ctx, ch, in, g.playNoLock, g.startBroadcast)
}

func (g *GB28181API) startStandardTalkWith(
	ctx context.Context,
	ch *Channel,
	in *VoiceInput,
	startPlay func(context.Context, *Channel, *PlayInput) error,
	startBroadcast func(context.Context, *Channel, *VoiceInput) error,
) (err error) {
	playKey := standardTalkPlayKey(in.Channel.DeviceID, in.Channel.ChannelID)
	// 标准对讲的上行 Play 使用固定会话键。重复启动时必须先完整停止旧的
	// 标准对讲，否则新 Play 建立后，旧 Broadcast 的联动清理会按同一键
	// 误停刚创建的新 Play，形成只剩下行广播的半会话。
	if value, ok := g.broadcastSessions.Load(in.Channel.ChannelID); ok {
		if previous, valid := value.(*broadcastSession); valid && previous != nil &&
			strings.TrimSpace(previous.StandardTalkPlayKey) == playKey {
			if stopErr := g.stopBroadcastSession(previous, true); stopErr != nil {
				return fmt.Errorf("stop previous standard Talk session: %w", stopErr)
			}
		}
	}
	playInput := &PlayInput{
		Channel: in.Channel, SMS: in.SMS, StreamMode: in.StreamMode,
		sessionKey: playKey, streamID: standardTalkStreamID(in.Channel), audioOnly: true,
	}
	if err = startPlay(ctx, ch, playInput); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			cleanupErr := g.stopPlayContext(context.WithoutCancel(ctx), ch, &StopPlayInput{Channel: in.Channel, sessionKey: playKey})
			idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
			err = errors.Join(err, cleanupErr, idleErr)
		}
	}()

	broadcastInput := *in
	broadcastInput.Mode = voiceModeBroadcast
	broadcastInput.standardTalkPlayKey = playKey
	if err = startBroadcast(ctx, ch, &broadcastInput); err != nil {
		return err
	}
	return nil
}

func (g *GB28181API) startTalk(ctx context.Context, ch *Channel, in *VoiceInput) (err error) {
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	source, err := g.resolveVoiceMediaSourceContext(requestCtx, in)
	if err != nil {
		return operation.ErrorOr(err)
	}
	key := voiceKey(voiceModeTalk, in.Channel.DeviceID, in.Channel.ChannelID)
	if _, exists := g.streams.Load(key); exists {
		if stopErr := g.stopVoiceNoLock(requestCtx, ch, &StopVoiceInput{Channel: in.Channel, Mode: voiceModeTalk}); stopErr != nil {
			return fmt.Errorf("stop previous Talk session: %w", stopErr)
		}
	}
	stream := &Streams{DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID, StreamID: in.Channel.ID, Status: -1}
	session := &talkSession{
		DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID, ReceiveStream: in.Channel.ID,
		SourceVHost: source.VHost, SourceApp: source.App, SourceStream: source.Stream,
		SMS: in.SMS, Stream: stream, ready: make(chan error, 1),
	}
	defer func() {
		if err == nil {
			return
		}
		cleanupErr := g.stopTalkSession(session, err)
		err = errors.Join(err, cleanupErr)
		err = errors.Join(err, g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), session.DeviceID, session.ChannelID))
	}()
	var (
		previous any
		loaded   bool
	)
	if !operation.Deliver(func() {
		previous, loaded = g.talkSessions.LoadOrStore(session.ReceiveStream, session)
	}) {
		return operation.Cause()
	}
	if loaded {
		if old, ok := previous.(*talkSession); ok {
			if stopErr := g.stopTalkSession(old, fmt.Errorf("Talk session replaced")); stopErr != nil {
				return fmt.Errorf("stop previous Talk session: %w", stopErr)
			}
		} else {
			return fmt.Errorf("previous Talk session has invalid runtime state")
		}
		if !operation.Deliver(func() {
			g.talkSessions.Store(session.ReceiveStream, session)
		}) {
			return operation.Cause()
		}
	}
	if !operation.Deliver(func() {
		g.streams.Store(key, stream)
	}) {
		return operation.Cause()
	}
	// Talk 接收端口与 SDP offer 必须复用同一个 SSRC，防止其他会话串入音频。
	receiveSSRC, releaseReceiveSSRC, err := g.reserveSSRC(0)
	if err != nil {
		return err
	}
	if err := stream.bindSSRCReservation(receiveSSRC, releaseReceiveSSRC); err != nil {
		return err
	}
	receiveSSRCValue, err := strconv.ParseUint(receiveSSRC, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid GB28181 SSRC %q: %w", receiveSSRC, err)
	}
	resp, err := openRTPServerContext(requestCtx, g.sms, in.SMS, zlm.OpenRTPServerRequest{
		TCPMode: in.StreamMode, StreamID: in.Channel.ID,
		SSRC:    receiveSSRCValue,
		TCPRTCP: shouldEnableTCPRTCP(g.getDeviceGBProtocolVersion(in.Channel.DeviceID), in.StreamMode != 0),
	})
	if err != nil {
		return err
	}
	session.mu.Lock()
	session.receiverOpened = true
	session.mu.Unlock()
	if err = g.sipInviteVoice(requestCtx, ch, in, resp.Port, receiveSSRC, stream); err != nil {
		return operation.ErrorOr(err)
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
	case <-requestCtx.Done():
		return operation.Cause()
	case <-g.serviceDone():
		return ErrServiceStopped
	case <-timer.C:
		return fmt.Errorf("wait Talk media stream timeout")
	}
	streamPublished := false
	if !operation.Deliver(func() {
		streamPublished, err = g.commitChannelStreamStart(requestCtx, key, stream)
	}) {
		return operation.Cause()
	}
	if !streamPublished {
		return nil
	}
	if err != nil {
		return fmt.Errorf("persist Talk playing state: %w", err)
	}
	return nil
}

func (g *GB28181API) startBroadcast(ctx context.Context, ch *Channel, in *VoiceInput) (err error) {
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	session, err := g.newBroadcastSessionContext(requestCtx, in)
	if err != nil {
		return operation.ErrorOr(err)
	}
	if version, ok := ParseGBProtocolVersion(ch.GBVersion()); ok {
		session.Version = version
	} else {
		session.Version = GBVersion11
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, g.stopBroadcastSession(session, true))
		}
	}()
	var (
		existing any
		loaded   bool
	)
	if !operation.Deliver(func() {
		existing, loaded = g.broadcastSessions.LoadOrStore(session.ChannelID, session)
	}) {
		return operation.Cause()
	}
	if loaded {
		if previous, ok := existing.(*broadcastSession); ok {
			if stopErr := g.stopBroadcastSession(previous, true); stopErr != nil {
				return fmt.Errorf("stop previous Broadcast session: %w", stopErr)
			}
		} else {
			return fmt.Errorf("previous Broadcast session has invalid runtime state")
		}
		if !operation.Deliver(func() {
			g.broadcastSessions.Store(session.ChannelID, session)
		}) {
			return operation.Cause()
		}
	}

	if err = g.startBroadcastNotification(requestCtx, ch, in); err != nil {
		return operation.ErrorOr(err)
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
	case <-requestCtx.Done():
		return operation.Cause()
	case <-g.serviceDone():
		return ErrServiceStopped
	case <-timer.C:
		return fmt.Errorf("wait Broadcast session establishment timeout")
	}

	var persistErr error
	streamPublished := false
	if !operation.Deliver(func() {
		key := voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID)
		g.streams.Store(key, session.Stream)
		streamPublished, persistErr = g.commitChannelStreamStart(requestCtx, key, session.Stream)
	}) {
		return operation.Cause()
	}
	if !streamPublished {
		return nil
	}
	if persistErr != nil {
		return fmt.Errorf("persist Broadcast playing state: %w", persistErr)
	}
	return nil
}

func (g *GB28181API) newBroadcastSession(in *VoiceInput) (*broadcastSession, error) {
	return g.newBroadcastSessionContext(context.Background(), in)
}

func (g *GB28181API) newBroadcastSessionContext(ctx context.Context, in *VoiceInput) (*broadcastSession, error) {
	source, err := g.resolveVoiceMediaSourceContext(ctx, in)
	if err != nil {
		return nil, err
	}
	return &broadcastSession{
		DeviceID:            in.Channel.DeviceID,
		ChannelID:           in.Channel.ChannelID,
		SourceID:            source.ID,
		SourceVHost:         source.VHost,
		SourceApp:           source.App,
		SourceStream:        source.Stream,
		SMS:                 in.SMS,
		StandardTalkPlayKey: strings.TrimSpace(in.standardTalkPlayKey),
		Stream: &Streams{
			T: 0, DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
			StreamID: in.Channel.ID, Status: -1,
		},
		CreatedAt: time.Now(),
		ready:     make(chan error, 1),
	}, nil
}

func (g *GB28181API) resolveVoiceMediaSource(in *VoiceInput) (*voiceMediaSource, error) {
	return g.resolveVoiceMediaSourceContext(context.Background(), in)
}

func (g *GB28181API) resolveVoiceMediaSourceContext(ctx context.Context, in *VoiceInput) (*voiceMediaSource, error) {
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
	items, err := getMediaInfoContext(ctx, g.sms, in.SMS, sourceApp, sourceStream)
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
	operation := newPendingDeviceOperation(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	sn, key, pending := g.reservePendingBroadcast(ch.ChannelID, operation)
	defer g.pendingBroadcast.CompareAndDelete(key, pending)
	defer pending.operation.Cancel(nil)
	body, err := sip.XMLEncode(broadcastNotify{
		CmdType:  "Broadcast",
		SN:       sn,
		SourceID: broadcastSourceID(cfg.ID, in.SourceID),
		TargetID: ch.ChannelID,
	})
	if err != nil {
		return err
	}
	requestCtx := pending.operation.Context(ctx)
	tx, err := g.svr.wrapRequestContext(requestCtx, ch, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return pending.operation.ErrorOr(err)
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		return pending.operation.ErrorOr(err)
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case response := <-pending.wait:
		if !strings.EqualFold(strings.TrimSpace(response.Result), "OK") {
			return fmt.Errorf("broadcast rejected: %s", response.Result)
		}
		return nil
	case <-pending.operation.Done():
		return pending.operation.Cause()
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
	if err := validateBroadcastResponseStructure(ctx.Request.Body()); err != nil {
		ctx.String(400, err.Error())
		return
	}
	response.CmdType = strings.TrimSpace(response.CmdType)
	response.DeviceID = strings.TrimSpace(response.DeviceID)
	response.Result = strings.TrimSpace(response.Result)
	if response.XMLName.Local != "Response" || !strings.EqualFold(response.CmdType, "Broadcast") ||
		response.SN <= 0 || !isGBResultValue(response.Result) {
		ctx.String(400, "invalid Broadcast response")
		return
	}
	if !g.getDeviceGBProtocolVersion(ctx.DeviceID).AtLeast(GBVersion11) {
		ctx.String(400, "Broadcast requires GB/T 28181-2014 or later")
		return
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, response.DeviceID); err != nil {
		ctx.String(400, err.Error())
		return
	}
	key := buildPendingBroadcastKey(response.DeviceID, response.SN)
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond Broadcast", "err", err, "sn", response.SN, "target_id", response.DeviceID)
		return
	}
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlockCommit()
	if value, ok := g.pendingBroadcast.Load(key); ok {
		pending := value.(*pendingBroadcastResponse)
		pending.operation.Deliver(func() {
			select {
			case pending.wait <- &response:
			default:
			}
		})
	}
}

func buildPendingBroadcastKey(targetID string, sn int) string {
	return strings.TrimSpace(targetID) + ":" + fmt.Sprintf("%d", sn)
}

func (g *GB28181API) reservePendingBroadcast(targetID string, operation *pendingDeviceOperation) (int, string, *pendingBroadcastResponse) {
	for {
		sn := g.nextControlSN()
		key := buildPendingBroadcastKey(targetID, sn)
		pending := &pendingBroadcastResponse{
			wait:      make(chan *broadcastResponse, 1),
			operation: operation,
		}
		if _, loaded := g.pendingBroadcast.LoadOrStore(key, pending); !loaded {
			return sn, key, pending
		}
	}
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
	if in.Mode != voiceModeTalk && in.Mode != voiceModeTalkStandard && in.Mode != voiceModeBroadcast {
		return fmt.Errorf("invalid voice mode: %s", in.Mode)
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
	stopErr := g.stopVoiceNoLock(ctx, ch, in)
	persistErr := g.persistChannelIdleIfNoActive(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	return errors.Join(stopErr, persistErr)
}

func (g *GB28181API) stopVoiceNoLock(ctx context.Context, ch *Channel, in *StopVoiceInput) error {
	if in.Mode == voiceModeTalkStandard {
		if value, ok := g.broadcastSessions.Load(in.Channel.ChannelID); ok {
			if session, ok := value.(*broadcastSession); ok && session.StandardTalkPlayKey != "" {
				return g.stopBroadcastSession(session, true)
			}
		}
		return g.stopPlayContext(ctx, ch, &StopPlayInput{
			Channel: in.Channel, sessionKey: standardTalkPlayKey(in.Channel.DeviceID, in.Channel.ChannelID),
		})
	}
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
	stream, ok := g.streams.Load(key)
	if !ok {
		return nil
	}
	if stream == nil {
		g.streams.CompareAndDelete(key, nil)
		return nil
	}
	if value, ok := g.talkSessions.Load(stream.StreamID); ok {
		if session, ok := value.(*talkSession); ok && session != nil && session.Stream == stream {
			return g.stopTalkSession(session, nil)
		}
	}
	g.markMediaStreamStopped(stream, "stopped_by_user", false)
	complete, result := g.cleanupMediaStreamContext(ctx, key, stream)
	if result != nil {
		return result
	}
	if !complete {
		return fmt.Errorf("Talk media cleanup remains pending")
	}
	return nil
}

func (g *GB28181API) startTalkRTP(streamID string) error {
	return g.startTalkRTPForMediaServer(streamID, "")
}

func (g *GB28181API) startTalkRTPForMediaServer(streamID, mediaServerID string) error {
	value, ok := g.talkSessions.Load(strings.TrimSpace(streamID))
	if !ok {
		return nil
	}
	session, ok := value.(*talkSession)
	if !ok || session == nil {
		return nil
	}
	if !mediaServerEventMatches(session.SMS, mediaServerID) {
		return nil
	}
	session.mu.Lock()
	if session.stopped || session.rtpStarted || session.startBusy {
		session.mu.Unlock()
		return nil
	}
	session.startBusy = true
	session.mu.Unlock()
	finishStart := func(ssrc string, started bool) bool {
		session.mu.Lock()
		if started {
			session.SSRC = ssrc
			session.rtpStarted = true
		}
		session.startBusy = false
		stopped := session.stopped
		session.mu.Unlock()
		return stopped
	}
	ssrc, releaseSSRC, err := g.reserveSSRC(0)
	if err != nil {
		session.complete(err)
		if finishStart("", false) {
			return errors.Join(err, g.stopTalkSession(session, nil))
		}
		return err
	}
	_, err = startSendRTPTalkContext(g.serviceContext(), g.sms, session.SMS, zlm.StartSendRTPTalkRequest{
		Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream,
		SSRC: ssrc, RecvStreamID: session.ReceiveStream, Type: broadcastRTPTypeES, PT: broadcastPCMAPayload, OnlyAudio: true,
	})
	if err != nil {
		releaseSSRC()
		session.complete(fmt.Errorf("start Talk RTP: %w", err))
		if finishStart("", false) {
			return errors.Join(err, g.stopTalkSession(session, nil))
		}
		return err
	}
	session.mu.Lock()
	session.ssrcRelease = releaseSSRC
	session.mu.Unlock()
	if finishStart(ssrc, true) {
		// 停止可能在媒体服务确认发送成功前到达。先记录已创建资源，再交给统一状态机清理，
		// 这样 StopSendRTP 失败时仍保留会话并由运行态清理器重试。
		return g.stopTalkSession(session, nil)
	}
	session.complete(nil)
	return nil
}

func (g *GB28181API) stopTalkSession(session *talkSession, cause error) (result error) {
	if session == nil {
		return nil
	}
	session.stopMu.Lock()
	defer session.stopMu.Unlock()
	cleanupCtx := g.mediaPersistenceContext()
	stream := session.Stream
	if stream != nil {
		reason := "talk_stopped"
		if cause != nil && strings.TrimSpace(cause.Error()) != "" {
			reason = cause.Error()
		}
		g.markMediaStreamStopped(stream, reason, false)
	}
	session.mu.Lock()
	session.stopped = true
	started := session.rtpStarted
	opened := session.receiverOpened
	ssrc := session.SSRC
	session.mu.Unlock()
	if cause != nil {
		session.complete(cause)
	}
	dialogDone := stream == nil
	if stream != nil {
		stream.cleanupMu.Lock()
		if stream.Resp == nil {
			stream.dialogStopped = true
		} else if !stream.dialogStopped {
			err := g.sendStreamBYEContext(cleanupCtx, stream)
			result = errors.Join(result, err)
			if err == nil {
				stream.dialogStopped = true
			}
		}
		dialogDone = stream.dialogStopped
		stream.cleanupMu.Unlock()
	}
	if started {
		if g.sms == nil || session.SMS == nil {
			result = errors.Join(result, fmt.Errorf("Talk RTP media service is unavailable"))
		} else {
			_, err := stopSendRTPContext(cleanupCtx, g.sms, session.SMS, zlm.StopSendRTPRequest{
				Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc,
			})
			result = errors.Join(result, err)
			if err == nil {
				session.mu.Lock()
				session.rtpStarted = false
				session.mu.Unlock()
			}
		}
	}
	if opened {
		if g.sms == nil || session.SMS == nil {
			result = errors.Join(result, fmt.Errorf("Talk RTP receiver service is unavailable"))
		} else {
			_, err := closeRTPServerContext(cleanupCtx, g.sms, session.SMS, zlm.CloseRTPServerRequest{StreamID: session.ReceiveStream})
			result = errors.Join(result, err)
			if err == nil {
				session.mu.Lock()
				session.receiverOpened = false
				session.mu.Unlock()
			}
		}
	}
	if result != nil {
		return result
	}
	session.mu.Lock()
	complete := !session.rtpStarted && !session.receiverOpened && !session.startBusy && dialogDone
	releaseSendSSRC := session.ssrcRelease
	if !session.rtpStarted && !session.startBusy {
		session.ssrcRelease = nil
	} else {
		releaseSendSSRC = nil
	}
	session.mu.Unlock()
	if releaseSendSSRC != nil {
		releaseSendSSRC()
	}
	if !complete {
		return nil
	}
	g.talkSessions.CompareAndDelete(session.ReceiveStream, session)
	if g.streams != nil && session.Stream != nil {
		g.compareAndDeleteChannelStream(voiceKey(voiceModeTalk, session.DeviceID, session.ChannelID), session.Stream)
	}
	if session.Stream != nil {
		session.Stream.releaseSSRCReservation()
	}
	return g.persistChannelIdleIfNoActive(cleanupCtx, session.DeviceID, session.ChannelID)
}

func (g *GB28181API) stopBroadcastSession(session *broadcastSession, sendBYE bool) (result error) {
	if session == nil {
		return nil
	}
	session.stopMu.Lock()
	defer session.stopMu.Unlock()
	cleanupCtx := g.mediaPersistenceContext()
	session.mu.Lock()
	session.stopped = true
	dialog := session.Dialog
	started := session.rtpStarted
	ssrc := session.SSRC
	if dialog == nil || !sendBYE {
		session.dialogDone = true
	}
	dialogDone := session.dialogDone
	cascade := session.Cascade
	session.mu.Unlock()
	session.complete(fmt.Errorf("Broadcast session stopped"))
	if !dialogDone {
		err := g.sendInboundDialogBYEContext(cleanupCtx, dialog)
		result = errors.Join(result, err)
		if err == nil {
			session.mu.Lock()
			session.dialogDone = true
			session.mu.Unlock()
		}
	}
	if started {
		if g.sms == nil || session.SMS == nil {
			result = errors.Join(result, fmt.Errorf("Broadcast RTP media service is unavailable"))
		} else {
			_, err := stopSendRTPContext(cleanupCtx, g.sms, session.SMS, zlm.StopSendRTPRequest{
				Vhost: session.SourceVHost, App: session.SourceApp, Stream: session.SourceStream, SSRC: ssrc,
			})
			result = errors.Join(result, err)
			if err == nil {
				session.mu.Lock()
				session.rtpStarted = false
				session.mu.Unlock()
			}
		}
	}
	if cascade != nil {
		err := g.stopCascadeVoiceSource(cascade, true)
		result = errors.Join(result, err)
		if err == nil {
			session.mu.Lock()
			session.Cascade = nil
			session.mu.Unlock()
		}
	}
	playKey := strings.TrimSpace(session.StandardTalkPlayKey)
	if playKey != "" {
		playHandled := false
		if g.svr != nil && g.svr.memoryStorer != nil {
			if ch, ok := g.svr.memoryStorer.GetChannel(session.DeviceID, session.ChannelID); ok {
				playHandled = true
				result = errors.Join(result, g.stopPlayContext(g.mediaPersistenceContext(), ch, &StopPlayInput{
					Channel: &ipc.Channel{DeviceID: session.DeviceID, ChannelID: session.ChannelID}, sessionKey: playKey, skipLinkedVoice: true,
				}))
			}
		}
		if !playHandled {
			stream, ok := g.streams.Load(playKey)
			if ok && stream != nil {
				stream.cleanupMu.Lock()
				stream.dialogStopped = true
				if stream.mediaServer == nil || strings.TrimSpace(stream.StreamID) == "" {
					stream.rtpClosed = true
				} else if g.sms == nil {
					result = errors.Join(result, fmt.Errorf("standard Talk RTP media service is unavailable"))
				} else if !stream.rtpClosed {
					_, err := closeRTPServerContext(cleanupCtx, g.sms, stream.mediaServer, zlm.CloseRTPServerRequest{StreamID: stream.StreamID})
					result = errors.Join(result, err)
					if err == nil {
						stream.rtpClosed = true
					}
				}
				playComplete := stream.dialogStopped && stream.rtpClosed
				stream.cleanupMu.Unlock()
				if playComplete {
					g.compareAndDeleteChannelStream(playKey, stream)
					stream.releaseSSRCReservation()
				}
			}
		}
	}
	if result != nil {
		return result
	}
	session.mu.Lock()
	complete := session.dialogDone && !session.rtpStarted && !session.inviteBusy && session.Cascade == nil
	session.mu.Unlock()
	if playKey != "" {
		if _, exists := g.streams.Load(playKey); exists {
			complete = false
		}
	}
	if !complete {
		return nil
	}
	if dialog != nil {
		g.inviteDialogs.CompareAndDelete(dialog.CallID, dialog)
	}
	g.broadcastSessions.CompareAndDelete(session.ChannelID, session)
	if session.Stream != nil {
		if g.streams != nil {
			g.compareAndDeleteChannelStream(voiceKey(voiceModeBroadcast, session.DeviceID, session.ChannelID), session.Stream)
		}
		session.Stream.releaseSSRCReservation()
	}
	return g.persistChannelIdleIfNoActive(cleanupCtx, session.DeviceID, session.ChannelID)
}

// cleanupStoppedVoiceSessions 只重试已经进入终止态的语音对象，避免后台清理误伤活动会话。
// 返回 true 表示仍有对象保留在运行时索引中，需要后续继续重试。
func (g *GB28181API) cleanupStoppedVoiceSessions() (pending bool) {
	if g == nil {
		return false
	}
	g.talkSessions.Range(func(key, value any) bool {
		session, ok := value.(*talkSession)
		if !ok || session == nil {
			g.talkSessions.CompareAndDelete(key, value)
			return true
		}
		session.mu.Lock()
		stopped := session.stopped
		session.mu.Unlock()
		if !stopped {
			return true
		}
		_ = g.stopTalkSession(session, nil)
		if current, exists := g.talkSessions.Load(key); exists && current == session {
			pending = true
		}
		return true
	})
	g.broadcastSessions.Range(func(key, value any) bool {
		session, ok := value.(*broadcastSession)
		if !ok || session == nil {
			g.broadcastSessions.CompareAndDelete(key, value)
			return true
		}
		session.mu.Lock()
		stopped := session.stopped
		session.mu.Unlock()
		if !stopped {
			return true
		}
		_ = g.stopBroadcastSession(session, true)
		if current, exists := g.broadcastSessions.Load(key); exists && current == session {
			pending = true
		}
		return true
	})
	g.cascadeVoiceDialogs.Range(func(key, value any) bool {
		source, ok := value.(*cascadeVoiceSourceSession)
		if !ok || source == nil {
			g.cascadeVoiceDialogs.CompareAndDelete(key, value)
			return true
		}
		source.mu.Lock()
		stopping := source.stopping
		session := source.broadcast
		source.mu.Unlock()
		if !stopping {
			return true
		}
		if session != nil {
			if current, exists := g.broadcastSessions.Load(session.ChannelID); exists && current == session {
				// 关联的广播会话由上面的扫描统一重试，避免同一轮重复产生媒体副作用。
				pending = true
				return true
			}
			_ = g.stopBroadcastSession(session, true)
		} else {
			_ = g.stopCascadeVoiceSource(source, true)
		}
		if current, exists := g.cascadeVoiceDialogs.Load(key); exists && current == source {
			pending = true
		}
		return true
	})
	g.pendingCascadeVoiceCleanups.Range(func(key, value any) bool {
		source, keyOK := key.(*cascadeVoiceSourceSession)
		current, valueOK := value.(*cascadeVoiceSourceSession)
		if !keyOK || !valueOK || source == nil || current != source {
			g.pendingCascadeVoiceCleanups.CompareAndDelete(key, value)
			return true
		}
		source.mu.Lock()
		stopping := source.stopping
		callID := source.callID
		source.mu.Unlock()
		if !stopping {
			return true
		}
		if callID != "" {
			if active, exists := g.cascadeVoiceDialogs.Load(callID); exists && active == source {
				// 活动索引已经在上一轮扫描中负责重试，避免同一轮重复媒体副作用。
				pending = true
				return true
			}
		}
		_ = g.stopCascadeVoiceSource(source, true)
		if retained, exists := g.pendingCascadeVoiceCleanups.Load(source); exists && retained == source {
			pending = true
		}
		return true
	})
	return pending
}

func (g *GB28181API) pendingVoiceCleanupOwnsStream(key string, stream *Streams) bool {
	if g == nil || stream == nil {
		return false
	}
	if key == voiceKey(voiceModeTalk, stream.DeviceID, stream.ChannelID) {
		if value, ok := g.talkSessions.Load(stream.StreamID); ok {
			session, _ := value.(*talkSession)
			if session != nil && session.Stream == stream {
				session.mu.Lock()
				stopped := session.stopped
				session.mu.Unlock()
				return stopped
			}
		}
	}
	if value, ok := g.broadcastSessions.Load(stream.ChannelID); ok {
		session, _ := value.(*broadcastSession)
		if session != nil {
			session.mu.Lock()
			stopped := session.stopped
			ownsStream := session.Stream == stream || strings.TrimSpace(session.StandardTalkPlayKey) == key
			session.mu.Unlock()
			return stopped && ownsStream
		}
	}
	return false
}

func (g *GB28181API) retryStoppedVoiceSessions(ctx context.Context, interval time.Duration) error {
	if g == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = voiceShutdownRetryInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if !g.cleanupStoppedVoiceSessions() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (g *GB28181API) stopStandardTalkForPlayKey(playKey string) {
	playKey = strings.TrimSpace(playKey)
	if g == nil || playKey == "" {
		return
	}
	g.broadcastSessions.Range(func(_, value any) bool {
		session, ok := value.(*broadcastSession)
		if !ok || session == nil || session.StandardTalkPlayKey != playKey {
			return true
		}
		_ = g.stopBroadcastSession(session, true)
		return false
	})
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

func (g *GB28181API) findBroadcastSessionForInvite(deviceID string, request *sip.Request) (*broadcastSession, error) {
	subject, err := optionalGBInviteSubject(request)
	if err != nil {
		return g.findBroadcastSession(deviceID), err
	}
	if subject != nil {
		value, ok := g.broadcastSessions.Load(subject.ReceiverID)
		if !ok {
			return nil, nil
		}
		session, _ := value.(*broadcastSession)
		if session == nil || strings.TrimSpace(session.DeviceID) != strings.TrimSpace(deviceID) {
			return nil, nil
		}
		if err := validateGBInviteSubject(subject, session.SourceID, session.ChannelID, 0); err != nil {
			return session, err
		}
		return session, nil
	}
	session := g.findBroadcastSession(deviceID)
	if session != nil && session.Version.AtLeast(GBVersion11) {
		return session, fmt.Errorf("Subject header is required")
	}
	return session, nil
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
		formatName, mappingErr := sdpPayloadFormat(media, value)
		if mappingErr != nil {
			return 0, "", 0, fmt.Errorf("invalid Broadcast SDP: %w", mappingErr)
		}
		formatName = strings.ToUpper(strings.TrimSpace(formatName))
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

func (g *GB28181API) sipInviteVoice(ctx context.Context, ch *Channel, in *VoiceInput, port int, ssrc string, stream *Streams) error {
	cfg := g.configSnapshot()
	if cfg == nil {
		return fmt.Errorf("SIP configuration is unavailable")
	}
	ipAddress, err := GetIP(in.SMS.GetSDPIP())
	if err != nil {
		return err
	}
	body, err := buildVoiceSDP(ch.ChannelID, ipAddress, port, in.StreamMode, ssrc)
	if err != nil {
		return err
	}
	tx, err := g.svr.wrapRequestContext(ctx, ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(ch.ChannelID, ssrc, cfg.ID)})
	})
	if err != nil {
		return err
	}
	resp, err := sipInviteResponseContext(ctx, tx)
	if err != nil {
		return err
	}
	stream.Resp = resp
	stream.T = 0
	stream.DeviceID = in.Channel.DeviceID
	stream.ChannelID = in.Channel.ChannelID
	stream.StreamID = in.Channel.ID
	stream.Status = 0
	if callID, ok := resp.CallID(); ok {
		stream.CallID = normalizeCallID(callID)
	}
	return g.ackAndConnectActiveRTP(ctx, tx, resp, in.SMS, stream.StreamID, in.StreamMode, "audio", ssrc, g.getDeviceGBProtocolVersion(in.Channel.DeviceID), "8", "0", "9")
}

func buildVoiceSDP(channelID, ipAddress string, port int, streamMode int8, ssrc string) ([]byte, error) {
	if streamMode < 0 || streamMode > 2 {
		return nil, fmt.Errorf("invalid RTP stream mode: %d", streamMode)
	}
	protocol := "TCP/RTP/AVP"
	if streamMode == 0 {
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
	if streamMode == 1 {
		audio.AddAttribute("setup", "passive")
		audio.AddAttribute("connection", "new")
	}
	if streamMode == 2 {
		audio.AddAttribute("setup", "active")
		audio.AddAttribute("connection", "new")
	}
	audio.AddAttribute("sendrecv")
	audio.AddAttribute("rtpmap", "8", "PCMA/8000")
	audio.AddAttribute("rtpmap", "0", "PCMU/8000")
	audio.AddAttribute("rtpmap", "9", "G722/8000")

	address, err := parseSDPAddress(ipAddress)
	if err != nil {
		return nil, err
	}
	msg := &sdp.Message{
		Origin: sdp.Origin{
			Username:    channelID,
			NetworkType: "IN",
			AddressType: address.Type,
			Address:     address.Canonical,
		},
		Name: historyModePlay,
		Connection: sdp.ConnectionData{
			NetworkType: "IN",
			AddressType: address.Type,
			IP:          address.IP,
		},
		Timing: []sdp.Timing{{}},
		Medias: []sdp.Media{audio},
		SSRC:   ssrc,
	}
	body := msg.Append(nil).AppendTo(nil)
	return append(body, "f=v/////a/1/8/1\r\n"...), nil
}
