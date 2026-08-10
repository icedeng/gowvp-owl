package gbs

import (
	"testing"

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
	if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, &deviceControlA23Request{}); err != nil {
		t.Fatalf("1.1 IFrame control rejected: %v", err)
	}
	if err := api.requireConfigTypeVersion("device", "BasicParam"); err != nil {
		t.Fatalf("1.1 ConfigDownload rejected: %v", err)
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionPresetQuery, ""); err == nil {
		t.Fatal("1.1 must reject 2.0 PresetQuery")
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err == nil {
		t.Fatal("1.1 direct TCP download must not enable RTP over TCP")
	}

	memory.device.setGBVersion(GBVersion20)
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionPresetQuery, ""); err != nil {
		t.Fatalf("2.0 PresetQuery rejected: %v", err)
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err != nil {
		t.Fatalf("2.0 RTP over TCP rejected: %v", err)
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
func (m *versionGateMemory) LoadDeviceToMemory(sip.Connection)       {}
func (m *versionGateMemory) RangeDevices(func(string, *Device) bool) {}
func (m *versionGateMemory) Change(string, func(*ipc.Device) error, func(*Device)) error {
	return nil
}
func (m *versionGateMemory) Load(string) (*Device, bool)                { return m.device, true }
func (m *versionGateMemory) Store(_ string, device *Device)             { m.device = device }
func (m *versionGateMemory) GetChannel(string, string) (*Channel, bool) { return nil, false }
