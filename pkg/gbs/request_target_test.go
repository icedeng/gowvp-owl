package gbs

import (
	"errors"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestChannelInitRejectsMalformedSIPTarget(t *testing.T) {
	channel := &Channel{ChannelID: "bad\r\nVia: injected"}
	if err := channel.init("3402000000"); err == nil {
		t.Fatal("malformed channel SIP target was accepted")
	}
	if channel.To() != nil {
		t.Fatalf("malformed channel retained target: %+v", channel.To())
	}
}

func TestInvalidCatalogSnapshotDoesNotReplaceRuntimeChannels(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	existing := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	if err := existing.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(existing.ChannelID, existing)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	api.saveCatalogChannels(gb10DeviceID, []Channels{{ChannelID: "bad\r\nVia: injected"}})
	if _, ok := memory.runtime.Channels.Load(existing.ChannelID); !ok {
		t.Fatal("invalid Catalog snapshot removed the existing runtime channel")
	}
}

func TestLoadChannelsSkipsInvalidPersistedTarget(t *testing.T) {
	device := &Device{Address: "192.0.2.10:5060"}
	device.LoadChannels(
		&ipc.Channel{ChannelID: gb10ChannelID},
		&ipc.Channel{ChannelID: "bad\r\nVia: injected"},
	)
	if _, ok := device.Channels.Load(gb10ChannelID); !ok {
		t.Fatal("valid persisted channel was not restored")
	}
	if _, ok := device.Channels.Load("bad\r\nVia: injected"); ok {
		t.Fatal("invalid persisted channel was restored")
	}
}

func TestWrapRequestRejectsIncompleteTarget(t *testing.T) {
	local, err := sip.ParseSipURI("sip:34020000002000000001@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.DefaultConfig().Sip
	api := &GB28181API{cfg: &cfg}
	server := &Server{
		Server:      sip.NewServer(&sip.Address{URI: &local, Params: sip.NewParams()}),
		gb:          api,
		fromAddress: sip.Address{URI: &local, Params: sip.NewParams()},
	}
	api.svr = server
	t.Cleanup(server.Close)

	tests := []struct {
		name   string
		target *Channel
		want   string
	}{
		{name: "missing URI", target: &Channel{}, want: "target URI"},
		{name: "missing connection", target: validRequestTarget(t, nil), want: "connection"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := server.wrapRequest(test.target, sip.MethodMessage, &sip.ContentTypeXML, nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("incomplete target error = %v", err)
			}
		})
	}
}

func TestWrapRequestRejectsStoppedService(t *testing.T) {
	local, err := sip.ParseSipURI("sip:34020000002000000001@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	server := &Server{
		Server:      sip.NewServer(&sip.Address{URI: &local, Params: sip.NewParams()}),
		gb:          api,
		fromAddress: sip.Address{URI: &local, Params: sip.NewParams()},
	}
	api.svr = server
	t.Cleanup(server.Close)
	api.close()

	if _, err = server.wrapRequest(&Channel{}, sip.MethodMessage, &sip.ContentTypeXML, nil); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("stopped service error = %v; want %v", err, ErrServiceStopped)
	}
}

func validRequestTarget(t *testing.T, conn sip.Connection) *Channel {
	t.Helper()
	channel := &Channel{ChannelID: "34020000001320000001", device: &Device{conn: conn}}
	if err := channel.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	return channel
}
