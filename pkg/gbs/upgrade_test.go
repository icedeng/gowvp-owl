package gbs

import (
	"errors"
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
	// GB/T 28181-2022 A.2.3.1.12 和 A.2.5.7/A.2.5.9 仅约束
	// SessionID 为 32～128 个字符，不限制为字母、数字和连字符。
	for _, valid := range []string{strings.Repeat("a", 31) + "_", strings.Repeat("会", 32)} {
		if got, err := normalizeUpgradeSessionID(valid); err != nil || got != valid {
			t.Fatalf("schema-valid SessionID = %q, %v", got, err)
		}
	}
	for _, invalid := range []string{"short", strings.Repeat("a", 129), strings.Repeat("会", 129), strings.Repeat("a", 31) + "\x00"} {
		if _, err := normalizeUpgradeSessionID(invalid); err == nil {
			t.Fatalf("invalid SessionID accepted: %q", invalid)
		}
	}
}

func TestDeviceUpgradeResultAcceptsSchemaValidSessionIDCharacters(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := strings.Repeat("a", 31) + "_"
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "accepted", Firmware: "V1",
	})
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>91</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V2</Firmware></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-schema-session-id", body, api.sipMessageDeviceUpgradeResult)
	assertFlowOK(t, response)
	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" {
		t.Fatalf("upgrade state = %+v, %v", state, ok)
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
		`<Firmware> V1.2.4 </Firmware></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-result", body, api.sipMessageDeviceUpgradeResult)
	assertFlowOK(t, response)

	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != "OK" || state.Firmware != " V1.2.4 " || state.SN != 92 {
		t.Fatalf("upgrade state = %+v, %v", state, ok)
	}
}

func TestDeviceUpgradeResultRejectsConflictingTerminalRetransmission(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "upgrade-session-terminal-conflict-01"
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "accepted", Firmware: "V1.2.3",
	})
	success := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>192</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V1.2.4</Firmware></Notify>`)
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-terminal-success", success, api.sipMessageDeviceUpgradeResult))

	duplicate := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>193</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V1.2.4</Firmware></Notify>`)
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-terminal-duplicate", duplicate, api.sipMessageDeviceUpgradeResult))

	conflict := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>194</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>ERROR</UpgradeResult>` +
		`<Firmware>V1.2.4</Firmware><UpgradeFailedReason>02</UpgradeFailedReason></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-terminal-conflict", conflict, api.sipMessageDeviceUpgradeResult)
	if !strings.Contains(response, "SIP/2.0 409") {
		t.Fatalf("conflicting upgrade terminal response = %s", response)
	}
	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != "OK" || state.Firmware != "V1.2.4" || state.SN != 192 {
		t.Fatalf("conflicting upgrade notification changed final state = %+v, %v", state, ok)
	}
}

func TestDeviceUpgradeResultPersistsBeforeSIPOKAndRetriesIdempotently(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "upgrade-session-ack-write-failure-01"
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "accepted", Firmware: "V1.2.3",
	})
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>195</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V1.2.4</Firmware></Notify>`)
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("DeviceUpgradeResult SIP OK write failed"),
	}
	request := newFlowRequest(t, base, sip.MethodMessage, "upgrade-ack-write-failure", body)
	request.SetConnection(connection)
	done := make(chan struct{})
	go func() {
		api.sipMessageDeviceUpgradeResult(&sip.Context{
			Request: request, Tx: sip.NewTransaction("upgrade-ack-write-failure-tx", connection),
			DeviceID: gb10DeviceID, Source: base.remote,
		})
		close(done)
	}()
	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("DeviceUpgradeResult SIP OK write did not start")
	}
	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != "OK" {
		close(connection.release)
		t.Fatalf("upgrade state before required 200 response = %+v, %v", state, ok)
	}
	committedAt := state.UpdatedAt
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DeviceUpgradeResult handler did not return after SIP OK write failure")
	}

	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-ack-write-retry", body, api.sipMessageDeviceUpgradeResult))
	state, ok = api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != "OK" || !state.UpdatedAt.Equal(committedAt) {
		t.Fatalf("retried upgrade final state = %+v, %v", state, ok)
	}
}

func TestDeviceUpgradeResultAcceptsPresentEmptyFirmware(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "upgrade-session-empty-fw-0000000001"
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "accepted", Firmware: "old",
	})
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>DeviceUpgradeResult</CmdType><SN>97</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware/></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-empty-firmware", body, api.sipMessageDeviceUpgradeResult)
	assertFlowOK(t, response)
	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Firmware != "" {
		t.Fatalf("upgrade state = %+v, %v", state, ok)
	}
}

func TestDeviceUpgradeRequestStringsPreserveWhitespace(t *testing.T) {
	config := newDeviceUpgradeConfig(&UpgradeInput{
		Firmware: " V1.2.4 ", FileURL: " https://example.invalid/fw.bin ", Manufacturer: " Vendor ",
	}, "upgrade-session-request-0000000001")
	body, err := sip.XMLEncode(deviceControlA23Request{DeviceUpgrade: config})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"<Firmware> V1.2.4 </Firmware>",
		"<FileURL> https://example.invalid/fw.bin </FileURL>",
		"<Manufacturer> Vendor </Manufacturer>",
	} {
		if !strings.Contains(string(body), value) {
			t.Fatalf("DeviceUpgrade XML = %s", body)
		}
	}
}

func TestDeviceUpgradeResultRestoresTrackedSessionAfterRestart(t *testing.T) {
	store := newPersistentTaskMemory(GBVersion30)
	sessionID := "upgrade-session-0000000000000002"
	first := &GB28181API{svr: &Server{memoryStorer: store}}
	if err := first.storeUpgradeStateContext(t.Context(), UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "accepted", Firmware: "V1.2.3",
	}); err != nil {
		t.Fatal(err)
	}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
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

func TestDeviceUpgradeResultRejectsUnknownSession(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "upgrade-session-unknown-00000000001"
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>93</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>ERROR</UpgradeResult>` +
		`<Firmware>V1.2.3</Firmware><UpgradeFailedReason>02</UpgradeFailedReason></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-unknown", body, api.sipMessageDeviceUpgradeResult)
	if !strings.Contains(response, "SIP/2.0 400 DeviceUpgradeResult session not found") {
		t.Fatalf("unknown upgrade session response = %s", response)
	}
	if _, ok := api.UpgradeState(gb10DeviceID, sessionID); ok {
		t.Fatal("unknown upgrade notification created state")
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

func TestDeviceUpgradeResultRejectsSchemaViolations(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "upgrade-session-0000000000000099"
	tests := []struct {
		name     string
		root     string
		deviceID string
		sn       string
		body     string
	}{
		{name: "non-positive SN", sn: "0", body: `<UpgradeResult>OK</UpgradeResult><Firmware>V1</Firmware>`},
		{name: "wrong root", root: "Response", sn: "1", body: `<UpgradeResult>OK</UpgradeResult><Firmware>V1</Firmware>`},
		{name: "invalid device", deviceID: "bad", sn: "1", body: `<UpgradeResult>OK</UpgradeResult><Firmware>V1</Firmware>`},
		{name: "invalid result", sn: "1", body: `<UpgradeResult>MAYBE</UpgradeResult><Firmware>V1</Firmware>`},
		{name: "missing firmware", sn: "1", body: `<UpgradeResult>OK</UpgradeResult>`},
		{name: "missing failure reason", sn: "1", body: `<UpgradeResult>ERROR</UpgradeResult><Firmware>V1</Firmware>`},
		{name: "invalid failure reason", sn: "1", body: `<UpgradeResult>ERROR</UpgradeResult><Firmware>V1</Firmware><UpgradeFailedReason>04</UpgradeFailedReason>`},
		{name: "duplicate firmware", sn: "1", body: `<UpgradeResult>OK</UpgradeResult><Firmware>V1</Firmware><Firmware>V2</Firmware>`},
		{name: "unknown element", sn: "1", body: `<UpgradeResult>OK</UpgradeResult><Firmware>V1</Firmware><Info/>`},
		{name: "element attribute", sn: "1", body: `<UpgradeResult>OK</UpgradeResult><Firmware vendor="1">V1</Firmware>`},
		{name: "nested element", sn: "1", body: `<UpgradeResult>OK</UpgradeResult><Firmware><Version>V1</Version></Firmware>`},
		{name: "out of order", sn: "1", body: `<Firmware>V1</Firmware><UpgradeResult>OK</UpgradeResult>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := test.root
			if root == "" {
				root = "Notify"
			}
			deviceID := test.deviceID
			if deviceID == "" {
				deviceID = gb10DeviceID
			}
			body := []byte(`<` + root + `><CmdType>DeviceUpgradeResult</CmdType><SN>` + test.sn + `</SN><DeviceID>` + deviceID +
				`</DeviceID><SessionID>` + sessionID + `</SessionID>` + test.body + `</` + root + `>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-schema-"+test.name, body, api.sipMessageDeviceUpgradeResult)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("schema-invalid upgrade result response = %s", response)
			}
		})
	}
	if _, ok := api.UpgradeState(gb10DeviceID, sessionID); ok {
		t.Fatal("schema-invalid upgrade result changed state")
	}
}

func TestDeviceUpgradeResultRejectsSiblingChannelSession(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	firstChannelID := gb10ChannelID
	secondChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(firstChannelID, &Channel{ChannelID: firstChannelID, device: memory.runtime})
	memory.runtime.Channels.Store(secondChannelID, &Channel{ChannelID: secondChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	sessionID := "upgrade-session-0000000000000004"
	api.storeUpgradeState(UpgradeState{
		DeviceID: gb10DeviceID, ChannelID: firstChannelID, SessionID: sessionID,
		Status: "accepted", Firmware: "V1.2.3",
	})
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>96</SN><DeviceID>` + secondChannelID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V1.2.4</Firmware></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "upgrade-wrong-channel", body, api.sipMessageDeviceUpgradeResult)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling channel upgrade result response = %s", response)
	}
	state, ok := api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.ChannelID != firstChannelID || state.Status != "accepted" || state.SN != 0 {
		t.Fatalf("sibling channel changed upgrade state = %+v, %v", state, ok)
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

func TestUpgradeFinalNotificationIsImmutableAgainstLateStateChanges(t *testing.T) {
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

	if err := api.storeUpgradeStateContext(t.Context(), UpgradeState{
		DeviceID: gb10DeviceID, SessionID: sessionID, Status: "failed", Result: "ERROR", FailedReason: "02",
	}); !errors.Is(err, errUpgradeFinalConflict) {
		t.Fatalf("conflicting final state error = %v", err)
	}
	state, ok = api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != "OK" || state.Firmware != "V2" {
		t.Fatalf("conflicting final state replaced completed upgrade state = %+v, %v", state, ok)
	}
}
