package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestCleanupQueryStatesExpiresAndBoundsSnapshots(t *testing.T) {
	now := time.Now()
	api := &GB28181API{}
	api.queryStates.Store("expired", &QueryState{UpdatedAt: now.Add(-queryStateTTL - time.Second)})
	api.queryStates.Store("invalid", "unexpected")
	for i := 0; i < maxQueryStateEntries+2; i++ {
		deviceID := fmt.Sprintf("device-%04d", i)
		api.queryStates.Store(deviceID, &QueryState{UpdatedAt: now.Add(time.Duration(i) * time.Nanosecond)})
	}

	api.cleanupQueryStates(now.Add(time.Second))

	if _, ok := api.queryStates.Load("expired"); ok {
		t.Fatal("expired query state was retained")
	}
	if _, ok := api.queryStates.Load("invalid"); ok {
		t.Fatal("invalid query state was retained")
	}
	count := 0
	api.queryStates.Range(func(_, _ any) bool {
		count++
		return true
	})
	if count != maxQueryStateEntries {
		t.Fatalf("query states = %d; want %d", count, maxQueryStateEntries)
	}
	if _, ok := api.queryStates.Load("device-0000"); ok {
		t.Fatal("oldest query state was retained")
	}
}

func TestGenericQueryAcknowledgesBeforeSinglePersistence(t *testing.T) {
	base, _, _ := newCascadeMediaCore(t)
	deviceStore := &countingQueryDeviceStore{DeviceStorer: base.Store().Device()}
	store := &queryTestStore{Storer: base.Store(), device: deviceStore}
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{})}
	memory := &blockingQueryMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		queryMu:    &api.queryStateMu,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	api.svr = &Server{memoryStorer: memory}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceStatus", 91), pending)

	conn := newFlowConnection()
	body := []byte(`<Response><CmdType>DeviceStatus</CmdType><SN>91</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><Online>ONLINE</Online><Status>OK</Status>` +
		`<Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID><DoorID>` + gb10DeviceID +
		`</DoorID></doorType></Info></Response>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "device-status-single-persistence", body)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipMessageQueryGeneric(&sip.Context{
			Request:  request,
			Tx:       sip.NewTransaction("device-status-single-persistence-tx", conn),
			DeviceID: gb10DeviceID,
			Source:   conn.remote,
			To:       to,
			Log:      slog.Default(),
		})
	}()
	release := func() {
		select {
		case <-memory.release:
		default:
			close(memory.release)
		}
	}
	defer release()

	select {
	case <-memory.entered:
	case <-time.After(time.Second):
		t.Fatal("DeviceStatus persistence was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected SIP response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("DeviceStatus persistence delayed SIP 200 OK")
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DeviceStatus handler did not finish")
	}

	if got := memory.changes.Load(); got != 1 {
		t.Fatalf("DeviceStatus changes = %d, want 1", got)
	}
	if memory.queryLockHeld.Load() {
		t.Fatal("DeviceStatus persistence ran while queryStateMu was held")
	}
	if got := deviceStore.updates.Load(); got != 1 {
		t.Fatalf("Appendix A.4 persistence updates = %d, want 1", got)
	}
	select {
	case output := <-pending.wait:
		if output.Data == nil || len(output.AppendixA4) != 1 {
			t.Fatalf("pending DeviceStatus output = %+v", output)
		}
	default:
		t.Fatal("DeviceStatus response did not resolve pending query")
	}
}

func TestAppendixA4HandlersAcknowledgeBeforePersistence(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	defer func() { config = previousConfig }()

	tests := []struct {
		name    string
		method  string
		body    []byte
		handler func(*GB28181API, *sip.Context)
	}{
		{
			name:   "Alarm",
			method: sip.MethodMessage,
			body: []byte(`<Notify><CmdType>Alarm</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Notify>`),
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageAlarm(ctx)
			},
		},
		{
			name:   "DeviceConfig",
			method: sip.MethodMessage,
			body: []byte(`<Response><CmdType>DeviceConfig</CmdType><SN>93</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result><Info><doorType><DeviceID>` + gb10DeviceID +
				`</DeviceID></doorType></Info></Response>`),
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.handleDeviceConfig(ctx)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base, _, _ := newCascadeMediaCore(t)
			deviceStore := &blockingQueryDeviceStore{
				DeviceStorer: base.Store().Device(),
				entered:      make(chan struct{}),
				release:      make(chan struct{}),
			}
			store := &queryTestStore{Storer: base.Store(), device: deviceStore}
			api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{})}
			conn := newFlowConnection()
			request := newFlowRequest(t, conn, test.method, "appendix-a4-ack-"+test.name, test.body)
			to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
			done := make(chan struct{})
			go func() {
				defer close(done)
				test.handler(api, &sip.Context{
					Request: request, Tx: sip.NewTransaction("appendix-a4-ack-"+test.name+"-tx", conn),
					DeviceID: gb10DeviceID, Source: conn.remote, To: to, Log: slog.Default(),
				})
			}()
			release := func() {
				select {
				case <-deviceStore.release:
				default:
					close(deviceStore.release)
				}
			}
			defer release()

			select {
			case <-deviceStore.entered:
			case <-time.After(time.Second):
				t.Fatal("Appendix A.4 persistence was not reached")
			}
			select {
			case payload := <-conn.writes:
				if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
					t.Fatalf("unexpected SIP response:\n%s", response)
				}
			default:
				release()
				<-done
				t.Fatal("Appendix A.4 persistence delayed SIP 200 OK")
			}
			release()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler did not finish")
			}
			if got := deviceStore.updates.Load(); got != 1 {
				t.Fatalf("Appendix A.4 persistence updates = %d, want 1", got)
			}
		})
	}
}

func TestKeepaliveAcknowledgesBeforePersistenceAndTreatsMissingStatusOnline(t *testing.T) {
	api := &GB28181API{}
	memory := &blockingKeepaliveMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	api.svr = &Server{memoryStorer: memory}
	conn := newFlowConnection()
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>94</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "keepalive-ack-before-persistence", body)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipMessageKeepalive(&sip.Context{
			Request: request, Tx: sip.NewTransaction("keepalive-ack-before-persistence-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote, To: to, Log: slog.Default(), XGBVer: string(GBVersion10),
		})
	}()
	release := func() {
		select {
		case <-memory.release:
		default:
			close(memory.release)
		}
	}
	defer release()

	select {
	case <-memory.entered:
	case <-time.After(time.Second):
		t.Fatal("Keepalive persistence was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected SIP response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("Keepalive persistence delayed SIP 200 OK")
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.DeviceStatus == nil || state.DeviceStatus.Online != "ONLINE" {
		t.Fatalf("missing-status Keepalive state = %+v", state)
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Keepalive handler did not finish")
	}
	if !memory.persistent.IsOnline {
		t.Fatal("missing-status Keepalive persisted device as offline")
	}
}

func TestDeviceInfoAcknowledgesBeforePersistence(t *testing.T) {
	base, _, _ := newCascadeMediaCore(t)
	deviceStore := &blockingQueryDeviceStore{
		DeviceStorer: base.Store().Device(),
		entered:      make(chan struct{}),
		release:      make(chan struct{}),
	}
	store := &queryTestStore{Storer: base.Store(), device: deviceStore}
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{})}
	conn := newFlowConnection()
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>95</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>OK</Result><DeviceName>Slow IPC</DeviceName></Response>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "device-info-ack-before-persistence", body)
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipMessageDeviceInfo(&sip.Context{
			Request: request, Tx: sip.NewTransaction("device-info-ack-before-persistence-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote, To: to, Log: slog.Default(),
		})
	}()
	release := func() {
		select {
		case <-deviceStore.release:
		default:
			close(deviceStore.release)
		}
	}
	defer release()

	select {
	case <-deviceStore.entered:
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo persistence was not reached")
	}
	select {
	case payload := <-conn.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 200 OK") {
			t.Fatalf("unexpected SIP response:\n%s", response)
		}
	default:
		release()
		<-done
		t.Fatal("DeviceInfo persistence delayed SIP 200 OK")
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo handler did not finish")
	}
	if got := deviceStore.updates.Load(); got != 1 {
		t.Fatalf("DeviceInfo persistence updates = %d, want 1", got)
	}
}

type blockingQueryMemory struct {
	*flowMemory
	queryMu       *sync.RWMutex
	entered       chan struct{}
	release       chan struct{}
	changes       atomic.Int32
	queryLockHeld atomic.Bool
	enterOnce     sync.Once
}

type blockingKeepaliveMemory struct {
	*flowMemory
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func (m *blockingKeepaliveMemory) Change(deviceID string, changePersistent func(*ipc.Device) error, changeRuntime func(*Device)) error {
	m.enterOnce.Do(func() { close(m.entered) })
	<-m.release
	return m.flowMemory.Change(deviceID, changePersistent, changeRuntime)
}

func (m *blockingQueryMemory) Change(deviceID string, changePersistent func(*ipc.Device) error, changeRuntime func(*Device)) error {
	m.changes.Add(1)
	if !m.queryMu.TryLock() {
		m.queryLockHeld.Store(true)
	} else {
		m.queryMu.Unlock()
	}
	m.enterOnce.Do(func() { close(m.entered) })
	<-m.release
	return m.flowMemory.Change(deviceID, changePersistent, changeRuntime)
}

type countingQueryDeviceStore struct {
	ipc.DeviceStorer
	updates atomic.Int32
}

type blockingQueryDeviceStore struct {
	ipc.DeviceStorer
	updates   atomic.Int32
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func (s *blockingQueryDeviceStore) Update(ctx context.Context, device *ipc.Device, change func(*ipc.Device) error, opts ...orm.QueryOption) error {
	s.updates.Add(1)
	s.enterOnce.Do(func() { close(s.entered) })
	<-s.release
	return s.DeviceStorer.Update(ctx, device, change, opts...)
}

func (s *countingQueryDeviceStore) Update(ctx context.Context, device *ipc.Device, change func(*ipc.Device) error, opts ...orm.QueryOption) error {
	s.updates.Add(1)
	return s.DeviceStorer.Update(ctx, device, change, opts...)
}

type queryTestStore struct {
	ipc.Storer
	device ipc.DeviceStorer
}

func (s *queryTestStore) Device() ipc.DeviceStorer {
	return s.device
}
