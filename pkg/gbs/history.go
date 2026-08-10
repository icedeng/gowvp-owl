package gbs

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
)

const (
	historyModePlayback = "Playback"
	historyModeDownload = "Download"

	historyTransportRTP       = "rtp"
	historyTransportDirectTCP = "direct_tcp"
)

// HistoryInput 历史回放/下载参数。
type HistoryInput struct {
	Channel    *ipc.Channel
	SMS        *sms.MediaServer
	StreamMode int8
	StartAt    time.Time
	EndAt      time.Time
	Mode       string // Playback 或 Download
	Transport  string // rtp 或 direct_tcp；空值保持原 RTP 行为
}

type StopHistoryInput struct {
	Channel *ipc.Channel
	Mode    string
}

type ControlHistoryInput struct {
	Channel *ipc.Channel
	Mode    string
	Cmd     string  // MANSRTSP 控制文本（优先）
	Action  string  // 结构化控制动作：play/pause/speed/seek
	Scale   float64 // speed 动作速度倍率
	SeekAt  int64   // seek 动作目标时间（unix 秒）
}

func historyKey(mode, deviceID, channelID string) string {
	return "history:" + mode + ":" + deviceID + ":" + channelID
}

// StartHistory 启动历史回放或文件下载会话（9.8/9.9）。
func (g *GB28181API) StartHistory(ctx context.Context, in *HistoryInput) error {
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid history input")
	}
	if in.StartAt.IsZero() || in.EndAt.IsZero() || !in.EndAt.After(in.StartAt) {
		return fmt.Errorf("invalid history range")
	}
	if in.Mode != historyModePlayback && in.Mode != historyModeDownload {
		return fmt.Errorf("invalid history mode: %s", in.Mode)
	}

	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return ErrChannelNotExist
	}
	if !ch.device.IsOnline {
		return ErrDeviceOffline
	}
	transport := strings.ToLower(strings.TrimSpace(in.Transport))
	if transport == "" {
		transport = historyTransportRTP
	}
	if transport == historyTransportDirectTCP {
		return g.startDirectTCPHistory(ctx, ch, in)
	}
	if transport != historyTransportRTP {
		return fmt.Errorf("invalid history transport: %s", in.Transport)
	}
	if err := g.requireMediaTransport(in.Channel.DeviceID, in.StreamMode, "历史视音频"+in.Mode); err != nil {
		return err
	}

	ch.device.playMutex.Lock()
	defer ch.device.playMutex.Unlock()

	key := historyKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	stream, existed := g.streams.LoadOrStore(key, &Streams{})
	if existed {
		_ = g.stopHistoryNoLock(ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode})
		stream = &Streams{}
		g.streams.Store(key, stream)
	}
	stream.DeviceID = in.Channel.DeviceID
	stream.ChannelID = in.Channel.ChannelID
	stream.StreamID = in.Channel.ID

	resp, err := g.sms.OpenRTPServer(in.SMS, zlm.OpenRTPServerRequest{
		TCPMode:  in.StreamMode,
		StreamID: in.Channel.ID,
	})
	if err != nil {
		return err
	}

	if err := g.sipInviteHistory(ch, in, resp.Port, stream); err != nil {
		return err
	}
	// 历史播放/下载属于播放态，复用播放状态字段。
	_ = g.svr.gb.core.EditPlaying(ctx, in.Channel.DeviceID, in.Channel.ChannelID, true)
	return nil
}

// StopHistory 停止历史回放或下载会话。
func (g *GB28181API) StopHistory(ctx context.Context, in *StopHistoryInput) error {
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid stop history input")
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return ErrChannelNotExist
	}
	ch.device.playMutex.Lock()
	defer ch.device.playMutex.Unlock()
	defer func() {
		_ = g.svr.gb.core.EditPlaying(ctx, in.Channel.DeviceID, in.Channel.ChannelID, false)
	}()
	return g.stopHistoryNoLock(ch, in)
}

// ControlHistory 通过 INFO 下发历史会话控制命令（9.8/9.9）。
func (g *GB28181API) ControlHistory(_ context.Context, in *ControlHistoryInput) error {
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid control history input")
	}
	if in.Mode != historyModePlayback && in.Mode != historyModeDownload {
		return fmt.Errorf("invalid history mode: %s", in.Mode)
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return ErrChannelNotExist
	}
	key := historyKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	stream, ok := g.streams.Load(key)
	if !ok || stream.Resp == nil {
		return fmt.Errorf("history session not found")
	}
	cmd, err := g.buildHistoryControlCmd(stream, in)
	if err != nil {
		return err
	}
	req := sip.NewRequestFromResponse(sip.MethodInfo, stream.Resp)
	req.SetBody([]byte(cmd), true)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	req.SetDestination(ch.Source())
	req.SetConnection(ch.Conn())
	tx, err := g.svr.Request(req)
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}

// buildHistoryControlCmd 将结构化控制参数转换为 MANSRTSP 文本。
func (g *GB28181API) buildHistoryControlCmd(stream *Streams, in *ControlHistoryInput) (string, error) {
	if strings.TrimSpace(in.Cmd) != "" {
		return in.Cmd, nil
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "" {
		return "", fmt.Errorf("history control requires cmd or action")
	}
	stream.CseqNo++
	cseq := stream.CseqNo
	switch action {
	case "play", "resume":
		return fmt.Sprintf("PLAY MANSRTSP/1.0\r\nCSeq: %d\r\n\r\n", cseq), nil
	case "pause":
		return fmt.Sprintf("PAUSE MANSRTSP/1.0\r\nCSeq: %d\r\n\r\n", cseq), nil
	case "speed":
		if in.Scale == 0 {
			return "", fmt.Errorf("history speed action requires scale")
		}
		return fmt.Sprintf("PLAY MANSRTSP/1.0\r\nCSeq: %d\r\nScale: %.2f\r\n\r\n", cseq, in.Scale), nil
	case "seek":
		if in.SeekAt <= 0 {
			return "", fmt.Errorf("history seek action requires seek_at")
		}
		seek := time.Unix(in.SeekAt, 0).In(time.Local).Format("20060102T150405")
		return fmt.Sprintf("PLAY MANSRTSP/1.0\r\nCSeq: %d\r\nRange: clock=%s-\r\n\r\n", cseq, seek), nil
	default:
		return "", fmt.Errorf("unsupported history action: %s", action)
	}
}

func (g *GB28181API) stopHistoryNoLock(ch *Channel, in *StopHistoryInput) error {
	key := historyKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	stream, ok := g.streams.Load(key)
	if !ok || stream.Resp == nil {
		return nil
	}
	if stream.DirectTCP && g.directDownloads != nil && g.directDownloads.Cancel(stream.DirectSessionID) {
		return nil
	}
	if !g.streams.CompareAndDelete(key, stream) {
		return nil
	}
	return g.sendHistoryBYE(ch, stream)
}

func (g *GB28181API) sendHistoryBYE(ch *Channel, stream *Streams) error {
	if ch == nil || stream == nil || stream.Resp == nil {
		return nil
	}
	req := sip.NewRequestFromResponse(sip.MethodBYE, stream.Resp)
	req.SetDestination(ch.Source())
	req.SetConnection(ch.Conn())
	tx, err := g.svr.Request(req)
	if err != nil {
		return err
	}
	_, err = sipResponse(tx)
	return err
}

func (g *GB28181API) startDirectTCPHistory(ctx context.Context, ch *Channel, in *HistoryInput) error {
	if in.Mode != historyModeDownload {
		return fmt.Errorf("direct TCP transport is only valid for Download")
	}
	if err := g.requireGBFeature(in.Channel.DeviceID, "2014 直接 TCP 文件下载", func(c GBCapabilities) bool {
		return c.DirectTCPDownload
	}); err != nil {
		return err
	}
	policy := g.directTCPPolicySnapshot()
	if !policy.Enabled {
		return fmt.Errorf("2014 直接 TCP 文件下载未启用")
	}
	if _, allowed := policy.Allowlist[in.Channel.DeviceID]; !allowed {
		return fmt.Errorf("设备 %s 未加入直接 TCP 下载白名单", in.Channel.DeviceID)
	}
	if g.directDownloads == nil {
		return fmt.Errorf("direct TCP download manager is unavailable")
	}

	ch.device.playMutex.Lock()
	defer ch.device.playMutex.Unlock()
	key := historyKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	if existing, existed := g.streams.Load(key); existed {
		_ = g.stopHistoryNoLock(ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode})
		if existing.DirectTCP && existing.DirectSessionID != "" {
			waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_, waitErr := g.directDownloads.Wait(waitCtx, existing.DirectSessionID)
			cancel()
			if waitErr != nil {
				return fmt.Errorf("wait previous direct TCP download to stop: %w", waitErr)
			}
		}
	}
	stream := &Streams{
		T:         1,
		DeviceID:  in.Channel.DeviceID,
		ChannelID: in.Channel.ChannelID,
		StreamID:  in.Channel.ID,
		DirectTCP: true,
	}
	g.streams.Store(key, stream)

	offer, err := g.sipInviteDirectTCPHistory(ch, in, stream, policy.OfferPort)
	if err != nil {
		g.streams.Delete(key)
		return err
	}
	registeredIP := addressIP(ch.Source())
	managerCtx := context.WithoutCancel(ctx)
	err = g.directDownloads.Start(managerCtx, DirectTCPDownloadRequest{
		SessionID:     stream.DirectSessionID,
		DeviceID:      in.Channel.DeviceID,
		ChannelID:     in.Channel.ChannelID,
		Address:       offer.Address,
		RegisteredIP:  registeredIP,
		FileSize:      offer.FileSize,
		FileSizeKnown: offer.FileSizeKnown,
		OnFinish: func(state DirectTCPDownloadState) {
			g.finishDirectTCPHistory(key, ch, stream, state)
		},
	})
	if err != nil {
		g.streams.Delete(key)
		_ = g.sendHistoryBYE(ch, stream)
		return err
	}
	g.metrics.directStarted.Add(1)
	if g.core.Store() != nil {
		_ = g.svr.gb.core.EditPlaying(ctx, in.Channel.DeviceID, in.Channel.ChannelID, true)
	}
	return nil
}

func (g *GB28181API) sipInviteDirectTCPHistory(ch *Channel, in *HistoryInput, stream *Streams, port int) (directTCPDownloadOffer, error) {
	ip4str, err := GetIP(g.boot.Media.SDPIP)
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	ssrc := g.getSSRC(1)
	body, err := buildGBSDP(gbSDPInput{
		Version:     g.getDeviceGBProtocolVersion(in.Channel.DeviceID),
		SessionName: historyModeDownload,
		ChannelID:   ch.ChannelID,
		URI:         fmt.Sprintf("%s:3", ch.ChannelID),
		IP:          ip4str,
		Port:        port,
		StartAt:     in.StartAt,
		EndAt:       in.EndAt,
		SSRC:        ssrc,
		DirectTCP:   true,
	})
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	tx, err := g.svr.wrapRequest(ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(ch.ChannelID, ssrc, g.cfg.ID)})
	})
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	resp, err := sipResponse(tx)
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	offer, err := parseDirectTCPDownloadSDP(resp.Body())
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	if contact, _ := resp.Contact(); contact == nil {
		resp.AppendHeader(&sip.ContactHeader{
			DisplayName: g.svr.fromAddress.DisplayName,
			Address:     &sip.URI{FUser: sip.String{Str: g.cfg.ID}, FHost: g.cfg.Domain},
			Params:      sip.NewParams(),
		})
	}
	stream.Resp = resp
	stream.ssrc = ssrc
	stream.Status = 0
	stream.Stop = false
	if callID, ok := resp.CallID(); ok {
		stream.CallID = normalizeCallID(callID)
		stream.DirectSessionID = stream.CallID
	}
	if stream.DirectSessionID == "" {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP download response missing Call-ID")
	}
	if err := tx.Request(sip.NewRequestFromResponse(sip.MethodACK, resp)); err != nil {
		return directTCPDownloadOffer{}, err
	}
	return offer, nil
}

func (g *GB28181API) finishDirectTCPHistory(key string, ch *Channel, stream *Streams, state DirectTCPDownloadState) {
	switch state.Status {
	case directTCPStatusCompleted:
		g.metrics.directCompleted.Add(1)
		if state.Received > 0 {
			g.metrics.directBytes.Add(uint64(state.Received))
		}
	case directTCPStatusCancelled:
		g.metrics.directCancelled.Add(1)
	default:
		g.metrics.directFailed.Add(1)
	}
	if !g.streams.CompareAndDelete(key, stream) {
		return
	}
	stream.Status = 1
	stream.Stop = true
	stream.EndReason = state.EndReason
	_ = g.sendHistoryBYE(ch, stream)
	if g.core.Store() != nil {
		_ = g.core.EditPlaying(context.Background(), stream.DeviceID, stream.ChannelID, false)
	}
}

func addressIP(address net.Addr) net.IP {
	switch value := address.(type) {
	case *net.UDPAddr:
		return value.IP
	case *net.TCPAddr:
		return value.IP
	}
	if address == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(address.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(strings.Trim(host, "[]"))
}

func (g *GB28181API) sipInviteHistory(ch *Channel, in *HistoryInput, port int, stream *Streams) error {
	ip4str, err := GetIP(in.SMS.GetSDPIP())
	if err != nil {
		return err
	}
	ssrc := g.getSSRC(1)
	body, err := buildGBSDP(gbSDPInput{
		Version:     g.getDeviceGBProtocolVersion(in.Channel.DeviceID),
		SessionName: in.Mode,
		ChannelID:   ch.ChannelID,
		URI:         fmt.Sprintf("%s:0", ch.ChannelID),
		IP:          ip4str,
		Port:        port,
		StreamMode:  in.StreamMode,
		StartAt:     in.StartAt,
		EndAt:       in.EndAt,
		SSRC:        ssrc,
	})
	if err != nil {
		return err
	}
	tx, err := g.svr.wrapRequest(ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(ch.ChannelID, ssrc, g.cfg.ID)})
	})
	if err != nil {
		return err
	}
	resp, err := sipResponse(tx)
	if err != nil {
		return err
	}
	if contact, _ := resp.Contact(); contact == nil {
		resp.AppendHeader(&sip.ContactHeader{
			DisplayName: g.svr.fromAddress.DisplayName,
			Address:     &sip.URI{FUser: sip.String{Str: g.cfg.ID}, FHost: g.cfg.Domain},
			Params:      sip.NewParams(),
		})
	}
	stream.Resp = resp
	stream.T = 1
	stream.DeviceID = in.Channel.DeviceID
	stream.ChannelID = in.Channel.ChannelID
	stream.StreamID = in.Channel.ID
	stream.Status = 0
	stream.Stop = false
	stream.ssrc = ssrc
	if callID, ok := resp.CallID(); ok {
		stream.CallID = normalizeCallID(callID)
	}
	return tx.Request(sip.NewRequestFromResponse(sip.MethodACK, resp))
}
