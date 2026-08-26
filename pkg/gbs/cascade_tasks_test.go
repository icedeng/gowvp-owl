package gbs

import (
	"context"
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
	upgradeRoute, created, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, upgradeSession)
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

	snapshotSession := "snapshot-session-0000000000000102"
	snapshotRoute, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskSnapshot, worker, channel, testExposedChannelID, snapshotSession)
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
	firstRoute, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, first, channel, testExposedChannelID, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	secondRoute, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, second, channel, testExposedChannelID, sessionID)
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
	route, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, "upgrade-session-0000000000000104")
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

func TestCascadeTaskRoutesAreRemovedWithUpstreamWorker(t *testing.T) {
	api := &GB28181API{}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	route, _, err := api.registerCascadeTaskRoute(t.Context(), cascadeTaskUpgrade, worker, channel, testExposedChannelID, "upgrade-session-0000000000000105")
	if err != nil {
		t.Fatal(err)
	}
	api.removeCascadeTaskRoutes(worker)
	if _, ok := api.cascadeTaskRoutes.Load(cascadeTaskRouteKey(cascadeTaskUpgrade, gb10DeviceID, route.downstreamSessionID)); ok {
		t.Fatal("task route survived upstream removal")
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
