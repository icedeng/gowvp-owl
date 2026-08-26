package gbs

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestDeviceInfoResponseAcceptsKnownChildChannel(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	connection := newFlowConnection()
	runtimeDevice := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtimeChannel := &Channel{ChannelID: persistentChannel.ChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{
		flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice},
		channel:    runtimeChannel,
	}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api

	sn := 901
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", sn), pending)
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>901</SN><DeviceID>` + persistentChannel.ChannelID + `</DeviceID><Result>OK</Result><DeviceName>Gate IPC</DeviceName><Manufacturer>Vendor A</Manufacturer><Model>M-1</Model><Firmware>3.2.1</Firmware></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "device-info-channel", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("device-info-channel", connection), DeviceID: persistentDevice.DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), Log: slog.Default(),
	}
	api.sipMessageDeviceInfo(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("DeviceInfo response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo response timeout")
	}
	select {
	case output := <-pending.wait:
		if output.DeviceID != persistentChannel.ChannelID || output.Result != "OK" {
			t.Fatalf("pending DeviceInfo output = %+v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("channel DeviceInfo did not resolve pending query")
	}

	var updated ipc.Channel
	if err := adapter.Store().Channel().Get(t.Context(), &updated, orm.Where("channel_id = ?", persistentChannel.ChannelID)); err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Gate IPC" || updated.Ext.Manufacturer != "Vendor A" || updated.Ext.Model != "M-1" || updated.Ext.Firmware != "3.2.1" {
		t.Fatalf("updated channel DeviceInfo = %+v", updated)
	}
	var parent ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &parent, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if parent.Ext.Manufacturer != "" || parent.Ext.Model != "" || parent.Ext.Firmware != "" {
		t.Fatalf("child DeviceInfo overwrote parent = %+v", parent.Ext)
	}
}

func TestDeviceInfoResponseRejectsInvalidEnvelopeBeforeWait(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion10)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceInfo", 903), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Query><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Query>`},
		{name: "notify root", body: `<Notify><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Notify>`},
		{name: "wrong command", body: `<Response><CmdType>DeviceStatus</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "non-positive SN", body: `<Response><CmdType>DeviceInfo</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "missing result", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><DeviceName>Untrusted</DeviceName></Response>`},
		{name: "invalid result", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>SUCCESS</Result><DeviceName>Untrusted</DeviceName></Response>`},
		{name: "unknown target", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>34020000001320000009</DeviceID><Result>OK</Result></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-info-invalid-"+test.name, []byte(test.body), api.sipMessageDeviceInfo)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid DeviceInfo response = %s", response)
			}
		})
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("invalid DeviceInfo resolved pending query: %+v", output)
	default:
	}
}

func TestDeviceInfoErrorResolvesPendingQuery(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	connection := newFlowConnection()
	memory := newFlowMemory(persistentDevice.DeviceID)
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	sn := 902
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", sn), pending)
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>902</SN><DeviceID>` + persistentDevice.DeviceID + `</DeviceID><Result>ERROR</Result></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "device-info-error", body)
	api.sipMessageDeviceInfo(&sip.Context{
		Request: request, Tx: sip.NewTransaction("device-info-error", connection), DeviceID: persistentDevice.DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), Log: slog.Default(),
	})
	select {
	case output := <-pending.wait:
		if output.Result != "ERROR" {
			t.Fatalf("DeviceInfo error output = %+v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo ERROR did not resolve pending query")
	}
}
