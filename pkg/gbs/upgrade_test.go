package gbs

import (
	"strings"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestNormalizeUpgradeSessionIDFollows2022Schema(t *testing.T) {
	generated, err := normalizeUpgradeSessionID("")
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != 32 {
		t.Fatalf("generated SessionID length = %d", len(generated))
	}
	valid := "upgrade-session-0000000000000001"
	if got, err := normalizeUpgradeSessionID(valid); err != nil || got != valid {
		t.Fatalf("valid SessionID = %q, %v", got, err)
	}
	for _, invalid := range []string{"short", strings.Repeat("a", 129), strings.Repeat("a", 31) + "_"} {
		if _, err := normalizeUpgradeSessionID(invalid); err == nil {
			t.Fatalf("invalid SessionID accepted: %q", invalid)
		}
	}
}

func TestDeviceUpgradeResultCompletesTrackedSession(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "upgrade-session-0000000000000001"
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "accepted", Firmware: "V1.2.3",
	})
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V1.2.4</Firmware></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-result", body, api.sipMessageDeviceUpgradeResult)
	assertFlowOK(t, response)

	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != "OK" || state.Firmware != "V1.2.4" || state.SN != 92 {
		t.Fatalf("upgrade state = %+v, %v", state, ok)
	}
}

func TestDeviceUpgradeResultStoresFailureAfterRestart(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "upgrade-session-0000000000000002"
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>93</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>ERROR</UpgradeResult>` +
		`<Firmware>V1.2.3</Firmware><UpgradeFailedReason>02</UpgradeFailedReason></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-failure", body, api.sipMessageDeviceUpgradeResult)
	assertFlowOK(t, response)

	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "failed" || state.FailedReason != "02" {
		t.Fatalf("upgrade failure state = %+v, %v", state, ok)
	}
}

func TestDeviceUpgradeResultRequires2022AndValidSession(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>94</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>upgrade-session-0000000000000003</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V1.2.4</Firmware></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-old-version", body, api.sipMessageDeviceUpgradeResult)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("2.0 upgrade result response = %s", response)
	}

	memory.device.setGBVersion(GBVersion30)
	invalid := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>95</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>short</SessionID><UpgradeResult>OK</UpgradeResult></Notify>`)
	response = runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-invalid-session", invalid, api.sipMessageDeviceUpgradeResult)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("invalid SessionID response = %s", response)
	}
}
