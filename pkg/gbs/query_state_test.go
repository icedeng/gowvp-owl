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

func TestGetQueryStateReturnsDeepSnapshot(t *testing.T) {
	api := &GB28181API{}
	api.queryStates.Store(gb10DeviceID, &QueryState{
		DeviceStatus: &DeviceStatusData{Online: "ONLINE", FaultDeviceIDs: []string{gb10ChannelID}},
		CruiseTracks: []CruiseTrackData{{Number: 1, Points: []CruisePointData{{PresetIndex: 7}}}},
		AppendixA4:   []AppendixA4Object{{Type: "doorType", Fields: map[string]string{"DeviceID": gb10DeviceID}}},
	})

	snapshot, ok := api.GetQueryState(gb10DeviceID)
	if !ok {
		t.Fatal("query state snapshot not found")
	}
	snapshot.DeviceStatus.Online = "OFFLINE"
	snapshot.DeviceStatus.FaultDeviceIDs[0] = "mutated"
	snapshot.CruiseTracks[0].Points[0].PresetIndex = 99
	snapshot.AppendixA4[0].Fields["DeviceID"] = "mutated"

	current, ok := api.GetQueryState(gb10DeviceID)
	if !ok {
		t.Fatal("query state disappeared")
	}
	if current.DeviceStatus.Online != "ONLINE" || current.DeviceStatus.FaultDeviceIDs[0] != gb10ChannelID ||
		current.CruiseTracks[0].Points[0].PresetIndex != 7 || current.AppendixA4[0].Fields["DeviceID"] != gb10DeviceID {
		t.Fatalf("GetQueryState leaked internal state: %+v", current)
	}
}

func TestQueryStateConcurrentSnapshotsAreIsolated(t *testing.T) {
	api := &GB28181API{}
	api.storeQueryState(gb10DeviceID, "DeviceStatus", &DeviceStatusData{Online: "ONLINE", FaultDeviceIDs: []string{gb10ChannelID}})
	api.storeAppendixA4State(gb10DeviceID, []AppendixA4Object{{Type: "doorType", Fields: map[string]string{"DeviceID": gb10DeviceID}}})

	var group sync.WaitGroup
	group.Add(3)
	go func() {
		defer group.Done()
		for index := 0; index < 500; index++ {
			api.storeQueryState(gb10DeviceID, "DeviceStatus", &DeviceStatusData{
				Online: "ONLINE", FaultDeviceIDs: []string{fmt.Sprintf("fault-%d", index)},
			})
			api.storeAppendixA4State(gb10DeviceID, []AppendixA4Object{{
				Type: "doorType", Fields: map[string]string{"DeviceID": fmt.Sprintf("device-%d", index)},
			}})
		}
	}()
	for range 2 {
		go func() {
			defer group.Done()
			for index := 0; index < 500; index++ {
				state, ok := api.GetQueryState(gb10DeviceID)
				if !ok {
					continue
				}
				if state.DeviceStatus != nil && len(state.DeviceStatus.FaultDeviceIDs) > 0 {
					state.DeviceStatus.FaultDeviceIDs[0] = "reader mutation"
				}
				if len(state.AppendixA4) > 0 {
					state.AppendixA4[0].Fields["DeviceID"] = "reader mutation"
				}
			}
		}()
	}
	group.Wait()
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
	memory.runtime.setGBVersion(GBVersion30)
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
		status, ok := output.Data.(*DeviceStatusData)
		if !ok || len(output.AppendixA4) != 1 {
			t.Fatalf("pending DeviceStatus output = %+v", output)
		}
		status.Online = "OFFLINE"
		output.AppendixA4[0].Fields["DeviceID"] = "mutated"
	default:
		t.Fatal("DeviceStatus response did not resolve pending query")
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.DeviceStatus == nil || state.DeviceStatus.Online != "ONLINE" ||
		len(state.AppendixA4) != 1 || state.AppendixA4[0].Fields["DeviceID"] != gb10DeviceID {
		t.Fatalf("DeviceQuery output leaked internal state: %+v", state)
	}
}

func TestConfigDownloadAcknowledgesWhenRuntimeDisappears(t *testing.T) {
	tests := []struct {
		version            GBProtocolVersion
		param              string
		extra              string
		loadsBeforeMissing int32
		wantAppendixA4     bool
	}{
		{
			version:            GBVersion11,
			loadsBeforeMissing: 2,
			param: `<BasicParam><Name>IPC</Name><DeviceID>` + gb10DeviceID + `</DeviceID>` +
				`<SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP>` +
				`<SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration>` +
				`<Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			version:            GBVersion20,
			loadsBeforeMissing: 2,
			param: `<BasicParam><Name>IPC</Name><Expiration>3600</Expiration>` +
				`<HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
		},
		{
			version:            GBVersion30,
			loadsBeforeMissing: 3,
			param:              `<BasicParam><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount></BasicParam>`,
			extra:              `<ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID + `"}</ExtraInfo>`,
			wantAppendixA4:     true,
		},
	}

	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			adapter, _, _ := newCascadeMediaCore(t)
			memory := &disappearingConfigMemory{
				versionGateMemory:  &versionGateMemory{device: &Device{IsOnline: true, gbVersion: string(test.version)}},
				loadsBeforeMissing: test.loadsBeforeMissing,
			}
			api := &GB28181API{svr: &Server{memoryStorer: memory}, core: adapter}
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "ConfigDownload", 92), pending)
			body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result>` + test.param + test.extra + `</Response>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-runtime-disappears", body, api.sipMessageConfigDownload)
			assertFlowOK(t, response)
			select {
			case output := <-pending.wait:
				if output == nil || output.Result != "OK" || output.CmdType != "ConfigDownload" {
					t.Fatalf("ConfigDownload output = %+v", output)
				}
			default:
				t.Fatal("ConfigDownload runtime disappearance left query waiting")
			}
			state, ok := api.GetQueryState(gb10DeviceID)
			if !ok {
				t.Fatal("ConfigDownload runtime disappearance discarded validated response state")
			}
			if test.wantAppendixA4 && (len(state.AppendixA4) != 1 || state.AppendixA4[0].Type != "doorType") {
				t.Fatalf("ConfigDownload runtime disappearance lost validated Appendix A.4: %+v", state.AppendixA4)
			}
		})
	}
}

type disappearingConfigMemory struct {
	*versionGateMemory
	loads              atomic.Int32
	loadsBeforeMissing int32
}

func (m *disappearingConfigMemory) Load(deviceID string) (*Device, bool) {
	if m.loads.Add(1) > m.loadsBeforeMissing {
		return nil, false
	}
	return m.versionGateMemory.Load(deviceID)
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
				`</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime>` +
				`<Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Notify>`),
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
		{
			name:   "RecordInfo",
			method: sip.MethodMessage,
			body: []byte(`<Response><CmdType>RecordInfo</CmdType><SN>94</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Name>camera</Name><SumNum>0</SumNum><ExtraInfo>{"type":"doorType","DeviceID":"` + gb10DeviceID +
				`"}</ExtraInfo></Response>`),
			handler: func(api *GB28181API, ctx *sip.Context) {
				api.sipMessageRecordInfo(ctx)
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
			api, _ := newVersionGateAPI(GBVersion30)
			api.core = ipc.NewAdapter(store, uniqueid.Core{})
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

func TestLegacyAppendixA4QueryHandlersRejectBeforeStateAndWait(t *testing.T) {
	tests := []struct {
		name    string
		cmdType string
		method  string
		body    string
		handler func(*GB28181API, *sip.Context)
		wait    bool
	}{
		{
			name: "DeviceInfo", cmdType: "DeviceInfo", method: sip.MethodMessage, wait: true,
			body:    `<Response><CmdType>DeviceInfo</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageDeviceInfo(ctx) },
		},
		{
			name: "ConfigDownload", cmdType: "ConfigDownload", method: sip.MethodMessage, wait: true,
			body:    `<Response><CmdType>ConfigDownload</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>ERROR</Result><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageConfigDownload(ctx) },
		},
		{
			name: "Catalog response", cmdType: "Catalog", method: sip.MethodMessage, wait: true,
			body:    `<Response><CmdType>Catalog</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><DeviceList Num="0"></DeviceList><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Response>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipMessageCatalog(ctx) },
		},
		{
			name: "Catalog notification", cmdType: "Catalog", method: sip.MethodNotify,
			body:    `<Notify><CmdType>Catalog</CmdType><SN>96</SN><DeviceID>` + gb10DeviceID + `</DeviceID><SumNum>0</SumNum><DeviceList Num="0"></DeviceList><Info><doorType><DeviceID>` + gb10DeviceID + `</DeviceID></doorType></Info></Notify>`,
			handler: func(api *GB28181API, ctx *sip.Context) { api.sipNotifyCatalog(ctx) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion20)
			var pending *pendingQueryWait
			if test.wait {
				pending = &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
				api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, test.cmdType, 96), pending)
			}
			response := runFlowHandler(t, newFlowConnection(), api, test.method, "legacy-a4-"+test.name, []byte(test.body), func(ctx *sip.Context) {
				test.handler(api, ctx)
			})
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("legacy Appendix A.4 response = %s", response)
			}
			if _, ok := api.GetQueryState(gb10DeviceID); ok {
				t.Fatal("legacy Appendix A.4 changed query state")
			}
			if pending != nil {
				select {
				case output := <-pending.wait:
					t.Fatalf("legacy Appendix A.4 resolved query wait: %+v", output)
				default:
				}
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
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{}), svr: &Server{memoryStorer: memory}}
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
