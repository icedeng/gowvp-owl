package gbs

import (
	"context"
	"encoding/xml"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestValidateCascadeDeviceControlVersionAndScope(t *testing.T) {
	ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionLeft, Speed: 40})
	if err != nil {
		t.Fatal(err)
	}
	base := deviceControlA23Request{
		XMLName: xml.Name{Local: "Control"}, CmdType: ptzCmdTypeDeviceControl, SN: 1,
		DeviceID: testExposedChannelID, PTZCmd: ptz,
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		if err := validateCascadeDeviceControl(&base, version, version); err != nil {
			t.Fatalf("base PTZ rejected for %s: %v", version, err)
		}
	}
	record := base
	record.PTZCmd = ""
	record.RecordCmd = "Record"
	streamNumber := 3
	record.StreamNumber = &streamNumber
	if err := validateCascadeDeviceControl(&record, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("2022 StreamNumber 3 rejected: %v", err)
	}
	if err := validateCascadeDeviceControl(&record, GBVersion20, GBVersion30); err == nil {
		t.Fatal("2016 upstream StreamNumber was accepted")
	}
	if err := validateCascadeDeviceControl(&record, GBVersion30, GBVersion20); err == nil {
		t.Fatal("non-zero StreamNumber was accepted by a 2016 downstream")
	}
	streamNumber = -1
	if err := validateCascadeDeviceControl(&record, GBVersion30, GBVersion30); err == nil {
		t.Fatal("negative cascade StreamNumber was accepted")
	}
	streamNumber = 0
	if err := validateCascadeDeviceControl(&record, GBVersion30, GBVersion20); err != nil {
		t.Fatalf("default StreamNumber could not downgrade to 2016: %v", err)
	}
	if err := translateCascadeStreamNumber(&record, GBVersion20); err != nil || record.StreamNumber != nil {
		t.Fatalf("default StreamNumber downgrade = %+v, err = %v", record.StreamNumber, err)
	}
	ptzParams := base
	cruise, err := encodePTZCommand(&PTZInput{Action: PTZActionCruiseStart})
	if err != nil {
		t.Fatal(err)
	}
	ptzParams.PTZCmd = cruise
	ptzParams.PTZCmdParams = &deviceControlA23PTZCmdParam{CruiseTrackName: strings.Repeat("a", 32)}
	if err := validateCascadeDeviceControl(&ptzParams, GBVersion20, GBVersion20); err == nil {
		t.Fatal("2.0 accepted 3.0 PTZCmdParams")
	}
	if err := validateCascadeDeviceControl(&ptzParams, GBVersion30, GBVersion20); err == nil {
		t.Fatal("2.0 downstream accepted 3.0 PTZCmdParams")
	}
	if err := validateCascadeDeviceControl(&ptzParams, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("3.0 PTZCmdParams with a 32-byte cruise track name rejected: %v", err)
	}
	ptzParams.PTZCmdParams = &deviceControlA23PTZCmdParam{CruiseTrackName: strings.Repeat("a", 33)}
	if err := validateCascadeDeviceControl(&ptzParams, GBVersion30, GBVersion30); err == nil {
		t.Fatal("33-byte CruiseTrackName was accepted")
	}
	ptzParams.PTZCmdParams = &deviceControlA23PTZCmdParam{CruiseTrackName: strings.Repeat("轨", 11)}
	if err := validateCascadeDeviceControl(&ptzParams, GBVersion30, GBVersion30); err == nil {
		t.Fatal("33-byte UTF-8 CruiseTrackName was accepted")
	}
	ptzParams.PTZCmd = ""
	ptzParams.PTZCmdParams = &deviceControlA23PTZCmdParam{PresetName: "entrance"}
	if err := validateCascadeDeviceControl(&ptzParams, GBVersion30, GBVersion30); err == nil {
		t.Fatal("orphan PTZCmdParams was accepted")
	}
	ptzParams.PTZCmdParams = &deviceControlA23PTZCmdParam{}
	if err := validateCascadeDeviceControl(&ptzParams, GBVersion20, GBVersion20); err == nil {
		t.Fatal("2.0 accepted an empty 3.0 PTZCmdParams element")
	}
	badChecksum := base
	badChecksum.PTZCmd = ptz[:14] + "00"
	if err := validateCascadeDeviceControl(&badChecksum, GBVersion20, GBVersion20); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad PTZ checksum error = %v", err)
	}
	badVersion := base
	badVersion.PTZCmd = "A50E0100000000B4"
	if err := validateCascadeDeviceControl(&badVersion, GBVersion20, GBVersion20); err == nil {
		t.Fatal("PTZCmd with an invalid version byte was accepted")
	}
	targetArea := &deviceControlA23DragZoom{
		Length: 1920, Width: 1080, MidPointX: 960, MidPointY: 540, LengthX: 300, LengthY: 200,
	}
	manualTrack := base
	manualTrack.PTZCmd = ""
	manualTrack.TargetTrack = "Manual"
	manualTrack.TargetArea = targetArea
	if err := validateCascadeDeviceControl(&manualTrack, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("valid manual TargetTrack rejected: %v", err)
	}
	manualTrack.TargetArea = nil
	if err := validateCascadeDeviceControl(&manualTrack, GBVersion30, GBVersion30); err == nil {
		t.Fatal("manual TargetTrack without TargetArea was accepted")
	}
	for _, mode := range []string{"Auto", "Stop"} {
		track := manualTrack
		track.TargetTrack = mode
		track.TargetArea = targetArea
		if err := validateCascadeDeviceControl(&track, GBVersion30, GBVersion30); err == nil {
			t.Fatalf("cascade %s TargetTrack accepted manual-only TargetArea", mode)
		}
		track.TargetArea = nil
		if err := validateCascadeDeviceControl(&track, GBVersion30, GBVersion30); err != nil {
			t.Fatalf("cascade %s TargetTrack without TargetArea rejected: %v", mode, err)
		}
	}
	presetSet, err := encodePTZCommand(&PTZInput{Action: PTZActionPresetSet, Preset: 1})
	if err != nil {
		t.Fatal(err)
	}
	presetCall, err := encodePTZCommand(&PTZInput{Action: PTZActionPresetCall, Preset: 1})
	if err != nil {
		t.Fatal(err)
	}
	semanticParams := base
	semanticParams.PTZCmd = presetSet
	semanticParams.PTZCmdParams = &deviceControlA23PTZCmdParam{PresetName: " entrance "}
	if err := validateCascadeDeviceControl(&semanticParams, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("set-preset PresetName rejected: %v", err)
	}
	if semanticParams.PTZCmdParams.PresetName != " entrance " {
		t.Fatalf("cascade PresetName was modified: %q", semanticParams.PTZCmdParams.PresetName)
	}
	semanticParams.PTZCmd = presetCall
	if err := validateCascadeDeviceControl(&semanticParams, GBVersion30, GBVersion30); err == nil {
		t.Fatal("preset-call accepted PresetName")
	}
	semanticParams.PTZCmd = ptz
	semanticParams.PTZCmdParams = &deviceControlA23PTZCmdParam{CruiseTrackName: "day"}
	if err := validateCascadeDeviceControl(&semanticParams, GBVersion30, GBVersion30); err == nil {
		t.Fatal("ordinary PTZ accepted CruiseTrackName")
	}
	semanticParams.PTZCmd = cruise
	semanticParams.PTZCmdParams = &deviceControlA23PTZCmdParam{PresetName: "entrance", CruiseTrackName: "day"}
	if err := validateCascadeDeviceControl(&semanticParams, GBVersion30, GBVersion30); err == nil {
		t.Fatal("PTZCmdParams accepted two command-specific names")
	}
	drag := base
	drag.PTZCmd = ""
	drag.DragZoomIn = &deviceControlA23DragZoom{Length: 100, Width: 100, MidPointX: 50, MidPointY: 50, LengthX: 20, LengthY: 20}
	if err := validateCascadeDeviceControl(&drag, GBVersion10, GBVersion11); err == nil {
		t.Fatal("1.0 upstream accepted 1.1 DragZoom")
	}
	if err := validateCascadeDeviceControl(&drag, GBVersion11, GBVersion11); err != nil {
		t.Fatalf("1.1 DragZoom rejected: %v", err)
	}
	invalidDrag := drag
	invalidDrag.DragZoomIn = &deviceControlA23DragZoom{Length: 100, Width: 100, MidPointX: 101, MidPointY: 50, LengthX: 20, LengthY: 20}
	if err := validateCascadeDeviceControl(&invalidDrag, GBVersion11, GBVersion11); err == nil {
		t.Fatal("invalid DragZoom coordinates were accepted")
	}
	precise := base
	precise.PTZCmd = ""
	pan := 120.5
	precise.PTZPreciseCtrl = &deviceControlA23PTZPrecise{Pan: &pan}
	if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion20); err == nil {
		t.Fatal("2.0 downstream accepted 3.0 precise PTZ")
	}
	if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("3.0 precise PTZ rejected: %v", err)
	}
	invalidPan := math.NaN()
	precise.PTZPreciseCtrl = &deviceControlA23PTZPrecise{Pan: &invalidPan}
	if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion30); err == nil {
		t.Fatal("non-finite precise PTZ was accepted")
	}
	for _, tilt := range []float64{-30.01, 90.01} {
		precise.PTZPreciseCtrl = &deviceControlA23PTZPrecise{Tilt: &tilt}
		if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion30); err == nil {
			t.Fatalf("out-of-range precise PTZ tilt %v was accepted", tilt)
		}
	}
	for _, tilt := range []float64{-30, 90} {
		precise.PTZPreciseCtrl = &deviceControlA23PTZPrecise{Tilt: &tilt}
		if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion30); err != nil {
			t.Fatalf("valid precise PTZ tilt %v was rejected: %v", tilt, err)
		}
	}
	invalidZoom := 0.99
	precise.PTZPreciseCtrl = &deviceControlA23PTZPrecise{Zoom: &invalidZoom}
	if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion30); err == nil {
		t.Fatal("precise PTZ accepted an optical zoom below 1.0")
	}
	validZoom := 1.0
	precise.PTZPreciseCtrl = &deviceControlA23PTZPrecise{Zoom: &validZoom}
	if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("precise PTZ rejected 1.0x optical zoom: %v", err)
	}
	home := base
	home.PTZCmd = ""
	enabled := 1
	negativeReset := -1
	home.HomePosition = &deviceControlA23HomePosition{Enabled: &enabled, ResetTime: &negativeReset}
	if err := validateCascadeDeviceControl(&home, GBVersion20, GBVersion20); err != nil {
		t.Fatalf("standard integer HomePosition reset time was rejected: %v", err)
	}
	disabled := 0
	resetTime := 30
	home.HomePosition = &deviceControlA23HomePosition{Enabled: &disabled, ResetTime: &resetTime}
	if err := validateCascadeDeviceControl(&home, GBVersion20, GBVersion20); err == nil {
		t.Fatal("disabled HomePosition accepted ResetTime")
	}
	presetIndex := 1
	home.HomePosition = &deviceControlA23HomePosition{Enabled: &disabled, PresetIndex: &presetIndex}
	if err := validateCascadeDeviceControl(&home, GBVersion20, GBVersion20); err == nil {
		t.Fatal("disabled HomePosition accepted PresetIndex")
	}
	home.HomePosition = &deviceControlA23HomePosition{Enabled: &disabled}
	if err := validateCascadeDeviceControl(&home, GBVersion20, GBVersion20); err != nil {
		t.Fatalf("valid HomePosition disable rejected: %v", err)
	}
	upgrade := base
	upgrade.PTZCmd = ""
	upgrade.DeviceUpgrade = &deviceUpgradeConfig{
		Firmware: "V3", FileURL: "https://example.invalid/firmware.bin", Manufacturer: "Vendor",
		SessionID: "upgrade-session-0000000000000104",
	}
	if err := validateCascadeDeviceControl(&upgrade, GBVersion20, GBVersion30); err == nil {
		t.Fatal("2.0 upstream accepted 3.0 DeviceUpgrade")
	}
	if err := validateCascadeDeviceControl(&upgrade, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("3.0 DeviceUpgrade rejected: %v", err)
	}
	track := base
	track.PTZCmd = ""
	track.TargetTrack = "Manual"
	track.DeviceID2 = track.DeviceID
	track.TargetArea = &deviceControlA23DragZoom{Length: 1920, Width: 1080, MidPointX: 960, MidPointY: 540, LengthX: 300, LengthY: 200}
	if err := validateCascadeDeviceControl(&track, GBVersion30, GBVersion20); err == nil {
		t.Fatal("2.0 downstream accepted 3.0 TargetTrack")
	}
	if err := validateCascadeDeviceControl(&track, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("3.0 TargetTrack rejected: %v", err)
	}
	invalidTrackArea := track
	invalidTrackArea.TargetArea = &deviceControlA23DragZoom{Length: 1920, Width: 1080, MidPointX: -1, MidPointY: 540, LengthX: 300, LengthY: 200}
	if err := validateCascadeDeviceControl(&invalidTrackArea, GBVersion30, GBVersion30); err == nil {
		t.Fatal("TargetTrack accepted a midpoint outside the playback window")
	}
	crossChannelTrack := track
	crossChannelTrack.DeviceID2 = gb10ChannelID
	if err := validateCascadeDeviceControl(&crossChannelTrack, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("TargetTrack rejected a syntactically valid panorama channel: %v", err)
	}
	invalidTrackDeviceID2 := track
	invalidTrackDeviceID2.DeviceID2 = "invalid"
	if err := validateCascadeDeviceControl(&invalidTrackDeviceID2, GBVersion30, GBVersion30); err == nil {
		t.Fatal("TargetTrack accepted an invalid DeviceID2")
	}
	for name, orphan := range map[string]deviceControlA23Request{
		"StreamNumber": func() deviceControlA23Request {
			request := base
			streamNumber := 1
			request.StreamNumber = &streamNumber
			return request
		}(),
		"Info": func() deviceControlA23Request {
			request := base
			request.Info = &deviceControlA23Info{AlarmMethod: "2"}
			return request
		}(),
		"TargetArea": func() deviceControlA23Request {
			request := base
			request.TargetArea = &deviceControlA23DragZoom{Length: 100, Width: 100, MidPointX: 50, MidPointY: 50, LengthX: 20, LengthY: 20}
			return request
		}(),
	} {
		if err := validateCascadeDeviceControl(&orphan, GBVersion30, GBVersion30); err == nil {
			t.Fatalf("cascade DeviceControl accepted orphan %s", name)
		}
	}
	reboot := base
	reboot.PTZCmd = ""
	reboot.TeleBoot = "Boot"
	if err := validateCascadeDeviceControl(&reboot, GBVersion30, GBVersion30); err == nil || !strings.Contains(err.Error(), "device-scoped") {
		t.Fatalf("device-scoped control error = %v", err)
	}
}

func TestDeviceControlRequestStructureAndExtraInfoVersionMatrix(t *testing.T) {
	ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionLeft, Speed: 40})
	if err != nil {
		t.Fatal(err)
	}
	base := `<Control><CmdType>DeviceControl</CmdType><SN>1</SN><DeviceID>` + testExposedChannelID +
		`</DeviceID><PTZCmd>` + ptz + `</PTZCmd>`
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
		wantOK  bool
	}{
		{name: "2011 multiple plain Info", version: GBVersion10, body: base + `<Info> first </Info><Info></Info></Control>`, wantOK: true},
		{name: "2016 structured and plain Info", version: GBVersion20, body: base + `<AlarmCmd>ResetAlarm</AlarmCmd><HomePosition><Enabled>1</Enabled></HomePosition><Info><AlarmMethod>2</AlarmMethod><AlarmType>1</AlarmType></Info><Info>vendor</Info></Control>`, wantOK: true},
		{name: "2022 multiple ExtraInfo", version: GBVersion30, body: base + `<ExtraInfo> first </ExtraInfo><ExtraInfo></ExtraInfo></Control>`, wantOK: true},
		{name: "2022 rejects plain Info", version: GBVersion30, body: base + `<Info>vendor</Info></Control>`},
		{name: "2016 rejects ExtraInfo", version: GBVersion20, body: base + `<ExtraInfo>vendor</ExtraInfo></Control>`},
		{name: "2016 rejects multiple structured Info", version: GBVersion20, body: base + `<Info><AlarmMethod>2</AlarmMethod></Info><Info><AlarmType>1</AlarmType></Info></Control>`},
		{name: "2022 rejects misspelled ExtraInfo", version: GBVersion30, body: base + `<ExtralInfo>vendor</ExtralInfo></Control>`},
		{name: "duplicate SN", version: GBVersion30, body: `<Control><CmdType>DeviceControl</CmdType><SN>1</SN><SN>2</SN><DeviceID>` + testExposedChannelID + `</DeviceID><PTZCmd>` + ptz + `</PTZCmd></Control>`},
		{name: "unknown field", version: GBVersion30, body: base + `<VendorField>value</VendorField></Control>`},
		{name: "nested ExtraInfo", version: GBVersion30, body: base + `<ExtraInfo><Value>vendor</Value></ExtraInfo></Control>`},
		{name: "long ExtraInfo", version: GBVersion30, body: base + `<ExtraInfo>` + strings.Repeat("测", 1025) + `</ExtraInfo></Control>`},
		{name: "out of order envelope", version: GBVersion30, body: `<Control><CmdType>DeviceControl</CmdType><DeviceID>` + testExposedChannelID + `</DeviceID><SN>1</SN><PTZCmd>` + ptz + `</PTZCmd></Control>`},
		{name: "incomplete drag zoom", version: GBVersion11, body: `<Control><CmdType>DeviceControl</CmdType><SN>1</SN><DeviceID>` + testExposedChannelID + `</DeviceID><DragZoomIn><Length>100</Length><Width>100</Width><MidPointX>50</MidPointX><MidPointY>50</MidPointY><LengthX>10</LengthX></DragZoomIn></Control>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateDeviceControlRequestStructure([]byte(test.body), test.version)
			if test.wantOK && err != nil {
				t.Fatalf("valid DeviceControl structure rejected: %v", err)
			}
			if !test.wantOK && err == nil {
				t.Fatal("invalid DeviceControl structure accepted")
			}
		})
	}

	if err := validateDeviceControlExtraInfo([]string{"", " vendor "}, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("2022 ExtraInfo rejected: %v", err)
	}
	if err := validateDeviceControlExtraInfo([]string{"vendor"}, GBVersion30, GBVersion20); err != nil {
		t.Fatalf("2022 text extension could not be translated for a 2016 peer: %v", err)
	}
	if err := validateDeviceControlExtraInfo([]string{strings.Repeat("测", 1025)}, GBVersion30); err == nil {
		t.Fatal("oversized DeviceControl ExtraInfo accepted")
	}
}

func TestDeviceControlTextInfoEncodingByVersion(t *testing.T) {
	body := []byte(`<Control><CmdType>DeviceControl</CmdType><SN>1</SN><DeviceID>` + testExposedChannelID +
		`</DeviceID><AlarmCmd>ResetAlarm</AlarmCmd><Info><AlarmMethod>2</AlarmMethod></Info>` +
		`<Info> first &amp; &lt; </Info><Info></Info></Control>`)
	var request deviceControlA23Request
	if err := decodeDeviceControlRequest(body, &request); err != nil {
		t.Fatalf("decode 2016 DeviceControl: %v", err)
	}
	if request.Info == nil || request.Info.AlarmMethod != "2" {
		t.Fatalf("decoded structured Info = %+v", request.Info)
	}
	if len(request.LegacyInfo) != 2 || request.LegacyInfo[0] != " first & < " || request.LegacyInfo[1] != "" {
		t.Fatalf("decoded plain Info = %#v", request.LegacyInfo)
	}

	legacyBody, err := encodeDeviceControlRequest(&request, GBVersion20)
	if err != nil {
		t.Fatalf("encode 2016 DeviceControl: %v", err)
	}
	legacyXML := string(legacyBody)
	if strings.Contains(legacyXML, "<ExtraInfo>") || strings.Count(legacyXML, "<Info>") != 3 ||
		!strings.Contains(legacyXML, "<Info> first &amp; &lt; </Info>") || !strings.Contains(legacyXML, "<Info></Info>") {
		t.Fatalf("encoded 2016 DeviceControl text extensions = %s", legacyXML)
	}

	currentBody, err := encodeDeviceControlRequest(&request, GBVersion30)
	if err != nil {
		t.Fatalf("encode 2022 DeviceControl: %v", err)
	}
	currentXML := string(currentBody)
	if strings.Count(currentXML, "<Info>") != 1 || strings.Count(currentXML, "<ExtraInfo>") != 2 ||
		!strings.Contains(currentXML, "<ExtraInfo> first &amp; &lt; </ExtraInfo>") || !strings.Contains(currentXML, "<ExtraInfo></ExtraInfo>") {
		t.Fatalf("encoded 2022 DeviceControl text extensions = %s", currentXML)
	}

	var translated deviceControlA23Request
	if err := decodeDeviceControlRequest(currentBody, &translated); err != nil {
		t.Fatalf("decode translated 2022 DeviceControl: %v", err)
	}
	values := deviceControlTextInfo(&translated)
	if len(values) != 2 || values[0] != " first & < " || values[1] != "" {
		t.Fatalf("translated 2022 text extensions = %#v", values)
	}
}

func TestCascadeIFrameCommandFieldByProtocolVersion(t *testing.T) {
	base := deviceControlA23Request{
		XMLName: xml.Name{Local: "Control"}, CmdType: ptzCmdTypeDeviceControl, SN: 1,
		DeviceID: testExposedChannelID,
	}
	tests := []struct {
		name       string
		request    deviceControlA23Request
		upstream   GBProtocolVersion
		downstream GBProtocolVersion
		wantErr    bool
	}{
		{name: "2016 standard field", request: func() deviceControlA23Request { request := base; request.IFameCmd = "Send"; return request }(), upstream: GBVersion20, downstream: GBVersion20},
		{name: "2016 rejects 2022 field", request: func() deviceControlA23Request { request := base; request.IFrameCmd = "Send"; return request }(), upstream: GBVersion20, downstream: GBVersion20, wantErr: true},
		{name: "2022 standard field", request: func() deviceControlA23Request { request := base; request.IFrameCmd = "Send"; return request }(), upstream: GBVersion30, downstream: GBVersion30},
		{name: "2022 rejects 2016 field", request: func() deviceControlA23Request { request := base; request.IFameCmd = "Send"; return request }(), upstream: GBVersion30, downstream: GBVersion30, wantErr: true},
		{name: "both fields rejected", request: func() deviceControlA23Request {
			request := base
			request.IFameCmd = "Send"
			request.IFrameCmd = "Send"
			return request
		}(), upstream: GBVersion30, downstream: GBVersion30, wantErr: true},
		{name: "2014 rejects force I frame", request: func() deviceControlA23Request { request := base; request.IFameCmd = "Send"; return request }(), upstream: GBVersion11, downstream: GBVersion20, wantErr: true},
		{name: "2016 to 2022", request: func() deviceControlA23Request { request := base; request.IFameCmd = "Send"; return request }(), upstream: GBVersion20, downstream: GBVersion30},
		{name: "2022 to 2016", request: func() deviceControlA23Request { request := base; request.IFrameCmd = "Send"; return request }(), upstream: GBVersion30, downstream: GBVersion20},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCascadeDeviceControl(&test.request, test.upstream, test.downstream)
			if test.wantErr {
				if err == nil {
					t.Fatal("invalid force-I-frame field was accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("valid force-I-frame field rejected: %v", err)
			}
			if err := translateCascadeIFrameCommand(&test.request, test.upstream, test.downstream); err != nil {
				t.Fatalf("translate force-I-frame field: %v", err)
			}
			if test.downstream == GBVersion20 {
				if test.request.IFameCmd != "Send" || test.request.IFrameCmd != "" {
					t.Fatalf("translated 2016 request = %+v", test.request)
				}
			} else if test.request.IFrameCmd != "Send" || test.request.IFameCmd != "" {
				t.Fatalf("translated 2022 request = %+v", test.request)
			}
		})
	}
}

func TestCascadePTZControlPriorityVersionMatrix(t *testing.T) {
	ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionLeft, Speed: 40})
	if err != nil {
		t.Fatal(err)
	}
	base := deviceControlA23Request{
		XMLName: xml.Name{Local: "Control"}, CmdType: ptzCmdTypeDeviceControl, SN: 1,
		DeviceID: testExposedChannelID, PTZCmd: ptz,
		Info: &deviceControlA23Info{ControlPriority: intPointer(5)},
	}
	for _, versions := range [][2]GBProtocolVersion{
		{GBVersion10, GBVersion10}, {GBVersion10, GBVersion11},
		{GBVersion11, GBVersion10}, {GBVersion11, GBVersion11},
	} {
		if err := validateCascadeDeviceControl(&base, versions[0], versions[1]); err != nil {
			t.Fatalf("ControlPriority rejected for %s -> %s: %v", versions[0], versions[1], err)
		}
	}
	for _, versions := range [][2]GBProtocolVersion{
		{GBVersion20, GBVersion20}, {GBVersion30, GBVersion30},
		{GBVersion10, GBVersion20}, {GBVersion30, GBVersion11},
	} {
		if err := validateCascadeDeviceControl(&base, versions[0], versions[1]); err == nil {
			t.Fatalf("ControlPriority accepted for %s -> %s", versions[0], versions[1])
		}
	}
	orphan := base
	orphan.PTZCmd = ""
	if err := validateCascadeDeviceControl(&orphan, GBVersion10, GBVersion11); err == nil {
		t.Fatal("ControlPriority without PTZCmd was accepted")
	}
	mixed := base
	mixed.AlarmCmd = "ResetAlarm"
	mixed.Info = &deviceControlA23Info{ControlPriority: intPointer(5), AlarmMethod: "2"}
	if err := validateCascadeDeviceControl(&mixed, GBVersion20, GBVersion20); err == nil {
		t.Fatal("ControlPriority combined with AlarmCmd Info was accepted")
	}
}

func TestValidateTargetTrackChannelsRequiresKnownSiblingChannels(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.gbVersion = string(GBVersion30)
	memory.runtime.Address = "local.example"
	memory.runtime.LoadChannels(
		&ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID},
		&ipc.Channel{DeviceID: gb10DeviceID, ChannelID: "34020000001320000012"},
	)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	request := &deviceControlA23Request{TargetTrack: "Auto"}
	if err := api.validateTargetTrackChannels(gb10DeviceID, gb10DeviceID, request); err == nil {
		t.Fatal("TargetTrack accepted the parent device as the ball camera channel")
	}
	if err := api.validateTargetTrackChannels(gb10DeviceID, testCascadeChannelID, request); err != nil {
		t.Fatalf("TargetTrack rejected a known ball camera channel: %v", err)
	}
	request.DeviceID2 = testCascadeChannelID
	if err := api.validateTargetTrackChannels(gb10DeviceID, testCascadeChannelID, request); err == nil {
		t.Fatal("TargetTrack accepted the ball camera channel as DeviceID2")
	}
	request.DeviceID2 = "34020000001320000012"
	if err := api.validateTargetTrackChannels(gb10DeviceID, testCascadeChannelID, request); err != nil {
		t.Fatalf("TargetTrack rejected a known panorama sibling channel: %v", err)
	}
	request.DeviceID2 = "34020000001320000013"
	if err := api.validateTargetTrackChannels(gb10DeviceID, testCascadeChannelID, request); err == nil {
		t.Fatal("TargetTrack accepted an unknown panorama channel")
	}
}

func TestResolveCascadeTargetTrackDeviceID2MapsSharedSibling(t *testing.T) {
	adapter, persistentDevice, target := newCascadeMediaCore(t)
	panorama := &ipc.Channel{
		ID: "GBC_cascade_panorama", DID: persistentDevice.ID, DeviceID: persistentDevice.DeviceID,
		ChannelID: "34020000001320000012", Name: "Panorama", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := adapter.Store().Channel().Create(t.Context(), panorama); err != nil {
		t.Fatal(err)
	}
	otherDevice := &ipc.Device{ID: "GB_cascade_other", DeviceID: "34020000002000000002", Type: ipc.TypeGB28181, IsOnline: true}
	if err := adapter.Store().Device().Create(t.Context(), otherDevice); err != nil {
		t.Fatal(err)
	}
	otherChannel := &ipc.Channel{
		ID: "GBC_cascade_other", DID: otherDevice.ID, DeviceID: otherDevice.DeviceID,
		ChannelID: "34020000001320000013", Name: "Other", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := adapter.Store().Channel().Create(t.Context(), otherChannel); err != nil {
		t.Fatal(err)
	}

	platform := testSharedCascadePlatform(t)
	platform.sharedChannels = append(platform.sharedChannels, panorama.ChannelID, otherChannel.ChannelID)
	platform.channelIDMap[panorama.ChannelID] = "34020000001320000912"
	platform.channelIDMap[otherChannel.ChannelID] = "34020000001320000913"
	platform.exposedChannelMap["34020000001320000912"] = panorama.ChannelID
	platform.exposedChannelMap["34020000001320000913"] = otherChannel.ChannelID
	api := &GB28181API{core: adapter}
	request := &deviceControlA23Request{TargetTrack: "Manual", DeviceID2: "34020000001320000912"}

	mapped, err := api.resolveCascadeTargetTrackDeviceID2(t.Context(), platform, target, request)
	if err != nil || mapped != panorama.ChannelID {
		t.Fatalf("mapped panorama channel = %q, err = %v", mapped, err)
	}
	request.DeviceID2 = testExposedChannelID
	if _, err := api.resolveCascadeTargetTrackDeviceID2(t.Context(), platform, target, request); err == nil {
		t.Fatal("TargetTrack accepted the exposed ball camera channel as DeviceID2")
	}
	request.DeviceID2 = "34020000001320000913"
	if _, err := api.resolveCascadeTargetTrackDeviceID2(t.Context(), platform, target, request); err == nil {
		t.Fatal("TargetTrack accepted a panorama channel owned by another device")
	}
	request.DeviceID2 = "34020000001320000914"
	if _, err := api.resolveCascadeTargetTrackDeviceID2(t.Context(), platform, target, request); err == nil {
		t.Fatal("TargetTrack accepted an unshared panorama channel")
	}
}

func TestForwardCascadeTargetTrackMapsPanoramaChannel(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	panorama := &ipc.Channel{
		ID: "GBC_forward_panorama", DID: persistentDevice.ID, DeviceID: persistentDevice.DeviceID,
		ChannelID: "34020000001320000012", Name: "Panorama", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := adapter.Store().Channel().Create(t.Context(), panorama); err != nil {
		t.Fatal(err)
	}
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	platform.sharedChannels = append(platform.sharedChannels, panorama.ChannelID)
	platform.channelIDMap[panorama.ChannelID] = "34020000001320000912"
	platform.exposedChannelMap["34020000001320000912"] = panorama.ChannelID
	runtimeDevice := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	forwarded := make(chan deviceControlA23Request, 1)
	api.cascadeDeviceControl = func(_ context.Context, _ *ipc.Channel, request *deviceControlA23Request) (string, error) {
		forwarded <- *request
		return ptzResultOK, nil
	}
	body, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 79, DeviceID: testExposedChannelID,
		TargetTrack: "Auto", DeviceID2: "34020000001320000912",
	})
	if err != nil {
		t.Fatal(err)
	}
	api.forwardCascadeDeviceControl(worker, body)
	select {
	case request := <-forwarded:
		if request.DeviceID2 != panorama.ChannelID {
			t.Fatalf("forwarded DeviceID2 = %q, want %q", request.DeviceID2, panorama.ChannelID)
		}
	case <-time.After(time.Second):
		t.Fatal("TargetTrack forwarding timeout")
	}
}

func TestDeviceControlBusinessResponseMatrix(t *testing.T) {
	pan := 1.0
	tests := []struct {
		name    string
		request deviceControlA23Request
		want    bool
	}{
		{name: "camera control", request: deviceControlA23Request{PTZCmd: "A50F0100000000B5"}},
		{name: "tele boot", request: deviceControlA23Request{TeleBoot: "Boot"}},
		{name: "record", request: deviceControlA23Request{RecordCmd: "Record"}, want: true},
		{name: "guard", request: deviceControlA23Request{GuardCmd: "SetGuard"}, want: true},
		{name: "alarm reset", request: deviceControlA23Request{AlarmCmd: "ResetAlarm"}, want: true},
		{name: "iframe 2016", request: deviceControlA23Request{IFameCmd: "Send"}},
		{name: "iframe 2022", request: deviceControlA23Request{IFrameCmd: "Send"}},
		{name: "drag zoom in", request: deviceControlA23Request{DragZoomIn: &deviceControlA23DragZoom{}}},
		{name: "drag zoom out", request: deviceControlA23Request{DragZoomOut: &deviceControlA23DragZoom{}}},
		{name: "home position", request: deviceControlA23Request{HomePosition: &deviceControlA23HomePosition{}}, want: true},
		{name: "precise ptz", request: deviceControlA23Request{PTZPreciseCtrl: &deviceControlA23PTZPrecise{Pan: &pan}}},
		{name: "format sd card", request: deviceControlA23Request{FormatSDCard: new(int)}},
		{name: "target track", request: deviceControlA23Request{TargetTrack: "Auto"}},
		{name: "device upgrade", request: deviceControlA23Request{DeviceUpgrade: &deviceUpgradeConfig{}}, want: true},
		{name: "unknown future control", request: deviceControlA23Request{}, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := deviceControlRequiresBusinessResponse(&test.request); got != test.want {
				t.Fatalf("deviceControlRequiresBusinessResponse() = %v, want %v", got, test.want)
			}
		})
	}
	if deviceControlRequiresBusinessResponse(nil) {
		t.Fatal("nil DeviceControl request requires a business response")
	}
}

func TestCascadeDeviceControlRoutesSharedChannelByResponseMode(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	runtimeDevice := &Device{
		IsOnline: true, gbVersion: string(GBVersion11), conn: connection, source: connection.remote,
		to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example"),
	}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}, channel: runtimeChannel}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{cfg: &conf.SIP{}, core: adapter, svr: server}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(server, platform)
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	responses := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		responses <- request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	type forwardedControl struct {
		channel *ipc.Channel
		request deviceControlA23Request
	}
	forwarded := make(chan forwardedControl, 4)
	api.cascadeDeviceControl = func(_ context.Context, channel *ipc.Channel, request *deviceControlA23Request) (string, error) {
		forwarded <- forwardedControl{channel: channel, request: *request}
		return ptzResultOK, nil
	}
	ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionRight, Speed: 40})
	if err != nil {
		t.Fatal(err)
	}
	body, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 73, DeviceID: testExposedChannelID, PTZCmd: ptz,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-control", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-control", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("cascade control SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade control SIP response timeout")
	}
	var ptzForwarded forwardedControl
	select {
	case ptzForwarded = <-forwarded:
	case <-time.After(time.Second):
		t.Fatal("cascade PTZ forwarding timeout")
	}
	if ptzForwarded.channel == nil || ptzForwarded.channel.ChannelID != persistentChannel.ChannelID ||
		ptzForwarded.request.DeviceID != testExposedChannelID || ptzForwarded.request.SN != 73 {
		t.Fatalf("forwarded cascade PTZ = %+v", ptzForwarded)
	}
	select {
	case response := <-responses:
		t.Fatalf("no-response cascade PTZ emitted business response: %s", response.Body())
	case <-time.After(50 * time.Millisecond):
	}

	recordBody, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 74, DeviceID: testExposedChannelID, RecordCmd: "Record",
	})
	if err != nil {
		t.Fatal(err)
	}
	recordRequest := newFlowRequest(t, connection, sip.MethodMessage, "cascade-record-control", recordBody)
	recordCtx := &sip.Context{
		Request: recordRequest, Tx: sip.NewTransaction("cascade-record-control", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	recordCtx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(recordCtx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("cascade record SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade record SIP response timeout")
	}
	select {
	case recordForwarded := <-forwarded:
		if recordForwarded.request.SN != 74 || recordForwarded.request.RecordCmd != "Record" {
			t.Fatalf("forwarded cascade record = %+v", recordForwarded)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade record forwarding timeout")
	}
	select {
	case response := <-responses:
		text := string(response.Body())
		for _, expected := range []string{
			"<CmdType>DeviceControl</CmdType>", "<SN>74</SN>",
			"<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Result>OK</Result>",
		} {
			if !strings.Contains(text, expected) {
				t.Fatalf("cascade record business response missing %q: %s", expected, text)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("cascade record business response timeout")
	}

	runtimeDevice.gbVersion = string(GBVersion20)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	extraBody, err := encodeDeviceControlRequest(&deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 75, DeviceID: testExposedChannelID, PTZCmd: ptz,
		ExtraInfo: []string{" first ", "second"},
	}, GBVersion30)
	if err != nil {
		t.Fatal(err)
	}
	extraRequest := newFlowRequest(t, connection, sip.MethodMessage, "cascade-control-extra-info", extraBody)
	extraCtx := &sip.Context{
		Request: extraRequest, Tx: sip.NewTransaction("cascade-control-extra-info", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	extraCtx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(extraCtx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("cascade ExtraInfo SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade ExtraInfo SIP response timeout")
	}
	select {
	case extraForwarded := <-forwarded:
		values := deviceControlTextInfo(&extraForwarded.request)
		if len(values) != 2 || values[0] != " first " || values[1] != "second" {
			t.Fatalf("forwarded 2022 to 2016 text extensions = %#v", values)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade ExtraInfo forwarding timeout")
	}

	runtimeDevice.gbVersion = string(GBVersion30)
	worker.mu.Lock()
	worker.effective = GBVersion20
	worker.mu.Unlock()
	legacyBody, err := encodeDeviceControlRequest(&deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 76, DeviceID: testExposedChannelID, PTZCmd: ptz,
		LegacyInfo: []string{" legacy ", ""},
	}, GBVersion20)
	if err != nil {
		t.Fatal(err)
	}
	legacyRequest := newFlowRequest(t, connection, sip.MethodMessage, "cascade-control-legacy-info", legacyBody)
	legacyCtx := &sip.Context{
		Request: legacyRequest, Tx: sip.NewTransaction("cascade-control-legacy-info", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	legacyCtx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(legacyCtx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("cascade legacy Info SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade legacy Info SIP response timeout")
	}
	select {
	case legacyForwarded := <-forwarded:
		values := deviceControlTextInfo(&legacyForwarded.request)
		if len(values) != 2 || values[0] != " legacy " || values[1] != "" {
			t.Fatalf("forwarded 2016 to 2022 text extensions = %#v", values)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade legacy Info forwarding timeout")
	}
}

func TestCascadeDeviceControlRejectsInvalidNoResponseCommandBeforeSIPOK(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	runtimeDevice := &Device{
		IsOnline: true, gbVersion: string(GBVersion11), conn: connection, source: connection.remote,
		to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example"),
	}
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{cfg: &conf.SIP{}, core: adapter, svr: server}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(server, platform)
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	invalidPTZ := "A50F01202828F000"
	body, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 75, DeviceID: testExposedChannelID, PTZCmd: invalidPTZ,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-invalid-control", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-invalid-control", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("invalid cascade control SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid cascade control SIP response timeout")
	}
	body, err = sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 76, DeviceID: testExposedChannelID, PTZCmd: "A50F0100000000B5",
		PTZCmdParams: &deviceControlA23PTZCmdParam{},
	})
	if err != nil {
		t.Fatal(err)
	}
	request = newFlowRequest(t, connection, sip.MethodMessage, "cascade-versioned-control", body)
	ctx = &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-versioned-control", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("unsupported cascade PTZCmdParams SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("unsupported cascade PTZCmdParams SIP response timeout")
	}
	runtimeDevice.gbVersion = string(GBVersion30)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	body, err = sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 77, DeviceID: testExposedChannelID, PTZCmd: "A50F0100000000B5",
		PTZCmdParams: &deviceControlA23PTZCmdParam{PresetName: "entrance"},
	})
	if err != nil {
		t.Fatal(err)
	}
	request = newFlowRequest(t, connection, sip.MethodMessage, "cascade-semantic-control", body)
	ctx = &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-semantic-control", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("invalid cascade PTZCmdParams semantics SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid cascade PTZCmdParams semantics SIP response timeout")
	}
	body, err = sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 78, DeviceID: testExposedChannelID,
		TargetTrack: "Auto", DeviceID2: "34020000001320000912",
	})
	if err != nil {
		t.Fatal(err)
	}
	request = newFlowRequest(t, connection, sip.MethodMessage, "cascade-unshared-panorama", body)
	ctx = &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-unshared-panorama", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("unshared panorama channel SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("unshared panorama channel SIP response timeout")
	}
	body, err = sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 79, DeviceID: testExposedChannelID,
		PTZCmd: "A50F0100000000B5",
		TargetArea: &deviceControlA23DragZoom{
			Length: 100, Width: 100, MidPointX: 50, MidPointY: 50, LengthX: 20, LengthY: 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request = newFlowRequest(t, connection, sip.MethodMessage, "cascade-orphan-target-area", body)
	ctx = &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-orphan-target-area", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("orphan TargetArea SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("orphan TargetArea SIP response timeout")
	}
}
