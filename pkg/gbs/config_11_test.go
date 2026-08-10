package gbs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestConfigDownload11BasicParamAndRawXML(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "config-download-basic-response.xml"))
	if err != nil {
		t.Fatal(err)
	}
	state := decodeConfigDownloadState(body)
	if state == nil || state.BasicParam == nil {
		t.Fatal("BasicParam was not decoded")
	}
	if state.BasicParam.HeartBeatInterval != 60 || state.BasicParam.HeartBeatCount != 3 {
		t.Fatalf("BasicParam = %+v", state.BasicParam)
	}
	if !strings.Contains(state.RawXML, "<VendorParam>retained</VendorParam>") {
		t.Fatalf("raw ConfigDownload XML was not retained: %q", state.RawXML)
	}
}

func TestDeviceConfig11BasicParamRequest(t *testing.T) {
	body := NewDeviceConfig(gb10DeviceID).SetSN(31).SetBasicParam(&BasicParam{
		Name:              "Fixture Device",
		Expiration:        3600,
		HeartBeatInterval: 60,
		HeartBeatCount:    3,
	}).Marshal()
	text := string(body)
	for _, required := range []string{
		"<CmdType>DeviceConfig</CmdType>",
		"<BasicParam>",
		"<Expiration>3600</Expiration>",
		"<HeartBeatInterval>60</HeartBeatInterval>",
		"<HeartBeatCount>3</HeartBeatCount>",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("DeviceConfig request missing %q: %s", required, text)
		}
	}
}

func TestHandleDeviceConfig11StoresRawXML(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "device-config-basic-response.xml"))
	if err != nil {
		t.Fatal(err)
	}
	conn := newFlowConnection()
	api := &GB28181API{}
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1)}
	api.pendingDeviceConfig.Store(buildPendingDeviceConfigKey(gb10DeviceID, 31), pending)
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "config-1", body, api.handleDeviceConfig)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.DeviceConfig == nil || !strings.Contains(state.DeviceConfig.RawXML, "<VendorResult>accepted</VendorResult>") {
		t.Fatalf("DeviceConfig state did not retain raw XML: %+v", state)
	}
	select {
	case businessResponse := <-pending.wait:
		if !strings.Contains(businessResponse.RawXML, "<VendorResult>accepted</VendorResult>") {
			t.Fatalf("pending DeviceConfig response did not retain raw XML: %+v", businessResponse)
		}
	default:
		t.Fatal("pending DeviceConfig response was not delivered")
	}
}
