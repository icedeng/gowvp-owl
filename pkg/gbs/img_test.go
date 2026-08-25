package gbs

import (
	"strings"
	"testing"

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
