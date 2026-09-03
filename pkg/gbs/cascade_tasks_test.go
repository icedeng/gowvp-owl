package gbs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestReplacedCascadeWorkerCannotPublishRoutes(t *testing.T) {
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	manager := NewCascadeManager(server)
	server.cascade = manager
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion30
	oldWorker := newCascadeWorker(server, platform)
	replacement := newCascadeWorker(server, platform)
	oldWorker.effective = GBVersion30
	replacement.effective = GBVersion30
	t.Cleanup(oldWorker.cancel)
	t.Cleanup(replacement.cancel)
	manager.items[platform.name] = replacement

	if route := api.storeCascadeMobilePositionQuery(oldWorker, testExposedChannelID, gb10DeviceID, testCascadeChannelID); route != nil {
		t.Fatal("replaced worker published a MobilePosition route")
	}
	if route := api.storeCascadeSystemMobilePositionQuery(oldWorker, platform.localID, []*ipc.Channel{{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, IsOnline: true,
	}}); route != nil {
		t.Fatal("replaced worker published a system MobilePosition route")
	}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, IsOnline: true}
	_, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, oldWorker, channel,
		testExposedChannelID, "upgrade-session-0000000000000200", "request-fingerprint")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replaced worker task route error = %v", err)
	}
	if count := syncMapLen(&api.cascadeTaskRoutes); count != 0 {
		t.Fatalf("replaced worker task route count = %d", count)
	}
}

func TestCascadeUpgradeAndSnapshotMapIndependentDownstreamSessions(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	upstreamUpgrade := "upgrade-session-0000000000000100"
	firmware := " V3 "
	fileURL := " https://example.invalid/firmware.bin "
	manufacturer := " Vendor "
	var downstreamUpgrade string
	api.cascadeDeviceControl = func(_ context.Context, target *ipc.Channel, request *deviceControlA23Request) (string, error) {
		if target.ChannelID != channel.ChannelID || request.DeviceUpgrade == nil {
			t.Fatalf("upgrade target/request = %+v / %+v", target, request)
		}
		if request.DeviceUpgrade.Firmware != firmware || request.DeviceUpgrade.FileURL != fileURL || request.DeviceUpgrade.Manufacturer != manufacturer {
			t.Fatalf("upgrade text fields were modified: %+v", request.DeviceUpgrade)
		}
		downstreamUpgrade = request.DeviceUpgrade.SessionID
		return ptzResultOK, nil
	}
	upgradeBody, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 99, DeviceID: testExposedChannelID,
		DeviceUpgrade: &deviceUpgradeConfig{
			Firmware: firmware, FileURL: fileURL, Manufacturer: manufacturer, SessionID: upstreamUpgrade,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api.forwardCascadeDeviceControl(worker, upgradeBody, t.Context())
	if downstreamUpgrade == "" || downstreamUpgrade == upstreamUpgrade {
		t.Fatalf("downstream upgrade session = %q", downstreamUpgrade)
	}
	if state, ok := api.UpgradeState(gb10DeviceID, downstreamUpgrade); !ok || state.Status != "accepted" || state.ChannelID != channel.ChannelID || state.Firmware != firmware {
		t.Fatalf("cascade upgrade state = %+v, %v", state, ok)
	}

	upstreamSnapshot := "snapshot-session-0000000000000100"
	var downstreamSnapshot string
	api.cascadeDeviceConfig = func(_ context.Context, target *ipc.Channel, request *DeviceConfigRequest) (string, error) {
		if target.ChannelID != channel.ChannelID || request.SnapShotConfig == nil {
			t.Fatalf("snapshot target/request = %+v / %+v", target, request)
		}
		downstreamSnapshot = request.SnapShotConfig.SessionID
		return "OK", nil
	}
	snapshotBody, err := sip.XMLEncode(DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 100, DeviceID: testExposedChannelID,
		SnapShotConfig: &SnapShot{SnapNum: 1, Interval: 1, UploadURL: "https://example.invalid/upload", SessionID: upstreamSnapshot},
	})
	if err != nil {
		t.Fatal(err)
	}
	api.forwardCascadeDeviceConfig(worker, snapshotBody, t.Context())
	if downstreamSnapshot == "" || downstreamSnapshot == upstreamSnapshot || downstreamSnapshot == downstreamUpgrade {
		t.Fatalf("downstream snapshot session = %q, upgrade = %q", downstreamSnapshot, downstreamUpgrade)
	}
	if state, ok := api.SnapshotState(gb10DeviceID, downstreamSnapshot); !ok || state.Status != "accepted" || state.ChannelID != channel.ChannelID || state.ExpectedCount != 1 {
		t.Fatalf("cascade snapshot state = %+v, %v", state, ok)
	}
}

func TestCascadeUpgradeFingerprintPreservesOrdinaryStrings(t *testing.T) {
	base := &deviceUpgradeConfig{
		Firmware: "V3", FileURL: "https://example.invalid/firmware.bin", Manufacturer: "Vendor",
		SessionID: "upgrade-session-0000000000000100",
	}
	withWhitespace := *base
	withWhitespace.Firmware = " V3 "
	if cascadeUpgradeFingerprint(base) == cascadeUpgradeFingerprint(&withWhitespace) {
		t.Fatal("upgrade fingerprints ignored significant Firmware whitespace")
	}

	trimmedSession := *base
	trimmedSession.SessionID = " " + base.SessionID + " "
	if cascadeUpgradeFingerprint(base) != cascadeUpgradeFingerprint(&trimmedSession) {
		t.Fatal("upgrade fingerprints did not normalize the structured SessionID")
	}
}

func TestCascadeTaskRouteForwardsAndRewritesFinalNotification(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	requests := make(chan *sip.Request, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests <- request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}

	upgradeSession := "upgrade-session-0000000000000101"
	upgradeRoute, created, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, upgradeSession, "upgrade-request")
	if err != nil || !created {
		t.Fatalf("register upgrade route = created %v, err %v", created, err)
	}
	upgrade := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>101</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + upgradeRoute.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	if forwarded, err := api.forwardCascadeTaskNotification(t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, upgradeRoute.downstreamSessionID, upgrade); err != nil || !forwarded {
		t.Fatalf("forward upgrade = %v, %v", forwarded, err)
	}
	assertCascadeTaskBody(t, <-requests, "DeviceUpgradeResult", testExposedChannelID, upgradeSession)
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, upgradeRoute.downstreamSessionID)); ok {
		t.Fatal("completed upgrade route was retained")
	}
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskUpstreamRouteKey(cascadeTaskUpgrade, worker, testExposedChannelID, upgradeSession)); !ok {
		t.Fatal("completed upgrade route lost its upstream deduplication tombstone")
	}

	snapshotSession := "snapshot-session-0000000000000102"
	snapshotRoute, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskSnapshot, worker, channel, testExposedChannelID, snapshotSession, "snapshot-request")
	if err != nil {
		t.Fatal(err)
	}
	localFileID := testCascadeChannelID + "02" + strings.Repeat("1", 19)
	snapshot := []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>102</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID> ` + snapshotRoute.downstreamSessionID + ` </SessionID><SnapShotList><SnapShotFileID> ` + localFileID +
		` </SnapShotFileID></SnapShotList></Notify>`)
	if forwarded, err := api.forwardCascadeTaskNotification(t.Context(), cascadeTaskSnapshot, gb10DeviceID, testCascadeChannelID, snapshotRoute.downstreamSessionID, snapshot); err != nil || !forwarded {
		t.Fatalf("forward snapshot = %v, %v", forwarded, err)
	}
	request := <-requests
	assertCascadeTaskBody(t, request, "UploadSnapShotFinished", testExposedChannelID, snapshotSession)
	if !strings.Contains(string(request.Body()), testExposedChannelID+"02"+strings.Repeat("1", 19)) {
		t.Fatalf("snapshot file id was not rewritten: %s", request.Body())
	}
}

func TestCascadeTaskRoutePersistenceFailureDoesNotPublishRoute(t *testing.T) {
	store := &failingCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}

	if _, created, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-route-save-failure-01", "upgrade-request",
	); !errors.Is(err, errCascadeTaskRouteSave) || created {
		t.Fatalf("register route with persistence failure = created %v, err %v", created, err)
	}
	if count := api.cascadeTaskRouteCount(); count != 0 {
		t.Fatalf("failed persistent route published %d runtime routes", count)
	}
	store.mu.Lock()
	taskStates := len(store.records)
	downstreamRoutes := len(store.downstreamRoutes)
	upstreamRoutes := len(store.upstreamRoutes)
	store.mu.Unlock()
	if taskStates != 0 || downstreamRoutes != 0 || upstreamRoutes != 0 {
		t.Fatalf("failed persistent route left task=%d downstream=%d upstream=%d records", taskStates, downstreamRoutes, upstreamRoutes)
	}
}

func TestCascadeTaskRouteCorruptPersistenceIsQuarantined(t *testing.T) {
	t.Run("downstream", func(t *testing.T) {
		store := newPersistentTaskMemory(GBVersion30)
		key := persistentCascadeDownstreamKey(cascadeTaskUpgrade, gb10DeviceID, "corrupt-downstream-session-0001")
		store.downstreamRoutes[key] = []byte("not-json")
		api := &GB28181API{svr: &Server{memoryStorer: store}}

		if _, found, err := api.loadCascadeTaskRouteStateByDownstream(
			t.Context(), cascadeTaskUpgrade, gb10DeviceID, "corrupt-downstream-session-0001",
		); err != nil || found {
			t.Fatalf("load corrupt downstream route = found:%v err:%v", found, err)
		}
		store.mu.Lock()
		_, retained := store.downstreamRoutes[key]
		store.mu.Unlock()
		if retained {
			t.Fatal("corrupt downstream route was retained")
		}
	})

	t.Run("upstream", func(t *testing.T) {
		store := newPersistentTaskMemory(GBVersion30)
		platformName := "corrupt-upstream-platform"
		exposedID := testExposedChannelID
		sessionID := "corrupt-upstream-session-000001"
		key := persistentCascadeUpstreamKey(cascadeTaskUpgrade, platformName, exposedID, sessionID)
		store.upstreamRoutes[key] = []byte("not-json")
		api := &GB28181API{svr: &Server{memoryStorer: store}}

		if _, found, err := api.loadCascadeTaskRouteStateByUpstream(
			t.Context(), cascadeTaskUpgrade, platformName, exposedID, sessionID,
		); err != nil || found {
			t.Fatalf("load corrupt upstream route = found:%v err:%v", found, err)
		}
		store.mu.Lock()
		_, retained := store.upstreamRoutes[key]
		store.mu.Unlock()
		if retained {
			t.Fatal("corrupt upstream route was retained")
		}
	})
}

func TestCascadeTaskRouteUnknownSchemaIsRetainedForForwardCompatibility(t *testing.T) {
	store := newPersistentTaskMemory(GBVersion30)
	platformName := "future-schema-platform"
	sessionID := "future-schema-session-000000001"
	key := persistentCascadeUpstreamKey(cascadeTaskUpgrade, platformName, testExposedChannelID, sessionID)
	store.upstreamRoutes[key] = []byte(`{"schema_version":2}`)
	api := &GB28181API{svr: &Server{memoryStorer: store}}

	if _, found, err := api.loadCascadeTaskRouteStateByUpstream(
		t.Context(), cascadeTaskUpgrade, platformName, testExposedChannelID, sessionID,
	); !found || !errors.Is(err, errUnsupportedCascadeTaskRouteSchema) {
		t.Fatalf("load future-schema route = found:%v err:%v", found, err)
	}
	store.mu.Lock()
	_, retained := store.upstreamRoutes[key]
	store.mu.Unlock()
	if !retained {
		t.Fatal("future-schema route was deleted by an older runtime")
	}
}

func TestCascadeTaskRouteMissingOrZeroSchemaIsQuarantined(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
	}{
		{name: "missing", payload: []byte(`{}`)},
		{name: "zero", payload: []byte(`{"schema_version":0}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newPersistentTaskMemory(GBVersion30)
			sessionID := "invalid-schema-session-" + test.name
			key := persistentCascadeDownstreamKey(cascadeTaskUpgrade, gb10DeviceID, sessionID)
			store.downstreamRoutes[key] = test.payload
			api := &GB28181API{svr: &Server{memoryStorer: store}}

			if _, found, err := api.loadCascadeTaskRouteStateByDownstream(
				t.Context(), cascadeTaskUpgrade, gb10DeviceID, sessionID,
			); err != nil || found {
				t.Fatalf("load invalid-schema route = found:%v err:%v", found, err)
			}
			store.mu.Lock()
			_, retained := store.downstreamRoutes[key]
			store.mu.Unlock()
			if retained {
				t.Fatal("invalid-schema route was retained")
			}
		})
	}
}

func TestCascadeTaskRouteFutureTimestampIsQuarantined(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		name      string
		createdAt time.Time
		updatedAt time.Time
	}{
		{name: "created", createdAt: now.Add(time.Hour), updatedAt: now.Add(time.Hour)},
		{name: "updated", createdAt: now, updatedAt: now.Add(time.Hour)},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newPersistentTaskMemory(GBVersion30)
			sessionID := "future-time-" + test.name + "-" + strings.Repeat("d", 32)
			state := cascadeTaskRoutePersistentState{
				SchemaVersion:       cascadeTaskRouteSchemaVersion,
				Kind:                cascadeTaskUpgrade,
				PlatformName:        "future-time-platform",
				DownstreamDeviceID:  gb10DeviceID,
				DownstreamTargetID:  testCascadeChannelID,
				ExposedID:           testExposedChannelID,
				UpstreamSessionID:   "future-time-upstream-" + strings.Repeat("u", 32),
				DownstreamSessionID: sessionID,
				RequestFingerprint:  "future-time-request",
				CreatedAt:           test.createdAt,
				UpdatedAt:           test.updatedAt,
				StartFinished:       true,
				StartResult:         ptzResultOK,
				Completed:           true,
			}
			payload, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SaveGBCascadeTaskRoute(
				t.Context(), state.Kind, state.PlatformName, state.DownstreamDeviceID, state.DownstreamSessionID,
				state.ExposedID, state.UpstreamSessionID, payload, test.updatedAt,
			); err != nil {
				t.Fatal(err)
			}
			api := &GB28181API{svr: &Server{memoryStorer: store}}
			if _, found, err := api.loadCascadeTaskRouteStateByDownstream(
				t.Context(), state.Kind, state.DownstreamDeviceID, state.DownstreamSessionID,
			); err != nil || found {
				t.Fatalf("load future-time route = found:%v err:%v", found, err)
			}
			if _, found, err := store.LoadGBCascadeTaskRouteByDownstream(
				t.Context(), state.Kind, state.DownstreamDeviceID, state.DownstreamSessionID,
			); err != nil || found {
				t.Fatalf("future-time route remained = found:%v err:%v", found, err)
			}
		})
	}
}

func TestCascadeTaskRouteMismatchedPersistenceIsQuarantined(t *testing.T) {
	now := time.Now()
	validState := cascadeTaskRoutePersistentState{
		SchemaVersion: cascadeTaskRouteSchemaVersion,
		Kind:          cascadeTaskUpgrade, PlatformName: "persisted-platform",
		DownstreamDeviceID: gb10DeviceID, DownstreamTargetID: testCascadeChannelID,
		ExposedID:         testExposedChannelID,
		UpstreamSessionID: "persisted-upstream-session-00001", DownstreamSessionID: "persisted-downstream-session-001",
		RequestFingerprint: "persisted-request", CreatedAt: now, UpdatedAt: now,
		StartFinished: true, StartResult: ptzResultOK, Completed: true,
	}
	payload, err := json.Marshal(validState)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("downstream", func(t *testing.T) {
		store := newPersistentTaskMemory(GBVersion30)
		requestedSessionID := "requested-downstream-session-001"
		if err := store.SaveGBCascadeTaskRoute(
			t.Context(), validState.Kind, validState.PlatformName, validState.DownstreamDeviceID, requestedSessionID,
			validState.ExposedID, validState.UpstreamSessionID, payload, now,
		); err != nil {
			t.Fatal(err)
		}
		api := &GB28181API{svr: &Server{memoryStorer: store}}
		if _, found, err := api.restoreCascadeTaskRouteByDownstream(
			t.Context(), validState.Kind, validState.DownstreamDeviceID, requestedSessionID,
		); err != nil || found {
			t.Fatalf("restore mismatched downstream route = found:%v err:%v", found, err)
		}
		if _, ok, err := store.LoadGBCascadeTaskRouteByDownstream(t.Context(), validState.Kind, validState.DownstreamDeviceID, requestedSessionID); err != nil || ok {
			t.Fatalf("mismatched downstream route remained: found:%v err:%v", ok, err)
		}
	})

	t.Run("upstream", func(t *testing.T) {
		store := newPersistentTaskMemory(GBVersion30)
		requestedSessionID := "requested-upstream-session-000001"
		if err := store.SaveGBCascadeTaskRoute(
			t.Context(), validState.Kind, validState.PlatformName, validState.DownstreamDeviceID, validState.DownstreamSessionID,
			validState.ExposedID, requestedSessionID, payload, now,
		); err != nil {
			t.Fatal(err)
		}
		server := &Server{memoryStorer: store}
		api := &GB28181API{svr: server}
		worker := newCascadeWorker(server, testSharedCascadePlatform(t))
		worker.platform.name = validState.PlatformName
		worker.effective = GBVersion30
		if _, found, err := api.restoreCascadeTaskRouteByUpstreamLocked(
			t.Context(), validState.Kind, worker, validState.ExposedID, requestedSessionID,
		); err != nil || found {
			t.Fatalf("restore mismatched upstream route = found:%v err:%v", found, err)
		}
		if _, ok, err := store.LoadGBCascadeTaskRouteByUpstream(t.Context(), validState.Kind, validState.PlatformName, validState.ExposedID, requestedSessionID); err != nil || ok {
			t.Fatalf("mismatched upstream route remained: found:%v err:%v", ok, err)
		}
	})
}

type observingCascadeTaskRollbackMemory struct {
	*failingCascadeTaskRouteMemory
	deleteTaskContexts  chan context.Context
	deleteRouteContexts chan context.Context
}

func (m *observingCascadeTaskRollbackMemory) DeleteGBTaskState(ctx context.Context, kind, deviceID, sessionID string) error {
	m.deleteTaskContexts <- ctx
	return m.persistentTaskMemory.DeleteGBTaskState(ctx, kind, deviceID, sessionID)
}

func (m *observingCascadeTaskRollbackMemory) DeleteGBCascadeTaskRoute(ctx context.Context, kind, deviceID, sessionID string) error {
	m.deleteRouteContexts <- ctx
	return m.persistentTaskMemory.DeleteGBCascadeTaskRoute(ctx, kind, deviceID, sessionID)
}

func TestCascadeTaskRouteFailureRollbackUsesTaskPersistenceContext(t *testing.T) {
	store := &observingCascadeTaskRollbackMemory{
		failingCascadeTaskRouteMemory: &failingCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)},
		deleteTaskContexts:            make(chan context.Context, 1),
		deleteRouteContexts:           make(chan context.Context, 1),
	}
	lifecycleCtx := context.WithValue(context.Background(), taskContextMarkerKey{}, "lifecycle")
	api := &GB28181API{svr: &Server{memoryStorer: store}, lifecycleCtx: lifecycleCtx}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}

	if _, created, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-route-rollback-context-01", "upgrade-request",
	); !errors.Is(err, errCascadeTaskRouteSave) || created {
		t.Fatalf("register route with persistence failure = created %v, err %v", created, err)
	}
	for name, ctx := range map[string]context.Context{
		"task state": <-store.deleteTaskContexts,
		"task route": <-store.deleteRouteContexts,
	} {
		if marker := ctx.Value(taskContextMarkerKey{}); marker != "lifecycle" {
			t.Fatalf("%s rollback context marker = %v", name, marker)
		}
	}
}

func TestCascadeTaskRouteIsolatesCrossUpstreamSessionReuse(t *testing.T) {
	api := &GB28181API{}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	first := newCascadeWorker(nil, testSharedCascadePlatform(t))
	first.effective = GBVersion30
	secondPlatform := testSharedCascadePlatform(t)
	secondPlatform.name = "city"
	second := newCascadeWorker(nil, secondPlatform)
	second.effective = GBVersion30
	sessionID := "upgrade-session-0000000000000103"
	firstRoute, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, first, channel, testExposedChannelID, sessionID, "first-request")
	if err != nil {
		t.Fatal(err)
	}
	secondRoute, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, second, channel, testExposedChannelID, sessionID, "second-request")
	if err != nil {
		t.Fatal(err)
	}
	if firstRoute.downstreamSessionID == secondRoute.downstreamSessionID {
		t.Fatal("cross-upstream task sessions reused the same downstream id")
	}
	api.cleanupCascadeTaskRoutes(time.Now().Add(cascadeTaskRouteTTL + time.Second))
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, firstRoute.downstreamSessionID)); ok {
		t.Fatal("expired cascade task route survived cleanup")
	}
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskUpstreamRouteKey(cascadeTaskUpgrade, first, testExposedChannelID, sessionID)); ok {
		t.Fatal("expired cascade task upstream index survived cleanup")
	}
}

func TestCascadeTaskRouteRecreatesExpiredUpstreamSession(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	api := &GB28181API{}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	sessionID := "upgrade-session-0000000000000111"
	first, created, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, sessionID, "upgrade-request")
	if err != nil || !created {
		t.Fatalf("register first route = created %v, err %v", created, err)
	}
	first.createdAt = time.Now().Add(-cascadeTaskRouteTTL - time.Second)
	first.setUpdatedAt(first.createdAt)
	second, created, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, sessionID, "upgrade-request")
	if err != nil || !created {
		t.Fatalf("recreate expired route = created %v, err %v", created, err)
	}
	if second == first || second.downstreamSessionID == first.downstreamSessionID {
		t.Fatal("expired upstream task route was reused")
	}
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, first.downstreamSessionID)); ok {
		t.Fatal("expired downstream task index survived replacement")
	}
}

func TestCascadeTaskRouteTTLUsesLastUpdate(t *testing.T) {
	api := &GB28181API{}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	now := time.Now()
	route := &cascadeTaskRoute{
		kind: cascadeTaskUpgrade, worker: worker, upstreamPlatform: worker.platform.name,
		downstreamDeviceID: gb10DeviceID, downstreamTargetID: testCascadeChannelID,
		exposedID:           testExposedChannelID,
		upstreamSessionID:   "ttl-upstream-session-00000000001",
		downstreamSessionID: "ttl-downstream-session-000000001",
		requestFingerprint:  "ttl-request", createdAt: now.Add(-cascadeTaskRouteTTL - time.Hour),
		startDone: make(chan struct{}),
	}
	route.setUpdatedAt(now)
	downstreamKey := cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID)
	upstreamKey := cascadeTaskUpstreamRouteKey(route.kind, worker, route.exposedID, route.upstreamSessionID)
	api.cascadeTaskRoutes.Store(downstreamKey, route)
	api.cascadeTaskRoutes.Store(upstreamKey, route)

	api.cleanupCascadeTaskRoutes(now)
	if _, ok := api.cascadeTaskRoutes.Load(downstreamKey); !ok {
		t.Fatal("recently updated cascade task route expired from creation time")
	}
	route.setUpdatedAt(now.Add(-cascadeTaskRouteTTL - time.Second))
	api.cleanupCascadeTaskRoutes(now)
	if _, ok := api.cascadeTaskRoutes.Load(downstreamKey); ok {
		t.Fatal("stale cascade task route survived last-update TTL")
	}
}

func TestCascadeTaskRouteRestoresLegacyRecordUsingCreatedAt(t *testing.T) {
	store := newPersistentTaskMemory(GBVersion30)
	createdAt := time.Now().Add(-time.Minute)
	state := cascadeTaskRoutePersistentState{
		SchemaVersion:       cascadeTaskRouteSchemaVersion,
		Kind:                cascadeTaskUpgrade,
		PlatformName:        "legacy-created-at-platform",
		DownstreamDeviceID:  gb10DeviceID,
		DownstreamTargetID:  testCascadeChannelID,
		ExposedID:           testExposedChannelID,
		UpstreamSessionID:   "legacy-created-at-upstream-00001",
		DownstreamSessionID: "legacy-created-at-downstream-001",
		RequestFingerprint:  "legacy-created-at-request",
		CreatedAt:           createdAt,
		StartFinished:       true,
		StartResult:         ptzResultOK,
		Completed:           true,
	}
	first := &GB28181API{svr: &Server{memoryStorer: store}}
	if err := first.persistCascadeTaskRouteState(t.Context(), state); err != nil {
		t.Fatal(err)
	}

	restarted := &GB28181API{svr: &Server{memoryStorer: store}}
	route, found, err := restarted.restoreCascadeTaskRouteByDownstream(
		t.Context(), state.Kind, state.DownstreamDeviceID, state.DownstreamSessionID,
	)
	if err != nil || !found || route == nil {
		t.Fatalf("restore legacy cascade route = route:%+v found:%v err:%v", route, found, err)
	}
	if !route.lastUpdatedAt().Equal(createdAt) {
		t.Fatalf("legacy cascade route updated at = %v, want created at %v", route.lastUpdatedAt(), createdAt)
	}
	upstreamKey := cascadeTaskUpstreamRouteKeyByName(
		state.Kind, state.PlatformName, state.ExposedID, state.UpstreamSessionID,
	)
	if indexed, ok := restarted.cascadeTaskRoutes.Load(upstreamKey); !ok || indexed != route {
		t.Fatal("legacy cascade route did not restore its upstream index")
	}
}

func TestCascadeTaskRouteSerializesDuplicateFinalNotifications(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var sends atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		sends.Add(1)
		entered <- struct{}{}
		<-release
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, "upgrade-session-0000000000000104", "upgrade-request")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>104</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := api.forwardCascadeTaskNotification(t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body)
			if err != nil {
				errs <- err
			}
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("forward duplicate notification: %v", err)
	}
	if sends.Load() != 1 {
		t.Fatalf("duplicate final notification sends = %d, want 1", sends.Load())
	}
}

type failCompletedCascadeTaskRouteMemory struct {
	*persistentTaskMemory
	failCompleted      atomic.Bool
	completionAttempts atomic.Int32
}

type blockingPendingCascadeTaskRouteMemory struct {
	*persistentTaskMemory
	blockNextPending atomic.Bool
	entered          chan struct{}
	release          chan struct{}
	mu               sync.Mutex
	updatedAt        []time.Time
}

func (m *blockingPendingCascadeTaskRouteMemory) SaveGBCascadeTaskRoute(
	ctx context.Context,
	kind, platformName, downstreamDeviceID, downstreamSessionID, exposedID, upstreamSessionID string,
	payload []byte,
	updatedAt time.Time,
) error {
	m.mu.Lock()
	m.updatedAt = append(m.updatedAt, updatedAt)
	m.mu.Unlock()
	if !strings.Contains(string(payload), `"completed":true`) && m.blockNextPending.CompareAndSwap(true, false) {
		close(m.entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.release:
		}
	}
	return m.persistentTaskMemory.SaveGBCascadeTaskRoute(
		ctx, kind, platformName, downstreamDeviceID, downstreamSessionID, exposedID, upstreamSessionID, payload, updatedAt,
	)
}

func TestCascadeTaskRoutePersistenceUsesUpdatedAt(t *testing.T) {
	store := &blockingPendingCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	createdAt := time.Now().Add(-cascadeTaskRouteTTL - time.Hour)
	route := &cascadeTaskRoute{
		kind: cascadeTaskUpgrade, upstreamPlatform: "updated-at-platform",
		downstreamDeviceID: gb10DeviceID, downstreamTargetID: testCascadeChannelID,
		exposedID:           testExposedChannelID,
		upstreamSessionID:   "updated-at-upstream-session-00001",
		downstreamSessionID: "updated-at-downstream-session-001",
		requestFingerprint:  "updated-at-request", createdAt: createdAt,
		startDone: make(chan struct{}),
	}
	route.setUpdatedAt(createdAt)
	close(route.startDone)
	before := time.Now()
	if err := api.persistCascadeTaskRoute(t.Context(), route); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	indexedAt := store.updatedAt[len(store.updatedAt)-1]
	store.mu.Unlock()
	if indexedAt.Before(before) || !indexedAt.Equal(route.lastUpdatedAt()) || indexedAt.Equal(createdAt) {
		t.Fatalf("cascade route indexed timestamp = %v, route=%v created=%v before=%v", indexedAt, route.lastUpdatedAt(), createdAt, before)
	}
}

func TestCascadeTaskPendingPersistenceCannotOverwriteCompletedTombstone(t *testing.T) {
	store := &blockingPendingCascadeTaskRouteMemory{
		persistentTaskMemory: newPersistentTaskMemory(GBVersion30),
		entered:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	var sends atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		sends.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-stale-save-000001", "upgrade-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	store.blockNextPending.Store(true)
	pendingDone := make(chan error, 1)
	go func() { pendingDone <- api.persistCascadeTaskRoute(t.Context(), route) }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("pending cascade route persistence did not block")
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>209</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	finalDone := make(chan error, 1)
	go func() {
		_, finalErr := api.forwardCascadeTaskNotification(
			t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
		)
		finalDone <- finalErr
	}()
	select {
	case err := <-finalDone:
		close(store.release)
		t.Fatalf("final notification bypassed in-flight pending persistence: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	if err := <-pendingDone; err != nil {
		t.Fatal(err)
	}
	if err := <-finalDone; err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 1 {
		t.Fatalf("final notification sends = %d, want 1", sends.Load())
	}
	persisted, found, err := api.loadCascadeTaskRouteStateByDownstream(
		t.Context(), cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID,
	)
	if err != nil || !found || !persisted.Completed {
		t.Fatalf("cascade completion tombstone after stale save race = found:%v completed:%v err:%v", found, persisted.Completed, err)
	}
}

func (m *failCompletedCascadeTaskRouteMemory) SaveGBCascadeTaskRoute(
	ctx context.Context,
	kind, platformName, downstreamDeviceID, downstreamSessionID, exposedID, upstreamSessionID string,
	payload []byte,
	updatedAt time.Time,
) error {
	if strings.Contains(string(payload), `"completed":true`) {
		m.completionAttempts.Add(1)
		if m.failCompleted.Load() {
			return errCascadeTaskRouteSave
		}
	}
	return m.persistentTaskMemory.SaveGBCascadeTaskRoute(
		ctx, kind, platformName, downstreamDeviceID, downstreamSessionID, exposedID, upstreamSessionID, payload, updatedAt,
	)
}

func TestCascadeTaskCompletionPersistenceRetryIsBounded(t *testing.T) {
	store := &failCompletedCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	for index := range 9 {
		route := &cascadeTaskRoute{
			kind: cascadeTaskUpgrade, worker: worker, upstreamPlatform: worker.platform.name,
			downstreamDeviceID: gb10DeviceID, downstreamTargetID: testCascadeChannelID,
			exposedID:           testExposedChannelID,
			upstreamSessionID:   fmt.Sprintf("bounded-upstream-session-%08d", index),
			downstreamSessionID: fmt.Sprintf("bounded-downstream-session-%06d", index),
			requestFingerprint:  fmt.Sprintf("request-%d", index), createdAt: time.Now(),
			completed: true, completionPending: true, startDone: make(chan struct{}),
		}
		route.setUpdatedAt(route.createdAt)
		close(route.startDone)
		api.cascadeTaskRoutes.Store(cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID), route)
	}

	api.retryPendingCompletedCascadeTaskRoutes()
	if got := store.completionAttempts.Load(); got != 8 {
		t.Fatalf("first completion persistence retry batch = %d, want 8", got)
	}
	api.retryPendingCompletedCascadeTaskRoutes()
	if got := store.completionAttempts.Load(); got != 9 {
		t.Fatalf("second completion persistence retry total = %d, want 9", got)
	}
}

func TestCascadeTaskCompletionPersistenceRetrySkipsRetiredRoute(t *testing.T) {
	store := &failCompletedCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	api := &GB28181API{svr: &Server{memoryStorer: store}}
	route := &cascadeTaskRoute{
		kind: cascadeTaskUpgrade, upstreamPlatform: "retired-platform",
		downstreamDeviceID: gb10DeviceID, downstreamTargetID: testCascadeChannelID,
		exposedID:           testExposedChannelID,
		upstreamSessionID:   "retired-upstream-session-000001",
		downstreamSessionID: "retired-downstream-session-00001",
		requestFingerprint:  "retired-request", createdAt: time.Now(),
		completed: true, completionPending: true, startDone: make(chan struct{}),
	}
	route.setUpdatedAt(route.createdAt)
	close(route.startDone)
	api.cascadeTaskRoutes.Store(cascadeTaskRouteKey(route.kind, route.downstreamDeviceID, route.downstreamSessionID), route)
	route.retired.Store(true)

	api.retryPendingCompletedCascadeTaskRoutes()
	if got := store.completionAttempts.Load(); got != 0 {
		t.Fatalf("retired route completion persistence attempts = %d, want 0", got)
	}
	if !route.completionPending {
		t.Fatal("retired route completion state was mutated")
	}
}

func TestCascadeTaskCompletionRetryCannotRecreateRouteAfterDeviceDeletion(t *testing.T) {
	store := &failCompletedCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-delete-race-0001", "upgrade-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	route.notifyMu.Lock()
	route.completed = true
	route.completionPending = true
	route.notifyMu.Unlock()

	unlockDelete := server.LockDeviceDelete(gb10DeviceID)
	retryDone := make(chan struct{})
	go func() {
		api.retryPendingCompletedCascadeTaskRoutes()
		close(retryDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		api.registerOperationMu.Lock()
		entry := api.registerOperations[gb10DeviceID]
		queued := entry != nil && entry.refs == 2
		api.registerOperationMu.Unlock()
		if queued {
			break
		}
		if time.Now().After(deadline) {
			unlockDelete()
			t.Fatal("completion retry did not queue behind device deletion lock")
		}
		runtime.Gosched()
	}
	api.removeCascadeTaskRoutesForDevice(gb10DeviceID)
	if err := store.DeleteGBCascadeTaskRoute(t.Context(), route.kind, route.downstreamDeviceID, route.downstreamSessionID); err != nil {
		unlockDelete()
		t.Fatal(err)
	}
	store.completionAttempts.Store(0)
	unlockDelete()
	select {
	case <-retryDone:
	case <-time.After(time.Second):
		t.Fatal("completion retry did not finish after device deletion")
	}
	if got := store.completionAttempts.Load(); got != 0 {
		t.Fatalf("completion retry recreated deleted device route with %d saves", got)
	}
	if _, found, err := api.loadCascadeTaskRouteStateByDownstream(
		t.Context(), route.kind, route.downstreamDeviceID, route.downstreamSessionID,
	); err != nil || found {
		t.Fatalf("deleted device route after retry = found:%v err:%v", found, err)
	}
}

func TestCascadeFinalNotificationRetriesCompletionPersistenceWithoutResending(t *testing.T) {
	store := &failCompletedCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	store.failCompleted.Store(true)
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	var sends atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		sends.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-final-persist-retry-1", "upgrade-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>206</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	if forwarded, err := api.forwardCascadeTaskNotification(
		t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
	); !forwarded || !errors.Is(err, errCascadeTaskRouteSave) {
		t.Fatalf("first final notification = forwarded:%v err:%v", forwarded, err)
	}
	if sends.Load() != 1 || !route.completed {
		t.Fatalf("first final notification state = sends:%d completed:%v", sends.Load(), route.completed)
	}

	store.failCompleted.Store(false)
	if forwarded, err := api.forwardCascadeTaskNotification(
		t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
	); err != nil || !forwarded {
		t.Fatalf("retried final notification = forwarded:%v err:%v", forwarded, err)
	}
	if sends.Load() != 1 {
		t.Fatalf("retried final notification sends = %d, want 1", sends.Load())
	}
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID)); ok {
		t.Fatal("persisted completed route retained its downstream runtime index")
	}
}

func TestCascadeTaskCompletionPersistenceRetriesWithoutDeviceRetransmission(t *testing.T) {
	store := &failCompletedCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	store.failCompleted.Store(true)
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	var sends atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		sends.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-background-persist-1", "upgrade-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>208</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	if forwarded, err := api.forwardCascadeTaskNotification(
		t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
	); !forwarded || !errors.Is(err, errCascadeTaskRouteSave) {
		t.Fatalf("first final notification = forwarded:%v err:%v", forwarded, err)
	}
	if !route.completionPending {
		t.Fatal("failed completion persistence was not retained for retry")
	}

	store.failCompleted.Store(false)
	api.retryPendingCompletedCascadeTaskRoutes()
	if route.completionPending {
		t.Fatal("successful background retry retained pending completion state")
	}
	if sends.Load() != 1 {
		t.Fatalf("background completion retry resent business notification %d times", sends.Load())
	}
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID)); ok {
		t.Fatal("background completion retry retained downstream runtime index")
	}
	persisted, found, err := api.loadCascadeTaskRouteStateByDownstream(
		t.Context(), cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID,
	)
	if err != nil || !found || !persisted.Completed {
		t.Fatalf("background completion persistence = found:%v completed:%v err:%v", found, persisted.Completed, err)
	}
}

func TestCascadeCompletedRouteSurvivesWorkerReplacementWhenCompletionPersistenceFails(t *testing.T) {
	store := &failCompletedCascadeTaskRouteMemory{persistentTaskMemory: newPersistentTaskMemory(GBVersion30)}
	store.failCompleted.Store(true)
	server := &Server{memoryStorer: store}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	var sends atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		sends.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-worker-replacement-1", "upgrade-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>207</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	if forwarded, err := api.forwardCascadeTaskNotification(
		t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
	); !forwarded || !errors.Is(err, errCascadeTaskRouteSave) {
		t.Fatalf("first final notification = forwarded:%v err:%v", forwarded, err)
	}

	// 模拟 CascadeManager.Apply 替换上级 worker。完成墓碑仍未落库时，
	// 路由必须保留为幂等墓碑，不能被热更新清理。
	api.removeCascadeTaskRoutes(worker)
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID)); !ok {
		t.Fatal("completed route was removed during worker replacement")
	}
	if !route.workerDetached.Load() {
		t.Fatal("completed route was not detached from replaced worker")
	}

	store.failCompleted.Store(false)
	if forwarded, err := api.forwardCascadeTaskNotification(
		t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
	); err != nil || !forwarded {
		t.Fatalf("retry final notification after worker replacement = forwarded:%v err:%v", forwarded, err)
	}
	if sends.Load() != 1 {
		t.Fatalf("final notification sends after worker replacement = %d, want 1", sends.Load())
	}
}

func TestCascadeUpgradeFinalNotificationIsNotForwardedAgainAfterSIPOKFailure(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(testCascadeChannelID, &Channel{ChannelID: testCascadeChannelID, device: memory.runtime})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(
		t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-ack-write-failure-02", "upgrade-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	var forwards atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		forwards.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>205</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("cascade upgrade SIP OK write failed"),
	}
	request := newFlowRequest(t, base, sip.MethodMessage, "cascade-upgrade-ack-write-failure", body)
	request.SetConnection(connection)
	done := make(chan struct{})
	go func() {
		api.sipMessageDeviceUpgradeResult(&sip.Context{
			Request: request, Tx: sip.NewTransaction("cascade-upgrade-ack-write-failure-tx", connection),
			DeviceID: gb10DeviceID, Source: base.remote,
		})
		close(done)
	}()
	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("cascade upgrade SIP OK write did not start")
	}
	if forwards.Load() != 1 || !route.completed {
		close(connection.release)
		t.Fatalf("cascade upgrade before SIP OK = forwards:%d completed:%v", forwards.Load(), route.completed)
	}
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cascade upgrade handler did not return after SIP OK write failure")
	}

	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "cascade-upgrade-ack-write-retry", body, api.sipMessageDeviceUpgradeResult))
	if forwards.Load() != 1 {
		t.Fatalf("cascade upgrade final notification forwards = %d, want 1", forwards.Load())
	}
}

func TestCascadeUpgradeRetransmissionStartsDownstreamOnce(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var starts atomic.Int32
	api.cascadeDeviceControl = func(_ context.Context, target *ipc.Channel, request *deviceControlA23Request) (string, error) {
		if target.ChannelID != channel.ChannelID || request.DeviceUpgrade == nil {
			t.Fatalf("upgrade target/request = %+v / %+v", target, request)
		}
		starts.Add(1)
		entered <- struct{}{}
		<-release
		return ptzResultOK, nil
	}
	body, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 105, DeviceID: testExposedChannelID,
		DeviceUpgrade: &deviceUpgradeConfig{
			Firmware: "V3", FileURL: "https://example.invalid/firmware.bin", Manufacturer: "Vendor",
			SessionID: "upgrade-session-0000000000000106",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			api.forwardCascadeDeviceControl(worker, body, t.Context())
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("duplicate upgrade downstream starts = %d, want 1", starts.Load())
	}
}

func TestCascadeSnapshotRetransmissionStartsDownstreamOnce(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var starts atomic.Int32
	api.cascadeDeviceConfig = func(_ context.Context, target *ipc.Channel, request *DeviceConfigRequest) (string, error) {
		if target.ChannelID != channel.ChannelID || request.SnapShotConfig == nil {
			t.Fatalf("snapshot target/request = %+v / %+v", target, request)
		}
		starts.Add(1)
		entered <- struct{}{}
		<-release
		return "OK", nil
	}
	body, err := sip.XMLEncode(DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 106, DeviceID: testExposedChannelID,
		SnapShotConfig: &SnapShot{SnapNum: 1, Interval: 1, UploadURL: "https://example.invalid/upload", SessionID: "snapshot-session-0000000000000106"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			api.forwardCascadeDeviceConfig(worker, body, t.Context())
		}()
	}
	<-entered
	close(release)
	wg.Wait()
	if starts.Load() != 1 {
		t.Fatalf("duplicate snapshot downstream starts = %d, want 1", starts.Load())
	}
}

func TestCascadeDeviceConfigRespectsDisabledDeviceCapabilitiesBeforeTaskRegistration(t *testing.T) {
	tests := []struct {
		name     string
		disabled string
		request  DeviceConfigRequest
	}{
		{
			name: "config write", disabled: "config_write",
			request: DeviceConfigRequest{
				CmdType: "DeviceConfig", SN: 201, DeviceID: testExposedChannelID,
				BasicParam: &BasicParam{Name: "camera", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
			},
		},
		{
			name: "snapshot", disabled: "snapshot",
			request: DeviceConfigRequest{
				CmdType: "DeviceConfig", SN: 202, DeviceID: testExposedChannelID,
				SnapShotConfig: &SnapShot{
					SnapNum: 1, Interval: 1, UploadURL: "https://example.invalid/upload",
					SessionID: "snapshot-disabled-session-0000000001",
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBProfile(GBVersion30, []string{test.disabled})
			server := &Server{memoryStorer: memory}
			api := &GB28181API{core: adapter, svr: server}
			server.gb = api
			worker := newCascadeWorker(server, testSharedCascadePlatform(t))
			worker.effective = GBVersion30
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			calls := 0
			api.cascadeDeviceConfig = func(context.Context, *ipc.Channel, *DeviceConfigRequest) (string, error) {
				calls++
				return "OK", nil
			}
			body, err := sip.XMLEncode(test.request)
			if err != nil {
				t.Fatal(err)
			}

			api.forwardCascadeDeviceConfig(worker, body, t.Context())

			if calls != 0 {
				t.Fatalf("disabled %s reached downstream DeviceConfig %d times", test.disabled, calls)
			}
			if got := api.cascadeTaskRouteCount(); got != 0 {
				t.Fatalf("disabled %s registered %d cascade task routes", test.disabled, got)
			}
		})
	}
}

func TestCascadeDeviceConfigRespectsDownstreamVersionBeforeTaskRegistration(t *testing.T) {
	tests := []struct {
		name              string
		downstreamVersion GBProtocolVersion
		request           DeviceConfigRequest
	}{
		{
			name: "2011 config write", downstreamVersion: GBVersion10,
			request: DeviceConfigRequest{
				CmdType: "DeviceConfig", SN: 203, DeviceID: testExposedChannelID,
				BasicParam: &BasicParam{Name: "camera"},
			},
		},
		{
			name: "2014 snapshot", downstreamVersion: GBVersion11,
			request: DeviceConfigRequest{
				CmdType: "DeviceConfig", SN: 204, DeviceID: testExposedChannelID,
				SnapShotConfig: &SnapShot{
					SnapNum: 1, Interval: 1, UploadURL: "https://example.invalid/upload",
					SessionID: "snapshot-session-0000000000000204",
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBProfile(test.downstreamVersion, nil)
			server := &Server{memoryStorer: memory}
			api := &GB28181API{core: adapter, svr: server}
			server.gb = api
			worker := newCascadeWorker(server, testSharedCascadePlatform(t))
			worker.effective = GBVersion30
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			calls := 0
			api.cascadeDeviceConfig = func(context.Context, *ipc.Channel, *DeviceConfigRequest) (string, error) {
				calls++
				return "OK", nil
			}
			body, err := sip.XMLEncode(test.request)
			if err != nil {
				t.Fatal(err)
			}

			api.forwardCascadeDeviceConfig(worker, body, t.Context())

			if calls != 0 {
				t.Fatalf("2022 DeviceConfig reached %s downstream hook %d times", test.downstreamVersion.StandardName(), calls)
			}
			if got := api.cascadeTaskRouteCount(); got != 0 {
				t.Fatalf("unsupported DeviceConfig registered %d cascade task routes", got)
			}
		})
	}
}

func TestCascadeCompletedTaskRetainsUpstreamDeduplication(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	sessionID := "upgrade-session-0000000000000107"
	route, created, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, sessionID, "upgrade-request")
	if err != nil || !created {
		t.Fatalf("register route = created %v, err %v", created, err)
	}
	if result, err := route.finishStart(ptzResultOK, nil); err != nil || result != ptzResultOK {
		t.Fatalf("finish start = %q, %v", result, err)
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>107</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	if forwarded, err := api.forwardCascadeTaskNotification(t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body); err != nil || !forwarded {
		t.Fatalf("forward completed task = %v, %v", forwarded, err)
	}
	existing, created, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, sessionID, "upgrade-request")
	if err != nil || created || existing != route {
		t.Fatalf("retransmission route = same %v, created %v, err %v", existing == route, created, err)
	}
	if result, err := existing.waitStart(t.Context()); err != nil || result != ptzResultOK {
		t.Fatalf("cached task result = %q, %v", result, err)
	}
	if _, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, sessionID, "changed-request"); err == nil {
		t.Fatal("same session with changed task payload was accepted")
	}
}

func TestCascadeTaskRouteRestoresBothIndexesAfterRestart(t *testing.T) {
	for _, test := range []struct {
		name         string
		kind         string
		upstreamID   string
		fingerprint  string
		initialState any
		body         func(string) []byte
	}{
		{
			name: "upgrade", kind: cascadeTaskUpgrade,
			upstreamID: "upgrade-session-restart-000000001", fingerprint: "upgrade-request-restart",
			initialState: UpgradeState{Firmware: "V3"},
			body: func(sessionID string) []byte {
				return []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>120</SN><DeviceID>` + testCascadeChannelID +
					`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
			},
		},
		{
			name: "snapshot", kind: cascadeTaskSnapshot,
			upstreamID: "snapshot-session-restart-00000001", fingerprint: "snapshot-request-restart",
			initialState: SnapshotState{ExpectedCount: 1},
			body: func(sessionID string) []byte {
				return []byte(`<Notify><CmdType>UploadSnapShotFinished</CmdType><SN>121</SN><DeviceID>` + testCascadeChannelID +
					`</DeviceID><SessionID>` + sessionID + `</SessionID><SnapShotList><SnapShotFileID>` +
					testCascadeChannelID + `022026082508150000001</SnapShotFileID></SnapShotList></Notify>`)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newPersistentTaskMemory(GBVersion30)
			store.device.Channels.Store(testCascadeChannelID, &Channel{ChannelID: testCascadeChannelID, device: store.device})
			platform := testSharedCascadePlatform(t)
			firstWorker := newCascadeWorker(nil, platform)
			firstWorker.effective = GBVersion30
			first := &GB28181API{svr: &Server{memoryStorer: store}}
			channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
			route, created, err := first.registerCascadeTaskRoute(
				t.Context(), test.kind, firstWorker, channel, testExposedChannelID,
				test.upstreamID, test.fingerprint, test.initialState,
			)
			if err != nil || !created {
				t.Fatalf("register persistent route = created %v, err %v", created, err)
			}
			if _, err := route.finishStart("OK", nil); err != nil {
				t.Fatal(err)
			}
			if err := first.persistCascadeTaskRoute(t.Context(), route); err != nil {
				t.Fatal(err)
			}
			if err := first.finishCascadeTaskState(t.Context(), route, "OK", nil); err != nil {
				t.Fatal(err)
			}

			forwardedBodies := make(chan string, 1)
			restartedWorker := newCascadeWorker(nil, platform)
			restartedWorker.effective = GBVersion30
			restartedWorker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				forwardedBodies <- string(request.Body())
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			restartedServer := &Server{memoryStorer: store}
			restartedManager := NewCascadeManager(restartedServer)
			restartedManager.items[platform.name] = restartedWorker
			restartedServer.cascade = restartedManager
			restarted := &GB28181API{svr: restartedServer}
			restartedServer.gb = restarted

			restored, created, err := restarted.registerCascadeTaskRoute(
				t.Context(), test.kind, restartedWorker, channel, testExposedChannelID,
				test.upstreamID, test.fingerprint, test.initialState,
			)
			if err != nil || created || restored.downstreamSessionID != route.downstreamSessionID {
				t.Fatalf("restore upstream route = %+v, created %v, err %v", restored, created, err)
			}
			if result, err := restored.waitStart(t.Context()); err != nil || result != "OK" {
				t.Fatalf("restored start result = %q, %v", result, err)
			}
			body := test.body(route.downstreamSessionID)
			if forwarded, err := restarted.forwardCascadeTaskNotification(
				t.Context(), test.kind, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
			); err != nil || !forwarded {
				t.Fatalf("restored final forwarding = %v, %v", forwarded, err)
			}
			forwarded := <-forwardedBodies
			if !strings.Contains(forwarded, test.upstreamID) || !strings.Contains(forwarded, testExposedChannelID) ||
				strings.Contains(forwarded, route.downstreamSessionID) || strings.Contains(forwarded, testCascadeChannelID) {
				t.Fatalf("restored final notification was not rewritten: %s", forwarded)
			}

			// 已完成路由只用于幂等确认，不应因当前上级配置已删除而拒绝下级重传。
			duplicateServer := &Server{memoryStorer: store}
			duplicateAPI := &GB28181API{svr: duplicateServer}
			duplicateServer.gb = duplicateAPI
			if forwarded, err := duplicateAPI.forwardCascadeTaskNotification(
				t.Context(), test.kind, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body,
			); err != nil || !forwarded {
				t.Fatalf("completed route duplicate without upstream config = %v, err %v", forwarded, err)
			}
			retryWorker := newCascadeWorker(nil, platform)
			retryWorker.effective = GBVersion30
			retried, created, err := duplicateAPI.registerCascadeTaskRoute(
				t.Context(), test.kind, retryWorker, channel, testExposedChannelID,
				test.upstreamID, test.fingerprint, test.initialState,
			)
			if err != nil || created || retried.downstreamSessionID != route.downstreamSessionID {
				t.Fatalf("completed route upstream retry = %+v, created %v, err %v", retried, created, err)
			}
		})
	}
}

func TestCascadeTaskFinalNotificationWinsLateStartFailure(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-0000000000000108", "upgrade-request")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>108</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + route.downstreamSessionID + `</SessionID><UpgradeResult>OK</UpgradeResult><Firmware>V3</Firmware></Notify>`)
	if forwarded, err := api.forwardCascadeTaskNotification(t.Context(), cascadeTaskUpgrade, gb10DeviceID, testCascadeChannelID, route.downstreamSessionID, body); err != nil || !forwarded {
		t.Fatalf("forward early final notification = %v, %v", forwarded, err)
	}
	result, err := route.finishStart("ERROR", context.DeadlineExceeded)
	if err != nil || result != ptzResultOK {
		t.Fatalf("late start failure won over final notification: %q, %v", result, err)
	}
	if result, err := route.waitStart(t.Context()); err != nil || result != ptzResultOK {
		t.Fatalf("cached final task result = %q, %v", result, err)
	}
}

func TestCascadeTaskRouteCapacityRejectsNewTaskWithoutEviction(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	api := &GB28181API{}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	for index := range maxCascadeTaskRoutes {
		route := &cascadeTaskRoute{
			kind: cascadeTaskUpgrade, worker: worker, downstreamDeviceID: gb10DeviceID, downstreamTargetID: testCascadeChannelID,
			exposedID: testExposedChannelID, upstreamSessionID: fmt.Sprintf("upgrade-session-%017d", index),
			downstreamSessionID: fmt.Sprintf("downstream-session-%014d", index), requestFingerprint: fmt.Sprintf("request-%d", index),
			startDone: make(chan struct{}), createdAt: time.Now(),
		}
		route.setUpdatedAt(route.createdAt)
		api.cascadeTaskRoutes.Store(cascadeTaskUpstreamRouteKey(route.kind, worker, route.exposedID, route.upstreamSessionID), route)
	}
	if _, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-0000000000000109", "new-request"); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("capacity error = %v", err)
	}
	if count := api.cascadeTaskRouteCount(); count != maxCascadeTaskRoutes {
		t.Fatalf("task routes after capacity rejection = %d, want %d", count, maxCascadeTaskRoutes)
	}
}

func TestCascadeTaskRoutesAreRemovedWithUpstreamWorker(t *testing.T) {
	api := &GB28181API{}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, "upgrade-session-0000000000000105", "upgrade-request")
	if err != nil {
		t.Fatal(err)
	}
	api.removeCascadeTaskRoutes(worker)
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID)); ok {
		t.Fatal("task route survived upstream removal")
	}
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskUpstreamRouteKey(cascadeTaskUpgrade, worker, testExposedChannelID, route.upstreamSessionID)); ok {
		t.Fatal("task upstream index survived upstream removal")
	}
}

func TestCascadeTaskRoutesAreRemovedWithDownstreamDevice(t *testing.T) {
	api := &GB28181API{}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID,
		"upgrade-session-0000000000000110", "upgrade-request")
	if err != nil {
		t.Fatal(err)
	}
	api.removeCascadeTaskRoutesForDevice(gb10DeviceID)
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID)); ok {
		t.Fatal("task route survived downstream device removal")
	}
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskUpstreamRouteKey(cascadeTaskUpgrade, worker, testExposedChannelID, route.upstreamSessionID)); ok {
		t.Fatal("task upstream index survived downstream device removal")
	}
}

func TestCascadeVideoUploadNotifyForwardsOnlyShared2022Target(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.effective = GBVersion30
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	requests := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests <- request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	server := &Server{cascade: manager}
	api := &GB28181API{svr: server}
	server.gb = api
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>103</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-08-26T20:00:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}
	assertCascadeTaskBody(t, <-requests, "VideoUploadNotify", testExposedChannelID, "")
}

func TestCascadeVideoUploadNotifyDuplicateNewTransactionsForwardOnceAcrossRestart(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.effective = GBVersion30
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	var calls atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		calls.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	store := newPersistentTaskMemory(GBVersion30)
	server := &Server{memoryStorer: store, cascade: manager}
	api := &GB28181API{svr: server}
	server.gb = api
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>203</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-09-01T03:20:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}

	restartedServer := &Server{memoryStorer: store, cascade: manager}
	restarted := &GB28181API{svr: restartedServer}
	restartedServer.gb = restarted
	if err := restarted.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("duplicate VideoUploadNotify forwards = %d, want 1", got)
	}
}

func TestCascadeVideoUploadNotifyRetriesOnlyFailedUpstream(t *testing.T) {
	firstPlatform := testSharedCascadePlatform(t)
	firstPlatform.name = "video-upload-first"
	secondPlatform := testSharedCascadePlatform(t)
	secondPlatform.name = "video-upload-second"
	first := newCascadeWorker(nil, firstPlatform)
	second := newCascadeWorker(nil, secondPlatform)
	first.effective = GBVersion30
	second.effective = GBVersion30
	first.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	second.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	first.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		firstCalls.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	second.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		call := secondCalls.Add(1)
		status := http.StatusServiceUnavailable
		if call > 1 {
			status = http.StatusOK
		}
		return sip.NewResponseFromRequest("", request, status, http.StatusText(status), nil), nil
	}
	manager := NewCascadeManager(nil)
	manager.items[firstPlatform.name] = first
	manager.items[secondPlatform.name] = second
	store := newPersistentTaskMemory(GBVersion30)
	server := &Server{memoryStorer: store, cascade: manager}
	api := &GB28181API{svr: server}
	server.gb = api
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>204</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-09-01T03:21:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err == nil {
		t.Fatal("failed VideoUploadNotify upstream was reported as success")
	}
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("successful VideoUploadNotify upstream calls = %d, want 1", got)
	}
	if got := secondCalls.Load(); got != 2 {
		t.Fatalf("retried VideoUploadNotify upstream calls = %d, want 2", got)
	}
}

func TestCascadeVideoUploadNotifySkipsLegacyUpstream(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.effective = GBVersion20
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	called := false
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		called = true
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	server := &Server{cascade: manager}
	api := &GB28181API{cfg: &conf.SIP{}, svr: server}
	server.gb = api
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>104</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-08-26T20:00:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude></Notify>`)
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("VideoUploadNotify was forwarded to a 2.0 upstream")
	}
}

func TestInvalidVideoUploadNotifyDoesNotForwardCascade(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(testCascadeChannelID, &Channel{ChannelID: testCascadeChannelID, device: memory.runtime})
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.effective = GBVersion30
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	called := false
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		called = true
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	server := &Server{memoryStorer: memory, cascade: manager}
	api := &GB28181API{svr: server}
	server.gb = api
	body := []byte(`<Notify><CmdType>VideoUploadNotify</CmdType><SN>105</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><Time>2026-08-26T20:00:00</Time><Longitude>120.12</Longitude><Latitude>30.28</Latitude><Info><doorType/></Info></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "invalid-video-upload-no-cascade", body, api.sipMessageVideoUploadNotify)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("invalid VideoUploadNotify response = %s", response)
	}
	api.lifecycleWG.Wait()
	if called {
		t.Fatal("invalid VideoUploadNotify was forwarded to an upstream platform")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && (state.VideoUpload != nil || len(state.AppendixA4) != 0) {
		t.Fatalf("invalid VideoUploadNotify changed state: %+v", state)
	}
}

func assertCascadeTaskBody(t *testing.T, request *sip.Request, cmdType, exposedID, sessionID string) {
	t.Helper()
	if request == nil {
		t.Fatal("cascade task request is nil")
	}
	body := string(request.Body())
	for _, expected := range []string{"<CmdType>" + cmdType + "</CmdType>", "<DeviceID>" + exposedID + "</DeviceID>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("cascade task body missing %q: %s", expected, body)
		}
	}
	if sessionID != "" && !strings.Contains(body, "<SessionID>"+sessionID+"</SessionID>") {
		t.Fatalf("cascade task body missing session %s: %s", sessionID, body)
	}
}
