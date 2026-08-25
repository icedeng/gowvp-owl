package gbs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
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
		if !directExists && !rtpExists && !queryExists {
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
