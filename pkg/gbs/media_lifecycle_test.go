package gbs

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestMediaStreamLifecycleActiveAndInactive(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	stream := &Streams{
		DeviceID:  gb10DeviceID,
		ChannelID: gb10ChannelID,
		StreamID:  "internal-stream-1",
	}
	streams.Store("history:Playback:"+gb10DeviceID+":"+gb10ChannelID, stream)
	api := &GB28181API{streams: streams}
	now := time.Now()

	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Active: true, At: now}); err != nil {
		t.Fatal(err)
	}
	if !stream.Stream || !stream.LastMediaAt.Equal(now) {
		t.Fatalf("active stream state = %+v", stream)
	}
	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "rtp_server_timeout", At: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if stream.Stream || !stream.Stop || stream.EndReason != "rtp_server_timeout" {
		t.Fatalf("inactive stream state = %+v", stream)
	}
	if streams.Len() != 0 {
		t.Fatal("inactive stream was not removed")
	}
	if got := api.metrics.Snapshot().MediaDisconnects; got != 1 {
		t.Fatalf("media disconnect metric = %d; want 1", got)
	}

	// 重复注销和 MediaStatus 竞争后均应保持幂等。
	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "duplicate"}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaStreamLossSendsDeviceBYEAndClosesRTPServer(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "live", key: resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")},
		{name: "playback", key: historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseConnection := newFlowConnection()
			connection := &tcpFlowConnection{flowConnection: baseConnection}
			local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			remote := mustFlowAddress(t, "sip:"+gb10ChannelID+"@192.0.2.10:5060")
			local.Params.Add("tag", sip.String{Str: "platform-tag"})
			callID := sip.CallID("media-loss-" + test.name)
			invite := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetContact(local).SetMethod(sip.MethodInvite).
					SetSeqNo(1).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			invite.SetConnection(connection)
			invite.SetSource(baseConnection.local)
			invite.SetDestination(baseConnection.remote)
			response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
			response.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
			response.SetConnection(connection)

			sipServer := sip.NewServer(local)
			defer sipServer.Close()
			media := &fakeRTPMediaService{}
			streams := &conc.Map[string, *Streams]{}
			stream := &Streams{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "lost-" + test.name,
				Resp: response, mediaServer: &sms.MediaServer{ID: sms.DefaultMediaServerID},
			}
			streams.Store(test.key, stream)
			api := &GB28181API{sms: media, streams: streams}
			api.svr = &Server{Server: sipServer, gb: api, fromAddress: *local}

			if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "rtp_timeout"}); err != nil {
				t.Fatal(err)
			}
			select {
			case payload := <-baseConnection.writes:
				bye := string(payload)
				if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+string(callID)) || !strings.Contains(bye, "CSeq: 2 BYE") {
					t.Fatalf("media loss BYE:\n%s", bye)
				}
			case <-time.After(time.Second):
				t.Fatal("media loss did not send device BYE")
			}
			media.mu.Lock()
			closeCalls, closed := media.closeCalls, media.closed
			media.mu.Unlock()
			if closeCalls != 1 || closed.StreamID != stream.StreamID {
				t.Fatalf("RTP cleanup = calls:%d request:%+v", closeCalls, closed)
			}
			if _, ok := streams.Load(test.key); ok {
				t.Fatal("lost stream remained registered")
			}

			if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "duplicate"}); err != nil {
				t.Fatal(err)
			}
			media.mu.Lock()
			closeCalls = media.closeCalls
			media.mu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("duplicate stream loss closed RTP %d times", closeCalls)
			}
		})
	}
}

func TestMediaStartFailuresDoNotRetainPlaceholders(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	media := &fakeRTPMediaService{}
	streams := &conc.Map[string, *Streams]{}
	cfg := conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}
	api := &GB28181API{cfg: &cfg, sms: media, streams: streams}
	api.svr = &Server{gb: api, memoryStorer: memory}
	inputChannel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}
	mediaServer := &sms.MediaServer{}

	if err := api.Play(&PlayInput{Channel: inputChannel, SMS: mediaServer}); err == nil {
		t.Fatal("Play unexpectedly succeeded")
	}
	if streams.Len() != 0 {
		t.Fatal("failed Play retained a stream placeholder")
	}

	if err := api.StartHistory(t.Context(), &HistoryInput{
		Channel: inputChannel, SMS: mediaServer, Mode: historyModePlayback,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now(),
	}); err == nil {
		t.Fatal("StartHistory unexpectedly succeeded")
	}
	if streams.Len() != 0 {
		t.Fatal("failed history start retained a stream placeholder")
	}

	placeholderKey := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	streams.Store(placeholderKey, &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID})
	if err := api.stopHistoryNoLock(channel, &StopHistoryInput{Channel: inputChannel, Mode: historyModePlayback}); err != nil {
		t.Fatal(err)
	}
	if streams.Len() != 0 {
		t.Fatal("stopping an incomplete history session retained its placeholder")
	}
}
