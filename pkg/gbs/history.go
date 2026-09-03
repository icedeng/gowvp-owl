package gbs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	Channel            *ipc.Channel
	SMS                *sms.MediaServer
	ResolveMediaServer MediaServerResolver
	StreamMode         int8
	StartAt            time.Time
	EndAt              time.Time
	Mode               string // Playback 或 Download
	Transport          string // rtp 或 direct_tcp；空值保持原 RTP 行为
	// DownloadSpeed 是 2014 附录 F 定义的整数下载倍速，0 表示不携带、由设备按 1 倍速处理。
	DownloadSpeed int
	// RecordType 是 SDP u 字段的录像/下载类型：0=all、1=manual、2=alarm、3=time。
	// nil 保持历史 API 行为，按明确时间段请求 time 类型。
	RecordType *int
	// sessionKey/streamID 仅供平台级联创建相互隔离的历史媒体会话；普通 API 保持原有按通道单会话行为。
	sessionKey    string
	streamID      string
	preferredPath string
	routeResponse *sip.Response
}

type StopHistoryInput struct {
	Channel *ipc.Channel
	Mode    string
	// sessionKey 为空时使用历史兼容键。
	sessionKey string
}

type ControlHistoryInput struct {
	Channel *ipc.Channel
	Mode    string
	Cmd     string  // MANSRTSP 控制文本（优先）
	Action  string  // 结构化控制动作：play/pause/speed/seek
	Scale   float64 // speed 动作速度倍率
	SeekAt  int64   // seek 动作目标时间（unix 秒）
	// sessionKey 为空时使用历史兼容键。
	sessionKey string
}

// historyControlState 保存历史会话最近一次成功生效的倍率。2022 B.2.8 规定
// PLAY 仅携带 Range 时沿用前端上次记录的 Scale；零值表示初始正常倍率 1。
type historyControlState struct {
	mu    sync.RWMutex
	scale float64
}

func (state *historyControlState) effectiveScale(request *cascadeMANSRTSPRequest) float64 {
	if request != nil && request.hasScale {
		return request.scale
	}
	if state == nil {
		return 1
	}
	state.mu.RLock()
	scale := state.scale
	state.mu.RUnlock()
	if scale == 0 {
		return 1
	}
	return scale
}

func (state *historyControlState) commit(request *cascadeMANSRTSPRequest, response *historyControlResponse) {
	if state == nil {
		return
	}
	scale := 0.0
	if response != nil && response.hasScale {
		scale = response.scale
	} else if request != nil && request.hasScale {
		scale = request.scale
	}
	if scale == 0 {
		return
	}
	state.mu.Lock()
	state.scale = scale
	state.mu.Unlock()
}

func (state *historyControlState) commitResult(request *cascadeMANSRTSPRequest, response *historyControlResponse, err error) {
	if err == nil {
		state.commit(request, response)
	}
}

func historyKey(mode, deviceID, channelID string) string {
	return "history:" + mode + ":" + deviceID + ":" + channelID
}

func resolveHistorySessionKey(mode, deviceID, channelID, sessionKey string) string {
	if key := strings.TrimSpace(sessionKey); key != "" {
		return key
	}
	return historyKey(mode, deviceID, channelID)
}

func historyURI(channelID string, recordType *int) (string, error) {
	typeValue := 3
	if recordType != nil {
		typeValue = *recordType
	}
	if typeValue < 0 || typeValue > 3 {
		return "", fmt.Errorf("invalid history record type: %d", typeValue)
	}
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return "", fmt.Errorf("history channel ID is required")
	}
	return fmt.Sprintf("%s:%d", channelID, typeValue), nil
}

// StartHistory 启动历史回放或文件下载会话（9.8/9.9）。
func (g *GB28181API) StartHistory(ctx context.Context, in *HistoryInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid history input")
	}
	if in.StartAt.IsZero() || in.EndAt.IsZero() || !in.EndAt.After(in.StartAt) {
		return fmt.Errorf("invalid history range")
	}
	if in.Mode != historyModePlayback && in.Mode != historyModeDownload {
		return fmt.Errorf("invalid history mode: %s", in.Mode)
	}
	if _, err := historyURI(in.Channel.ChannelID, in.RecordType); err != nil {
		return err
	}
	advertisedDownloadSpeeds := ""
	if in.Channel.Ext.GBCatalog != nil {
		advertisedDownloadSpeeds = in.Channel.Ext.GBCatalog.DownloadSpeed
	}
	if err := g.requireHistoryDownloadSpeed(in.Channel.DeviceID, in.DownloadSpeed, advertisedDownloadSpeeds); err != nil {
		return err
	}

	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return ErrChannelNotExist
	}
	if !ch.device.IsOnlineNow() {
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

	key := resolveHistorySessionKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID, in.sessionKey)
	var (
		stream  *Streams
		existed bool
	)
	if !operation.Deliver(func() {
		stream, existed = g.streams.LoadOrStore(key, &Streams{})
	}) {
		return operation.Cause()
	}
	if existed {
		if err := g.stopHistoryNoLockContext(requestCtx, ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode, sessionKey: key}); err != nil {
			return operation.ErrorOr(fmt.Errorf("cleanup previous history session: %w", err))
		}
		stream = &Streams{}
		if !operation.Deliver(func() {
			stream, existed = g.streams.LoadOrStore(key, stream)
		}) {
			return operation.Cause()
		}
		if existed {
			return operation.ErrorOr(fmt.Errorf("history session was replaced concurrently"))
		}
	}
	streamID := strings.TrimSpace(in.streamID)
	if streamID == "" {
		streamID = in.Channel.ID
	}
	stream.DeviceID = in.Channel.DeviceID
	stream.ChannelID = in.Channel.ChannelID
	stream.StreamID = streamID
	stream.mediaServer = in.SMS
	stream.sessionKey = key
	stream.S = in.StartAt
	stream.E = in.EndAt
	// SSRC 必须在打开 RTP 端口前生成并绑定，避免其他设备或会话向该端口串流。
	ssrc, releaseSSRC, err := g.reserveSSRC(1)
	if err != nil {
		g.compareAndDeleteChannelStream(key, stream)
		return operation.ErrorOr(err)
	}
	if err := stream.bindSSRCReservation(ssrc, releaseSSRC); err != nil {
		g.compareAndDeleteChannelStream(key, stream)
		return operation.ErrorOr(err)
	}
	ssrcValue, err := strconv.ParseUint(ssrc, 10, 64)
	if err != nil {
		g.compareAndDeleteChannelStream(key, stream)
		stream.releaseSSRCReservation()
		return operation.ErrorOr(fmt.Errorf("invalid GB28181 SSRC %q: %w", ssrc, err))
	}

	resp, err := openRTPServerContext(requestCtx, g.sms, in.SMS, zlm.OpenRTPServerRequest{
		TCPMode:  in.StreamMode,
		StreamID: streamID,
		SSRC:     ssrcValue,
		TCPRTCP:  shouldEnableTCPRTCP(g.getDeviceGBProtocolVersion(in.Channel.DeviceID), in.StreamMode != 0),
	})
	if err != nil {
		g.compareAndDeleteChannelStream(key, stream)
		stream.releaseSSRCReservation()
		return err
	}

	if err := g.sipInviteHistory(requestCtx, ch, in, resp.Port, ssrc, stream); err != nil {
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_failed")
		return errors.Join(operation.ErrorOr(err), cleanupErr)
	}
	var persistErr error
	var downloadStateErr error
	streamPublished := false
	if !operation.Deliver(func() {
		if in.Mode == historyModeDownload {
			downloadStateErr = g.registerRTPDownload(stream)
			if downloadStateErr != nil {
				return
			}
		}
		// 历史播放/下载属于播放态，复用播放状态字段。
		streamPublished, persistErr = g.commitChannelStreamStart(requestCtx, key, stream)
	}) {
		cleanupErr := g.stopHistoryNoLock(ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode, sessionKey: key})
		idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
		return errors.Join(operation.Cause(), cleanupErr, idleErr)
	}
	if downloadStateErr != nil {
		cleanupErr := g.stopHistoryNoLock(ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode, sessionKey: key})
		idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
		return errors.Join(fmt.Errorf("persist RTP download active marker: %w", downloadStateErr), cleanupErr, idleErr)
	}
	if !streamPublished {
		return nil
	}
	if persistErr != nil {
		cleanupErr := g.stopHistoryNoLock(ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode, sessionKey: key})
		idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
		return errors.Join(fmt.Errorf("persist history playing state: %w", persistErr), cleanupErr, idleErr)
	}
	return nil
}

func (g *GB28181API) requireHistoryDownloadSpeed(deviceID string, speed int, advertised ...string) error {
	if speed < 0 {
		return fmt.Errorf("下载倍速不能为负数: %d", speed)
	}
	if speed == 0 {
		return nil
	}
	if err := g.requireGBFeature(deviceID, "download_speed", "历史视音频下载倍速", func(c GBCapabilities) bool {
		return c.DownloadSpeed
	}); err != nil {
		return err
	}
	if len(advertised) == 0 || strings.TrimSpace(advertised[0]) == "" {
		return nil
	}
	values := strings.Split(advertised[0], "/")
	matched := false
	for _, value := range values {
		candidate, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || candidate <= 0 {
			return fmt.Errorf("设备上报了非法下载倍速列表 %q", advertised[0])
		}
		if candidate == speed {
			matched = true
		}
	}
	if matched {
		return nil
	}
	return fmt.Errorf("下载倍速 %d 不在设备支持列表 %q 中", speed, advertised[0])
}

// StopHistory 停止历史回放或下载会话。
func (g *GB28181API) StopHistory(ctx context.Context, in *StopHistoryInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid stop history input")
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
	err = g.stopHistoryNoLockContext(ctx, ch, in)
	persistErr := g.persistChannelIdleIfNoActive(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	return errors.Join(err, persistErr)
}

// ControlHistory 通过 INFO 下发历史会话控制命令（9.8/9.9）。
func (g *GB28181API) ControlHistory(ctx context.Context, in *ControlHistoryInput) error {
	_, err := g.controlHistory(ctx, in)
	return err
}

// controlHistory 下发历史控制并解析设备返回的 MANSRTSP/RTSP 业务应答。
// 空业务正文保留存量设备兼容；一旦携带正文，就必须与本次请求的 CSeq 关联且返回业务 200。
func (g *GB28181API) controlHistory(ctx context.Context, in *ControlHistoryInput) (*historyControlResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil || in.Channel == nil {
		return nil, fmt.Errorf("invalid control history input")
	}
	if in.Mode != historyModePlayback && in.Mode != historyModeDownload {
		return nil, fmt.Errorf("invalid history mode: %s", in.Mode)
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return nil, ErrChannelNotExist
	}
	key := resolveHistorySessionKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID, in.sessionKey)
	stream, ok := g.streams.Load(key)
	if !ok || stream.Resp == nil || g.mediaStreamStopping(stream) {
		return nil, fmt.Errorf("history session not found")
	}
	stream.historyControlMu.Lock()
	defer stream.historyControlMu.Unlock()
	cmd, err := g.buildHistoryControlCmd(stream, in)
	if err != nil {
		return nil, err
	}
	command, err := parseCascadeMANSRTSP([]byte(cmd))
	if err != nil {
		return nil, fmt.Errorf("invalid history control command: %w", err)
	}
	commandCSeq := uint32(command.cseq)
	previousCommandCSeq := commandCSeq - 1
	version := GBVersion10
	if g.svr != nil && g.svr.memoryStorer != nil {
		version = g.getDeviceGBProtocolVersion(in.Channel.DeviceID)
	}
	if err := validateHistoryControlCommand(command, version, stream); err != nil {
		return nil, err
	}
	tx, err := g.svr.requestFromResponsePreparedContextWithLocalFailure(ctx, ch, sip.MethodInfo, stream.Resp, func(req *sip.Request) error {
		req.SetBody([]byte(cmd), true)
		req.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
		return nil
	}, func() {
		atomic.CompareAndSwapUint32(&stream.CseqNo, commandCSeq, previousCommandCSeq)
	})
	if err != nil {
		return nil, err
	}
	response, err := sipResponseContext(ctx, tx)
	if err != nil {
		return nil, err
	}
	business, err := parseHistoryControlSIPResponse(response, command.version, command.cseq)
	if err != nil {
		return business, err
	}
	stream.historyState.commitResult(command, business, nil)
	if command.method == "TEARDOWN" {
		g.completeHistoryTeardown(key, stream)
	}
	return business, nil
}

// completeHistoryTeardown 在设备确认 TEARDOWN 后收敛本地历史会话。
// TEARDOWN 已结束嵌入式 MANSRTSP 会话，因此这里不再额外发送 SIP BYE。
func (g *GB28181API) completeHistoryTeardown(key string, stream *Streams) {
	if stream == nil {
		return
	}
	firstStop := g.markMediaStreamStopped(stream, "teardown", true)
	if firstStop && stream.T == 1 && !stream.DirectTCP {
		g.finishRTPDownload(stream, rtpDownloadStopped, "teardown")
	}
	if _, err := g.cleanupMediaStreamContext(g.mediaPersistenceContext(), key, stream); err != nil {
		slog.Warn("cleanup history TEARDOWN media", "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "stream_id", stream.StreamID, "err", err)
	}
	if err := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), stream.DeviceID, stream.ChannelID); err != nil {
		slog.Warn("persist history TEARDOWN channel state", "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "err", err)
	}
}

// buildHistoryControlCmd 将结构化控制参数转换为 MANSRTSP 文本。
func (g *GB28181API) buildHistoryControlCmd(stream *Streams, in *ControlHistoryInput) (string, error) {
	version := GBVersion10
	if g != nil && g.svr != nil && g.svr.memoryStorer != nil && in != nil && in.Channel != nil {
		version = g.getDeviceGBProtocolVersion(in.Channel.DeviceID)
	}
	if strings.TrimSpace(in.Cmd) != "" {
		command, err := parseCascadeMANSRTSP([]byte(in.Cmd))
		if err != nil {
			return "", fmt.Errorf("invalid raw history control command: %w", err)
		}
		if err := validateHistoryControlCommand(command, version, stream); err != nil {
			return "", err
		}
		cseq, err := stream.nextCSeq()
		if err != nil {
			return "", err
		}
		return string(command.body(cseq, historyControlProtocolVersion(version))), nil
	}
	return buildHistoryControlCmdForVersion(stream, in, version)
}

func buildHistoryControlCmdForVersion(stream *Streams, in *ControlHistoryInput, version GBProtocolVersion) (string, error) {
	if stream == nil || in == nil {
		return "", fmt.Errorf("invalid history control input")
	}
	action := strings.ToLower(strings.TrimSpace(in.Action))
	if action == "" {
		return "", fmt.Errorf("history control requires cmd or action")
	}
	protocol := historyControlProtocolVersion(version)
	switch action {
	case "play", "resume":
		cseq, err := stream.nextCSeq()
		if err != nil {
			return "", err
		}
		if version.AtLeast(GBVersion11) {
			return fmt.Sprintf("PLAY %s\r\nCSeq: %d\r\nRange: npt=now-\r\n\r\n", protocol, cseq), nil
		}
		return fmt.Sprintf("PLAY %s\r\nCSeq: %d\r\n\r\n", protocol, cseq), nil
	case "pause":
		cseq, err := stream.nextCSeq()
		if err != nil {
			return "", err
		}
		if version.AtLeast(GBVersion11) {
			return fmt.Sprintf("PAUSE %s\r\nCSeq: %d\r\nPauseTime: now\r\n\r\n", protocol, cseq), nil
		}
		return fmt.Sprintf("PAUSE %s\r\nCSeq: %d\r\n\r\n", protocol, cseq), nil
	case "speed":
		if in.Scale == 0 {
			return "", fmt.Errorf("history speed action requires scale")
		}
		if in.SeekAt > 0 {
			if version != GBVersion10 && (!version.AtLeast(GBVersion30) || in.Scale >= 0) {
				return "", fmt.Errorf("history speed seek_at is only valid for GB/T 28181-2011 speed playback or GB/T 28181-2022 reverse playback")
			}
			offset, err := historyControlOffset(stream, in.SeekAt)
			if err != nil {
				return "", err
			}
			cseq, err := stream.nextCSeq()
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("PLAY %s\r\nCSeq: %d\r\nScale: %.2f\r\nRange: npt=%d-\r\n\r\n", protocol, cseq, in.Scale, offset), nil
		}
		cseq, err := stream.nextCSeq()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("PLAY %s\r\nCSeq: %d\r\nScale: %.2f\r\n\r\n", protocol, cseq, in.Scale), nil
	case "seek":
		if in.SeekAt <= 0 {
			return "", fmt.Errorf("history seek action requires seek_at")
		}
		offset, err := historyControlOffset(stream, in.SeekAt)
		if err != nil {
			return "", err
		}
		cseq, err := stream.nextCSeq()
		if err != nil {
			return "", err
		}
		if version == GBVersion10 {
			return fmt.Sprintf("PLAY %s\r\nCSeq: %d\r\nRange: smpte=%s-\r\n\r\n", protocol, cseq, formatHistorySMPTEOffset(offset)), nil
		}
		return fmt.Sprintf("PLAY %s\r\nCSeq: %d\r\nRange: npt=%d-\r\n\r\n", protocol, cseq, offset), nil
	case "teardown", "stop":
		cseq, err := stream.nextCSeq()
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("TEARDOWN %s\r\nCSeq: %d\r\n\r\n", protocol, cseq), nil
	default:
		return "", fmt.Errorf("unsupported history action: %s", action)
	}
}

func historyControlOffset(stream *Streams, seekAt int64) (int64, error) {
	if stream == nil || stream.S.IsZero() || stream.E.IsZero() {
		return 0, fmt.Errorf("history seek requires session time range")
	}
	offset := seekAt - stream.S.Unix()
	if offset < 0 || seekAt > stream.E.Unix() {
		return 0, fmt.Errorf("history seek_at is outside session range")
	}
	return offset, nil
}

// formatHistorySMPTEOffset 将相对秒数编码为 GB/T 28181 附录 B 要求的 SMPTE 时间。
func formatHistorySMPTEOffset(offset int64) string {
	if offset < 0 {
		offset = 0
	}
	hours := offset / 3600
	minutes := (offset % 3600) / 60
	seconds := offset % 60
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, seconds)
}

func historyControlProtocolVersion(version GBProtocolVersion) string {
	if version.AtLeast(GBVersion11) {
		return "RTSP/1.0"
	}
	return "MANSRTSP/1.0"
}

func (g *GB28181API) stopHistoryNoLock(ch *Channel, in *StopHistoryInput) error {
	return g.stopHistoryNoLockContext(context.Background(), ch, in)
}

func (g *GB28181API) stopHistoryNoLockContext(ctx context.Context, _ *Channel, in *StopHistoryInput) error {
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid stop history input")
	}
	key := resolveHistorySessionKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID, in.sessionKey)
	stream, ok := g.streams.Load(key)
	if !ok {
		return nil
	}
	if stream == nil {
		g.streams.CompareAndDelete(key, nil)
		return nil
	}
	firstStop := g.markMediaStreamStopped(stream, "stopped_by_user", false)
	if stream.DirectTCP && g.directDownloads != nil && g.directDownloads.Cancel(stream.DirectSessionID) {
		return nil
	}
	if firstStop && in.Mode == historyModeDownload && !stream.DirectTCP {
		g.finishRTPDownload(stream, rtpDownloadStopped, "stopped_by_user")
	}
	complete, cleanupErr := g.cleanupMediaStreamContext(ctx, key, stream)
	if cleanupErr != nil {
		return cleanupErr
	}
	if !complete {
		return fmt.Errorf("history media cleanup remains pending")
	}
	return nil
}

func (g *GB28181API) sendHistoryBYE(ch *Channel, stream *Streams) error {
	return g.sendHistoryBYEContext(context.Background(), ch, stream)
}

func (g *GB28181API) sendHistoryBYEContext(ctx context.Context, ch *Channel, stream *Streams) error {
	if ch == nil || stream == nil || stream.Resp == nil {
		return nil
	}
	var responseErr error
	if g == nil || g.svr == nil {
		responseErr = fmt.Errorf("SIP server is unavailable")
	} else {
		tx, err := g.svr.requestFromResponseContext(ctx, ch, sip.MethodBYE, stream.Resp)
		if err != nil {
			responseErr = err
		} else {
			_, responseErr = sipResponseContext(ctx, tx)
		}
	}
	if stream.mediaServer != nil && g.sms != nil {
		_, closeErr := closeRTPServerContext(g.mediaPersistenceContext(), g.sms, stream.mediaServer, zlm.CloseRTPServerRequest{StreamID: stream.StreamID})
		if responseErr == nil {
			responseErr = closeErr
		}
	}
	return responseErr
}

func (g *GB28181API) hasActiveChannelStream(deviceID, channelID string) bool {
	if g == nil || g.streams == nil {
		return false
	}
	active := false
	g.streams.Range(func(_ string, stream *Streams) bool {
		if stream != nil && !g.mediaStreamStopping(stream) && stream.DeviceID == deviceID && stream.ChannelID == channelID {
			active = true
			return false
		}
		return true
	})
	return active
}

// persistChannelIdleIfNoActive 仅在同一通道的最后一条媒体流结束后写入停止状态。
// 直播、回放、下载和语音可以并存，任一会话结束都不能覆盖其他活动会话的状态。
func (g *GB28181API) persistChannelIdleIfNoActive(ctx context.Context, deviceID, channelID string) error {
	if g == nil || g.core.Store() == nil || g.hasActiveChannelStream(deviceID, channelID) {
		return nil
	}
	return g.core.EditPlaying(ctx, deviceID, channelID, false)
}

func (g *GB28181API) persistChannelActive(ctx context.Context, deviceID, channelID string) error {
	if g == nil || g.core.Store() == nil {
		return nil
	}
	return g.core.EditPlaying(ctx, deviceID, channelID, true)
}

func (g *GB28181API) startDirectTCPHistory(ctx context.Context, ch *Channel, in *HistoryInput) error {
	if in.Mode != historyModeDownload {
		return fmt.Errorf("direct TCP transport is only valid for Download")
	}
	if err := g.requireGBFeature(in.Channel.DeviceID, "direct_tcp_download", "2014 直接 TCP 文件下载", func(c GBCapabilities) bool {
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
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)

	unlock, err := ch.device.lockMediaContext(requestCtx, ch.ChannelID)
	if err != nil {
		return operation.ErrorOr(err)
	}
	defer unlock()
	key := historyKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	if existing, existed := g.streams.Load(key); existed {
		if err := g.stopHistoryNoLockContext(requestCtx, ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode}); err != nil {
			return operation.ErrorOr(fmt.Errorf("cleanup previous direct TCP history session: %w", err))
		}
		if existing.DirectTCP && existing.DirectSessionID != "" {
			waitCtx, cancel := context.WithTimeout(requestCtx, 3*time.Second)
			_, waitErr := g.directDownloads.Wait(waitCtx, existing.DirectSessionID)
			cancel()
			if waitErr != nil {
				return operation.ErrorOr(fmt.Errorf("wait previous direct TCP download to stop: %w", waitErr))
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
	occupied := false
	if !operation.Deliver(func() {
		_, occupied = g.streams.LoadOrStore(key, stream)
	}) {
		return operation.Cause()
	}
	if occupied {
		return operation.ErrorOr(fmt.Errorf("direct TCP history session was replaced concurrently"))
	}

	offer, err := g.sipInviteDirectTCPHistory(requestCtx, ch, in, stream, policy.OfferPort)
	if err != nil {
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_failed")
		return errors.Join(operation.ErrorOr(err), cleanupErr)
	}
	registeredIP := addressIP(ch.Source())
	initialState := DirectTCPDownloadState{
		SessionID:     stream.DirectSessionID,
		DeviceID:      in.Channel.DeviceID,
		ChannelID:     in.Channel.ChannelID,
		Status:        directTCPStatusConnecting,
		FileSize:      offer.FileSize,
		FileSizeKnown: offer.FileSizeKnown,
		StartedAt:     time.Now(),
		UpdatedAt:     time.Now(),
	}
	// 活动标记按设备+通道覆盖上一次终态；若进程中断，恢复时删除标记而不误报旧结果。
	if err := g.persistDirectTCPDownloadState(initialState, false); err != nil {
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "persist_failed")
		return errors.Join(fmt.Errorf("persist direct TCP download marker: %w", err), cleanupErr)
	}
	managerCtx := context.WithoutCancel(requestCtx)
	if !operation.Deliver(func() {
		err = g.directDownloads.Start(managerCtx, DirectTCPDownloadRequest{
			SessionID:           stream.DirectSessionID,
			DeviceID:            in.Channel.DeviceID,
			ChannelID:           in.Channel.ChannelID,
			Address:             offer.Address,
			RegisteredIP:        registeredIP,
			FileSize:            offer.FileSize,
			FileSizeKnown:       offer.FileSizeKnown,
			MediaStatusDisabled: g.isDeviceCapabilityDisabled(in.Channel.DeviceID, "media_status"),
			OnFinish: func(state DirectTCPDownloadState) {
				if persistErr := g.persistDirectTCPDownloadState(state, true); persistErr != nil && !g.serviceStopped() {
					slog.Warn("persist direct TCP download terminal state failed", "device_id", state.DeviceID, "channel_id", state.ChannelID, "session_id", state.SessionID, "err", persistErr)
				}
				g.finishDirectTCPHistory(key, ch, stream, state)
			},
		})
	}) {
		deleteErr := g.deleteDirectTCPDownloadMarker(initialState)
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_cancelled")
		return errors.Join(operation.Cause(), deleteErr, cleanupErr)
	}
	if err != nil {
		deleteErr := g.deleteDirectTCPDownloadMarker(initialState)
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_failed")
		return errors.Join(operation.ErrorOr(err), deleteErr, cleanupErr)
	}
	g.metrics.directStarted.Add(1)
	var persistErr error
	streamPublished := false
	if !operation.Deliver(func() {
		streamPublished, persistErr = g.commitChannelStreamStart(requestCtx, key, stream)
	}) {
		g.directDownloads.Cancel(stream.DirectSessionID)
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_cancelled")
		idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
		return errors.Join(operation.Cause(), cleanupErr, idleErr)
	}
	// 下载可能在异步管理器 Start 返回后立即完成；完成回调已提交终态时，
	// 启动路径不得再把通道覆盖回播放中。
	if !streamPublished {
		return nil
	}
	if persistErr != nil {
		g.directDownloads.Cancel(stream.DirectSessionID)
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "persist_failed")
		idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
		return errors.Join(fmt.Errorf("persist direct TCP download playing state: %w", persistErr), cleanupErr, idleErr)
	}
	return nil
}

// startCascadeDirectTCPSource 只建立附录 O 下级设备会话，不启动本地落盘下载。
// 上级级联随后通过 directTCPRelay 连接设备并流式转发原始 PS 数据。
func (g *GB28181API) startCascadeDirectTCPSource(ctx context.Context, in *HistoryInput, key string) (directTCPDownloadOffer, error) {
	if in == nil || in.Channel == nil || in.Mode != historyModeDownload {
		return directTCPDownloadOffer{}, fmt.Errorf("invalid cascade direct TCP history input")
	}
	if err := g.requireGBFeature(in.Channel.DeviceID, "direct_tcp_download", "2014 上级平台裸 TCP 下载中继", func(c GBCapabilities) bool {
		return c.DirectTCPDownload
	}); err != nil {
		return directTCPDownloadOffer{}, err
	}
	policy := g.directTCPPolicySnapshot()
	if !policy.CascadeRelayEnabled {
		return directTCPDownloadOffer{}, fmt.Errorf("2014 上级平台裸 TCP 下载中继未启用")
	}
	if _, allowed := policy.Allowlist[in.Channel.DeviceID]; !allowed {
		return directTCPDownloadOffer{}, fmt.Errorf("设备 %s 未加入直接 TCP 下载白名单", in.Channel.DeviceID)
	}
	if g.directDownloads == nil {
		return directTCPDownloadOffer{}, fmt.Errorf("direct TCP resource manager is unavailable")
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return directTCPDownloadOffer{}, ErrChannelNotExist
	}
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	unlock, err := ch.device.lockMediaContext(requestCtx, ch.ChannelID)
	if err != nil {
		return directTCPDownloadOffer{}, operation.ErrorOr(err)
	}
	defer unlock()

	stream := &Streams{
		T: 1, DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
		StreamID: in.streamID, DirectTCP: true, sessionKey: key, S: in.StartAt, E: in.EndAt,
	}
	if stream.StreamID == "" {
		stream.StreamID = cascadeSourceStreamID(key)
	}
	occupied := false
	if !operation.Deliver(func() {
		_, occupied = g.streams.LoadOrStore(key, stream)
	}) {
		return directTCPDownloadOffer{}, operation.Cause()
	}
	if occupied {
		return directTCPDownloadOffer{}, fmt.Errorf("cascade direct TCP source was replaced concurrently")
	}

	offer, err := g.sipInviteDirectTCPHistory(requestCtx, ch, in, stream, policy.OfferPort)
	if err != nil {
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_failed")
		return directTCPDownloadOffer{}, errors.Join(operation.ErrorOr(err), cleanupErr)
	}
	stream.FileSize = offer.FileSize
	stream.FileSizeKnown = offer.FileSizeKnown
	var persistErr error
	published := false
	if !operation.Deliver(func() {
		published, persistErr = g.commitChannelStreamStart(requestCtx, key, stream)
	}) {
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_cancelled")
		return directTCPDownloadOffer{}, errors.Join(operation.Cause(), cleanupErr)
	}
	if !published {
		return directTCPDownloadOffer{}, fmt.Errorf("cascade direct TCP source ended during setup")
	}
	if persistErr != nil {
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "persist_failed")
		return directTCPDownloadOffer{}, errors.Join(fmt.Errorf("persist cascade direct TCP channel state: %w", persistErr), cleanupErr)
	}
	return offer, nil
}

func (g *GB28181API) sipInviteDirectTCPHistory(ctx context.Context, ch *Channel, in *HistoryInput, stream *Streams, port int) (directTCPDownloadOffer, error) {
	cfg := g.configSnapshot()
	if cfg == nil {
		return directTCPDownloadOffer{}, fmt.Errorf("SIP configuration is unavailable")
	}
	ip4str, err := GetIP(g.boot.Media.SDPIP)
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	ssrc, releaseSSRC, err := g.reserveSSRC(1)
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	if err := stream.bindSSRCReservation(ssrc, releaseSSRC); err != nil {
		return directTCPDownloadOffer{}, err
	}
	uri, err := historyURI(ch.ChannelID, in.RecordType)
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	body, err := buildGBSDP(gbSDPInput{
		Version:       g.getDeviceGBProtocolVersion(in.Channel.DeviceID),
		SessionName:   historyModeDownload,
		ChannelID:     ch.ChannelID,
		URI:           uri,
		IP:            ip4str,
		Port:          port,
		StartAt:       in.StartAt,
		EndAt:         in.EndAt,
		SSRC:          ssrc,
		DirectTCP:     true,
		DownloadSpeed: in.DownloadSpeed,
		H265Disabled:  g.isDeviceCapabilityDisabled(in.Channel.DeviceID, "h265"),
	})
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	tx, err := g.svr.wrapRequestContext(ctx, ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(ch.ChannelID, ssrc, cfg.ID)})
	})
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	resp, err := sipInviteResponseContext(ctx, tx)
	in.routeResponse = resp
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	ack, err := sip.NewRequestFromResponseChecked(sip.MethodACK, resp)
	if err != nil {
		return directTCPDownloadOffer{}, err
	}
	if err := requestInviteACKContext(ctx, tx, ack); err != nil {
		return directTCPDownloadOffer{}, err
	}
	failConfirmed := func(cause error) (directTCPDownloadOffer, error) {
		byeErr := g.sendInviteResponseBYE(g.mediaPersistenceContext(), ch, resp)
		g.rememberMediaDialogCleanupResult(stream, resp, byeErr)
		return directTCPDownloadOffer{}, errors.Join(cause, byeErr)
	}
	if err := validateSIPContentType(resp, string(sip.ContentTypeSDP)); err != nil {
		return failConfirmed(fmt.Errorf("direct TCP download INVITE response %w", err))
	}
	if strings.TrimSpace(string(resp.Body())) == "" {
		return failConfirmed(fmt.Errorf("direct TCP download INVITE response SDP body is empty"))
	}
	offer, err := parseDirectTCPDownloadSDP(resp.Body())
	if err != nil {
		return failConfirmed(err)
	}
	if !validGBSSRC(offer.SSRC) {
		return failConfirmed(fmt.Errorf("direct TCP download response has invalid SSRC %q", offer.SSRC))
	}
	if offer.SSRC != ssrc {
		return failConfirmed(fmt.Errorf("direct TCP download response SSRC %s does not match offer %s", offer.SSRC, ssrc))
	}
	callID := ""
	if responseCallID, ok := resp.CallID(); ok {
		callID = normalizeCallID(responseCallID)
	}
	if callID == "" {
		return failConfirmed(fmt.Errorf("direct TCP download response missing Call-ID"))
	}
	stream.Resp = resp
	stream.Status = 0
	stream.Stop = false
	stream.CallID = callID
	stream.DirectSessionID = callID
	return offer, nil
}

func (g *GB28181API) finishDirectTCPHistory(key string, _ *Channel, stream *Streams, state DirectTCPDownloadState) {
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
	g.markMediaStreamStopped(stream, state.EndReason, false)
	if _, err := g.cleanupMediaStreamContext(g.mediaPersistenceContext(), key, stream); err != nil {
		slog.Warn("cleanup direct TCP history media", "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "err", err)
	}
	if err := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), stream.DeviceID, stream.ChannelID); err != nil {
		slog.Warn("persist direct TCP download channel state", "device_id", stream.DeviceID, "channel_id", stream.ChannelID, "err", err)
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

func (g *GB28181API) sipInviteHistory(ctx context.Context, ch *Channel, in *HistoryInput, port int, ssrc string, stream *Streams) error {
	cfg := g.configSnapshot()
	if cfg == nil {
		return fmt.Errorf("SIP configuration is unavailable")
	}
	ip4str, err := GetIP(in.SMS.GetSDPIP())
	if err != nil {
		return err
	}
	version := g.getDeviceGBProtocolVersion(in.Channel.DeviceID)
	h265Disabled := g.isDeviceCapabilityDisabled(in.Channel.DeviceID, "h265")
	if in.preferredPath != "" && version != GBVersion30 {
		return fmt.Errorf("X-PreferredPath requires downstream protocol 3.0, got %s", version)
	}
	uri, err := historyURI(ch.ChannelID, in.RecordType)
	if err != nil {
		return err
	}
	body, err := buildGBSDP(gbSDPInput{
		Version:       version,
		SessionName:   in.Mode,
		ChannelID:     ch.ChannelID,
		URI:           uri,
		IP:            ip4str,
		Port:          port,
		StreamMode:    in.StreamMode,
		StartAt:       in.StartAt,
		EndAt:         in.EndAt,
		SSRC:          ssrc,
		DownloadSpeed: in.DownloadSpeed,
		H265Disabled:  h265Disabled,
	})
	if err != nil {
		return err
	}
	tx, err := g.svr.wrapRequestContext(ctx, ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(ch.ChannelID, ssrc, cfg.ID)})
		if in.preferredPath != "" {
			r.AppendHeader(&sip.GenericHeader{HeaderName: cascadePreferredPathHeader, Contents: in.preferredPath})
		}
	})
	if err != nil {
		return err
	}
	resp, err := sipInviteResponseContext(ctx, tx)
	if err != nil {
		return err
	}
	stream.Resp = resp
	stream.T = 1
	stream.DeviceID = in.Channel.DeviceID
	stream.ChannelID = in.Channel.ChannelID
	if stream.StreamID == "" {
		stream.StreamID = in.Channel.ID
	}
	stream.mediaServer = in.SMS
	stream.Status = 0
	stream.Stop = false
	stream.ssrc = ssrc
	if callID, ok := resp.CallID(); ok {
		stream.CallID = normalizeCallID(callID)
	}
	if err := g.ackAndConnectActiveRTP(ctx, tx, resp, in.SMS, stream.StreamID, in.StreamMode, "video", ssrc, version, gbVideoPayloadFormats(version, h265Disabled)...); err != nil {
		byeErr := g.sendInviteResponseBYE(g.mediaPersistenceContext(), ch, resp)
		g.rememberMediaDialogCleanupResult(stream, resp, byeErr)
		return errors.Join(err, byeErr)
	}
	if in.Mode == historyModeDownload {
		size, known, parseErr := parseRTPDownloadFileSize(resp.Body())
		if parseErr != nil {
			byeErr := g.sendInviteResponseBYE(g.mediaPersistenceContext(), ch, resp)
			g.rememberMediaDialogCleanupResult(stream, resp, byeErr)
			return errors.Join(parseErr, byeErr)
		}
		stream.FileSize = size
		stream.FileSizeKnown = known
	}
	return nil
}
