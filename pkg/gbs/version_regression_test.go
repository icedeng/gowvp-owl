package gbs

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestGB20And30FeatureRegression(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	if err := api.requireGBFeature("device", "语音对讲", func(c GBCapabilities) bool { return c.VoiceIntercom }); err != nil {
		t.Fatalf("2.0 voice intercom rejected: %v", err)
	}
	if err := api.requireGBFeature("device", "直接 TCP", func(c GBCapabilities) bool { return c.DirectTCPDownload }); err == nil {
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
	for _, action := range []string{deviceQueryActionPTZPosition, deviceQueryActionSDCardStatus} {
		if _, err := api.resolveDeviceQueryCmdType("device", action, ""); err == nil {
			t.Fatalf("2.0 must reject 3.0 query %s", action)
		}
	}
	if err := api.requireConfigTypeVersion("device", "SnapShot"); err == nil {
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
		deviceQueryActionPTZPosition:  "PTZPosition",
		deviceQueryActionSDCardStatus: "SDCardStatus",
	} {
		got, err := api.resolveDeviceQueryCmdType("device", action, "")
		if err != nil || got != want {
			t.Fatalf("3.0 query %s = %q, %v; want %q", action, got, err, want)
		}
	}
	if err := api.requireConfigTypeVersion("device", "SnapShot"); err != nil {
		t.Fatalf("3.0 snapshot config rejected: %v", err)
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
	body := []byte(`<Notify><CmdType>Alarm</CmdType><Info><alarmType level="2"><Code>door_open</Code><VendorField>retained</VendorField></alarmType></Info></Notify>`)
	objects := (&GB28181API{}).decodeAppendixA4Objects("Alarm", body)
	if len(objects) != 1 {
		t.Fatalf("A.4 objects = %+v", objects)
	}
	object := objects[0]
	if object.Type != "alarmType" || object.CmdType != "Alarm" || object.Fields["Code"] != "door_open" || object.Fields["@level"] != "2" || !strings.Contains(object.RawXML, "VendorField") {
		t.Fatalf("A.4 object = %+v", object)
	}
}
