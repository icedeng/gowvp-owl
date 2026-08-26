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

func TestDeviceConfig11VideoAudioAndSVACRequest(t *testing.T) {
	api := &GB28181API{}
	request, err := api.buildDeviceConfigRequest(gb10DeviceID, nil, &DeviceConfigInput{
		VideoParamConfig: &VideoParamConfigWrite{Items: []VideoParamWriteItem{{
			StreamName: "Stream1", VideoFormat: "H.264", Resolution: "1920x1080",
			FrameRate: "25", BitRateType: "1", VideoBitRate: "4096",
		}}},
		AudioParamConfig: &AudioParamConfigWrite{Items: []AudioParamWriteItem{{
			StreamName: "Stream1", AudioFormat: "G.711", AudioBitRate: "64", SamplingRate: "8",
		}}},
		SVACEncodeConfig: &SVACEncodeConfig{InnerXML: `<SVCParam><SVCFlag>1</SVCFlag><SVCSTMMode>2</SVCSTMMode></SVCParam>`},
		SVACDecodeConfig: &SVACDecodeConfig{InnerXML: `<SVCParam><SVCSTMMode>1</SVCSTMMode></SVCParam>`},
	})
	if err != nil {
		t.Fatal(err)
	}
	request.SetSN(32)
	body, err := sip.XMLEncode(request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		`<VideoParamConfig Num="1">`, "<StreamName>Stream1</StreamName>", "<VideoFormat>H.264</VideoFormat>",
		"<Resolution>1920x1080</Resolution>", "<FrameRate>25</FrameRate>", "<BitRateType>1</BitRateType>", "<VideoBitRate>4096</VideoBitRate>",
		`<AudioParamConfig Num="1">`, "<AudioFormat>G.711</AudioFormat>", "<AudioBitRate>64</AudioBitRate>", "<SamplingRate>8</SamplingRate>",
		"<SVACEncodeConfig><SVCParam><SVCFlag>1</SVCFlag><SVCSTMMode>2</SVCSTMMode></SVCParam></SVACEncodeConfig>",
		"<SVACDecodeConfig><SVCParam><SVCSTMMode>1</SVCSTMMode></SVCParam></SVACDecodeConfig>",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("DeviceConfig request missing %q: %s", required, text)
		}
	}
}

func TestDeviceConfigResponseRejectsInvalidEnvelopeBeforeStateAndWait(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1)}
	api.pendingDeviceConfig.Store(buildPendingDeviceConfigKey(gb10DeviceID, 31), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "unsupported version", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "wrong command", body: `<Response><CmdType>DeviceStatus</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "wrong root", body: `<Notify><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Notify>`},
		{name: "non-positive SN", body: `<Response><CmdType>DeviceConfig</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "missing result", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Response>`},
		{name: "invalid result", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>SUCCESS</Result></Response>`},
		{name: "unknown target", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>34020000001320000009</DeviceID><Result>OK</Result></Response>`},
	}
	for index, test := range tests {
		if index == 1 {
			memory.device.setGBVersion(GBVersion11)
		}
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-config-invalid-"+test.name, []byte(test.body), api.handleDeviceConfig)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid DeviceConfig response = %s", response)
			}
		})
	}
	select {
	case response := <-pending.wait:
		t.Fatalf("invalid DeviceConfig resolved wait: %+v", response)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceConfig != nil {
		t.Fatalf("invalid DeviceConfig changed state: %+v", state.DeviceConfig)
	}
}

func TestDeviceConfigResponseRejectsSiblingPendingTargetBeforeState(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	firstChannelID := gb10ChannelID
	secondChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(firstChannelID, &Channel{ChannelID: firstChannelID, device: memory.runtime})
	memory.runtime.Channels.Store(secondChannelID, &Channel{ChannelID: secondChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1), targetID: firstChannelID}
	api.pendingDeviceConfig.Store(buildPendingDeviceConfigKey(gb10DeviceID, 32), pending)
	body := []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>32</SN><DeviceID>` + secondChannelID + `</DeviceID><Result>OK</Result></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-config-sibling-target", body, api.handleDeviceConfig)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling DeviceConfig response = %s", response)
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("sibling DeviceConfig resolved pending wait: %+v", output)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceConfig != nil {
		t.Fatalf("sibling DeviceConfig changed state: %+v", state.DeviceConfig)
	}
}

func TestDeviceConfig11RejectsIncompleteAndUnsafeSections(t *testing.T) {
	api := &GB28181API{}
	tests := []struct {
		name  string
		input *DeviceConfigInput
	}{
		{name: "empty", input: &DeviceConfigInput{}},
		{name: "video missing field", input: &DeviceConfigInput{VideoParamConfig: &VideoParamConfigWrite{Items: []VideoParamWriteItem{{StreamName: "Stream1"}}}}},
		{name: "audio missing field", input: &DeviceConfigInput{AudioParamConfig: &AudioParamConfigWrite{Items: []AudioParamWriteItem{{StreamName: "Stream1"}}}}},
		{name: "svac malformed", input: &DeviceConfigInput{SVACEncodeConfig: &SVACEncodeConfig{InnerXML: `<SVCParam>`}}},
		{name: "svac directive", input: &DeviceConfigInput{SVACDecodeConfig: &SVACDecodeConfig{InnerXML: `<!DOCTYPE test><SVCParam/>`}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := api.buildDeviceConfigRequest(gb10DeviceID, nil, test.input); err == nil {
				t.Fatal("invalid DeviceConfig section was accepted")
			}
		})
	}
}

func TestDeviceConfig30WritesAll2022Sections(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	request, err := api.buildDeviceConfigRequest("device", nil, &DeviceConfigInput{
		DeviceID:            "device",
		VideoParamAttribute: &VideoParamAttribute{InnerXML: `<Item><StreamNumber>0</StreamNumber><VideoFormat>H.265</VideoFormat><Resolution>1920x1080</Resolution><FrameRate>25</FrameRate><BitRateType>1</BitRateType></Item>`},
		VideoRecordPlan:     &VideoRecordPlan{InnerXML: `<RecordPlan>1</RecordPlan>`},
		VideoAlarmRecord:    &VideoAlarmRecord{InnerXML: `<RecordEnable>1</RecordEnable><StreamNumber>0</StreamNumber>`},
		PictureMask:         &PictureMask{InnerXML: `<Enable>1</Enable><SumNum>0</SumNum>`},
		FrameMirror:         &FrameMirror{InnerXML: `1`},
		AlarmReport:         &AlarmReport{InnerXML: `<MotionDetection>1</MotionDetection><FieldDetection>1</FieldDetection>`},
		OSDConfig:           &OSDConfig{InnerXML: `<Length>1920</Length><Width>1080</Width><TimeX>10</TimeX><TimeY>10</TimeY><SumNum>0</SumNum>`},
		SnapShotConfig: &SnapShot{
			SnapNum: 1, Interval: 1, UploadURL: "https://example.invalid/snapshot",
			SessionID: "snapshot-session-0000000000000001",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request.SetSN(33)
	body, err := sip.XMLEncode(request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, required := range []string{
		"<VideoParamAttribute>", "<VideoRecordPlan>", "<VideoAlarmRecord>", "<PictureMask>",
		"<FrameMirror>1</FrameMirror>", "<AlarmReport>", "<OSDConfig>", "<SnapShotConfig>",
		"<SessionID>snapshot-session-0000000000000001</SessionID>",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("2022 DeviceConfig request missing %q: %s", required, text)
		}
	}
}

func TestDeviceConfig2022SectionsAreVersionGatedAndValidated(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	input := &DeviceConfigInput{DeviceID: "device", FrameMirror: &FrameMirror{InnerXML: `1`}}
	if _, err := api.buildDeviceConfigRequest("device", nil, input); err == nil || !strings.Contains(err.Error(), "2022") {
		t.Fatalf("2.0 FrameMirror error = %v", err)
	}
	memory.device.setGBVersion(GBVersion30)
	input.FrameMirror.InnerXML = `<!DOCTYPE test><Value>1</Value>`
	if _, err := api.buildDeviceConfigRequest("device", nil, input); err == nil {
		t.Fatal("unsafe 2022 DeviceConfig XML was accepted")
	}
	input.FrameMirror = nil
	input.SnapShotConfig = &SnapShot{SnapNum: 11, Interval: 0, SessionID: "short"}
	if _, err := api.buildDeviceConfigRequest("device", nil, input); err == nil {
		t.Fatal("invalid SnapShotConfig was accepted")
	}
}

func TestHandleDeviceConfig11StoresRawXML(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "device-config-basic-response.xml"))
	if err != nil {
		t.Fatal(err)
	}
	conn := newFlowConnection()
	api, _ := newVersionGateAPI(GBVersion11)
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

func TestConfigDownloadResponseValidatesBeforeRuntimeUpdate(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	initial := memory.device.runtimeSnapshot()
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong command", body: `<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>device</DeviceID><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`},
		{name: "non-positive SN", body: `<Response><CmdType>ConfigDownload</CmdType><SN>0</SN><DeviceID>device</DeviceID><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`},
		{name: "missing result", body: `<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>device</DeviceID><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`},
		{name: "invalid result", body: `<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>device</DeviceID><Result>SUCCESS</Result><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`},
		{name: "missing heartbeat interval", body: `<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>device</DeviceID><Result>OK</Result><BasicParam><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`},
		{name: "negative heartbeat count", body: `<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>device</DeviceID><Result>OK</Result><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>-1</HeartBeatCount></BasicParam></Response>`},
		{name: "heartbeat interval overflow", body: `<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>device</DeviceID><Result>OK</Result><BasicParam><HeartBeatInterval>65536</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`},
		{name: "unknown target", body: `<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: "device"}
			api.pendingDeviceQuery.Store(buildPendingQueryKey("device", CMDTypeConfigDownload, 1), pending)
			defer api.pendingDeviceQuery.Delete(buildPendingQueryKey("device", CMDTypeConfigDownload, 1))
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-invalid-"+test.name, []byte(test.body), api.sipMessageConfigDownload)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid ConfigDownload response = %s", response)
			}
			state := memory.device.runtimeSnapshot()
			if state.KeepaliveInterval != initial.KeepaliveInterval || state.KeepaliveTimeout != initial.KeepaliveTimeout {
				t.Fatalf("invalid ConfigDownload changed runtime: %+v", state)
			}
			select {
			case output := <-pending.wait:
				t.Fatalf("invalid ConfigDownload resolved pending query: %+v", output)
			default:
			}
			if state, ok := api.GetQueryState("device"); ok && state.ConfigDownload != nil {
				t.Fatalf("invalid ConfigDownload changed query state: %+v", state.ConfigDownload)
			}
		})
	}
}

func TestConfigDownloadFailureAndChildBasicParamDoNotChangeParentRuntime(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	for _, body := range []string{
		`<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`,
		`<Response><CmdType>ConfigDownload</CmdType><SN>2</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`,
	} {
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-no-parent-update", []byte(body), api.sipMessageConfigDownload)
		assertFlowOK(t, response)
	}
	state := memory.runtime.runtimeSnapshot()
	if state.KeepaliveInterval != 0 || state.KeepaliveTimeout != 0 {
		t.Fatalf("failed/child ConfigDownload changed parent runtime: %+v", state)
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
	resolve := func(body []byte) {
		decoded := api.decodeAndStoreQueryResult(gb10DeviceID, "ConfigDownload", body)
		api.resolvePendingDeviceQueryResult(gb10DeviceID, "ConfigDownload", 18, "OK", body, gb10DeviceID, decoded)
	}
	resolve([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>18</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name></BasicParam></Response>`))
	select {
	case out := <-pending.wait:
		t.Fatalf("combined query completed after first response: %+v", out)
	default:
	}
	resolve([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>18</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC duplicate</Name></BasicParam></Response>`))
	select {
	case out := <-pending.wait:
		t.Fatalf("combined query completed after duplicate response: %+v", out)
	default:
	}
	resolve([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>18</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><VideoParamOpt><VideoFormatOpt>2/5</VideoFormatOpt></VideoParamOpt></Response>`))
	select {
	case out := <-pending.wait:
		state, ok := out.Data.(*ConfigDownloadState)
		if !ok || state.BasicParam == nil || state.VideoParamOpt == nil {
			t.Fatalf("aggregated ConfigDownload = %#v", out.Data)
		}
		if len(out.responseXML) != 2 || !strings.Contains(out.responseXML[0], "<BasicParam>") || !strings.Contains(out.responseXML[1], "<VideoParamOpt>") {
			t.Fatalf("ConfigDownload response XML = %v", out.responseXML)
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
