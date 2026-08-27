package gbs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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
		VideoRecordPlan:     &VideoRecordPlan{InnerXML: `<RecordEnable>1</RecordEnable><RecordScheduleSumNum>1</RecordScheduleSumNum><RecordSchedule><WeekDayNum>1</WeekDayNum><TimeSegmentSumNum>1</TimeSegmentSumNum><TimeSegment><StartHour>0</StartHour><StartMin>0</StartMin><StartSec>0</StartSec><StopHour>23</StopHour><StopMin>59</StopMin><StopSec>59</StopSec></TimeSegment></RecordSchedule><StreamNumber>0</StreamNumber>`},
		VideoAlarmRecord:    &VideoAlarmRecord{InnerXML: `<RecordEnable>1</RecordEnable><StreamNumber>0</StreamNumber>`},
		PictureMask:         &PictureMask{InnerXML: `<On>1</On><SumNum>1</SumNum><RegionList Num="1"><Item><Seq>1</Seq><Point>20,30,50,60</Point></Item></RegionList>`},
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
		`<VideoParamAttribute Num="1">`, "<VideoRecordPlan>", "<VideoAlarmRecord>", "<PictureMask>",
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

func TestDeviceConfig30RejectsInvalidStructuredSections(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	tests := []struct {
		name  string
		input *DeviceConfigInput
	}{
		{name: "video attribute missing Num fields", input: &DeviceConfigInput{DeviceID: "device", VideoParamAttribute: &VideoParamAttribute{InnerXML: `<Item><StreamNumber>0</StreamNumber></Item>`}}},
		{name: "video attribute stream range", input: &DeviceConfigInput{DeviceID: "device", VideoParamAttribute: &VideoParamAttribute{InnerXML: `<Item><StreamNumber>3</StreamNumber><VideoFormat>H.265</VideoFormat><Resolution>1080P</Resolution><FrameRate>25</FrameRate><BitRateType>1</BitRateType></Item>`}}},
		{name: "record plan missing fields", input: &DeviceConfigInput{DeviceID: "device", VideoRecordPlan: &VideoRecordPlan{InnerXML: `<RecordPlan>1</RecordPlan>`}}},
		{name: "record plan bad time", input: &DeviceConfigInput{DeviceID: "device", VideoRecordPlan: &VideoRecordPlan{InnerXML: `<RecordEnable>1</RecordEnable><RecordScheduleSumNum>1</RecordScheduleSumNum><RecordSchedule><WeekDayNum>1</WeekDayNum><TimeSegmentSumNum>1</TimeSegmentSumNum><TimeSegment><StartHour>24</StartHour><StartMin>0</StartMin><StartSec>0</StartSec><StopHour>23</StopHour><StopMin>59</StopMin><StopSec>59</StopSec></TimeSegment></RecordSchedule><StreamNumber>0</StreamNumber>`}}},
		{name: "alarm recording enum", input: &DeviceConfigInput{DeviceID: "device", VideoAlarmRecord: &VideoAlarmRecord{InnerXML: `<RecordEnable>2</RecordEnable><StreamNumber>0</StreamNumber>`}}},
		{name: "alarm recording negative time", input: &DeviceConfigInput{DeviceID: "device", VideoAlarmRecord: &VideoAlarmRecord{InnerXML: `<RecordEnable>1</RecordEnable><RecordTime>-1</RecordTime><StreamNumber>0</StreamNumber>`}}},
		{name: "picture mask missing On", input: &DeviceConfigInput{DeviceID: "device", PictureMask: &PictureMask{InnerXML: `<SumNum>0</SumNum>`}}},
		{name: "picture mask count mismatch", input: &DeviceConfigInput{DeviceID: "device", PictureMask: &PictureMask{InnerXML: `<On>1</On><SumNum>1</SumNum><RegionList Num="0"><Item><Seq>1</Seq><Point>20,30,50,60</Point></Item></RegionList>`}}},
		{name: "picture mask point format", input: &DeviceConfigInput{DeviceID: "device", PictureMask: &PictureMask{InnerXML: `<On>1</On><SumNum>1</SumNum><RegionList Num="1"><Item><Seq>1</Seq><Point>20,30,10,60</Point></Item></RegionList>`}}},
		{name: "frame mirror enum", input: &DeviceConfigInput{DeviceID: "device", FrameMirror: &FrameMirror{InnerXML: `4`}}},
		{name: "alarm report missing field", input: &DeviceConfigInput{DeviceID: "device", AlarmReport: &AlarmReport{InnerXML: `<MotionDetection>1</MotionDetection>`}}},
		{name: "osd count mismatch", input: &DeviceConfigInput{DeviceID: "device", OSDConfig: &OSDConfig{InnerXML: `<Length>1920</Length><Width>1080</Width><TimeX>0</TimeX><TimeY>0</TimeY><SumNum>1</SumNum>`}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := api.buildDeviceConfigRequest("device", nil, test.input); err == nil {
				t.Fatal("invalid 2022 DeviceConfig section was accepted")
			}
		})
	}
}

func TestConfigDownload30RejectsInvalidStructuredSectionBeforeStateAndWait(t *testing.T) {
	tests := []struct {
		name    string
		section string
	}{
		{name: "record plan", section: `<VideoRecordPlan><RecordEnable>1</RecordEnable><RecordScheduleSumNum>1</RecordScheduleSumNum><StreamNumber>0</StreamNumber></VideoRecordPlan>`},
		{name: "picture mask", section: `<PictureMask><On>1</On><SumNum>1</SumNum><RegionList Num="0"><Item><Seq>1</Seq><Point>20,30,50,60</Point></Item></RegionList></PictureMask>`},
		{name: "frame mirror", section: `<FrameMirror>4</FrameMirror>`},
		{name: "alarm report", section: `<AlarmReport><MotionDetection>1</MotionDetection></AlarmReport>`},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			sn := 81 + index
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
			key := buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, sn)
			api.pendingDeviceQuery.Store(key, pending)
			defer api.pendingDeviceQuery.Delete(key)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>` + strconv.Itoa(sn) + `</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result>` + test.section + `</Response>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-invalid-2022-section", body, api.sipMessageConfigDownload)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid ConfigDownload response = %s", response)
			}
			select {
			case output := <-pending.wait:
				t.Fatalf("invalid ConfigDownload resolved pending query: %+v", output)
			default:
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.ConfigDownload != nil {
				t.Fatalf("invalid ConfigDownload changed query state: %+v", state.ConfigDownload)
			}
		})
	}
}

func TestConfigDownload11Rejects2022Section(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>82</SN><DeviceID>device</DeviceID><Result>OK</Result><FrameMirror>1</FrameMirror></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-2022-section-on-2014", body, api.sipMessageConfigDownload)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("2014 accepted 2022 ConfigDownload section: %s", response)
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

func TestConfigDownloadBasicParamValidationByVersion(t *testing.T) {
	full2014 := `<BasicParam><Name>IPC</Name><DeviceID>` + gb10DeviceID + `</DeviceID><SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP><SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration><Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`
	tests := []struct {
		name    string
		version GBProtocolVersion
		param   string
		ok      bool
	}{
		{name: "2014 complete", version: GBVersion11, param: full2014, ok: true},
		{name: "2014 partial", version: GBVersion11, param: `<BasicParam><Name>IPC</Name></BasicParam>`},
		{name: "2016 complete", version: GBVersion20, param: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`, ok: true},
		{name: "2016 partial", version: GBVersion20, param: `<BasicParam><Name>IPC</Name></BasicParam>`},
		{name: "2022 partial", version: GBVersion30, param: `<BasicParam><Name>IPC</Name></BasicParam>`, ok: true},
		{name: "2022 invalid present heartbeat", version: GBVersion30, param: `<BasicParam><HeartBeatInterval>0</HeartBeatInterval></BasicParam>`},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>` + strconv.Itoa(90+index) + `</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result>` + test.param + `</Response>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-basic-version", body, api.sipMessageConfigDownload)
			gotOK := strings.Contains(response, "SIP/2.0 200")
			if gotOK != test.ok {
				t.Fatalf("response = %s, want ok=%v", response, test.ok)
			}
		})
	}
}

func TestDeviceConfigAndQueryUseExactVersionProfiles(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion20, GBVersion30} {
		api, _ := newVersionGateAPI(version)
		if err := api.requireConfigTypeVersion("device", "AudioParamConfig"); err == nil {
			t.Fatalf("%s accepted removed AudioParamConfig query", version)
		}
		if _, err := api.buildDeviceConfigRequest("device", nil, &DeviceConfigInput{
			DeviceID: "device", AudioParamConfig: &AudioParamConfigWrite{Items: []AudioParamWriteItem{{
				StreamName: "Stream1", AudioFormat: "G.711", AudioBitRate: "64", SamplingRate: "8",
			}}},
		}); err == nil {
			t.Fatalf("%s accepted removed AudioParamConfig write", version)
		}
	}
	api, _ := newVersionGateAPI(GBVersion30)
	request, err := api.buildDeviceConfigRequest("device", nil, &DeviceConfigInput{
		DeviceID: "device", BasicParam: &BasicParam{Name: "IPC", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := sip.XMLEncode(request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, removed := range []string{"<DeviceID>", "<SIPServerID>", "<SIPServerIP>", "<SIPServerPort>", "<DomainName>", "<Password>"} {
		if strings.Count(text, removed) > map[string]int{"<DeviceID>": 1}[removed] {
			t.Fatalf("2022 BasicParam contains removed %s: %s", removed, text)
		}
	}
	if !strings.Contains(text, "<Name>IPC</Name>") || !strings.Contains(text, "<Expiration>3600</Expiration>") {
		t.Fatalf("2022 BasicParam missing supported fields: %s", text)
	}
}

func TestConfigDownloadFailureAndChildBasicParamDoNotChangeParentRuntime(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	for _, body := range []string{
		`<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`,
		`<Response><CmdType>ConfigDownload</CmdType><SN>2</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name><DeviceID>` + gb10ChannelID + `</DeviceID><SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP><SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration><Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`,
	} {
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-no-parent-update", []byte(body), api.sipMessageConfigDownload)
		assertFlowOK(t, response)
	}
	state := memory.runtime.runtimeSnapshot()
	if state.KeepaliveInterval != 0 || state.KeepaliveTimeout != 0 {
		t.Fatalf("failed/child ConfigDownload changed parent runtime: %+v", state)
	}
}

func TestConfigDownload30UsesStandardSnapShotResponseName(t *testing.T) {
	canonical, ok := normalizeConfigTypes("snapshot/SnapShotConfig")
	if !ok || canonical != "SnapShotConfig" {
		t.Fatalf("snapshot config normalization = %q, %v", canonical, ok)
	}
	standard := decodeConfigDownloadState([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>10</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShot><SnapNum>2</SnapNum><Interval>3</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>standard</SessionID></SnapShot></Response>`))
	if standard == nil || standard.SnapShot == nil || standard.SnapShot.SnapNum != 2 || standard.SnapShot.SessionID != "standard" {
		t.Fatalf("standard SnapShotConfig state = %+v", standard)
	}
	legacy := decodeConfigDownloadState([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>11</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShotConfig><SnapNum>1</SnapNum><Interval>1</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>legacy</SessionID></SnapShotConfig></Response>`))
	if legacy == nil || legacy.SnapShot == nil || legacy.SnapShot.SessionID != "legacy" {
		t.Fatalf("legacy SnapShot state = %+v", legacy)
	}
	if types := configDownloadStateTypes(standard); len(types) != 1 || types[0] != "SnapShotConfig" {
		t.Fatalf("snapshot config response types = %v", types)
	}
}

func TestConfigDownload30AcceptsStandardSnapShotResponse(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>12</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShot><SnapNum>2</SnapNum><Interval>3</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>snapshot-session-0000000000000012</SessionID></SnapShot></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-standard-snapshot", body, api.sipMessageConfigDownload)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.ConfigDownload == nil || state.ConfigDownload.SnapShot == nil ||
		state.ConfigDownload.SnapShot.SessionID != "snapshot-session-0000000000000012" {
		t.Fatalf("standard SnapShot response state = %+v", state.ConfigDownload)
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

func TestSnapshotConfigUploadPathCompatibility(t *testing.T) {
	sessionID := "snapshot-session-0000000000000200"
	for _, test := range []struct {
		name      string
		uploadURL string
		wantErr   bool
	}{
		{name: "http", uploadURL: "http://images.example.invalid/upload"},
		{name: "https", uploadURL: "https://images.example.invalid/upload"},
		{name: "relative", uploadURL: "/upload"},
		{name: "ftp", uploadURL: "ftp://images.example.invalid/upload"},
		{name: "blank", uploadURL: "  ", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateSnapshotConfig(&SnapShot{SnapNum: 1, Interval: 1, UploadURL: test.uploadURL, SessionID: sessionID})
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSnapshotConfig(%q) error = %v, wantErr %v", test.uploadURL, err, test.wantErr)
			}
		})
	}
}
