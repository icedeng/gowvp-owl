package gbs

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestUpgradeStatesExpireAndRemainBounded(t *testing.T) {
	api := &GB28181API{}
	now := time.Now()
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, SessionID: "upgrade-expired-0000000000000001",
		Status: "completed", UpdatedAt: now.Add(-upgradeStateTTL - time.Second),
	})
	if _, ok := api.UpgradeState(gb10DeviceID, "upgrade-expired-0000000000000001"); ok {
		t.Fatal("expired upgrade state survived lazy cleanup")
	}

	for index := 0; index < maxUpgradeStates+5; index++ {
		api.storeUpgradeState(UpgradeState{
			DeviceID: gb10DeviceID, SessionID: fmt.Sprintf("upgrade-session-%020d", index),
			Status: "completed", UpdatedAt: now.Add(time.Duration(index) * time.Nanosecond),
		})
	}
	api.upgradeStateMu.RLock()
	count := len(api.upgradeStates)
	api.upgradeStateMu.RUnlock()
	if count != maxUpgradeStates {
		t.Fatalf("upgrade state count = %d; want %d", count, maxUpgradeStates)
	}
}

func TestUpgradeAndSnapshotStateCleanupConcurrent(t *testing.T) {
	api := &GB28181API{}
	now := time.Now()
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		for index := 0; index < 500; index++ {
			sessionID := fmt.Sprintf("runtime-state-%020d", index)
			updatedAt := now.Add(time.Duration(index) * time.Nanosecond)
			api.storeUpgradeState(UpgradeState{DeviceID: gb10DeviceID, SessionID: sessionID, UpdatedAt: updatedAt})
			api.storeSnapshotState(SnapshotState{DeviceID: gb10DeviceID, SessionID: sessionID, UpdatedAt: updatedAt})
		}
	}()
	go func() {
		defer workers.Done()
		for index := 0; index < 500; index++ {
			sessionID := fmt.Sprintf("runtime-state-%020d", index)
			_, _ = api.UpgradeState(gb10DeviceID, sessionID)
			_, _ = api.SnapshotState(gb10DeviceID, sessionID)
		}
	}()
	go func() {
		defer workers.Done()
		for index := 0; index < 100; index++ {
			api.cleanupUpgradeStates(now)
			api.cleanupSnapshotStates(now)
		}
	}()
	workers.Wait()
}

func TestUpgradeFinalNotificationOutranksLateControlResponse(t *testing.T) {
	api := &GB28181API{}
	sessionID := "upgrade-final-first-00000000000001"
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, SessionID: sessionID, Status: "completed", Result: "OK", Firmware: "V2",
	})
	for _, status := range []string{"accepted", "rejected"} {
		api.storeUpgradeState(UpgradeState{
			DeviceID: gb10DeviceID, SessionID: sessionID, Status: status, Result: "ERROR", Firmware: "V1",
		})
	}
	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != "OK" || state.Firmware != "V2" {
		t.Fatalf("late control response replaced final upgrade state = %+v, %v", state, ok)
	}

	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, SessionID: sessionID, Status: "failed", Result: "ERROR", FailedReason: "02",
	})
	state, ok = api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "failed" || state.FailedReason != "02" {
		t.Fatalf("new final notification did not update final upgrade state = %+v, %v", state, ok)
	}
}
