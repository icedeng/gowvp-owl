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
	sdp "github.com/panjjo/gosdp"
)

const (
	historyModePlayback = "Playback"
	historyModeDownload = "Download"
)

// HistoryInput 历史回放/下载参数。
type HistoryInput struct {
	Channel    *ipc.Channel
	SMS        *sms.MediaServer
	StreamMode int8
	StartAt    time.Time
	EndAt      time.Time
	Mode       string // Playback 或 Download
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

	ch.device.playMutex.Lock()
	defer ch.device.playMutex.Unlock()

	key := historyKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	stream, existed := g.streams.LoadOrStore(key, &Streams{})
	if existed {
		_ = g.stopHistoryNoLock(ch, &StopHistoryInput{Channel: in.Channel, Mode: in.Mode})
	}

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
	stream, ok := g.streams.LoadAndDelete(key)
	if !ok || stream.Resp == nil {
		return nil
	}
	req := sip.NewRequestFromResponse(sip.MethodBYE, stream.Resp)
	req.SetDestination(ch.Source())
	req.SetConnection(ch.Conn())
	_, err := g.svr.Request(req)
	return err
}

func (g *GB28181API) sipInviteHistory(ch *Channel, in *HistoryInput, port int, stream *Streams) error {
	protocol := "TCP/RTP/AVP"
	if in.StreamMode == 0 {
		protocol = "RTP/AVP"
	}
	video := sdp.Media{
		Description: sdp.MediaDescription{
			Type:     "video",
			Port:     port,
			Formats:  []string{"96", "97", "98"},
			Protocol: protocol,
		},
	}
	video.AddAttribute("recvonly")
	if in.StreamMode == 1 {
		video.AddAttribute("setup", "passive")
		video.AddAttribute("connection", "new")
	}
	if in.StreamMode == 2 {
		video.AddAttribute("setup", "active")
		video.AddAttribute("connection", "new")
	}
	video.AddAttribute("rtpmap", "96", "PS/90000")
	video.AddAttribute("rtpmap", "97", "MPEG4/90000")
	video.AddAttribute("rtpmap", "98", "H264/90000")

	ip4str, err := GetIP(in.SMS.GetSDPIP())
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
		Name: in.Mode,
		URI:  fmt.Sprintf("%s:0", ch.ChannelID),
		Connection: sdp.ConnectionData{
			NetworkType: "IN",
			AddressType: "IP4",
			IP:          net.ParseIP(ip4str),
		},
		Timing: []sdp.Timing{
			{
				Start: in.StartAt,
				End:   in.EndAt,
			},
		},
		Medias: []sdp.Media{video},
		SSRC:   g.getSSRC(1),
	}

	body := msg.Append(nil).AppendTo(nil)
	tx, err := g.svr.wrapRequest(ch, sip.MethodInvite, &sip.ContentTypeSDP, body, func(r *sip.Request) {
		r.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: fmt.Sprintf("%s:%s,%s:%s", ch.ChannelID, in.Channel.ID, in.Channel.DeviceID, in.Channel.ID)})
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
	return tx.Request(sip.NewRequestFromResponse(sip.MethodACK, resp))
}
