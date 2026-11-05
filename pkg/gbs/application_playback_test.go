package gbs

import (
	"bufio"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestApplicationPlaybackSIPIsolation(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion20)
	adapter, _, channel := newCascadeMediaCore(t)
	media := &fakeRTPMediaService{openPort: 30000}
	ms := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
	api := fixture.api
	api.core = adapter
	api.sms = media
	api.streams = &conc.Map[string, *Streams]{}
	// 保留管理端会话，申请和停止三方会话都不得替换它。
	managementKey := historyKey(historyModePlayback, channel.DeviceID, channel.ChannelID)
	management := &Streams{DeviceID: channel.DeviceID, ChannelID: channel.ChannelID, StreamID: "management", T: 1}
	api.streams.Store(managementKey, management)
	peerDone := make(chan error, 1)
	go func() {
		_ = fixture.peer.SetDeadline(time.Now().Add(5 * time.Second))
		reader := bufio.NewReader(fixture.peer)
		for range 4 { // 两次INVITE（各含ACK），随后两次BYE。
			request, err := readAnnexGTestSIPFrame(reader)
			if err != nil {
				peerDone <- err
				return
			}
			if strings.HasPrefix(request, "INVITE ") {
				ssrc := directTCPSDPLineValue([]byte(request), "y")
				body := "v=0\r\no=" + gb10DeviceID + " 0 0 IN IP4 192.0.2.30\r\ns=Playback\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=video 9000 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\ny=" + ssrc + "\r\n"
				if _, err = io.WriteString(fixture.peer, directTCPTestResponse(request, body, "Application/SDP")); err != nil {
					peerDone <- err
					return
				}
				if _, err = readAnnexGTestSIPFrame(reader); err != nil {
					peerDone <- err
					return
				}
			} else {
				if _, err = io.WriteString(fixture.peer, annexGTestSIPResponse(request, http.StatusOK, "OK", "")); err != nil {
					peerDone <- err
					return
				}
			}
		}
		peerDone <- nil
	}()
	first, second := uuid.NewString(), uuid.NewString()
	for _, id := range []string{first, second} {
		if err := api.svr.StartApplicationPlayback(t.Context(), id, HistoryInput{Channel: channel, SMS: ms, StartAt: time.Unix(1700000000, 0), EndAt: time.Unix(1700000060, 0)}); err != nil {
			t.Fatal(err)
		}
		key, _ := applicationPlaybackKey(id)
		stream, ok := api.streams.Load(key)
		if !ok || stream.StreamID != ApplicationPlaybackStreamID(id) || !api.svr.ApplicationPlaybackActive(id) {
			t.Fatal("missing isolated stream", id)
		}
	}
	if err := api.svr.StopApplicationPlayback(t.Context(), first, StopHistoryInput{Channel: channel}); err != nil {
		t.Fatal(err)
	}
	if api.svr.ApplicationPlaybackActive(first) || !api.svr.ApplicationPlaybackActive(second) {
		t.Fatal("stop crossed session boundary")
	}
	if got, ok := api.streams.Load(managementKey); !ok || got != management {
		t.Fatal("management session replaced")
	}
	if err := api.svr.StopApplicationPlayback(t.Context(), second, StopHistoryInput{Channel: channel}); err != nil {
		t.Fatal(err)
	}
	if err := <-peerDone; err != nil {
		t.Fatal(err)
	}
	// 无内存会话的重复停止保持幂等，不回退停止管理端。
	if err := api.svr.StopApplicationPlayback(t.Context(), first, StopHistoryInput{Channel: channel}); err != nil {
		t.Fatal(err)
	}
	if got, ok := api.streams.Load(managementKey); !ok || got != management {
		t.Fatal("repeat stop affected management")
	}
}
