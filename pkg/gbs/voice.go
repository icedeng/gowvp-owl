package gbs

import (
	"context"
	"fmt"
	"net"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	sdp "github.com/panjjo/gosdp"
)

const (
	voiceModeTalk      = "Talk"
	voiceModeBroadcast = "Broadcast"
)

type VoiceInput struct {
	Channel    *ipc.Channel
	SMS        *sms.MediaServer
	StreamMode int8
	Mode       string // Talk/Broadcast
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
	if !ch.device.IsOnline {
		return ErrDeviceOffline
	}

	ch.device.playMutex.Lock()
	defer ch.device.playMutex.Unlock()

	key := voiceKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	stream, existed := g.streams.LoadOrStore(key, &Streams{})
	if existed {
		_ = g.stopVoiceNoLock(ch, &StopVoiceInput{Channel: in.Channel, Mode: in.Mode})
	}

	resp, err := g.sms.OpenRTPServer(in.SMS, zlm.OpenRTPServerRequest{
		TCPMode:  in.StreamMode,
		StreamID: in.Channel.ID,
	})
	if err != nil {
		return err
	}
	if err := g.sipInviteVoice(ch, in, resp.Port, stream); err != nil {
		return err
	}
	_ = g.svr.gb.core.EditPlaying(ctx, in.Channel.DeviceID, in.Channel.ChannelID, true)
	return nil
}

// StopVoice 停止语音会话。
func (g *GB28181API) StopVoice(ctx context.Context, in *StopVoiceInput) error {
	if in == nil || in.Channel == nil {
		return fmt.Errorf("invalid stop voice input")
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
	return g.stopVoiceNoLock(ch, in)
}

func (g *GB28181API) stopVoiceNoLock(ch *Channel, in *StopVoiceInput) error {
	key := voiceKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
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

func (g *GB28181API) sipInviteVoice(ch *Channel, in *VoiceInput, port int, stream *Streams) error {
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
	// Talk 默认双向；Broadcast 按发送广播场景声明 sendonly。
	if in.Mode == voiceModeBroadcast {
		audio.AddAttribute("sendonly")
	} else {
		audio.AddAttribute("sendrecv")
	}
	audio.AddAttribute("rtpmap", "8", "PCMA/8000")
	audio.AddAttribute("rtpmap", "0", "PCMU/8000")
	audio.AddAttribute("rtpmap", "9", "G722/8000")

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
		Connection: sdp.ConnectionData{
			NetworkType: "IN",
			AddressType: "IP4",
			IP:          net.ParseIP(ip4str),
		},
		Timing: []sdp.Timing{{}},
		Medias: []sdp.Media{audio},
		SSRC:   g.getSSRC(0),
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
	stream.Resp = resp
	return tx.Request(sip.NewRequestFromResponse(sip.MethodACK, resp))
}
