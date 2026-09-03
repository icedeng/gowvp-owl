package gbs

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestMediaStreamLifecycleActiveAndInactive(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	stream := &Streams{
		DeviceID:  gb10DeviceID,
		ChannelID: gb10ChannelID,
		StreamID:  "internal-stream-1",
	}
	streams.Store("history:Playback:"+gb10DeviceID+":"+gb10ChannelID, stream)
	api := &GB28181API{streams: streams}
	now := time.Now()

	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Active: true, At: now}); err != nil {
		t.Fatal(err)
	}
	if !stream.Stream || !stream.LastMediaAt.Equal(now) {
		t.Fatalf("active stream state = %+v", stream)
	}
	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "rtp_server_timeout", At: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if stream.Stream || !stream.Stop || stream.EndReason != "rtp_server_timeout" {
		t.Fatalf("inactive stream state = %+v", stream)
	}
	if streams.Len() != 0 {
		t.Fatal("inactive stream was not removed")
	}
	if got := api.metrics.Snapshot().MediaDisconnects; got != 1 {
		t.Fatalf("media disconnect metric = %d; want 1", got)
	}

	// 重复注销和 MediaStatus 竞争后均应保持幂等。
	if err := api.OnMediaStreamChanged(context.Background(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "duplicate"}); err != nil {
		t.Fatal(err)
	}
}

func TestMediaStreamLifecycleSerializesDuplicateActiveEvents(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "duplicate-active-stream",
	}
	streams.Store(resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, ""), stream)
	api := &GB28181API{streams: streams}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for index := 0; index < 16; index++ {
		wg.Add(1)
		go func(offset int) {
			defer wg.Done()
			<-start
			if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{
				StreamID: stream.StreamID, Active: true, At: time.Unix(int64(offset+1), 0),
			}); err != nil {
				t.Errorf("duplicate active event %d: %v", offset, err)
			}
		}(index)
	}
	close(start)
	wg.Wait()
	if !stream.Stream || stream.Status != 0 || stream.LastMediaAt.IsZero() {
		t.Fatalf("duplicate active stream state = %+v", stream)
	}
}

func TestMediaStreamLifecycleIgnoresAnotherMediaServerGeneration(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "shared-stream-id",
		Stream: true, mediaServer: &sms.MediaServer{ID: "edge-zlm-1"},
	}
	key := resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")
	streams.Store(key, stream)
	api := &GB28181API{streams: streams, sms: &fakeRTPMediaService{}}

	if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{
		MediaServerID: "edge-zlm-2", StreamID: stream.StreamID, Reason: "rtp_server_timeout",
	}); err != nil {
		t.Fatal(err)
	}
	if current, ok := streams.Load(key); !ok || current != stream || stream.Stop || !stream.Stream {
		t.Fatalf("foreign media server event changed stream: exists:%v stream:%+v", ok, stream)
	}
	if got := api.metrics.Snapshot().MediaDisconnects; got != 0 {
		t.Fatalf("foreign media server disconnect metric = %d", got)
	}

	if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{
		MediaServerID: "edge-zlm-1", StreamID: stream.StreamID, Reason: "rtp_server_timeout",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := streams.Load(key); ok || !stream.Stop || stream.Stream {
		t.Fatalf("owning media server event did not stop stream: exists:%v stream:%+v", ok, stream)
	}
}

func TestMediaStreamLossSendsDeviceBYEAndClosesRTPServer(t *testing.T) {
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "live", key: resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")},
		{name: "playback", key: historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			baseConnection := newFlowConnection()
			connection := &tcpFlowConnection{flowConnection: baseConnection}
			local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			remote := mustFlowAddress(t, "sip:"+gb10ChannelID+"@192.0.2.10:5060")
			local.Params.Add("tag", sip.String{Str: "platform-tag"})
			callID := sip.CallID("media-loss-" + test.name)
			invite := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetContact(local).SetMethod(sip.MethodInvite).
					SetSeqNo(1).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			invite.SetConnection(connection)
			invite.SetSource(baseConnection.local)
			invite.SetDestination(baseConnection.remote)
			response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
			response.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
			response.SetConnection(connection)

			sipServer := sip.NewServer(local)
			defer sipServer.Close()
			media := &fakeRTPMediaService{}
			streams := &conc.Map[string, *Streams]{}
			stream := &Streams{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "lost-" + test.name,
				Resp: response, mediaServer: &sms.MediaServer{ID: sms.DefaultMediaServerID},
			}
			streams.Store(test.key, stream)
			api := &GB28181API{sms: media, streams: streams}
			api.svr = &Server{Server: sipServer, gb: api, fromAddress: *local}

			if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "rtp_timeout"}); err != nil {
				t.Fatal(err)
			}
			select {
			case payload := <-baseConnection.writes:
				bye := string(payload)
				if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+string(callID)) || !strings.Contains(bye, "CSeq: 2 BYE") {
					t.Fatalf("media loss BYE:\n%s", bye)
				}
			case <-time.After(time.Second):
				t.Fatal("media loss did not send device BYE")
			}
			media.mu.Lock()
			closeCalls, closed := media.closeCalls, media.closed
			media.mu.Unlock()
			if closeCalls != 1 || closed.StreamID != stream.StreamID {
				t.Fatalf("RTP cleanup = calls:%d request:%+v", closeCalls, closed)
			}
			if _, ok := streams.Load(test.key); ok {
				t.Fatal("lost stream remained registered")
			}

			if err := api.OnMediaStreamChanged(t.Context(), MediaStreamEvent{StreamID: stream.StreamID, Reason: "duplicate"}); err != nil {
				t.Fatal(err)
			}
			media.mu.Lock()
			closeCalls = media.closeCalls
			media.mu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("duplicate stream loss closed RTP %d times", closeCalls)
			}
		})
	}
}

func TestDeviceLogoutReleasesActiveMediaChain(t *testing.T) {
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10ChannelID+"@192.0.2.10:5060")
	local.Params.Add("tag", sip.String{Str: "platform-tag"})
	callID := sip.CallID("device-offline-media")
	invite := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetContact(local).SetMethod(sip.MethodInvite).
			SetSeqNo(1).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	invite.SetConnection(connection)
	invite.SetSource(baseConnection.local)
	invite.SetDestination(baseConnection.remote)
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
	response.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
	response.SetConnection(connection)

	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.conn = connection
	memory.runtime.source = baseConnection.remote
	memory.runtime.to = remote
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	media := &fakeRTPMediaService{}
	streams := &conc.Map[string, *Streams]{}
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "offline-live",
		Resp: response, mediaServer: &sms.MediaServer{ID: sms.DefaultMediaServerID},
	}
	key := resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")
	streams.Store(key, stream)
	sipServer := sip.NewServer(local)
	defer sipServer.Close()
	api := &GB28181API{sms: media, streams: streams}
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	pendingOperation := newPendingDeviceOperation(context.Background(), gb10DeviceID)
	api.pendingDeviceQuery.Store("offline-query", &pendingQueryWait{operation: pendingOperation})
	api.cascadeMobilePositionQueries.Store("offline-mobile", &cascadeMobilePositionQueryRoute{sourceDeviceID: gb10DeviceID})
	offlineSubscriptionKey := gb10DeviceID + "|" + gb10ChannelID + "|CATALOG"
	otherSubscriptionKey := "34020000001320000009|34020000001320000010|CATALOG"
	api.outgoingSubscriptions.Store(offlineSubscriptionKey, &outgoingSubscriptionDialog{
		deviceID: gb10DeviceID, targetID: gb10ChannelID, expiresAt: time.Now().Add(time.Minute),
	})
	api.outgoingSubscriptions.Store(otherSubscriptionKey, &outgoingSubscriptionDialog{})

	if err := api.logout(gb10DeviceID, func(device *ipc.Device) error {
		device.IsOnline = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if memory.runtime.IsOnlineNow() {
		t.Fatal("logged out device remained online")
	}
	select {
	case <-pendingOperation.Done():
		if !errors.Is(pendingOperation.Cause(), ErrDeviceOffline) {
			t.Fatalf("offline pending operation cause = %v", pendingOperation.Cause())
		}
	default:
		t.Fatal("device logout did not cancel pending operation")
	}
	if _, exists := api.pendingDeviceQuery.Load("offline-query"); exists {
		t.Fatal("device logout retained pending operation")
	}
	if _, exists := api.cascadeMobilePositionQueries.Load("offline-mobile"); exists {
		t.Fatal("device logout retained MobilePosition route")
	}
	if _, exists := api.outgoingSubscriptions.Load(offlineSubscriptionKey); exists {
		t.Fatal("device logout retained outgoing subscription dialog")
	}
	if _, exists := api.outgoingSubscriptions.Load(otherSubscriptionKey); !exists {
		t.Fatal("device logout removed another device subscription dialog")
	}
	if _, exists := streams.Load(key); exists || !stream.Stop || stream.EndReason != "device_offline" {
		t.Fatalf("offline media state = exists:%v stream:%+v", exists, stream)
	}
	select {
	case payload := <-baseConnection.writes:
		bye := string(payload)
		if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+string(callID)) {
			t.Fatalf("device logout BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("device logout did not release the SIP media dialog")
	}
	media.mu.Lock()
	closeCalls, closed := media.closeCalls, media.closed
	media.mu.Unlock()
	if closeCalls != 1 || closed.StreamID != stream.StreamID {
		t.Fatalf("device logout RTP cleanup = calls:%d request:%+v", closeCalls, closed)
	}
}

func TestOfflineCatalogChannelReleasesOnlyItsMediaChain(t *testing.T) {
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+testCascadeChannelID+"@192.0.2.10:5060")
	local.Params.Add("tag", sip.String{Str: "platform-tag"})
	callID := sip.CallID("catalog-channel-offline")
	invite := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetContact(local).SetMethod(sip.MethodInvite).
			SetSeqNo(1).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	invite.SetConnection(connection)
	invite.SetSource(baseConnection.local)
	invite.SetDestination(baseConnection.remote)
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
	response.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
	response.SetConnection(connection)

	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.conn = connection
	memory.runtime.source = baseConnection.remote
	memory.runtime.to = remote
	memory.runtime.Channels.Store(testCascadeChannelID, &Channel{ChannelID: testCascadeChannelID, device: memory.runtime})
	otherChannelID := "34020000001320000003"
	memory.runtime.Channels.Store(otherChannelID, &Channel{ChannelID: otherChannelID, device: memory.runtime})
	media := &fakeRTPMediaService{}
	streams := &conc.Map[string, *Streams]{}
	offlineStream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, StreamID: "catalog-offline",
		Resp: response, mediaServer: &sms.MediaServer{ID: sms.DefaultMediaServerID},
	}
	otherStream := &Streams{DeviceID: gb10DeviceID, ChannelID: otherChannelID, StreamID: "catalog-online"}
	offlineKey := resolvePlaySessionKey(gb10DeviceID, testCascadeChannelID, "")
	otherKey := resolvePlaySessionKey(gb10DeviceID, otherChannelID, "")
	streams.Store(offlineKey, offlineStream)
	streams.Store(otherKey, otherStream)
	adapter, device, _ := newCascadeMediaCore(t)
	if err := adapter.Store().Channel().Create(t.Context(), &ipc.Channel{
		ID: "GBC_catalog_online_sibling", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: otherChannelID, Name: "online", Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(local)
	defer sipServer.Close()
	api := &GB28181API{
		core: adapter, sms: media, streams: streams, lifecycleDone: make(chan struct{}),
		recordResponses: newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
	}
	defer func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	}()
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	offlineOperation := newPendingDeviceOperation(context.Background(), gb10DeviceID, testCascadeChannelID)
	otherOperation := newPendingDeviceOperation(context.Background(), gb10DeviceID, otherChannelID)
	offlineRequest := newPendingDeviceOperation(context.Background(), gb10DeviceID, testCascadeChannelID)
	otherRequest := newPendingDeviceOperation(context.Background(), gb10DeviceID, otherChannelID)
	defer otherOperation.Cancel(nil)
	defer otherRequest.Cancel(nil)
	api.pendingDeviceQuery.Store("offline-channel-query", &pendingQueryWait{operation: offlineOperation})
	api.pendingDeviceQuery.Store("online-channel-query", &pendingQueryWait{operation: otherOperation})
	api.pendingDeviceRequests.Store("offline-channel-request", offlineRequest)
	api.pendingDeviceRequests.Store("online-channel-request", otherRequest)
	api.cascadeMobilePositionQueries.Store("offline-channel-mobile", &cascadeMobilePositionQueryRoute{
		sourceDeviceID: gb10DeviceID, sourceTargetID: testCascadeChannelID,
	})
	api.cascadeMobilePositionQueries.Store("online-channel-mobile", &cascadeMobilePositionQueryRoute{
		sourceDeviceID: gb10DeviceID, sourceTargetID: otherChannelID,
	})
	recordKey := buildMultiResponseKey(testCascadeChannelID, "RecordInfo", 41)
	recordAlias := buildMultiResponseKey(gb10DeviceID, "RecordInfo", 41)
	recordOperation := newPendingDeviceOperation(context.Background(), gb10DeviceID, testCascadeChannelID)
	api.recordResponses.Start(recordKey)
	api.pendingMultiResponse.Store(recordKey, recordOperation)
	api.recordResponseAliases.Store(recordAlias, recordKey)
	recordDone := make(chan multiResponseResult[RecordItem], 1)
	go func() { recordDone <- api.recordResponses.Wait(context.Background(), recordKey) }()

	api.saveCatalogChannels(gb10DeviceID, []Channels{
		{ChannelID: testCascadeChannelID, Name: "offline", Status: "OFF"},
		{ChannelID: otherChannelID, Name: "online", Status: "ON"},
	})
	if _, exists := streams.Load(offlineKey); exists || !offlineStream.Stop || offlineStream.EndReason != "channel_offline" {
		t.Fatalf("offline Catalog media = exists:%v stream:%+v", exists, offlineStream)
	}
	if current, exists := streams.Load(otherKey); !exists || current != otherStream || otherStream.Stop {
		t.Fatalf("online sibling media was affected: exists:%v stream:%+v", exists, current)
	}
	select {
	case <-offlineOperation.Done():
		if !errors.Is(offlineOperation.Cause(), ErrChannelOffline) {
			t.Fatalf("offline channel pending cause = %v", offlineOperation.Cause())
		}
	default:
		t.Fatal("offline channel pending operation was not cancelled")
	}
	select {
	case <-otherOperation.Done():
		t.Fatalf("online sibling pending operation was cancelled: %v", otherOperation.Cause())
	default:
	}
	select {
	case <-offlineRequest.Done():
		if !errors.Is(offlineRequest.Cause(), ErrChannelOffline) {
			t.Fatalf("offline channel pending SIP request cause = %v", offlineRequest.Cause())
		}
	default:
		t.Fatal("offline channel pending SIP request was not cancelled")
	}
	select {
	case <-otherRequest.Done():
		t.Fatalf("online sibling pending SIP request was cancelled: %v", otherRequest.Cause())
	default:
	}
	if _, exists := api.cascadeMobilePositionQueries.Load("offline-channel-mobile"); exists {
		t.Fatal("offline channel MobilePosition route survived")
	}
	if _, exists := api.cascadeMobilePositionQueries.Load("online-channel-mobile"); !exists {
		t.Fatal("online sibling MobilePosition route was removed")
	}
	select {
	case result := <-recordDone:
		if !errors.Is(result.Err, ErrChannelOffline) {
			t.Fatalf("offline channel RecordInfo result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("offline channel RecordInfo waiter did not stop")
	}
	select {
	case <-recordOperation.Done():
		if !errors.Is(recordOperation.Cause(), ErrChannelOffline) {
			t.Fatalf("offline channel RecordInfo operation cause = %v", recordOperation.Cause())
		}
	default:
		t.Fatal("offline channel RecordInfo SIP operation was not cancelled")
	}
	select {
	case payload := <-baseConnection.writes:
		if bye := string(payload); !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+string(callID)) {
			t.Fatalf("Catalog offline BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("offline Catalog channel did not release the SIP media dialog")
	}
}

func TestMediaStartFailuresDoNotRetainPlaceholders(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	media := &fakeRTPMediaService{}
	streams := &conc.Map[string, *Streams]{}
	cfg := conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}
	api := &GB28181API{cfg: &cfg, sms: media, streams: streams}
	api.svr = &Server{gb: api, memoryStorer: memory}
	inputChannel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}
	mediaServer := &sms.MediaServer{}

	if err := api.Play(&PlayInput{Channel: inputChannel, SMS: mediaServer}); err == nil {
		t.Fatal("Play unexpectedly succeeded")
	}
	if streams.Len() != 0 {
		t.Fatal("failed Play retained a stream placeholder")
	}

	if err := api.StartHistory(t.Context(), &HistoryInput{
		Channel: inputChannel, SMS: mediaServer, Mode: historyModePlayback,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now(),
	}); err == nil {
		t.Fatal("StartHistory unexpectedly succeeded")
	}
	if streams.Len() != 0 {
		t.Fatal("failed history start retained a stream placeholder")
	}

	placeholderKey := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	streams.Store(placeholderKey, &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID})
	if err := api.stopHistoryNoLock(channel, &StopHistoryInput{Channel: inputChannel, Mode: historyModePlayback}); err != nil {
		t.Fatal(err)
	}
	if streams.Len() != 0 {
		t.Fatal("stopping an incomplete history session retained its placeholder")
	}
}
