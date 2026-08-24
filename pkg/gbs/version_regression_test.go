package gbs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

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
	api := &GB28181API{}
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

func TestGB30CruiseTrackQueriesDecodeStructuredState(t *testing.T) {
	api := &GB28181API{}
	listBody := []byte(`<Response><CmdType>CruiseTrackListQuery</CmdType><SN>74</SN><DeviceID>` + gb10ChannelID + `</DeviceID><SumNum>2</SumNum><CruiseTrackList Num="2"><CruiseTrack><Number>0</Number><Name>白天</Name></CruiseTrack><CruiseTrack><Number>1</Number><Name>夜间</Name></CruiseTrack></CruiseTrackList></Response>`)
	list := api.decodeAndStoreQueryData(gb10DeviceID, "CruiseTrackListQuery", listBody)
	tracks, ok := list.([]CruiseTrackData)
	if !ok || len(tracks) != 2 || tracks[0].Number != 0 || tracks[0].Name != "白天" || tracks[1].Number != 1 {
		t.Fatalf("CruiseTrackListQuery data = %+v", list)
	}

	detailBody := []byte(`<Response><CmdType>CruiseTrackQuery</CmdType><SN>75</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Number>1</Number><Name>夜间</Name><SumNum>2</SumNum><CruisePointList Num="2"><CruisePoint><PresetIndex>3</PresetIndex><StayTime>10</StayTime><Speed>5</Speed></CruisePoint><CruisePoint><PresetIndex>7</PresetIndex><StayTime>20</StayTime><Speed>8</Speed></CruisePoint></CruisePointList></Response>`)
	detail := api.decodeAndStoreQueryData(gb10DeviceID, "CruiseTrackQuery", detailBody)
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
	api := &GB28181API{}
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
