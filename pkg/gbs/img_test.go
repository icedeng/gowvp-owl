package gbs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestSnapshotUploadRequiresKnownSessionAndCover(t *testing.T) {
	api := &GB28181API{}
	sessionID := "snapshot-session-0000000000000001"
	api.storeSnapshotState(SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, CoverKey: "cover-1", SessionID: sessionID,
		Status: "accepted", ExpectedCount: 1,
	})
	if err := api.ValidateSnapshotUpload(gb10DeviceID, "cover-1", sessionID); err != nil {
		t.Fatal(err)
	}
	if err := api.ValidateSnapshotUpload(gb10DeviceID, "cover-2", sessionID); err == nil {
		t.Fatal("mismatched cover key was accepted")
	}
	if err := api.ValidateSnapshotUpload(gb10DeviceID, "cover-1", strings.Repeat("x", 32)); err == nil {
		t.Fatal("unknown snapshot session was accepted")
	}
	api.MarkSnapshotUploaded(gb10DeviceID, sessionID)
	state, ok := api.SnapshotState(gb10DeviceID, sessionID)
	if !ok || state.Status != "uploading" || state.ReceivedCount != 1 {
		t.Fatalf("snapshot upload state = %+v, %v", state, ok)
	}
}

func TestSnapshotFinishedCompletesTrackedSession(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "snapshot-session-0000000000000002"
	api.storeSnapshotState(SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "uploading", ExpectedCount: 2, ReceivedCount: 1,
	})
	body := []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>102</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><SnapShotList>` +
		`<SnapShotFileID>` + gb10DeviceID + `022026082508150000001</SnapShotFileID>` +
		`<SnapShotFileID>` + gb10DeviceID + `022026082508150000002</SnapShotFileID>` +
		`</SnapShotList></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "snapshot-finished", body, api.sipMessageSnapshotFinished)
	assertFlowOK(t, response)

	state, ok := api.SnapshotState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || len(state.FileIDs) != 2 || state.ReceivedCount != 1 {
		t.Fatalf("snapshot final state = %+v, %v", state, ok)
	}
}

func TestSnapshotFinishedDetectsPartialFailure(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "snapshot-session-0000000000000003"
	api.storeSnapshotState(SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10DeviceID, SessionID: sessionID,
		Status: "accepted", ExpectedCount: 2,
	})
	body := []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>103</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><SnapShotList>` +
		`<SnapShotFileID>` + gb10DeviceID + `022026082508150000001</SnapShotFileID>` +
		`</SnapShotList></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "snapshot-partial", body, api.sipMessageSnapshotFinished)
	assertFlowOK(t, response)

	state, ok := api.SnapshotState(gb10DeviceID, sessionID)
	if !ok || state.Status != "partial_failed" {
		t.Fatalf("snapshot partial state = %+v, %v", state, ok)
	}
}

func TestSnapshotFinishedRequires2022(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion20)
	body := []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>104</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><SessionID>snapshot-session-0000000000000004</SessionID><SnapShotList/></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "snapshot-old-version", body, api.sipMessageSnapshotFinished)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("2.0 snapshot finished response = %s", response)
	}
}

func TestSnapshotFinishedRejectsSchemaViolations(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	sessionID := "snapshot-session-0000000000000099"
	fileID := gb10DeviceID + "022026082508150000001"
	tests := []struct {
		name     string
		root     string
		deviceID string
		sn       string
		cmdType  string
		files    string
	}{
		{name: "non-positive SN", sn: "0", cmdType: "UploadSnapShotFinished"},
		{name: "wrong root", root: "Response", sn: "1", cmdType: "UploadSnapShotFinished"},
		{name: "invalid device", deviceID: "bad", sn: "1", cmdType: "UploadSnapShotFinished"},
		{name: "wrong command", sn: "1", cmdType: "Catalog"},
		{name: "missing list", sn: "1", cmdType: "UploadSnapShotFinished", files: "__NO_LIST__"},
		{name: "invalid file", sn: "1", cmdType: "UploadSnapShotFinished", files: `<SnapShotFileID>bad</SnapShotFileID>`},
		{name: "too many files", sn: "1", cmdType: "UploadSnapShotFinished", files: strings.Repeat("<SnapShotFileID>"+fileID+"</SnapShotFileID>", 11)},
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
			list := `<SnapShotList>` + test.files + `</SnapShotList>`
			if test.files == "__NO_LIST__" {
				list = ""
			}
			body := []byte(`<` + root + `><CmdType>` + test.cmdType + `</CmdType><SN>` + test.sn + `</SN><DeviceID>` + deviceID +
				`</DeviceID><SessionID>` + sessionID + `</SessionID>` + list + `</` + root + `>`)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "snapshot-schema-"+test.name, body, api.sipMessageSnapshotFinished)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("schema-invalid snapshot response = %s", response)
			}
		})
	}
	if _, ok := api.SnapshotState(gb10DeviceID, sessionID); ok {
		t.Fatal("schema-invalid snapshot notification changed state")
	}
}

func TestSnapshotFinishedRejectsSiblingChannelSession(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	firstChannelID := gb10ChannelID
	secondChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(firstChannelID, &Channel{ChannelID: firstChannelID, device: memory.runtime})
	memory.runtime.Channels.Store(secondChannelID, &Channel{ChannelID: secondChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	sessionID := "snapshot-session-0000000000000005"
	api.storeSnapshotState(SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: firstChannelID, SessionID: sessionID,
		Status: "uploading", ExpectedCount: 1, ReceivedCount: 1,
	})
	body := []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>105</SN><DeviceID>` + secondChannelID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><SnapShotList/></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "snapshot-wrong-channel", body, api.sipMessageSnapshotFinished)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("sibling channel snapshot response = %s", response)
	}
	state, ok := api.SnapshotState(gb10DeviceID, sessionID)
	if !ok || state.ChannelID != firstChannelID || state.Status != "uploading" || state.ReceivedCount != 1 {
		t.Fatalf("sibling channel changed snapshot state = %+v, %v", state, ok)
	}
}

func TestSnapshotStatesExpireAndRemainBounded(t *testing.T) {
	api := &GB28181API{}
	now := time.Now()
	api.storeSnapshotState(SnapshotState{
		DeviceID: gb10DeviceID, SessionID: "snapshot-expired-000000000000001",
		Status: "completed", UpdatedAt: now.Add(-snapshotStateTTL - time.Second),
	})
	if _, ok := api.SnapshotState(gb10DeviceID, "snapshot-expired-000000000000001"); ok {
		t.Fatal("expired snapshot state survived lazy cleanup")
	}

	for index := 0; index < maxSnapshotStates+5; index++ {
		api.storeSnapshotState(SnapshotState{
			DeviceID: gb10DeviceID, SessionID: fmt.Sprintf("snapshot-session-%019d", index),
			Status: "completed", UpdatedAt: now.Add(time.Duration(index) * time.Nanosecond),
		})
	}
	api.snapshotStateMu.RLock()
	count := len(api.snapshotStates)
	api.snapshotStateMu.RUnlock()
	if count != maxSnapshotStates {
		t.Fatalf("snapshot state count = %d; want %d", count, maxSnapshotStates)
	}
}

func TestMarkSnapshotUploadedIsAtomicAndPreservesTerminalState(t *testing.T) {
	api := &GB28181API{}
	sessionID := "snapshot-concurrent-00000000000001"
	api.storeSnapshotState(SnapshotState{
		DeviceID: gb10DeviceID, SessionID: sessionID, Status: "completed", ExpectedCount: 200,
	})
	var uploads sync.WaitGroup
	for index := 0; index < 200; index++ {
		uploads.Add(1)
		go func() {
			defer uploads.Done()
			api.MarkSnapshotUploaded(gb10DeviceID, sessionID)
		}()
	}
	uploads.Wait()
	state, ok := api.SnapshotState(gb10DeviceID, sessionID)
	if !ok || state.ReceivedCount != 200 || state.Status != "completed" {
		t.Fatalf("concurrent snapshot upload state = %+v, %v", state, ok)
	}
}

func TestSnapshotPendingSessionAcceptsEarlyUploadWithoutStateRegression(t *testing.T) {
	api := &GB28181API{}
	sessionID := "snapshot-early-upload-000000000001"
	api.storeSnapshotState(SnapshotState{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CoverKey: "cover-early", SessionID: sessionID,
		Status: "pending", ExpectedCount: 1,
	})
	if err := api.ValidateSnapshotUpload(gb10DeviceID, "cover-early", sessionID); err != nil {
		t.Fatalf("early snapshot upload was rejected: %v", err)
	}
	api.MarkSnapshotUploaded(gb10DeviceID, sessionID)
	state, ok := api.transitionSnapshotState(gb10DeviceID, sessionID, "accepted")
	if !ok || state.Status != "uploading" || state.ReceivedCount != 1 {
		t.Fatalf("accepted response regressed early upload state = %+v, %v", state, ok)
	}
	api.transitionSnapshotState(gb10DeviceID, sessionID, "rejected")
	state, ok = api.SnapshotState(gb10DeviceID, sessionID)
	if !ok || state.Status != "uploading" || state.ReceivedCount != 1 {
		t.Fatalf("late rejection regressed early upload state = %+v, %v", state, ok)
	}
}

func TestQuerySnapshotRequiresOnlineDeviceAndOwnedTarget(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	if _, err := api.QuerySnapshotContext(context.Background(), gb10DeviceID, gb10DeviceID, "../cover"); err == nil {
		t.Fatal("unsafe snapshot cover key was accepted")
	}
	if _, err := api.QuerySnapshotContext(context.Background(), gb10DeviceID, gb10ChannelID, "cover"); !errors.Is(err, ErrChannelNotExist) {
		t.Fatalf("unowned snapshot target error = %v; want %v", err, ErrChannelNotExist)
	}
	memory.device.IsOnline = false
	if _, err := api.QuerySnapshotContext(context.Background(), gb10DeviceID, gb10DeviceID, "cover"); !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("offline snapshot device error = %v; want %v", err, ErrDeviceOffline)
	}
}

func TestQuerySnapshotRemovesPendingSessionWhenRequestWasNotSent(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	if _, err := api.QuerySnapshotContext(context.Background(), gb10DeviceID, gb10DeviceID, "cover"); err == nil {
		t.Fatal("snapshot query unexpectedly succeeded without a SIP server")
	}
	api.snapshotStateMu.RLock()
	count := len(api.snapshotStates)
	api.snapshotStateMu.RUnlock()
	if count != 0 {
		t.Fatalf("unsent snapshot request retained %d pending sessions", count)
	}
}
