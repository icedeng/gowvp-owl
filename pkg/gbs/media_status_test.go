package gbs

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

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
