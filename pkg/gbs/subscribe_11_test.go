package gbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

type tcpFlowConnection struct {
	*flowConnection
}

func (*tcpFlowConnection) Network() string { return "tcp" }

type eventNotifyPanickingCloneHeader struct{}

func (eventNotifyPanickingCloneHeader) Name() string      { return "X-Panic-Clone" }
func (eventNotifyPanickingCloneHeader) String() string    { return "X-Panic-Clone: test" }
func (eventNotifyPanickingCloneHeader) Equals(any) bool   { return false }
func (eventNotifyPanickingCloneHeader) Clone() sip.Header { panic("event NOTIFY clone panic") }

func attachFlowEventSubscriptionDialog(t *testing.T, sub *eventSubscription, conn *flowConnection, callID string) {
	t.Helper()
	request := newFlowRequest(t, conn, sip.MethodSubscribe, callID, []byte("query"))
	sub.DialogRequest = request
	sub.Response = sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
}

func TestKeepaliveRespondsBeforeSlowSubscriptionNotify(t *testing.T) {
	baseConn := newFlowConnection()
	conn := &tcpFlowConnection{flowConnection: baseConn}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{
		Server: sipServer, fromAddress: *platform, memoryStorer: memory,
	}
	api := &GB28181API{svr: server}
	server.gb = api
	statusSubscription := &eventSubscription{
		Key: "slow-device-status", CmdType: "DeviceStatus", DeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
		To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Source:    baseConn.remote, Conn: conn, GBVersion: string(GBVersion10), Event: "presence",
	}
	attachFlowEventSubscriptionDialog(t, statusSubscription, baseConn, "slow-device-status-dialog")
	api.eventSubscribers.Store("slow-device-status", statusSubscription)

	request := newFlowRequest(t, baseConn, sip.MethodMessage, "keepalive-before-notify", readGB10Fixture(t, "keepalive.xml"))
	request.SetConnection(conn)
	done := make(chan struct{})
	go func() {
		api.sipMessageKeepalive(&sip.Context{
			Request: request, Tx: sip.NewTransaction("keepalive-before-notify-tx", conn),
			DeviceID: gb10DeviceID, Source: baseConn.remote,
			To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
		})
		close(done)
	}()

	var first string
	select {
	case payload := <-baseConn.writes:
		first = string(payload)
	case <-time.After(time.Second):
		t.Fatal("Keepalive response timeout")
	}
	if !strings.Contains(first, "SIP/2.0 200 OK") {
		t.Fatalf("first Keepalive write was delayed by subscription NOTIFY:\n%s", first)
	}
	select {
	case payload := <-baseConn.writes:
		if !strings.HasPrefix(string(payload), "NOTIFY ") {
			t.Fatalf("second Keepalive write was not subscription NOTIFY:\n%s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription NOTIFY was not sent")
	}
	sipServer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Keepalive handler did not stop after SIP shutdown")
	}
}

func TestAlarmRespondsBeforeSlowSubscriptionNotify(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })
	baseConn := newFlowConnection()
	conn := &tcpFlowConnection{flowConnection: baseConn}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	server := &Server{Server: sipServer, fromAddress: *platform}
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	memory.runtime.UpdateRuntime(func(current *Device) {
		current.conn = conn
		current.source = baseConn.remote
		current.to = mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	})
	server.memoryStorer = memory
	api := &GB28181API{svr: server}
	server.gb = api
	alarmSubscription := &eventSubscription{
		Key: "slow-alarm", CmdType: "Alarm", DeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
		To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Source:    baseConn.remote, Conn: conn, GBVersion: string(GBVersion10), Event: "presence",
	}
	attachFlowEventSubscriptionDialog(t, alarmSubscription, baseConn, "slow-alarm-dialog")
	api.eventSubscribers.Store("slow-alarm", alarmSubscription)

	request := newFlowRequest(t, baseConn, sip.MethodMessage, "alarm-before-notify", readGB10Fixture(t, "alarm-notify.xml"))
	request.SetConnection(conn)
	done := make(chan struct{})
	go func() {
		api.sipMessageAlarm(&sip.Context{
			Request: request, Tx: sip.NewTransaction("alarm-before-notify-tx", conn),
			DeviceID: gb10DeviceID, Source: baseConn.remote,
			To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
		})
		close(done)
	}()

	select {
	case payload := <-baseConn.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("first Alarm write was delayed by subscription NOTIFY:\n%s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm response timeout")
	}
	select {
	case payload := <-baseConn.writes:
		if !strings.HasPrefix(string(payload), "MESSAGE ") || !strings.Contains(string(payload), "<Response>") || !strings.Contains(string(payload), "<CmdType>Alarm</CmdType>") {
			t.Fatalf("second Alarm write was not business response MESSAGE:\n%s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm business response MESSAGE was not sent")
	}
	select {
	case payload := <-baseConn.writes:
		if !strings.HasPrefix(string(payload), "NOTIFY ") {
			t.Fatalf("third Alarm write was not subscription NOTIFY:\n%s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm subscription NOTIFY was not sent")
	}
	server.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Alarm handler did not stop after SIP shutdown")
	}
}

func TestDeviceEventNotifyUsesSubscriptionDialogAndSerializesCSeq(t *testing.T) {
	oldConn := newFlowConnection()
	currentBase := newFlowConnection()
	currentConn := &tcpFlowConnection{flowConnection: currentBase}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api

	subscribe := newFlowRequest(t, oldConn, sip.MethodSubscribe, "device-notify-dialog", []byte("query"))
	contact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@contact.example:5070")
	subscribe.AppendHeader(&sip.ContactHeader{Address: contact.URI.Clone(), Params: sip.NewParams()})
	proxy, _ := sip.ParseURI("sip:proxy.example;lr")
	subscribe.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{proxy}})
	response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
	sub := &eventSubscription{
		Key: "device-notify-dialog", CmdType: "Alarm", DeviceID: gb10DeviceID, Event: "presence",
		ExpiresAt: time.Now().Add(time.Minute), To: contact, Source: currentBase.remote, Conn: currentConn,
		GBVersion: string(GBVersion10), DialogRequest: subscribe, Response: response,
	}
	api.eventSubscribers.Store(sub.Key, sub)
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	errs := make(chan error, 2)
	firstCtx, firstCancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer firstCancel()
	secondCtx, secondCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer secondCancel()
	go func() { errs <- api.sendEventNotifyContext(firstCtx, sub, "Alarm", body) }()

	requests := make([]string, 0, 2)
	select {
	case payload := <-currentBase.writes:
		requests = append(requests, string(payload))
	case err := <-errs:
		t.Fatalf("first subscription NOTIFY failed before send: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first subscription NOTIFY was not sent")
	}
	go func() { errs <- api.sendEventNotifyContext(secondCtx, sub, "Alarm", body) }()
	select {
	case payload := <-currentBase.writes:
		requests = append(requests, string(payload))
	case <-time.After(time.Second):
		t.Fatal("second subscription NOTIFY was not sent")
	}
	for range 2 {
		if err := <-errs; err == nil {
			t.Fatal("NOTIFY without response unexpectedly succeeded")
		}
	}
	select {
	case payload := <-oldConn.writes:
		t.Fatalf("NOTIFY reused stale subscription connection: %s", payload)
	default:
	}

	for index, request := range requests {
		for _, expected := range []string{
			"NOTIFY sip:" + gb10DeviceID + "@contact.example:5070 SIP/2.0",
			"Route: <sip:proxy.example;lr>",
			"Call-ID: device-notify-dialog",
			fmt.Sprintf("CSeq: %d NOTIFY", index+1),
			"Event: presence",
		} {
			if !strings.Contains(request, expected) {
				t.Fatalf("NOTIFY %d missing %q:\n%s", index+1, expected, request)
			}
		}
	}
	if actual, exists := api.eventSubscribers.Load(sub.Key); !exists || actual != sub {
		t.Fatal("caller timeout removed the valid device subscription")
	}
}

func TestEventNotifyTargetsStartIndependentlyFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
			api := &GB28181API{
				lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
				lifecycleDone: make(chan struct{}),
			}
			t.Cleanup(func() {
				api.beginClose()
				api.lifecycleWG.Wait()
			})

			started := make(chan string, 2)
			workers := make([]*cascadeWorker, 0, 2)
			for _, name := range []string{"event-upstream-a", "event-upstream-b"} {
				worker := registeredAlarmDispatchWorker(t, version)
				worker.platform.name = name
				worker.exchange = func(ctx context.Context, _ *sip.Request) (*sip.Response, error) {
					started <- name
					<-ctx.Done()
					return nil, ctx.Err()
				}
				workers = append(workers, worker)

				sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
				sub.Key = name
				sub.GBVersion = string(version)
				api.eventSubscribers.Store(sub.Key, sub)
			}

			publishDone := make(chan struct{})
			go func() {
				api.publishEventNotify("Alarm", gb10DeviceID, cascadeAlarmPayload())
				close(publishDone)
			}()

			seen := make(map[string]struct{}, len(workers))
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			for len(seen) < len(workers) {
				select {
				case name := <-started:
					seen[name] = struct{}{}
				case <-deadline.C:
					for _, worker := range workers {
						worker.cancel()
					}
					<-publishDone
					t.Fatalf("event NOTIFY targets were serialized: started=%v", seen)
				}
			}

			for _, worker := range workers {
				worker.cancel()
			}
			select {
			case <-publishDone:
			case <-time.After(time.Second):
				t.Fatal("event NOTIFY publisher did not stop after workers were cancelled")
			}
		})
	}
}

func TestLocalEventNotifyTargetsStartIndependentlyFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
			sipServer := sip.NewServer(platform)
			lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
			server := &Server{Server: sipServer, fromAddress: *platform}
			api := &GB28181API{
				svr: server, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
				lifecycleDone: make(chan struct{}),
			}
			server.gb = api
			t.Cleanup(func() {
				api.beginClose()
				api.lifecycleWG.Wait()
				sipServer.Close()
			})

			writes := make([]<-chan []byte, 0, 2)
			for index, name := range []string{"local-event-subscriber-a", "local-event-subscriber-b"} {
				baseConn := newFlowConnection()
				connection := &tcpFlowConnection{flowConnection: baseConn}
				sub := &eventSubscription{
					Key: name, CmdType: "Alarm", DeviceID: gb10DeviceID,
					ExpiresAt: time.Now().Add(time.Minute),
					To:        mustFlowAddress(t, fmt.Sprintf("sip:%s@subscriber-%d.example:5060", gb10DeviceID, index)),
					Source:    baseConn.remote, Conn: connection, GBVersion: string(version), Event: "presence",
				}
				attachFlowEventSubscriptionDialog(t, sub, baseConn, name+"-dialog")
				api.eventSubscribers.Store(sub.Key, sub)
				writes = append(writes, baseConn.writes)
			}

			publishDone := make(chan struct{})
			go func() {
				api.publishEventNotify("Alarm", gb10DeviceID, cascadeAlarmPayload())
				close(publishDone)
			}()
			for index, output := range writes {
				select {
				case payload := <-output:
					if !strings.HasPrefix(string(payload), "NOTIFY ") {
						t.Fatalf("subscriber %d request is not NOTIFY: %s", index, payload)
					}
				case <-time.After(time.Second):
					t.Fatalf("subscriber %d NOTIFY was blocked by another target", index)
				}
			}
			select {
			case <-publishDone:
			case <-time.After(time.Second):
				t.Fatal("event publisher remained blocked on subscription responses")
			}

			api.beginClose()
			waitDone := make(chan struct{})
			go func() {
				api.lifecycleWG.Wait()
				close(waitDone)
			}()
			select {
			case <-waitDone:
			case <-time.After(time.Second):
				t.Fatal("local event NOTIFY tasks did not stop with the service")
			}
		})
	}
}

func TestEventNotifyUsesSingleFIFOQueuePerSubscriptionFourVersionMatrix(t *testing.T) {
	const total = 32
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
			api := &GB28181API{
				lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
				lifecycleDone: make(chan struct{}),
			}
			t.Cleanup(func() {
				api.beginClose()
				api.lifecycleWG.Wait()
			})

			worker := registeredAlarmDispatchWorker(t, version)
			firstStarted := make(chan struct{})
			releaseFirst := make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-releaseFirst:
				default:
					close(releaseFirst)
				}
			})
			sent := make(chan int, total)
			worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
				var alarm messageAlarm
				if err := sip.XMLDecode(request.Body(), &alarm); err != nil {
					return nil, err
				}
				sent <- alarm.SN
				if alarm.SN == 1 {
					close(firstStarted)
					select {
					case <-releaseFirst:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
			sub.Key = "queued-event-" + string(version)
			sub.GBVersion = string(version)
			api.eventSubscribers.Store(sub.Key, sub)

			api.publishEventNotify("Alarm", gb10DeviceID, queuedEventAlarmBody(1))
			select {
			case <-firstStarted:
			case <-time.After(time.Second):
				t.Fatal("first queued event NOTIFY did not start")
			}
			for sn := 2; sn <= total; sn++ {
				api.publishEventNotify("Alarm", gb10DeviceID, queuedEventAlarmBody(sn))
			}

			sub.notifyDispatchMu.Lock()
			workerActive := sub.notifyWorker
			queued := len(sub.notifyQueue)
			queuedBytes := sub.notifyQueueBytes
			sub.notifyDispatchMu.Unlock()
			if !workerActive || queued != total-1 || queuedBytes <= 0 {
				t.Fatalf("event NOTIFY dispatcher state = active:%v queued:%d bytes:%d; want one worker and %d queued batches",
					workerActive, queued, queuedBytes, total-1)
			}

			close(releaseFirst)
			for want := 1; want <= total; want++ {
				select {
				case got := <-sent:
					if got != want {
						t.Fatalf("event NOTIFY order at %d = %d", want, got)
					}
				case <-time.After(time.Second):
					t.Fatalf("event NOTIFY %d was not sent", want)
				}
			}
		})
	}
}

func TestEventNotifyDropsQueuedBatchAfterSubscriptionRenewalFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			testEventNotifyDropsQueuedBatchAfterSubscriptionRenewal(t, version)
		})
	}
}

func TestEventNotifyRetriesTransientFailureInOrderFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
			api := &GB28181API{
				lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
				lifecycleDone:        make(chan struct{}),
				eventNotifyRetryWait: func(int, *sip.Response) time.Duration { return 0 },
			}
			t.Cleanup(func() {
				api.beginClose()
				api.lifecycleWG.Wait()
			})
			worker := registeredAlarmDispatchWorker(t, version)
			requests := make(chan int, 4)
			calls := 0
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				var alarm messageAlarm
				if err := sip.XMLDecode(request.Body(), &alarm); err != nil {
					return nil, err
				}
				requests <- alarm.SN
				calls++
				if calls == 1 {
					response := sip.NewResponseFromRequest("", request, http.StatusServiceUnavailable, "Service Unavailable", nil)
					response.AppendHeader(&sip.GenericHeader{HeaderName: "Retry-After", Contents: "0"})
					return response, nil
				}
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
			sub.Key = "transient-event-" + string(version)
			sub.GBVersion = string(version)
			api.eventSubscribers.Store(sub.Key, sub)

			api.startEventNotifyTask(sub.Key, sub, worker, sub.RemoteCSeq, "Alarm", gb10DeviceID,
				[][]byte{queuedEventAlarmBody(1), queuedEventAlarmBody(2)})
			want := []int{1, 1, 2}
			for index, expected := range want {
				select {
				case actual := <-requests:
					if actual != expected {
						t.Fatalf("NOTIFY request %d SN = %d, want %d", index, actual, expected)
					}
				case <-time.After(time.Second):
					t.Fatalf("NOTIFY request %d was not sent", index)
				}
			}
			deadline := time.Now().Add(time.Second)
			for {
				sub.notifyDispatchMu.Lock()
				idle := !sub.notifyWorker && len(sub.notifyQueue) == 0
				sub.notifyDispatchMu.Unlock()
				if idle {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("transient NOTIFY worker did not become idle")
				}
				time.Sleep(time.Millisecond)
			}
			if current, exists := api.eventSubscribers.Load(sub.Key); !exists || current != sub {
				t.Fatal("successful transient retry removed subscription")
			}
		})
	}
}

func TestEventNotifyRetryExhaustionDetachesSubscription(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone:        make(chan struct{}),
		eventNotifyRetryWait: func(int, *sip.Response) time.Duration { return 0 },
	}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	worker := registeredAlarmDispatchWorker(t, GBVersion30)
	var calls atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		calls.Add(1)
		return sip.NewResponseFromRequest("", request, http.StatusServiceUnavailable, "Service Unavailable", nil), nil
	}
	sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
	sub.Key = "exhausted-event-notify"
	sub.GBVersion = string(GBVersion30)
	api.eventSubscribers.Store(sub.Key, sub)
	api.startEventNotifyTask(sub.Key, sub, worker, sub.RemoteCSeq, "Alarm", gb10DeviceID, [][]byte{queuedEventAlarmBody(1)})

	deadline := time.Now().Add(time.Second)
	for {
		if _, exists := api.eventSubscribers.Load(sub.Key); !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("exhausted NOTIFY retained subscription")
		}
		time.Sleep(time.Millisecond)
	}
	if actual := calls.Load(); actual != eventNotifyMaxAttempts {
		t.Fatalf("exhausted NOTIFY calls = %d, want %d", actual, eventNotifyMaxAttempts)
	}
}

func TestEventNotifyRetryStopsAfterSubscriptionRenewal(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone:        make(chan struct{}),
		eventNotifyRetryWait: func(int, *sip.Response) time.Duration { return 50 * time.Millisecond },
	}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	worker := registeredAlarmDispatchWorker(t, GBVersion30)
	first := make(chan struct{})
	var calls atomic.Int32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if calls.Add(1) == 1 {
			close(first)
		}
		return sip.NewResponseFromRequest("", request, http.StatusServiceUnavailable, "Service Unavailable", nil), nil
	}
	sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
	sub.Key = "renew-during-notify-retry"
	sub.GBVersion = string(GBVersion30)
	sub.RemoteCSeq = 1
	api.eventSubscribers.Store(sub.Key, sub)
	api.startEventNotifyTask(sub.Key, sub, worker, 1, "Alarm", gb10DeviceID, [][]byte{queuedEventAlarmBody(1)})
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first NOTIFY retry attempt did not start")
	}
	sub.mu.Lock()
	sub.RemoteCSeq = 2
	sub.ExpiresAt = time.Now().Add(time.Hour)
	sub.mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	if actual := calls.Load(); actual != 1 {
		t.Fatalf("old subscription generation retry calls = %d, want 1", actual)
	}
	if current, exists := api.eventSubscribers.Load(sub.Key); !exists || current != sub {
		t.Fatal("old retry removed renewed subscription")
	}
}

func testEventNotifyDropsQueuedBatchAfterSubscriptionRenewal(t *testing.T, version GBProtocolVersion) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})

	worker := registeredAlarmDispatchWorker(t, version)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	})
	sent := make(chan int, 2)
	worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
		var alarm messageAlarm
		if err := sip.XMLDecode(request.Body(), &alarm); err != nil {
			return nil, err
		}
		sent <- alarm.SN
		if alarm.SN == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
	sub.Key = "renewed-event-subscription"
	sub.GBVersion = string(version)
	sub.RemoteCSeq = 1
	api.eventSubscribers.Store(sub.Key, sub)

	api.publishEventNotify("Alarm", gb10DeviceID, queuedEventAlarmBody(1))
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first renewal test NOTIFY did not start")
	}
	api.publishEventNotify("Alarm", gb10DeviceID, queuedEventAlarmBody(2))

	// 模拟同一对话续订提交：对象和上级 worker 均复用，但远端 SUBSCRIBE CSeq 已进入新代次。
	sub.mu.Lock()
	sub.RemoteCSeq = 2
	sub.ExpiresAt = time.Now().Add(2 * time.Hour)
	sub.mu.Unlock()
	close(releaseFirst)

	select {
	case got := <-sent:
		if got != 1 {
			t.Fatalf("first renewal test NOTIFY SN = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first renewal test NOTIFY was not observed")
	}
	deadline := time.Now().Add(time.Second)
	for {
		sub.notifyDispatchMu.Lock()
		idle := !sub.notifyWorker && len(sub.notifyQueue) == 0
		sub.notifyDispatchMu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event NOTIFY worker did not finish after subscription renewal")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case got := <-sent:
		t.Fatalf("queued pre-renewal NOTIFY %d was sent on the renewed dialog", got)
	default:
	}
}

func TestEventNotifyQueueOverflowDetachesSlowSubscription(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	worker := registeredAlarmDispatchWorker(t, GBVersion30)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	})
	sent := make(chan int, eventNotifyQueueMaxBatches+2)
	worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
		var alarm messageAlarm
		if err := sip.XMLDecode(request.Body(), &alarm); err != nil {
			return nil, err
		}
		sent <- alarm.SN
		if alarm.SN == 1 {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
	sub.Key = "overflowed-event-subscription"
	api.eventSubscribers.Store(sub.Key, sub)

	api.publishEventNotify("Alarm", gb10DeviceID, queuedEventAlarmBody(1))
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first overflow test NOTIFY did not start")
	}
	for sn := 2; sn <= eventNotifyQueueMaxBatches+2; sn++ {
		api.publishEventNotify("Alarm", gb10DeviceID, queuedEventAlarmBody(sn))
	}
	deadline := time.Now().Add(time.Second)
	for {
		if _, exists := api.eventSubscribers.Load(sub.Key); !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queue overflow retained the unresponsive event subscription")
		}
		time.Sleep(time.Millisecond)
	}
	sub.notifyDispatchMu.Lock()
	queued := len(sub.notifyQueue)
	queuedBytes := sub.notifyQueueBytes
	sub.notifyDispatchMu.Unlock()
	if queued != 0 || queuedBytes != 0 {
		t.Fatalf("overflowed event queue = %d batches/%d bytes; want empty", queued, queuedBytes)
	}

	close(releaseFirst)
	select {
	case got := <-sent:
		if got != 1 {
			t.Fatalf("first overflow test NOTIFY SN = %d", got)
		}
	case <-time.After(time.Second):
		t.Fatal("first overflow test NOTIFY was not observed")
	}
	select {
	case got := <-sent:
		t.Fatalf("overflowed subscription sent queued NOTIFY %d", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventNotifyQueueByteLimitDetachesSubscription(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	sub := &eventSubscription{
		Key:       "oversized-event-subscription",
		CmdType:   "Alarm",
		DeviceID:  gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	api.eventSubscribers.Store(sub.Key, sub)

	api.startEventNotifyTask(sub.Key, sub, nil, sub.RemoteCSeq, "Alarm", gb10DeviceID, [][]byte{make([]byte, eventNotifyQueueMaxBytes+1)})

	deadline := time.Now().Add(time.Second)
	for {
		if _, exists := api.eventSubscribers.Load(sub.Key); !exists {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("event body above the byte limit retained the subscription")
		}
		time.Sleep(time.Millisecond)
	}
	sub.notifyDispatchMu.Lock()
	overloaded := sub.notifyOverloaded
	queued := len(sub.notifyQueue)
	queuedBytes := sub.notifyQueueBytes
	workerActive := sub.notifyWorker
	sub.notifyDispatchMu.Unlock()
	if !overloaded || queued != 0 || queuedBytes != 0 || workerActive {
		t.Fatalf("oversized event dispatcher state = overloaded:%v active:%v queued:%d bytes:%d",
			overloaded, workerActive, queued, queuedBytes)
	}
}

func TestEventNotifyQueueOverflowDoesNotDeleteRenewedSubscription(t *testing.T) {
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	sub := &eventSubscription{
		Key:        "renewed-overflowed-event-subscription",
		CmdType:    "Alarm",
		DeviceID:   gb10DeviceID,
		ExpiresAt:  time.Now().Add(time.Minute),
		RemoteCSeq: 1,
	}
	api.eventSubscribers.Store(sub.Key, sub)

	unlock, err := api.lockEventSubscriptionOperation(t.Context(), sub.Key)
	if err != nil {
		t.Fatalf("lock event subscription operation: %v", err)
	}
	api.startEventNotifyTask(sub.Key, sub, nil, sub.RemoteCSeq, "Alarm", gb10DeviceID,
		[][]byte{make([]byte, eventNotifyQueueMaxBytes+1)})
	sub.notifyDispatchMu.Lock()
	overloaded := sub.notifyOverloaded
	sub.notifyDispatchMu.Unlock()
	if !overloaded {
		unlock()
		t.Fatal("oversized event did not mark the old subscription generation overloaded")
	}

	// 模拟在旧过载清理取得订阅操作锁前，续订已原地提交到同一个对象。
	sub.mu.Lock()
	sub.RemoteCSeq = 2
	sub.ExpiresAt = time.Now().Add(2 * time.Hour)
	sub.mu.Unlock()
	unlock()
	api.lifecycleWG.Wait()

	if actual, exists := api.eventSubscribers.Load(sub.Key); !exists || actual != sub {
		t.Fatal("old queue overflow cleanup deleted the renewed subscription generation")
	}
	sub.notifyDispatchMu.Lock()
	overloaded = sub.notifyOverloaded
	queued := len(sub.notifyQueue)
	queuedBytes := sub.notifyQueueBytes
	sub.notifyDispatchMu.Unlock()
	if overloaded || queued != 0 || queuedBytes != 0 {
		t.Fatalf("renewed event dispatcher state = overloaded:%v queued:%d bytes:%d", overloaded, queued, queuedBytes)
	}
}

func TestEventNotifyOverloadStateIsIsolatedBySubscriptionGeneration(t *testing.T) {
	sub := &eventSubscription{notifyOverloaded: true, notifyOverloadDialogCSeq: 2}
	clearEventNotifyOverload(sub, 1)
	sub.notifyDispatchMu.Lock()
	overloaded := sub.notifyOverloaded
	marker := sub.notifyOverloadDialogCSeq
	sub.notifyDispatchMu.Unlock()
	if !overloaded || marker != 2 {
		t.Fatalf("old cleanup changed newer overload state = overloaded:%v marker:%d", overloaded, marker)
	}

	resetStaleEventNotifyOverload(sub, 2)
	sub.notifyDispatchMu.Lock()
	overloaded = sub.notifyOverloaded
	marker = sub.notifyOverloadDialogCSeq
	sub.notifyDispatchMu.Unlock()
	if !overloaded || marker != 2 {
		t.Fatalf("same-generation renewal reset current overload = overloaded:%v marker:%d", overloaded, marker)
	}

	resetStaleEventNotifyOverload(sub, 3)
	sub.notifyDispatchMu.Lock()
	overloaded = sub.notifyOverloaded
	marker = sub.notifyOverloadDialogCSeq
	sub.notifyDispatchMu.Unlock()
	if overloaded || marker != 0 {
		t.Fatalf("new subscription generation retained old overload = overloaded:%v marker:%d", overloaded, marker)
	}
}

func TestEventNotifyDispatchRequiresCapturedCascadeIdentity(t *testing.T) {
	api := &GB28181API{}
	sub := &eventSubscription{Key: "event-dispatch-identity", ExpiresAt: time.Now().Add(time.Minute), RemoteCSeq: 1}
	api.eventSubscribers.Store(sub.Key, sub)
	if !api.eventSubscriptionCurrentForDispatch(sub.Key, sub, nil, sub.RemoteCSeq, time.Now()) {
		t.Fatal("current local subscription did not match its captured local dispatch identity")
	}
	if _, err := api.sendDirectEventNotifyContextExpected(t.Context(), sub, "Alarm", nil,
		&eventNotifyDispatchExpectation{dialogCSeq: sub.RemoteCSeq - 1}); !errors.Is(err, errStaleEventNotifyDispatch) {
		t.Fatalf("stale local dialog snapshot error = %v; want %v", err, errStaleEventNotifyDispatch)
	}

	worker := &cascadeWorker{}
	sub.mu.Lock()
	sub.Cascade = worker
	sub.mu.Unlock()
	if api.eventSubscriptionCurrentForDispatch(sub.Key, sub, nil, sub.RemoteCSeq, time.Now()) {
		t.Fatal("queued local dispatch matched a subscription that changed to cascade")
	}
	if !api.eventSubscriptionCurrentForDispatch(sub.Key, sub, worker, sub.RemoteCSeq, time.Now()) {
		t.Fatal("current cascade subscription did not match its captured worker")
	}
	if _, err := api.sendCascadeEventNotifyRequestContextExpected(t.Context(), sub, "Alarm", nil,
		&eventNotifyDispatchExpectation{cascade: worker, dialogCSeq: sub.RemoteCSeq - 1}); !errors.Is(err, errStaleEventNotifyDispatch) {
		t.Fatalf("stale cascade dialog snapshot error = %v; want %v", err, errStaleEventNotifyDispatch)
	}
	if api.eventSubscriptionCurrentForDispatch(sub.Key, sub, &cascadeWorker{}, sub.RemoteCSeq, time.Now()) {
		t.Fatal("queued cascade dispatch matched a replacement worker")
	}
}

func TestEventNotifyConstructionFailureDoesNotConsumeCSeq(t *testing.T) {
	t.Run("direct invalid dialog", func(t *testing.T) {
		worker := newCascadeWorker(&Server{}, testSharedCascadePlatform(t))
		t.Cleanup(worker.cancel)
		sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
		sub.Cascade = nil
		sub.CSeq = 7
		sub.Response.RemoveHeader("From")

		if _, err := (&GB28181API{}).sendDirectEventNotifyContextExpected(t.Context(), sub, "Alarm", nil, nil); err == nil {
			t.Fatal("invalid direct subscription dialog was accepted")
		}
		if sub.CSeq != 7 {
			t.Fatalf("invalid direct subscription dialog consumed CSeq: got %d, want 7", sub.CSeq)
		}
	})

	t.Run("direct clone panic", func(t *testing.T) {
		worker := newCascadeWorker(&Server{}, testSharedCascadePlatform(t))
		t.Cleanup(worker.cancel)
		sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
		sub.Cascade = nil
		sub.CSeq = 11
		sub.DialogRequest.AppendHeader(eventNotifyPanickingCloneHeader{})

		if _, err := (&GB28181API{}).sendDirectEventNotifyContextExpected(t.Context(), sub, "Alarm", nil, nil); err == nil {
			t.Fatal("panicking direct subscription dialog clone was accepted")
		}
		if sub.CSeq != 11 {
			t.Fatalf("panicking direct subscription dialog clone consumed CSeq: got %d, want 11", sub.CSeq)
		}
	})

	t.Run("cascade identity", func(t *testing.T) {
		worker := newCascadeWorker(&Server{}, testSharedCascadePlatform(t))
		t.Cleanup(worker.cancel)
		policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
		if err != nil {
			t.Fatal(err)
		}
		worker.platform.monitorUserIdentity = policy
		var exchanged atomic.Bool
		worker.exchange = func(context.Context, *sip.Request) (*sip.Response, error) {
			exchanged.Store(true)
			return nil, errors.New("unexpected cascade exchange")
		}
		sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
		sub.CSeq = 13
		sub.Identity = &monitorUserIdentity{
			Gateways:     []string{testTrustedGatewayID},
			UserID:       testRemoteUserID,
			Organization: "remoteorg",
			Category:     "dispatcher",
			Rank:         "level2",
		}

		if _, err := (&GB28181API{}).sendCascadeEventNotifyRequestContextExpected(t.Context(), sub, "Alarm", nil, nil); err == nil {
			t.Fatal("invalid cascade Monitor-User-Identity was accepted")
		}
		if exchanged.Load() {
			t.Fatal("invalid cascade Monitor-User-Identity reached the network exchange")
		}
		if sub.CSeq != 13 {
			t.Fatalf("invalid cascade Monitor-User-Identity consumed CSeq: got %d, want 13", sub.CSeq)
		}
	})
}

func TestCanceledEventNotifyDoesNotConsumeCSeq(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	worker := newCascadeWorker(&Server{}, testSharedCascadePlatform(t))
	t.Cleanup(worker.cancel)
	for _, cascade := range []bool{false, true} {
		name := "direct"
		if cascade {
			name = "cascade"
		}
		t.Run(name, func(t *testing.T) {
			sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
			sub.CSeq = 17
			var err error
			if cascade {
				_, err = (&GB28181API{}).sendCascadeEventNotifyRequestContextExpected(ctx, sub, "Alarm", nil, nil)
			} else {
				sub.Cascade = nil
				_, err = (&GB28181API{}).sendDirectEventNotifyContextExpected(ctx, sub, "Alarm", nil, nil)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled %s NOTIFY error = %v, want context.Canceled", name, err)
			}
			if sub.CSeq != 17 {
				t.Fatalf("canceled %s NOTIFY consumed CSeq: got %d, want 17", name, sub.CSeq)
			}
		})
	}
}

func TestFailedEventNotifyDoesNotDeleteRenewedSubscriptionFourVersionMatrix(t *testing.T) {
	versions := []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30}
	for _, version := range versions {
		t.Run(string(version), func(t *testing.T) {
			for _, dispatch := range []string{"event", "current", "cascade"} {
				t.Run(dispatch, func(t *testing.T) {
					worker := newCascadeWorker(&Server{}, testSharedCascadePlatform(t))
					t.Cleanup(worker.cancel)
					started := make(chan struct{})
					release := make(chan struct{})
					worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
						close(started)
						select {
						case <-ctx.Done():
							return nil, ctx.Err()
						case <-release:
							return sip.NewResponseFromRequest("", request, 481, "Call/Transaction Does Not Exist", nil), nil
						}
					}

					sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
					sub.Key = "renewed-notify-failure-" + string(version) + "-" + dispatch
					sub.GBVersion = string(version)
					sub.RemoteCSeq = 1
					api := &GB28181API{}
					api.eventSubscribers.Store(sub.Key, sub)

					done := make(chan error, 1)
					go func() {
						switch dispatch {
						case "current":
							_, err := api.sendCurrentEventNotifyContext(t.Context(), sub.Key, sub, worker, 1, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
							done <- err
						case "cascade":
							done <- api.sendCascadeEventNotifyContext(t.Context(), sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
						default:
							done <- api.sendEventNotifyContext(t.Context(), sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
						}
					}()
					select {
					case <-started:
					case <-time.After(time.Second):
						t.Fatal("event NOTIFY did not start")
					}

					unlock, err := api.lockEventSubscriptionOperation(t.Context(), sub.Key)
					if err != nil {
						t.Fatal(err)
					}
					sub.mu.Lock()
					sub.RemoteCSeq = 2
					renewedExpiresAt := time.Now().Add(2 * time.Hour)
					sub.ExpiresAt = renewedExpiresAt
					sub.mu.Unlock()
					close(release)
					waitForEventSubscriptionLockWaiter(t, api, sub.Key)
					unlock()

					select {
					case err = <-done:
						if err == nil {
							t.Fatal("481 NOTIFY response unexpectedly succeeded")
						}
					case <-time.After(time.Second):
						t.Fatal("failed event NOTIFY did not finish")
					}
					if actual, exists := api.eventSubscribers.Load(sub.Key); !exists || actual != sub {
						t.Fatal("old NOTIFY failure deleted the renewed subscription")
					}
					sub.mu.Lock()
					remoteCSeq := sub.RemoteCSeq
					expiresAt := sub.ExpiresAt
					sub.mu.Unlock()
					if remoteCSeq != 2 || !expiresAt.Equal(renewedExpiresAt) {
						t.Fatalf("renewed subscription state changed: RemoteCSeq=%d ExpiresAt=%v", remoteCSeq, expiresAt)
					}
				})
			}
		})
	}
}

func queuedEventAlarmBody(sn int) []byte {
	return []byte(fmt.Sprintf(`<Notify><CmdType>Alarm</CmdType><SN>%d</SN><DeviceID>%s</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-30T10:00:00</AlarmTime></Notify>`, sn, testCascadeChannelID))
}

func TestOfflineDeviceCancelsOwnedInboundEventNotify(t *testing.T) {
	baseConn := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConn}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api

	subscribe := newFlowRequest(t, baseConn, sip.MethodSubscribe, "device-notify-offline", []byte("query"))
	contact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@contact.example:5070")
	response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
	sub := &eventSubscription{
		Key: "device-notify-offline", CmdType: "Alarm", DeviceID: gb10ChannelID, OwnerDeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute), To: contact, Source: baseConn.remote, Conn: connection,
		GBVersion: string(GBVersion10), Event: "presence", DialogRequest: subscribe, Response: response,
	}
	api.eventSubscribers.Store(sub.Key, sub)

	done := make(chan error, 1)
	go func() {
		done <- api.sendEventNotifyContext(t.Context(), sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10ChannelID+`</DeviceID></Notify>`))
	}()
	select {
	case <-baseConn.writes:
	case err := <-done:
		t.Fatalf("event NOTIFY ended before device cleanup: %v", err)
	case <-time.After(time.Second):
		t.Fatal("event NOTIFY was not sent")
	}

	api.cleanupOfflineDeviceRuntime(gb10DeviceID)
	select {
	case err := <-done:
		if !errors.Is(err, ErrDeviceOffline) {
			t.Fatalf("offline event NOTIFY error = %v; want %v", err, ErrDeviceOffline)
		}
	case <-time.After(time.Second):
		t.Fatal("offline device event NOTIFY did not stop")
	}
	if _, exists := api.eventSubscribers.Load(sub.Key); exists {
		t.Fatal("offline device inbound event subscription survived cleanup")
	}
}

func TestDeviceEventNotifyResponseUpdatesSubscriptionLifecycle(t *testing.T) {
	tests := []struct {
		name        string
		version     GBProtocolVersion
		status      int
		extra       string
		wantErr     bool
		wantRemoved bool
	}{
		{name: "accepted 202", version: GBVersion10, status: http.StatusAccepted},
		{name: "2016 dialog missing 481", version: GBVersion20, status: 481, extra: "Retry-After: 30\r\n", wantErr: true, wantRemoved: true},
		{name: "2016 server error 500", version: GBVersion20, status: http.StatusInternalServerError, wantErr: true, wantRemoved: true},
		{name: "2016 service unavailable with retry", version: GBVersion20, status: http.StatusServiceUnavailable, extra: "Retry-After: 30 (maintenance);duration=60\r\n", wantErr: true},
		{name: "2016 unauthorized", version: GBVersion20, status: http.StatusUnauthorized, wantErr: true},
		{name: "2016 malformed retry", version: GBVersion20, status: http.StatusServiceUnavailable, extra: "Retry-After: later\r\n", wantErr: true, wantRemoved: true},
		{name: "2016 duplicate retry", version: GBVersion20, status: http.StatusServiceUnavailable, extra: "Retry-After: 30\r\nRetry-After: 60\r\n", wantErr: true, wantRemoved: true},
		{name: "2022 bad event 489", version: GBVersion30, status: 489, wantErr: true, wantRemoved: true},
		{name: "2022 server error 500", version: GBVersion30, status: http.StatusInternalServerError, wantErr: true},
		{name: "2022 service unavailable 503", version: GBVersion30, status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			sipServer := sip.NewServer(platform)
			server := &Server{Server: sipServer, fromAddress: *platform}
			api := &GB28181API{svr: server}
			server.gb = api

			localRaw, remoteRaw := net.Pipe()
			connection := sip.NewTCPConnection(localRaw)
			go sipServer.ProcessTCPConnection(connection)
			defer func() {
				_ = remoteRaw.Close()
				sipServer.Close()
			}()

			remoteErr := make(chan error, 1)
			go func() {
				_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
				request, err := readAnnexGTestSIPFrame(bufio.NewReader(remoteRaw))
				if err == nil {
					_, err = remoteRaw.Write([]byte(annexGTestSIPResponse(request, tt.status, http.StatusText(tt.status), tt.extra)))
				}
				remoteErr <- err
			}()

			remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@remote.example")
			callID := sip.CallID("device-notify-response-" + strings.ReplaceAll(tt.name, " ", "-"))
			subscribe := sip.NewRequest("", sip.MethodSubscribe, platform.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(platform).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "remote.example", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
			sub := &eventSubscription{
				Key: "device-notify-response-" + tt.name, CmdType: "Alarm", DeviceID: gb10DeviceID,
				ExpiresAt: time.Now().Add(time.Minute), To: remote, Source: connection.RemoteAddr(), Conn: connection,
				GBVersion: string(tt.version), Event: "presence", DialogRequest: subscribe, Response: response,
			}
			wantExpiresAt := sub.ExpiresAt
			wantDialogRequest := sub.DialogRequest
			wantResponse := sub.Response
			api.eventSubscribers.Store(sub.Key, sub)

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err := api.sendEventNotifyContext(ctx, sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
			if (err != nil) != tt.wantErr {
				t.Fatalf("NOTIFY error = %v, wantErr %v", err, tt.wantErr)
			}
			if err := <-remoteErr; err != nil {
				t.Fatal(err)
			}
			actual, exists := api.eventSubscribers.Load(sub.Key)
			if tt.wantRemoved {
				if exists {
					t.Fatalf("failed NOTIFY retained subscription: %T", actual)
				}
			} else if !exists || actual != sub {
				t.Fatal("NOTIFY response removed a subscription that must be retained")
			} else if !sub.ExpiresAt.Equal(wantExpiresAt) || sub.DialogRequest != wantDialogRequest || sub.Response != wantResponse {
				t.Fatal("retained subscription state changed after NOTIFY response")
			}
		})
	}
}

func TestValidRetryAfterValue(t *testing.T) {
	tests := []struct {
		value string
		valid bool
	}{
		{value: "0", valid: true},
		{value: "120 (maintenance)", valid: true},
		{value: `120 (nested (detail));duration=60;reason="planned"`, valid: true},
		{value: "120;vendor", valid: true},
		{value: ""},
		{value: "-1"},
		{value: "later"},
		{value: "30x"},
		{value: "30 (unterminated"},
		{value: "30;duration"},
		{value: "30;duration=soon"},
		{value: "30;"},
		{value: `30;reason="unterminated`},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			if actual := validRetryAfterValue(test.value); actual != test.valid {
				t.Fatalf("validRetryAfterValue(%q) = %v; want %v", test.value, actual, test.valid)
			}
		})
	}
}

func TestCatalogSubscriptionEventValue11(t *testing.T) {
	value := buildSubscriptionEventValue("Catalog", "1894")
	if value != "Catalog;id=1894" {
		t.Fatalf("Catalog Event = %q", value)
	}
	parsed, id, err := parseSubscriptionEvent(value)
	if err != nil || parsed != value || id != "1894" {
		t.Fatalf("parseSubscriptionEvent() = %q, %q, %v", parsed, id, err)
	}
}

func TestCatalogSubscriptionEventValueByVersion(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		want    string
	}{
		{version: GBVersion10, want: "presence"},
		{version: GBVersion11, want: "Catalog;id=1894"},
		{version: GBVersion20, want: "Catalog;id=1894"},
		{version: GBVersion30, want: "Catalog;id=1894"},
	}

	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			if got := buildSubscriptionEventValueForVersion(tt.version, "Catalog", "1894"); got != tt.want {
				t.Fatalf("Catalog Event = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestOutgoingDeviceCatalogSubscribeUsesPresence(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			baseConnection := newFlowConnection()
			connection := &tcpFlowConnection{flowConnection: baseConnection}
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
			sipServer := sip.NewServer(platform)
			t.Cleanup(sipServer.Close)
			runtime := &Device{IsOnline: true, gbVersion: string(version)}
			runtime.UpdateRuntime(func(current *Device) {
				current.conn = connection
				current.source = baseConnection.remote
				current.to = device
			})
			server := &Server{
				Server: sipServer, fromAddress: *platform,
				memoryStorer: &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime},
			}
			api := &GB28181API{svr: server}
			server.gb = api
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- api.Subscribe(ctx, &SubscribeInput{DeviceID: gb10DeviceID, Event: "Catalog", Expires: 600})
			}()

			var payload string
			select {
			case raw := <-baseConnection.writes:
				payload = string(raw)
			case <-time.After(time.Second):
				t.Fatal("Catalog SUBSCRIBE was not sent")
			}
			if event := cascadeTestHeader(payload, "Event"); event != "presence" {
				t.Fatalf("Catalog Event = %q; want presence for device subscription", event)
			}
			cancel()
			if err := <-done; err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
				t.Fatalf("cancelled Catalog SUBSCRIBE error = %v", err)
			}
		})
	}
}

func TestOutgoingCatalogSubscribePreservesTimeRangeAcrossDialog(t *testing.T) {
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(platform)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api

	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = remoteRaw.Close()
		sipServer.Close()
	})
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	runtime.UpdateRuntime(func(current *Device) {
		current.conn = connection
		current.source = connection.RemoteAddr()
		current.to = device
	})
	server.memoryStorer = &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}

	requests := make(chan []string, 1)
	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(remoteRaw)
		captured := make([]string, 0, 3)
		for index := 0; index < 3; index++ {
			request, err := readAnnexGTestSIPFrame(reader)
			if err != nil {
				remoteErr <- err
				return
			}
			captured = append(captured, request)
			extra := ""
			if index < 2 {
				extra = "Expires: 60"
			}
			response := annexGTestSIPResponse(request, http.StatusOK, "OK", extra)
			if index == 0 {
				to := annexGTestSIPHeader(request, "To")
				response = strings.Replace(response, "To: "+to+"\r\n", "To: "+to+";tag=catalog-time-range\r\n", 1)
			}
			if _, err = remoteRaw.Write([]byte(response)); err != nil {
				remoteErr <- err
				return
			}
		}
		requests <- captured
		remoteErr <- nil
	}()

	input := &SubscribeInput{
		DeviceID:  gb10DeviceID,
		Event:     "Catalog",
		Expires:   60,
		StartTime: "2026-08-01T00:00:00",
		EndTime:   "2026-08-31T23:59:59",
	}
	if err := api.Subscribe(t.Context(), input); err != nil {
		t.Fatalf("initial Catalog SUBSCRIBE failed: %v", err)
	}
	if err := api.Subscribe(t.Context(), input); err != nil {
		t.Fatalf("refresh Catalog SUBSCRIBE failed: %v", err)
	}
	cancel := *input
	cancel.Cancel = true
	if err := api.Subscribe(t.Context(), &cancel); err != nil {
		t.Fatalf("cancel Catalog SUBSCRIBE failed: %v", err)
	}
	if err := <-remoteErr; err != nil {
		t.Fatal(err)
	}

	captured := <-requests
	if len(captured) != 3 {
		t.Fatalf("captured %d SUBSCRIBE requests; want 3", len(captured))
	}
	var event string
	for index, request := range captured {
		if !strings.Contains(request, "<CmdType>Catalog</CmdType>") ||
			!strings.Contains(request, "<StartTime>2026-08-01T00:00:00</StartTime>") ||
			!strings.Contains(request, "<EndTime>2026-08-31T23:59:59</EndTime>") {
			t.Fatalf("Catalog SUBSCRIBE %d lost its time range:\n%s", index, request)
		}
		if strings.Contains(request, "StartAlarmTime") || strings.Contains(request, "EndAlarmTime") {
			t.Fatalf("Catalog SUBSCRIBE %d encoded Alarm time fields:\n%s", index, request)
		}
		currentEvent := annexGTestSIPHeader(request, "Event")
		if index == 0 {
			event = currentEvent
		} else if currentEvent != event {
			t.Fatalf("Catalog Event changed across dialog: %q then %q", event, currentEvent)
		}
		wantExpires := "60"
		if index == 2 {
			wantExpires = "0"
		}
		if expires := annexGTestSIPHeader(request, "Expires"); expires != wantExpires {
			t.Fatalf("Catalog SUBSCRIBE %d Expires = %q; want %q", index, expires, wantExpires)
		}
	}
}

func TestOutgoingSubscribeAcceptsAllSuccessfulResponses(t *testing.T) {
	for _, status := range []int{http.StatusAccepted, http.StatusNoContent} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
			sipServer := sip.NewServer(platform)
			server := &Server{Server: sipServer, fromAddress: *platform}
			api := &GB28181API{svr: server}
			server.gb = api

			localRaw, remoteRaw := net.Pipe()
			connection := sip.NewTCPConnection(localRaw)
			go sipServer.ProcessTCPConnection(connection)
			t.Cleanup(func() {
				_ = remoteRaw.Close()
				sipServer.Close()
			})
			runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
			runtime.UpdateRuntime(func(current *Device) {
				current.conn = connection
				current.source = connection.RemoteAddr()
				current.to = device
			})
			server.memoryStorer = &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}

			remoteErr := make(chan error, 1)
			go func() {
				_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
				request, err := readAnnexGTestSIPFrame(bufio.NewReader(remoteRaw))
				if err == nil {
					to := annexGTestSIPHeader(request, "To")
					response := annexGTestSIPResponse(request, status, http.StatusText(status), "Expires: 60")
					response = strings.Replace(response, "To: "+to+"\r\n", "To: "+to+";tag=successful-subscribe\r\n", 1)
					_, err = remoteRaw.Write([]byte(response))
				}
				remoteErr <- err
			}()

			if err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60}); err != nil {
				t.Fatalf("SUBSCRIBE %d response rejected: %v", status, err)
			}
			if err := <-remoteErr; err != nil {
				t.Fatal(err)
			}
			stored := false
			api.outgoingSubscriptions.Range(func(_, value any) bool {
				dialog, _ := value.(*outgoingSubscriptionDialog)
				stored = dialog != nil && dialog.response != nil && dialog.response.StatusCode() == status &&
					dialog.autoRefresh && dialog.refreshInput.DeviceID == gb10DeviceID &&
					dialog.refreshInput.Event == "alarm" && dialog.refreshInput.Expires == 60
				return false
			})
			if !stored {
				t.Fatalf("successful SUBSCRIBE %d response did not create a dialog", status)
			}
		})
	}
}

func TestOutgoingSubscribeSeparatesVerifiedIdentityDialogs(t *testing.T) {
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(platform)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api

	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = remoteRaw.Close()
		sipServer.Close()
	})
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtime.UpdateRuntime(func(current *Device) {
		current.conn = connection
		current.source = connection.RemoteAddr()
		current.to = device
	})
	server.memoryStorer = &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}

	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(remoteRaw)
		for index := range 2 {
			request, err := readAnnexGTestSIPFrame(reader)
			if err != nil {
				remoteErr <- err
				return
			}
			to := annexGTestSIPHeader(request, "To")
			response := annexGTestSIPResponse(request, http.StatusOK, "OK", "Expires: 60")
			response = strings.Replace(response, "To: "+to+"\r\n", fmt.Sprintf("To: %s;tag=identity-%d\r\n", to, index+1), 1)
			if _, err = remoteRaw.Write([]byte(response)); err != nil {
				remoteErr <- err
				return
			}
		}
		remoteErr <- nil
	}()

	first := &monitorUserIdentity{
		Gateways: []string{testRemoteGatewayID}, UserID: testRemoteUserID,
		Organization: "remoteorg", Category: "dispatcher", Rank: "level2",
	}
	second := first.clone()
	second.UserID = "34030000003000000002"
	input := &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60}
	firstCtx := withMonitorUserIdentityRoute(t.Context(), first, testLocalGatewayID)
	secondCtx := withMonitorUserIdentityRoute(t.Context(), second, testLocalGatewayID)
	if err := api.Subscribe(firstCtx, input); err != nil {
		t.Fatal(err)
	}
	if err := api.Subscribe(secondCtx, input); err != nil {
		t.Fatal(err)
	}
	if err := <-remoteErr; err != nil {
		t.Fatal(err)
	}

	baseKey := buildOutgoingSubscriptionKey(gb10DeviceID, gb10DeviceID, "Alarm", input)
	firstValue, firstOK := api.outgoingSubscriptions.Load(baseKey + monitorUserIdentitySubscriptionKey(firstCtx))
	secondValue, secondOK := api.outgoingSubscriptions.Load(baseKey + monitorUserIdentitySubscriptionKey(secondCtx))
	if !firstOK || !secondOK || firstValue == secondValue {
		t.Fatalf("identity subscription dialogs = first:%v second:%v same:%v", firstOK, secondOK, firstValue == secondValue)
	}
	firstDialog, _ := firstValue.(*outgoingSubscriptionDialog)
	secondDialog, _ := secondValue.(*outgoingSubscriptionDialog)
	if firstDialog == nil || firstDialog.identity == nil || firstDialog.identity.UserID != first.UserID ||
		secondDialog == nil || secondDialog.identity == nil || secondDialog.identity.UserID != second.UserID {
		t.Fatalf("identity subscription ownership = first:%+v second:%+v", firstDialog, secondDialog)
	}
}

func TestOutgoingSubscribeRefreshFailureTerminatesDialogByVersion(t *testing.T) {
	tests := []struct {
		name        string
		version     GBProtocolVersion
		status      int
		wantRemoved bool
	}{
		{name: "2016 481", version: GBVersion20, status: 481, wantRemoved: true},
		{name: "2016 489", version: GBVersion20, status: 489},
		{name: "2022 489", version: GBVersion30, status: 489, wantRemoved: true},
		{name: "2022 500", version: GBVersion30, status: 500},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
			sipServer := sip.NewServer(platform)
			server := &Server{Server: sipServer, fromAddress: *platform}
			api := &GB28181API{svr: server}
			server.gb = api

			localRaw, remoteRaw := net.Pipe()
			connection := sip.NewTCPConnection(localRaw)
			go sipServer.ProcessTCPConnection(connection)
			t.Cleanup(func() {
				_ = remoteRaw.Close()
				sipServer.Close()
			})
			runtime := &Device{IsOnline: true, gbVersion: string(test.version)}
			runtime.UpdateRuntime(func(current *Device) {
				current.conn = connection
				current.source = connection.RemoteAddr()
				current.to = device
			})
			server.memoryStorer = &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}

			remoteErr := make(chan error, 1)
			go func() {
				_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(remoteRaw)
				for index, status := range []int{http.StatusOK, test.status} {
					request, err := readAnnexGTestSIPFrame(reader)
					if err != nil {
						remoteErr <- err
						return
					}
					extra := ""
					if status >= 200 && status < 300 {
						extra = "Expires: 60"
					}
					response := annexGTestSIPResponse(request, status, http.StatusText(status), extra)
					if index == 0 {
						to := annexGTestSIPHeader(request, "To")
						response = strings.Replace(response, "To: "+to+"\r\n", "To: "+to+";tag=refresh-device\r\n", 1)
					}
					if _, err = remoteRaw.Write([]byte(response)); err != nil {
						remoteErr <- err
						return
					}
				}
				remoteErr <- nil
			}()

			input := &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60}
			if err := api.Subscribe(t.Context(), input); err != nil {
				t.Fatalf("initial SUBSCRIBE failed: %v", err)
			}
			if err := api.Subscribe(t.Context(), input); err == nil {
				t.Fatalf("refresh SUBSCRIBE %d unexpectedly succeeded", test.status)
			}
			if err := <-remoteErr; err != nil {
				t.Fatal(err)
			}
			stored := false
			api.outgoingSubscriptions.Range(func(_, _ any) bool {
				stored = true
				return false
			})
			if stored == test.wantRemoved {
				t.Fatalf("refresh status %d stored=%v, wantRemoved=%v", test.status, stored, test.wantRemoved)
			}
		})
	}
}

func TestBasicSubscriptionEventValueUsesPresence(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, cmdType := range []string{"Alarm", "MobilePosition", "PTZPosition"} {
			if got := buildSubscriptionEventValueForVersion(version, cmdType, "1894"); got != "presence" {
				t.Fatalf("version %s %s Event = %q; want presence", version, cmdType, got)
			}
		}
	}
}

func TestSubscriptionEventHeaderMustMatchBody(t *testing.T) {
	if err := validateSubscriptionEventHeader("presence", "Alarm", ""); err != nil {
		t.Fatalf("standard Alarm presence Event rejected: %v", err)
	}
	if err := validateSubscriptionEventHeader("Catalog;id=1894", "Catalog", "1894"); err != nil {
		t.Fatalf("standard Catalog Event rejected: %v", err)
	}
	if err := validateSubscriptionEventHeader("Catalog;id=1894", "Alarm", "1894"); err == nil {
		t.Fatal("Catalog Event accepted for Alarm subscription body")
	}
	if err := validateSubscriptionEventHeader("Alarm;id=1894", "Alarm", "1894"); err == nil {
		t.Fatal("non-Catalog subscription accepted Event id")
	}
}

func TestSubscriptionEventDialogMatchingIsByteExact(t *testing.T) {
	for _, test := range []struct {
		name     string
		expected string
		actual   string
		want     bool
	}{
		{name: "same presence", expected: "presence", actual: "presence", want: true},
		{name: "generic parameters ignored", expected: "presence;vendor=one", actual: "presence;vendor=two", want: true},
		{name: "event type case differs", expected: "presence", actual: "Presence"},
		{name: "same catalog id", expected: "Catalog;id=AbC", actual: "Catalog;id=AbC", want: true},
		{name: "catalog id case differs", expected: "Catalog;id=AbC", actual: "Catalog;id=abc"},
		{name: "catalog id missing", expected: "Catalog;id=1894", actual: "Catalog"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := subscriptionEventHeadersMatch(test.expected, test.actual); got != test.want {
				t.Fatalf("subscriptionEventHeadersMatch(%q, %q) = %v, want %v", test.expected, test.actual, got, test.want)
			}
		})
	}
}

func TestInterdomainCatalogEventHeaderFormat(t *testing.T) {
	valid := "Catalog;id=1894"
	if err := validateInterdomainCatalogEventHeader(valid); err != nil {
		t.Fatalf("standard interdomain Catalog Event rejected: %v", err)
	}
	if err := validateInterdomainCatalogEventHeader("Catalog;id=44010000002000000001"); err != nil {
		t.Fatalf("numeric Event id independent from DeviceID rejected: %v", err)
	}
	tests := []struct {
		name  string
		value string
	}{
		{name: "legacy presence", value: "presence"},
		{name: "missing id", value: "Catalog"},
		{name: "extra parameter", value: valid + ";foo=bar"},
		{name: "wrong parameter", value: "Catalog;target=1894"},
		{name: "non numeric", value: "Catalog;id=system-root"},
		{name: "quoted id", value: `Catalog;id="1894"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateInterdomainCatalogEventHeader(test.value); err == nil {
				t.Fatalf("invalid Event %q accepted", test.value)
			}
		})
	}
}

func TestNormalizeSubscribeCmdType(t *testing.T) {
	tests := map[string]string{
		"alarm":           "Alarm",
		"mobile_position": "MobilePosition",
		"device-position": "MobilePosition",
		"ptz_position":    "PTZPosition",
		"Catalog":         "Catalog",
	}
	for input, want := range tests {
		got, ok := normalizeSubscribeCmdType(input)
		if !ok || got != want {
			t.Fatalf("normalizeSubscribeCmdType(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if got, ok := normalizeSubscribeCmdType("Alarm\r\nX-Injected: true"); ok || got != "" {
		t.Fatalf("header injection event accepted: %q, %v", got, ok)
	}
	for _, unsupported := range []string{
		"DeviceInfo", "DeviceStatus", "RecordInfo", "ConfigDownload", "PresetQuery",
		"HomePositionQuery", "CruiseTrackListQuery", "CruiseTrackQuery", "SDCardStatus", "Broadcast", "VendorStatus",
	} {
		if got, ok := normalizeSubscribeCmdType(unsupported); ok || got != "" {
			t.Errorf("non-standard subscription %q accepted as %q", unsupported, got)
		}
	}
}

func TestParseSubscribeExpiresRejectsValuesOutsideSIPRange(t *testing.T) {
	if got, err := parseSubscribeExpires("4294967295"); err != nil || got != int(^uint32(0)) {
		t.Fatalf("maximum SIP Expires was rejected: got %d, err %v", got, err)
	}
	for _, value := range []string{"4294967296", "18446744073709551615"} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseSubscribeExpires(value); err == nil {
				t.Fatalf("out-of-range SIP Expires %q was accepted", value)
			}
		})
	}
}

func TestEventSubscriptionVersionMatrix(t *testing.T) {
	for _, test := range []struct {
		cmdType string
		version GBProtocolVersion
		wantOK  bool
	}{
		{cmdType: "Alarm", version: GBVersion10, wantOK: true},
		{cmdType: "Catalog", version: GBVersion10, wantOK: true},
		{cmdType: "MobilePosition", version: GBVersion11},
		{cmdType: "MobilePosition", version: GBVersion20, wantOK: true},
		{cmdType: "PTZPosition", version: GBVersion20},
		{cmdType: "PTZPosition", version: GBVersion30, wantOK: true},
		{cmdType: "DeviceStatus", version: GBVersion30},
	} {
		t.Run(string(test.version)+"-"+test.cmdType, func(t *testing.T) {
			err := validateSubscribeEventRequest(subscribeEventRequest{SN: 1, DeviceID: gb10DeviceID}, test.cmdType, test.version)
			if test.wantOK && err != nil {
				t.Fatalf("valid subscription rejected: %v", err)
			}
			if !test.wantOK && err == nil {
				t.Fatal("invalid subscription accepted")
			}
		})
	}
}

func TestInboundSubscribeUsesRegisteredDeviceVersion(t *testing.T) {
	for _, test := range []struct {
		name          string
		runtime       GBProtocolVersion
		header        GBProtocolVersion
		cmdType       string
		wantStatus    string
		wantStored    bool
		wantLegacyXML bool
	}{
		{name: "2011 rejects spoofed 2022 PTZPosition", runtime: GBVersion10, header: GBVersion30, cmdType: "PTZPosition", wantStatus: "SIP/2.0 400"},
		{name: "2014 rejects spoofed 2022 MobilePosition", runtime: GBVersion11, header: GBVersion30, cmdType: "MobilePosition", wantStatus: "SIP/2.0 400"},
		{name: "2016 rejects spoofed 2022 PTZPosition", runtime: GBVersion20, header: GBVersion30, cmdType: "PTZPosition", wantStatus: "SIP/2.0 400"},
		{name: "2022 ignores downgraded header", runtime: GBVersion30, header: GBVersion10, cmdType: "PTZPosition", wantStatus: "SIP/2.0 200", wantStored: true},
		{name: "2011 Alarm keeps legacy profile", runtime: GBVersion10, header: GBVersion30, cmdType: "Alarm", wantStatus: "SIP/2.0 200", wantStored: true, wantLegacyXML: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.runtime)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			connection := newFlowConnection()
			body := []byte(`<?xml version="1.0"?><Query><CmdType>` + test.cmdType + `</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
			request := newFlowRequest(t, connection, sip.MethodSubscribe, "registered-version-"+strings.ReplaceAll(test.name, " ", "-"), body)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("registered-version-"+test.name, connection), DeviceID: gb10DeviceID,
				Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(test.header),
			}

			api.sipSubscribeEvent(ctx)
			response := <-flowResponse(t, connection)
			if !strings.Contains(response, test.wantStatus) {
				t.Fatalf("SUBSCRIBE response = %s; want %s", response, test.wantStatus)
			}
			if got := strings.Contains(response, "<Response>"); got != test.wantLegacyXML {
				t.Fatalf("legacy response body = %v, want %v:\n%s", got, test.wantLegacyXML, response)
			}
			var stored *eventSubscription
			api.eventSubscribers.Range(func(_, value any) bool {
				stored, _ = value.(*eventSubscription)
				return false
			})
			if (stored != nil) != test.wantStored {
				t.Fatalf("stored subscription = %v, wantStored %v", stored, test.wantStored)
			}
			if stored != nil && stored.GBVersion != string(test.runtime) {
				t.Fatalf("stored version = %q; want registered %q", stored.GBVersion, test.runtime)
			}
		})
	}
}

func TestEventSubscriptionRequiresEnvelopeOnCreateAndCancel(t *testing.T) {
	for _, expires := range []string{"90", "0"} {
		for _, body := range []string{
			`<Query><CmdType>Alarm</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`,
			`<Query><CmdType>Alarm</CmdType><SN>1</SN></Query>`,
			`<Query><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>invalid</DeviceID></Query>`,
		} {
			api := &GB28181API{}
			conn := newFlowConnection()
			req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-invalid-envelope-"+expires, []byte(body))
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
			ctx := &sip.Context{
				Request: req, Tx: sip.NewTransaction("subscribe-invalid-envelope-tx-"+expires, conn), DeviceID: gb10PlatformID,
				Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion30),
			}
			api.sipSubscribeEvent(ctx)
			if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("Expires %s invalid envelope response:\n%s", expires, response)
			}
		}
	}
}

func TestEventSubscriptionRejectsMalformedXMLBeforeStateChange(t *testing.T) {
	base := `<Query><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: strings.Replace(base, "</Query>", "<Vendor>1</Vendor></Query>", 1)},
		{name: "duplicate device", body: strings.Replace(base, "</Query>", "<DeviceID>"+gb10DeviceID+"</DeviceID></Query>", 1)},
		{name: "root attribute", body: strings.Replace(base, "<Query>", `<Query vendor="1">`, 1)},
		{name: "root namespace", body: strings.Replace(strings.Replace(base, "<Query>", `<gb:Query xmlns:gb="urn:vendor">`, 1), "</Query>", "</gb:Query>", 1)},
		{name: "simple field attribute", body: strings.Replace(base, "<DeviceID>", `<DeviceID vendor="1">`, 1)},
		{name: "simple field nesting", body: strings.Replace(base, gb10DeviceID, "<Value>"+gb10DeviceID+"</Value>", 1)},
		{name: "out of order", body: `<Query><CmdType>Alarm</CmdType><DeviceID>` + gb10DeviceID + `</DeviceID><SN>1</SN></Query>`},
	}
	for _, expires := range []string{"90", "0"} {
		for _, test := range tests {
			t.Run(expires+"/"+test.name, func(t *testing.T) {
				api := &GB28181API{}
				conn := newFlowConnection()
				request := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-malformed-"+expires+"-"+test.name, []byte(test.body))
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
				ctx := &sip.Context{
					Request: request, Tx: sip.NewTransaction("subscribe-malformed-"+expires+"-"+test.name, conn), DeviceID: gb10PlatformID,
					Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion30),
				}
				api.sipSubscribeEvent(ctx)
				if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 400") || strings.Contains(response, "SIP/2.0 200") {
					t.Fatalf("malformed SUBSCRIBE response:\n%s", response)
				}
				stored := false
				api.eventSubscribers.Range(func(_, _ any) bool {
					stored = true
					return false
				})
				if stored {
					t.Fatal("malformed SUBSCRIBE changed subscription state")
				}
			})
		}
	}
}

func TestEventSubscriptionRequiresEventHeader(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<Query><CmdType>Catalog</CmdType><SN>53</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-missing-event", body)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
	ctx := &sip.Context{
		Request: req, Tx: sip.NewTransaction("subscribe-missing-event-tx", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
	}
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, conn)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("missing Event header response:\n%s", response)
	}
	stored := false
	api.eventSubscribers.Range(func(_, _ any) bool {
		stored = true
		return false
	})
	if stored {
		t.Fatal("subscription without Event header was stored")
	}
}

func TestEventSubscriptionRejectsNonStandardQueryCommand(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>DeviceStatus</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-non-event", body)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
	ctx := &sip.Context{
		Request: req, Tx: sip.NewTransaction("subscribe-non-event-tx", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion30),
	}
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, conn)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("non-standard subscription response:\n%s", response)
	}
	stored := false
	api.eventSubscribers.Range(func(_, _ any) bool {
		stored = true
		return false
	})
	if stored {
		t.Fatal("non-standard subscription was stored")
	}
}

func TestOutgoingSubscriptionRenewalReusesDialog(t *testing.T) {
	conn := newFlowConnection()
	initial := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-dialog", []byte("query"))
	response := sip.NewResponseFromRequest("", initial, 200, "OK", nil)
	to, ok := response.To()
	if !ok || to == nil {
		t.Fatal("initial response missing To")
	}
	if to.Params == nil {
		to.Params = sip.NewParams()
	}
	to.Params.Add("tag", sip.String{Str: "remote-tag"})
	remoteTarget := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.99:5070")
	response.AppendHeader(&sip.ContactHeader{Address: remoteTarget.URI.Clone(), Params: sip.NewParams()})
	proxy1, _ := sip.ParseURI("sip:proxy1.example;lr")
	proxy2, _ := sip.ParseURI("sip:proxy2.example;lr")
	response.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{proxy1, proxy2}})

	currentConn := newFlowConnection()
	renewal := newFlowRequest(t, currentConn, sip.MethodSubscribe, "different-dialog", []byte("query"))
	dialogRequest, err := sip.NewRequestFromResponseChecked(sip.MethodSubscribe, response)
	if err != nil {
		t.Fatal(err)
	}
	applyOutgoingSubscriptionDialog(renewal, dialogRequest)

	callID, ok := renewal.CallID()
	if !ok || normalizeCallID(callID) != "subscribe-dialog" {
		t.Fatalf("renewal Call-ID = %v", callID)
	}
	cseq, ok := renewal.CSeq()
	if !ok || cseq.SeqNo != 2 || cseq.MethodName != sip.MethodSubscribe {
		t.Fatalf("renewal CSeq = %+v", cseq)
	}
	renewalTo, ok := renewal.To()
	if !ok || renewalTo.Params == nil {
		t.Fatal("renewal missing remote To tag")
	}
	tag, ok := renewalTo.Params.Get("tag")
	if !ok || tag.String() != "remote-tag" {
		t.Fatalf("renewal To tag = %v", tag)
	}
	if renewal.Recipient().String() != remoteTarget.URI.String() {
		t.Fatalf("renewal target = %s; want %s", renewal.Recipient(), remoteTarget.URI)
	}
	route, ok := renewal.GetHeaders("Route")[0].(*sip.RouteHeader)
	if !ok || len(route.Addresses) != 2 || route.Addresses[0].Host() != "proxy2.example" || route.Addresses[1].Host() != "proxy1.example" {
		t.Fatalf("renewal Route = %#v", renewal.GetHeaders("Route"))
	}
	if renewal.GetConnection() != currentConn {
		t.Fatal("renewal replaced the current connection with the original dialog connection")
	}
}

func TestOutgoingSubscriptionCleanerDoesNotDeleteReplacement(t *testing.T) {
	api := &GB28181API{}
	now := time.Now()
	key := "outgoing-cleaner-replacement"
	expired := &outgoingSubscriptionDialog{expiresAt: now.Add(-time.Second)}
	replacement := &outgoingSubscriptionDialog{expiresAt: now.Add(time.Minute)}
	api.outgoingSubscriptions.Store(key, replacement)

	api.cleanupOutgoingSubscription(key, expired, now)
	if current, exists := api.outgoingSubscriptions.Load(key); !exists || current != replacement {
		t.Fatal("outgoing subscription cleaner deleted the replacement dialog")
	}

	api.cleanupOutgoingSubscription(key, replacement, now)
	if current, exists := api.outgoingSubscriptions.Load(key); !exists || current != replacement {
		t.Fatal("outgoing subscription cleaner deleted an active dialog")
	}

	replacement.mu.Lock()
	replacement.expiresAt = now.Add(-time.Second)
	replacement.mu.Unlock()
	api.cleanupOutgoingSubscription(key, replacement, now)
	if _, exists := api.outgoingSubscriptions.Load(key); exists {
		t.Fatal("outgoing subscription cleaner retained the expired current dialog")
	}
}

func TestNegotiatedSubscribeExpiresUsesSuccessfulResponseValue(t *testing.T) {
	tests := []struct {
		name      string
		headers   []string
		requested int
		want      int
		wantErr   bool
	}{
		{name: "omitted compatibility", requested: 600, want: 600},
		{name: "shortened", headers: []string{"120"}, requested: 600, want: 120},
		{name: "same", headers: []string{"600"}, requested: 600, want: 600},
		{name: "zero", headers: []string{"0"}, requested: 600, wantErr: true},
		{name: "invalid", headers: []string{"soon"}, requested: 600, wantErr: true},
		{name: "extended", headers: []string{"601"}, requested: 600, wantErr: true},
		{name: "duplicate", headers: []string{"120", "120"}, requested: 600, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil)
			for _, value := range test.headers {
				response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: value})
			}
			got, err := negotiatedSubscribeExpires(response, test.requested)
			if (err != nil) != test.wantErr {
				t.Fatalf("negotiated expiration error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && got != test.want {
				t.Fatalf("negotiated expiration = %d, want %d", got, test.want)
			}
		})
	}
}

func TestValidateOutgoingSubscribeExpiresRange(t *testing.T) {
	for _, expires := range []int{0, 600} {
		if err := validateOutgoingSubscribeExpires(expires); err != nil {
			t.Fatalf("validate outgoing expires %d: %v", expires, err)
		}
	}
	if err := validateOutgoingSubscribeExpires(-1); err == nil {
		t.Fatal("negative outgoing expires was accepted")
	}

	maxUint32 := uint64(^uint32(0))
	if uint64(^uint(0)) <= maxUint32 {
		return
	}
	if err := validateOutgoingSubscribeExpires(int(maxUint32)); err != nil {
		t.Fatalf("maximum SIP delta-seconds was rejected: %v", err)
	}
	if err := validateOutgoingSubscribeExpires(int(maxUint32 + 1)); err == nil {
		t.Fatal("outgoing expires above SIP delta-seconds range was accepted")
	}
}

func TestOutgoingSubscriptionCleanerRefreshesReferencedCascadeDialog(t *testing.T) {
	now := time.Now()
	key := "referenced-cascade-refresh"
	dialog := &outgoingSubscriptionDialog{
		expiresAt: now.Add(time.Minute),
		refreshAt: now.Add(-time.Second),
	}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Catalog", Expires: 600}
	api := &GB28181API{cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
		key: {Input: input, Refs: 1},
	}}
	api.outgoingSubscriptions.Store(key, dialog)
	refreshed := make(chan SubscribeInput, 1)
	api.cascadeSubscribe = func(_ context.Context, actual *SubscribeInput) error {
		dialog.mu.Lock()
		dialog.expiresAt = now.Add(10 * time.Minute)
		dialog.refreshAt = now.Add(8 * time.Minute)
		dialog.refreshing = false
		dialog.mu.Unlock()
		refreshed <- *actual
		return nil
	}

	api.cleanupOutgoingSubscription(key, dialog, now)
	select {
	case actual := <-refreshed:
		if actual != input {
			t.Fatalf("cascade refresh input = %+v, want %+v", actual, input)
		}
	case <-time.After(time.Second):
		t.Fatal("referenced cascade subscription was not refreshed before expiry")
	}
	api.lifecycleWG.Wait()
	dialog.mu.Lock()
	refreshing := dialog.refreshing
	refreshAt := dialog.refreshAt
	dialog.mu.Unlock()
	if refreshing || !refreshAt.After(now) {
		t.Fatalf("refreshed dialog state: refreshing=%v refreshAt=%v", refreshing, refreshAt)
	}
}

func TestOutgoingSubscriptionCleanerRefreshesManualDialog(t *testing.T) {
	now := time.Now()
	key := "manual-subscription-refresh"
	input := SubscribeInput{
		DeviceID: gb10DeviceID, TargetID: gb10ChannelID, Event: "Alarm", Expires: 600,
		StartAlarmPriority: "1", EndAlarmPriority: "4", AlarmMethod: "2",
	}
	dialog := &outgoingSubscriptionDialog{
		expiresAt: now.Add(time.Minute), refreshAt: now.Add(-time.Second),
		autoRefresh: true, refreshInput: input,
	}
	api := &GB28181API{}
	api.outgoingSubscriptions.Store(key, dialog)
	refreshed := make(chan SubscribeInput, 1)
	api.manualSubscribeRefresh = func(_ context.Context, actual *SubscribeInput) error {
		dialog.mu.Lock()
		dialog.expiresAt = now.Add(10 * time.Minute)
		dialog.refreshAt = now.Add(8 * time.Minute)
		dialog.refreshing = false
		dialog.mu.Unlock()
		refreshed <- *actual
		return nil
	}

	api.cleanupOutgoingSubscription(key, dialog, now)
	select {
	case actual := <-refreshed:
		if actual != input {
			t.Fatalf("manual refresh input = %+v, want %+v", actual, input)
		}
	case <-time.After(time.Second):
		t.Fatal("manual subscription was not refreshed before expiry")
	}
	api.lifecycleWG.Wait()
	dialog.mu.Lock()
	refreshing := dialog.refreshing
	refreshAt := dialog.refreshAt
	dialog.mu.Unlock()
	if refreshing || !refreshAt.After(now) {
		t.Fatalf("refreshed manual dialog state: refreshing=%v refreshAt=%v", refreshing, refreshAt)
	}
}

func TestOutgoingSubscriptionCleanerRecreatesExpiredManualDialog(t *testing.T) {
	now := time.Now()
	key := "expired-manual-subscription"
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10ChannelID, Event: "Catalog", Expires: 600}
	dialog := &outgoingSubscriptionDialog{
		expiresAt: now.Add(-time.Second), refreshAt: now.Add(-time.Minute),
		autoRefresh: true, refreshInput: input,
	}
	api := &GB28181API{}
	api.outgoingSubscriptions.Store(key, dialog)
	recreated := make(chan SubscribeInput, 1)
	api.manualSubscribeRefresh = func(_ context.Context, actual *SubscribeInput) error {
		recreated <- *actual
		return nil
	}

	api.cleanupOutgoingSubscription(key, dialog, now)
	select {
	case actual := <-recreated:
		if actual != input {
			t.Fatalf("recreated subscription input = %+v, want %+v", actual, input)
		}
	case <-time.After(time.Second):
		t.Fatal("expired manual subscription was not recreated")
	}
	api.lifecycleWG.Wait()
	if current, exists := api.outgoingSubscriptions.Load(key); exists && current == dialog {
		t.Fatal("expired manual subscription kept the old dialog generation")
	}
}

func TestCascadeSubscriptionContextMarker(t *testing.T) {
	if isCascadeSubscribeContext(t.Context()) {
		t.Fatal("ordinary subscription context was marked as cascade managed")
	}
	api := &GB28181API{}
	marked := false
	api.cascadeSubscribe = func(ctx context.Context, _ *SubscribeInput) error {
		marked = isCascadeSubscribeContext(ctx)
		return nil
	}
	if err := api.invokeCascadeSubscribe(t.Context(), &SubscribeInput{}); err != nil {
		t.Fatalf("invoke cascade subscription: %v", err)
	}
	if !marked {
		t.Fatal("cascade subscription context marker was not propagated")
	}
}

func TestOutgoingSubscriptionCleanerDoesNotRenewPendingUnsubscribe(t *testing.T) {
	now := time.Now()
	key := "pending-unsubscribe-cleanup"
	dialog := &outgoingSubscriptionDialog{expiresAt: now.Add(-time.Second), refreshAt: now.Add(-time.Minute)}
	dialog.cancelPending.Store(true)
	api := &GB28181API{cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
		key: {Input: SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60}, Refs: 1},
	}}
	api.outgoingSubscriptions.Store(key, dialog)
	refreshed := make(chan struct{}, 1)
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
		refreshed <- struct{}{}
		return nil
	}

	api.cleanupOutgoingSubscription(key, dialog, now)
	if _, exists := api.outgoingSubscriptions.Load(key); exists {
		t.Fatal("expired pending unsubscribe was not removed")
	}
	select {
	case <-refreshed:
		t.Fatal("pending unsubscribe was automatically renewed")
	default:
	}
}

func TestOutgoingSubscriptionCancelRequiresDialog(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: gb10DeviceID, Event: "catalog", Cancel: true})
	if err == nil || !strings.Contains(err.Error(), "subscription does not exist") {
		t.Fatalf("cancel missing subscription error = %v", err)
	}
}

func TestRefreshInboundSubscriptionDialogPreservesRouteAndUpdatesContact(t *testing.T) {
	conn := newFlowConnection()
	initial := newFlowRequest(t, conn, sip.MethodSubscribe, "refresh-inbound-dialog", []byte("query"))
	oldContact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@old-contact.example:5060")
	initial.AppendHeader(&sip.ContactHeader{Address: oldContact.URI.Clone(), Params: sip.NewParams()})
	proxy, _ := sip.ParseURI("sip:proxy.example;lr")
	initial.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{proxy}})

	refresh := newFlowRequest(t, conn, sip.MethodSubscribe, "refresh-inbound-dialog", []byte("query"))
	newContact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@new-contact.example:5070")
	refresh.AppendHeader(&sip.ContactHeader{Address: newContact.URI.Clone(), Params: sip.NewParams()})
	merged := refreshInboundSubscriptionDialog(initial, refresh)
	if merged == nil {
		t.Fatal("refreshed dialog snapshot is nil")
	}
	contact, ok := merged.Contact()
	if !ok || contact == nil || contact.Address == nil || contact.Address.Host() != "new-contact.example" {
		t.Fatalf("refreshed Contact = %v", contact)
	}
	recordRoutes := merged.GetHeaders("Record-Route")
	if len(recordRoutes) != 1 {
		t.Fatalf("refreshed Record-Route = %v", recordRoutes)
	}
	route, ok := recordRoutes[0].(*sip.RecordRouteHeader)
	if !ok || len(route.Addresses) != 1 || route.Addresses[0].Host() != "proxy.example" {
		t.Fatalf("refreshed route set = %#v", recordRoutes)
	}
}

func TestTerminatedNotifyClearsOutgoingSubscription(t *testing.T) {
	api := &GB28181API{}
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	api.outgoingSubscriptions.Store("catalog-key", dialog)

	request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-subscription", nil)
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("terminated-notify", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	assertFlowOK(t, <-flowResponse(t, connection))
	if _, exists := api.outgoingSubscriptions.Load("catalog-key"); exists {
		t.Fatal("terminated NOTIFY left outgoing subscription dialog")
	}
}

func TestTerminatedNotifyRenewsReferencedCascadeSubscription(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-cascade-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	key := "cascade-catalog-key"
	api.outgoingSubscriptions.Store(key, dialog)
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Catalog", Expires: 60}
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{Input: input, Refs: 1}
	renewed := make(chan SubscribeInput, 1)
	api.cascadeSubscribe = func(_ context.Context, actual *SubscribeInput) error {
		renewed <- *actual
		return nil
	}

	request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-cascade-subscription", nil)
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("terminated-cascade-notify", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	assertFlowOK(t, <-flowResponse(t, connection))
	select {
	case actual := <-renewed:
		if actual.DeviceID != input.DeviceID || actual.TargetID != input.TargetID || actual.Event != input.Event || actual.Cancel {
			t.Fatalf("renewed cascade subscription = %+v", actual)
		}
	case <-time.After(time.Second):
		t.Fatal("terminated referenced cascade subscription was not renewed")
	}
}

func TestTerminatedCascadeRenewalDoesNotClearNewerTermination(t *testing.T) {
	const key = "terminated-renewal-newer-termination"
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 60}
	state := &cascadeDownstreamSubscription{Input: input, Refs: 1, RetryGeneration: 1}
	terminated := &outgoingSubscriptionDialog{}
	api := &GB28181API{cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{key: state}}
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
		newer := &outgoingSubscriptionDialog{}
		api.outgoingSubscriptions.Store(key, newer)
		if !api.terminateOutgoingSubscription(key, newer, false, 0, time.Now()) {
			t.Fatal("newer terminated NOTIFY did not remove the replacement dialog")
		}
		return nil
	}
	api.renewTerminatedCascadeSubscription(key, terminated, t.Context())
	api.cascadeSubscriptionMu.Lock()
	retryBlocked := state.RetryBlocked
	retryGeneration := state.RetryGeneration
	api.cascadeSubscriptionMu.Unlock()
	if !retryBlocked || retryGeneration != 2 {
		t.Fatalf("renewal cleared newer termination: blocked=%v generation=%d", retryBlocked, retryGeneration)
	}
}

func TestTerminatedNotifyDoesNotRenewRejectedCascadeSubscription(t *testing.T) {
	for _, reason := range []string{"rejected", "noresource"} {
		t.Run(reason, func(t *testing.T) {
			api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
			connection := newFlowConnection()
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-blocked-"+reason, []byte("query"))
			dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			key := "cascade-blocked-" + reason
			api.outgoingSubscriptions.Store(key, dialog)
			api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{
				Input: SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 60}, Refs: 1,
			}
			renewed := make(chan struct{}, 1)
			api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
				renewed <- struct{}{}
				return nil
			}

			request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-blocked-"+reason, nil)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=" + reason})
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("terminated-blocked-"+reason, connection),
				DeviceID: gb10DeviceID, Source: connection.remote,
			}
			api.sipNotifySubscriptionState(ctx)
			assertFlowOK(t, <-flowResponse(t, connection))
			if _, exists := api.outgoingSubscriptions.Load(key); exists {
				t.Fatal("terminated NOTIFY left outgoing subscription dialog")
			}
			select {
			case <-renewed:
				t.Fatalf("terminated reason %s triggered automatic renewal", reason)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

func TestTerminatedNotifyHonorsRetryAfterBeforeCascadeRenewal(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-retry-after", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	key := "cascade-retry-after"
	api.outgoingSubscriptions.Store(key, dialog)
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{
		Input: SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 60}, Refs: 1,
	}
	renewed := make(chan time.Time, 1)
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
		renewed <- time.Now()
		return nil
	}

	request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-retry-after", nil)
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=probation;retry-after=1"})
	startedAt := time.Now()
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("terminated-retry-after", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	assertFlowOK(t, <-flowResponse(t, connection))
	select {
	case <-renewed:
		t.Fatal("terminated subscription renewed before retry-after elapsed")
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case renewedAt := <-renewed:
		if renewedAt.Before(startedAt.Add(time.Second)) {
			t.Fatalf("subscription renewed after %v, want at least 1s", renewedAt.Sub(startedAt))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("terminated subscription was not renewed after retry-after")
	}
}

func TestTerminatedNotifyRetryAfterUsesAcceptanceTimeFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			api := &GB28181API{
				svr:                  &Server{memoryStorer: memory},
				cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
				lifecycleCtx:         lifecycleCtx,
				lifecycleCancel:      lifecycleCancel,
				lifecycleDone:        make(chan struct{}),
			}
			t.Cleanup(func() {
				api.beginClose()
				api.lifecycleWG.Wait()
			})

			connection := newFlowConnection()
			callID := "terminated-accepted-retry-after-" + string(version)
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
			dialog := &outgoingSubscriptionDialog{
				response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
				deviceID: gb10DeviceID,
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			key := callID
			input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 60}
			state := &cascadeDownstreamSubscription{Input: input, Refs: 1}
			api.outgoingSubscriptions.Store(key, dialog)
			api.cascadeSubscriptions[key] = state
			renewed := make(chan time.Time, 1)
			api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
				renewed <- time.Now()
				return nil
			}

			request := newFlowRequest(t, connection, sip.MethodNotify, callID, nil)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=probation;retry-after=1"})
			validatedAt := time.Now().Add(-2 * time.Second)
			wantRetryAt := validatedAt.Add(time.Second)

			if _, err := api.validateOutgoingSubscriptionNotifyModeAt(true, validatedAt, gb10DeviceID, request, "Alarm", gb10DeviceID); err != nil {
				t.Fatalf("accepted terminated NOTIFY rejected: %v", err)
			}
			api.cascadeSubscriptionMu.Lock()
			retryAt := state.RetryAt
			api.cascadeSubscriptionMu.Unlock()
			if !retryAt.Equal(wantRetryAt) {
				t.Fatalf("retry deadline = %v, want acceptance-relative %v", retryAt, wantRetryAt)
			}
			select {
			case renewedAt := <-renewed:
				if renewedAt.Before(wantRetryAt) {
					t.Fatalf("subscription renewed at %v before retry deadline %v", renewedAt, wantRetryAt)
				}
			case <-time.After(250 * time.Millisecond):
				t.Fatal("elapsed retry-after was restarted from terminated NOTIFY commit")
			}
		})
	}
}

func TestMalformedSubscriptionStateReturnsBadRequestWithoutDeletingDialog(t *testing.T) {
	api := &GB28181API{}
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "malformed-subscription-state", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	key := "malformed-subscription-state"
	api.outgoingSubscriptions.Store(key, dialog)

	request := newFlowRequest(t, connection, sip.MethodNotify, "malformed-subscription-state", nil)
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=probation;retry-after=invalid"})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("malformed-subscription-state", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	if response := <-flowResponse(t, connection); !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("malformed Subscription-State response = %s", response)
	}
	if actual, exists := api.outgoingSubscriptions.Load(key); !exists || actual != dialog {
		t.Fatal("malformed Subscription-State changed the valid subscription dialog")
	}
}

func TestTerminatedNotifyRejectsForeignSourceAndDialogTags(t *testing.T) {
	for _, test := range []struct {
		name     string
		deviceID string
		wrongTag bool
	}{
		{name: "foreign source", deviceID: "34020000001320000009"},
		{name: "foreign dialog", deviceID: gb10DeviceID, wrongTag: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &GB28181API{}
			connection := newFlowConnection()
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-protected", []byte("query"))
			dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			api.outgoingSubscriptions.Store("protected-key", dialog)
			request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-protected", nil)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			if test.wrongTag {
				from, _ := request.From()
				from.Params.Add("tag", sip.String{Str: "foreign-tag"})
			}
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
			ctx := &sip.Context{Request: request, Tx: sip.NewTransaction("terminated-protected", connection), DeviceID: test.deviceID, Source: connection.remote}
			api.sipNotifySubscriptionState(ctx)
			if response := <-flowResponse(t, connection); !strings.Contains(response, "SIP/2.0 481") {
				t.Fatalf("foreign terminated NOTIFY response = %s", response)
			}
			if _, exists := api.outgoingSubscriptions.Load("protected-key"); !exists {
				t.Fatal("foreign terminated NOTIFY removed subscription dialog")
			}
		})
	}
}

func prepareOutgoingNotifyDialog(t *testing.T, dialog *outgoingSubscriptionDialog, event, cmdType, targetID string) {
	t.Helper()
	if dialog == nil || dialog.response == nil {
		t.Fatal("subscription dialog response is unavailable")
	}
	callID, ok := dialog.response.CallID()
	if !ok || callID == nil {
		t.Fatal("subscription response missing Call-ID")
	}
	dialog.notify = outgoingSubscriptionNotifyDialog{
		callID:    normalizeCallID(callID),
		localTag:  sipResponseFromTag(dialog.response),
		remoteTag: sipResponseToTag(dialog.response),
		event:     event,
		cmdType:   cmdType,
		deviceID:  gb10DeviceID,
		targetID:  targetID,
		expiresAt: time.Now().Add(time.Minute),
	}
}

func TestActiveNotifyRequiresMatchingOutgoingSubscriptionDialog(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "active-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	api := &GB28181API{}
	api.outgoingSubscriptions.Store("alarm-key", dialog)
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)

	valid := newFlowRequest(t, connection, sip.MethodNotify, "active-subscription", body)
	applyTerminatedNotifyDialog(t, valid, dialog.response)
	valid.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	valid.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, valid, "Alarm", gb10DeviceID); err != nil {
		t.Fatalf("valid active NOTIFY rejected: %v", err)
	}
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, valid, "Alarm", gb10DeviceID); err == nil {
		t.Fatal("replayed NOTIFY CSeq accepted")
	}

	for _, test := range []struct {
		name   string
		mutate func(*sip.Request)
	}{
		{name: "missing Event", mutate: func(request *sip.Request) { request.RemoveHeader("Event") }},
		{name: "missing Subscription-State", mutate: func(request *sip.Request) { request.RemoveHeader("Subscription-State") }},
		{name: "wrong Event", mutate: func(request *sip.Request) {
			request.RemoveHeader("Event")
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog"})
		}},
		{name: "wrong Call-ID", mutate: func(request *sip.Request) {
			request.RemoveHeader("Call-ID")
			callID := sip.CallID("foreign-call-id")
			request.AppendHeader(&callID)
		}},
		{name: "wrong From tag", mutate: func(request *sip.Request) {
			from, _ := request.From()
			from.Params.Add("tag", sip.String{Str: "foreign-tag"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := newFlowRequest(t, connection, sip.MethodNotify, "active-subscription", body)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
			cseq, _ := request.CSeq()
			cseq.SeqNo = 2
			test.mutate(request)
			if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, request, "Alarm", gb10DeviceID); err == nil {
				t.Fatal("forged active NOTIFY accepted")
			}
		})
	}
}

func TestActiveNotifyExpiresRequirementByVersion(t *testing.T) {
	for _, test := range []struct {
		version GBProtocolVersion
		wantErr bool
	}{
		{version: GBVersion10},
		{version: GBVersion11},
		{version: GBVersion20},
		{version: GBVersion30, wantErr: true},
	} {
		t.Run(string(test.version), func(t *testing.T) {
			connection := newFlowConnection()
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "active-expires-"+string(test.version), []byte("query"))
			response := sip.NewResponseFromRequest("", subscribe, 200, "OK", nil)
			dialog := &outgoingSubscriptionDialog{response: response, deviceID: gb10DeviceID}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			api, _ := newVersionGateAPI(test.version)
			api.outgoingSubscriptions.Store("alarm-key", dialog)

			request := newFlowRequest(t, connection, sip.MethodNotify, "active-expires-"+string(test.version), nil)
			applyTerminatedNotifyDialog(t, request, response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active"})
			_, err := api.validateOutgoingSubscriptionNotifyMode(false, gb10DeviceID, request, "Alarm", gb10DeviceID)
			if (err != nil) != test.wantErr {
				t.Fatalf("active NOTIFY without expires error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestSubscriptionStateParameterValidation(t *testing.T) {
	duration := func(value time.Duration) *time.Duration { return &value }
	valid := []struct {
		value      string
		state      string
		reason     string
		expires    *time.Duration
		retryAfter *time.Duration
	}{
		{value: "active", state: "active"},
		{value: "active;expires=90;retry-after=0", state: "active", expires: duration(90 * time.Second), retryAfter: duration(0)},
		{value: "pending;expires=0", state: "pending", expires: duration(0)},
		{value: "terminated;reason=timeout;retry-after=10", state: "terminated", reason: "timeout", retryAfter: duration(10 * time.Second)},
		{value: "terminated;reason=Vendor-Reason", state: "terminated", reason: "vendor-reason"},
		{value: `active;vendor="value;with-semicolon"`, state: "active"},
		{value: "waiting-approval;vendor=queue", state: "waiting-approval"},
	}
	for _, test := range valid {
		t.Run("valid "+test.value, func(t *testing.T) {
			actual, err := parseSubscriptionState(test.value)
			if err != nil {
				t.Fatalf("parseSubscriptionState(%q): %v", test.value, err)
			}
			if actual.name != test.state {
				t.Fatalf("state = %q, want %q", actual.name, test.state)
			}
			if actual.reason != test.reason {
				t.Fatalf("reason = %q, want %q", actual.reason, test.reason)
			}
			if (actual.expires == nil) != (test.expires == nil) || actual.expires != nil && *actual.expires != *test.expires {
				t.Fatalf("expires = %v, want %v", actual.expires, test.expires)
			}
			if (actual.retryAfter == nil) != (test.retryAfter == nil) || actual.retryAfter != nil && *actual.retryAfter != *test.retryAfter {
				t.Fatalf("retry-after = %v, want %v", actual.retryAfter, test.retryAfter)
			}
		})
	}

	invalid := []string{
		"",
		"unknown state",
		"active;",
		"active;expires",
		"active;expires=-1",
		`active;expires="90"`,
		"active;expires=10;Expires=9",
		"active;expires=9223372037",
		"terminated;reason",
		`terminated;reason="timeout"`,
		"terminated;reason=bad value",
		"terminated;reason=timeout;Reason=probation",
		"terminated;retry-after",
		"terminated;retry-after=-1",
		`terminated;retry-after="10"`,
		"terminated;retry-after=9223372037",
		`active;vendor="unterminated`,
	}
	for _, value := range invalid {
		t.Run("invalid "+value, func(t *testing.T) {
			if _, err := parseSubscriptionState(value); err == nil {
				t.Fatalf("parseSubscriptionState(%q) succeeded", value)
			}
		})
	}
}

func TestTerminatedSubscriptionRetryPolicy(t *testing.T) {
	for _, test := range []struct {
		value   string
		version GBProtocolVersion
		retry   bool
		delay   time.Duration
	}{
		{value: "terminated;reason=timeout", retry: true},
		{value: "terminated;reason=deactivated;retry-after=60", retry: true},
		{value: "terminated;reason=probation", retry: true, delay: defaultProbationSubscribeRetryDelay},
		{value: "terminated;reason=probation;retry-after=10", retry: true, delay: 10 * time.Second},
		{value: "terminated;reason=giveup", retry: true},
		{value: "terminated;reason=giveup;retry-after=10", retry: true, delay: 10 * time.Second},
		{value: "terminated;reason=rejected;retry-after=10"},
		{value: "terminated;reason=noresource"},
		{value: "terminated;reason=invariant;retry-after=3", version: GBVersion30},
		{value: "terminated;reason=invariant;retry-after=3", version: GBVersion20, retry: true, delay: 3 * time.Second},
		{value: "terminated;reason=vendor;retry-after=3", retry: true, delay: 3 * time.Second},
		{value: "terminated;retry-after=3", retry: true, delay: 3 * time.Second},
	} {
		t.Run(test.value+"/"+string(test.version), func(t *testing.T) {
			state, err := parseSubscriptionState(test.value)
			if err != nil {
				t.Fatalf("parseSubscriptionState(%q): %v", test.value, err)
			}
			version := test.version
			if version == "" {
				version = GBVersion20
			}
			retry, delay := terminatedSubscriptionRetry(state, version)
			if retry != test.retry || delay != test.delay {
				t.Fatalf("retry policy = %v, %v; want %v, %v", retry, delay, test.retry, test.delay)
			}
		})
	}
}

func TestCascadeSubscriptionRefreshRespectsTerminatedRetryPolicy(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 60}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	state := &cascadeDownstreamSubscription{Input: input, Refs: 1, RetryAt: time.Now().Add(time.Hour)}
	api.cascadeSubscriptions[key] = state
	called := 0
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
		called++
		return nil
	}
	desired := map[string]SubscribeInput{key: input}

	if _, err := api.syncCascadeDownstreamSubscriptions(context.Background(), []string{key}, desired); err != nil {
		t.Fatalf("refresh during retry-after: %v", err)
	}
	if called != 0 {
		t.Fatalf("refresh bypassed retry-after: calls = %d", called)
	}

	api.cascadeSubscriptionMu.Lock()
	state.RetryAt = time.Now().Add(-time.Second)
	api.cascadeSubscriptionMu.Unlock()
	if _, err := api.syncCascadeDownstreamSubscriptions(context.Background(), []string{key}, desired); err != nil {
		t.Fatalf("refresh after retry-after: %v", err)
	}
	if called != 1 {
		t.Fatalf("refresh after retry-after calls = %d, want 1", called)
	}

	api.cascadeSubscriptionMu.Lock()
	state.RetryBlocked = true
	api.cascadeSubscriptionMu.Unlock()
	if _, err := api.syncCascadeDownstreamSubscriptions(context.Background(), []string{key}, desired); err != nil {
		t.Fatalf("blocked refresh: %v", err)
	}
	if called != 1 {
		t.Fatalf("rejected/noresource refresh calls = %d, want 1", called)
	}
}

func TestActiveNotifyUsesAuthoritativeShorterExpiry(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			connection := newFlowConnection()
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "shortened-subscription-"+string(version), []byte("query"))
			dialog := &outgoingSubscriptionDialog{
				response:  sip.NewResponseFromRequest("", subscribe, 200, "OK", nil),
				deviceID:  gb10DeviceID,
				expiresAt: time.Now().Add(time.Minute),
				refreshAt: time.Now().Add(50 * time.Second),
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			api := &GB28181API{}
			api.outgoingSubscriptions.Store("shortened-"+string(version), dialog)

			request := newFlowRequest(t, connection, sip.MethodNotify, "shortened-subscription-"+string(version), []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=20"})
			startedAt := time.Now()
			if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, request, "Alarm", gb10DeviceID); err != nil {
				t.Fatalf("shortened NOTIFY rejected: %v", err)
			}

			dialog.mu.Lock()
			expiresAt := dialog.expiresAt
			refreshAt := dialog.refreshAt
			dialog.mu.Unlock()
			if expiresAt.Before(startedAt.Add(19*time.Second)) || expiresAt.After(startedAt.Add(21*time.Second)) {
				t.Fatalf("subscription expiry = %v, want about 20 seconds", expiresAt.Sub(startedAt))
			}
			if !refreshAt.After(startedAt) || !refreshAt.Before(expiresAt) {
				t.Fatalf("refresh deadline = %v, expiry = %v", refreshAt, expiresAt)
			}
		})
	}
}

func TestActiveNotifyExpiryUsesAcceptanceTimeFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			connection := newFlowConnection()
			callID := "accepted-notify-expiry-" + string(version)
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
			dialog := &outgoingSubscriptionDialog{
				response:  sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
				deviceID:  gb10DeviceID,
				expiresAt: time.Now().Add(2 * time.Minute),
				refreshAt: time.Now().Add(time.Minute),
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			dialog.notify.expiresAt = dialog.expiresAt

			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			api.outgoingSubscriptions.Store(callID, dialog)

			request := newFlowRequest(t, connection, sip.MethodNotify, callID, []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=40"})
			validatedAt := time.Now().Add(-30 * time.Second)
			wantExpiresAt := validatedAt.Add(40 * time.Second)

			if _, err := api.validateOutgoingSubscriptionNotifyModeAt(true, validatedAt, gb10DeviceID, request, "Alarm", gb10DeviceID); err != nil {
				t.Fatalf("accepted active NOTIFY rejected: %v", err)
			}
			snapshot := dialog.snapshotNotifyDialog()
			if !snapshot.reportedExpiresAt.Equal(wantExpiresAt) {
				t.Fatalf("reported expiry = %v, want acceptance-relative %v", snapshot.reportedExpiresAt, wantExpiresAt)
			}
			if !snapshot.expiresAt.Equal(wantExpiresAt) {
				t.Fatalf("NOTIFY expiry = %v, want acceptance-relative %v", snapshot.expiresAt, wantExpiresAt)
			}
			dialog.mu.Lock()
			expiresAt := dialog.expiresAt
			dialog.mu.Unlock()
			if !expiresAt.Equal(wantExpiresAt) {
				t.Fatalf("subscription expiry = %v, want acceptance-relative %v", expiresAt, wantExpiresAt)
			}
		})
	}
}

func TestActiveNotifyCannotLengthenSubscription(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "non-lengthened-subscription", []byte("query"))
	originalExpiry := time.Now().Add(10 * time.Second)
	dialog := &outgoingSubscriptionDialog{
		response:  sip.NewResponseFromRequest("", subscribe, 200, "OK", nil),
		deviceID:  gb10DeviceID,
		expiresAt: originalExpiry,
		refreshAt: time.Now().Add(5 * time.Second),
	}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	dialog.notify.expiresAt = originalExpiry
	api := &GB28181API{}
	api.outgoingSubscriptions.Store("non-lengthened", dialog)

	request := newFlowRequest(t, connection, sip.MethodNotify, "non-lengthened-subscription", []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, request, "Alarm", gb10DeviceID); err != nil {
		t.Fatalf("NOTIFY with longer reported expiry rejected: %v", err)
	}

	dialog.mu.Lock()
	actualExpiry := dialog.expiresAt
	dialog.mu.Unlock()
	if !actualExpiry.Equal(originalExpiry) {
		t.Fatalf("subscription expiry lengthened from %v to %v", originalExpiry, actualExpiry)
	}
	if notifyExpiry := dialog.snapshotNotifyDialog().expiresAt; !notifyExpiry.Equal(originalExpiry) {
		t.Fatalf("NOTIFY dialog expiry lengthened from %v to %v", originalExpiry, notifyExpiry)
	}
}

func TestEarlyNotifyExpirySurvivesSubscribeResponse(t *testing.T) {
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodSubscribe, "early-expiry", []byte("query"))
	dialog := &outgoingSubscriptionDialog{eventValue: "presence"}
	dialog.setPendingNotifyDialog(request, "Alarm", gb10DeviceID, gb10DeviceID, 60)
	reportedExpiry := time.Now().Add(20 * time.Second)
	dialog.notifyMu.Lock()
	dialog.notify.reportedExpiresAt = reportedExpiry
	dialog.notify.expiresAt = reportedExpiry
	dialog.notifyMu.Unlock()

	response := sip.NewResponseFromRequest("", request, 200, "OK", nil)
	to, ok := response.To()
	if !ok || to == nil {
		t.Fatal("response missing To")
	}
	to.Params.Add("tag", sip.String{Str: "remote-tag"})
	if err := dialog.confirmNotifyDialog(response, 60); err != nil {
		t.Fatalf("confirmNotifyDialog(): %v", err)
	}
	if actual := dialog.snapshotNotifyDialog().expiresAt; !actual.Equal(reportedExpiry) {
		t.Fatalf("early NOTIFY expiry = %v, want %v", actual, reportedExpiry)
	}
}

func TestFailedOutgoingSubscriptionRefreshPreservesConcurrentNotifyProgressFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			connection := newFlowConnection()
			callID := "failed-refresh-notify-progress-" + string(version)
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
			response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
			to, ok := response.To()
			if !ok || to == nil {
				t.Fatal("subscription response missing To")
			}
			to.Params.Add("tag", sip.String{Str: "remote-tag"})
			dialog := &outgoingSubscriptionDialog{
				response:   response,
				deviceID:   gb10DeviceID,
				eventValue: "presence",
				expiresAt:  time.Now().Add(time.Minute),
				refreshAt:  time.Now().Add(50 * time.Second),
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)

			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			api.outgoingSubscriptions.Store(callID, dialog)

			notify := func(cseq uint32, host string, expires int) {
				t.Helper()
				request := newFlowRequest(t, connection, sip.MethodNotify, callID, []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
				applyTerminatedNotifyDialog(t, request, response)
				request.RemoveHeader("Contact")
				contact, err := sip.ParseURI("sip:" + host)
				if err != nil {
					t.Fatalf("parse NOTIFY Contact: %v", err)
				}
				request.AppendHeader(&sip.ContactHeader{Address: contact})
				sequence, _ := request.CSeq()
				sequence.SeqNo = cseq
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: fmt.Sprintf("active;expires=%d", expires)})
				if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, request, "Alarm", gb10DeviceID); err != nil {
					t.Fatalf("active NOTIFY rejected: %v", err)
				}
			}

			notify(1, "notify-a.example:5070", 50)
			previous := dialog.snapshotNotifyDialog()
			dialog.setPendingNotifyDialog(subscribe, "Alarm", gb10DeviceID, gb10DeviceID, 60)
			dialog.mu.Lock()
			notify(2, "notify-b.example:5080", 10)
			concurrent := dialog.snapshotNotifyDialog()

			dialog.restoreNotifyDialogLocked(previous, time.Now())
			restored := dialog.snapshotNotifyDialog()
			restoredOuterExpiry := dialog.expiresAt
			dialog.mu.Unlock()
			if restored.cseq != concurrent.cseq {
				t.Fatalf("restored NOTIFY CSeq = %d, want concurrent %d", restored.cseq, concurrent.cseq)
			}
			if restored.routeRequest == nil || concurrent.routeRequest == nil ||
				firstSingleHeaderValue(restored.routeRequest, "Contact") != firstSingleHeaderValue(concurrent.routeRequest, "Contact") {
				t.Fatalf("restored NOTIFY Contact = %q, want concurrent %q",
					firstSingleHeaderValue(restored.routeRequest, "Contact"), firstSingleHeaderValue(concurrent.routeRequest, "Contact"))
			}
			if !restored.reportedExpiresAt.Equal(concurrent.reportedExpiresAt) {
				t.Fatalf("restored reported expiry = %v, want concurrent %v", restored.reportedExpiresAt, concurrent.reportedExpiresAt)
			}
			if !restored.expiresAt.Equal(concurrent.expiresAt) {
				t.Fatalf("restored NOTIFY expiry = %v, want concurrent %v", restored.expiresAt, concurrent.expiresAt)
			}
			if !restoredOuterExpiry.Equal(concurrent.expiresAt) {
				t.Fatalf("restored subscription expiry = %v, want concurrent %v", restoredOuterExpiry, concurrent.expiresAt)
			}
		})
	}
}

func TestOutgoingSubscriptionRefreshUsesNotifyDialogRoute(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "notify-dialog-route", []byte("query"))
	responseTarget, _ := sip.ParseURI("sip:response-target.example:5070")
	responseProxy, _ := sip.ParseURI("sip:response-proxy.example;lr")
	response := sip.NewResponseFromRequest("", subscribe, 200, "OK", nil)
	response.AppendHeader(&sip.ContactHeader{Address: responseTarget})
	response.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{responseProxy}})
	to, _ := response.To()
	to.Params.Add("tag", sip.String{Str: "remote-tag"})

	notify := newFlowRequest(t, connection, sip.MethodNotify, "notify-dialog-route", nil)
	applyTerminatedNotifyDialog(t, notify, response)
	notifyTarget, _ := sip.ParseURI("sip:notify-target.example:5080")
	notifyProxy, _ := sip.ParseURI("sip:notify-proxy.example;lr")
	notify.RemoveHeader("Contact")
	notify.AppendHeader(&sip.ContactHeader{Address: notifyTarget})
	notify.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{notifyProxy}})

	refresh, err := buildOutgoingSubscriptionRefreshRequest(response, notify)
	if err != nil {
		t.Fatalf("buildOutgoingSubscriptionRefreshRequest(): %v", err)
	}
	if recipient := refresh.Recipient(); recipient == nil || recipient.String() != notifyTarget.String() {
		t.Fatalf("refresh recipient = %v; want NOTIFY Contact %s", recipient, notifyTarget)
	}
	routes := refresh.GetHeaders("Route")
	if len(routes) != 1 || !strings.Contains(routes[0].String(), notifyProxy.String()) || strings.Contains(routes[0].String(), responseProxy.String()) {
		t.Fatalf("refresh routes = %v; want NOTIFY route %s", routes, notifyProxy)
	}
}

func TestOutgoingSubscriptionRefreshRouteFailureDoesNotConsumeCSeq(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "notify-dialog-invalid-route", []byte("query"))
	responseTarget, _ := sip.ParseURI("sip:response-target.example:5070")
	response := sip.NewResponseFromRequest("", subscribe, 200, "OK", nil)
	response.AppendHeader(&sip.ContactHeader{Address: responseTarget})
	to, _ := response.To()
	to.Params.Add("tag", sip.String{Str: "remote-tag"})

	invalid := newFlowRequest(t, connection, sip.MethodNotify, "notify-dialog-invalid-route", nil)
	applyTerminatedNotifyDialog(t, invalid, response)
	invalid.RemoveHeader("From")
	if request, err := buildOutgoingSubscriptionRefreshRequest(response, invalid); err == nil || request != nil {
		t.Fatalf("invalid NOTIFY dialog refresh = request %v, err %v", request, err)
	}

	valid := newFlowRequest(t, connection, sip.MethodNotify, "notify-dialog-invalid-route", nil)
	applyTerminatedNotifyDialog(t, valid, response)
	refresh, err := buildOutgoingSubscriptionRefreshRequest(response, valid)
	if err != nil {
		t.Fatal(err)
	}
	cseq, ok := refresh.CSeq()
	if !ok || cseq == nil || cseq.SeqNo != 2 {
		t.Fatalf("refresh CSeq after invalid route = %+v, want 2", cseq)
	}
}

func TestOutgoingSubscriptionRefreshWritesNotifyDialogRoute(t *testing.T) {
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(platform)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api

	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = remoteRaw.Close()
		sipServer.Close()
	})
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtime.UpdateRuntime(func(current *Device) {
		current.conn = connection
		current.source = connection.RemoteAddr()
		current.to = device
	})
	server.memoryStorer = &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}

	readAndRespond := func(extra string) <-chan string {
		t.Helper()
		captured := make(chan string, 1)
		go func() {
			_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
			request, err := readAnnexGTestSIPFrame(bufio.NewReader(remoteRaw))
			if err != nil {
				captured <- "read request: " + err.Error()
				return
			}
			to := annexGTestSIPHeader(request, "To")
			response := annexGTestSIPResponse(request, http.StatusOK, "OK", extra)
			if !strings.Contains(to, ";tag=") {
				response = strings.Replace(response, "To: "+to+"\r\n", "To: "+to+";tag=notify-route\r\n", 1)
			}
			if _, err = remoteRaw.Write([]byte(response)); err != nil {
				captured <- "write response: " + err.Error()
				return
			}
			captured <- request
		}()
		return captured
	}

	initialCapture := readAndRespond("Expires: 60\r\nContact: <sip:response-target.example:5070>\r\nRecord-Route: <sip:response-proxy.example;lr>")
	input := &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60}
	if err := api.Subscribe(t.Context(), input); err != nil {
		t.Fatalf("initial Alarm SUBSCRIBE failed: %v", err)
	}
	if initial := <-initialCapture; strings.HasPrefix(initial, "read request:") || strings.HasPrefix(initial, "write response:") {
		t.Fatal(initial)
	}

	var dialog *outgoingSubscriptionDialog
	api.outgoingSubscriptions.Range(func(_, value any) bool {
		dialog, _ = value.(*outgoingSubscriptionDialog)
		return false
	})
	if dialog == nil || dialog.response == nil {
		t.Fatal("initial SUBSCRIBE did not retain its dialog")
	}
	callID, ok := dialog.response.CallID()
	if !ok || callID == nil {
		t.Fatal("initial SUBSCRIBE response missing Call-ID")
	}
	notifyConnection := newFlowConnection()
	notify := newFlowRequest(t, notifyConnection, sip.MethodNotify, string(*callID), nil)
	notify.SetConnection(connection)
	notify.SetSource(connection.RemoteAddr())
	notify.SetDestination(connection.LocalAddr())
	applyTerminatedNotifyDialog(t, notify, dialog.response)
	notifyTarget, _ := sip.ParseURI("sip:notify-target.example:5080")
	notifyProxy, _ := sip.ParseURI("sip:notify-proxy.example;lr")
	notify.RemoveHeader("Contact")
	notify.AppendHeader(&sip.ContactHeader{Address: notifyTarget})
	notify.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{notifyProxy}})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, notify, "Alarm", gb10DeviceID); err != nil {
		t.Fatalf("commit first NOTIFY dialog route: %v", err)
	}

	refreshCapture := readAndRespond("Expires: 60")
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- api.Subscribe(t.Context(), input) }()
	refresh := <-refreshCapture
	if strings.HasPrefix(refresh, "read request:") || strings.HasPrefix(refresh, "write response:") {
		t.Fatal(refresh)
	}
	if err := <-refreshDone; err != nil {
		t.Fatalf("refresh Alarm SUBSCRIBE failed: %v\nrequest:\n%s", err, refresh)
	}
	if firstLine := strings.SplitN(refresh, "\r\n", 2)[0]; firstLine != "SUBSCRIBE "+notifyTarget.String()+" SIP/2.0" {
		t.Fatalf("refresh request line = %q; want NOTIFY Contact target", firstLine)
	}
	if route := annexGTestSIPHeader(refresh, "Route"); !strings.Contains(route, notifyProxy.String()) || strings.Contains(route, "response-proxy.example") {
		t.Fatalf("refresh Route = %q; want NOTIFY Record-Route only", route)
	}
}

func TestInitialOutgoingNotifyRejectsInvalidDialogRoute(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*sip.Request)
	}{
		{
			name: "missing Contact",
			mutate: func(request *sip.Request) {
				request.RemoveHeader("Contact")
			},
		},
		{
			name: "duplicate Contact",
			mutate: func(request *sip.Request) {
				target, _ := sip.ParseURI("sip:duplicate-contact.example")
				request.AppendHeader(&sip.ContactHeader{Address: target})
			},
		},
		{
			name: "malformed Record-Route",
			mutate: func(request *sip.Request) {
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Record-Route", Contents: "<>"})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection := newFlowConnection()
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "invalid-notify-route", []byte("query"))
			response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
			to, _ := response.To()
			to.Params.Add("tag", sip.String{Str: "remote-tag"})
			dialog := &outgoingSubscriptionDialog{response: response, deviceID: gb10DeviceID}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			api := &GB28181API{}
			api.outgoingSubscriptions.Store(test.name, dialog)

			notify := newFlowRequest(t, connection, sip.MethodNotify, "invalid-notify-route", nil)
			applyTerminatedNotifyDialog(t, notify, response)
			notify.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			notify.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
			test.mutate(notify)
			if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, notify, "Alarm", gb10DeviceID); err == nil {
				t.Fatal("invalid initial NOTIFY dialog route was accepted")
			}
			if snapshot := dialog.snapshotNotifyDialog(); snapshot.routeRequest != nil {
				t.Fatal("invalid initial NOTIFY changed the dialog route")
			}
		})
	}
}

func TestSubsequentNotifyRefreshesRemoteTargetWithoutChangingRouteSet(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "notify-target-refresh", []byte("query"))
	response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
	to, _ := response.To()
	to.Params.Add("tag", sip.String{Str: "remote-tag"})
	dialog := &outgoingSubscriptionDialog{response: response, deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	api := &GB28181API{}
	api.outgoingSubscriptions.Store("notify-target-refresh", dialog)

	firstTarget, _ := sip.ParseURI("sip:first-notify-target.example:5070")
	firstProxy, _ := sip.ParseURI("sip:first-notify-proxy.example;lr")
	first := newFlowRequest(t, connection, sip.MethodNotify, "notify-target-refresh", nil)
	applyTerminatedNotifyDialog(t, first, response)
	first.RemoveHeader("Contact")
	first.AppendHeader(&sip.ContactHeader{Address: firstTarget})
	first.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{firstProxy}})
	first.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	first.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, first, "Alarm", gb10DeviceID); err != nil {
		t.Fatalf("commit first NOTIFY: %v", err)
	}

	secondTarget, _ := sip.ParseURI("sip:second-notify-target.example:5080")
	secondProxy, _ := sip.ParseURI("sip:second-notify-proxy.example;lr")
	second := newFlowRequest(t, connection, sip.MethodNotify, "notify-target-refresh", nil)
	applyTerminatedNotifyDialog(t, second, response)
	secondCSeq, _ := second.CSeq()
	secondCSeq.SeqNo = 2
	second.RemoveHeader("Contact")
	second.AppendHeader(&sip.ContactHeader{Address: secondTarget})
	second.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{secondProxy}})
	second.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	second.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, second, "Alarm", gb10DeviceID); err != nil {
		t.Fatalf("commit target-refresh NOTIFY: %v", err)
	}

	refresh, err := buildOutgoingSubscriptionRefreshRequest(response, dialog.snapshotNotifyDialog().routeRequest)
	if err != nil {
		t.Fatalf("build refresh request: %v", err)
	}
	if recipient := refresh.Recipient(); recipient == nil || recipient.String() != secondTarget.String() {
		t.Fatalf("refresh recipient = %v; want latest NOTIFY Contact %s", recipient, secondTarget)
	}
	routes := refresh.GetHeaders("Route")
	if len(routes) != 1 || !strings.Contains(routes[0].String(), firstProxy.String()) || strings.Contains(routes[0].String(), secondProxy.String()) {
		t.Fatalf("refresh routes = %v; want initial NOTIFY route set %s", routes, firstProxy)
	}
}

func TestSuccessfulUnsubscribeWaitsForFinalTerminatedNotify(t *testing.T) {
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(platform)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api

	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = remoteRaw.Close()
		sipServer.Close()
	})
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtime.UpdateRuntime(func(current *Device) {
		current.conn = connection
		current.source = connection.RemoteAddr()
		current.to = device
	})
	server.memoryStorer = &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}

	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(remoteRaw)
		for index := 0; index < 2; index++ {
			request, err := readAnnexGTestSIPFrame(reader)
			if err != nil {
				remoteErr <- err
				return
			}
			extra := ""
			if index == 0 {
				extra = "Expires: 60"
			}
			response := annexGTestSIPResponse(request, http.StatusOK, "OK", extra)
			if index == 0 {
				to := annexGTestSIPHeader(request, "To")
				response = strings.Replace(response, "To: "+to+"\r\n", "To: "+to+";tag=unsubscribe-final\r\n", 1)
			}
			if _, err = remoteRaw.Write([]byte(response)); err != nil {
				remoteErr <- err
				return
			}
		}
		remoteErr <- nil
	}()

	input := &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60}
	if err := api.Subscribe(t.Context(), input); err != nil {
		t.Fatalf("initial Alarm SUBSCRIBE failed: %v", err)
	}
	cancel := *input
	cancel.Cancel = true
	if err := api.Subscribe(t.Context(), &cancel); err != nil {
		t.Fatalf("Alarm unsubscribe failed: %v", err)
	}
	if err := <-remoteErr; err != nil {
		t.Fatal(err)
	}

	var key any
	var dialog *outgoingSubscriptionDialog
	api.outgoingSubscriptions.Range(func(candidate, value any) bool {
		key = candidate
		dialog, _ = value.(*outgoingSubscriptionDialog)
		return false
	})
	if dialog == nil || dialog.response == nil {
		t.Fatal("successful unsubscribe removed dialog before final NOTIFY")
	}
	callID, _ := dialog.response.CallID()
	if callID == nil {
		t.Fatal("retained unsubscribe dialog missing Call-ID")
	}
	terminated := newFlowRequest(t, newFlowConnection(), sip.MethodNotify, string(*callID), nil)
	applyTerminatedNotifyDialog(t, terminated, dialog.response)
	terminated.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	terminated.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
	if matched, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, terminated, "Alarm", gb10DeviceID); err != nil || matched != key {
		t.Fatalf("final terminated NOTIFY match = %v, error = %v", matched, err)
	}
	if _, exists := api.outgoingSubscriptions.Load(key); exists {
		t.Fatal("final terminated NOTIFY did not remove unsubscribed dialog")
	}
}

func TestSuccessfulUnsubscribeWaitStartsAtFinalResponseFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
			sipServer := sip.NewServer(platform)
			server := &Server{Server: sipServer, fromAddress: *platform}
			api := &GB28181API{svr: server}
			server.gb = api

			localRaw, remoteRaw := net.Pipe()
			connection := sip.NewTCPConnection(localRaw)
			go sipServer.ProcessTCPConnection(connection)
			cancelReceived := make(chan struct{})
			releaseResponse := make(chan struct{})
			responseReleased := false
			t.Cleanup(func() {
				if !responseReleased {
					close(releaseResponse)
				}
				_ = remoteRaw.Close()
				sipServer.Close()
			})
			runtime := &Device{IsOnline: true, gbVersion: string(version)}
			runtime.UpdateRuntime(func(current *Device) {
				current.conn = connection
				current.source = connection.RemoteAddr()
				current.to = device
			})
			server.memoryStorer = &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}

			remoteErr := make(chan error, 1)
			go func() {
				_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(remoteRaw)
				for index := 0; index < 2; index++ {
					request, err := readAnnexGTestSIPFrame(reader)
					if err != nil {
						remoteErr <- err
						return
					}
					extra := ""
					if index == 0 {
						extra = "Expires: 60"
					} else {
						close(cancelReceived)
						<-releaseResponse
					}
					response := annexGTestSIPResponse(request, http.StatusOK, "OK", extra)
					if index == 0 {
						to := annexGTestSIPHeader(request, "To")
						response = strings.Replace(response, "To: "+to+"\r\n", "To: "+to+";tag=unsubscribe-window\r\n", 1)
					}
					if _, err = remoteRaw.Write([]byte(response)); err != nil {
						remoteErr <- err
						return
					}
				}
				remoteErr <- nil
			}()

			input := &SubscribeInput{DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60}
			if err := api.Subscribe(t.Context(), input); err != nil {
				t.Fatalf("initial Alarm SUBSCRIBE failed: %v", err)
			}
			var dialog *outgoingSubscriptionDialog
			api.outgoingSubscriptions.Range(func(_, value any) bool {
				dialog, _ = value.(*outgoingSubscriptionDialog)
				return false
			})
			if dialog == nil {
				t.Fatal("initial subscription dialog missing")
			}

			cancel := *input
			cancel.Cancel = true
			cancelDone := make(chan error, 1)
			go func() {
				cancelDone <- api.Subscribe(t.Context(), &cancel)
			}()
			select {
			case <-cancelReceived:
			case <-time.After(time.Second):
				t.Fatal("unsubscribe request was not received")
			}
			responseReleasedAt := time.Now()
			close(releaseResponse)
			responseReleased = true
			select {
			case err := <-cancelDone:
				if err != nil {
					t.Fatalf("Alarm unsubscribe failed: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("unsubscribe did not finish after final response")
			}
			if err := <-remoteErr; err != nil {
				t.Fatal(err)
			}

			minimumDeadline := responseReleasedAt.Add(outgoingUnsubscribeNotifyWait)
			dialog.mu.Lock()
			expiresAt := dialog.expiresAt
			dialog.mu.Unlock()
			if expiresAt.Before(minimumDeadline) {
				t.Fatalf("unsubscribe wait deadline = %v, want no earlier than final response deadline %v", expiresAt, minimumDeadline)
			}
			if notifyExpiresAt := dialog.snapshotNotifyDialog().expiresAt; notifyExpiresAt.Before(minimumDeadline) {
				t.Fatalf("NOTIFY wait deadline = %v, want no earlier than final response deadline %v", notifyExpiresAt, minimumDeadline)
			}
		})
	}
}

func TestLateActiveNotifyDoesNotShortenSuccessfulUnsubscribeWaitFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			connection := newFlowConnection()
			callID := "late-active-notify-unsubscribe-" + string(version)
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
			response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
			deadline := time.Now().Add(outgoingUnsubscribeNotifyWait)
			dialog := &outgoingSubscriptionDialog{
				response:  response,
				deviceID:  gb10DeviceID,
				expiresAt: deadline,
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			dialog.notify.expiresAt = deadline
			dialog.cancelPending.Store(true)

			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			api.outgoingSubscriptions.Store(callID, dialog)

			request := newFlowRequest(t, connection, sip.MethodNotify, callID, []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
			applyTerminatedNotifyDialog(t, request, response)
			request.RemoveHeader("Contact")
			contact, err := sip.ParseURI("sip:late-notify.example:5080")
			if err != nil {
				t.Fatalf("parse late NOTIFY Contact: %v", err)
			}
			request.AppendHeader(&sip.ContactHeader{Address: contact})
			cseq, _ := request.CSeq()
			cseq.SeqNo = 2
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=1"})
			validatedAt := time.Now()
			wantReportedExpiresAt := validatedAt.Add(time.Second)

			if _, err := api.validateOutgoingSubscriptionNotifyModeAt(true, validatedAt, gb10DeviceID, request, "Alarm", gb10DeviceID); err != nil {
				t.Fatalf("late active NOTIFY rejected: %v", err)
			}
			snapshot := dialog.snapshotNotifyDialog()
			if snapshot.cseq != cseq.SeqNo {
				t.Fatalf("late NOTIFY CSeq = %d, want %d", snapshot.cseq, cseq.SeqNo)
			}
			if snapshot.routeRequest == nil || firstSingleHeaderValue(snapshot.routeRequest, "Contact") != firstSingleHeaderValue(request, "Contact") {
				t.Fatalf("late NOTIFY Contact = %q, want %q",
					firstSingleHeaderValue(snapshot.routeRequest, "Contact"), firstSingleHeaderValue(request, "Contact"))
			}
			if !snapshot.reportedExpiresAt.Equal(wantReportedExpiresAt) {
				t.Fatalf("late NOTIFY reported expiry = %v, want %v", snapshot.reportedExpiresAt, wantReportedExpiresAt)
			}
			if !snapshot.expiresAt.Equal(deadline) {
				t.Fatalf("late NOTIFY shortened Timer N from %v to %v", deadline, snapshot.expiresAt)
			}
			dialog.mu.Lock()
			expiresAt := dialog.expiresAt
			dialog.mu.Unlock()
			if !expiresAt.Equal(deadline) {
				t.Fatalf("late NOTIFY shortened outer Timer N from %v to %v", deadline, expiresAt)
			}
		})
	}
}

func TestFailedUnsubscribePreservesShortestLateNotifyExpiryFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			connection := newFlowConnection()
			callID := "failed-unsubscribe-late-notify-expiry-" + string(version)
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
			response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
			originalExpiry := time.Now().Add(time.Minute)
			dialog := &outgoingSubscriptionDialog{
				response:  response,
				deviceID:  gb10DeviceID,
				expiresAt: originalExpiry,
				refreshAt: time.Now().Add(50 * time.Second),
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			previousNotify := dialog.snapshotNotifyDialog()
			cancelDeadline := time.Now().Add(outgoingUnsubscribeNotifyWait)
			dialog.notify.expiresAt = cancelDeadline
			dialog.cancelPending.Store(true)

			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			api.outgoingSubscriptions.Store(callID, dialog)
			validatedAt := time.Now()
			shortestExpiry := validatedAt.Add(2 * time.Second)

			commitNotify := func(cseq uint32, expires int, host string) {
				t.Helper()
				request := newFlowRequest(t, connection, sip.MethodNotify, callID, []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
				applyTerminatedNotifyDialog(t, request, response)
				request.RemoveHeader("Contact")
				contact, err := sip.ParseURI("sip:" + host)
				if err != nil {
					t.Fatalf("parse late NOTIFY Contact: %v", err)
				}
				request.AppendHeader(&sip.ContactHeader{Address: contact})
				sequence, _ := request.CSeq()
				sequence.SeqNo = cseq
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: fmt.Sprintf("active;expires=%d", expires)})
				if _, err := api.validateOutgoingSubscriptionNotifyModeAt(true, validatedAt, gb10DeviceID, request, "Alarm", gb10DeviceID); err != nil {
					t.Fatalf("late active NOTIFY %d rejected: %v", cseq, err)
				}
			}

			commitNotify(1, 2, "short-expiry.example:5070")
			commitNotify(2, 20, "latest-target.example:5080")
			pending := dialog.snapshotNotifyDialog()
			if !pending.expiresAt.Equal(cancelDeadline) {
				t.Fatalf("late NOTIFY changed pending Timer N from %v to %v", cancelDeadline, pending.expiresAt)
			}
			if !pending.reportedExpiresAt.Equal(shortestExpiry) {
				t.Fatalf("pending reported expiry = %v, want shortest %v", pending.reportedExpiresAt, shortestExpiry)
			}

			dialog.mu.Lock()
			dialog.cancelPending.Store(false)
			dialog.restoreNotifyDialogLocked(previousNotify, time.Now())
			restoredOuterExpiry := dialog.expiresAt
			dialog.mu.Unlock()
			restored := dialog.snapshotNotifyDialog()
			if restored.cseq != 2 {
				t.Fatalf("restored NOTIFY CSeq = %d, want 2", restored.cseq)
			}
			if restored.routeRequest == nil || !strings.Contains(firstSingleHeaderValue(restored.routeRequest, "Contact"), "latest-target.example") {
				t.Fatalf("restored NOTIFY Contact = %q, want latest target", firstSingleHeaderValue(restored.routeRequest, "Contact"))
			}
			if !restored.expiresAt.Equal(shortestExpiry) {
				t.Fatalf("restored NOTIFY expiry = %v, want shortest %v", restored.expiresAt, shortestExpiry)
			}
			if !restoredOuterExpiry.Equal(shortestExpiry) {
				t.Fatalf("restored outer expiry = %v, want shortest %v", restoredOuterExpiry, shortestExpiry)
			}
		})
	}
}

func TestEarlyNotifyExpiryDoesNotBlockPendingSubscribe(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "early-nonblocking", []byte("query"))
	dialog := &outgoingSubscriptionDialog{eventValue: "presence"}
	dialog.setPendingNotifyDialog(subscribe, "Alarm", gb10DeviceID, gb10DeviceID, 60)
	api := &GB28181API{}
	api.outgoingSubscriptions.Store("early-nonblocking", dialog)

	notify := newFlowRequest(t, connection, sip.MethodNotify, "early-nonblocking", []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
	notify.RemoveHeader("To")
	local, _ := subscribe.From()
	notify.AppendHeader(&sip.ToHeader{Address: local.Address.Clone(), Params: local.Params.Clone()})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=20"})

	dialog.mu.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, notify, "Alarm", gb10DeviceID)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("early NOTIFY rejected: %v", err)
		}
	case <-time.After(time.Second):
		dialog.mu.Unlock()
		t.Fatal("early NOTIFY blocked behind pending SUBSCRIBE")
	}
	dialog.mu.Unlock()

	snapshot := dialog.snapshotNotifyDialog()
	if snapshot.reportedExpiresAt.IsZero() || snapshot.expiresAt.IsZero() {
		t.Fatal("early NOTIFY expiry was not preserved for final response reconciliation")
	}
	if snapshot.routeRequest == nil {
		t.Fatal("early NOTIFY did not seed the subscription dialog route")
	}
}

func TestActiveNotifyMiddlewareRejectsMissingDialog(t *testing.T) {
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodNotify, "missing-active-subscription", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	api := &GB28181API{}
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("missing-active-subscription", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	if response := <-flowResponse(t, connection); !strings.Contains(response, "SIP/2.0 481") {
		t.Fatalf("unsolicited active NOTIFY response = %s", response)
	}
}

func TestNonEmptyNotifyCommitsSubscriptionOnlyAfterBusinessValidation(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	validBody := readGB10Fixture(t, "alarm-notify.xml")
	malformedBody := []byte(strings.Replace(string(validBody), "<SN>4</SN>", "<SN>4</SN><SN>5</SN>", 1))
	for _, test := range []struct {
		name        string
		body        []byte
		wantStatus  string
		wantRemoved bool
	}{
		{name: "malformed body", body: malformedBody, wantStatus: "SIP/2.0 400"},
		{name: "valid body", body: validBody, wantStatus: "SIP/2.0 200", wantRemoved: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			connection := newFlowConnection()
			callID := "validated-notify-" + strings.ReplaceAll(test.name, " ", "-")
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
			dialog := &outgoingSubscriptionDialog{
				response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil),
				deviceID: gb10DeviceID,
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			before := dialog.snapshotNotifyDialog()

			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			key := "validated-notify-key-" + test.name
			api.outgoingSubscriptions.Store(key, dialog)

			request := newFlowRequest(t, connection, sip.MethodNotify, callID, test.body)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction(callID, connection),
				DeviceID: gb10DeviceID, Source: connection.remote,
			}

			api.sipNotifySubscriptionState(ctx)
			if current, exists := api.outgoingSubscriptions.Load(key); !exists || current != dialog {
				t.Fatal("NOTIFY middleware changed the subscription before business validation")
			}
			if after := dialog.snapshotNotifyDialog(); after != before {
				t.Fatalf("NOTIFY middleware changed dialog state before validation: before=%+v after=%+v", before, after)
			}

			api.sipNotifyAlarm(ctx)
			if response := <-flowResponse(t, connection); !strings.Contains(response, test.wantStatus) {
				t.Fatalf("NOTIFY response = %s; want %s", response, test.wantStatus)
			}
			_, exists := api.outgoingSubscriptions.Load(key)
			if exists == test.wantRemoved {
				t.Fatalf("subscription existence = %t, want removed=%t", exists, test.wantRemoved)
			}
			if !test.wantRemoved {
				if after := dialog.snapshotNotifyDialog(); after != before {
					t.Fatalf("invalid NOTIFY changed dialog state: before=%+v after=%+v", before, after)
				}
			}
		})
	}
}

func TestNonEmptyNotifyCommitsSubscriptionOnlyAfterSuccessfulSIPOK(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	for _, test := range []struct {
		name     string
		writeErr error
		removed  bool
	}{
		{name: "success", removed: true},
		{name: "write failure", writeErr: errors.New("write failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newFlowConnection()
			conn := &blockingFlowResponseConnection{
				flowConnection: base,
				started:        make(chan struct{}, 1),
				release:        make(chan struct{}),
				writeErr:       test.writeErr,
			}
			callID := "notify-sip-commit-" + strings.ReplaceAll(test.name, " ", "-")
			subscribe := newFlowRequest(t, base, sip.MethodSubscribe, callID, []byte("query"))
			dialog := &outgoingSubscriptionDialog{
				response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil),
				deviceID: gb10DeviceID,
			}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			before := dialog.snapshotNotifyDialog()

			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			key := "notify-sip-commit-key-" + test.name
			api.outgoingSubscriptions.Store(key, dialog)

			request := newFlowRequest(t, base, sip.MethodNotify, callID, readGB10Fixture(t, "alarm-notify.xml"))
			request.SetConnection(conn)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction(callID, conn),
				DeviceID: gb10DeviceID, Source: base.remote, Log: slog.Default(),
			}
			api.sipNotifySubscriptionState(ctx)
			if after := dialog.snapshotNotifyDialog(); after != before {
				t.Fatalf("NOTIFY middleware changed dialog state: before=%+v after=%+v", before, after)
			}

			done := make(chan struct{})
			go func() {
				api.sipNotifyAlarm(ctx)
				close(done)
			}()
			select {
			case <-conn.started:
			case <-time.After(time.Second):
				close(conn.release)
				t.Fatal("NOTIFY SIP response write did not start")
			}
			if current, exists := api.outgoingSubscriptions.Load(key); !exists || current != dialog {
				close(conn.release)
				<-done
				t.Fatal("NOTIFY changed subscription before SIP 200 was written")
			}
			if after := dialog.snapshotNotifyDialog(); after != before {
				close(conn.release)
				<-done
				t.Fatalf("NOTIFY changed dialog before SIP 200: before=%+v after=%+v", before, after)
			}
			finishBlockingFlowHandler(t, conn, done)

			_, exists := api.outgoingSubscriptions.Load(key)
			if exists == test.removed {
				t.Fatalf("subscription existence = %t, want removed=%t", exists, test.removed)
			}
			if !test.removed {
				if after := dialog.snapshotNotifyDialog(); after != before {
					t.Fatalf("failed SIP response changed dialog: before=%+v after=%+v", before, after)
				}
			}
		})
	}
}

func TestConcurrentNotifyRequestsSerializeDialogCommit(t *testing.T) {
	callID := "concurrent-notify-dialog"
	flowConn := newFlowConnection()
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	dialogCallID := sip.CallID(callID)
	subscribe := sip.NewRequest("", sip.MethodSubscribe, remote.URI.Clone(), sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetCallID(&dialogCallID).
			SetMethod(sip.MethodSubscribe).SetSeqNo(1).AddVia(&sip.ViaHop{
			Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP",
			Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
		}).Build(), []byte("query"))
	dialog := &outgoingSubscriptionDialog{
		response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		deviceID: gb10DeviceID,
	}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)

	api := &GB28181API{}
	api.outgoingSubscriptions.Store("concurrent-notify-key", dialog)
	server := sip.NewServer(local)
	t.Cleanup(server.Close)
	started := make(chan uint32, 2)
	committed := make(chan uint32, 2)
	server.Notify(api.sipNotifySubscriptionState).Handle("Alarm", func(ctx *sip.Context) {
		cseq, _ := ctx.Request.CSeq()
		started <- cseq.SeqNo
		if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
			return
		}
		if api.commitOutgoingSubscriptionNotifyAfterResponse(ctx) {
			committed <- cseq.SeqNo
		}
	})

	newPeer := func() net.Conn {
		serverRaw, peer := net.Pipe()
		go server.ProcessTCPConnection(sip.NewTCPConnection(serverRaw))
		t.Cleanup(func() { _ = peer.Close() })
		return peer
	}
	newNotify := func(cseq uint32) string {
		body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
		request := newFlowRequest(t, flowConn, sip.MethodNotify, callID, body)
		applyTerminatedNotifyDialog(t, request, dialog.response)
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
		requestCSeq, _ := request.CSeq()
		requestCSeq.SeqNo = cseq
		via, _ := request.ViaHop()
		via.Transport = "TCP"
		return request.String()
	}
	writeRequest := func(peer net.Conn, request string) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := peer.Write([]byte(request))
			done <- err
		}()
		return done
	}
	waitWrite := func(done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("NOTIFY request write timed out")
		}
	}
	waitSequence := func(ch <-chan uint32, want uint32, label string) {
		t.Helper()
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("%s CSeq = %d, want %d", label, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s CSeq %d", label, want)
		}
	}
	readResponse := func(peer net.Conn) string {
		t.Helper()
		if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		frame, err := readAnnexGTestSIPFrame(bufio.NewReader(peer))
		if err != nil {
			t.Fatal(err)
		}
		return frame
	}

	firstPeer := newPeer()
	waitWrite(writeRequest(firstPeer, newNotify(1)))
	waitSequence(started, 1, "started")

	secondPeer := newPeer()
	waitWrite(writeRequest(secondPeer, newNotify(2)))
	select {
	case got := <-started:
		t.Fatalf("higher CSeq %d entered the handler before lower CSeq completed", got)
	case <-time.After(100 * time.Millisecond):
	}

	if response := readResponse(firstPeer); !strings.Contains(response, "SIP/2.0 200") {
		t.Fatalf("first NOTIFY response = %s", response)
	}
	waitSequence(committed, 1, "committed")
	waitSequence(started, 2, "started")
	if response := readResponse(secondPeer); !strings.Contains(response, "SIP/2.0 200") {
		t.Fatalf("second NOTIFY response = %s", response)
	}
	waitSequence(committed, 2, "committed")
}

func TestOutgoingSubscriptionCleanupWaitsForAcceptedNotify(t *testing.T) {
	connection := newFlowConnection()
	callID := "cleanup-during-accepted-notify"
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
	dialog := &outgoingSubscriptionDialog{
		response:  sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		deviceID:  gb10DeviceID,
		expiresAt: time.Now().Add(-time.Minute),
	}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	dialog.notify.expiresAt = time.Now().Add(-time.Minute)
	api := &GB28181API{}
	key := "cleanup-during-accepted-notify-key"
	api.outgoingSubscriptions.Store(key, dialog)

	request := newFlowRequest(t, connection, sip.MethodNotify, callID,
		[]byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
	_, _, unlock, err := api.lockValidatedOutgoingSubscriptionNotify(gb10DeviceID, request, "Alarm", gb10DeviceID)
	if err != nil {
		t.Fatal(err)
	}

	cleaned := make(chan struct{})
	go func() {
		api.cleanupOutgoingSubscription(key, dialog, time.Now())
		close(cleaned)
	}()
	select {
	case <-cleaned:
		unlock()
		t.Fatal("subscription cleanup passed an accepted NOTIFY operation")
	case <-time.After(100 * time.Millisecond):
	}
	if current, exists := api.outgoingSubscriptions.Load(key); !exists || current != dialog {
		unlock()
		t.Fatal("subscription was removed before accepted NOTIFY completed")
	}

	unlock()
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("subscription cleanup did not resume after NOTIFY completed")
	}
	if _, exists := api.outgoingSubscriptions.Load(key); exists {
		t.Fatal("expired subscription remained after serialized cleanup")
	}
}

func TestOutgoingSubscriptionCleanupDoesNotInvertSubscribeLockOrder(t *testing.T) {
	api := &GB28181API{}
	now := time.Now()
	key := "outgoing-cleaner-lock-order"
	dialog := &outgoingSubscriptionDialog{expiresAt: now.Add(time.Minute)}
	api.outgoingSubscriptions.Store(key, dialog)

	// 模拟 Subscribe 正持有对话锁执行网络请求。再预占 NOTIFY 操作锁，保证清理器
	// 已经排队后才释放；旧锁序会让清理器取得 NOTIFY 锁并等待对话锁。
	dialog.mu.Lock()
	dialog.notifyOperationMu.Lock()
	started := make(chan struct{})
	cleaned := make(chan struct{})
	go func() {
		close(started)
		api.cleanupOutgoingSubscription(key, dialog, now)
		close(cleaned)
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	dialog.notifyOperationMu.Unlock()
	time.Sleep(20 * time.Millisecond)

	notifyLockAvailable := dialog.notifyOperationMu.TryLock()
	if notifyLockAvailable {
		dialog.notifyOperationMu.Unlock()
	}
	dialog.mu.Unlock()
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("subscription cleanup deadlocked after releasing dialog lock")
	}
	if !notifyLockAvailable {
		t.Fatal("subscription cleanup held NOTIFY operation lock while waiting for dialog lock")
	}
}

func TestDeviceSubscriptionReleaseWaitsForAcceptedNotify(t *testing.T) {
	api := &GB28181API{}
	dialog := &outgoingSubscriptionDialog{deviceID: gb10DeviceID}
	key := gb10DeviceID + "|release-during-accepted-notify"
	api.outgoingSubscriptions.Store(key, dialog)
	dialog.notifyOperationMu.Lock()

	released := make(chan struct{})
	go func() {
		api.releaseOutgoingSubscriptionsForDeviceContext(t.Context(), gb10DeviceID)
		close(released)
	}()
	select {
	case <-released:
		dialog.notifyOperationMu.Unlock()
		t.Fatal("device subscription release passed an accepted NOTIFY operation")
	case <-time.After(100 * time.Millisecond):
	}
	if current, exists := api.outgoingSubscriptions.Load(key); !exists || current != dialog {
		dialog.notifyOperationMu.Unlock()
		t.Fatal("device subscription was removed before accepted NOTIFY completed")
	}

	dialog.notifyOperationMu.Unlock()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("device subscription release did not resume after NOTIFY completed")
	}
	if _, exists := api.outgoingSubscriptions.Load(key); exists {
		t.Fatal("device subscription remained after serialized release")
	}
}

func TestAcceptedNotifyCommitsAfterSubscriptionExpiry(t *testing.T) {
	callID := "accepted-notify-crosses-expiry"
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	dialogCallID := sip.CallID(callID)
	subscribe := sip.NewRequest("", sip.MethodSubscribe, remote.URI.Clone(), sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetCallID(&dialogCallID).
			SetMethod(sip.MethodSubscribe).SetSeqNo(1).AddVia(&sip.ViaHop{
			Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP",
			Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
		}).Build(), []byte("query"))
	dialog := &outgoingSubscriptionDialog{
		response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		deviceID: gb10DeviceID,
	}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)

	api := &GB28181API{}
	api.outgoingSubscriptions.Store("accepted-notify-crosses-expiry-key", dialog)
	server := sip.NewServer(local)
	t.Cleanup(server.Close)
	started := make(chan struct{}, 1)
	committed := make(chan bool, 1)
	server.Notify(api.sipNotifySubscriptionState).Handle("Alarm", func(ctx *sip.Context) {
		started <- struct{}{}
		if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
			committed <- false
			return
		}
		committed <- api.commitOutgoingSubscriptionNotifyAfterResponse(ctx)
	})

	serverRaw, peer := net.Pipe()
	go server.ProcessTCPConnection(sip.NewTCPConnection(serverRaw))
	t.Cleanup(func() { _ = peer.Close() })
	flowConn := newFlowConnection()
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	request := newFlowRequest(t, flowConn, sip.MethodNotify, callID, body)
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	via, _ := request.ViaHop()
	via.Transport = "TCP"
	expiresAt := time.Now().Add(time.Second)
	dialog.mu.Lock()
	dialog.expiresAt = expiresAt
	dialog.mu.Unlock()
	dialog.notifyMu.Lock()
	dialog.notify.expiresAt = expiresAt
	dialog.notifyMu.Unlock()

	written := make(chan error, 1)
	go func() {
		_, err := peer.Write([]byte(request.String()))
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("NOTIFY request write timed out")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("accepted NOTIFY did not enter handler before expiry")
	}
	if wait := time.Until(expiresAt) + 20*time.Millisecond; wait > 0 {
		time.Sleep(wait)
	}
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := readAnnexGTestSIPFrame(bufio.NewReader(peer))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response, "SIP/2.0 200") {
		t.Fatalf("NOTIFY response = %s", response)
	}
	select {
	case ok := <-committed:
		if !ok {
			t.Fatal("accepted NOTIFY was not committed after response crossed subscription expiry")
		}
	case <-time.After(time.Second):
		t.Fatal("accepted NOTIFY commit timed out")
	}
}

func TestActiveNotifyMiddlewareMapsEventErrorsWithoutChangingDialog(t *testing.T) {
	tests := []struct {
		name         string
		eventValues  []string
		wantResponse string
	}{
		{name: "missing", wantResponse: "SIP/2.0 400"},
		{name: "empty", eventValues: []string{""}, wantResponse: "SIP/2.0 400"},
		{name: "duplicate", eventValues: []string{"presence", "Catalog"}, wantResponse: "SIP/2.0 400"},
		{name: "unsupported package", eventValues: []string{"vendor-event"}, wantResponse: "SIP/2.0 489"},
		{name: "event package case differs", eventValues: []string{"Presence"}, wantResponse: "SIP/2.0 489"},
		{name: "supported package dialog mismatch", eventValues: []string{"Catalog"}, wantResponse: "SIP/2.0 481"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			connection := newFlowConnection()
			callID := "notify-event-error-" + strings.ReplaceAll(test.name, " ", "-")
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, callID, []byte("query"))
			dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			before := dialog.snapshotNotifyDialog()
			api := &GB28181API{}
			api.outgoingSubscriptions.Store("alarm-key", dialog)

			body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
			request := newFlowRequest(t, connection, sip.MethodNotify, callID, body)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			for _, value := range test.eventValues {
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: value})
			}
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=30"})
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction(callID, connection), DeviceID: gb10DeviceID, Source: connection.remote,
			}

			api.sipNotifySubscriptionState(ctx)
			if response := <-flowResponse(t, connection); !strings.Contains(response, test.wantResponse) {
				t.Fatalf("Event response = %s; want %s", response, test.wantResponse)
			}
			if after := dialog.snapshotNotifyDialog(); after != before {
				t.Fatalf("invalid Event changed subscription dialog: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestActiveMobilePositionNotifyStoresMatchedDialogKey(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "active-mobile-position", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "MobilePosition", gb10ChannelID)
	api := &GB28181API{}
	key := "mobile-position-key"
	api.outgoingSubscriptions.Store(key, dialog)
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>1</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodNotify, "active-mobile-position", body)
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	ctx := &sip.Context{Request: request, Tx: sip.NewTransaction("active-mobile-position", connection), DeviceID: gb10DeviceID, Source: connection.remote}
	api.sipNotifySubscriptionState(ctx)
	matched, ok := ctx.Get(outgoingSubscriptionNotifyContextKey)
	if !ok || matched != key {
		t.Fatalf("matched MobilePosition subscription key = %#v, %v", matched, ok)
	}
}

func TestPendingNotifyBindsRemoteTagBeforeSubscribeResponse(t *testing.T) {
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodSubscribe, "pending-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{eventValue: "Catalog;id=" + gb10DeviceID}
	dialog.setPendingNotifyDialog(request, "Catalog", gb10DeviceID, gb10DeviceID, 60)
	api := &GB28181API{}
	api.outgoingSubscriptions.Store("pending-key", dialog)

	notify := newFlowRequest(t, connection, sip.MethodNotify, "pending-subscription", []byte(`<Notify><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
	notify.RemoveHeader("To")
	local, _ := request.From()
	notify.AppendHeader(&sip.ToHeader{Address: local.Address.Clone(), Params: local.Params.Clone()})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "pending;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, notify, "Catalog", gb10DeviceID); err != nil {
		t.Fatalf("early pending NOTIFY rejected: %v", err)
	}

	foreign := newFlowRequest(t, connection, sip.MethodNotify, "pending-subscription", notify.Body())
	foreign.RemoveHeader("To")
	foreign.AppendHeader(&sip.ToHeader{Address: local.Address.Clone(), Params: local.Params.Clone()})
	foreign.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	foreign.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	cseq, _ := foreign.CSeq()
	cseq.SeqNo = 2
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, foreign, "Catalog", gb10DeviceID); err == nil {
		t.Fatal("second remote tag replaced early NOTIFY dialog binding")
	}
}

func applyTerminatedNotifyDialog(t *testing.T, request *sip.Request, response *sip.Response) {
	t.Helper()
	request.RemoveHeader("From")
	request.RemoveHeader("To")
	remote, ok := response.To()
	if !ok || remote == nil {
		t.Fatal("subscription response missing remote address")
	}
	local, ok := response.From()
	if !ok || local == nil {
		t.Fatal("subscription response missing local address")
	}
	request.AppendHeader(&sip.FromHeader{Address: remote.Address.Clone(), Params: remote.Params.Clone()})
	request.AppendHeader(&sip.ToHeader{Address: local.Address.Clone(), Params: local.Params.Clone()})
}

func applyInboundSubscribeDialog(t *testing.T, request *sip.Request, localTag string, cseq uint32) {
	t.Helper()
	to, ok := request.To()
	if !ok || to == nil || to.Address == nil {
		t.Fatal("SUBSCRIBE request missing To header")
	}
	params := sip.NewParams()
	if to.Params != nil {
		params = to.Params.Clone()
	}
	if strings.TrimSpace(localTag) != "" {
		params.Add("tag", sip.String{Str: localTag})
	}
	request.RemoveHeader("To")
	request.AppendHeader(&sip.ToHeader{DisplayName: to.DisplayName, Address: to.Address.Clone(), Params: params})
	current, ok := request.CSeq()
	if !ok || current == nil {
		t.Fatal("SUBSCRIBE request missing CSeq")
	}
	current.SeqNo = cseq
}

func TestCatalogSubscriptionInitialRenewCancel11(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>50</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)

	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-1", body)
	initialFrom, _ := req.From()
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	tx := sip.NewTransaction("subscribe-1-tx", conn)
	ctx := &sip.Context{
		Request:  req,
		Tx:       tx,
		DeviceID: gb10PlatformID,
		Source:   conn.remote,
		To:       mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"),
		XGBVer:   string(GBVersion11),
	}
	api.sipSubscribeEvent(ctx)
	initialResponse := <-flowResponse(t, conn)
	assertFlowOK(t, initialResponse)
	if !strings.Contains(initialResponse, "Event: Catalog;id="+gb10DeviceID) || !strings.Contains(initialResponse, "Expires: 60") {
		t.Fatalf("initial SUBSCRIBE response missing dialog headers:\n%s", initialResponse)
	}

	var key string
	var firstExpiry time.Time
	var firstSubscription *eventSubscription
	api.eventSubscribers.Range(func(storedKey, value any) bool {
		key = storedKey.(string)
		sub := value.(*eventSubscription)
		firstSubscription = sub
		sub.CSeq = 7
		firstExpiry = sub.ExpiresAt
		if sub.Event != "Catalog;id="+gb10DeviceID {
			t.Errorf("stored Event = %q", sub.Event)
		}
		return false
	})
	if key == "" {
		t.Fatal("initial subscription was not stored")
	}

	req = newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-1", body)
	req.RemoveHeader("From")
	req.AppendHeader(initialFrom.Clone())
	applyInboundSubscribeDialog(t, req, firstSubscription.LocalTag, 2)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "120"})
	ctx.Request = req
	ctx.Tx = sip.NewTransaction("subscribe-renew-tx", conn)
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))
	value, ok := api.eventSubscribers.Load(key)
	if !ok || !value.(*eventSubscription).ExpiresAt.After(firstExpiry) {
		t.Fatal("subscription renewal did not extend expiry")
	}
	renewed := value.(*eventSubscription)
	if renewed != firstSubscription || renewed.CSeq != 7 {
		t.Fatalf("subscription renewal replaced dialog state: first=%p renewed=%p CSeq=%d", firstSubscription, renewed, renewed.CSeq)
	}

	req = newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-1", body)
	req.RemoveHeader("From")
	req.AppendHeader(initialFrom.Clone())
	applyInboundSubscribeDialog(t, req, firstSubscription.LocalTag, 3)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "0"})
	ctx.Request = req
	ctx.Tx = sip.NewTransaction("subscribe-cancel-tx", conn)
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))
	if _, ok := api.eventSubscribers.Load(key); ok {
		t.Fatal("subscription cancel did not remove state")
	}
}

func TestInboundSubscribeRejectsEventPackageChangeOnRenewal(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>58</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	const callID = "subscribe-event-package-change"

	request := newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
	from, _ := request.From()
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction(callID+"-create", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
	}
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))

	var subscription *eventSubscription
	api.eventSubscribers.Range(func(_, value any) bool {
		subscription, _ = value.(*eventSubscription)
		return false
	})
	if subscription == nil {
		t.Fatal("initial subscription was not stored")
	}
	subscription.mu.Lock()
	localTag := subscription.LocalTag
	initialExpiry := subscription.ExpiresAt
	initialEvent := subscription.Event
	subscription.mu.Unlock()

	renewal := newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
	renewal.RemoveHeader("From")
	renewal.AppendHeader(from.Clone())
	applyInboundSubscribeDialog(t, renewal, localTag, 2)
	renewal.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	renewal.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "120"})
	ctx.Request = renewal
	ctx.Tx = sip.NewTransaction(callID+"-renew", conn)
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, conn)
	if !strings.Contains(response, "SIP/2.0 481") {
		t.Fatalf("Event package change was accepted; response:\n%s", response)
	}

	current, ok := api.eventSubscribers.Load(subscription.Key)
	if !ok || current != subscription {
		t.Fatal("event package change altered the existing subscription")
	}
	subscription.mu.Lock()
	defer subscription.mu.Unlock()
	if subscription.Event != initialEvent || !subscription.ExpiresAt.Equal(initialExpiry) {
		t.Fatalf("event package change mutated subscription: event=%q expiry=%v", subscription.Event, subscription.ExpiresAt)
	}
}

func TestInboundSubscribeCommitsOnlyAfterSuccessfulSIPOK(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>57</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	const callID = "subscribe-confirmed-commit"

	request := newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
	initialFrom, _ := request.From()
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	ctx := &sip.Context{
		Request: request, DeviceID: gb10PlatformID, Source: conn.remote,
		To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11), Log: slog.Default(),
	}
	failResponse := func(txID string) {
		t.Helper()
		failing := &blockingFlowResponseConnection{
			flowConnection: conn,
			started:        make(chan struct{}, 1),
			release:        make(chan struct{}),
			writeErr:       errors.New("write failed"),
		}
		close(failing.release)
		ctx.Request.SetConnection(failing)
		ctx.Tx = sip.NewTransaction(txID, failing)
		api.sipSubscribeEvent(ctx)
	}
	retryResponse := func(txID string) {
		t.Helper()
		ctx.Request.SetConnection(conn)
		ctx.Tx = sip.NewTransaction(txID, conn)
		api.sipSubscribeEvent(ctx)
		assertFlowOK(t, <-flowResponse(t, conn))
	}
	loadSubscription := func() *eventSubscription {
		t.Helper()
		var result *eventSubscription
		api.eventSubscribers.Range(func(_, value any) bool {
			result, _ = value.(*eventSubscription)
			return false
		})
		return result
	}

	failResponse("subscribe-confirmed-create-failure")
	if subscription := loadSubscription(); subscription != nil {
		t.Fatal("failed initial SUBSCRIBE response committed subscription state")
	}
	retryResponse("subscribe-confirmed-create-retry")
	subscription := loadSubscription()
	if subscription == nil {
		t.Fatal("initial SUBSCRIBE retry did not commit subscription state")
	}
	subscription.mu.Lock()
	localTag := subscription.LocalTag
	initialExpiry := subscription.ExpiresAt
	initialCSeq := subscription.RemoteCSeq
	subscription.mu.Unlock()

	request = newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
	request.RemoveHeader("From")
	request.AppendHeader(initialFrom.Clone())
	applyInboundSubscribeDialog(t, request, localTag, 2)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "120"})
	ctx.Request = request
	failResponse("subscribe-confirmed-refresh-failure")
	subscription.mu.Lock()
	failedRefreshExpiry := subscription.ExpiresAt
	failedRefreshCSeq := subscription.RemoteCSeq
	subscription.mu.Unlock()
	if !failedRefreshExpiry.Equal(initialExpiry) || failedRefreshCSeq != initialCSeq {
		t.Fatalf("failed SUBSCRIBE refresh committed state: expiry=%v CSeq=%d", failedRefreshExpiry, failedRefreshCSeq)
	}
	retryResponse("subscribe-confirmed-refresh-retry")
	subscription.mu.Lock()
	refreshedExpiry := subscription.ExpiresAt
	refreshedCSeq := subscription.RemoteCSeq
	subscription.mu.Unlock()
	if !refreshedExpiry.After(initialExpiry) || refreshedCSeq != 2 {
		t.Fatalf("SUBSCRIBE refresh retry state: expiry=%v CSeq=%d", refreshedExpiry, refreshedCSeq)
	}

	request = newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
	request.RemoveHeader("From")
	request.AppendHeader(initialFrom.Clone())
	applyInboundSubscribeDialog(t, request, localTag, 3)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "0"})
	ctx.Request = request
	failResponse("subscribe-confirmed-cancel-failure")
	if current := loadSubscription(); current != subscription {
		t.Fatal("failed SUBSCRIBE cancel response removed subscription state")
	}
	subscription.mu.Lock()
	failedCancelExpiry := subscription.ExpiresAt
	failedCancelCSeq := subscription.RemoteCSeq
	subscription.mu.Unlock()
	if !failedCancelExpiry.Equal(refreshedExpiry) || failedCancelCSeq != refreshedCSeq {
		t.Fatalf("failed SUBSCRIBE cancel committed state: expiry=%v CSeq=%d", failedCancelExpiry, failedCancelCSeq)
	}
	retryResponse("subscribe-confirmed-cancel-retry")
	if current := loadSubscription(); current != nil {
		t.Fatal("SUBSCRIBE cancel retry did not remove subscription state")
	}
}

func TestInboundSubscribeDoesNotPublishAfterOwnerGoesOfflineDuringResponse(t *testing.T) {
	api := &GB28181API{}
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
	}
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>61</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	request := newFlowRequest(t, base, sip.MethodSubscribe, "subscribe-owner-offline-during-response", body)
	request.SetConnection(connection)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	ctx := &sip.Context{
		Request: request,
		Tx:      sip.NewTransaction("subscribe-owner-offline-during-response-tx", connection),
		// 订阅目标可以是其他设备；清理必须按订阅所有者，而不是正文 DeviceID。
		DeviceID: gb10PlatformID,
		Source:   base.remote,
		To:       mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"),
		XGBVer:   string(GBVersion11),
		Log:      slog.Default(),
	}

	handlerDone := make(chan struct{})
	go func() {
		api.sipSubscribeEvent(ctx)
		close(handlerDone)
	}()
	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("SUBSCRIBE SIP response write did not start")
	}

	var operation *pendingDeviceOperation
	api.pendingDeviceRequests.Range(func(_, value any) bool {
		operation = pendingOperation(value)
		return false
	})
	if operation == nil || operation.deviceID != gb10PlatformID {
		close(connection.release)
		t.Fatal("inbound SUBSCRIBE was not tracked by its owner device")
	}

	cleanupDone := make(chan struct{})
	go func() {
		api.cleanupOfflineDeviceRuntime(gb10PlatformID)
		close(cleanupDone)
	}()
	select {
	case <-operation.Done():
		if !errors.Is(operation.Cause(), ErrDeviceOffline) {
			close(connection.release)
			t.Fatalf("inbound SUBSCRIBE cancellation cause = %v; want %v", operation.Cause(), ErrDeviceOffline)
		}
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("offline cleanup did not cancel the accepted inbound SUBSCRIBE")
	}

	close(connection.release)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("inbound SUBSCRIBE handler did not finish after response release")
	}
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("offline cleanup did not finish after inbound SUBSCRIBE handler")
	}
	assertFlowOK(t, <-flowResponse(t, base))
	if count := syncMapLen(&api.eventSubscribers); count != 0 {
		t.Fatalf("offline owner republished inbound subscription after successful SIP response: %d", count)
	}
	if _, exists := api.pendingDeviceRequests.Load(operation); exists {
		t.Fatal("offline inbound SUBSCRIBE pending operation survived handler completion")
	}
}

func TestInboundSubscribeRejectsInvalidDialogWithoutSideEffects(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>58</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	const callID = "subscribe-dialog-security"

	initial := newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
	initialFrom, _ := initial.From()
	initial.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	initial.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	ctx := &sip.Context{
		Request: initial, Tx: sip.NewTransaction("subscribe-dialog-security-initial", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
	}
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))

	var subscription *eventSubscription
	api.eventSubscribers.Range(func(_, value any) bool {
		subscription, _ = value.(*eventSubscription)
		return false
	})
	if subscription == nil || subscription.LocalTag == "" {
		t.Fatal("initial subscription dialog was not stored")
	}
	subscription.mu.Lock()
	subscription.Filter = eventSubscriptionFilter{AlarmMethod: "5"}
	subscription.DownstreamKeys = []string{"protected-downstream"}
	wantExpiry := subscription.ExpiresAt
	wantFilter := subscription.Filter
	wantDownstream := append([]string(nil), subscription.DownstreamKeys...)
	wantCSeq := subscription.RemoteCSeq
	localTag := subscription.LocalTag
	subscription.mu.Unlock()

	tests := []struct {
		name    string
		request string
		expires string
		toTag   string
		cseq    uint32
	}{
		{name: "create with To tag", request: callID + "-new", expires: "60", toTag: localTag, cseq: 1},
		{name: "refresh without To tag", request: callID, expires: "120", cseq: 2},
		{name: "refresh with wrong To tag", request: callID, expires: "120", toTag: "wrong-local-tag", cseq: 2},
		{name: "refresh replays CSeq", request: callID, expires: "120", toTag: localTag, cseq: 1},
		{name: "cancel without To tag", request: callID, expires: "0", cseq: 2},
		{name: "cancel with wrong To tag", request: callID, expires: "0", toTag: "wrong-local-tag", cseq: 2},
		{name: "cancel replays CSeq", request: callID, expires: "0", toTag: localTag, cseq: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newFlowRequest(t, conn, sip.MethodSubscribe, test.request, body)
			request.RemoveHeader("From")
			request.AppendHeader(initialFrom.Clone())
			applyInboundSubscribeDialog(t, request, test.toTag, test.cseq)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: test.expires})
			ctx.Request = request
			ctx.Tx = sip.NewTransaction("subscribe-dialog-security-"+test.name, conn)
			api.sipSubscribeEvent(ctx)
			if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 481") {
				t.Fatalf("invalid SUBSCRIBE response = %s", response)
			}

			value, loaded := api.eventSubscribers.Load(subscription.Key)
			if !loaded || value != subscription {
				t.Fatal("invalid SUBSCRIBE replaced or removed the existing dialog")
			}
			subscription.mu.Lock()
			gotExpiry := subscription.ExpiresAt
			gotFilter := subscription.Filter
			gotDownstream := append([]string(nil), subscription.DownstreamKeys...)
			gotCSeq := subscription.RemoteCSeq
			subscription.mu.Unlock()
			if !gotExpiry.Equal(wantExpiry) || gotFilter != wantFilter || !slices.Equal(gotDownstream, wantDownstream) || gotCSeq != wantCSeq {
				t.Fatalf("invalid SUBSCRIBE changed state: expiry=%v filter=%+v downstream=%v CSeq=%d", gotExpiry, gotFilter, gotDownstream, gotCSeq)
			}
		})
	}
}

func TestInboundSubscribeRejectsDuplicateSingletonHeaders(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>60</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	for _, test := range []struct {
		name      string
		duplicate *sip.GenericHeader
	}{
		{name: "Event", duplicate: &sip.GenericHeader{HeaderName: "Event", Contents: "presence"}},
		{name: "Expires", duplicate: &sip.GenericHeader{HeaderName: "Expires", Contents: "120"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &GB28181API{}
			conn := newFlowConnection()
			request := newFlowRequest(t, conn, sip.MethodSubscribe, "duplicate-subscribe-"+test.name, body)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
			request.AppendHeader(test.duplicate)
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("duplicate-subscribe-"+test.name, conn), DeviceID: gb10PlatformID,
				Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
			}

			api.sipSubscribeEvent(ctx)
			if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("duplicate %s response = %s", test.name, response)
			}
			stored := false
			api.eventSubscribers.Range(func(_, _ any) bool {
				stored = true
				return false
			})
			if stored {
				t.Fatalf("duplicate %s created subscription state", test.name)
			}
		})
	}
}

func TestCatalogSubscriptionDialogIsolatedBySubscriber(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>59</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	const callID = "shared-subscribe-dialog"
	const fromTag = "shared-from-tag"
	makeContext := func(subscriberID, expires, txID string) *sip.Context {
		req := newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
		req.RemoveHeader("From")
		from := mustFlowAddress(t, "sip:"+subscriberID+"@3402000000")
		from.Params.Add("tag", sip.String{Str: fromTag})
		req.AppendHeader(&sip.FromHeader{Address: from.URI, Params: from.Params})
		req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
		req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
		return &sip.Context{
			Request: req, Tx: sip.NewTransaction(txID, conn), DeviceID: subscriberID,
			Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
		}
	}

	api.sipSubscribeEvent(makeContext(gb10PlatformID, "60", "subscriber-a"))
	assertFlowOK(t, <-flowResponse(t, conn))
	api.sipSubscribeEvent(makeContext("44010000002000000001", "0", "subscriber-b-cancel"))
	if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 481") {
		t.Fatalf("different subscriber cancel response = %s", response)
	}

	wantKey := buildEventSubscriptionKey("device:"+gb10PlatformID, callID, fromTag, "Catalog", gb10DeviceID)
	if _, ok := api.eventSubscribers.Load(wantKey); !ok {
		t.Fatal("different subscriber cancelled the existing dialog")
	}
}

func TestEventSubscriptionKeyIsolatesCascadeWorkers(t *testing.T) {
	first := newCascadeWorker(nil, testSharedCascadePlatform(t))
	secondPlatform := testSharedCascadePlatform(t)
	secondPlatform.name = "secondary"
	second := newCascadeWorker(nil, secondPlatform)
	ctx := &sip.Context{DeviceID: first.platform.serverID}

	firstKey := buildEventSubscriptionKey(subscriptionOwnerKey(ctx, first), "shared-call", "shared-tag", "Catalog", first.platform.localID)
	secondKey := buildEventSubscriptionKey(subscriptionOwnerKey(ctx, second), "shared-call", "shared-tag", "Catalog", first.platform.localID)
	if firstKey == secondKey {
		t.Fatalf("different cascade workers share subscription key %q", firstKey)
	}
	if buildEventSubscriptionKey("device:a", "b|c", "d", "Catalog", "e") ==
		buildEventSubscriptionKey("device:a", "b", "c|d", "Catalog", "e") {
		t.Fatal("subscription key is ambiguous when SIP dialog fields contain separators")
	}
}

func TestCatalogSubscriptionBusinessResponseVersionMatrix(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		event   string
		wantXML bool
	}{
		{name: "2011 traditional", version: GBVersion10, event: "presence", wantXML: true},
		{name: "2014 traditional", version: GBVersion11, event: "presence", wantXML: true},
		{name: "2016 traditional", version: GBVersion20, event: "presence", wantXML: true},
		{name: "2022 traditional", version: GBVersion30, event: "presence"},
		{name: "2014 interdomain", version: GBVersion11, event: "Catalog;id=1894"},
		{name: "2016 interdomain", version: GBVersion20, event: "Catalog;id=1894"},
		{name: "2022 interdomain", version: GBVersion30, event: "Catalog;id=1894"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := &GB28181API{}
			conn := newFlowConnection()
			body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>51</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
			req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-catalog-"+string(test.version), body)
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: test.event})
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
			ctx := &sip.Context{
				Request: req, Tx: sip.NewTransaction("subscribe-catalog-tx-"+string(test.version), conn), DeviceID: gb10PlatformID,
				Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(test.version),
			}
			api.sipSubscribeEvent(ctx)
			response := <-flowResponse(t, conn)
			for _, required := range []string{"Event: " + strings.Split(test.event, ";")[0], "Expires: 90"} {
				if !strings.Contains(response, required) {
					t.Fatalf("%s SUBSCRIBE response missing %q:\n%s", test.version, required, response)
				}
			}
			hasXML := strings.Contains(response, "<Response>") && strings.Contains(response, "<CmdType>Catalog</CmdType>") &&
				strings.Contains(response, "<SN>51</SN>") && strings.Contains(response, "<Result>OK</Result>")
			if hasXML != test.wantXML {
				t.Fatalf("%s Catalog SUBSCRIBE XML response = %v, want %v:\n%s", test.version, hasXML, test.wantXML, response)
			}
		})
	}
}

func TestEventNotifyBusinessResponseVersionMatrix(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		cmdType string
		event   string
		wantXML bool
	}{
		{name: "2011 Alarm", version: GBVersion10, cmdType: "Alarm", event: "presence", wantXML: true},
		{name: "2014 Alarm", version: GBVersion11, cmdType: "Alarm", event: "presence", wantXML: true},
		{name: "2016 Alarm", version: GBVersion20, cmdType: "Alarm", event: "presence", wantXML: true},
		{name: "2022 Alarm", version: GBVersion30, cmdType: "Alarm", event: "presence"},
		{name: "2011 traditional Catalog", version: GBVersion10, cmdType: "Catalog", event: "presence", wantXML: true},
		{name: "2014 traditional Catalog", version: GBVersion11, cmdType: "Catalog", event: "presence", wantXML: true},
		{name: "2016 traditional Catalog", version: GBVersion20, cmdType: "Catalog", event: "presence", wantXML: true},
		{name: "2022 traditional Catalog", version: GBVersion30, cmdType: "Catalog", event: "presence"},
		{name: "2014 interdomain Catalog", version: GBVersion11, cmdType: "Catalog", event: "Catalog;id=1894"},
		{name: "2016 interdomain Catalog", version: GBVersion20, cmdType: "Catalog", event: "Catalog;id=1894"},
		{name: "2022 interdomain Catalog", version: GBVersion30, cmdType: "Catalog", event: "Catalog;id=1894"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := newFlowConnection()
			request := newFlowRequest(t, conn, sip.MethodNotify, "notify-response-"+test.name, nil)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: test.event})
			ctx := &sip.Context{Request: request, Tx: sip.NewTransaction("notify-response-tx-"+test.name, conn)}
			respondEventNotifyOK(ctx, test.version, test.cmdType, 61, gb10ChannelID)
			response := <-flowResponse(t, conn)
			hasXML := strings.Contains(response, "Content-Type: Application/MANSCDP+xml") &&
				strings.Contains(response, "<Response><CmdType>"+test.cmdType+"</CmdType><SN>61</SN><DeviceID>"+gb10ChannelID+"</DeviceID><Result>OK</Result></Response>")
			if hasXML != test.wantXML {
				t.Fatalf("%s %s NOTIFY response XML = %v, want %v:\n%s", test.version, test.cmdType, hasXML, test.wantXML, response)
			}
			if !test.wantXML && !strings.Contains(response, "Content-Length: 0") {
				t.Fatalf("%s %s NOTIFY response should be empty:\n%s", test.version, test.cmdType, response)
			}
		})
	}
}

func TestValidateEventNotifyBusinessResponse(t *testing.T) {
	requestBody := []byte(`<Notify><CmdType>Alarm</CmdType><SN>62</SN><DeviceID>` + gb10ChannelID + `</DeviceID></Notify>`)
	response := func(body string, contentType bool) *sip.Response {
		result := sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, []byte(body))
		if contentType {
			result.AppendHeader(&sip.ContentTypeXML)
		}
		return result
	}
	valid := `<Response><CmdType>Alarm</CmdType><SN>62</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`
	tests := []struct {
		name        string
		version     GBProtocolVersion
		body        string
		contentType bool
		wantErr     bool
	}{
		{name: "valid legacy response", version: GBVersion20, body: valid, contentType: true},
		{name: "empty legacy vendor response", version: GBVersion20},
		{name: "missing content type", version: GBVersion20, body: valid, wantErr: true},
		{name: "business error", version: GBVersion20, contentType: true, body: `<Response><CmdType>Alarm</CmdType><SN>62</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>ERROR</Result></Response>`, wantErr: true},
		{name: "wrong sequence", version: GBVersion11, contentType: true, body: `<Response><CmdType>Alarm</CmdType><SN>63</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`, wantErr: true},
		{name: "unknown field", version: GBVersion10, contentType: true, body: `<Response><CmdType>Alarm</CmdType><SN>62</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><Vendor>bad</Vendor></Response>`, wantErr: true},
		{name: "2022 ignores obsolete body", version: GBVersion30, body: `<Response><Result>ERROR</Result></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateEventNotifyBusinessResponse(response(test.body, test.contentType), requestBody, "Alarm", "presence", test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateEventNotifyBusinessResponse() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
	if shouldRemoveEventSubscriptionAfterNotifyFailure(context.Background(), response(valid, true), fmt.Errorf("invalid business response"), nil) {
		t.Fatal("invalid business body in a 2xx NOTIFY response removed the RFC subscription")
	}
}

func TestCatalogSubscription10RejectsInterdomainEvent(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>51</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-catalog-10-interdomain-event", body)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=1894"})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
	ctx := &sip.Context{
		Request: req, Tx: sip.NewTransaction("subscribe-catalog-10-interdomain-event-tx", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion10),
	}
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, conn)
	if !strings.Contains(response, "SIP/2.0 400") || !strings.Contains(response, "2011 Catalog Event must use presence") {
		t.Fatalf("2011 interdomain Event response:\n%s", response)
	}
}

func TestAlarmSubscriptionBusinessResponseVersionMatrix(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		wantXML bool
	}{
		{version: GBVersion10, wantXML: true},
		{version: GBVersion11, wantXML: true},
		{version: GBVersion20, wantXML: true},
		{version: GBVersion30, wantXML: false},
	}
	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			api := &GB28181API{}
			conn := newFlowConnection()
			body := []byte(`<?xml version="1.0"?><Query><CmdType>Alarm</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
			req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-alarm-"+string(test.version), body)
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
			ctx := &sip.Context{
				Request: req, Tx: sip.NewTransaction("subscribe-alarm-tx-"+string(test.version), conn), DeviceID: gb10PlatformID,
				Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(test.version),
			}
			api.sipSubscribeEvent(ctx)
			response := <-flowResponse(t, conn)
			hasXML := strings.Contains(response, "<Response>") && strings.Contains(response, "<CmdType>Alarm</CmdType>") &&
				strings.Contains(response, "<SN>52</SN>") && strings.Contains(response, "<Result>OK</Result>")
			if hasXML != test.wantXML {
				t.Fatalf("%s Alarm SUBSCRIBE XML response = %v, want %v:\n%s", test.version, hasXML, test.wantXML, response)
			}
		})
	}
}

func TestValidateSubscribeBusinessResponse(t *testing.T) {
	request := subscribeEventRequest{CmdType: "Alarm", SN: 52, DeviceID: gb10DeviceID}
	response := func(body string) *sip.Response {
		result := sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, []byte(body))
		if body != "" {
			result.AppendHeader(&sip.ContentTypeXML)
		}
		return result
	}
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
		wantErr bool
	}{
		{name: "valid 2016 response", version: GBVersion20, body: `<Response><CmdType>Alarm</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "empty legacy vendor response", version: GBVersion20},
		{name: "business error", version: GBVersion20, body: `<Response><CmdType>Alarm</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result></Response>`, wantErr: true},
		{name: "wrong sequence", version: GBVersion11, body: `<Response><CmdType>Alarm</CmdType><SN>53</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`, wantErr: true},
		{name: "wrong target", version: GBVersion10, body: `<Response><CmdType>Alarm</CmdType><SN>52</SN><DeviceID>44010000001320000001</DeviceID><Result>OK</Result></Response>`, wantErr: true},
		{name: "wrong root", version: GBVersion20, body: `<Notify><CmdType>Alarm</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Notify>`, wantErr: true},
		{name: "malformed xml", version: GBVersion20, body: `<Response>`, wantErr: true},
		{name: "unknown field", version: GBVersion20, body: `<Response><CmdType>Alarm</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Vendor>bad</Vendor></Response>`, wantErr: true},
		{name: "2022 ignores obsolete body", version: GBVersion30, body: `<Response><Result>ERROR</Result></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSubscribeBusinessResponse(response(test.body), request, "presence", test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSubscribeBusinessResponse() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateCatalogBusinessResponseBySubscriptionScope(t *testing.T) {
	request := subscribeEventRequest{CmdType: "Catalog", SN: 53, DeviceID: gb10DeviceID}
	response := func(body string) *sip.Response {
		result := sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, []byte(body))
		if body != "" {
			result.AppendHeader(&sip.ContentTypeXML)
		}
		return result
	}
	valid := `<Response><CmdType>Catalog</CmdType><SN>53</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`
	tests := []struct {
		name       string
		version    GBProtocolVersion
		eventValue string
		body       string
		wantErr    bool
	}{
		{name: "2011 traditional valid", version: GBVersion10, eventValue: "presence", body: valid},
		{name: "2014 traditional failure", version: GBVersion11, eventValue: "presence", body: `<Response><CmdType>Catalog</CmdType><SN>53</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result></Response>`, wantErr: true},
		{name: "2016 traditional malformed", version: GBVersion20, eventValue: "presence", body: `<Response>`, wantErr: true},
		{name: "2016 interdomain ignores nonstandard body", version: GBVersion20, eventValue: "Catalog;id=1894", body: `<Response>`},
		{name: "2022 traditional ignores obsolete body", version: GBVersion30, eventValue: "presence", body: `<Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSubscribeBusinessResponse(response(test.body), request, test.eventValue, test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSubscribeBusinessResponse() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestValidateCatalogNotifyBusinessResponseBySubscriptionScope(t *testing.T) {
	requestBody := []byte(`<Notify><CmdType>Catalog</CmdType><SN>54</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	response := func(body string) *sip.Response {
		result := sip.NewResponse("", sip.DefaultSipVersion, 200, "OK", nil, []byte(body))
		if body != "" {
			result.AppendHeader(&sip.ContentTypeXML)
		}
		return result
	}
	valid := `<Response><CmdType>Catalog</CmdType><SN>54</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`
	tests := []struct {
		name       string
		version    GBProtocolVersion
		eventValue string
		body       string
		wantErr    bool
	}{
		{name: "2014 traditional valid", version: GBVersion11, eventValue: "presence", body: valid},
		{name: "2016 traditional mismatch", version: GBVersion20, eventValue: "presence", body: `<Response><CmdType>Catalog</CmdType><SN>55</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`, wantErr: true},
		{name: "2016 interdomain ignores nonstandard body", version: GBVersion20, eventValue: "Catalog;id=1894", body: `<Response>`},
		{name: "2022 interdomain ignores obsolete body", version: GBVersion30, eventValue: "Catalog;id=1894", body: `<Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateEventNotifyBusinessResponse(response(test.body), requestBody, "Catalog", test.eventValue, test.version)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateEventNotifyBusinessResponse() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func flowResponse(t *testing.T, conn *flowConnection) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	select {
	case payload := <-conn.writes:
		out <- string(payload)
	case <-time.After(time.Second):
		t.Fatal("SIP response timeout")
	}
	return out
}

func TestCatalogSubscriptionRejectsMismatchedEventID11(t *testing.T) {
	_, id, err := parseSubscriptionEvent("Catalog;id=34020000001320009999")
	if err != nil || !strings.HasSuffix(id, "9999") {
		t.Fatalf("unexpected Event parse: id=%q err=%v", id, err)
	}
}
