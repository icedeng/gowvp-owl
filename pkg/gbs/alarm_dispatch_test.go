package gbs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	testAlarmReceiverID      = "34020000002000000011"
	testOtherAlarmReceiverID = "34020000002000000012"
)

func cascadeAlarmPayload() []byte {
	return []byte(`<Notify><CmdType>Alarm</CmdType><SN>17</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod>` +
		`<AlarmTime>2026-08-29T10:00:00</AlarmTime><AlarmDescription>  door alarm  </AlarmDescription>` +
		`<Longitude>120.1</Longitude><Latitude>30.2</Latitude></Notify>`)
}

func registeredAlarmDispatchWorker(t *testing.T, version GBProtocolVersion) *cascadeWorker {
	t.Helper()
	platform := testSharedCascadePlatform(t)
	platform.version = version
	platform.alarmDispatchEnabled = true
	worker := newCascadeWorker(nil, platform)
	t.Cleanup(worker.cancel)
	worker.effective = version
	worker.updateStatus(func(status *CascadePlatformStatus) {
		status.Registered = true
		status.State = "registered"
	})
	return worker
}

type localAlarmDispatchMemory struct {
	mu      sync.RWMutex
	devices map[string]*Device
}

func (m *localAlarmDispatchMemory) LoadOrStore(deviceID string, device *Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.devices[deviceID]; !exists {
		m.devices[deviceID] = device
	}
}

func (m *localAlarmDispatchMemory) LoadDeviceToMemory(sip.Connection) error { return nil }

func (m *localAlarmDispatchMemory) RangeDevices(fn func(string, *Device) bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for deviceID, device := range m.devices {
		if !fn(deviceID, device) {
			return
		}
	}
}

func (m *localAlarmDispatchMemory) Change(deviceID string, _ func(*ipc.Device) error, changeRuntime func(*Device)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	device, exists := m.devices[deviceID]
	if !exists {
		return ErrDeviceNotExist
	}
	if changeRuntime != nil {
		changeRuntime(device)
	}
	return nil
}

func (m *localAlarmDispatchMemory) Load(deviceID string) (*Device, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	device, exists := m.devices[deviceID]
	return device, exists
}

func (m *localAlarmDispatchMemory) Store(deviceID string, device *Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices[deviceID] = device
}

func (*localAlarmDispatchMemory) GetChannel(string, string) (*Channel, bool) { return nil, false }

type localAlarmSIPFixture struct {
	api      *GB28181API
	target   localAlarmDispatchTarget
	peer     net.Conn
	server   *sip.Server
	receiver *Device
}

func newLocalAlarmSIPFixture(t *testing.T, version GBProtocolVersion) *localAlarmSIPFixture {
	t.Helper()
	localAddress := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(localAddress)
	localRaw, remoteRaw := net.Pipe()
	localConn := &cascadeDownstreamTCPConn{
		Conn:   localRaw,
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	}
	peer := &cascadeDownstreamTCPConn{Conn: remoteRaw, local: localConn.remote, remote: localConn.local}
	connection := sip.NewTCPConnection(localConn)
	receiver := &Device{
		IsOnline:  true,
		gbVersion: string(version),
		conn:      connection,
		source:    localConn.remote,
		to:        mustFlowAddress(t, "sip:"+testAlarmReceiverID+"@192.0.2.30:5060"),
	}
	memory := &localAlarmDispatchMemory{devices: map[string]*Device{testAlarmReceiverID: receiver}}
	cfg := &conf.SIP{
		ID: gb10PlatformID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060,
		AlarmReceivers: []conf.SIPAlarmReceiver{{
			Name: "local-alarm-client", Enabled: true, DeviceID: testAlarmReceiverID,
			SourceIDs: []string{testCascadeChannelID},
		}},
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		cfg: cfg, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *localAddress}
	api.svr = server
	sipServer.Message().Handle("Alarm", api.sipMessageAlarm)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		api.beginClose()
		_ = peer.Close()
		sipServer.Close()
	})
	return &localAlarmSIPFixture{
		api: api, peer: peer, server: sipServer, receiver: receiver,
		target: localAlarmDispatchTarget{config: cfg.AlarmReceivers[0], device: receiver},
	}
}

func localAlarmBusinessMessage(receiverID string, version GBProtocolVersion, body []byte) string {
	return fmt.Sprintf(
		"MESSAGE sip:%s@192.0.2.20:5060 SIP/2.0\r\n"+
			"Via: SIP/2.0/TCP 192.0.2.30:5060;branch=z9hG4bK-local-alarm-%s\r\n"+
			"From: <sip:%s@192.0.2.30:5060>;tag=local-alarm\r\n"+
			"To: <sip:%s@192.0.2.20:5060>\r\n"+
			"Call-ID: local-alarm-%s@192.0.2.30\r\n"+
			"CSeq: 1 MESSAGE\r\n"+
			"Max-Forwards: 70\r\n"+
			"Content-Type: Application/MANSCDP+xml\r\n"+
			"X-GB-Ver: %s\r\n"+
			"Content-Length: %d\r\n\r\n%s",
		gb10PlatformID, version, receiverID, gb10PlatformID, version, version, len(body), body,
	)
}

func observeLocalAlarmSIP(peer net.Conn, version GBProtocolVersion, resultValue string, result chan<- cascadeDownstreamSIPObservation) {
	observation := cascadeDownstreamSIPObservation{}
	reader := bufio.NewReader(peer)
	request, err := readAnnexGTestSIPFrame(reader)
	if err != nil {
		observation.err = err
		result <- observation
		return
	}
	observation.request = request
	if _, err = io.WriteString(peer, annexGTestSIPResponse(request, http.StatusOK, "OK", "")); err != nil {
		observation.err = err
		result <- observation
		return
	}
	var alarm messageAlarm
	if err = sip.XMLDecode(cascadeDownstreamSIPBody(request), &alarm); err != nil {
		observation.err = err
		result <- observation
		return
	}
	responseBody := []byte(fmt.Sprintf(
		`<Response><CmdType>Alarm</CmdType><SN>%d</SN><DeviceID>%s</DeviceID><Result>%s</Result></Response>`,
		alarm.SN, alarm.DeviceID, resultValue,
	))
	if _, err = io.WriteString(peer, localAlarmBusinessMessage(testAlarmReceiverID, version, responseBody)); err != nil {
		observation.err = err
		result <- observation
		return
	}
	observation.ack, observation.err = readAnnexGTestSIPFrame(reader)
	result <- observation
}

func TestLocalAlarmDispatchFourVersionMatrixUsesRegisteredSIPPath(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			fixture := newLocalAlarmSIPFixture(t, version)
			observed := make(chan cascadeDownstreamSIPObservation, 1)
			go observeLocalAlarmSIP(fixture.peer, version, "OK", observed)

			if err := fixture.api.dispatchAlarmToLocalTarget(
				t.Context(), fixture.target, gb10DeviceID, testCascadeChannelID, cascadeAlarmPayload(),
			); err != nil {
				t.Fatal(err)
			}
			observation := awaitCascadeDownstreamSIPObservation(t, observed)
			if observation.err != nil {
				t.Fatal(observation.err)
			}
			if !strings.HasPrefix(observation.request, "MESSAGE sip:"+testAlarmReceiverID+"@") {
				t.Fatalf("registered receiver request = %s", observation.request)
			}
			if got := annexGTestSIPHeader(observation.request, "X-GB-Ver"); got != string(version) {
				t.Fatalf("registered receiver X-GB-Ver = %q, want %q", got, version)
			}
			if !strings.HasPrefix(observation.ack, "SIP/2.0 200 OK") {
				t.Fatalf("registered receiver business response ACK = %s", observation.ack)
			}
			var alarm messageAlarm
			if err := sip.XMLDecode(cascadeDownstreamSIPBody(observation.request), &alarm); err != nil {
				t.Fatal(err)
			}
			if alarm.SN <= 0 || alarm.SN == 17 || alarm.DeviceID != testCascadeChannelID ||
				alarm.AlarmDescription != "  door alarm  " {
				t.Fatalf("registered receiver Alarm = %+v", alarm)
			}
			if count := alarmDispatchPendingCount(&fixture.api.pendingLocalAlarmDispatch); count != 0 {
				t.Fatalf("pending registered receiver Alarm count = %d", count)
			}
		})
	}
}

func TestLocalAlarmDispatchRejectsInvalidPayloadBeforeAllocatingPending(t *testing.T) {
	fixture := newLocalAlarmSIPFixture(t, GBVersion20)
	invalid := bytes.Replace(cascadeAlarmPayload(), []byte("<SN>17</SN>"), nil, 1)

	err := fixture.api.dispatchAlarmToLocalTarget(
		t.Context(), fixture.target, gb10DeviceID, testCascadeChannelID, invalid,
	)
	if err == nil || !strings.Contains(err.Error(), "SN") {
		t.Fatalf("invalid Alarm dispatch error = %v", err)
	}
	if count := alarmDispatchPendingCount(&fixture.api.pendingLocalAlarmDispatch); count != 0 {
		t.Fatalf("invalid Alarm dispatch left pending state: %d", count)
	}
}

func TestLocalAlarmDispatchTargetsStartIndependently(t *testing.T) {
	localAddress := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(localAddress)
	memory := &localAlarmDispatchMemory{devices: make(map[string]*Device)}
	peers := make(map[string]net.Conn, 2)
	for index, receiverID := range []string{testAlarmReceiverID, testOtherAlarmReceiverID} {
		localRaw, remoteRaw := net.Pipe()
		localConn := &cascadeDownstreamTCPConn{
			Conn:   localRaw,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060 + index},
		}
		peer := &cascadeDownstreamTCPConn{Conn: remoteRaw, local: localConn.remote, remote: localConn.local}
		connection := sip.NewTCPConnection(localConn)
		memory.devices[receiverID] = &Device{
			IsOnline: true, gbVersion: string(GBVersion20), conn: connection, source: localConn.remote,
			to: mustFlowAddress(t, "sip:"+receiverID+"@192.0.2.30:5060"),
		}
		peers[receiverID] = peer
		go sipServer.ProcessTCPConnection(connection)
	}
	cfg := &conf.SIP{
		ID: gb10PlatformID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060,
		AlarmReceivers: []conf.SIPAlarmReceiver{
			{Name: "receiver-a", Enabled: true, DeviceID: testAlarmReceiverID, SourceIDs: []string{testCascadeChannelID}},
			{Name: "receiver-b", Enabled: true, DeviceID: testOtherAlarmReceiverID, SourceIDs: []string{testCascadeChannelID}},
		},
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		cfg: cfg, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
	}
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *localAddress}
	t.Cleanup(func() {
		api.beginClose()
		for _, peer := range peers {
			_ = peer.Close()
		}
		sipServer.Close()
	})

	type observation struct {
		receiverID string
		request    string
		err        error
	}
	observed := make(chan observation, len(peers))
	for receiverID, peer := range peers {
		receiverID, peer := receiverID, peer
		go func() {
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			request, err := readAnnexGTestSIPFrame(bufio.NewReader(peer))
			observed <- observation{receiverID: receiverID, request: request, err: err}
		}()
	}

	api.dispatchAlarmToLocalTargets(gb10DeviceID, cascadeAlarmPayload())
	for range peers {
		result := <-observed
		if result.err != nil {
			t.Fatalf("receiver %s did not start independently: %v", result.receiverID, result.err)
		}
		if !strings.HasPrefix(result.request, "MESSAGE sip:"+result.receiverID+"@") {
			t.Fatalf("receiver %s request = %s", result.receiverID, result.request)
		}
	}
}

func TestCascadeAlarmDispatchTargetsStartIndependently(t *testing.T) {
	workers := []*cascadeWorker{
		registeredAlarmDispatchWorker(t, GBVersion20),
		registeredAlarmDispatchWorker(t, GBVersion20),
	}
	workers[0].platform.name = "alarm-upstream-a"
	workers[1].platform.name = "alarm-upstream-b"
	started := make(chan string, len(workers))
	for _, worker := range workers {
		worker := worker
		worker.exchange = func(ctx context.Context, _ *sip.Request) (*sip.Response, error) {
			started <- worker.platform.name
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	manager := NewCascadeManager(nil)
	for _, worker := range workers {
		manager.items[worker.platform.name] = worker
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		lifecycleDone: make(chan struct{}),
		svr:           &Server{cascade: manager},
	}
	t.Cleanup(api.beginClose)

	api.dispatchAlarmToCascadeTargets(gb10DeviceID, cascadeAlarmPayload())
	seen := make(map[string]struct{}, len(workers))
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(seen) < len(workers) {
		select {
		case name := <-started:
			seen[name] = struct{}{}
		case <-deadline.C:
			t.Fatalf("cascade Alarm targets were serialized: started=%v", seen)
		}
	}
}

func TestLocalAlarmDispatchTargetSelectionIsOptInAuthorizedAndOnline(t *testing.T) {
	online := &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	offline := &Device{IsOnline: false, gbVersion: string(GBVersion20)}
	memory := &localAlarmDispatchMemory{devices: map[string]*Device{
		testAlarmReceiverID:      online,
		testOtherAlarmReceiverID: offline,
	}}
	api := &GB28181API{
		cfg: &conf.SIP{AlarmReceivers: []conf.SIPAlarmReceiver{
			{Name: "enabled", Enabled: true, DeviceID: testAlarmReceiverID, SourceIDs: []string{testCascadeChannelID}},
			{Name: "duplicate", Enabled: true, DeviceID: testAlarmReceiverID, SourceIDs: []string{testCascadeChannelID}},
			{Name: "disabled", Enabled: false, DeviceID: testOtherAlarmReceiverID, SourceIDs: []string{testCascadeChannelID}},
			{Name: "offline", Enabled: true, DeviceID: testOtherAlarmReceiverID, SourceIDs: []string{testCascadeChannelID}},
			{Name: "unauthorized", Enabled: true, DeviceID: "34020000002000000003", SourceIDs: []string{"34020000001320000999"}},
		}},
		svr: &Server{memoryStorer: memory},
	}
	targets := api.localAlarmDispatchTargets(testCascadeChannelID)
	if len(targets) != 1 || targets[0].config.DeviceID != testAlarmReceiverID || targets[0].device != online {
		t.Fatalf("registered Alarm targets = %+v", targets)
	}
	if targets := api.localAlarmDispatchTargets("34020000001320000999"); len(targets) != 0 {
		t.Fatalf("unauthorized/offline Alarm targets = %+v", targets)
	}
	api.cfg.AlarmReceivers = nil
	if targets := api.localAlarmDispatchTargets(testCascadeChannelID); len(targets) != 0 {
		t.Fatalf("default-disabled registered Alarm targets = %+v", targets)
	}
}

func TestLocalAlarmBusinessResponseStrictAndTargetCorrelated(t *testing.T) {
	api := &GB28181API{}
	operation := newPendingDeviceOperation(t.Context(), testAlarmReceiverID, testCascadeChannelID)
	pending := &pendingLocalAlarmDispatch{wait: make(chan alarmBusinessResponse, 1), operation: operation}
	key := pendingLocalAlarmDispatchKey{receiverID: testAlarmReceiverID, sn: 81, deviceID: testCascadeChannelID}
	api.pendingLocalAlarmDispatch.Store(key, pending)
	t.Cleanup(func() {
		api.pendingLocalAlarmDispatch.Delete(key)
		operation.Cancel(nil)
	})

	tests := []struct {
		name   string
		body   string
		status string
	}{
		{name: "wrong target", body: `<Response><CmdType>Alarm</CmdType><SN>81</SN><DeviceID>34020000001320000999</DeviceID><Result>OK</Result></Response>`, status: "400"},
		{name: "stale sequence", body: `<Response><CmdType>Alarm</CmdType><SN>82</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>OK</Result></Response>`, status: "200"},
		{name: "duplicate result", body: `<Response><CmdType>Alarm</CmdType><SN>81</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>OK</Result><Result>OK</Result></Response>`, status: "400"},
		{name: "invalid result", body: `<Response><CmdType>Alarm</CmdType><SN>81</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>SUCCESS</Result></Response>`, status: "400"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := newFlowConnection()
			request := newFlowRequest(t, conn, sip.MethodMessage, fmt.Sprintf("local-alarm-strict-%d", index), []byte(test.body))
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction(fmt.Sprintf("local-alarm-strict-tx-%d", index), conn),
				DeviceID: testAlarmReceiverID, Source: conn.remote,
			}
			api.sipMessageAlarm(ctx)
			select {
			case payload := <-conn.writes:
				if !strings.Contains(string(payload), "SIP/2.0 "+test.status) {
					t.Fatalf("response = %s", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("SIP response timeout")
			}
			select {
			case response := <-pending.wait:
				t.Fatalf("invalid response was delivered: %+v", response)
			default:
			}
		})
	}
}

func TestAlarmResponseRoutesPendingQueryBeforeBusinessDispatch(t *testing.T) {
	t.Run("matching query response", func(t *testing.T) {
		api, _ := newVersionGateAPI(GBVersion10)
		pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
		key := buildPendingQueryKey(gb10DeviceID, "Alarm", 83)
		api.pendingDeviceQuery.Store(key, pending)
		t.Cleanup(func() { api.pendingDeviceQuery.Delete(key) })

		body := []byte(`<Response><CmdType>Alarm</CmdType><SN>83</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "alarm-query-response", body, api.sipMessageAlarm)
		assertFlowOK(t, response)
		select {
		case output := <-pending.wait:
			if output == nil || output.CmdType != "Alarm" || output.SN != 83 || output.DeviceID != gb10DeviceID || output.Result != "OK" {
				t.Fatalf("Alarm query output = %+v", output)
			}
		default:
			t.Fatal("Alarm query response did not wake the pending query")
		}
	})

	t.Run("target mismatch is rejected by query route", func(t *testing.T) {
		api, _ := newVersionGateAPI(GBVersion10)
		pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
		key := buildPendingQueryKey(gb10DeviceID, "Alarm", 84)
		api.pendingDeviceQuery.Store(key, pending)
		t.Cleanup(func() { api.pendingDeviceQuery.Delete(key) })

		body := []byte(`<Response><CmdType>Alarm</CmdType><SN>84</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>OK</Result></Response>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "alarm-query-target-mismatch", body, api.sipMessageAlarm)
		if !strings.Contains(response, "SIP/2.0 400") {
			t.Fatalf("Alarm query target mismatch response = %s", response)
		}
		select {
		case output := <-pending.wait:
			t.Fatalf("target-mismatched Alarm query woke pending wait: %+v", output)
		default:
		}
	})
}

func TestAlarmBusinessResponsesCompleteOnlyAfterSuccessfulSIPOK(t *testing.T) {
	for _, writeFailure := range []bool{false, true} {
		name := "success"
		var writeErr error
		if writeFailure {
			name = "write failure"
			writeErr = errors.New("write failed")
		}
		t.Run("cascade/"+name, func(t *testing.T) {
			api := &GB28181API{}
			worker := registeredAlarmDispatchWorker(t, GBVersion20)
			pending, _ := newPendingAlarmDispatch(t.Context())
			key := pendingAlarmDispatchKey{worker: worker, sn: 99, deviceID: testExposedChannelID}
			api.pendingAlarmDispatch.Store(key, pending)
			body := []byte(`<Response><CmdType>Alarm</CmdType><SN>99</SN><DeviceID>` + testExposedChannelID + `</DeviceID><Result>OK</Result></Response>`)
			conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "cascade-alarm-commit-"+name, body, func(ctx *sip.Context) {
				api.handleCascadeAlarmBusinessResponse(ctx, worker)
			}, writeErr)
			completedBeforeSIP := false
			select {
			case <-pending.result:
				completedBeforeSIP = true
			default:
			}
			finishBlockingFlowHandler(t, conn, done)
			if completedBeforeSIP {
				t.Fatal("cascade Alarm response completed before SIP 200 was written")
			}
			completed := false
			select {
			case <-pending.result:
				completed = true
			default:
			}
			if completed != !writeFailure {
				t.Fatalf("cascade Alarm completed = %v, want %v", completed, !writeFailure)
			}
		})

		t.Run("local/"+name, func(t *testing.T) {
			api := &GB28181API{}
			operation := newPendingDeviceOperation(t.Context(), testAlarmReceiverID, testCascadeChannelID)
			defer operation.Cancel(nil)
			pending := &pendingLocalAlarmDispatch{wait: make(chan alarmBusinessResponse, 1), operation: operation}
			key := pendingLocalAlarmDispatchKey{receiverID: testAlarmReceiverID, sn: 99, deviceID: testCascadeChannelID}
			api.pendingLocalAlarmDispatch.Store(key, pending)
			body := []byte(`<Response><CmdType>Alarm</CmdType><SN>99</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><Result>OK</Result></Response>`)
			conn, done := startBlockingFlowHandlerForDevice(t, api, testAlarmReceiverID, sip.MethodMessage, "local-alarm-commit-"+name, body, api.handleLocalAlarmBusinessResponse, writeErr)
			completedBeforeSIP := false
			select {
			case <-pending.wait:
				completedBeforeSIP = true
			default:
			}
			finishBlockingFlowHandler(t, conn, done)
			if completedBeforeSIP {
				t.Fatal("local Alarm response completed before SIP 200 was written")
			}
			completed := false
			select {
			case <-pending.wait:
				completed = true
			default:
			}
			if completed != !writeFailure {
				t.Fatalf("local Alarm completed = %v, want %v", completed, !writeFailure)
			}
		})
	}
}

func TestLocalAlarmDispatchReportsBusinessError(t *testing.T) {
	fixture := newLocalAlarmSIPFixture(t, GBVersion20)
	observed := make(chan cascadeDownstreamSIPObservation, 1)
	go observeLocalAlarmSIP(fixture.peer, GBVersion20, "ERROR", observed)

	err := fixture.api.dispatchAlarmToLocalTarget(
		t.Context(), fixture.target, gb10DeviceID, testCascadeChannelID, cascadeAlarmPayload(),
	)
	if err == nil || !strings.Contains(err.Error(), "rejected: ERROR") {
		t.Fatalf("registered receiver business ERROR result = %v", err)
	}
	observation := awaitCascadeDownstreamSIPObservation(t, observed)
	if observation.err != nil || !strings.HasPrefix(observation.ack, "SIP/2.0 200 OK") {
		t.Fatalf("registered receiver ERROR observation = %+v", observation)
	}
}

func TestLocalAlarmDispatchDoesNotReflectToSourceDevice(t *testing.T) {
	receiverConn := newFlowConnection()
	receiver := &Device{
		IsOnline: true, gbVersion: string(GBVersion20), conn: receiverConn,
		source: receiverConn.remote, to: mustFlowAddress(t, "sip:"+testAlarmReceiverID+"@192.0.2.10:5060"),
	}
	api := &GB28181API{}
	target := localAlarmDispatchTarget{
		config: conf.SIPAlarmReceiver{Name: "source", Enabled: true, DeviceID: testAlarmReceiverID},
		device: receiver,
	}
	if err := api.dispatchAlarmToLocalTarget(
		t.Context(), target, testAlarmReceiverID, testCascadeChannelID, cascadeAlarmPayload(),
	); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-receiverConn.writes:
		t.Fatalf("Alarm was reflected to source device: %s", payload)
	default:
	}
}

func TestLocalAlarmDispatchStopsWithReceiverLifecycle(t *testing.T) {
	tests := []struct {
		name      string
		cause     error
		cancel    func(*GB28181API, error)
		wantError error
	}{
		{
			name: "offline", cause: ErrDeviceOffline,
			cancel:    func(api *GB28181API, cause error) { api.cancelPendingDeviceOperations(testAlarmReceiverID, cause) },
			wantError: ErrDeviceOffline,
		},
		{
			name: "deleted", cause: ErrDeviceNotExist,
			cancel:    func(api *GB28181API, cause error) { api.cancelPendingDeviceOperations(testAlarmReceiverID, cause) },
			wantError: ErrDeviceNotExist,
		},
		{
			name: "service stopped", cause: ErrServiceStopped,
			cancel:    func(api *GB28181API, _ error) { api.beginClose() },
			wantError: ErrServiceStopped,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLocalAlarmSIPFixture(t, GBVersion20)
			requestSeen := make(chan error, 1)
			go func() {
				reader := bufio.NewReader(fixture.peer)
				_, err := readAnnexGTestSIPFrame(reader)
				requestSeen <- err
			}()
			done := make(chan error, 1)
			go func() {
				done <- fixture.api.dispatchAlarmToLocalTarget(
					t.Context(), fixture.target, gb10DeviceID, testCascadeChannelID, cascadeAlarmPayload(),
				)
			}()
			select {
			case err := <-requestSeen:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("registered receiver Alarm request timeout")
			}
			test.cancel(fixture.api, test.cause)
			select {
			case err := <-done:
				if !errors.Is(err, test.wantError) {
					t.Fatalf("registered receiver lifecycle error = %v; want %v", err, test.wantError)
				}
			case <-time.After(time.Second):
				t.Fatal("registered receiver Alarm dispatch did not stop")
			}
			if count := alarmDispatchPendingCount(&fixture.api.pendingLocalAlarmDispatch); count != 0 {
				t.Fatalf("pending registered receiver Alarm count = %d", count)
			}
		})
	}
}

func TestNotifyAlarmDoesNotDispatchToLocalReceiver(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })
	receiverConn := newFlowConnection()
	receiver := &Device{
		IsOnline: true, gbVersion: string(GBVersion20), conn: receiverConn,
		source: receiverConn.remote, to: mustFlowAddress(t, "sip:"+testAlarmReceiverID+"@192.0.2.10:5060"),
	}
	source := &Device{IsOnline: true, gbVersion: string(GBVersion20)}
	memory := &localAlarmDispatchMemory{devices: map[string]*Device{
		gb10DeviceID: source, testAlarmReceiverID: receiver,
	}}
	api := &GB28181API{
		cfg: &conf.SIP{AlarmReceivers: []conf.SIPAlarmReceiver{{
			Name: "local-alarm-client", Enabled: true, DeviceID: testAlarmReceiverID,
			SourceIDs: []string{gb10DeviceID},
		}}},
		svr: &Server{memoryStorer: memory},
	}
	sourceConn := newFlowConnection()
	payload := []byte(strings.ReplaceAll(string(cascadeAlarmPayload()), testCascadeChannelID, gb10DeviceID))
	response := runFlowHandler(t, sourceConn, api, sip.MethodNotify, "notify-alarm-no-dispatch", payload, api.sipNotifyAlarm)
	assertFlowOK(t, response)
	select {
	case payload := <-receiverConn.writes:
		t.Fatalf("NOTIFY Alarm was dispatched to registered receiver: %s", payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCascadeAlarmDispatchFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			api := &GB28181API{}
			worker := registeredAlarmDispatchWorker(t, version)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				var alarm messageAlarm
				if err := sip.XMLDecode(request.Body(), &alarm); err != nil {
					t.Fatal(err)
				}
				if alarm.SN <= 0 || alarm.SN == 17 || alarm.DeviceID != testExposedChannelID {
					t.Fatalf("rewritten cascade Alarm = %+v", alarm)
				}
				if alarm.AlarmDescription != "  door alarm  " {
					t.Fatalf("AlarmDescription whitespace = %q", alarm.AlarmDescription)
				}
				if headers := request.GetHeaders("X-GB-Ver"); len(headers) != 1 || !strings.Contains(headers[0].String(), string(version)) {
					t.Fatalf("cascade Alarm X-GB-Ver = %v", headers)
				}

				responseBody := []byte(fmt.Sprintf(`<Response><CmdType>Alarm</CmdType><SN>%d</SN><DeviceID>%s</DeviceID><Result>OK</Result></Response>`, alarm.SN, alarm.DeviceID))
				conn := newFlowConnection()
				responseRequest := newFlowRequest(t, conn, sip.MethodMessage, "cascade-alarm-response-"+string(version), responseBody)
				responseCtx := &sip.Context{
					Request: responseRequest, Tx: sip.NewTransaction("cascade-alarm-response-tx-"+string(version), conn),
					DeviceID: worker.platform.serverID, Source: conn.remote,
				}
				responseCtx.Set(cascadeWorkerContextKey, worker)
				api.sipCascadeMessageMiddleware(responseCtx)
				select {
				case payload := <-conn.writes:
					if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
						t.Fatalf("cascade Alarm business SIP response = %s", payload)
					}
				case <-time.After(time.Second):
					t.Fatal("cascade Alarm business SIP response timeout")
				}
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}

			if err := api.dispatchAlarmToCascadeTarget(t.Context(), worker, gb10DeviceID, cascadeAlarmPayload()); err != nil {
				t.Fatal(err)
			}
			if count := alarmDispatchPendingCount(&api.pendingAlarmDispatch); count != 0 {
				t.Fatalf("pending cascade Alarm count = %d", count)
			}
		})
	}
}

func alarmDispatchPendingCount(items *sync.Map) int {
	count := 0
	items.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

func TestCascadeAlarmDispatchTargetSelectionIsOptInAndShared(t *testing.T) {
	localID := testCascadeChannelID
	enabled := registeredAlarmDispatchWorker(t, GBVersion20)
	disabled := registeredAlarmDispatchWorker(t, GBVersion20)
	disabled.platform.name = "disabled"
	disabled.platform.alarmDispatchEnabled = false
	unshared := registeredAlarmDispatchWorker(t, GBVersion20)
	unshared.platform.name = "unshared"
	delete(unshared.platform.channelIDMap, localID)
	offline := registeredAlarmDispatchWorker(t, GBVersion20)
	offline.platform.name = "offline"
	offline.updateStatus(func(status *CascadePlatformStatus) { status.Registered = false })

	manager := &CascadeManager{items: map[string]*cascadeWorker{
		"enabled": enabled, "disabled": disabled, "unshared": unshared, "offline": offline,
	}}
	workers := manager.alarmDispatchWorkers(localID)
	if len(workers) != 1 || workers[0] != enabled {
		t.Fatalf("Alarm dispatch workers = %+v", workers)
	}
	if workers := manager.alarmDispatchWorkers("34020000001320000999"); len(workers) != 0 {
		t.Fatalf("unshared Alarm dispatch workers = %+v", workers)
	}
}

func TestCascadeAlarmBusinessResponseStrictAndTargetCorrelated(t *testing.T) {
	api := &GB28181API{}
	worker := registeredAlarmDispatchWorker(t, GBVersion20)
	pending, _ := newPendingAlarmDispatch(t.Context())
	key := pendingAlarmDispatchKey{worker: worker, sn: 81, deviceID: testExposedChannelID}
	api.pendingAlarmDispatch.Store(key, pending)

	tests := []struct {
		name   string
		body   string
		status string
	}{
		{name: "wrong target", body: `<Response><CmdType>Alarm</CmdType><SN>81</SN><DeviceID>34020000001320000999</DeviceID><Result>OK</Result></Response>`, status: "400"},
		{name: "stale sequence", body: `<Response><CmdType>Alarm</CmdType><SN>82</SN><DeviceID>` + testExposedChannelID + `</DeviceID><Result>OK</Result></Response>`, status: "200"},
		{name: "duplicate result", body: `<Response><CmdType>Alarm</CmdType><SN>81</SN><DeviceID>` + testExposedChannelID + `</DeviceID><Result>OK</Result><Result>OK</Result></Response>`, status: "400"},
		{name: "invalid result", body: `<Response><CmdType>Alarm</CmdType><SN>81</SN><DeviceID>` + testExposedChannelID + `</DeviceID><Result>SUCCESS</Result></Response>`, status: "400"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn := newFlowConnection()
			request := newFlowRequest(t, conn, sip.MethodMessage, "invalid-cascade-alarm-"+test.name, []byte(test.body))
			ctx := &sip.Context{Request: request, Tx: sip.NewTransaction("invalid-cascade-alarm-tx-"+test.name, conn), DeviceID: worker.platform.serverID, Source: conn.remote}
			ctx.Set(cascadeWorkerContextKey, worker)
			api.sipCascadeMessageMiddleware(ctx)
			select {
			case payload := <-conn.writes:
				if !strings.Contains(string(payload), "SIP/2.0 "+test.status) {
					t.Fatalf("response = %s", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("SIP response timeout")
			}
			select {
			case outcome := <-pending.result:
				t.Fatalf("invalid response was delivered: %+v", outcome)
			default:
			}
		})
	}
}

func TestCascadeAlarmDispatchReportsBusinessError(t *testing.T) {
	api := &GB28181API{}
	worker := registeredAlarmDispatchWorker(t, GBVersion20)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		var alarm messageAlarm
		if err := sip.XMLDecode(request.Body(), &alarm); err != nil {
			return nil, err
		}
		key := pendingAlarmDispatchKey{worker: worker, sn: alarm.SN, deviceID: alarm.DeviceID}
		value, ok := api.pendingAlarmDispatch.Load(key)
		if !ok {
			return nil, fmt.Errorf("pending cascade Alarm is missing")
		}
		value.(*pendingAlarmDispatch).complete(alarmBusinessResponse{
			CmdType: "Alarm", SN: alarm.SN, DeviceID: alarm.DeviceID, Result: "ERROR",
		})
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	err := api.dispatchAlarmToCascadeTarget(t.Context(), worker, gb10DeviceID, cascadeAlarmPayload())
	if err == nil || !strings.Contains(err.Error(), "rejected: ERROR") {
		t.Fatalf("business ERROR result = %v", err)
	}
}

func TestCascadeAlarmDispatchStopsWithWorker(t *testing.T) {
	api := &GB28181API{}
	worker := registeredAlarmDispatchWorker(t, GBVersion20)
	started := make(chan struct{})
	worker.exchange = func(ctx context.Context, _ *sip.Request) (*sip.Response, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		done <- api.dispatchAlarmToCascadeTarget(t.Context(), worker, gb10DeviceID, cascadeAlarmPayload())
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade Alarm dispatch did not start")
	}
	worker.cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("dispatch cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade Alarm dispatch did not stop")
	}
}

func TestRemoveCascadeAlarmDispatchesIsWorkerScoped(t *testing.T) {
	api := &GB28181API{}
	workerA := registeredAlarmDispatchWorker(t, GBVersion20)
	workerB := registeredAlarmDispatchWorker(t, GBVersion20)
	pendingA, _ := newPendingAlarmDispatch(t.Context())
	pendingB, _ := newPendingAlarmDispatch(t.Context())
	keyA := pendingAlarmDispatchKey{worker: workerA, sn: 81, deviceID: testExposedChannelID}
	keyB := pendingAlarmDispatchKey{worker: workerB, sn: 81, deviceID: testExposedChannelID}
	api.pendingAlarmDispatch.Store(keyA, pendingA)
	api.pendingAlarmDispatch.Store(keyB, pendingB)

	api.removeCascadeAlarmDispatches(workerA)

	select {
	case outcome := <-pendingA.result:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("removed worker outcome = %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("removed worker Alarm dispatch was not canceled")
	}
	if _, ok := api.pendingAlarmDispatch.Load(keyA); ok {
		t.Fatal("removed worker Alarm dispatch remained registered")
	}
	if value, ok := api.pendingAlarmDispatch.Load(keyB); !ok || value != pendingB {
		t.Fatalf("unrelated worker Alarm dispatch = %#v, %v", value, ok)
	}
	select {
	case outcome := <-pendingB.result:
		t.Fatalf("unrelated worker Alarm dispatch was completed: %+v", outcome)
	default:
	}
}

func TestRemoveCascadeAlarmDispatchesConvergesWithBusinessResponse(t *testing.T) {
	for range 100 {
		api := &GB28181API{}
		worker := registeredAlarmDispatchWorker(t, GBVersion20)
		pending, _ := newPendingAlarmDispatch(t.Context())
		key := pendingAlarmDispatchKey{worker: worker, sn: 81, deviceID: testExposedChannelID}
		api.pendingAlarmDispatch.Store(key, pending)
		response := alarmBusinessResponse{CmdType: "Alarm", SN: 81, DeviceID: testExposedChannelID, Result: "OK"}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			pending.complete(response)
		}()
		go func() {
			defer wg.Done()
			api.removeCascadeAlarmDispatches(worker)
		}()
		wg.Wait()

		select {
		case outcome := <-pending.result:
			if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
				t.Fatalf("concurrent terminal outcome = %+v", outcome)
			}
			if outcome.err == nil && outcome.response.Result != "OK" {
				t.Fatalf("concurrent business outcome = %+v", outcome)
			}
		case <-time.After(time.Second):
			t.Fatal("concurrent Alarm dispatch did not converge")
		}
		if _, ok := api.pendingAlarmDispatch.Load(key); ok {
			t.Fatal("concurrent Alarm dispatch remained registered")
		}

		replacement := registeredAlarmDispatchWorker(t, GBVersion20)
		replacementPending, _ := newPendingAlarmDispatch(t.Context())
		replacementKey := pendingAlarmDispatchKey{worker: replacement, sn: key.sn, deviceID: key.deviceID}
		api.pendingAlarmDispatch.Store(replacementKey, replacementPending)
		api.removeCascadeAlarmDispatches(worker)
		if value, ok := api.pendingAlarmDispatch.Load(replacementKey); !ok || value != replacementPending {
			t.Fatalf("replacement worker Alarm dispatch = %#v, %v", value, ok)
		}
	}
}

func TestCascadeManagerApplyCancelsReplacedWorkerAlarmDispatch(t *testing.T) {
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	manager := NewCascadeManager(server)
	server.cascade = manager
	worker := registeredAlarmDispatchWorker(t, GBVersion20)
	close(worker.done)
	manager.items[worker.platform.name] = worker
	pending, _ := newPendingAlarmDispatch(t.Context())
	key := pendingAlarmDispatchKey{worker: worker, sn: 81, deviceID: testExposedChannelID}
	api.pendingAlarmDispatch.Store(key, pending)

	local := conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}
	if err := manager.Apply(local, nil); err != nil {
		t.Fatal(err)
	}

	select {
	case outcome := <-pending.result:
		if !errors.Is(outcome.err, context.Canceled) {
			t.Fatalf("replaced worker outcome = %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("CascadeManager.Apply did not cancel replaced worker Alarm dispatch")
	}
	if _, ok := api.pendingAlarmDispatch.Load(key); ok {
		t.Fatal("replaced worker Alarm dispatch remained registered")
	}
}
