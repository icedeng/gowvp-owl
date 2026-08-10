package gbs

import (
	"context"
	"encoding/xml"
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
	voiceModeTalk      = "Talk"
	voiceModeBroadcast = "Broadcast"
)

type VoiceInput struct {
	Channel    *ipc.Channel
	SMS        *sms.MediaServer
	StreamMode int8
	Mode       string // Talk/Broadcast
	Timeout    time.Duration
}

type broadcastNotify struct {
	XMLName  xml.Name `xml:"Notify"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	SourceID string   `xml:"SourceID"`
	TargetID string   `xml:"TargetID"`
}

type broadcastResponse struct {
	CmdType  string `xml:"CmdType"`
	SN       int    `xml:"SN"`
	DeviceID string `xml:"DeviceID"`
	Result   string `xml:"Result"`
}

type pendingBroadcastResponse struct {
	wait chan *broadcastResponse
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
	switch in.Mode {
	case voiceModeBroadcast:
		if err := g.requireGBFeature(in.Channel.DeviceID, "语音广播", func(c GBCapabilities) bool {
			return c.VoiceBroadcast
		}); err != nil {
			return err
		}
	case voiceModeTalk:
		if err := g.requireGBFeature(in.Channel.DeviceID, "语音对讲", func(c GBCapabilities) bool {
			return c.VoiceIntercom
		}); err != nil {
			return err
		}
	}
	if err := g.requireMediaTransport(in.Channel.DeviceID, in.StreamMode, "语音会话"); err != nil {
		return err
	}
	if in.Mode == voiceModeBroadcast {
		if err := g.startBroadcastNotification(ctx, ch, in); err != nil {
			return err
		}
	}

	ch.device.playMutex.Lock()
	defer ch.device.playMutex.Unlock()

	key := voiceKey(in.Mode, in.Channel.DeviceID, in.Channel.ChannelID)
	stream, existed := g.streams.LoadOrStore(key, &Streams{})
	if existed {
		_ = g.stopVoiceNoLock(ch, &StopVoiceInput{Channel: in.Channel, Mode: in.Mode})
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
	if err := g.sipInviteVoice(ch, in, resp.Port, stream); err != nil {
		return err
	}
	_ = g.svr.gb.core.EditPlaying(ctx, in.Channel.DeviceID, in.Channel.ChannelID, true)
	return nil
}

func (g *GB28181API) startBroadcastNotification(_ context.Context, ch *Channel, in *VoiceInput) error {
	sn := g.nextControlSN()
	body, err := sip.XMLEncode(broadcastNotify{
		CmdType:  "Broadcast",
		SN:       sn,
		SourceID: g.cfg.ID,
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
	if _, err = sipResponse(tx); err != nil {
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
	case <-timer.C:
		return fmt.Errorf("wait Broadcast response timeout")
	}
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
	ssrc := g.getSSRC(0)
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
	if in.Mode == voiceModeBroadcast {
		body = append(body, "f=v/////a/1/8/1\r\n"...)
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
	return tx.Request(sip.NewRequestFromResponse(sip.MethodACK, resp))
}
