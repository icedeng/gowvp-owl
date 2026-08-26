package gbs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestVersionGatesFor2011AndSupplement(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)

	if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, &deviceControlA23Request{}); err == nil {
		t.Fatal("1.0 must reject IFrame control")
	}
	if err := api.requireConfigTypeVersion("device", "BasicParam"); err == nil {
		t.Fatal("1.0 must reject ConfigDownload")
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err == nil {
		t.Fatal("1.0 must reject RTP over TCP")
	}

	memory.device.setGBVersion(GBVersion11)
	if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, &deviceControlA23Request{}); err == nil {
		t.Fatal("1.1 must reject 2.0 IFrame control")
	}
	if err := api.requireConfigTypeVersion("device", "BasicParam"); err != nil {
		t.Fatalf("1.1 ConfigDownload rejected: %v", err)
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionPresetQuery, ""); err != nil {
		t.Fatalf("1.1 PresetQuery rejected: %v", err)
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err == nil {
		t.Fatal("1.1 direct TCP download must not enable RTP over TCP")
	}

	memory.device.setGBVersion(GBVersion20)
	if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, &deviceControlA23Request{}); err != nil {
		t.Fatalf("2.0 IFrame control rejected: %v", err)
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionPresetQuery, ""); err != nil {
		t.Fatalf("2.0 PresetQuery rejected: %v", err)
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err != nil {
		t.Fatalf("2.0 RTP over TCP rejected: %v", err)
	}
}

func TestPresetQuery11AcceptsSupplementSpelling(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PresetQuery", 71), pending)
	conn := newFlowConnection()
	body := []byte(`<Response><CmdType>PersetQuery</CmdType><SN>71</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SumNum>0</SumNum><PresetList Num="0"></PresetList></Response>`)
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "preset-spelling", body, api.sipMessageQueryGeneric)
	assertFlowOK(t, response)
	select {
	case out := <-pending.wait:
		if out.CmdType != "PresetQuery" {
			t.Fatalf("canonical command = %q", out.CmdType)
		}
	default:
		t.Fatal("PersetQuery response did not resolve PresetQuery wait")
	}
}

func TestPresetQueryWireSpellingByVersion(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		want    string
	}{
		{name: "2011", version: GBVersion10, want: "PresetQuery"},
		{name: "2014 supplement", version: GBVersion11, want: "PersetQuery"},
		{name: "2016", version: GBVersion20, want: "PresetQuery"},
		{name: "2022", version: GBVersion30, want: "PresetQuery"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := gbQueryCmdTypeForVersion("PresetQuery", test.version); got != test.want {
				t.Fatalf("wire command = %q, want %q", got, test.want)
			}
			if got := gbQueryCmdTypeForVersion("PersetQuery", test.version); got != test.want {
				t.Fatalf("legacy wire command = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDeviceQuery11WritesSupplementPresetSpelling(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	flow := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: flow}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	memory.device.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = flow.remote
		device.to = remote
	})
	t.Cleanup(sipServer.Close)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := api.DeviceQuery(ctx, &DeviceQueryInput{DeviceID: gb10DeviceID, Action: deviceQueryActionPresetQuery})
		done <- err
	}()

	select {
	case payload := <-flow.writes:
		if body := string(payload); !strings.Contains(body, "<CmdType>PersetQuery</CmdType>") {
			t.Fatalf("2014 DeviceQuery body = %s", body)
		}
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("2014 DeviceQuery was not written")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled DeviceQuery error = %v", err)
	}
}

type versionGateMemory struct {
	device *Device
}

func newVersionGateAPI(version GBProtocolVersion) (*GB28181API, *versionGateMemory) {
	memory := &versionGateMemory{device: &Device{IsOnline: true, gbVersion: string(version)}}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	return api, memory
}

func (m *versionGateMemory) LoadOrStore(string, *Device)             {}
func (m *versionGateMemory) LoadDeviceToMemory(sip.Connection) error { return nil }
func (m *versionGateMemory) RangeDevices(func(string, *Device) bool) {}
func (m *versionGateMemory) Change(string, func(*ipc.Device) error, func(*Device)) error {
	return nil
}
func (m *versionGateMemory) Load(string) (*Device, bool)                { return m.device, true }
func (m *versionGateMemory) Store(_ string, device *Device)             { m.device = device }
func (m *versionGateMemory) GetChannel(string, string) (*Channel, bool) { return nil, false }
