package gbs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/conf"
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
		`<?xml version="1.0" encoding="GB2312"?>`,
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

func TestConfigDownload11SupportsAllSupplementTypes(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	for _, configType := range []string{
		"BasicParam", "VideoParamOpt", "VideoParamConfig", "AudioParamOpt", "AudioParamConfig",
		"SVACEncodeConfig", "SVACDecodeConfig",
	} {
		if err := api.requireConfigTypeVersion("device", configType); err != nil {
			t.Errorf("1.1 config type %s rejected: %v", configType, err)
		}
		if normalized, ok := normalizeConfigType(strings.ToLower(configType)); !ok || normalized != configType {
			t.Errorf("normalizeConfigType(%s) = %q, %v", configType, normalized, ok)
		}
	}
	if err := api.requireConfigTypeVersion("device", "VideoRecordPlan"); err == nil {
		t.Fatal("1.1 must reject 3.0 VideoRecordPlan")
	}
	combined, ok := normalizeConfigTypes("basicparam/video_param_opt/AudioParamConfig/BasicParam")
	if !ok || combined != "BasicParam/VideoParamOpt/AudioParamConfig" {
		t.Fatalf("combined ConfigType = %q, %v", combined, ok)
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionConfigDownload, combined); err != nil {
		t.Fatalf("1.1 combined ConfigDownload rejected: %v", err)
	}
}

func TestConfigDownload11DecodesVideoAndAudioParameters(t *testing.T) {
	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>9</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><VideoParamConfig Num="1"><Item><StreamName>Stream1</StreamName></Item></VideoParamConfig><AudioParamOpt><AudioFormatOpt>1/2</AudioFormatOpt></AudioParamOpt><AudioParamConfig Num="1"><Item><AudioFormat>1</AudioFormat></Item></AudioParamConfig></Response>`)
	state := decodeConfigDownloadState(body)
	if state == nil || state.VideoParamConfig == nil || state.AudioParamOpt == nil || state.AudioParamConfig == nil {
		t.Fatalf("ConfigDownload state = %+v", state)
	}
	if !strings.Contains(state.VideoParamConfig.InnerXML, "StreamName") ||
		!strings.Contains(state.AudioParamOpt.InnerXML, "AudioFormatOpt") ||
		!strings.Contains(state.AudioParamConfig.InnerXML, "AudioFormat") {
		t.Fatalf("ConfigDownload raw parameter data lost: %+v", state)
	}
}

func TestConfigDownload30UsesStandardSnapShotConfigName(t *testing.T) {
	canonical, ok := normalizeConfigTypes("snapshot/SnapShotConfig")
	if !ok || canonical != "SnapShotConfig" {
		t.Fatalf("snapshot config normalization = %q, %v", canonical, ok)
	}
	standard := decodeConfigDownloadState([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>10</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShotConfig><SnapNum>2</SnapNum><Interval>3</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>standard</SessionID></SnapShotConfig></Response>`))
	if standard == nil || standard.SnapShot == nil || standard.SnapShot.SnapNum != 2 || standard.SnapShot.SessionID != "standard" {
		t.Fatalf("standard SnapShotConfig state = %+v", standard)
	}
	legacy := decodeConfigDownloadState([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>11</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShot><SnapNum>1</SnapNum><Interval>1</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>legacy</SessionID></SnapShot></Response>`))
	if legacy == nil || legacy.SnapShot == nil || legacy.SnapShot.SessionID != "legacy" {
		t.Fatalf("legacy SnapShot state = %+v", legacy)
	}
	if types := configDownloadStateTypes(standard); len(types) != 1 || types[0] != "SnapShotConfig" {
		t.Fatalf("snapshot config response types = %v", types)
	}
}

func TestConfigDownload11AggregatesMultipleResponses(t *testing.T) {
	api := &GB28181API{}
	pending := &pendingQueryWait{
		wait: make(chan *DeviceQueryOutput, 1),
		expectedConfig: map[string]struct{}{
			"BasicParam": {}, "VideoParamOpt": {},
		},
	}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "ConfigDownload", 18), pending)
	api.resolvePendingDeviceQuery(gb10DeviceID, "ConfigDownload", 18, "OK", []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>18</SN><DeviceID>`+gb10DeviceID+`</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name></BasicParam></Response>`), gb10DeviceID)
	select {
	case out := <-pending.wait:
		t.Fatalf("combined query completed after first response: %+v", out)
	default:
	}
	api.resolvePendingDeviceQuery(gb10DeviceID, "ConfigDownload", 18, "OK", []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>18</SN><DeviceID>`+gb10DeviceID+`</DeviceID><Result>OK</Result><VideoParamOpt><VideoFormatOpt>2/5</VideoFormatOpt></VideoParamOpt></Response>`), gb10DeviceID)
	select {
	case out := <-pending.wait:
		state, ok := out.Data.(*ConfigDownloadState)
		if !ok || state.BasicParam == nil || state.VideoParamOpt == nil {
			t.Fatalf("aggregated ConfigDownload = %#v", out.Data)
		}
	default:
		t.Fatal("combined query did not complete after all responses")
	}
	stored, ok := api.GetQueryState(gb10DeviceID)
	if !ok || stored.ConfigDownload == nil || stored.ConfigDownload.BasicParam == nil || stored.ConfigDownload.VideoParamOpt == nil {
		t.Fatalf("stored combined ConfigDownload = %+v", stored)
	}
}

func TestCompleteBasicParam11IncludesRequiredServerFields(t *testing.T) {
	from := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000", Port: 5060},
		svr: &Server{fromAddress: *from},
	}
	param, err := api.completeBasicParam(gb10DeviceID, &Device{Password: "secret"}, BasicParam{
		Name: "Fixture Device", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if param.DeviceID != gb10DeviceID || param.SIPServerID != gb10PlatformID ||
		param.SIPServerIP != "192.0.2.20" || param.SIPServerPort != 5060 ||
		param.DomainName != "3402000000" || param.Password != "secret" {
		t.Fatalf("completed BasicParam = %+v", param)
	}
	encoded, err := json.Marshal(ConfigDownloadState{BasicParam: &param})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("BasicParam password leaked to JSON: %s", encoded)
	}
}
