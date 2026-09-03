package gbs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
)

type PlayInput struct {
	Channel            *ipc.Channel
	SMS                *sms.MediaServer
	ResolveMediaServer MediaServerResolver
	StreamMode         int8
	// 下列字段仅供 2022 多路径级联创建相互隔离的下级媒体会话。
	sessionKey    string
	streamID      string
	audioOnly     bool
	preferredPath string
	routeResponse *sip.Response
}

// MediaServerResolver 在取得通道媒体锁后解析实际节点，避免使用锁外旧快照。
type MediaServerResolver func(context.Context) (*sms.MediaServer, error)

func resolveMediaServerAfterLock(ctx context.Context, current *sms.MediaServer, resolver MediaServerResolver) (*sms.MediaServer, error) {
	if resolver == nil {
		return current, nil
	}
	server, err := resolver(ctx)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, fmt.Errorf("media server is unavailable")
	}
	return server, nil
}

type StopPlayInput struct {
	Channel *ipc.Channel
	// sessionKey 为空时使用普通实时点播兼容键。
	sessionKey string
	// skipLinkedVoice 由 Broadcast 主清理流程设置，避免标准 Talk 两条链路互相递归停止。
	skipLinkedVoice bool
}

func resolvePlaySessionKey(deviceID, channelID, sessionKey string) string {
	if key := strings.TrimSpace(sessionKey); key != "" {
		return key
	}
	return "play:" + deviceID + ":" + channelID
}

// stopPlay 不加锁的
func (g *GB28181API) stopPlay(ch *Channel, in *StopPlayInput) error {
	return g.stopPlayContext(context.Background(), ch, in)
}

func (g *GB28181API) stopPlayContext(ctx context.Context, _ *Channel, in *StopPlayInput) error {
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid stop play input")
	}
	key := resolvePlaySessionKey(in.Channel.DeviceID, in.Channel.ChannelID, in.sessionKey)
	stream, ok := g.streams.Load(key)
	if !ok {
		return nil
	}
	if stream == nil {
		g.streams.CompareAndDelete(key, nil)
		return nil
	}
	g.markMediaStreamStopped(stream, "stopped_by_user", false)
	complete, cleanupErr := g.cleanupMediaStreamContext(ctx, key, stream)
	if cleanupErr != nil {
		return cleanupErr
	}
	if !complete {
		return fmt.Errorf("media cleanup remains pending")
	}
	if !in.skipLinkedVoice {
		g.stopStandardTalkForPlayKey(key)
	}
	return nil
}

// StopPlay 加锁的停止播放
func (g *GB28181API) StopPlay(ctx context.Context, in *StopPlayInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid stop play input")
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		return ErrDeviceNotExist
	}

	unlock, err := ch.device.lockMediaContext(ctx, ch.ChannelID)
	if err != nil {
		return err
	}
	defer unlock()

	err = g.stopPlayContext(ctx, ch, in)
	persistErr := g.persistChannelIdleIfNoActive(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	return errors.Join(err, persistErr)
}

func (g *GB28181API) Play(in *PlayInput) error {
	return g.PlayContext(context.Background(), in)
}

// PlayContext 启动实时点播并允许调用方取消 SIP INVITE 等待。
func (g *GB28181API) PlayContext(ctx context.Context, in *PlayInput) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid play input")
	}
	log := slog.With("deviceID", in.Channel.DeviceID, "channelID", in.Channel.ChannelID)
	log.Info("开始播放流程")
	ch, ok := g.svr.memoryStorer.GetChannel(in.Channel.DeviceID, in.Channel.ChannelID)
	if !ok {
		log.Error("通道不存在")
		return ErrChannelNotExist
	}
	if err := ValidateRTPStreamMode(in.StreamMode); err != nil {
		return err
	}

	unlock, err := ch.device.lockMediaContext(ctx, ch.ChannelID)
	if err != nil {
		return err
	}
	defer unlock()
	in.SMS, err = resolveMediaServerAfterLock(ctx, in.SMS, in.ResolveMediaServer)
	if err != nil {
		return err
	}
	return g.playNoLock(ctx, ch, in)
}

func (g *GB28181API) playNoLock(ctx context.Context, ch *Channel, in *PlayInput) error {
	log := slog.With("deviceID", in.Channel.DeviceID, "channelID", in.Channel.ChannelID)
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.Channel.DeviceID, in.Channel.ChannelID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)
	if !ch.device.IsOnlineNow() {
		return ErrDeviceOffline
	}
	if err := g.requireMediaTransport(in.Channel.DeviceID, in.StreamMode, "实时点播"); err != nil {
		return err
	}

	// 附录 K/L 要求相同媒体流发送方标识只向设备发起一次请求，由平台完成媒体分发。
	// 已成功建立的同键直播直接复用；仅清理未完成或已停止的残留会话。
	key := resolvePlaySessionKey(in.Channel.DeviceID, in.Channel.ChannelID, in.sessionKey)
	var (
		stream *Streams
		ok     bool
	)
	if !operation.Deliver(func() {
		stream, ok = g.streams.LoadOrStore(key, &Streams{})
	}) {
		return operation.Cause()
	}
	if ok {
		if stream != nil && stream.Resp != nil && !g.mediaStreamStopping(stream) {
			if !operation.Deliver(func() {}) {
				return operation.Cause()
			}
			log.Debug("PLAY 复用已存在流", "stream_id", stream.StreamID)
			return nil
		}
		log.Warn("PLAY 清理未完成或已停止的残留流")
		if err := g.stopPlayContext(requestCtx, ch, &StopPlayInput{
			Channel: in.Channel, sessionKey: in.sessionKey,
		}); err != nil {
			return operation.ErrorOr(fmt.Errorf("cleanup previous Play session: %w", err))
		}
		stream = &Streams{}
		if !operation.Deliver(func() {
			stream, ok = g.streams.LoadOrStore(key, stream)
		}) {
			return operation.Cause()
		}
		if ok {
			return operation.ErrorOr(fmt.Errorf("Play session was replaced concurrently"))
		}
	}
	stream.DeviceID = in.Channel.DeviceID
	stream.ChannelID = in.Channel.ChannelID
	streamID := strings.TrimSpace(in.streamID)
	if streamID == "" {
		streamID = in.Channel.ID
	}
	stream.StreamID = streamID
	stream.mediaServer = in.SMS
	stream.sessionKey = key

	// SSRC 在打开 ZLM RTP 端口前生成并绑定，避免不同设备向同一端口串流。
	ssrc, releaseSSRC, err := g.reserveSSRC(0)
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
	log.Debug("1. 开启RTP服务器等待接收视频流", "ssrc", ssrc)
	tcpRTCP := shouldEnableTCPRTCP(g.getDeviceGBProtocolVersion(in.Channel.DeviceID), in.StreamMode != 0)
	// 开启RTP服务器等待接收视频流
	resp, err := openRTPServerContext(requestCtx, g.sms, in.SMS, zlm.OpenRTPServerRequest{
		TCPMode:  in.StreamMode,
		StreamID: streamID,
		SSRC:     ssrcValue,
		TCPRTCP:  tcpRTCP,
	})
	if err != nil {
		log.Debug("1.1. 开启RTP服务器失败", "err", err)
		// 如果是因为流已存在，先关闭再重新打开
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "-300") {
			log.Info("RTP服务器已存在，尝试关闭后重新打开", "stream_id", streamID)
			// 关闭旧的 RTP 服务器
			_, closeErr := closeRTPServerContext(requestCtx, g.sms, in.SMS, zlm.CloseRTPServerRequest{
				StreamID: streamID,
			})
			if closeErr != nil {
				log.Warn("关闭旧的RTP服务器失败", "err", closeErr)
			} else {
				log.Info("已关闭旧的RTP服务器，等待清理")
				// 等待一小段时间让 ZLM 清理资源
				timer := time.NewTimer(500 * time.Millisecond)
				select {
				case <-requestCtx.Done():
					timer.Stop()
					g.compareAndDeleteChannelStream(key, stream)
					stream.releaseSSRCReservation()
					return operation.Cause()
				case <-timer.C:
				}
			}
			// 重新打开 RTP 服务器
			resp, err = openRTPServerContext(requestCtx, g.sms, in.SMS, zlm.OpenRTPServerRequest{
				TCPMode:  in.StreamMode,
				StreamID: streamID,
				SSRC:     ssrcValue,
				TCPRTCP:  tcpRTCP,
			})
			if err != nil {
				log.Debug("1.2. 重新开启RTP服务器失败", "err", err)
				g.compareAndDeleteChannelStream(key, stream)
				stream.releaseSSRCReservation()
				return operation.ErrorOr(err)
			}
			log.Info("成功重新打开RTP服务器", "port", resp.Port)
		} else {
			g.compareAndDeleteChannelStream(key, stream)
			stream.releaseSSRCReservation()
			return operation.ErrorOr(err)
		}
	}

	log.Debug("2. 发送SDP请求", "port", resp.Port)
	if err := g.sipPlayPush2(requestCtx, ch, in, resp.Port, stream); err != nil {
		log.Debug("2.1. 发送SDP请求失败", "err", err)
		cleanupErr := g.cleanupFailedMediaStart(key, stream, "start_failed")
		return errors.Join(operation.ErrorOr(err), cleanupErr)
	}

	var persistErr error
	streamPublished := false
	if !operation.Deliver(func() {
		streamPublished, persistErr = g.commitChannelStreamStart(requestCtx, key, stream)
	}) {
		cleanupErr := g.stopPlayContext(g.mediaPersistenceContext(), ch, &StopPlayInput{
			Channel: in.Channel, sessionKey: in.sessionKey,
		})
		idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
		return errors.Join(operation.Cause(), cleanupErr, idleErr)
	}
	if !streamPublished {
		return nil
	}
	if persistErr != nil {
		cleanupErr := g.stopPlayContext(g.mediaPersistenceContext(), ch, &StopPlayInput{
			Channel: in.Channel, sessionKey: in.sessionKey,
		})
		idleErr := g.persistChannelIdleIfNoActive(g.mediaPersistenceContext(), in.Channel.DeviceID, in.Channel.ChannelID)
		return errors.Join(fmt.Errorf("persist playing state: %w", persistErr), cleanupErr, idleErr)
	}

	return nil
}

// GetIP 判断输入字符串并返回对应的 IP 地址。
// 输入可以是 IPv4、IPv6、域名或非法值；域名同时存在两种地址时优先 IPv4。
func GetIP(input string) (string, error) {
	slog.Info("开始域名解析", "输入", input)
	input = strings.TrimSpace(input)
	if input == "" {
		slog.Error("输入为空字符串")
		return input, fmt.Errorf("输入为空")
	}
	if strings.HasPrefix(input, "[") && strings.HasSuffix(input, "]") {
		input = strings.TrimSuffix(strings.TrimPrefix(input, "["), "]")
	}

	// 字面 IP 直接规范化；IPv4-mapped IPv6 仍按 IPv4 返回。
	if ip := net.ParseIP(input); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			return ipv4.String(), nil
		}
		return ip.String(), nil
	}

	// 尝试解析为域名
	ips, err := net.LookupIP(input)
	if err != nil {
		slog.Error("域名解析失败", "域名", input, "错误", err)
		return input, fmt.Errorf("无法解析域名: %w", err)
	}

	// 从解析结果中优先选择IPv4地址
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip.To4().String(), nil
		}
	}

	// 没有 IPv4 时选择第一个有效 IPv6 地址。
	for _, ip := range ips {
		if ip != nil && ip.To16() != nil {
			slog.Warn("域名只解析到IPv6地址", "域名", input)
			return ip.String(), nil
		}
	}

	slog.Error("域名没有解析到任何IP地址", "域名", input)
	return input, fmt.Errorf("域名没有解析到IP地址")
}

func (g *GB28181API) sipPlayPush2(ctx context.Context, ch *Channel, in *PlayInput, port int, stream *Streams) error {
	cfg := g.configSnapshot()
	if cfg == nil {
		return fmt.Errorf("SIP configuration is unavailable")
	}
	// 获取配置值
	ipstr := in.SMS.GetSDPIP()
	ip4str, err := GetIP(ipstr)
	if err != nil {
		slog.Error("域名解析失败", "域名", ipstr, "错误", err)
		return err
	}
	slog.Info("域名解析成功", "原始域名", ipstr, "解析IP", ip4str)

	ssrc := stream.ssrc
	version := g.getDeviceGBProtocolVersion(in.Channel.DeviceID)
	h265Disabled := g.isDeviceCapabilityDisabled(in.Channel.DeviceID, "h265")
	if in.preferredPath != "" && version != GBVersion30 {
		return fmt.Errorf("X-PreferredPath requires downstream protocol 3.0, got %s", version)
	}
	body, err := buildGBSDP(gbSDPInput{
		Version:      version,
		SessionName:  historyModePlay,
		ChannelID:    ch.ChannelID,
		IP:           ip4str,
		Port:         port,
		StreamMode:   in.StreamMode,
		SSRC:         ssrc,
		AudioOnly:    in.audioOnly,
		H265Disabled: h265Disabled,
		AACDisabled:  g.isDeviceCapabilityDisabled(in.Channel.DeviceID, "aac"),
	})
	if err != nil {
		return err
	}

	slog.Info(">>>", "body", string(body))
	// appending session to byte buffer
	// uri, _ := sip.ParseURI(channel.URIStr)
	// channel.addr = &sip.Address{URI: uri}
	// _serverDevices.addr.Params.Add("tag", sip.String{Str: sip.RandString(20)})
	tx, err := g.svr.wrapRequestContext(ctx, ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(ch.ChannelID, ssrc, cfg.ID)})
		if in.preferredPath != "" {
			r.AppendHeader(&sip.GenericHeader{HeaderName: cascadePreferredPathHeader, Contents: in.preferredPath})
		}
	})
	if err != nil {
		slog.Error("INVITE 发送失败", "channelID", ch.ChannelID, "ssrc", ssrc, "err", err)
		return err
	}
	resp, err := sipInviteResponseContext(ctx, tx)
	in.routeResponse = resp
	if err != nil {
		slog.Error("INVITE 等待响应失败", "channelID", ch.ChannelID, "ssrc", ssrc, "err", err)
		return err
	}

	stream.Resp = resp
	stream.T = 0
	stream.Status = 0
	stream.Stop = false
	stream.ssrc = ssrc
	if callID, ok := resp.CallID(); ok {
		stream.CallID = normalizeCallID(callID)
	}

	mediaType := "video"
	offeredFormats := gbVideoPayloadFormats(version, h265Disabled)
	if in.audioOnly {
		mediaType = "audio"
		offeredFormats = []string{"8"}
	}
	if err := g.ackAndConnectActiveRTP(ctx, tx, resp, in.SMS, stream.StreamID, in.StreamMode, mediaType, ssrc, version, offeredFormats...); err != nil {
		byeErr := g.sendInviteResponseBYE(g.mediaPersistenceContext(), ch, resp)
		g.rememberMediaDialogCleanupResult(stream, resp, byeErr)
		return errors.Join(err, byeErr)
	}
	return nil
}

// sip 请求播放
// func SipPlay(data *Streams) (*Streams, error) {
// 	channel := Channels{ChannelID: data.ChannelID}
// 	// if err := db.Get(db.DBClient, &channel); err != nil {
// 	// 	if db.RecordNotFound(err) {
// 	// 		return nil, errors.New("通道不存在")
// 	// 	}
// 	// 	return nil, err
// 	// }

// 	data.DeviceID = channel.DeviceID
// 	data.StreamType = channel.StreamType
// 	// 使用通道的播放模式进行处理
// 	switch channel.StreamType {
// 	case m.StreamTypePull:
// 		// 拉流

// 	default:
// 		// 推流模式要求设备在线且活跃
// 		if time.Now().Unix()-channel.Active > 30*60 || channel.Status != m.DeviceStatusON {
// 			return nil, errors.New("通道已离线")
// 		}
// 		user, ok := _activeDevices.Get(channel.DeviceID)
// 		if !ok {
// 			return nil, errors.New("设备已离线")
// 		}
// 		// GB28181推流
// 		if data.StreamID == "" {
// 			ssrcLock.Lock()
// 			// data.ssrc =g. getSSRC(data.T)
// 			data.StreamID = ssrc2stream(data.ssrc)

// 			// 成功后保存
// 			// db.Create(db.DBClient, data)
// 			ssrcLock.Unlock()
// 		}

// 		var err error
// 		data, err = sipPlayPush(data, channel, user)
// 		if err != nil {
// 			return nil, fmt.Errorf("获取视频失败:%v", err)
// 		}
// 	}

// 	data.HTTP = fmt.Sprintf("%s/rtp/%s/hls.m3u8", config.Media.HTTP, data.StreamID)
// 	data.RTMP = fmt.Sprintf("%s/rtp/%s", config.Media.RTMP, data.StreamID)
// 	data.RTSP = fmt.Sprintf("%s/rtp/%s", config.Media.RTSP, data.StreamID)
// 	data.WSFLV = fmt.Sprintf("%s/rtp/%s.live.flv", config.Media.WS, data.StreamID)

// 	data.Ext = time.Now().Unix() + 2*60 // 2分钟等待时间
// 	StreamList.Response.Store(data.StreamID, data)
// 	if data.T == 0 {
// 		StreamList.Succ.Store(data.ChannelID, data)
// 	}
// 	// db.Save(db.DBClient, data)
// 	return data, nil
// }

// func sipPlayPush(data *Streams, channel Channels, device Devices) (*Streams, error) {
// 	var (
// 		s sdp.Session
// 		b []byte
// 	)
// 	name := "Play"
// 	protocal := "TCP/RTP/AVP"
// 	if data.T == 1 {
// 		name = "Playback"
// 		protocal = "RTP/RTCP"
// 	}

// 	video := sdp.Media{
// 		Description: sdp.MediaDescription{
// 			Type:     "video",
// 			Port:     _sysinfo.MediaServerRtpPort,
// 			Formats:  []string{"96", "98", "97"},
// 			Protocol: protocal,
// 		},
// 	}
// 	video.AddAttribute("recvonly")
// 	if data.T == 0 {
// 		video.AddAttribute("setup", "passive")
// 		video.AddAttribute("connection", "new")
// 	}
// 	video.AddAttribute("rtpmap", "96", "PS/90000")
// 	video.AddAttribute("rtpmap", "98", "H264/90000")
// 	video.AddAttribute("rtpmap", "97", "MPEG4/90000")

// 	// defining message
// 	msg := &sdp.Message{
// 		Origin: sdp.Origin{
// 			Username: _serverDevices.DeviceID, // 媒体服务器id
// 			Address:  _sysinfo.MediaServerRtpIP.String(),
// 		},
// 		Name: name,
// 		Connection: sdp.ConnectionData{
// 			IP:  _sysinfo.MediaServerRtpIP,
// 			TTL: 0,
// 		},
// 		Timing: []sdp.Timing{
// 			{
// 				Start: data.S,
// 				End:   data.E,
// 			},
// 		},
// 		Medias: []sdp.Media{video},
// 		SSRC:   data.ssrc,
// 	}
// 	if data.T == 1 {
// 		msg.URI = fmt.Sprintf("%s:0", channel.ChannelID)
// 	}

// 	// appending message to session
// 	s = msg.Append(s)
// 	// appending session to byte buffer
// 	b = s.AppendTo(b)
// 	uri, _ := sip.ParseURI(channel.URIStr)
// 	channel.addr = &sip.Address{URI: uri}
// 	_serverDevices.addr.Params.Add("tag", sip.String{Str: sip.RandString(20)})
// 	hb := sip.NewHeaderBuilder().SetTo(channel.addr).SetFrom(_serverDevices.addr).AddVia(&sip.ViaHop{
// 		Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
// 	}).SetContentType(&sip.ContentTypeSDP).SetMethod(sip.MethodInvite).SetContact(_serverDevices.addr)
// 	req := sip.NewRequest("", sip.MethodInvite, channel.addr.URI, sip.DefaultSipVersion, hb.Build(), b)
// 	req.SetDestination(device.source)
// 	req.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: fmt.Sprintf("%s:%s,%s:%s", channel.ChannelID, data.StreamID, _serverDevices.DeviceID, data.StreamID)})
// 	req.SetRecipient(channel.addr.URI)
// 	tx, err := svr.Request(req)
// 	if err != nil {
// 		// logrus.Warningln("sipPlayPush fail.id:", device.DeviceID, channel.ChannelID, "err:", err)
// 		return data, err
// 	}
// 	// response
// 	response, err := sipResponse(tx)
// 	if err != nil {
// 		// logrus.Warningln("sipPlayPush response fail.id:", device.DeviceID, channel.ChannelID, "err:", err)
// 		return data, err
// 	}
// 	data.Resp = response
// 	// ACK
// 	tx.Request(sip.NewRequestFromResponse(sip.MethodACK, response))

// 	callid, _ := response.CallID()
// 	data.CallID = string(*callid)

// 	cseq, _ := response.CSeq()
// 	if cseq != nil {
// 		data.CseqNo = cseq.SeqNo
// 	}

// 	// from, _ := response.From()
// 	// to, _ := response.To()
// 	// for k, v := range to.Params.Items() {
// 	// 	data.Ttag[k] = v.String()
// 	// }
// 	// for k, v := range from.Params.Items() {
// 	// 	data.Ftag[k] = v.String()
// 	// }
// 	data.Status = 0

// 	return data, err
// }

// sip 停止播放
func SipStopPlay(ssrc string) {
	zlmCloseStream(ssrc)
	data, ok := StreamList.Response.Load(ssrc)
	if !ok {
		return
	}
	play := data.(*Streams)
	if play.StreamType == m.StreamTypePush {
		// 推流，需要发送关闭请求
		resp := play.Resp
		u, ok := _activeDevices.Load(play.DeviceID)
		if !ok {
			return
		}
		user := u.(Devices)
		server := defaultSIPServer.Load()
		if server == nil {
			play.Msg = "SIP server is unavailable"
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), legacyDeviceSIPWriteTimeout)
		defer cancel()
		tx, err := server.requestFromResponsePreparedContext(ctx, server.dialogTarget(play.DeviceID, play.ChannelID), sip.MethodBYE, resp, func(req *sip.Request) error {
			if req.Destination() == nil {
				req.SetDestination(user.source)
			}
			return nil
		})
		if err != nil {
			// logrus.Warningln("sipStopPlay bye fail.id:", play.DeviceID, play.ChannelID, "err:", err)
			play.Msg = err.Error()
			return
		}
		_, err = sipResponse(tx)
		if err != nil {
			// logrus.Warnln("sipStopPlay response fail", err)
			play.Msg = err.Error()
		} else {
			play.Status = 1
			play.Stop = true
		}
		// db.Save(db.DBClient, play)
	}
	StreamList.Response.Delete(ssrc)
	if play.T == 0 {
		StreamList.Succ.Delete(play.ChannelID)
	}
}
