package gbs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

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
	var downstreamUpgrade string
	api.cascadeDeviceControl = func(_ context.Context, target *ipc.Channel, request *deviceControlA23Request) (string, error) {
		if target.ChannelID != channel.ChannelID || request.DeviceUpgrade == nil {
			t.Fatalf("upgrade target/request = %+v / %+v", target, request)
		}
		downstreamUpgrade = request.DeviceUpgrade.SessionID
		return ptzResultOK, nil
	}
	upgradeBody, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 99, DeviceID: testExposedChannelID,
		DeviceUpgrade: &deviceUpgradeConfig{
			Firmware: "V3", FileURL: "https://example.invalid/firmware.bin", Manufacturer: "Vendor", SessionID: upstreamUpgrade,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api.forwardCascadeDeviceControl(worker, upgradeBody, t.Context())
	if downstreamUpgrade == "" || downstreamUpgrade == upstreamUpgrade {
		t.Fatalf("downstream upgrade session = %q", downstreamUpgrade)
	}
	if state, ok := api.UpgradeState(gb10DeviceID, downstreamUpgrade); !ok || state.Status != "accepted" || state.ChannelID != channel.ChannelID || state.Firmware != "V3" {
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
		`</DeviceID><Time>2026-08-26T20:00:00</Time></Notify>`)
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}
	assertCascadeTaskBody(t, <-requests, "VideoUploadNotify", testExposedChannelID, "")
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
		`</DeviceID><Time>2026-08-26T20:00:00</Time></Notify>`)
	if err := api.forwardCascadeVideoUploadNotify(t.Context(), gb10DeviceID, body); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("VideoUploadNotify was forwarded to a 2.0 upstream")
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
