package gbs

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestDeviceConfigExtraInfoVersionMatrix(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	values := []string{" first ", "第二项"}
	request, err := api.buildDeviceConfigRequest("device", nil, &DeviceConfigInput{
		DeviceID:   "device",
		ExtraInfo:  values,
		BasicParam: &BasicParam{Name: "IPC", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	values[0] = "changed"
	if len(request.ExtraInfo) != 2 || request.ExtraInfo[0] != " first " {
		t.Fatalf("DeviceConfig ExtraInfo = %#v", request.ExtraInfo)
	}
	body, err := sip.XMLEncode(request)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Count(text, "<ExtraInfo>") != 2 || !strings.Contains(text, "<ExtraInfo> first </ExtraInfo>") || strings.Contains(text, "ExtralInfo") {
		t.Fatalf("DeviceConfig ExtraInfo XML = %s", text)
	}

	memory.device.setGBVersion(GBVersion20)
	if _, err = api.buildDeviceConfigRequest("device", nil, &DeviceConfigInput{
		DeviceID: "device", ExtraInfo: []string{"legacy"}, BasicParam: &BasicParam{Name: "IPC"},
	}); err == nil || !strings.Contains(err.Error(), "protocol 3.0") {
		t.Fatalf("2.0 DeviceConfig ExtraInfo error = %v", err)
	}
	memory.device.setGBVersion(GBVersion30)
	if _, err = api.buildDeviceConfigRequest("device", nil, &DeviceConfigInput{
		DeviceID: "device", ExtraInfo: []string{strings.Repeat("界", 1025)}, BasicParam: &BasicParam{Name: "IPC"},
	}); err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("oversized DeviceConfig ExtraInfo error = %v", err)
	}
}

func TestDeviceConfigBasicParamNamePreservesWhitespace(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	api.setConfig(conf.SIP{
		ID: gb10PlatformID, Host: "192.0.2.20", Port: 5060, Domain: "3402000000", Password: "secret",
	})
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			memory.device.setGBVersion(version)
			param, err := api.prepareBasicParam(gb10DeviceID, memory.device, BasicParam{
				Name: " Fixture Device ", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3,
			}, version)
			if err != nil {
				t.Fatal(err)
			}
			if param.Name != " Fixture Device " {
				t.Fatalf("BasicParam Name = %q", param.Name)
			}
			body := NewDeviceConfig(gb10DeviceID).SetBasicParam(&param).Marshal()
			if !strings.Contains(string(body), "<Name> Fixture Device </Name>") {
				t.Fatalf("BasicParam XML = %s", body)
			}
		})
	}
}

func TestDeviceConfigBasicParamRejectsRegistrationExpirationBelowStandardMinimum(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	api.setConfig(conf.SIP{
		ID: gb10PlatformID, Host: "192.0.2.20", Port: 5060, Domain: "3402000000", Password: "secret",
	})
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			memory.device.setGBVersion(version)
			_, err := api.prepareBasicParam(gb10DeviceID, memory.device, BasicParam{
				Name: "IPC", Expiration: minimumStandardRegisterTTL - 1, HeartBeatInterval: 60, HeartBeatCount: 3,
			}, version)
			if err == nil {
				t.Fatalf("%s accepted BasicParam expiration below %d", version.StandardName(), minimumStandardRegisterTTL)
			}
		})
	}
}

func TestDeviceConfigSnapshotUploadURLPreservesWhitespace(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	uploadURL := " https://example.invalid/snapshot "
	request, err := api.buildDeviceConfigRequest("device", nil, &DeviceConfigInput{
		DeviceID: "device",
		SnapShotConfig: &SnapShot{
			SnapNum: 1, Interval: 1, UploadURL: uploadURL,
			SessionID: "snapshot-session-0000000000000001",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.SnapShotConfig == nil {
		t.Fatal("SnapShotConfig was not built")
	}
	if request.SnapShotConfig.UploadURL != uploadURL {
		t.Fatalf("SnapShotConfig UploadURL = %q", request.SnapShotConfig.UploadURL)
	}
	body, err := sip.XMLEncode(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<UploadURL>"+uploadURL+"</UploadURL>") {
		t.Fatalf("SnapShotConfig XML = %s", body)
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
		{name: "2014 Appendix A.4 extension", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`},
		{name: "duplicate result", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><Result>OK</Result></Response>`},
		{name: "unknown field", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Unknown>1</Unknown></Response>`},
		{name: "out of order", body: `<Response><CmdType>DeviceConfig</CmdType><DeviceID>` + gb10DeviceID + `</DeviceID><SN>31</SN><Result>OK</Result></Response>`},
		{name: "root attribute", body: `<Response version="1"><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "child attribute", body: `<Response><CmdType type="string">DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "nested simple field", body: `<Response><CmdType><Value>DeviceConfig</Value></CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "snapshot response payload", body: `<Response><CmdType>DeviceConfig</CmdType><SN>31</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShotConfig><SnapNum>1</SnapNum></SnapShotConfig></Response>`},
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

func TestDeviceConfigResponseStoresChannelTargetStateWithoutOverwritingParent(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	targetID := persistentChannel.ChannelID
	memory.runtime.Channels.Store(targetID, &Channel{ChannelID: targetID, device: memory.runtime})
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1), targetID: targetID}
	api.pendingDeviceConfig.Store(buildPendingDeviceConfigKey(persistentDevice.DeviceID, 34), pending)
	body := []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>34</SN><DeviceID>` + targetID +
		`</DeviceID><Result>OK</Result><Info><doorType><DeviceID>` + targetID +
		`</DeviceID></doorType></Info></Response>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"device-config-channel-state", body, api.handleDeviceConfig)
	assertFlowOK(t, response)

	channelState, ok := api.GetQueryState(targetID)
	if !ok || channelState.DeviceConfig == nil || channelState.DeviceConfig.SN != 34 ||
		channelState.DeviceConfig.DeviceID != targetID || len(channelState.AppendixA4) != 1 {
		t.Fatalf("channel DeviceConfig state = %+v", channelState)
	}
	if parentState, ok := api.GetQueryState(persistentDevice.DeviceID); ok &&
		(parentState.DeviceConfig != nil || len(parentState.AppendixA4) != 0) {
		t.Fatalf("channel DeviceConfig overwrote parent state: %+v", parentState)
	}
	select {
	case output := <-pending.wait:
		if output.DeviceID != targetID || output.SN != 34 {
			t.Fatalf("channel DeviceConfig pending output = %+v", output)
		}
	default:
		t.Fatal("channel DeviceConfig response was not delivered")
	}
}

func TestDeviceConfigResponseCommitsOnlyAfterSuccessfulSIPOK(t *testing.T) {
	tests := []struct {
		name     string
		writeErr error
		commit   bool
	}{
		{name: "success", commit: true},
		{name: "write failure", writeErr: errors.New("write failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion11)
			pending := &pendingDeviceConfig{
				wait:     make(chan *DeviceConfigResponse, 1),
				targetID: gb10DeviceID,
			}
			api.pendingDeviceConfig.Store(buildPendingDeviceConfigKey(gb10DeviceID, 33), pending)

			body := []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>33</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`)
			conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "device-config-commit-"+test.name, body, api.handleDeviceConfig, test.writeErr)
			var committedBeforeSIP bool
			select {
			case <-pending.wait:
				committedBeforeSIP = true
			default:
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceConfig != nil {
				committedBeforeSIP = true
			}

			finishBlockingFlowHandler(t, conn, done)
			if committedBeforeSIP {
				t.Fatal("DeviceConfig response committed before SIP 200 was written")
			}

			state, ok := api.GetQueryState(gb10DeviceID)
			committed := ok && state.DeviceConfig != nil
			select {
			case <-pending.wait:
				committed = committed && true
			default:
				committed = false
			}
			if committed != test.commit {
				t.Fatalf("DeviceConfig committed = %v, want %v", committed, test.commit)
			}
		})
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

func TestConfigDownloadLargeHeartbeatValuesDoNotTruncateRuntime(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	memory.device.UpdateRuntime(func(device *Device) {
		device.keepaliveInterval = 60
		device.keepaliveTimeout = 3
	})
	const sn = 89
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
	key := buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, sn)
	api.pendingDeviceQuery.Store(key, pending)
	defer api.pendingDeviceQuery.Delete(key)

	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>89</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam><HeartBeatInterval>65536</HeartBeatInterval><HeartBeatCount>70000</HeartBeatCount></BasicParam></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-large-heartbeat", body, api.sipMessageConfigDownload)
	if !strings.Contains(response, "SIP/2.0 200") {
		t.Fatalf("large integer BasicParam response = %s", response)
	}
	runtime := memory.device.runtimeSnapshot()
	if runtime.KeepaliveInterval != 60 || runtime.KeepaliveTimeout != 3 {
		t.Fatalf("large heartbeat values truncated into runtime: %+v", runtime)
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.ConfigDownload == nil || state.ConfigDownload.BasicParam == nil ||
		state.ConfigDownload.BasicParam.HeartBeatInterval != 65536 || state.ConfigDownload.BasicParam.HeartBeatCount != 70000 {
		t.Fatalf("large heartbeat values were not preserved: %+v", state)
	}
	select {
	case output := <-pending.wait:
		if output == nil {
			t.Fatal("resolved ConfigDownload is nil")
		}
		config, _ := output.Data.(*ConfigDownloadState)
		if config == nil || config.BasicParam == nil ||
			config.BasicParam.HeartBeatInterval != 65536 || config.BasicParam.HeartBeatCount != 70000 {
			t.Fatalf("resolved ConfigDownload lost large heartbeat values: %+v", output)
		}
	default:
		t.Fatal("large integer BasicParam did not resolve pending query")
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
		{name: "2014 expiration below minimum", version: GBVersion11, param: strings.Replace(full2014, `<Expiration>3600</Expiration>`, `<Expiration>3599</Expiration>`, 1)},
		{name: "2016 complete", version: GBVersion20, param: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`, ok: true},
		{name: "2016 positioning", version: GBVersion20, param: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount><PositionCapability>2</PositionCapability><Longitude>116.397</Longitude><Latitude>39.908</Latitude></BasicParam>`, ok: true},
		{name: "2016 explicit no positioning", version: GBVersion20, param: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount><PositionCapability>0</PositionCapability><Longitude>0</Longitude><Latitude>0</Latitude></BasicParam>`, ok: true},
		{name: "2016 invalid positioning capability", version: GBVersion20, param: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount><PositionCapability>3</PositionCapability></BasicParam>`},
		{name: "2016 rejects 2014 identity", version: GBVersion20, param: `<BasicParam><Name>IPC</Name><DeviceID>` + gb10DeviceID + `</DeviceID><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`},
		{name: "2016 partial", version: GBVersion20, param: `<BasicParam><Name>IPC</Name></BasicParam>`},
		{name: "2016 expiration below minimum", version: GBVersion20, param: `<BasicParam><Name>IPC</Name><Expiration>3599</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`},
		{name: "2022 partial", version: GBVersion30, param: `<BasicParam><Name>IPC</Name></BasicParam>`, ok: true},
		{name: "2022 rejects 2014 identity", version: GBVersion30, param: `<BasicParam><Name>IPC</Name><DeviceID>` + gb10DeviceID + `</DeviceID></BasicParam>`},
		{name: "2022 rejects 2016 positioning", version: GBVersion30, param: `<BasicParam><Name>IPC</Name><PositionCapability>1</PositionCapability></BasicParam>`},
		{name: "2022 expiration below minimum", version: GBVersion30, param: `<BasicParam><Expiration>3599</Expiration></BasicParam>`},
		{name: "2022 invalid present heartbeat", version: GBVersion30, param: `<BasicParam><HeartBeatInterval>0</HeartBeatInterval></BasicParam>`},
		{name: "2022 rejects unknown field", version: GBVersion30, param: `<BasicParam><Name>IPC</Name><VendorField>1</VendorField></BasicParam>`},
		{name: "2022 rejects duplicate field", version: GBVersion30, param: `<BasicParam><Name>IPC</Name><Name>duplicate</Name></BasicParam>`},
		{name: "2022 rejects out of order fields", version: GBVersion30, param: `<BasicParam><HeartBeatCount>3</HeartBeatCount><Name>IPC</Name></BasicParam>`},
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

func TestConfigDownloadBasicParamPreservesExplicitZeroPositionValues(t *testing.T) {
	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>120</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name><Expiration>3600</Expiration><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount><PositionCapability>0</PositionCapability><Longitude>0</Longitude><Latitude>0</Latitude></BasicParam></Response>`)
	var response ConfigDownloadResponse
	if err := sip.XMLDecode(body, &response); err != nil {
		t.Fatal(err)
	}
	if err := validateConfigDownloadResponseForVersion(body, GBVersion20); err != nil {
		t.Fatal(err)
	}
	param := response.BasicParam
	if param == nil || !param.present.PositionCapability || !param.present.Longitude || !param.present.Latitude ||
		param.PositionCapability != 0 || param.Longitude != 0 || param.Latitude != 0 {
		t.Fatalf("explicit zero position values were lost: %+v", param)
	}
}

func TestConfigDownloadSVACValidationByVersion(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		section string
		value   string
		valid   bool
	}{
		{
			name: "2014 encode response", version: GBVersion11, section: "SVACEncodeConfig", valid: true,
			value: `<ROIParam><ROIFlag>1</ROIFlag><ROINumber>1</ROINumber><Item><ROISeq>1</ROISeq><TopLeft>10</TopLeft><BottomRight>20</BottomRight><ROIQP>3</ROIQP></Item><BackGroundQP>2</BackGroundQP><BackGroundSkipFlag>0</BackGroundSkipFlag></ROIParam>`,
		},
		{name: "2014 decode response", version: GBVersion11, section: "SVACDecodeConfig", value: `<SVCParam><SVCSTMMode>2</SVCSTMMode></SVCParam>`, valid: true},
		{name: "2014 invalid response range", version: GBVersion11, section: "SVACEncodeConfig", value: `<ROIParam><ROIFlag>2</ROIFlag><ROINumber>0</ROINumber><BackGroundQP>1</BackGroundQP><BackGroundSkipFlag>0</BackGroundSkipFlag></ROIParam>`},
		{
			name: "2016 encode response", version: GBVersion20, section: "SVACEncodeConfig", valid: true,
			value: `<SVCParam><SVCSpaceDomainMode>1</SVCSpaceDomainMode><SVCTimeDomainMode>2</SVCTimeDomainMode><SVCSpaceSupportMode>3</SVCSpaceSupportMode><SVCTimeSupportMode>0</SVCTimeSupportMode></SVCParam>`,
		},
		{name: "2016 decode response", version: GBVersion20, section: "SVACDecodeConfig", value: `<SVCParam><SVCSpaceSupportMode>2</SVCSpaceSupportMode><SVCTimeSupportMode>3</SVCTimeSupportMode></SVCParam>`, valid: true},
		{name: "2016 rejects write SVC field", version: GBVersion20, section: "SVACDecodeConfig", value: `<SVCParam><SVCSTMMode>1</SVCSTMMode></SVCParam>`},
		{name: "2016 missing response capability", version: GBVersion20, section: "SVACEncodeConfig", value: `<SVCParam><SVCSpaceDomainMode>1</SVCSpaceDomainMode><SVCTimeDomainMode>1</SVCTimeDomainMode><SVCSpaceSupportMode>1</SVCSpaceSupportMode></SVCParam>`},
		{name: "2016 incomplete ROI item", version: GBVersion20, section: "SVACEncodeConfig", value: `<ROIParam><ROIFlag>1</ROIFlag><ROINumber>1</ROINumber><Item><ROISeq>1</ROISeq></Item><BackGroundQP>1</BackGroundQP><BackGroundSkipFlag>0</BackGroundSkipFlag></ROIParam>`},
		{
			name: "2022 encode response", version: GBVersion30, section: "SVACEncodeConfig", valid: true,
			value: `<SVCParam><SVCSpaceDomainMode>1</SVCSpaceDomainMode><SVCTimeDomainMode>2</SVCTimeDomainMode><SVCSpaceSupportMode>3</SVCSpaceSupportMode><SVCTimeSupportMode>2</SVCTimeSupportMode><SSVCRatioSupportList>4:3/2:1</SSVCRatioSupportList></SVCParam>`,
		},
		{name: "2022 decode response", version: GBVersion30, section: "SVACDecodeConfig", value: `<SVCParam><SVCSpaceSupportMode>1</SVCSpaceSupportMode><SVCTimeSupportMode>2</SVCTimeSupportMode></SVCParam>`, valid: true},
		{name: "2022 missing response capability", version: GBVersion30, section: "SVACDecodeConfig", value: `<SVCParam><SVCSTMMode>1</SVCSTMMode><SVCSpaceSupportMode>1</SVCSpaceSupportMode></SVCParam>`},
		{name: "2022 rejects legacy surveillance field", version: GBVersion30, section: "SVACEncodeConfig", value: `<SurveillanceParam><EventFlag>1</EventFlag></SurveillanceParam>`},
		{name: "2022 incomplete surveillance response", version: GBVersion30, section: "SVACDecodeConfig", value: `<SurveillanceParam><TimeShowFlag>1</TimeShowFlag></SurveillanceParam>`},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			sn := 200 + index
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
			key := buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, sn)
			api.pendingDeviceQuery.Store(key, pending)
			defer api.pendingDeviceQuery.Delete(key)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>` + strconv.Itoa(sn) + `</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><` + test.section + `>` + test.value + `</` + test.section + `></Response>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-svac-version", body, api.sipMessageConfigDownload)
			gotOK := strings.Contains(response, "SIP/2.0 200")
			if gotOK != test.valid {
				t.Fatalf("response = %s, want valid %v", response, test.valid)
			}
			state, ok := api.GetQueryState(gb10DeviceID)
			stored := ok && state.ConfigDownload != nil
			if stored != test.valid {
				t.Fatalf("stored query state = %+v, want ConfigDownload stored %v", state, test.valid)
			}
			select {
			case output := <-pending.wait:
				if !test.valid {
					t.Fatalf("invalid ConfigDownload resolved pending query: %+v", output)
				}
				if output.CmdType != CMDTypeConfigDownload || output.SN != sn || output.DeviceID != gb10DeviceID {
					t.Fatalf("resolved ConfigDownload = %+v", output)
				}
			default:
				if test.valid {
					t.Fatal("valid ConfigDownload did not resolve pending query")
				}
			}
		})
	}
}

func TestConfigDownloadMediaParameterValidationByVersion(t *testing.T) {
	videoOpt2014 := `<VideoParamOpt><VideoFormatOpt>2/5</VideoFormatOpt><ResolutionOpt>1/2</ResolutionOpt><FrameRateOpt>25/30</FrameRateOpt><BitRateTypeOpt>1/2</BitRateTypeOpt><VideoBitRateOpt>512/1024</VideoBitRateOpt><DownloadSpeedOpt>1/2/4</DownloadSpeedOpt></VideoParamOpt>`
	videoConfig2014 := `<VideoParamConfig Num="1"><Item><StreamName>Stream1</StreamName><VideoFormat>H.264</VideoFormat><Resolution>1920x1080</Resolution><FrameRate>25</FrameRate><BitRateType>CBR</BitRateType><VideoBitRate>2048</VideoBitRate></Item></VideoParamConfig>`
	audioOpt2014 := `<AudioParamOpt><AudioFormatOpt>1/2</AudioFormatOpt><AudioBitRateOpt>64/128</AudioBitRateOpt><SamplingRateOpt>8/16</SamplingRateOpt></AudioParamOpt>`
	audioConfig2014 := `<AudioParamConfig Num="1"><Item><StreamName>Stream1</StreamName><AudioFormat>G.711</AudioFormat><AudioBitRate>64</AudioBitRate><SamplingRate>8</SamplingRate></Item></AudioParamConfig>`
	tests := []struct {
		name    string
		version GBProtocolVersion
		section string
		valid   bool
	}{
		{name: "2014 complete video options", version: GBVersion11, section: videoOpt2014, valid: true},
		{name: "2014 missing video option", version: GBVersion11, section: `<VideoParamOpt><VideoFormatOpt>2/5</VideoFormatOpt></VideoParamOpt>`},
		{name: "2014 non-integer frame option", version: GBVersion11, section: strings.Replace(videoOpt2014, "25/30", "25/PAL", 1)},
		{name: "2014 duplicate video option", version: GBVersion11, section: strings.Replace(videoOpt2014, `</VideoParamOpt>`, `<ResolutionOpt>3</ResolutionOpt></VideoParamOpt>`, 1)},
		{name: "2014 unknown video option", version: GBVersion11, section: strings.Replace(videoOpt2014, `</VideoParamOpt>`, `<VendorOpt>1</VendorOpt></VideoParamOpt>`, 1)},
		{name: "2014 video option attribute", version: GBVersion11, section: strings.Replace(videoOpt2014, `<VideoParamOpt>`, `<VideoParamOpt vendor="1">`, 1)},
		{name: "2014 complete video config", version: GBVersion11, section: videoConfig2014, valid: true},
		{name: "2014 video config count mismatch", version: GBVersion11, section: strings.Replace(videoConfig2014, `Num="1"`, `Num="2"`, 1)},
		{name: "2014 incomplete video config item", version: GBVersion11, section: strings.Replace(videoConfig2014, `<VideoBitRate>2048</VideoBitRate>`, "", 1)},
		{name: "2014 duplicate video config field", version: GBVersion11, section: strings.Replace(videoConfig2014, `</Item>`, `<StreamName>Stream2</StreamName></Item>`, 1)},
		{name: "2014 complete audio options", version: GBVersion11, section: audioOpt2014, valid: true},
		{name: "2014 missing audio option", version: GBVersion11, section: `<AudioParamOpt><AudioFormatOpt>1/2</AudioFormatOpt></AudioParamOpt>`},
		{name: "2014 non-integer audio bit rate", version: GBVersion11, section: strings.Replace(audioOpt2014, "64/128", "64/high", 1)},
		{name: "2014 complete audio config", version: GBVersion11, section: audioConfig2014, valid: true},
		{name: "2014 audio config count mismatch", version: GBVersion11, section: strings.Replace(audioConfig2014, `Num="1"`, `Num="0"`, 1)},
		{name: "2014 incomplete audio config item", version: GBVersion11, section: strings.Replace(audioConfig2014, `<SamplingRate>8</SamplingRate>`, "", 1)},
		{name: "2016 video options", version: GBVersion20, section: `<VideoParamOpt><DownloadSpeed>1/2/4</DownloadSpeed><Resolution>1/2</Resolution></VideoParamOpt>`, valid: true},
		{name: "2016 empty video options", version: GBVersion20, section: `<VideoParamOpt/>`, valid: true},
		{name: "2016 rejects 2014 video option", version: GBVersion20, section: `<VideoParamOpt><VideoFormatOpt>2/5</VideoFormatOpt></VideoParamOpt>`},
		{name: "2016 empty option value", version: GBVersion20, section: `<VideoParamOpt><Resolution>1//2</Resolution></VideoParamOpt>`},
		{name: "2022 video options", version: GBVersion30, section: `<VideoParamOpt><DownloadSpeed>1/2/4</DownloadSpeed><Resolution>1/2</Resolution></VideoParamOpt>`, valid: true},
		{name: "2022 duplicate option", version: GBVersion30, section: `<VideoParamOpt><Resolution>1/2</Resolution><Resolution>3</Resolution></VideoParamOpt>`},
		{name: "duplicate top-level section", version: GBVersion30, section: `<VideoParamOpt/><VideoParamOpt/>`},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			sn := 300 + index
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
			key := buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, sn)
			api.pendingDeviceQuery.Store(key, pending)
			defer api.pendingDeviceQuery.Delete(key)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>` + strconv.Itoa(sn) + `</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result>` + test.section + `</Response>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-media-version", body, api.sipMessageConfigDownload)
			gotOK := strings.Contains(response, "SIP/2.0 200")
			if gotOK != test.valid {
				t.Fatalf("response = %s, want valid %v", response, test.valid)
			}
			state, ok := api.GetQueryState(gb10DeviceID)
			stored := ok && state.ConfigDownload != nil
			if stored != test.valid {
				t.Fatalf("stored query state = %+v, want ConfigDownload stored %v", state, test.valid)
			}
			select {
			case output := <-pending.wait:
				if !test.valid {
					t.Fatalf("invalid ConfigDownload resolved pending query: %+v", output)
				}
				if output.CmdType != CMDTypeConfigDownload || output.SN != sn || output.DeviceID != gb10DeviceID {
					t.Fatalf("resolved ConfigDownload = %+v", output)
				}
			default:
				if test.valid {
					t.Fatal("valid ConfigDownload did not resolve pending query")
				}
			}
		})
	}
}

func TestConfigDownloadRejectsDuplicateEnvelopeAndSnapshotAliases(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
	}{
		{
			name: "duplicate result", version: GBVersion11,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>401</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Result>ERROR</Result></Response>`,
		},
		{
			name: "duplicate basic param", version: GBVersion30,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>402</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam/><BasicParam/></Response>`,
		},
		{
			name: "snapshot aliases", version: GBVersion30,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>403</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShot><SnapNum>1</SnapNum><Interval>1</Interval><UploadURL>https://example.invalid/a</UploadURL><SessionID>snapshot-session-0000000000000403</SessionID></SnapShot><SnapShotConfig><SnapNum>1</SnapNum><Interval>1</Interval><UploadURL>https://example.invalid/b</UploadURL><SessionID>snapshot-session-0000000000000403</SessionID></SnapShotConfig></Response>`,
		},
		{
			name: "unknown top-level element", version: GBVersion30,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>404</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><VendorConfig>1</VendorConfig></Response>`,
		},
		{
			name: "out-of-order envelope", version: GBVersion30,
			body: `<Response><CmdType>ConfigDownload</CmdType><DeviceID>` + gb10DeviceID + `</DeviceID><SN>405</SN><Result>OK</Result></Response>`,
		},
		{
			name: "error with cross-version section", version: GBVersion11,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>406</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><SnapShot><SnapNum>1</SnapNum><Interval>1</Interval><UploadURL>https://example.invalid/a</UploadURL><SessionID>snapshot-session-0000000000000406</SessionID></SnapShot></Response>`,
		},
		{
			name: "root attribute", version: GBVersion30,
			body: `<Response version="3.0"><CmdType>ConfigDownload</CmdType><SN>407</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`,
		},
		{
			name: "root namespace", version: GBVersion30,
			body: `<gb:Response xmlns:gb="urn:vendor"><CmdType>ConfigDownload</CmdType><SN>408</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></gb:Response>`,
		},
		{
			name: "simple field attribute", version: GBVersion30,
			body: `<Response><CmdType type="string">ConfigDownload</CmdType><SN>409</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`,
		},
		{
			name: "simple field namespace", version: GBVersion30,
			body: `<Response><gb:CmdType xmlns:gb="urn:vendor">ConfigDownload</gb:CmdType><SN>410</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`,
		},
		{
			name: "simple field nesting", version: GBVersion30,
			body: `<Response><CmdType><Value>ConfigDownload</Value></CmdType><SN>411</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`,
		},
		{
			name: "config section attribute", version: GBVersion30,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>412</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam vendor="1"/></Response>`,
		},
		{
			name: "successful response without configuration section", version: GBVersion30,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>413</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`,
		},
		{
			name: "multiple configuration sections", version: GBVersion30,
			body: `<Response><CmdType>ConfigDownload</CmdType><SN>414</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name></BasicParam><VideoParamOpt/></Response>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-duplicate", []byte(test.body), api.sipMessageConfigDownload)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("duplicate ConfigDownload response = %s", response)
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.ConfigDownload != nil {
				t.Fatalf("duplicate ConfigDownload changed query state: %+v", state.ConfigDownload)
			}
		})
	}
}

func TestConfigDownloadRejectsUnrequestedResponseType(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	const sn = 415
	pending := &pendingQueryWait{
		wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID,
		expectedConfig:  map[string]struct{}{"BasicParam": {}},
		requestedConfig: map[string]struct{}{"BasicParam": {}},
	}
	key := buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, sn)
	api.pendingDeviceQuery.Store(key, pending)
	defer api.pendingDeviceQuery.Delete(key)
	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>415</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><VideoParamOpt/></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-unrequested-type", body, api.sipMessageConfigDownload)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("unrequested ConfigDownload type response = %s", response)
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("unrequested ConfigDownload type resolved query: %+v", output)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.ConfigDownload != nil {
		t.Fatalf("unrequested ConfigDownload type changed state: %+v", state.ConfigDownload)
	}
}

func TestConfigDownloadRejectsRepeatedResponseType(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	const sn = 416
	pending := &pendingQueryWait{
		wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID,
		expectedConfig:  map[string]struct{}{"VideoParamOpt": {}},
		requestedConfig: map[string]struct{}{"BasicParam": {}, "VideoParamOpt": {}},
		config:          &ConfigDownloadState{BasicParam: &BasicParam{Name: "first"}},
	}
	key := buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, sn)
	api.pendingDeviceQuery.Store(key, pending)
	defer api.pendingDeviceQuery.Delete(key)
	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>416</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam><Name>duplicate</Name></BasicParam></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-repeated-type", body, api.sipMessageConfigDownload)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("repeated ConfigDownload type response = %s", response)
	}
	if pending.config.BasicParam.Name != "first" {
		t.Fatalf("repeated ConfigDownload type overwrote aggregate: %+v", pending.config.BasicParam)
	}
}

func TestConfigDownloadMessageDoesNotPublishSubscriptionNotify(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	connection := newFlowConnection()
	subscription := &eventSubscription{
		Key: "config-download-message", CmdType: CMDTypeConfigDownload, DeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
		To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Source:    connection.remote, Conn: connection, GBVersion: string(GBVersion11), Event: "presence",
	}
	attachFlowEventSubscriptionDialog(t, subscription, connection, "config-download-message-dialog")
	api.eventSubscribers.Store(subscription.Key, subscription)
	defer api.eventSubscribers.Delete(subscription.Key)

	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>250</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SVACDecodeConfig><SVCParam><SVCSTMMode>2</SVCSTMMode></SVCParam></SVACDecodeConfig></Response>`)
	response := runFlowHandler(t, connection, api, sip.MethodMessage, "config-download-message-no-notify", body, api.sipMessageConfigDownload)
	assertFlowOK(t, response)

	subscription.mu.Lock()
	cseq := subscription.CSeq
	subscription.mu.Unlock()
	if cseq != 0 {
		t.Fatalf("ConfigDownload MESSAGE advanced subscription NOTIFY CSeq to %d", cseq)
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

func TestDeviceConfigSVACValidationByVersion(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		section string
		value   string
		valid   bool
	}{
		{
			name: "2014 complete encode", version: GBVersion11, section: "SVACEncodeConfig", valid: true,
			value: `<ROIParam><ROIFlag>1</ROIFlag><ROINumber>1</ROINumber><Item><ROISeq>1</ROISeq><TopLeft>10</TopLeft><BottomRight>20</BottomRight><ROIQP>3</ROIQP></Item><BackGroundQP>2</BackGroundQP><BackGroundSkipFlag>0</BackGroundSkipFlag></ROIParam>` +
				`<SVCParam><SVCFlag>1</SVCFlag><SVCSTMMode>3</SVCSTMMode><SVCSpaceDomainMode>2</SVCSpaceDomainMode><SVCTimeDomainMode>1</SVCTimeDomainMode></SVCParam>` +
				`<SurveillanceParam><TimeFlag>1</TimeFlag><EventFlag>0</EventFlag><AlertFlag>1</AlertFlag></SurveillanceParam>` +
				`<EncryptParam><EncryptionFlag>1</EncryptionFlag><AuthenticationFlag>0</AuthenticationFlag></EncryptParam>` +
				`<AudioParam><AudioRecognitionFlag>1</AudioRecognitionFlag></AudioParam>`,
		},
		{
			name: "2014 complete decode", version: GBVersion11, section: "SVACDecodeConfig", valid: true,
			value: `<SVCParam><SVCSTMMode>2</SVCSTMMode></SVCParam><SurveillanceParam><TimeShowFlag>1</TimeShowFlag><EventShowFlag>0</EventShowFlag><AlerShowtFlag>1</AlerShowtFlag></SurveillanceParam>`,
		},
		{name: "2014 invalid ROI switch", version: GBVersion11, section: "SVACEncodeConfig", value: `<ROIParam><ROIFlag>9</ROIFlag><ROINumber>0</ROINumber><BackGroundQP>2</BackGroundQP><BackGroundSkipFlag>0</BackGroundSkipFlag></ROIParam>`},
		{name: "2014 missing required ROI fields", version: GBVersion11, section: "SVACEncodeConfig", value: `<ROIParam><ROIFlag>1</ROIFlag><ROINumber>0</ROINumber></ROIParam>`},
		{name: "2014 wrong decode spelling", version: GBVersion11, section: "SVACDecodeConfig", value: `<SurveillanceParam><AlertShowFlag>1</AlertShowFlag></SurveillanceParam>`},
		{
			name: "2016 optional ROI encode", version: GBVersion20, section: "SVACEncodeConfig", valid: true,
			value: `<ROIParam><ROIFlag>1</ROIFlag><ROINumber>1</ROINumber><Item><ROISeq>2</ROISeq><TopLeft>100</TopLeft><BottomRight>200</BottomRight><ROIQP>2</ROIQP></Item><BackGroundQP>1</BackGroundQP><BackGroundSkipFlag>0</BackGroundSkipFlag></ROIParam>` +
				`<AudioParam><AudioRecognitionFlag>1</AudioRecognitionFlag></AudioParam>`,
		},
		{name: "2016 decode", version: GBVersion20, section: "SVACDecodeConfig", value: `<SVCParam><SVCSTMMode>3</SVCSTMMode></SVCParam><SurveillanceParam><AlerShowtFlag>1</AlerShowtFlag></SurveillanceParam>`, valid: true},
		{name: "2016 rejects 2014 SVC encode", version: GBVersion20, section: "SVACEncodeConfig", value: `<SVCParam><SVCFlag>1</SVCFlag></SVCParam>`},
		{name: "2016 mismatched ROI count", version: GBVersion20, section: "SVACEncodeConfig", value: `<ROIParam><ROINumber>0</ROINumber><Item><ROISeq>1</ROISeq></Item></ROIParam>`},
		{name: "2016 duplicate ROI sequence", version: GBVersion20, section: "SVACEncodeConfig", value: `<ROIParam><ROINumber>2</ROINumber><Item><ROISeq>1</ROISeq></Item><Item><ROISeq>1</ROISeq></Item></ROIParam>`},
		{name: "2016 reversed ROI coordinates", version: GBVersion20, section: "SVACEncodeConfig", value: `<ROIParam><ROINumber>1</ROINumber><Item><TopLeft>20</TopLeft><BottomRight>10</BottomRight></Item></ROIParam>`},
		{name: "2016 invalid decode mode", version: GBVersion20, section: "SVACDecodeConfig", value: `<SVCParam><SVCSTMMode>4</SVCSTMMode></SVCParam>`},
		{
			name: "2022 complete encode", version: GBVersion30, section: "SVACEncodeConfig", valid: true,
			value: `<ROIParam><ROIFlag>1</ROIFlag><ROINumber>1</ROINumber><Item><ROISeq>1</ROISeq><TopLeft>100</TopLeft><BottomRight>300</BottomRight><ROIQP>3</ROIQP></Item></ROIParam>` +
				`<SVCParam><SVCSpaceDomainMode>3</SVCSpaceDomainMode><SVCTimeDomainMode>2</SVCTimeDomainMode><SSVCRatioValue>4:3/2:1</SSVCRatioValue><SVCSpaceSupportMode>3</SVCSpaceSupportMode><SVCTimeSupportMode>2</SVCTimeSupportMode><SSVCRatioSupportList>4:3/2:1</SSVCRatioSupportList></SVCParam>` +
				`<SurveillanceParam><TimeFlag>1</TimeFlag><OSDFlag>1</OSDFlag><AIFlag>0</AIFlag><GISFlag>1</GISFlag></SurveillanceParam>` +
				`<AudioParam><AudioRecognitionFlag>1</AudioRecognitionFlag></AudioParam>`,
		},
		{
			name: "2022 complete decode", version: GBVersion30, section: "SVACDecodeConfig", valid: true,
			value: `<SVCParam><SVCSTMMode>1</SVCSTMMode><SVCSpaceSupportMode>2</SVCSpaceSupportMode><SVCTimeSupportMode>3</SVCTimeSupportMode></SVCParam>` +
				`<SurveillanceParam><TimeShowFlag>1</TimeShowFlag><OSDShowFlag>0</OSDShowFlag><AIShowFlag>1</AIShowFlag><GISShowFlag>0</GISShowFlag></SurveillanceParam>`,
		},
		{name: "2022 missing encode modes", version: GBVersion30, section: "SVACEncodeConfig", value: `<SVCParam><SVCSpaceDomainMode>1</SVCSpaceDomainMode></SVCParam>`},
		{name: "2022 invalid ratio", version: GBVersion30, section: "SVACEncodeConfig", value: `<SVCParam><SVCSpaceDomainMode>1</SVCSpaceDomainMode><SVCTimeDomainMode>1</SVCTimeDomainMode><SSVCRatioValue>4-3</SSVCRatioValue></SVCParam>`},
		{name: "2022 rejects legacy background", version: GBVersion30, section: "SVACEncodeConfig", value: `<ROIParam><BackGroundQP>1</BackGroundQP></ROIParam>`},
		{name: "2022 invalid surveillance switch", version: GBVersion30, section: "SVACEncodeConfig", value: `<SurveillanceParam><AIFlag>2</AIFlag></SurveillanceParam>`},
		{name: "2022 rejects legacy decode field", version: GBVersion30, section: "SVACDecodeConfig", value: `<SurveillanceParam><EventShowFlag>1</EventShowFlag></SurveillanceParam>`},
		{name: "duplicate singleton element", version: GBVersion30, section: "SVACDecodeConfig", value: `<SVCParam><SVCSTMMode>1</SVCSTMMode><SVCSTMMode>2</SVCSTMMode></SVCParam>`},
		{name: "unknown element", version: GBVersion30, section: "SVACEncodeConfig", value: `<VendorParam>1</VendorParam>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			input := &DeviceConfigInput{DeviceID: gb10DeviceID}
			cascade := &DeviceConfigRequest{
				XMLName: xml.Name{Local: "Control"}, CmdType: "DeviceConfig", SN: 1, DeviceID: gb10DeviceID,
			}
			switch test.section {
			case "SVACEncodeConfig":
				input.SVACEncodeConfig = &SVACEncodeConfig{InnerXML: test.value}
				cascade.SVACEncodeConfig = &SVACEncodeConfig{InnerXML: test.value}
			case "SVACDecodeConfig":
				input.SVACDecodeConfig = &SVACDecodeConfig{InnerXML: test.value}
				cascade.SVACDecodeConfig = &SVACDecodeConfig{InnerXML: test.value}
			default:
				t.Fatalf("unsupported test section %s", test.section)
			}
			_, directErr := api.buildDeviceConfigRequest(gb10DeviceID, nil, input)
			cascadeErr := validateCascadeDeviceConfigRequest(cascade, test.version)
			if (directErr == nil) != test.valid {
				t.Fatalf("direct validation error = %v, want valid %v", directErr, test.valid)
			}
			if (cascadeErr == nil) != test.valid {
				t.Fatalf("cascade validation error = %v, want valid %v", cascadeErr, test.valid)
			}
		})
	}
}

func TestConfigDownloadFailureAndChildBasicParamDoNotChangeParentRuntime(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10ChannelID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, 2), pending)
	for _, body := range []string{
		`<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result></Response>`,
		`<Response><CmdType>ConfigDownload</CmdType><SN>2</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name><DeviceID>` + gb10ChannelID + `</DeviceID><SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP><SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration><Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam></Response>`,
	} {
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-no-parent-update", []byte(body), api.sipMessageConfigDownload)
		assertFlowOK(t, response)
	}
	state := memory.runtime.runtimeSnapshot()
	if state.KeepaliveInterval != 0 || state.KeepaliveTimeout != 0 {
		t.Fatalf("failed/child ConfigDownload changed parent runtime: %+v", state)
	}
	channelState, ok := api.GetQueryState(gb10ChannelID)
	if !ok || channelState.ConfigDownload == nil || channelState.ConfigDownload.BasicParam == nil ||
		channelState.ConfigDownload.DeviceID != gb10ChannelID || channelState.ConfigDownload.BasicParam.Name != "IPC" {
		t.Fatalf("child ConfigDownload state = %+v", channelState)
	}
	if parentState, ok := api.GetQueryState(gb10DeviceID); ok && parentState.ConfigDownload != nil {
		t.Fatalf("child ConfigDownload overwrote parent query state: %+v", parentState.ConfigDownload)
	}
}

func TestConfigDownload30UsesStandardSnapShotResponseName(t *testing.T) {
	canonical, ok := normalizeConfigTypes("snapshot/SnapShotConfig")
	if !ok || canonical != "SnapShotConfig" {
		t.Fatalf("snapshot config normalization = %q, %v", canonical, ok)
	}
	standard := decodeConfigDownloadState([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>10</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShot><SnapNum>2</SnapNum><Interval>3</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>standard</SessionID></SnapShot></Response>`))
	if standard == nil || standard.SnapShot == nil || standard.SnapShot.SnapNum != 2 || standard.SnapShot.SessionID != "standard" {
		t.Fatalf("standard SnapShot state = %+v", standard)
	}
	legacy := decodeConfigDownloadState([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>11</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SnapShotConfig><SnapNum>1</SnapNum><Interval>1</Interval><UploadURL>https://example.invalid/upload</UploadURL><SessionID>legacy</SessionID></SnapShotConfig></Response>`))
	if legacy == nil || legacy.SnapShot == nil || legacy.SnapShot.SessionID != "legacy" {
		t.Fatalf("legacy SnapShotConfig state = %+v", legacy)
	}
	if types := configDownloadStateTypes(standard); len(types) != 1 || types[0] != "SnapShotConfig" {
		t.Fatalf("snapshot config response types = %v", types)
	}
}

func TestConfigDownload30AcceptsStandardSnapShotResponse(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, CMDTypeConfigDownload, 12), pending)
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

func TestConfigDownload30AggregatesAppendixA4AcrossResponses(t *testing.T) {
	api := &GB28181API{}
	pending := &pendingQueryWait{
		wait: make(chan *DeviceQueryOutput, 1),
		expectedConfig: map[string]struct{}{
			"BasicParam": {}, "VideoParamOpt": {},
		},
	}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "ConfigDownload", 19), pending)
	api.resolvePendingDeviceQueryResult(gb10DeviceID, "ConfigDownload", 19, "OK", []byte("<Response><BasicParam/></Response>"), gb10DeviceID, decodedDeviceQuery{
		data:       &ConfigDownloadState{BasicParam: &BasicParam{Name: "IPC"}},
		appendixA4: []AppendixA4Object{{Type: "personType", RawXML: "<Info type=\"personType\"/>"}},
	})
	api.resolvePendingDeviceQueryResult(gb10DeviceID, "ConfigDownload", 19, "OK", []byte("<Response><VideoParamOpt/></Response>"), gb10DeviceID, decodedDeviceQuery{
		data:       &ConfigDownloadState{VideoParamOpt: &VideoParamOpt{InnerXML: "<VideoFormatOpt>2/5</VideoFormatOpt>"}},
		appendixA4: []AppendixA4Object{{Type: "rectType", RawXML: "<Info type=\"rectType\"/>"}},
	})
	select {
	case out := <-pending.wait:
		if len(out.AppendixA4) != 2 || out.AppendixA4[0].Type != "personType" || out.AppendixA4[1].Type != "rectType" {
			t.Fatalf("aggregated ConfigDownload Appendix A.4 = %+v", out.AppendixA4)
		}
	default:
		t.Fatal("combined query did not complete after all Appendix A.4 responses")
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
