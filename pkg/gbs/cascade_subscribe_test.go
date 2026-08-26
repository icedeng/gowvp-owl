package gbs

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestRemovingCascadeSubscriptionDoesNotWaitForNotifyResponse(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-notify-removal")
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	worker := newCascadeWorker(&Server{}, platform)
	started := make(chan struct{})
	release := make(chan struct{})
	worker.exchange = func(ctx context.Context, req *sip.Request) (*sip.Response, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return sip.NewResponseFromRequest("", req, http.StatusOK, "OK", nil), nil
		}
	}
	sub := &eventSubscription{
		Key: "cascade-notify-removal", CmdType: "Alarm", DeviceID: platform.localID,
		ExpiresAt: time.Now().Add(time.Minute), To: remote, GBVersion: string(GBVersion30),
		Event: "presence", Response: response, Cascade: worker,
	}
	api := &GB28181API{}
	api.eventSubscribers.Store(sub.Key, sub)
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- api.sendCascadeEventNotify(sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade NOTIFY did not start")
	}

	removeDone := make(chan struct{})
	go func() {
		api.removeCascadeEventSubscriptions(worker)
		close(removeDone)
	}()
	select {
	case <-removeDone:
	case <-time.After(200 * time.Millisecond):
		close(release)
		<-sendDone
		<-removeDone
		t.Fatal("removing cascade subscription waited for in-flight NOTIFY response")
	}
	if _, exists := api.eventSubscribers.Load(sub.Key); exists {
		t.Fatal("cascade subscription remained after worker removal")
	}

	worker.cancel()
	select {
	case err := <-sendDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cascade NOTIFY error = %v; want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled cascade NOTIFY did not stop")
	}
}

func TestCascadeEventNotifyHonorsCallerCancellation(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-notify-caller-cancel")
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	worker := newCascadeWorker(&Server{}, platform)
	started := make(chan struct{})
	worker.exchange = func(ctx context.Context, _ *sip.Request) (*sip.Response, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sub := &eventSubscription{
		Key: "cascade-notify-caller-cancel", CmdType: "Catalog", DeviceID: platform.localID,
		ExpiresAt: time.Now().Add(time.Minute), To: remote, GBVersion: string(GBVersion30),
		Event: "Catalog", Response: response, Cascade: worker,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&GB28181API{}).sendCascadeEventNotifyContext(ctx, sub, "Catalog", []byte(`<Notify><CmdType>Catalog</CmdType></Notify>`))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade NOTIFY did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cascade NOTIFY error = %v; want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not stop cascade NOTIFY")
	}
}

func TestCascadeDownstreamSubscriptionDoesNotBlockUnrelatedKey(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.TargetID == "target-1" && !input.Cancel {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{
			"key-1": {DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60},
		})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first downstream subscription did not start")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{
			"key-2": {DeviceID: "device-2", TargetID: "target-2", Event: "Catalog", Expires: 60},
		})
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("unrelated downstream subscription failed: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(releaseFirst)
		<-firstDone
		<-secondDone
		t.Fatal("unrelated downstream subscription waited for global subscription lock")
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first downstream subscription failed: %v", err)
	}
	api.cascadeSubscriptionOpMu.Lock()
	operationCount := len(api.cascadeSubscriptionOps)
	api.cascadeSubscriptionOpMu.Unlock()
	if operationCount != 0 {
		t.Fatalf("released cascade subscription operations retained %d entries", operationCount)
	}
}

func TestEventSubscriptionOperationsSerializePerKeyWithoutCrossKeyBlocking(t *testing.T) {
	api := &GB28181API{}
	unlockFirst, err := api.lockEventSubscriptionOperation(context.Background(), "event-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = api.lockEventSubscriptionOperation(ctx, "event-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same event subscription lock error = %v; want %v", err, context.DeadlineExceeded)
	}
	otherCtx, otherCancel := context.WithTimeout(context.Background(), time.Second)
	defer otherCancel()
	unlockSecond, err := api.lockEventSubscriptionOperation(otherCtx, "event-2")
	if err != nil {
		t.Fatalf("unrelated event subscription was blocked: %v", err)
	}
	unlockSecond()
	unlockFirst()

	api.eventSubscriptionMu.Lock()
	count := len(api.eventSubscriptionOps)
	api.eventSubscriptionMu.Unlock()
	if count != 0 {
		t.Fatalf("released event subscription operations retained %d entries", count)
	}
}

func TestClosingCascadeSubscriptionsWaitsForPendingCreate(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	cancelled := make(chan struct{})
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.Cancel {
			close(cancelled)
			return nil
		}
		close(createStarted)
		<-releaseCreate
		return nil
	}

	createDone := make(chan error, 1)
	go func() {
		_, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{
			"pending-key": {DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60},
		})
		createDone <- err
	}()
	select {
	case <-createStarted:
	case <-time.After(time.Second):
		t.Fatal("pending downstream subscription did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		api.closeCascadeDownstreamSubscriptions()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("cascade subscription close returned before pending create settled")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCreate)
	if err := <-createDone; err != nil {
		t.Fatalf("pending downstream subscription failed: %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("cascade subscription close did not finish")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("pending downstream subscription was not cancelled after creation")
	}
	api.cascadeSubscriptionMu.Lock()
	remaining := len(api.cascadeSubscriptions)
	api.cascadeSubscriptionMu.Unlock()
	if remaining != 0 {
		t.Fatalf("cascade subscriptions retained %d entries after close", remaining)
	}
}

func TestAlarmSubscriptionFilterValidationAndMatch(t *testing.T) {
	valid := subscribeEventRequest{
		CmdType: "Alarm", SN: 1, DeviceID: gb10DeviceID,
		StartAlarmPriority: "1", EndAlarmPriority: "3", AlarmMethod: "25",
		AlarmType: "2", StartAlarmTime: "2026-08-25T08:00:00", EndAlarmTime: "2026-08-25T09:00:00",
	}
	if err := validateSubscribeEventRequest(valid, "Alarm", GBVersion30); err != nil {
		t.Fatalf("valid Alarm subscription rejected: %v", err)
	}
	filter := subscriptionFilterFromRequest(valid)
	matching := []byte(`<Notify><CmdType>Alarm</CmdType><SN>2</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>2</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmType>2</AlarmType><AlarmTime>2026-08-25T08:30:00</AlarmTime></Notify>`)
	if !alarmMatchesSubscription(filter, matching) {
		t.Fatal("matching Alarm event was filtered")
	}
	if alarmMatchesSubscription(filter, []byte(strings.ReplaceAll(string(matching), "<AlarmPriority>2</AlarmPriority>", "<AlarmPriority>4</AlarmPriority>"))) {
		t.Fatal("out-of-range Alarm priority was forwarded")
	}
	if alarmMatchesSubscription(filter, []byte(strings.ReplaceAll(string(matching), "<AlarmMethod>5</AlarmMethod>", "<AlarmMethod>6</AlarmMethod>"))) {
		t.Fatal("non-matching Alarm method was forwarded")
	}
	if err := validateSubscribeEventRequest(valid, "Alarm", GBVersion20); err != nil {
		t.Fatalf("2.0 AlarmType subscription rejected: %v", err)
	}
	if err := validateSubscribeEventRequest(valid, "Alarm", GBVersion11); err == nil || !strings.Contains(err.Error(), "2016") {
		t.Fatalf("1.1 AlarmType validation error = %v", err)
	}
	invalidRange := valid
	invalidRange.StartAlarmPriority, invalidRange.EndAlarmPriority = "4", "1"
	if err := validateSubscribeEventRequest(invalidRange, "Alarm", GBVersion30); err == nil {
		t.Fatal("invalid Alarm priority range was accepted")
	}
}

func TestCascadeAlarmSubscriptionBridgesRenewalAndCancel(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	connection := newFlowConnection()
	connection.remote = platform.remote
	runtimeDevice := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{
		flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice},
		channel:    runtimeChannel,
	}
	server := &Server{Server: sip.NewServer(local), memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server, cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()

	calls := make([]SubscribeInput, 0, 3)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	body, err := sip.XMLEncode(subscribeEventRequest{
		CmdType: "Alarm", SN: 18, DeviceID: testExposedChannelID,
		StartAlarmPriority: "1", EndAlarmPriority: "3", AlarmMethod: "25", AlarmType: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	callID := sip.CallID("cascade-alarm-bridge")
	from := remote.Clone()
	from.Params.Add("tag", sip.String{Str: "upstream-tag"})
	makeRequest := func(expires string, cseq uint32, localTag string) *sip.Request {
		request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(from).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		applyInboundSubscribeDialog(t, request, localTag, cseq)
		return request
	}
	localTag := ""
	invoke := func(expires string, cseq uint32) {
		ctx := &sip.Context{
			Request: makeRequest(expires, cseq, localTag), Tx: sip.NewTransaction("cascade-alarm-bridge-"+expires, connection),
			DeviceID: platform.serverID, Source: connection.remote, To: remote, XGBVer: string(GBVersion30),
		}
		ctx.Set(cascadeWorkerContextKey, worker)
		api.sipSubscribeEvent(ctx)
		select {
		case response := <-connection.writes:
			if !strings.Contains(string(response), "SIP/2.0 200 OK") {
				t.Fatalf("SUBSCRIBE %s response = %s", expires, response)
			}
		case <-time.After(time.Second):
			t.Fatalf("SUBSCRIBE %s response timeout", expires)
		}
		if localTag == "" {
			api.eventSubscribers.Range(func(_, value any) bool {
				if subscription, ok := value.(*eventSubscription); ok && subscription != nil {
					localTag = subscription.LocalTag
				}
				return false
			})
			if localTag == "" {
				t.Fatal("initial subscription missing local dialog tag")
			}
		}
	}

	invoke("120", 1)
	invoke("240", 2)
	invoke("0", 3)
	if len(calls) != 3 {
		t.Fatalf("downstream subscription calls = %d; want 3", len(calls))
	}
	for index, call := range calls[:2] {
		if call.Cancel || call.DeviceID != gb10DeviceID || call.TargetID != gb10DeviceID || call.Event != "Alarm" || call.AlarmMethod != "25" || call.AlarmType != "2" {
			t.Fatalf("downstream call %d = %+v", index, call)
		}
	}
	if calls[0].Expires != 120 || calls[1].Expires != 240 || !calls[2].Cancel {
		t.Fatalf("downstream lifecycle = %+v", calls)
	}
	if len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("downstream references remain after cancel: %+v", api.cascadeSubscriptions)
	}
}

func TestCascadePositionSubscriptionTargetsSharedChannel(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtime}}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	worker := newCascadeWorker(api.svr, testSharedCascadePlatform(t))
	interval := 5
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "MobilePosition", SN: 1, DeviceID: testExposedChannelID, Interval: &interval,
	}, "MobilePosition", testExposedChannelID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("position downstream targets = %d", len(desired))
	}
	for _, input := range desired {
		if input.DeviceID != gb10DeviceID || input.TargetID != testCascadeChannelID || input.Interval != 5 {
			t.Fatalf("position downstream input = %+v", input)
		}
	}
}

func TestCascadePlatformPositionSubscriptionAggregatesByParent(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	secondID := "34020000001320000012"
	if err := adapter.Store().Channel().Create(t.Context(), &ipc.Channel{
		ID: "GBC_cascade_channel_2", DID: persistentDevice.ID, DeviceID: persistentDevice.DeviceID,
		ChannelID: secondID, Name: "Back Gate", Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	platform := testSharedCascadePlatform(t)
	platform.sharedChannels = append(platform.sharedChannels, secondID)
	platform.channelIDMap[secondID] = secondID
	platform.exposedChannelMap[secondID] = secondID
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}}}
	worker := newCascadeWorker(api.svr, platform)
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "PTZPosition", SN: 1, DeviceID: platform.localID,
	}, "PTZPosition", platform.localID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("platform PTZ subscriptions = %d; want one parent subscription", len(desired))
	}
	for _, input := range desired {
		if input.TargetID != persistentDevice.DeviceID {
			t.Fatalf("platform PTZ target = %+v", input)
		}
	}
}

func TestCascadeCatalogSubscriptionBridgesDownstreamParent(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}},
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
	}
	worker := newCascadeWorker(api.svr, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "Catalog", SN: 1, DeviceID: platform.localID,
	}, "Catalog", platform.localID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("Catalog downstream subscriptions = %d; want one parent", len(desired))
	}
	var downstream SubscribeInput
	for _, input := range desired {
		downstream = input
	}
	if downstream.DeviceID != persistentDevice.DeviceID || downstream.TargetID != persistentDevice.DeviceID || downstream.Event != "Catalog" || downstream.Expires != 600 {
		t.Fatalf("Catalog downstream input = %+v", downstream)
	}
	calls := make([]SubscribeInput, 0, 2)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	keys, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), keys)
	if len(calls) != 2 || calls[0].Cancel || !calls[1].Cancel {
		t.Fatalf("Catalog downstream lifecycle = %+v", calls)
	}
}

func TestCascadeCatalogSubscriptionSupportsSharedChannelTarget(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}},
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
	}
	worker := newCascadeWorker(api.svr, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	if !cascadeSubscriptionTargetAllowed(platform, "Catalog", testExposedChannelID) {
		t.Fatal("shared channel Catalog subscription target was rejected")
	}
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "Catalog", SN: 1, DeviceID: testExposedChannelID,
	}, "Catalog", testExposedChannelID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("shared channel Catalog subscriptions = %d; want one", len(desired))
	}
	for _, downstream := range desired {
		if downstream.DeviceID != persistentDevice.DeviceID || downstream.TargetID != testCascadeChannelID || downstream.Event != "Catalog" {
			t.Fatalf("shared channel Catalog downstream input = %+v", downstream)
		}
	}
}

func TestCascadeSubscribeUsesNegotiatedVersionWhenHeaderMissing(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	api := &GB28181API{}
	connection := newFlowConnection()
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-catalog-without-version-header")
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Catalog", SN: 31, DeviceID: platform.localID})
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + platform.localID})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-catalog-without-version-header", connection),
		DeviceID: platform.serverID, Source: connection.remote, To: remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, connection)
	if !strings.Contains(response, "Event: Catalog;id="+platform.localID) {
		t.Fatalf("negotiated 1.1 response used wrong Event header:\n%s", response)
	}
	if strings.Contains(response, "<Response>") {
		t.Fatalf("negotiated 1.1 response incorrectly used legacy business body:\n%s", response)
	}
}

func TestCascadeSubscriptionReconcilesNewSharedParent(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}},
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
	}
	worker := newCascadeWorker(api.svr, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	calls := make([]SubscribeInput, 0, 2)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	request := subscribeEventRequest{CmdType: "Catalog", SN: 1, DeviceID: platform.localID}
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, request, "Catalog", platform.localID, 600)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	subscription := &eventSubscription{
		Key: "catalog-topology", CmdType: "Catalog", DeviceID: platform.localID, Cascade: worker,
		ExpiresAt: time.Now().Add(10 * time.Minute), DownstreamKeys: keys,
	}
	api.eventSubscribers.Store(subscription.Key, subscription)

	secondDeviceID := "34020000002000000002"
	secondChannelID := "34020000001320000012"
	if err := adapter.Store().Device().Create(t.Context(), &ipc.Device{
		ID: "GB_cascade_device_2", DeviceID: secondDeviceID, Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Store().Channel().Create(t.Context(), &ipc.Channel{
		ID: "GBC_cascade_channel_2", DID: "GB_cascade_device_2", DeviceID: secondDeviceID,
		ChannelID: secondChannelID, Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	worker.platform.sharedChannels = append(worker.platform.sharedChannels, secondChannelID)
	worker.platform.channelIDMap[secondChannelID] = secondChannelID
	worker.platform.exposedChannelMap[secondChannelID] = secondChannelID

	api.reconcileCascadeDownstreamSubscriptions(t.Context())
	if len(calls) != 2 {
		t.Fatalf("topology reconciliation calls = %d; want initial + new parent", len(calls))
	}
	if calls[0].DeviceID != persistentDevice.DeviceID || calls[1].DeviceID != secondDeviceID || calls[0].Cancel || calls[1].Cancel {
		t.Fatalf("topology reconciliation calls = %+v", calls)
	}
	subscription.mu.Lock()
	keyCount := len(subscription.DownstreamKeys)
	subscription.mu.Unlock()
	if keyCount != 2 {
		t.Fatalf("reconciled downstream keys = %d", keyCount)
	}
}

func TestCascadeDownstreamSubscriptionReferenceCounting(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	calls := make([]SubscribeInput, 0, 2)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 120}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	desired := map[string]SubscribeInput{key: input}
	first, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	second, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || api.cascadeSubscriptions[key].Refs != 2 {
		t.Fatalf("shared downstream state = calls %d, state %+v", len(calls), api.cascadeSubscriptions[key])
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), first)
	if len(calls) != 1 || api.cascadeSubscriptions[key].Refs != 1 {
		t.Fatalf("first release cancelled shared downstream: calls %d, state %+v", len(calls), api.cascadeSubscriptions[key])
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), second)
	if len(calls) != 2 || !calls[1].Cancel || len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("last release did not cancel downstream: calls %+v, states %+v", calls, api.cascadeSubscriptions)
	}
}

func TestRemovingCascadeWorkerReleasesDownstreamSubscription(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	cancelled := 0
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.Cancel {
			cancelled++
		}
		return nil
	}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 120}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{Input: input, Refs: 1}
	sub := &eventSubscription{
		Cascade: worker, ExpiresAt: time.Now().Add(time.Minute), DownstreamKeys: []string{key},
	}
	api.eventSubscribers.Store("worker-subscription", sub)
	api.removeCascadeEventSubscriptions(worker)
	if cancelled != 1 || len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("worker removal = cancelled %d, refs %+v", cancelled, api.cascadeSubscriptions)
	}
	if _, exists := api.eventSubscribers.Load("worker-subscription"); exists {
		t.Fatal("worker subscription remained after removal")
	}
}

func TestExpiredCascadeSubscriptionReleasesDownstreamReference(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	cancelled := 0
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.Cancel {
			cancelled++
		}
		return nil
	}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Catalog", Expires: 60}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{Input: input, Refs: 1}
	api.eventSubscribers.Store("expired-catalog", &eventSubscription{
		CmdType: "Catalog", ExpiresAt: time.Now().Add(-time.Second), DownstreamKeys: []string{key},
	})
	api.cleanupEventSubscriptions(time.Now())
	if cancelled != 1 || len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("expired cleanup = cancelled %d, refs %+v", cancelled, api.cascadeSubscriptions)
	}
	if _, exists := api.eventSubscribers.Load("expired-catalog"); exists {
		t.Fatal("expired Catalog subscription remained after cleanup")
	}
}

func TestCascadeSubscriptionRenewalWinsConcurrentCleanup(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	connection := newFlowConnection()
	connection.remote = platform.remote
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	server := &Server{
		Server:       sip.NewServer(local),
		memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime},
	}
	api := &GB28181API{core: adapter, svr: server, cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	callID := sip.CallID("cascade-catalog-renew-cleanup")
	from := remote.Clone()
	from.Params.Add("tag", sip.String{Str: "upstream-tag"})
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Catalog", SN: 33, DeviceID: platform.localID})
	if err != nil {
		t.Fatal(err)
	}
	localTag := ""
	makeContext := func(expires, txID string, cseq uint32) *sip.Context {
		request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(from).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + platform.localID})
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		applyInboundSubscribeDialog(t, request, localTag, cseq)
		ctx := &sip.Context{
			Request: request, Tx: sip.NewTransaction(txID, connection), DeviceID: platform.serverID,
			Source: connection.remote, To: remote, XGBVer: string(GBVersion11),
		}
		ctx.Set(cascadeWorkerContextKey, worker)
		return ctx
	}
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error { return nil }
	api.sipSubscribeEvent(makeContext("60", "catalog-initial", 1))
	assertFlowOK(t, <-flowResponse(t, connection))
	var subscription *eventSubscription
	api.eventSubscribers.Range(func(_, value any) bool {
		subscription, _ = value.(*eventSubscription)
		return false
	})
	if subscription == nil {
		t.Fatal("initial Catalog subscription missing")
	}
	localTag = subscription.LocalTag
	subscription.mu.Lock()
	subscription.ExpiresAt = time.Now().Add(-time.Second)
	subscription.mu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
		close(entered)
		<-release
		return nil
	}
	renewDone := make(chan struct{})
	go func() {
		api.sipSubscribeEvent(makeContext("120", "catalog-renew", 2))
		close(renewDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("renewal did not enter downstream refresh")
	}
	cleanupDone := make(chan struct{})
	go func() {
		api.cleanupEventSubscriptions(time.Now())
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
		t.Fatal("cleanup bypassed in-flight renewal serialization")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-renewDone:
	case <-time.After(time.Second):
		t.Fatal("renewal did not finish")
	}
	assertFlowOK(t, <-flowResponse(t, connection))
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after renewal")
	}
	if _, exists := api.eventSubscribers.Load(subscription.Key); !exists {
		t.Fatal("renewed Catalog subscription was removed by concurrent cleanup")
	}
}

func TestCascadeAlarmFilterSuppressesNotify(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	requestCount := 0
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		requestCount++
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
	sub.Filter = eventSubscriptionFilter{StartAlarmPriority: "1", EndAlarmPriority: "2", AlarmMethod: "5"}
	api := &GB28181API{}
	api.eventSubscribers.Store("filtered-alarm", sub)
	api.publishEventNotify("Alarm", gb10DeviceID, []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+testCascadeChannelID+`</DeviceID><AlarmPriority>3</AlarmPriority><AlarmMethod>5</AlarmMethod></Notify>`))
	if requestCount != 0 {
		t.Fatal("non-matching Alarm was sent upstream")
	}
}

func newCascadeEventSubscriptionForTest(t *testing.T, worker *cascadeWorker, cmdType, deviceID string) *eventSubscription {
	t.Helper()
	remoteURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@remote.example")
	localURI, _ := sip.ParseSipURI("sip:" + gb10DeviceID + "@local.example")
	callID := sip.CallID("cascade-filter-subscribe")
	subscribe := sip.NewRequest("", sip.MethodSubscribe, &localURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(&sip.Address{URI: &remoteURI, Params: sip.NewParams().Add("tag", sip.String{Str: "remote-tag"})}).
			SetTo(&sip.Address{URI: &localURI, Params: sip.NewParams()}).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	return &eventSubscription{
		CmdType: cmdType, DeviceID: deviceID, Event: "presence", ExpiresAt: time.Now().Add(time.Hour),
		To: &sip.Address{URI: &remoteURI, Params: sip.NewParams()}, GBVersion: string(GBVersion30),
		Response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil), Contact: worker.contactAddress(), Cascade: worker,
	}
}
