package gbs

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestTickerCheckStopsWithGB28181Lifecycle(t *testing.T) {
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	server := &Server{gb: api}
	done := make(chan struct{})
	api.startLifecycleWorker(func() {
		server.startTickerCheck()
		close(done)
	})
	api.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("device ticker did not stop with GB28181 lifecycle")
	}
}

func TestCloseWaitsForLifecycleWorkers(t *testing.T) {
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	started := make(chan struct{})
	release := make(chan struct{})
	api.startLifecycleWorker(func() {
		close(started)
		<-api.lifecycleDone
		<-release
	})
	<-started

	closed := make(chan struct{})
	go func() {
		api.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned while lifecycle worker was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not finish after lifecycle worker exited")
	}
}

func TestBackgroundWorkersStartAfterDependenciesAndOnlyOnce(t *testing.T) {
	cfg := conf.DefaultConfig()
	api := newGB28181API(&cfg, ipc.Adapter{}, nil)
	api.svr = &Server{}
	now := time.Now()
	api.directDownloads.mu.Lock()
	api.directDownloads.states["expired-before-start"] = DirectTCPDownloadState{
		SessionID:   "expired-before-start",
		Status:      directTCPStatusCompleted,
		StartedAt:   now.Add(-10 * 24 * time.Hour),
		CompletedAt: now.Add(-9 * 24 * time.Hour),
	}
	api.directDownloads.mu.Unlock()

	if !api.startBackgroundWorkers() {
		t.Fatal("background workers were already started before dependencies were attached")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, ok := api.directDownloads.State("expired-before-start"); !ok {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := api.directDownloads.State("expired-before-start"); ok {
		t.Fatal("runtime state cleaner did not run after background workers started")
	}
	if api.startBackgroundWorkers() {
		t.Fatal("background workers started more than once")
	}
	api.close()
}

func TestCloseCancelsAndWaitsForLifecycleTasks(t *testing.T) {
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	started := make(chan struct{})
	exited := make(chan struct{})
	if !api.startLifecycleTask(context.Background(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(exited)
	}) {
		t.Fatal("lifecycle task was rejected before close")
	}
	<-started

	api.close()
	select {
	case <-exited:
	default:
		t.Fatal("close returned before lifecycle task observed cancellation")
	}
	if api.startLifecycleTask(context.Background(), func(context.Context) {
		t.Error("task started after GB28181 close")
	}) {
		t.Fatal("lifecycle task was accepted after close")
	}
}

func TestCloseWaitsForAcceptedSIPRequestAndRejectsNewRequests(t *testing.T) {
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	requestDone, ok := api.beginLifecycleRequest()
	if !ok {
		t.Fatal("SIP request was rejected before close")
	}
	closeDone := make(chan struct{})
	go func() {
		api.close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("close returned while accepted SIP request was active")
	case <-time.After(20 * time.Millisecond):
	}
	if done, accepted := api.beginLifecycleRequest(); accepted {
		done()
		t.Fatal("SIP request was accepted after close started")
	}
	requestDone()
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("close did not return after accepted SIP request exited")
	}
}

func TestSIPLifecycleMiddlewareRejectsRequestAfterClose(t *testing.T) {
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	api.close()
	conn := newFlowConnection()
	messageRequest := newFlowRequest(t, conn, sip.MethodMessage, "after-close", nil)
	messageTx := sip.NewTransaction("after-close-tx", conn)
	api.sipLifecycleMiddleware(&sip.Context{Request: messageRequest, Tx: messageTx})
	select {
	case response := <-conn.writes:
		if !strings.Contains(string(response), "SIP/2.0 503") {
			t.Fatalf("closed lifecycle response = %q", response)
		}
		assertLifecycleVersionHeader(t, response, false)
	case <-time.After(time.Second):
		t.Fatal("closed lifecycle request was not rejected")
	}

	registerRequest := newFlowRequest(t, conn, sip.MethodRegister, "register-after-close", nil)
	registerTx := sip.NewTransaction("register-after-close-tx", conn)
	api.sipLifecycleMiddleware(&sip.Context{Request: registerRequest, Tx: registerTx})
	select {
	case response := <-conn.writes:
		if !strings.Contains(string(response), "SIP/2.0 503") {
			t.Fatalf("closed REGISTER lifecycle response = %q", response)
		}
		assertLifecycleVersionHeader(t, response, true)
	case <-time.After(time.Second):
		t.Fatal("closed lifecycle REGISTER was not rejected")
	}

	ackRequest := newFlowRequest(t, conn, sip.MethodACK, "ack-after-close", nil)
	ackCtx := &sip.Context{Request: ackRequest, Tx: sip.NewTransaction("ack-after-close-tx", conn)}
	api.sipLifecycleMiddleware(ackCtx)
	if !ackCtx.IsAborted() {
		t.Fatal("ACK was not aborted after lifecycle close")
	}
	select {
	case response := <-conn.writes:
		t.Fatalf("ACK received an invalid closed lifecycle response: %q", response)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestSIPLifecycleMiddlewareRejectsRequestsUntilStartupRecoveryCompletes(t *testing.T) {
	api := &GB28181API{
		lifecycleDone: make(chan struct{}),
		startupReady:  make(chan struct{}),
	}
	conn := newFlowConnection()
	request := newFlowRequest(t, conn, sip.MethodRegister, "during-startup", nil)
	tx := sip.NewTransaction("during-startup-tx", conn)
	ctx := &sip.Context{Request: request, Tx: tx}

	api.sipLifecycleMiddleware(ctx)
	if !ctx.IsAborted() {
		t.Fatal("SIP request was not aborted while startup recovery was incomplete")
	}
	select {
	case response := <-conn.writes:
		if !strings.Contains(string(response), "SIP/2.0 503 Service Unavailable") ||
			!strings.Contains(string(response), "\r\nRetry-After: 1\r\n") {
			t.Fatalf("startup response = %q", response)
		}
		assertLifecycleVersionHeader(t, response, true)
	case <-time.After(time.Second):
		t.Fatal("startup request was not rejected")
	}

	messageRequest := newFlowRequest(t, conn, sip.MethodMessage, "message-during-startup", nil)
	messageCtx := &sip.Context{Request: messageRequest, Tx: sip.NewTransaction("message-during-startup-tx", conn)}
	api.sipLifecycleMiddleware(messageCtx)
	if !messageCtx.IsAborted() {
		t.Fatal("MESSAGE was not aborted while startup recovery was incomplete")
	}
	select {
	case response := <-conn.writes:
		if !strings.Contains(string(response), "SIP/2.0 503 Service Unavailable") ||
			!strings.Contains(string(response), "\r\nRetry-After: 1\r\n") {
			t.Fatalf("startup MESSAGE response = %q", response)
		}
		assertLifecycleVersionHeader(t, response, false)
	case <-time.After(time.Second):
		t.Fatal("startup MESSAGE was not rejected")
	}

	api.markStartupReady()
	readyRequest := newFlowRequest(t, conn, sip.MethodRegister, "after-startup", nil)
	readyCtx := &sip.Context{Request: readyRequest, Tx: sip.NewTransaction("after-startup-tx", conn)}
	api.sipLifecycleMiddleware(readyCtx)
	if readyCtx.IsAborted() {
		t.Fatal("SIP request was rejected after startup recovery completed")
	}
	// 重复完成必须保持幂等，避免热路径误关已复用的就绪通道。
	api.markStartupReady()
}

func assertLifecycleVersionHeader(t *testing.T, response []byte, want bool) {
	t.Helper()
	raw := string(response)
	count := strings.Count(raw, "\r\nX-GB-Ver:")
	if !want {
		if count != 0 {
			t.Fatalf("lifecycle response unexpectedly contains X-GB-Ver: %q", response)
		}
		return
	}
	if count != 1 || !strings.Contains(raw, "\r\nX-GB-Ver: 3.0\r\n") {
		t.Fatalf("lifecycle REGISTER response must contain exactly one X-GB-Ver: 3.0 header: %q", response)
	}
}

func TestSIPLifecycleMiddlewareSilentlyDropsACKDuringStartupRecovery(t *testing.T) {
	api := &GB28181API{
		lifecycleDone: make(chan struct{}),
		startupReady:  make(chan struct{}),
	}
	conn := newFlowConnection()
	request := newFlowRequest(t, conn, sip.MethodACK, "startup-ack", nil)
	ctx := &sip.Context{Request: request, Tx: sip.NewTransaction("startup-ack-tx", conn)}

	api.sipLifecycleMiddleware(ctx)
	if !ctx.IsAborted() {
		t.Fatal("ACK was not aborted while startup recovery was incomplete")
	}
	select {
	case response := <-conn.writes:
		t.Fatalf("ACK received an invalid startup response: %q", response)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLifecycleTaskStartAndCloseAreRaceSafe(t *testing.T) {
	for range 50 {
		api := &GB28181API{lifecycleDone: make(chan struct{})}
		start := make(chan struct{})
		var callers sync.WaitGroup
		for range 20 {
			callers.Add(1)
			go func() {
				defer callers.Done()
				<-start
				api.startLifecycleTask(context.Background(), func(ctx context.Context) {
					<-ctx.Done()
				})
			}()
		}
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			api.close()
		}()
		close(start)
		callers.Wait()
		api.close()
	}
}

func TestOfflineCheckDoesNotOverrideNewKeepalive(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Minute)
	connection := newFlowConnection()
	base := &flowMemory{
		persistent: &ipc.Device{
			DeviceID: gb10DeviceID, IsOnline: true, RegisteredAt: orm.Time{Time: old},
			KeepaliveAt: orm.Time{Time: old}, Expires: 3600,
		},
		runtime: &Device{
			IsOnline: true, LastRegisterAt: old, LastKeepaliveAt: old, Expires: 3600,
			conn: connection, source: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			keepaliveInterval: 1, keepaliveTimeout: 1,
		},
	}
	memory := &refreshBeforeOfflineMemory{flowMemory: base, refreshedAt: now}
	api := &GB28181API{}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server

	server.checkOfflineDevices(now)

	if !memory.persistent.IsOnline || !memory.runtime.IsOnlineNow() {
		t.Fatal("stale offline scan overrode a newer keepalive")
	}
	if !memory.persistent.KeepaliveAt.Time.Equal(now) || !memory.runtime.runtimeSnapshot().LastKeepaliveAt.Equal(now) {
		t.Fatal("newer keepalive was not preserved")
	}
}

func TestOfflineCheckMarksUnchangedTimedOutDeviceOffline(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Minute)
	connection := newFlowConnection()
	memory := &flowMemory{
		persistent: &ipc.Device{
			DeviceID: gb10DeviceID, IsOnline: true, RegisteredAt: orm.Time{Time: old},
			KeepaliveAt: orm.Time{Time: old}, Expires: 3600,
		},
		runtime: &Device{
			IsOnline: true, LastRegisterAt: old, LastKeepaliveAt: old, Expires: 3600,
			conn: connection, source: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			keepaliveInterval: 1, keepaliveTimeout: 1,
		},
	}
	api := &GB28181API{}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server

	server.checkOfflineDevices(now)

	if memory.persistent.IsOnline || memory.runtime.IsOnlineNow() {
		t.Fatal("unchanged timed-out device remained online")
	}
}

func TestOfflineCheckRegistrationExpiry(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		registeredAt time.Time
		expires      int
		wantOnline   bool
	}{
		{name: "expired_with_recent_keepalive", registeredAt: now.Add(-11 * time.Second), expires: 10},
		{name: "not_expired_with_recent_keepalive", registeredAt: now.Add(-9 * time.Second), expires: 10, wantOnline: true},
		{name: "expires_at_boundary", registeredAt: now.Add(-10 * time.Second), expires: 10},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection := newFlowConnection()
			memory := &flowMemory{
				persistent: &ipc.Device{
					DeviceID: gb10DeviceID, IsOnline: true, RegisteredAt: orm.Time{Time: test.registeredAt},
					KeepaliveAt: orm.Time{Time: now}, Expires: test.expires,
				},
				runtime: &Device{
					IsOnline: true, LastRegisterAt: test.registeredAt, LastKeepaliveAt: now, Expires: test.expires,
					conn: connection, source: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
					keepaliveInterval: 60, keepaliveTimeout: 3,
				},
			}
			api := &GB28181API{}
			server := &Server{gb: api, memoryStorer: memory}
			api.svr = server

			server.checkOfflineDevices(now)

			if memory.persistent.IsOnline != test.wantOnline || memory.runtime.IsOnlineNow() != test.wantOnline {
				t.Fatalf("online state = persistent %t, runtime %t; want %t",
					memory.persistent.IsOnline, memory.runtime.IsOnlineNow(), test.wantOnline)
			}
		})
	}
}

func TestOfflineCheckDoesNotOverrideRenewedRegistration(t *testing.T) {
	now := time.Now()
	registeredAt := now.Add(-time.Minute)
	connection := newFlowConnection()
	base := &flowMemory{
		persistent: &ipc.Device{
			DeviceID: gb10DeviceID, IsOnline: true, RegisteredAt: orm.Time{Time: registeredAt},
			KeepaliveAt: orm.Time{Time: now}, Expires: 10,
		},
		runtime: &Device{
			IsOnline: true, LastRegisterAt: registeredAt, LastKeepaliveAt: now, Expires: 10,
			conn: connection, source: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			keepaliveInterval: 60, keepaliveTimeout: 3,
		},
	}
	memory := &renewRegistrationBeforeOfflineMemory{flowMemory: base, registeredAt: now, expires: 3600}
	api := &GB28181API{}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server

	server.checkOfflineDevices(now)

	if !memory.persistent.IsOnline || !memory.runtime.IsOnlineNow() {
		t.Fatal("stale registration-expiry scan overrode a renewed registration")
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.RegisteredAt.Time.Equal(now) || !state.LastRegisterAt.Equal(now) || state.Expires != 3600 {
		t.Fatal("renewed registration state was not preserved")
	}
}

func TestOfflineCheckRegistrationExpirySkipsTCPOptionsProbe(t *testing.T) {
	now := time.Now()
	registeredAt := now.Add(-time.Minute)
	connection := newFlowConnection()
	memory := &flowMemory{
		persistent: &ipc.Device{
			DeviceID: gb10DeviceID, IsOnline: true, RegisteredAt: orm.Time{Time: registeredAt},
			KeepaliveAt: orm.Time{Time: registeredAt}, Expires: 10,
		},
		runtime: &Device{
			IsOnline: true, LastRegisterAt: registeredAt, LastKeepaliveAt: registeredAt, Expires: 10,
			conn: connection, source: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			keepaliveInterval: 1, keepaliveTimeout: 1,
		},
	}
	api := &GB28181API{}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server

	server.checkOfflineDevices(now)

	if memory.persistent.IsOnline || memory.runtime.IsOnlineNow() {
		t.Fatal("expired registration remained online after TCP reachability check")
	}
	select {
	case payload := <-connection.writes:
		t.Fatalf("registration expiry unexpectedly sent OPTIONS: %q", payload)
	default:
	}
}

func TestOfflineCheckPersistenceFailureClosesRuntimeAndRetriesPersistence(t *testing.T) {
	now := time.Now()
	registeredAt := now.Add(-time.Minute)
	memory := &probePersistenceFailureMemory{
		flowMemory: &flowMemory{
			persistent: &ipc.Device{
				DeviceID: gb10DeviceID, IsOnline: true, RegisteredAt: orm.Time{Time: registeredAt},
				KeepaliveAt: orm.Time{Time: now}, Expires: 10,
			},
			runtime: &Device{
				IsOnline: true, LastRegisterAt: registeredAt, LastKeepaliveAt: now, Expires: 10,
				conn: newFlowConnection(), source: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			},
		},
		err: errors.New("database unavailable"),
	}
	api := &GB28181API{}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server
	pending := newPendingDeviceOperation(context.Background(), gb10DeviceID)
	api.pendingDeviceQuery.Store("offline-persistence", &pendingQueryWait{operation: pending})
	subscriptionKey := gb10DeviceID + "|" + gb10ChannelID + "|CATALOG"
	api.outgoingSubscriptions.Store(subscriptionKey, &outgoingSubscriptionDialog{deviceID: gb10DeviceID})

	server.checkOfflineDevices(now)

	state := memory.runtime.runtimeSnapshot()
	if state.IsOnline || !state.OfflinePersistencePending {
		t.Fatalf("failed persistence runtime state = online:%t pending:%t", state.IsOnline, state.OfflinePersistencePending)
	}
	if !memory.persistent.IsOnline {
		t.Fatal("failed persistence unexpectedly changed durable online state")
	}
	select {
	case <-pending.Done():
		if !errors.Is(pending.Cause(), ErrDeviceOffline) {
			t.Fatalf("pending operation cause = %v", pending.Cause())
		}
	default:
		t.Fatal("failed offline persistence retained pending device operation")
	}
	if _, exists := api.outgoingSubscriptions.Load(subscriptionKey); exists {
		t.Fatal("failed offline persistence retained outgoing subscription")
	}

	memory.err = nil
	server.checkOfflineDevices(now.Add(time.Second))
	state = memory.runtime.runtimeSnapshot()
	if memory.persistent.IsOnline || state.IsOnline || state.OfflinePersistencePending {
		t.Fatalf("offline persistence retry = durable:%t runtime:%t pending:%t",
			memory.persistent.IsOnline, state.IsOnline, state.OfflinePersistencePending)
	}
}

func TestOfflineCleanupCannotDeleteNewRegistrationState(t *testing.T) {
	now := time.Now()
	registeredAt := now.Add(-time.Minute)
	memory := &blockingOfflineCommitMemory{
		flowMemory: &flowMemory{
			persistent: &ipc.Device{
				DeviceID: gb10DeviceID, IsOnline: true, RegisteredAt: orm.Time{Time: registeredAt},
				KeepaliveAt: orm.Time{Time: now}, Expires: 10,
			},
			runtime: &Device{
				IsOnline: true, LastRegisterAt: registeredAt, LastKeepaliveAt: now, Expires: 10,
				conn: newFlowConnection(), source: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			},
		},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	api := &GB28181API{}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server

	scanDone := make(chan struct{})
	go func() {
		server.checkOfflineDevices(now)
		close(scanDone)
	}()
	select {
	case <-memory.entered:
	case <-time.After(time.Second):
		t.Fatal("offline persistence did not reach commit boundary")
	}

	subscriptionKey := gb10DeviceID + "|" + gb10ChannelID + "|CATALOG"
	registerDone := make(chan struct{})
	go func() {
		unlock := api.lockRegisterOperation(gb10DeviceID)
		memory.renew(now, 3600)
		api.outgoingSubscriptions.Store(subscriptionKey, &outgoingSubscriptionDialog{deviceID: gb10DeviceID})
		unlock()
		close(registerDone)
	}()
	select {
	case <-registerDone:
		t.Fatal("new registration bypassed in-flight offline cleanup")
	case <-time.After(50 * time.Millisecond):
	}
	close(memory.release)
	select {
	case <-scanDone:
	case <-time.After(time.Second):
		t.Fatal("offline cleanup did not finish")
	}
	select {
	case <-registerDone:
	case <-time.After(time.Second):
		t.Fatal("new registration did not resume after offline cleanup")
	}
	if _, exists := api.outgoingSubscriptions.Load(subscriptionKey); !exists {
		t.Fatal("old offline cleanup deleted the new registration subscription")
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline || state.OfflinePersistencePending ||
		!state.LastRegisterAt.Equal(now) || state.Expires != 3600 {
		t.Fatalf("new registration state = durable:%t runtime:%+v", memory.persistent.IsOnline, state)
	}
}

func TestPersistOptionsProbeActivityReturnsStateError(t *testing.T) {
	old := time.Now().Add(-time.Minute)
	memory := &probePersistenceFailureMemory{
		flowMemory: &flowMemory{
			persistent: &ipc.Device{DeviceID: gb10DeviceID, KeepaliveAt: orm.Time{Time: old}},
			runtime:    &Device{LastKeepaliveAt: old},
		},
		err: errors.New("database unavailable"),
	}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	err := api.persistOptionsProbeActivity(gb10DeviceID)
	if err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf("OPTIONS activity persistence result = %v", err)
	}
	if !memory.runtime.runtimeSnapshot().LastKeepaliveAt.Equal(old) {
		t.Fatal("failed OPTIONS activity persistence updated runtime state")
	}
}

func TestPersistOptionsProbeActivityCommitsOneOnlineObservation(t *testing.T) {
	old := time.Now().Add(-time.Minute)
	memory := &flowMemory{
		persistent: &ipc.Device{DeviceID: gb10DeviceID, KeepaliveAt: orm.Time{Time: old}},
		runtime: &Device{
			LastKeepaliveAt:                old,
			deviceStatusPersistencePending: true,
			keepalivePersistencePending:    true,
		},
	}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	if err := api.persistOptionsProbeActivity(gb10DeviceID); err != nil {
		t.Fatal(err)
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline || memory.persistent.KeepaliveAt.IsZero() ||
		!memory.persistent.KeepaliveAt.Time.Equal(state.LastKeepaliveAt) {
		t.Fatalf("OPTIONS activity mismatch: persistent=%+v runtime=%+v", memory.persistent, state)
	}
	if state.DeviceStatusPersistencePending || state.KeepalivePersistencePending || state.RegistrationClosed ||
		state.OfflinePersistencePending {
		t.Fatalf("OPTIONS activity retained stale runtime state: %+v", state)
	}
}

func TestOptionsProbeCannotOverwriteNewRegistration(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			oldRegisteredAt := time.Now().Add(-time.Minute)
			oldKeepaliveAt := oldRegisteredAt.Add(10 * time.Second)
			memory := &flowMemory{
				persistent: &ipc.Device{
					DeviceID:     gb10DeviceID,
					IsOnline:     true,
					RegisteredAt: orm.Time{Time: oldRegisteredAt},
					KeepaliveAt:  orm.Time{Time: oldKeepaliveAt},
					Expires:      3600,
				},
				runtime: &Device{},
			}
			setPersistedRegistrationClosed(memory.persistent, false)

			local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
			sipServer := sip.NewServer(local)
			localRaw, peer := net.Pipe()
			connection := sip.NewTCPConnection(localRaw)
			api := &GB28181API{}
			server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
			api.svr = server
			memory.runtime.setGBProfile(version, nil)
			memory.runtime.UpdateRuntime(func(device *Device) {
				device.IsOnline = true
				device.conn = connection
				device.source = connection.RemoteAddr()
				device.to = remote
				device.LastRegisterAt = oldRegisteredAt
				device.LastKeepaliveAt = oldKeepaliveAt
				device.Expires = 3600
				device.registrationClosed = false
			})
			go sipServer.ProcessTCPConnection(connection)
			t.Cleanup(func() {
				_ = peer.Close()
				sipServer.Close()
			})

			probeDone := make(chan error, 1)
			go func() {
				probeDone <- api.ProbeOptions(t.Context(), &OptionsProbeInput{
					DeviceID: gb10DeviceID,
					Timeout:  time.Second,
				})
			}()

			if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			request, err := readAnnexGTestSIPFrame(bufio.NewReader(peer))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(request, "OPTIONS ") {
				t.Fatalf("OPTIONS request = %s", request)
			}

			newRegisteredAt := time.Now()
			newKeepaliveAt := newRegisteredAt.Add(time.Second)
			newConnection := newFlowConnection()
			pendingDeviceStatusAt := newRegisteredAt.Add(2 * time.Second)
			pendingKeepaliveAt := newRegisteredAt.Add(3 * time.Second)
			unlockRegister := api.lockRegisterOperation(gb10DeviceID)
			memory.persistent.RegisteredAt = orm.Time{Time: newRegisteredAt}
			memory.persistent.KeepaliveAt = orm.Time{Time: newKeepaliveAt}
			memory.persistent.Expires = 7200
			memory.persistent.IsOnline = false
			setPersistedRegistrationClosed(memory.persistent, false)
			memory.runtime.UpdateRuntime(func(device *Device) {
				device.IsOnline = false
				device.conn = newConnection
				device.source = newConnection.remote
				device.Address = newConnection.remote.String()
				device.LastRegisterAt = newRegisteredAt
				device.LastKeepaliveAt = newKeepaliveAt
				device.Expires = 7200
				device.registrationClosed = false
				device.deviceStatusPersistencePending = true
				device.pendingDeviceStatusOnline = false
				device.pendingDeviceStatusAt = pendingDeviceStatusAt
				device.keepalivePersistencePending = true
				device.pendingKeepaliveOnline = false
				device.pendingKeepaliveAt = pendingKeepaliveAt
			})
			unlockRegister()

			to := cascadeTestHeader(request, "To")
			if !strings.Contains(strings.ToLower(to), ";tag=") {
				to += ";tag=options-probe"
			}
			response := "SIP/2.0 200 OK\r\n" +
				"Via: " + cascadeTestHeader(request, "Via") + "\r\n" +
				"From: " + cascadeTestHeader(request, "From") + "\r\n" +
				"To: " + to + "\r\n" +
				"Call-ID: " + cascadeTestHeader(request, "Call-ID") + "\r\n" +
				"CSeq: " + cascadeTestHeader(request, "CSeq") + "\r\n" +
				"Content-Length: 0\r\n\r\n"
			if _, err := peer.Write([]byte(response)); err != nil {
				t.Fatal(err)
			}

			select {
			case err := <-probeDone:
				if !errors.Is(err, errOptionsProbeRegistrationChanged) {
					t.Fatalf("late OPTIONS probe result = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("OPTIONS probe did not return after the SIP response")
			}

			state := memory.runtime.runtimeSnapshot()
			if memory.persistent.IsOnline || state.IsOnline ||
				!memory.persistent.RegisteredAt.Time.Equal(newRegisteredAt) ||
				!memory.persistent.KeepaliveAt.Time.Equal(newKeepaliveAt) ||
				memory.persistent.Expires != 7200 ||
				!state.LastRegisterAt.Equal(newRegisteredAt) ||
				!state.LastKeepaliveAt.Equal(newKeepaliveAt) || state.Expires != 7200 ||
				state.Conn != newConnection || state.Source != newConnection.remote ||
				!state.DeviceStatusPersistencePending ||
				!state.PendingDeviceStatusAt.Equal(pendingDeviceStatusAt) ||
				!state.KeepalivePersistencePending ||
				!state.PendingKeepaliveAt.Equal(pendingKeepaliveAt) {
				t.Fatalf("late OPTIONS probe overwrote new registration: persistent=%+v runtime=%+v", memory.persistent, state)
			}
		})
	}
}

func TestDeviceDeleteLockSerializesRegisterOperation(t *testing.T) {
	api := &GB28181API{}
	server := &Server{gb: api}
	unlockDelete := server.LockDeviceDelete(gb10DeviceID)

	registerAcquired := make(chan struct{})
	registerDone := make(chan struct{})
	go func() {
		unlockRegister := api.lockRegisterOperation(gb10DeviceID)
		close(registerAcquired)
		unlockRegister()
		close(registerDone)
	}()
	select {
	case <-registerAcquired:
		t.Fatal("REGISTER operation bypassed the device delete lock")
	case <-time.After(50 * time.Millisecond):
	}

	unlockDelete()
	select {
	case <-registerDone:
	case <-time.After(time.Second):
		t.Fatal("REGISTER operation did not resume after device deletion")
	}
}

func TestCredentialEditInvalidatesRegistrationAndPendingWork(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			registeredAt := time.Now().Add(-time.Minute)
			memory := &flowMemory{
				persistent: &ipc.Device{
					DeviceID:     gb10DeviceID,
					IsOnline:     true,
					RegisteredAt: orm.Time{Time: registeredAt},
					Expires:      3600,
				},
				runtime: &Device{
					IsOnline:       true,
					LastRegisterAt: registeredAt,
					Expires:        3600,
				},
			}
			setPersistedRegistrationClosed(memory.persistent, false)
			memory.runtime.setGBProfile(version, nil)
			api := &GB28181API{registerResults: make(map[[sha256.Size]byte]registerResultState)}
			resultKey := sha256.Sum256([]byte("credential-edit-" + string(version)))
			api.registerResults[resultKey] = registerResultState{
				DeviceID: gb10DeviceID, ExpiresAt: time.Now().Add(registerResultTTL),
			}
			server := &Server{gb: api, memoryStorer: memory}
			api.svr = server
			operation := newPendingDeviceOperation(t.Context(), gb10DeviceID)
			api.pendingDeviceRequests.Store(operation, operation)

			unlockEdit := server.LockDeviceEdit(gb10DeviceID)
			err := server.InvalidateDeviceRegistration(t.Context(), gb10DeviceID)
			unlockEdit()
			if err != nil {
				t.Fatal(err)
			}

			state := memory.runtime.runtimeSnapshot()
			if memory.persistent.IsOnline || !persistedRegistrationClosed(memory.persistent) ||
				state.IsOnline || !state.RegistrationClosed || state.Expires != 3600 ||
				!state.LastRegisterAt.Equal(registeredAt) {
				t.Fatalf("credential edit state = persistent:%+v runtime:%+v", memory.persistent, state)
			}
			if !errors.Is(operation.Cause(), ErrDeviceOffline) {
				t.Fatalf("pending operation cause = %v", operation.Cause())
			}
			if count := syncMapLen(&api.pendingDeviceRequests); count != 0 {
				t.Fatalf("credential edit retained %d pending operations", count)
			}
			if _, ok := api.registerResults[resultKey]; ok {
				t.Fatal("credential edit retained cached REGISTER success")
			}
		})
	}
}

func TestFinalizeCredentialEditDoesNotRepeatCommittedDatabaseUpdate(t *testing.T) {
	memory := &registerHandlerTestMemory{flowMemory: newFlowMemory(gb10DeviceID)}
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.Password = "old-password"
		device.registrationClosed = false
	})
	api := &GB28181API{}
	server := &Server{gb: api, memoryStorer: memory}
	api.svr = server
	closed := true
	committed := &ipc.Device{
		DeviceID: gb10DeviceID,
		Password: "new-password",
		IsOnline: false,
		Ext:      ipc.DeviceExt{GBRegistrationClosed: &closed},
	}

	if err := server.FinalizeDeviceCredentialEdit(t.Context(), committed); err != nil {
		t.Fatal(err)
	}
	if memory.changeCalls != 0 {
		t.Fatalf("committed credential edit repeated database update %d times", memory.changeCalls)
	}
	state := memory.runtime.runtimeSnapshot()
	if state.IsOnline || !state.RegistrationClosed || state.Password != "new-password" {
		t.Fatalf("finalized credential runtime = %+v", state)
	}
}

type refreshBeforeOfflineMemory struct {
	*flowMemory
	refreshedAt time.Time
	refreshed   bool
}

type renewRegistrationBeforeOfflineMemory struct {
	*flowMemory
	registeredAt time.Time
	expires      int
	renewed      bool
}

type probePersistenceFailureMemory struct {
	*flowMemory
	err error
}

type blockingOfflineCommitMemory struct {
	*flowMemory
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *probePersistenceFailureMemory) Change(_ string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	if m.err != nil {
		return m.err
	}
	if persistent != nil {
		if err := persistent(m.persistent); err != nil {
			return err
		}
	}
	if runtime != nil {
		runtime(m.runtime)
	}
	return nil
}

func (m *blockingOfflineCommitMemory) Change(_ string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	if persistent != nil {
		if err := persistent(m.persistent); err != nil {
			return err
		}
	}
	if runtime != nil {
		m.runtime.UpdateRuntime(runtime)
	}
	blocked := false
	m.once.Do(func() {
		blocked = true
		close(m.entered)
	})
	if blocked {
		<-m.release
	}
	return nil
}

func (m *blockingOfflineCommitMemory) renew(registeredAt time.Time, expires int) {
	m.persistent.IsOnline = true
	m.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	m.persistent.KeepaliveAt = orm.Time{Time: registeredAt}
	m.persistent.Expires = expires
	m.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = registeredAt
		device.LastKeepaliveAt = registeredAt
		device.Expires = expires
		device.offlinePersistencePending = false
		device.registrationClosed = false
	})
}

func (m *refreshBeforeOfflineMemory) Change(deviceID string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	if !m.refreshed {
		m.refreshed = true
		m.persistent.KeepaliveAt = orm.Time{Time: m.refreshedAt}
		m.runtime.UpdateRuntime(func(device *Device) {
			device.LastKeepaliveAt = m.refreshedAt
			device.conn = newFlowConnection()
		})
	}
	return m.flowMemory.Change(deviceID, persistent, runtime)
}

func (m *renewRegistrationBeforeOfflineMemory) Change(deviceID string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	if !m.renewed {
		m.renewed = true
		m.persistent.RegisteredAt = orm.Time{Time: m.registeredAt}
		m.persistent.Expires = m.expires
		m.runtime.UpdateRuntime(func(device *Device) {
			device.LastRegisterAt = m.registeredAt
			device.Expires = m.expires
		})
	}
	return m.flowMemory.Change(deviceID, persistent, runtime)
}

var _ MemoryStorer = (*refreshBeforeOfflineMemory)(nil)
var _ MemoryStorer = (*renewRegistrationBeforeOfflineMemory)(nil)
var _ MemoryStorer = (*blockingOfflineCommitMemory)(nil)

func TestRuntimeStateCleanerRunsWithoutNewRequestsAndStops(t *testing.T) {
	now := time.Now()
	manager := NewDirectTCPDownloadManager(DirectTCPDownloadOptions{StorageDir: t.TempDir(), RetainDays: 1})
	manager.states["expired"] = DirectTCPDownloadState{
		SessionID: "expired", Status: directTCPStatusCompleted,
		StartedAt: now.Add(-48 * time.Hour), CompletedAt: now.Add(-25 * time.Hour),
	}
	api := &GB28181API{lifecycleDone: make(chan struct{}), directDownloads: manager}
	api.rtpDownloads.Store("expired", testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now.Add(-rtpDownloadTerminalTTL-time.Second)))
	api.queryStates.Store("expired", &QueryState{UpdatedAt: now.Add(-queryStateTTL - time.Second)})
	api.storeUpgradeState(UpgradeState{DeviceID: gb10DeviceID, SessionID: "upgrade-expired-0000000000000001", UpdatedAt: now.Add(-upgradeStateTTL - time.Second)})
	api.storeSnapshotState(SnapshotState{DeviceID: gb10DeviceID, SessionID: "snapshot-expired-000000000000001", UpdatedAt: now.Add(-snapshotStateTTL - time.Second)})
	done := make(chan struct{})
	go func() {
		api.startRuntimeStateCleaner()
		close(done)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, directExists := manager.State("expired")
		_, rtpExists := api.rtpDownloads.Load("expired")
		_, queryExists := api.queryStates.Load("expired")
		api.upgradeStateMu.RLock()
		_, upgradeExists := api.upgradeStates[upgradeStateKey(gb10DeviceID, "upgrade-expired-0000000000000001")]
		api.upgradeStateMu.RUnlock()
		api.snapshotStateMu.RLock()
		_, snapshotExists := api.snapshotStates[snapshotStateKey(gb10DeviceID, "snapshot-expired-000000000000001")]
		api.snapshotStateMu.RUnlock()
		if !directExists && !rtpExists && !queryExists && !upgradeExists && !snapshotExists {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := manager.State("expired"); ok {
		t.Fatal("background cleaner did not remove Direct TCP state")
	}
	if _, ok := api.rtpDownloads.Load("expired"); ok {
		t.Fatal("background cleaner did not remove RTP state")
	}
	if _, ok := api.queryStates.Load("expired"); ok {
		t.Fatal("background cleaner did not remove query state")
	}
	api.upgradeStateMu.RLock()
	_, upgradeExists := api.upgradeStates[upgradeStateKey(gb10DeviceID, "upgrade-expired-0000000000000001")]
	api.upgradeStateMu.RUnlock()
	if upgradeExists {
		t.Fatal("background cleaner did not remove upgrade state")
	}
	api.snapshotStateMu.RLock()
	_, snapshotExists := api.snapshotStates[snapshotStateKey(gb10DeviceID, "snapshot-expired-000000000000001")]
	api.snapshotStateMu.RUnlock()
	if snapshotExists {
		t.Fatal("background cleaner did not remove snapshot state")
	}
	api.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("download state cleaner did not stop with GB28181 lifecycle")
	}
}

func TestCloseCleansOrdinaryMediaSessionsOnce(t *testing.T) {
	media := &fakeRTPMediaService{}
	server := &sms.MediaServer{}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{sms: media, streams: streams, lifecycleDone: make(chan struct{})}

	live := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "live-close", mediaServer: server}
	streams.Store(resolvePlaySessionKey(live.DeviceID, live.ChannelID, ""), live)

	talkStream := &Streams{DeviceID: gb10DeviceID, ChannelID: "34020000001320000002", StreamID: "talk-close", mediaServer: server}
	talk := &talkSession{
		DeviceID: talkStream.DeviceID, ChannelID: talkStream.ChannelID, ReceiveStream: talkStream.StreamID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: server, SSRC: "talk-ssrc", Stream: talkStream, receiverOpened: true, rtpStarted: true,
		ready: make(chan error, 1),
	}
	api.talkSessions.Store(talk.ReceiveStream, talk)
	streams.Store(voiceKey(voiceModeTalk, talk.DeviceID, talk.ChannelID), talkStream)

	broadcastStream := &Streams{DeviceID: gb10DeviceID, ChannelID: "34020000001320000003", StreamID: "broadcast-close"}
	broadcast := &broadcastSession{
		DeviceID: broadcastStream.DeviceID, ChannelID: broadcastStream.ChannelID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: server, SSRC: "broadcast-ssrc", Stream: broadcastStream, rtpStarted: true,
		ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(broadcast.ChannelID, broadcast)
	streams.Store(voiceKey(voiceModeBroadcast, broadcast.DeviceID, broadcast.ChannelID), broadcastStream)

	api.close()
	api.close()

	media.mu.Lock()
	closeCalls := media.closeCalls
	stopCalls := media.stopCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("RTP receiver close calls = %d; want 2", closeCalls)
	}
	if stopCalls != 2 {
		t.Fatalf("RTP sender stop calls = %d; want 2", stopCalls)
	}
	if streams.Len() != 0 {
		t.Fatalf("media streams survived close: %d", streams.Len())
	}
	if syncMapLen(&api.talkSessions) != 0 || syncMapLen(&api.broadcastSessions) != 0 {
		t.Fatal("voice sessions survived close")
	}
}

func TestCleanupDeviceOnlyRemovesMatchingRuntimeState(t *testing.T) {
	media := &fakeRTPMediaService{}
	server := &sms.MediaServer{}
	streams := &conc.Map[string, *Streams]{}
	api := &GB28181API{
		sms: media, streams: streams,
		cascadeSources:       make(map[string]*cascadeSourceRef),
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
		registerNonces:       make(map[string]registerNonceState),
		messageNonces:        make(map[string]messageNonceState),
		registerResults:      make(map[[sha256.Size]byte]registerResultState),
		upgradeStates:        make(map[string]UpgradeState),
		snapshotStates:       make(map[string]SnapshotState),
	}
	otherDeviceID := "34020000001320000009"

	live := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "delete-live", mediaServer: server}
	streams.Store(resolvePlaySessionKey(live.DeviceID, live.ChannelID, ""), live)
	otherLive := &Streams{DeviceID: otherDeviceID, ChannelID: "34020000001320000010", StreamID: "keep-live", mediaServer: server}
	streams.Store(resolvePlaySessionKey(otherLive.DeviceID, otherLive.ChannelID, ""), otherLive)

	talkStream := &Streams{DeviceID: gb10DeviceID, ChannelID: "34020000001320000002", StreamID: "delete-talk", mediaServer: server}
	talk := &talkSession{
		DeviceID: talkStream.DeviceID, ChannelID: talkStream.ChannelID, ReceiveStream: talkStream.StreamID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: server, SSRC: "delete-talk-ssrc", Stream: talkStream, receiverOpened: true, rtpStarted: true,
		ready: make(chan error, 1),
	}
	api.talkSessions.Store(talk.ReceiveStream, talk)
	streams.Store(voiceKey(voiceModeTalk, talk.DeviceID, talk.ChannelID), talkStream)

	broadcastStream := &Streams{DeviceID: gb10DeviceID, ChannelID: "34020000001320000003", StreamID: "delete-broadcast"}
	broadcast := &broadcastSession{
		DeviceID: broadcastStream.DeviceID, ChannelID: broadcastStream.ChannelID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: server, SSRC: "delete-broadcast-ssrc", Stream: broadcastStream, rtpStarted: true,
		ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(broadcast.ChannelID, broadcast)
	streams.Store(voiceKey(voiceModeBroadcast, broadcast.DeviceID, broadcast.ChannelID), broadcastStream)

	source := &cascadeSourceRef{key: "delete-source", refs: 1, stream: live}
	cascade := &cascadeMediaSession{
		source: source, server: server, ssrc: "delete-cascade-ssrc",
		vhost: defaultBroadcastVHost, app: defaultBroadcastApp, stream: "cascade-source",
	}
	api.cascadeSources[source.key] = source
	api.inviteDialogs.Store("delete-cascade", &inboundInviteDialog{CallID: "delete-cascade", Cascade: cascade})

	now := time.Now()
	api.queryStates.Store(gb10DeviceID, &QueryState{UpdatedAt: now})
	api.queryStates.Store(otherDeviceID, &QueryState{UpdatedAt: now})
	api.rtpDownloads.Store("delete-download", testRTPDownloadSession(gb10DeviceID, gb10ChannelID, now))
	api.rtpDownloads.Store("keep-download", testRTPDownloadSession(otherDeviceID, otherLive.ChannelID, now))
	api.storeUpgradeState(UpgradeState{DeviceID: gb10DeviceID, SessionID: "delete-upgrade", UpdatedAt: now})
	api.storeUpgradeState(UpgradeState{DeviceID: otherDeviceID, SessionID: "keep-upgrade", UpdatedAt: now})
	api.storeSnapshotState(SnapshotState{DeviceID: gb10DeviceID, SessionID: "delete-snapshot", UpdatedAt: now})
	api.storeSnapshotState(SnapshotState{DeviceID: otherDeviceID, SessionID: "keep-snapshot", UpdatedAt: now})
	api.outgoingSubscriptions.Store(gb10DeviceID+"|target|Catalog", &outgoingSubscriptionDialog{})
	api.outgoingSubscriptions.Store(otherDeviceID+"|target|Catalog", &outgoingSubscriptionDialog{})
	deletedInboundSubscription := &eventSubscription{Key: "delete-inbound-subscription", OwnerDeviceID: gb10DeviceID, DeviceID: otherDeviceID}
	keptInboundSubscription := &eventSubscription{Key: "keep-inbound-subscription", OwnerDeviceID: otherDeviceID, DeviceID: gb10DeviceID}
	api.eventSubscribers.Store(deletedInboundSubscription.Key, deletedInboundSubscription)
	api.eventSubscribers.Store(keptInboundSubscription.Key, keptInboundSubscription)
	api.cascadeSubscriptions[gb10DeviceID+"|target|Catalog"] = &cascadeDownstreamSubscription{Input: SubscribeInput{DeviceID: gb10DeviceID}}
	api.cascadeSubscriptions[otherDeviceID+"|target|Catalog"] = &cascadeDownstreamSubscription{Input: SubscribeInput{DeviceID: otherDeviceID}}
	api.registerNonces["delete-register-nonce"] = registerNonceState{DeviceID: gb10DeviceID}
	api.registerNonces["keep-register-nonce"] = registerNonceState{DeviceID: otherDeviceID}
	api.messageNonces["delete-message-nonce"] = messageNonceState{DeviceID: gb10DeviceID}
	api.messageNonces["keep-message-nonce"] = messageNonceState{DeviceID: otherDeviceID}
	deleteRegisterResultKey := [sha256.Size]byte{1}
	keepRegisterResultKey := [sha256.Size]byte{2}
	api.registerResults[deleteRegisterResultKey] = registerResultState{DeviceID: gb10DeviceID}
	api.registerResults[keepRegisterResultKey] = registerResultState{DeviceID: otherDeviceID}

	if err := api.cleanupDevice(context.Background(), gb10DeviceID); err != nil {
		t.Fatal(err)
	}

	if streams.Len() != 1 {
		t.Fatalf("remaining streams = %d; want 1", streams.Len())
	}
	if current, ok := streams.Load(resolvePlaySessionKey(otherLive.DeviceID, otherLive.ChannelID, "")); !ok || current != otherLive {
		t.Fatal("other device stream was removed")
	}
	if syncMapLen(&api.talkSessions) != 0 || syncMapLen(&api.broadcastSessions) != 0 {
		t.Fatal("deleted device voice sessions survived cleanup")
	}
	if _, ok := api.inviteDialogs.Load("delete-cascade"); ok {
		t.Fatal("deleted device cascade session survived cleanup")
	}
	api.cascadeMediaMu.Lock()
	_, sourceExists := api.cascadeSources[source.key]
	api.cascadeMediaMu.Unlock()
	if sourceExists {
		t.Fatal("deleted device cascade source survived cleanup")
	}
	if _, ok := api.queryStates.Load(gb10DeviceID); ok {
		t.Fatal("deleted device query state survived cleanup")
	}
	if _, ok := api.queryStates.Load(otherDeviceID); !ok {
		t.Fatal("other device query state was removed")
	}
	if _, ok := api.rtpDownloads.Load("delete-download"); ok {
		t.Fatal("deleted device RTP download state survived cleanup")
	}
	if _, ok := api.rtpDownloads.Load("keep-download"); !ok {
		t.Fatal("other device RTP download state was removed")
	}
	if _, ok := api.UpgradeState(gb10DeviceID, "delete-upgrade"); ok {
		t.Fatal("deleted device upgrade state survived cleanup")
	}
	if _, ok := api.UpgradeState(otherDeviceID, "keep-upgrade"); !ok {
		t.Fatal("other device upgrade state was removed")
	}
	if _, ok := api.SnapshotState(gb10DeviceID, "delete-snapshot"); ok {
		t.Fatal("deleted device snapshot state survived cleanup")
	}
	if _, ok := api.SnapshotState(otherDeviceID, "keep-snapshot"); !ok {
		t.Fatal("other device snapshot state was removed")
	}
	if _, ok := api.outgoingSubscriptions.Load(gb10DeviceID + "|target|Catalog"); ok {
		t.Fatal("deleted device outgoing subscription survived cleanup")
	}
	if _, ok := api.outgoingSubscriptions.Load(otherDeviceID + "|target|Catalog"); !ok {
		t.Fatal("other device outgoing subscription was removed")
	}
	if _, ok := api.eventSubscribers.Load(deletedInboundSubscription.Key); ok {
		t.Fatal("deleted device inbound event subscription survived cleanup")
	}
	if _, ok := api.eventSubscribers.Load(keptInboundSubscription.Key); !ok {
		t.Fatal("other device inbound event subscription was removed")
	}
	if _, ok := api.cascadeSubscriptions[gb10DeviceID+"|target|Catalog"]; ok {
		t.Fatal("deleted device cascade subscription survived cleanup")
	}
	if _, ok := api.cascadeSubscriptions[otherDeviceID+"|target|Catalog"]; !ok {
		t.Fatal("other device cascade subscription was removed")
	}
	if _, ok := api.registerNonces["delete-register-nonce"]; ok {
		t.Fatal("deleted device REGISTER nonce survived cleanup")
	}
	if _, ok := api.registerNonces["keep-register-nonce"]; !ok {
		t.Fatal("other device REGISTER nonce was removed")
	}
	if _, ok := api.messageNonces["delete-message-nonce"]; ok {
		t.Fatal("deleted device MESSAGE nonce survived cleanup")
	}
	if _, ok := api.messageNonces["keep-message-nonce"]; !ok {
		t.Fatal("other device MESSAGE nonce was removed")
	}
	if _, ok := api.registerResults[deleteRegisterResultKey]; ok {
		t.Fatal("deleted device REGISTER result survived cleanup")
	}
	if _, ok := api.registerResults[keepRegisterResultKey]; !ok {
		t.Fatal("other device REGISTER result was removed")
	}
	media.mu.Lock()
	closeCalls, stopCalls := media.closeCalls, media.stopCalls
	media.mu.Unlock()
	if closeCalls != 2 || stopCalls != 3 {
		t.Fatalf("media cleanup calls = close:%d stop:%d; want close:2 stop:3", closeCalls, stopCalls)
	}
}

func TestCleanupDeviceFinishesInboundSubscriptionRemovalAfterCallerCancellation(t *testing.T) {
	api := &GB28181API{}
	subscription := &eventSubscription{
		Key:           "delete-inbound-subscription-after-cancel",
		OwnerDeviceID: gb10DeviceID,
		DeviceID:      gb10ChannelID,
		ExpiresAt:     time.Now().Add(time.Minute),
	}
	api.eventSubscribers.Store(subscription.Key, subscription)

	unlockSubscription, err := api.lockEventSubscriptionOperation(t.Context(), subscription.Key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- api.cleanupDevice(ctx, gb10DeviceID)
	}()

	deadline := time.Now().Add(time.Second)
	for {
		api.eventSubscriptionMu.Lock()
		entry := api.eventSubscriptionOps[subscription.Key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		api.eventSubscriptionMu.Unlock()
		if refs >= 2 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			unlockSubscription()
			t.Fatal("device cleanup did not wait for the inbound subscription operation lock")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-cleanupDone:
		unlockSubscription()
		t.Fatalf("device cleanup returned before the owned subscription could be removed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unlockSubscription()
	select {
	case err := <-cleanupDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("device cleanup did not finish after the subscription lock was released")
	}
	if _, exists := api.eventSubscribers.Load(subscription.Key); exists {
		t.Fatal("caller cancellation left an owned inbound subscription behind")
	}
}

func TestCleanupDeviceSendsOutgoingSubscriptionCancellation(t *testing.T) {
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = baseConnection.remote
		device.to = remote
	})
	sipServer := sip.NewServer(local)
	api := &GB28181API{}
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	t.Cleanup(server.Close)

	callID := sip.CallID("cleanup-subscription-dialog")
	initial := sip.NewRequest("", sip.MethodSubscribe, remote.URI.Clone(), sip.DefaultSipVersion,
		sip.NewHeaderBuilder().
			SetFrom(local).
			SetTo(remote).
			SetContact(local).
			SetCallID(&callID).
			SetMethod(sip.MethodSubscribe).
			SetSeqNo(41).
			AddVia(&sip.ViaHop{
				Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP",
				Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
			}).Build(), nil)
	initial.SetConnection(connection)
	initial.SetSource(baseConnection.local)
	initial.SetDestination(baseConnection.remote)
	response := sip.NewResponseFromRequest("", initial, 200, "OK", nil)
	to, ok := response.To()
	if !ok || to == nil {
		t.Fatal("subscription response missing To")
	}
	if to.Params == nil {
		to.Params = sip.NewParams()
	}
	to.Params.Add("tag", sip.String{Str: "cleanup-remote-tag"})
	response.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
	response.SetConnection(connection)
	response.SetSource(baseConnection.remote)
	response.SetDestination(baseConnection.local)

	requestBody := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	deletedKey := gb10DeviceID + "|" + gb10DeviceID + "|CATALOG"
	api.outgoingSubscriptions.Store(deletedKey, &outgoingSubscriptionDialog{
		response: response, requestBody: requestBody, eventValue: "Catalog;id=" + gb10DeviceID,
		deviceID: gb10DeviceID, targetID: gb10DeviceID, expiresAt: time.Now().Add(time.Hour),
	})
	otherKey := "34020000001320000009|34020000001320000009|CATALOG"
	api.outgoingSubscriptions.Store(otherKey, &outgoingSubscriptionDialog{})

	startedAt := time.Now()
	if err := api.cleanupDevice(context.Background(), gb10DeviceID); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("cleanup waited for subscription response: %v", elapsed)
	}

	var cancellation string
	select {
	case payload := <-baseConnection.writes:
		cancellation = string(payload)
	case <-time.After(time.Second):
		t.Fatal("subscription cancellation was not sent")
	}
	for _, expected := range []string{
		"SUBSCRIBE sip:" + gb10DeviceID + "@192.0.2.10:5060 SIP/2.0",
		"Call-ID: cleanup-subscription-dialog",
		"CSeq: 42 SUBSCRIBE",
		"To: <sip:" + gb10DeviceID + "@192.0.2.10:5060>;tag=cleanup-remote-tag",
		"Event: Catalog;id=" + gb10DeviceID,
		"Expires: 0",
		string(requestBody),
	} {
		if !strings.Contains(cancellation, expected) {
			t.Fatalf("subscription cancellation missing %q:\n%s", expected, cancellation)
		}
	}
	if _, exists := api.outgoingSubscriptions.Load(deletedKey); exists {
		t.Fatal("deleted device outgoing subscription survived cleanup")
	}
	if _, exists := api.outgoingSubscriptions.Load(otherKey); !exists {
		t.Fatal("other device outgoing subscription was removed")
	}
}

func TestOutgoingSubscriptionCancellationLocalFailureDoesNotConsumeDialogCSeq(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = nil
		device.source = &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060}
		device.to = remote
	})
	sipServer := sip.NewServer(local)
	api := &GB28181API{}
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	t.Cleanup(server.Close)

	callID := sip.CallID("cleanup-subscription-local-failure")
	initial := sip.NewRequest("", sip.MethodSubscribe, remote.URI.Clone(), sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetContact(local).SetCallID(&callID).
			SetMethod(sip.MethodSubscribe).SetSeqNo(41).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "UDP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	initial.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	initial.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060})
	response := sip.NewResponseFromRequest("", initial, 200, "OK", nil)
	response.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
	dialog := &outgoingSubscriptionDialog{
		response:    response,
		requestBody: []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`),
		eventValue:  "presence", deviceID: gb10DeviceID, targetID: gb10DeviceID,
	}

	if _, _, err := api.sendOutgoingSubscriptionCancellationContext(t.Context(), dialog, false); err == nil || !strings.Contains(err.Error(), "connection") {
		t.Fatalf("subscription cancellation local error = %v", err)
	}
	next, err := sip.NewRequestFromResponseChecked(sip.MethodSubscribe, response)
	if err != nil {
		t.Fatal(err)
	}
	cseq, _ := next.CSeq()
	if cseq == nil || cseq.SeqNo != 42 {
		t.Fatalf("subscription cancellation local failure consumed CSeq: %+v", cseq)
	}
}

func TestCleanupDeviceCancelsOnlyMatchingPendingOperations(t *testing.T) {
	otherDeviceID := "34020000001320000009"
	api := &GB28181API{
		catalogResponses:     newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:      newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
		registerNonces:       make(map[string]registerNonceState),
		messageNonces:        make(map[string]messageNonceState),
		registerResults:      make(map[[sha256.Size]byte]registerResultState),
		upgradeStates:        make(map[string]UpgradeState),
		snapshotStates:       make(map[string]SnapshotState),
	}

	deletedOperations := []*pendingDeviceOperation{
		newPendingDeviceOperation(context.Background(), gb10DeviceID),
		newPendingDeviceOperation(context.Background(), gb10DeviceID),
		newPendingDeviceOperation(context.Background(), gb10DeviceID),
		newPendingDeviceOperation(context.Background(), gb10DeviceID),
		newPendingDeviceOperation(context.Background(), gb10DeviceID),
		newPendingDeviceOperation(context.Background(), gb10DeviceID),
	}
	keptOperations := []*pendingDeviceOperation{
		newPendingDeviceOperation(context.Background(), otherDeviceID),
		newPendingDeviceOperation(context.Background(), otherDeviceID),
		newPendingDeviceOperation(context.Background(), otherDeviceID),
		newPendingDeviceOperation(context.Background(), otherDeviceID),
		newPendingDeviceOperation(context.Background(), otherDeviceID),
		newPendingDeviceOperation(context.Background(), otherDeviceID),
	}
	defer func() {
		for _, operation := range keptOperations {
			operation.Cancel(nil)
		}
	}()

	api.pendingDeviceControl.Store("delete-control", &pendingDeviceControl{operation: deletedOperations[0]})
	api.pendingDeviceQuery.Store("delete-query", &pendingQueryWait{operation: deletedOperations[1]})
	api.pendingDeviceConfig.Store("delete-config", &pendingDeviceConfig{operation: deletedOperations[2]})
	api.pendingBroadcast.Store("delete-broadcast", &pendingBroadcastResponse{operation: deletedOperations[3]})
	api.pendingMultiResponse.Store("delete-multi-response", deletedOperations[4])
	api.pendingDeviceRequests.Store("delete-request", deletedOperations[5])
	api.pendingDeviceControl.Store("keep-control", &pendingDeviceControl{operation: keptOperations[0]})
	api.pendingDeviceQuery.Store("keep-query", &pendingQueryWait{operation: keptOperations[1]})
	api.pendingDeviceConfig.Store("keep-config", &pendingDeviceConfig{operation: keptOperations[2]})
	api.pendingBroadcast.Store("keep-broadcast", &pendingBroadcastResponse{operation: keptOperations[3]})
	api.pendingMultiResponse.Store("keep-multi-response", keptOperations[4])
	api.pendingDeviceRequests.Store("keep-request", keptOperations[5])
	api.cascadeMobilePositionQueries.Store("delete-mobile", &cascadeMobilePositionQueryRoute{sourceDeviceID: gb10DeviceID})
	api.cascadeMobilePositionQueries.Store("keep-mobile", &cascadeMobilePositionQueryRoute{sourceDeviceID: otherDeviceID})
	systemRoute := &cascadeMobilePositionQueryRoute{
		system: true,
		pending: map[string]string{
			gb10ChannelID:          gb10DeviceID,
			"34020000001320000010": otherDeviceID,
		},
		sources: map[string]string{
			"34020000001320000011": gb10DeviceID,
			"34020000001320000012": otherDeviceID,
		},
		positions: map[string]mobilePositionItemXML{
			"delete-position": {},
			"keep-position":   {},
		},
		positionSources: map[string]string{
			"delete-position": "34020000001320000011",
			"keep-position":   "34020000001320000012",
		},
	}
	api.cascadeMobilePositionQueries.Store("system-mobile", systemRoute)

	deletedCatalogKey := buildMultiResponseKey(gb10DeviceID, "Catalog", 21)
	keptCatalogKey := buildMultiResponseKey(otherDeviceID, "Catalog", 22)
	api.catalogResponses.Start(deletedCatalogKey)
	api.catalogResponses.Start(keptCatalogKey)
	deletedRecordKey := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 31)
	keptRecordKey := buildMultiResponseKey("34020000001320000010", "RecordInfo", 32)
	api.recordResponses.Start(deletedRecordKey)
	api.recordResponses.Start(keptRecordKey)
	deletedAliasKey := buildMultiResponseKey(gb10DeviceID, "RecordInfo", 31)
	keptAliasKey := buildMultiResponseKey(otherDeviceID, "RecordInfo", 32)
	api.recordResponseAliases.Store(deletedAliasKey, deletedRecordKey)
	api.recordResponseAliases.Store(keptAliasKey, keptRecordKey)
	api.startRecordResponseExtra(deletedRecordKey)

	catalogDone := make(chan multiResponseResult[Channels], 1)
	recordDone := make(chan multiResponseResult[RecordItem], 1)
	go func() { catalogDone <- api.catalogResponses.Wait(context.Background(), deletedCatalogKey) }()
	go func() { recordDone <- api.recordResponses.Wait(context.Background(), deletedRecordKey) }()

	if err := api.cleanupDevice(context.Background(), gb10DeviceID); err != nil {
		t.Fatal(err)
	}

	for index, operation := range deletedOperations {
		select {
		case <-operation.Done():
			if !errors.Is(operation.Cause(), ErrDeviceNotExist) {
				t.Errorf("deleted operation %d cause = %v", index, operation.Cause())
			}
		default:
			t.Errorf("deleted operation %d was not cancelled", index)
		}
		if operation.Deliver(func() {}) {
			t.Errorf("deleted operation %d accepted a late response", index)
		}
	}
	for index, operation := range keptOperations {
		select {
		case <-operation.Done():
			t.Errorf("other device operation %d was cancelled: %v", index, operation.Cause())
		default:
		}
	}

	for name, values := range map[string]*sync.Map{
		"device control": &api.pendingDeviceControl,
		"device query":   &api.pendingDeviceQuery,
		"device config":  &api.pendingDeviceConfig,
		"broadcast":      &api.pendingBroadcast,
		"multi response": &api.pendingMultiResponse,
		"device request": &api.pendingDeviceRequests,
	} {
		if count := syncMapLen(values); count != 1 {
			t.Errorf("%s pending entries = %d; want only the other device entry", name, count)
		}
	}
	if _, ok := api.cascadeMobilePositionQueries.Load("delete-mobile"); ok {
		t.Fatal("deleted device MobilePosition route survived cleanup")
	}
	if _, ok := api.cascadeMobilePositionQueries.Load("keep-mobile"); !ok {
		t.Fatal("other device MobilePosition route was removed")
	}
	if _, ok := api.cascadeMobilePositionQueries.Load("system-mobile"); !ok {
		t.Fatal("mixed system MobilePosition route was removed")
	}
	systemRoute.mu.Lock()
	_, deletedPending := systemRoute.pending[gb10ChannelID]
	_, keptPending := systemRoute.pending["34020000001320000010"]
	_, deletedSource := systemRoute.sources["34020000001320000011"]
	_, keptSource := systemRoute.sources["34020000001320000012"]
	_, deletedPosition := systemRoute.positions["delete-position"]
	_, keptPosition := systemRoute.positions["keep-position"]
	systemRoute.mu.Unlock()
	if deletedPending || deletedSource || deletedPosition || !keptPending || !keptSource || !keptPosition {
		t.Fatalf("system MobilePosition cleanup = pending:%v/%v sources:%v/%v positions:%v/%v",
			deletedPending, keptPending, deletedSource, keptSource, deletedPosition, keptPosition)
	}

	select {
	case result := <-catalogDone:
		if !errors.Is(result.Err, ErrDeviceNotExist) {
			t.Errorf("Catalog cancellation = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("Catalog waiter did not stop after device cleanup")
	}
	select {
	case result := <-recordDone:
		if !errors.Is(result.Err, ErrDeviceNotExist) {
			t.Errorf("RecordInfo cancellation = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("RecordInfo waiter did not stop after device cleanup")
	}
	if api.catalogResponses.Add(deletedCatalogKey, 1, []Channels{{ChannelID: gb10ChannelID}}) {
		t.Fatal("deleted device Catalog accepted a late response")
	}
	if api.recordResponses.Add(deletedRecordKey, 1, []RecordItem{{FilePath: "/late.ps"}}) {
		t.Fatal("deleted device RecordInfo accepted a late response")
	}
	if !api.catalogResponses.Has(keptCatalogKey) || !api.recordResponses.Has(keptRecordKey) {
		t.Fatal("other device multi-response wait was removed")
	}
	if _, ok := api.recordResponseAliases.Load(deletedAliasKey); ok {
		t.Fatal("deleted device RecordInfo alias survived cleanup")
	}
	if _, ok := api.recordResponseAliases.Load(keptAliasKey); !ok {
		t.Fatal("other device RecordInfo alias was removed")
	}
	api.catalogResponses.Cancel(keptCatalogKey)
	api.recordResponses.Cancel(keptRecordKey)
}

func TestCleanupDeviceCancelsCatalogWhileWaitingSIPResponse(t *testing.T) {
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.conn = connection
	memory.runtime.source = baseConnection.remote
	memory.runtime.to = remote
	sipServer := sip.NewServer(local)
	defer sipServer.Close()
	api := &GB28181API{
		catalogResponses:     newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:      newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
		registerNonces:       make(map[string]registerNonceState),
		messageNonces:        make(map[string]messageNonceState),
		registerResults:      make(map[[sha256.Size]byte]registerResultState),
		upgradeStates:        make(map[string]UpgradeState),
		snapshotStates:       make(map[string]SnapshotState),
	}
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}

	queryDone := make(chan error, 1)
	go func() {
		queryDone <- api.QueryCatalogContext(context.Background(), gb10DeviceID)
	}()
	select {
	case <-baseConnection.writes:
	case err := <-queryDone:
		t.Fatalf("Catalog request failed before send: %v", err)
	case <-time.After(time.Second):
		t.Fatal("Catalog request was not sent")
	}
	if err := api.cleanupDevice(context.Background(), gb10DeviceID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-queryDone:
		if !errors.Is(err, ErrDeviceNotExist) {
			t.Fatalf("Catalog cleanup error = %v; want %v", err, ErrDeviceNotExist)
		}
	case <-time.After(time.Second):
		t.Fatal("Catalog SIP response wait did not stop after device cleanup")
	}
	if count := syncMapLen(&api.pendingMultiResponse); count != 0 {
		t.Fatalf("Catalog pending SIP operations survived cleanup: %d", count)
	}
}

func TestCleanupDeviceCancelsMobilePositionWhileWaitingSIPResponse(t *testing.T) {
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.conn = connection
	memory.runtime.source = baseConnection.remote
	memory.runtime.to = remote
	memory.runtime.setGBProfile(GBVersion20, nil)
	sipServer := sip.NewServer(local)
	defer sipServer.Close()
	api := &GB28181API{
		catalogResponses:     newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:      newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
		registerNonces:       make(map[string]registerNonceState),
		messageNonces:        make(map[string]messageNonceState),
		registerResults:      make(map[[sha256.Size]byte]registerResultState),
		upgradeStates:        make(map[string]UpgradeState),
		snapshotStates:       make(map[string]SnapshotState),
	}
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}

	queryDone := make(chan error, 1)
	go func() {
		_, err := api.DeviceQuery(context.Background(), &DeviceQueryInput{
			DeviceID: gb10DeviceID,
			Action:   deviceQueryActionMobilePosition,
			Interval: 5,
		})
		queryDone <- err
	}()
	select {
	case <-baseConnection.writes:
	case err := <-queryDone:
		t.Fatalf("MobilePosition request failed before send: %v", err)
	case <-time.After(time.Second):
		t.Fatal("MobilePosition request was not sent")
	}
	if err := api.cleanupDevice(context.Background(), gb10DeviceID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-queryDone:
		if !errors.Is(err, ErrDeviceNotExist) {
			t.Fatalf("MobilePosition cleanup error = %v; want %v", err, ErrDeviceNotExist)
		}
	case <-time.After(time.Second):
		t.Fatal("MobilePosition SIP response wait did not stop after device cleanup")
	}
	if count := syncMapLen(&api.pendingDeviceRequests); count != 0 {
		t.Fatalf("MobilePosition pending SIP operations survived cleanup: %d", count)
	}
}

func TestCloseCancelsPendingWorkAndClearsRuntimeIndexes(t *testing.T) {
	api := &GB28181API{
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:  newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
		lifecycleDone:    make(chan struct{}),
	}
	api.catalogResponses.Start("device:Catalog:1")
	waitDone := make(chan struct{})
	go func() {
		api.catalogResponses.Wait(context.Background(), "device:Catalog:1")
		close(waitDone)
	}()

	pendingOperations := map[string]*pendingDeviceOperation{
		"device control": newPendingDeviceOperation(context.Background(), gb10DeviceID),
		"device query":   newPendingDeviceOperation(context.Background(), gb10DeviceID),
		"multi response": newPendingDeviceOperation(context.Background(), gb10DeviceID),
		"device request": newPendingDeviceOperation(context.Background(), gb10DeviceID),
		"device config":  newPendingDeviceOperation(context.Background(), gb10DeviceID),
		"broadcast":      newPendingDeviceOperation(context.Background(), gb10DeviceID),
	}
	api.pendingDeviceControl.Store("control", &pendingDeviceControl{operation: pendingOperations["device control"]})
	api.pendingDeviceQuery.Store("query", &pendingQueryWait{operation: pendingOperations["device query"]})
	api.pendingMultiResponse.Store("multi-response", pendingOperations["multi response"])
	pendingRequest := pendingOperations["device request"]
	api.pendingDeviceRequests.Store("device-request", pendingRequest)
	api.cascadeMobilePositionQueries.Store("mobile-position", &cascadeMobilePositionQueryRoute{})
	api.pendingDeviceConfig.Store("config", &pendingDeviceConfig{operation: pendingOperations["device config"]})
	api.pendingBroadcast.Store("broadcast", &pendingBroadcastResponse{operation: pendingOperations["broadcast"]})
	api.recordResponseAliases.Store("alias", "record")
	api.startRecordResponseExtra("record")
	api.appendRecordResponseMetadata("record", []byte("<Response/>"), []AppendixA4Object{{
		Type: "doorType", RawXML: `<ExtraInfo>{"type":"doorType"}</ExtraInfo>`,
	}})
	api.eventSubscribers.Store("event", &eventSubscription{})
	api.outgoingSubscriptions.Store("outgoing", &outgoingSubscriptionDialog{})

	api.close()
	api.close()
	for name, operation := range pendingOperations {
		if !errors.Is(operation.Cause(), ErrServiceStopped) {
			t.Errorf("pending %s close cause = %v; want %v", name, operation.Cause(), ErrServiceStopped)
		}
	}

	select {
	case <-api.serviceDone():
	default:
		t.Fatal("GB28181 lifecycle was not closed")
	}
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("catalog waiter did not stop with GB28181 lifecycle")
	}
	for name, values := range map[string]*sync.Map{
		"device control":   &api.pendingDeviceControl,
		"device query":     &api.pendingDeviceQuery,
		"multi response":   &api.pendingMultiResponse,
		"device request":   &api.pendingDeviceRequests,
		"mobile position":  &api.cascadeMobilePositionQueries,
		"device config":    &api.pendingDeviceConfig,
		"broadcast":        &api.pendingBroadcast,
		"record alias":     &api.recordResponseAliases,
		"event subscriber": &api.eventSubscribers,
		"outgoing dialog":  &api.outgoingSubscriptions,
	} {
		if count := syncMapLen(values); count != 0 {
			t.Errorf("%s entries survived close: %d", name, count)
		}
	}
	api.recordResponseExtraMu.Lock()
	extraCount := len(api.recordResponseExtra)
	xmlCount := len(api.recordResponseXML)
	appendixA4Count := len(api.recordResponseAppendixA4)
	metadataCount := len(api.recordResponseMetadata)
	api.recordResponseExtraMu.Unlock()
	if extraCount != 0 {
		t.Errorf("record response ExtraInfo entries survived close: %d", extraCount)
	}
	if xmlCount != 0 {
		t.Errorf("record response XML entries survived close: %d", xmlCount)
	}
	if appendixA4Count != 0 {
		t.Errorf("record response Appendix A.4 entries survived close: %d", appendixA4Count)
	}
	if metadataCount != 0 {
		t.Errorf("record response metadata budgets survived close: %d", metadataCount)
	}
}

func TestCloseStopsAutomaticQueryExpiryTimer(t *testing.T) {
	api := &GB28181API{
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:  newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
		lifecycleDone:    make(chan struct{}),
	}
	api.expectAutomaticQueryResponse(gb10DeviceID, "DeviceInfo", 901, gb10DeviceID)
	value, ok := api.pendingDeviceQuery.Load(buildPendingQueryKey(gb10DeviceID, "DeviceInfo", 901))
	if !ok {
		t.Fatal("automatic query expectation was not registered")
	}
	pending := value.(*pendingQueryWait)

	api.close()

	pending.expiryMu.Lock()
	timer := pending.expiryTimer
	stopped := pending.expiryStopped
	pending.expiryMu.Unlock()
	if timer != nil || !stopped {
		t.Fatalf("automatic query expiry survived close: timer=%v stopped=%v", timer, stopped)
	}
	if !errors.Is(pending.operation.Cause(), ErrServiceStopped) {
		t.Fatalf("automatic query close cause = %v; want %v", pending.operation.Cause(), ErrServiceStopped)
	}
}

func TestInviteDialogCleanerPreservesEstablishedMediaSessions(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	established := &inboundInviteDialog{Established: true, UpdatedAt: now.Add(-pendingInviteDialogTTL - time.Hour)}
	pending := &inboundInviteDialog{UpdatedAt: now.Add(-pendingInviteDialogTTL - time.Second)}
	api.inviteDialogs.Store("established", established)
	api.inviteDialogs.Store("pending", pending)
	api.inviteDialogs.Store("invalid", "unexpected")

	api.cleanupInviteDialogs(now)

	if _, ok := api.inviteDialogs.Load("established"); !ok {
		t.Fatal("established media dialog was expired by SIP inactivity")
	}
	if _, ok := api.inviteDialogs.Load("pending"); ok {
		t.Fatal("stale pending INVITE dialog was retained")
	}
	if _, ok := api.inviteDialogs.Load("invalid"); ok {
		t.Fatal("invalid INVITE dialog entry was retained")
	}
}

func syncMapLen(values *sync.Map) int {
	count := 0
	values.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}
