package gbs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
)

type blockingCloseRTPMediaService struct {
	*fakeRTPMediaService
	started chan struct{}
	release chan struct{}
}

func (f *blockingCloseRTPMediaService) CloseRTPServer(_ *sms.MediaServer, _ zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error) {
	close(f.started)
	<-f.release
	return &zlm.CloseRTPServerResponse{Hit: 1}, nil
}

func TestMediaStatusRespondsBeforeSlowMediaCleanup(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "media-status-notify.xml"))
	if err != nil {
		t.Fatal(err)
	}
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-slow-cleanup",
		StreamID: "slow-cleanup-stream", mediaServer: &sms.MediaServer{ID: sms.DefaultMediaServerID},
	}
	streams.Store(key, stream)
	media := &blockingCloseRTPMediaService{
		fakeRTPMediaService: &fakeRTPMediaService{}, started: make(chan struct{}), release: make(chan struct{}),
	}
	api := &GB28181API{streams: streams, sms: media}
	conn := newFlowConnection()
	request := newFlowRequest(t, conn, sip.MethodMessage, "media-status-slow-cleanup", body)
	done := make(chan struct{})
	go func() {
		api.sipMessageMediaStatus(&sip.Context{
			Request: request, Tx: sip.NewTransaction("media-status-slow-cleanup-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote,
		})
		close(done)
	}()
	select {
	case <-media.started:
	case <-time.After(time.Second):
		t.Fatal("MediaStatus media cleanup did not start")
	}
	select {
	case payload := <-conn.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("MediaStatus response = %s", payload)
		}
	case <-time.After(200 * time.Millisecond):
		close(media.release)
		<-done
		t.Fatal("MediaStatus response waited for media cleanup")
	}
	close(media.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MediaStatus handler did not finish after media cleanup")
	}
}

func TestMediaStatus11FinishesHistoryIdempotently(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "media-status-notify.xml"))
	if err != nil {
		t.Fatal(err)
	}
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{
		DeviceID:  gb10DeviceID,
		ChannelID: gb10ChannelID,
		CallID:    "media-status-1",
	}
	streams.Store(key, stream)
	api := &GB28181API{streams: streams}
	conn := newFlowConnection()

	response := runFlowHandler(t, conn, api, sip.MethodMessage, "media-status-1", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := streams.Load(key); ok {
		t.Fatal("MediaStatus did not remove history session")
	}
	if stream.Status != 1 || !stream.Stop {
		t.Fatalf("MediaStatus stream state = status:%d stop:%v", stream.Status, stream.Stop)
	}

	response = runFlowHandler(t, conn, api, sip.MethodMessage, "media-status-1", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
}

func TestMediaStatus11SignalsDirectTCPDownload(t *testing.T) {
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("direct media"))
		<-release
	})
	defer close(release)
	manager := newTestDirectTCPManager(t)
	if err := manager.Start(context.Background(), DirectTCPDownloadRequest{
		SessionID:    "direct-media-status",
		DeviceID:     gb10DeviceID,
		ChannelID:    gb10ChannelID,
		Address:      address,
		RegisteredIP: net.ParseIP("127.0.0.1"),
	}); err != nil {
		t.Fatal(err)
	}
	waitDirectTCPBytes(t, manager, "direct-media-status")
	api := &GB28181API{directDownloads: manager}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10DeviceID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "direct-media-status", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	state := waitDirectTCPState(t, manager, "direct-media-status")
	if state.Status != directTCPStatusCompleted || state.EndReason != "media_status" {
		t.Fatalf("direct MediaStatus state = %+v", state)
	}
}

func TestMediaStatus11UnknownNotifyTypeDoesNotStop(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	streams.Store(key, &Streams{CallID: "media-status-2"})
	api := &GB28181API{streams: streams}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>999</NotifyType></Notify>`)

	response := runFlowHandler(t, conn, api, sip.MethodMessage, "media-status-2", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := streams.Load(key); !ok {
		t.Fatal("unknown MediaStatus type stopped the session")
	}
}
