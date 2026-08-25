package gbs

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestTickerCheckStopsWithGB28181Lifecycle(t *testing.T) {
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	server := &Server{gb: api}
	done := make(chan struct{})
	go func() {
		server.startTickerCheck()
		close(done)
	}()
	api.close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("device ticker did not stop with GB28181 lifecycle")
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

type refreshBeforeOfflineMemory struct {
	*flowMemory
	refreshedAt time.Time
	refreshed   bool
}

type probePersistenceFailureMemory struct {
	*flowMemory
	err error
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

var _ MemoryStorer = (*refreshBeforeOfflineMemory)(nil)

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

	api.pendingDeviceControl.Store("control", &pendingDeviceControl{})
	api.pendingDeviceQuery.Store("query", &pendingQueryWait{})
	api.pendingDeviceConfig.Store("config", &pendingDeviceConfig{})
	api.pendingBroadcast.Store("broadcast", &pendingBroadcastResponse{})
	api.recordResponseAliases.Store("alias", "record")
	api.eventSubscribers.Store("event", &eventSubscription{})
	api.outgoingSubscriptions.Store("outgoing", &outgoingSubscriptionDialog{})

	api.close()
	api.close()

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
