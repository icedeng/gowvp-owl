package gbs

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func float64Pointer(value float64) *float64 { return &value }
func intPointer(value int) *int             { return &value }

func TestDeviceControlStandardParameterRanges(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	for _, input := range []*DeviceControlInput{
		{HomePosition: &HomePositionParam{Enabled: intPointer(-1)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(2)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(1), PresetIndex: intPointer(-1)}},
		{HomePosition: &HomePositionParam{Enabled: intPointer(1), PresetIndex: intPointer(256)}},
	} {
		if err := api.fillDeviceControlRequest("device", deviceControlActionHomePosition, input, &deviceControlA23Request{}); err == nil {
			t.Fatalf("invalid HomePosition accepted: %+v", input.HomePosition)
		}
	}
	for _, preset := range []int{0, 255} {
		input := &DeviceControlInput{HomePosition: &HomePositionParam{Enabled: intPointer(1), PresetIndex: intPointer(preset)}}
		if err := api.fillDeviceControlRequest("device", deviceControlActionHomePosition, input, &deviceControlA23Request{}); err != nil {
			t.Fatalf("valid HomePosition preset %d rejected: %v", preset, err)
		}
	}
	legacyAlarm := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionAlarmReset,
		&DeviceControlInput{AlarmMethod: "vendor", AlarmType: "vendor"}, legacyAlarm); err != nil {
		t.Fatalf("2.0 compatible alarm reset extension rejected: %v", err)
	}
	if err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl,
		&DeviceControlInput{PTZCmd: "A50F0100000000B5", PTZCmdParam: &PTZCmdParam{PresetName: "gate"}},
		&deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 PTZ command parameters")
	}

	memory.device.setGBVersion(GBVersion30)
	for _, input := range []*DeviceControlInput{
		{PTZPrecise: &PTZPreciseParam{Pan: float64Pointer(-0.01)}},
		{PTZPrecise: &PTZPreciseParam{Pan: float64Pointer(360.01)}},
		{PTZPrecise: &PTZPreciseParam{Pan: float64Pointer(math.NaN())}},
		{PTZPrecise: &PTZPreciseParam{Tilt: float64Pointer(math.Inf(-1))}},
		{PTZPrecise: &PTZPreciseParam{Zoom: float64Pointer(math.Inf(1))}},
	} {
		if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, input, &deviceControlA23Request{}); err == nil {
			t.Fatalf("invalid PTZPrecise accepted: %+v", input.PTZPrecise)
		}
	}
	valid := &DeviceControlInput{PTZPrecise: &PTZPreciseParam{
		Pan: float64Pointer(360), Tilt: float64Pointer(-45), Zoom: float64Pointer(1),
	}}
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, valid, &deviceControlA23Request{}); err != nil {
		t.Fatalf("valid PTZPrecise boundary rejected: %v", err)
	}
	for _, input := range []*DeviceControlInput{
		{AlarmMethod: "8"},
		{AlarmMethod: "2//5"},
		{AlarmMethod: "0/2"},
		{AlarmMethod: "2/2"},
		{AlarmMethod: "2/5", AlarmType: "1"},
		{AlarmMethod: "1", AlarmType: "1"},
		{AlarmMethod: "2", AlarmType: "6"},
		{AlarmMethod: "5", AlarmType: "14"},
		{AlarmMethod: "6", AlarmType: "3"},
		{AlarmType: "1"},
	} {
		if err := api.fillDeviceControlRequest("device", deviceControlActionAlarmReset, input, &deviceControlA23Request{}); err == nil {
			t.Fatalf("invalid 3.0 alarm reset accepted: %+v", input)
		}
	}
	for _, input := range []*DeviceControlInput{
		{AlarmMethod: "0"},
		{AlarmMethod: "1/2/5/7"},
		{AlarmMethod: "2", AlarmType: "5"},
		{AlarmMethod: "5", AlarmType: "13"},
		{AlarmMethod: "6", AlarmType: "2"},
	} {
		request := &deviceControlA23Request{}
		if err := api.fillDeviceControlRequest("device", deviceControlActionAlarmReset, input, request); err != nil || request.Info == nil {
			t.Fatalf("valid 3.0 alarm reset rejected: %+v, err = %v", input, err)
		}
	}
	for _, length := range []int{32, 33} {
		request := &deviceControlA23Request{}
		err := api.fillDeviceControlRequest("device", deviceControlActionCameraControl,
			&DeviceControlInput{PTZCmd: "A50F0100000000B5", PTZCmdParam: &PTZCmdParam{CruiseTrackName: strings.Repeat("a", length)}}, request)
		if length == 32 && (err != nil || request.PTZCmdParams == nil) {
			t.Fatalf("32-byte cruise track name rejected: %v", err)
		}
		if length == 33 && err == nil {
			t.Fatal("33-byte cruise track name accepted")
		}
	}
}

func TestGB20And30FeatureRegression(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	if err := api.requireGBFeature("device", "voice_intercom", "语音对讲", func(c GBCapabilities) bool { return c.VoiceIntercom }); err != nil {
		t.Fatalf("2.0 voice intercom rejected: %v", err)
	}
	if err := api.requireGBFeature("device", "direct_tcp_download", "直接 TCP", func(c GBCapabilities) bool { return c.DirectTCPDownload }); err == nil {
		t.Fatal("2.0 must not inherit the 1.1 direct TCP profile")
	}
	pan := 12.5
	precise := &DeviceControlInput{PTZPrecise: &PTZPreciseParam{Pan: &pan}}
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, precise, &deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 precise PTZ")
	}
	if err := api.fillDeviceControlRequest("device", deviceControlActionFormatSDCard, &DeviceControlInput{SDCardID: 1}, &deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 SD card formatting")
	}
	track := &DeviceControlInput{TargetTrack: &TargetTrackParam{Mode: "Manual", TargetArea: &DragZoomParam{Length: 1920, Width: 1080, MidPointX: 960, MidPointY: 540, LengthX: 300, LengthY: 200}}}
	if err := api.fillDeviceControlRequest("device", deviceControlActionTargetTrack, track, &deviceControlA23Request{}); err == nil {
		t.Fatal("2.0 must reject 3.0 target tracking")
	}
	for _, action := range []string{deviceQueryActionPTZPosition, deviceQueryActionSDCardStatus, deviceQueryActionCruiseTrackList, deviceQueryActionCruiseTrack} {
		if _, err := api.resolveDeviceQueryCmdType("device", action, ""); err == nil {
			t.Fatalf("2.0 must reject 3.0 query %s", action)
		}
	}
	if err := api.requireConfigTypeVersion("device", "SnapShotConfig"); err == nil {
		t.Fatal("2.0 must reject 3.0 snapshot configuration")
	}
	if _, err := api.Upgrade(context.Background(), &UpgradeInput{DeviceID: "device", ChannelID: "channel"}); err == nil || !strings.Contains(err.Error(), "2022") {
		t.Fatalf("2.0 upgrade gate error = %v", err)
	}

	memory.device.setGBVersion(GBVersion30)
	preciseRequest := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionPTZPrecise, precise, preciseRequest); err != nil || preciseRequest.PTZPreciseCtrl == nil {
		t.Fatalf("3.0 precise PTZ request = %+v, err = %v", preciseRequest, err)
	}
	sdRequest := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionFormatSDCard, &DeviceControlInput{SDCardID: 1}, sdRequest); err != nil || sdRequest.FormatSDCard == nil || *sdRequest.FormatSDCard != 1 {
		t.Fatalf("3.0 SD card request = %+v, err = %v", sdRequest, err)
	}
	trackRequest := &deviceControlA23Request{}
	if err := api.fillDeviceControlRequest("device", deviceControlActionTargetTrack, track, trackRequest); err != nil ||
		trackRequest.TargetTrack != "Manual" || trackRequest.TargetArea == nil {
		t.Fatalf("3.0 target track request = %+v, err = %v", trackRequest, err)
	}
	for action, want := range map[string]string{
		deviceQueryActionPTZPosition:     "PTZPosition",
		deviceQueryActionSDCardStatus:    "SDCardStatus",
		deviceQueryActionCruiseTrackList: "CruiseTrackListQuery",
		deviceQueryActionCruiseTrack:     "CruiseTrackQuery",
	} {
		got, err := api.resolveDeviceQueryCmdType("device", action, "")
		if err != nil || got != want {
			t.Fatalf("3.0 query %s = %q, %v; want %q", action, got, err, want)
		}
	}
	if err := api.requireConfigTypeVersion("device", "SnapShotConfig"); err != nil {
		t.Fatalf("3.0 snapshot config rejected: %v", err)
	}
	if normalized, ok := normalizeConfigType("SnapShot"); !ok || normalized != "SnapShotConfig" {
		t.Fatalf("legacy snapshot config alias = %q, %v", normalized, ok)
	}
	if _, err := api.Upgrade(context.Background(), &UpgradeInput{DeviceID: "device", ChannelID: "channel"}); err == nil || !strings.Contains(err.Error(), "firmware/file_url/manufacturer") {
		t.Fatalf("3.0 upgrade did not pass version gate: %v", err)
	}

	body, err := buildGBSDP(gbSDPInput{
		Version: GBVersion30, SessionName: historyModePlayback,
		ChannelID: gb10ChannelID, URI: gb10ChannelID + ":0",
		IP: "192.0.2.20", Port: 30000, StreamMode: 1,
		StartAt: time.Unix(1711929600, 0), EndAt: time.Unix(1711933200, 0), SSRC: "1100000001",
	})
	if err != nil || !strings.Contains(string(body), "TCP/RTP/AVP") {
		t.Fatalf("3.0 RTP over TCP SDP = %s, err = %v", body, err)
	}
}

func TestGB30AppendixA4Regression(t *testing.T) {
	body := []byte(`<Notify><CmdType>Alarm</CmdType><Info><alarmType level="2"><Code>door_open</Code><VendorField>retained</VendorField></alarmType><behavioralEventType><Code>loitering</Code></behavioralEventType></Info></Notify>`)
	objects := (&GB28181API{}).decodeAppendixA4Objects("Alarm", body)
	if len(objects) != 2 {
		t.Fatalf("A.4 objects = %+v", objects)
	}
	object := objects[0]
	if object.Type != "alarmType" || object.CmdType != "Alarm" || object.Fields["Code"] != "door_open" || object.Fields["@level"] != "2" || !strings.Contains(object.RawXML, "VendorField") {
		t.Fatalf("A.4 object = %+v", object)
	}
	if objects[1].Type != "behavioralEventType" || objects[1].Fields["Code"] != "loitering" {
		t.Fatalf("standard behavioralEventType = %+v", objects[1])
	}
}

func TestGB30AppendixA4ExtraInfoJSONPreservesNumbersAndArrays(t *testing.T) {
	body := []byte(`<Notify><CmdType>Alarm</CmdType><Info><ExtraInfo>[{"type":"doorType","DeviceID":"34020000001320000001","Sequence":100,"Zero":0,"Enabled":true}]</ExtraInfo></Info></Notify>`)
	objects := (&GB28181API{}).decodeAppendixA4Objects("Alarm", body)
	if len(objects) != 1 {
		t.Fatalf("A.4 ExtraInfo objects = %+v", objects)
	}
	object := objects[0]
	if object.Type != "doorType" || object.Fields["[0].DeviceID"] != "34020000001320000001" ||
		object.Fields["[0].Sequence"] != "100" || object.Fields["[0].Zero"] != "0" || object.Fields["[0].Enabled"] != "true" {
		t.Fatalf("A.4 ExtraInfo fields = %+v", object.Fields)
	}
}

func TestGB30PTZPositionNotifyStoresStructuredState(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	conn := newFlowConnection()
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Pan>12.5</Pan><Tilt>-3.25</Tilt><Zoom>2</Zoom></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodNotify, "ptz-position-notify", body, api.sipMessageQueryGeneric)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.PTZPosition == nil || state.PTZPosition.Pan == nil || *state.PTZPosition.Pan != 12.5 ||
		state.PTZPosition.Tilt == nil || *state.PTZPosition.Tilt != -3.25 || state.PTZPosition.Zoom == nil || *state.PTZPosition.Zoom != 2 {
		t.Fatalf("PTZPosition state = %+v", state)
	}
}

func TestGenericQueryResponseRejectsInvalidEnvelopeVersionAndTarget(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion20)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PTZPosition", 72), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid root", body: `<Query><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`},
		{name: "notify root over message", body: `<Notify><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status></Notify>`},
		{name: "non-positive SN", body: `<Response><CmdType>DeviceStatus</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Response>`},
		{name: "missing device", body: `<Response><CmdType>DeviceStatus</CmdType><SN>1</SN></Response>`},
		{name: "unknown command", body: `<Response><CmdType>Unknown</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Response>`},
		{name: "newer-version command", body: `<Notify><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Pan>1</Pan></Notify>`},
		{name: "unknown target", body: `<Response><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "generic-invalid-"+test.name, []byte(test.body), api.sipMessageQueryGeneric)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid generic query response = %s", response)
			}
		})
	}
	select {
	case out := <-pending.wait:
		t.Fatalf("invalid response resolved pending query: %+v", out)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && (state.DeviceStatus != nil || state.PTZPosition != nil) {
		t.Fatalf("invalid generic query response changed state: %+v", state)
	}
}

func TestGenericQueryNotifyDoesNotResolveMessagePending(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10ChannelID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PTZPosition", 72), pending)
	body := []byte(`<Notify><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Pan>1</Pan></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "ptz-notify-not-query-response", body, api.sipMessageQueryGeneric)
	assertFlowOK(t, response)
	select {
	case output := <-pending.wait:
		t.Fatalf("PTZPosition NOTIFY resolved MESSAGE query: %+v", output)
	default:
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.PTZPosition == nil || state.PTZPosition.Pan == nil || *state.PTZPosition.Pan != 1 {
		t.Fatalf("valid PTZPosition NOTIFY state = %+v", state)
	}
}

func TestGenericQueryNotifyRejectsResponseRoot(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	body := []byte(`<Response><CmdType>PTZPosition</CmdType><SN>72</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Pan>1</Pan></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "ptz-notify-response-root", body, api.sipMessageQueryGeneric)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("PTZPosition NOTIFY Response root = %s", response)
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.PTZPosition != nil {
		t.Fatalf("invalid PTZPosition NOTIFY changed state: %+v", state.PTZPosition)
	}
}

func TestGenericQueryResponseRejectsSiblingPendingTargetBeforeState(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	firstChannelID := gb10ChannelID
	secondChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(firstChannelID, &Channel{ChannelID: firstChannelID, device: memory.runtime})
	memory.runtime.Channels.Store(secondChannelID, &Channel{ChannelID: secondChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: firstChannelID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PTZPosition", 76), pending)
	body := []byte(`<Response><CmdType>PTZPosition</CmdType><SN>76</SN><DeviceID>` + secondChannelID + `</DeviceID><Pan>1</Pan></Response>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "query-sibling-target", body, api.sipMessageQueryGeneric)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling query response = %s", response)
	}
	select {
	case out := <-pending.wait:
		t.Fatalf("sibling response resolved pending query: %+v", out)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.PTZPosition != nil {
		t.Fatalf("sibling response changed query state: %+v", state.PTZPosition)
	}
}

func TestDeviceStatusResponseValidatesRequiredFieldsBeforeStateAndRuntime(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)
	initialOnline := memory.device.runtimeSnapshot().IsOnline
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceStatus", 81), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "missing result", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Online>ONLINE</Online><Status>OK</Status></Response>`},
		{name: "invalid result", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>SUCCESS</Result><Online>ONLINE</Online><Status>OK</Status></Response>`},
		{name: "invalid online", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ON</Online><Status>OK</Status></Response>`},
		{name: "invalid status", body: `<Response><CmdType>DeviceStatus</CmdType><SN>81</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>ON</Status></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-status-invalid-"+test.name, []byte(test.body), api.sipMessageQueryGeneric)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid DeviceStatus response = %s", response)
			}
		})
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("invalid DeviceStatus resolved pending query: %+v", output)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
		t.Fatalf("invalid DeviceStatus changed state: %+v", state.DeviceStatus)
	}
	if memory.device.runtimeSnapshot().IsOnline != initialOnline {
		t.Fatal("invalid DeviceStatus changed device runtime")
	}
}

func TestDeviceStatusFailureAndChildResponseDoNotChangeParentRuntime(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion10)
	memory.runtime.UpdateRuntime(func(device *Device) { device.IsOnline = false })
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	for _, body := range []string{
		`<Response><CmdType>DeviceStatus</CmdType><SN>82</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><Online>ONLINE</Online><Status>OK</Status></Response>`,
		`<Response><CmdType>DeviceStatus</CmdType><SN>83</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status></Response>`,
	} {
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-status-no-parent-update", []byte(body), api.sipMessageQueryGeneric)
		assertFlowOK(t, response)
	}
	if memory.runtime.runtimeSnapshot().IsOnline {
		t.Fatal("failed/child DeviceStatus changed parent runtime")
	}
}

func TestGB30VideoUploadNotifyStoresStructuredState(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "VideoUploadNotify", 108), pending)
	defer api.pendingDeviceQuery.Delete(buildPendingQueryKey(gb10DeviceID, "VideoUploadNotify", 108))
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>108</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-notify", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.VideoUpload == nil || state.VideoUpload.Time != "2026-08-25T08:48:00" ||
		state.VideoUpload.Longitude == nil || *state.VideoUpload.Longitude != 120.12 {
		t.Fatalf("VideoUploadNotify state = %+v, %v", state, ok)
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("VideoUploadNotify resolved query pending: %+v", output)
	default:
	}
	memory.device.setGBVersion(GBVersion20)
	response = runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-old", body, api.sipMessageVideoUploadNotify)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("2.0 VideoUploadNotify response = %s", response)
	}
}

func TestGB30VideoUploadNotifyRejectsSchemaAndTargetViolations(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong command", body: `<Notify><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time></Notify>`},
		{name: "missing device", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><Time>2026-08-25T08:48:00</Time></Notify>`},
		{name: "invalid time", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>not-a-time</Time></Notify>`},
		{name: "non-finite longitude", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T08:48:00</Time><Longitude>NaN</Longitude></Notify>`},
		{name: "unknown target", body: `<Notify><CmdType>VideoUploadNotify</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><Time>2026-08-25T08:48:00</Time></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "video-upload-invalid-"+test.name, []byte(test.body), api.sipMessageVideoUploadNotify)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid VideoUploadNotify response = %s", response)
			}
		})
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.VideoUpload != nil {
		t.Fatalf("invalid VideoUploadNotify changed state: %+v", state.VideoUpload)
	}
}

func TestGB30VideoUploadNotifyAcceptsOwnedChannel(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>109</SN><DeviceID>` + gb10ChannelID +
		`</DeviceID><Time>2026-08-25T08:49:00.123</Time><Longitude>120.12</Longitude></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "video-upload-channel", body, api.sipMessageVideoUploadNotify)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.VideoUpload == nil || state.VideoUpload.Time != "2026-08-25T08:49:00.123" {
		t.Fatalf("owned-channel VideoUploadNotify state = %+v, %v", state, ok)
	}
}

func TestGB30CruiseTrackQueriesDecodeStructuredState(t *testing.T) {
	api := &GB28181API{}
	listBody := []byte(`<Response><CmdType>CruiseTrackListQuery</CmdType><SN>74</SN><DeviceID>` + gb10ChannelID + `</DeviceID><SumNum>2</SumNum><CruiseTrackList Num="2"><CruiseTrack><Number>0</Number><Name>白天</Name></CruiseTrack><CruiseTrack><Number>1</Number><Name>夜间</Name></CruiseTrack></CruiseTrackList></Response>`)
	list := api.decodeAndStoreQueryResult(gb10DeviceID, "CruiseTrackListQuery", listBody).data
	tracks, ok := list.([]CruiseTrackData)
	if !ok || len(tracks) != 2 || tracks[0].Number != 0 || tracks[0].Name != "白天" || tracks[1].Number != 1 {
		t.Fatalf("CruiseTrackListQuery data = %+v", list)
	}

	detailBody := []byte(`<Response><CmdType>CruiseTrackQuery</CmdType><SN>75</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Number>1</Number><Name>夜间</Name><SumNum>2</SumNum><CruisePointList Num="2"><CruisePoint><PresetIndex>3</PresetIndex><StayTime>10</StayTime><Speed>5</Speed></CruisePoint><CruisePoint><PresetIndex>7</PresetIndex><StayTime>20</StayTime><Speed>8</Speed></CruisePoint></CruisePointList></Response>`)
	detail := api.decodeAndStoreQueryResult(gb10DeviceID, "CruiseTrackQuery", detailBody).data
	track, ok := detail.(*CruiseTrackData)
	if !ok || track.Number != 1 || track.Name != "夜间" || len(track.Points) != 2 || track.Points[0].PresetIndex != 3 || track.Points[1].Speed != 8 {
		t.Fatalf("CruiseTrackQuery data = %+v", detail)
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || len(state.CruiseTracks) != 2 || state.CruiseTrack == nil || state.CruiseTrack.Number != 1 {
		t.Fatalf("cruise query state = %+v", state)
	}
}

func TestGB20MobilePositionNotifyStoresStructuredState(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion20)
	conn := newFlowConnection()
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>73</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-25T12:00:00</Time><Longitude>120.5</Longitude><Latitude>30.25</Latitude><Speed>18.5</Speed><Direction>90</Direction></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodNotify, "mobile-position-notify", body, api.sipNotifyMobilePosition)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.MobilePosition == nil || state.MobilePosition.Longitude == nil || *state.MobilePosition.Longitude != 120.5 ||
		state.MobilePosition.Latitude == nil || *state.MobilePosition.Latitude != 30.25 || state.MobilePosition.Speed == nil || *state.MobilePosition.Speed != 18.5 {
		t.Fatalf("MobilePosition state = %+v", state)
	}
}

func TestGB30SIPTLSListenPlanDoesNotReuseTCPPort(t *testing.T) {
	plain := buildSIPListenPlan(conf.SIP{Port: 5060})
	if !plain.PlainTCP || plain.TLS {
		t.Fatalf("plain listen plan = %+v", plain)
	}
	shared := buildSIPListenPlan(conf.SIP{Port: 5060, EnableTLS: true})
	if shared.PlainTCP || !shared.TLS || shared.TLSPort != 5060 {
		t.Fatalf("shared TLS listen plan = %+v", shared)
	}
	separate := buildSIPListenPlan(conf.SIP{Port: 5060, EnableTLS: true, TLSPort: 5061})
	if !separate.PlainTCP || !separate.TLS || separate.TLSPort != 5061 {
		t.Fatalf("separate TLS listen plan = %+v", separate)
	}
}
