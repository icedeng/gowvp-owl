package gbs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestBroadcastNotify11Fixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "broadcast-notify.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var notify broadcastNotify
	if err := sip.XMLDecode(body, &notify); err != nil {
		t.Fatal(err)
	}
	if notify.CmdType != "Broadcast" || notify.SourceID != gb10PlatformID || notify.TargetID != gb10ChannelID {
		t.Fatalf("Broadcast notify = %+v", notify)
	}
}

func TestBroadcastResponse11ResolvesPending(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "broadcast-response.xml"))
	if err != nil {
		t.Fatal(err)
	}
	pending := &pendingBroadcastResponse{wait: make(chan *broadcastResponse, 1)}
	api := &GB28181API{}
	api.pendingBroadcast.Store(buildPendingBroadcastKey(gb10ChannelID, 60), pending)
	conn := newFlowConnection()

	response := runFlowHandler(t, conn, api, sip.MethodMessage, "broadcast-1", body, api.sipMessageBroadcastResponse)
	assertFlowOK(t, response)
	select {
	case result := <-pending.wait:
		if result.Result != "OK" || result.DeviceID != gb10ChannelID {
			t.Fatalf("Broadcast response = %+v", result)
		}
	default:
		t.Fatal("Broadcast response did not resolve pending request")
	}
}
