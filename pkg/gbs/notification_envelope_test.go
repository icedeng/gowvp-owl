package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestKeepaliveRejectsInvalidEnvelopeBeforeLoadingOrState(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Response><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Response>`},
		{name: "wrong command", body: `<Notify><CmdType>DeviceStatus</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>Keepalive</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "device mismatch", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><Status>OK</Status></Notify>`},
		{name: "invalid status", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>ONLINE</Status></Notify>`},
		{name: "invalid fault device", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status><Info><DeviceID>bad</DeviceID></Info></Notify>`},
		{name: "duplicate SN", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><SN>2</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "unknown field", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status><Vendor>1</Vendor></Notify>`},
		{name: "root attribute", body: `<Notify vendor="x"><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "element attribute", body: `<Notify><CmdType vendor="x">Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "nested simple field", body: `<Notify><CmdType><Value>Keepalive</Value></CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`},
		{name: "out of order", body: `<Notify><CmdType>Keepalive</CmdType><DeviceID>` + gb10DeviceID + `</DeviceID><SN>1</SN><Status>OK</Status></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}}
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "keepalive-invalid-"+test.name, []byte(test.body), api.sipMessageKeepalive)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Keepalive response = %s", response)
			}
			if memory.runtime != nil || memory.persistent.IsOnline || !memory.persistent.KeepaliveAt.IsZero() {
				t.Fatalf("invalid Keepalive changed device state: persistent=%+v runtime=%+v", memory.persistent, memory.runtime)
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
				t.Fatalf("invalid Keepalive changed query state: %+v", state.DeviceStatus)
			}
		})
	}
}

func TestKeepaliveRejectsUnknownDeviceBeforeRuntimeAndState(t *testing.T) {
	const unknownDeviceID = "34020000001320000009"
	adapter, _, _ := newCascadeMediaCore(t)
	memory := &flowMemory{persistent: &ipc.Device{DeviceID: unknownDeviceID}}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + unknownDeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, unknownDeviceID, "keepalive-unknown", body)
	if !strings.Contains(response, "SIP/2.0 403") {
		t.Fatalf("unknown Keepalive response = %s", response)
	}
	if memory.runtime != nil || memory.persistent.IsOnline || !memory.persistent.KeepaliveAt.IsZero() {
		t.Fatalf("unknown Keepalive changed device state: persistent=%+v runtime=%+v", memory.persistent, memory.runtime)
	}
	if state, ok := api.GetQueryState(unknownDeviceID); ok && state.DeviceStatus != nil {
		t.Fatalf("unknown Keepalive changed query state: %+v", state.DeviceStatus)
	}
	if requests := api.metrics.Snapshot().CatalogRequests; requests != 0 {
		t.Fatalf("unknown Keepalive triggered %d Catalog requests", requests)
	}
}

func TestKeepaliveRejectsInactiveRegistrationBeforeState(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		online       bool
		registeredAt time.Time
		expires      int
		closed       bool
	}{
		{name: "expired", online: true, registeredAt: now.Add(-time.Minute), expires: 10},
		{name: "registration_closed", online: true, registeredAt: now, expires: 3600, closed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.persistent.IsOnline = test.online
			memory.persistent.RegisteredAt = orm.Time{Time: test.registeredAt}
			memory.persistent.Expires = test.expires
			memory.runtime.UpdateRuntime(func(device *Device) {
				device.IsOnline = test.online
				device.LastRegisterAt = test.registeredAt
				device.Expires = test.expires
				device.registrationClosed = test.closed
			})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

			response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-inactive-"+test.name, body)

			if !strings.Contains(response, "SIP/2.0 403") {
				t.Fatalf("inactive Keepalive response = %s", response)
			}
			if memory.persistent.IsOnline != test.online || !memory.persistent.KeepaliveAt.IsZero() {
				t.Fatalf("inactive Keepalive changed persistent state: %+v", memory.persistent)
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
				t.Fatalf("inactive Keepalive changed query state: %+v", state.DeviceStatus)
			}
		})
	}
}

func TestKeepaliveRestoresDeviceStatusOfflineWithinActiveRegistration(t *testing.T) {
	registeredAt := time.Now()
	memory := newFlowMemory(gb10DeviceID)
	memory.persistent.IsOnline = false
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = false
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
	})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	api.deviceOfflineTombstones.Store(gb10DeviceID, struct{}{})
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-status-offline", body)

	assertFlowOK(t, response)
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline || state.RegistrationClosed {
		t.Fatalf("Keepalive did not restore DeviceStatus offline state: persistent=%+v runtime=%+v", memory.persistent, state)
	}
	query, ok := api.GetQueryState(gb10DeviceID)
	if !ok || query.DeviceStatus == nil || query.DeviceStatus.Online != "ONLINE" {
		t.Fatalf("restored Keepalive query state = %+v", query)
	}
	if _, offline := api.deviceOfflineTombstones.Load(gb10DeviceID); offline {
		t.Fatal("online Keepalive retained offline-operation tombstone")
	}
}

func TestKeepalivePersistenceFailureKeepsObservedActivityAndRetries(t *testing.T) {
	persistErr := errors.New("keepalive persistence unavailable")
	registeredAt := time.Now().Add(-time.Minute)
	memory := &deviceStatusFailureMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		changeErr:  persistErr,
	}
	memory.persistent.IsOnline = false
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	registrationClosed := false
	memory.persistent.Ext.GBRegistrationClosed = &registrationClosed
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = false
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	conn := newFlowConnection()
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, conn, api, gb10DeviceID, "keepalive-persistence-failure", body)

	assertFlowOK(t, response)
	state := memory.runtime.runtimeSnapshot()
	if memory.persistent.IsOnline {
		t.Fatal("failed Keepalive persistence changed persistent online state")
	}
	if !state.IsOnline || state.LastKeepaliveAt.IsZero() || state.Conn != conn || state.Source != conn.remote ||
		state.RegistrationClosed || state.OfflinePersistencePending || !state.KeepalivePersistencePending {
		t.Fatalf("failed Keepalive persistence lost observed activity: %+v", state)
	}

	memory.changeErr = nil
	server.checkOfflineDevices(time.Now())
	state = memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || memory.persistent.KeepaliveAt.IsZero() || !state.IsOnline ||
		state.LastKeepaliveAt.IsZero() || state.RegistrationClosed || state.OfflinePersistencePending ||
		state.KeepalivePersistencePending {
		t.Fatalf("retried Keepalive state = persistent:%+v runtime:%+v", memory.persistent, state)
	}
}

func TestPendingKeepaliveCannotOverwriteNewRegistration(t *testing.T) {
	persistErr := errors.New("keepalive persistence unavailable")
	registeredAt := time.Now().Add(-time.Minute)
	memory := &deviceStatusFailureMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		changeErr:  persistErr,
	}
	memory.persistent.IsOnline = false
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	registrationClosed := false
	memory.persistent.Ext.GBRegistrationClosed = &registrationClosed
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = false
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-stale-generation", body)
	assertFlowOK(t, response)
	pending := memory.runtime.runtimeSnapshot()
	if !pending.KeepalivePersistencePending {
		t.Fatalf("pending Keepalive state = %+v", pending)
	}

	newRegisteredAt := time.Now()
	memory.changeErr = nil
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: newRegisteredAt}
	memory.persistent.Expires = 7200
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = newRegisteredAt
		device.Expires = 7200
		clearPendingKeepaliveLocked(device)
	})
	changed, err := api.retryPendingKeepalive(gb10DeviceID, pending)
	if err != nil || changed {
		t.Fatalf("stale Keepalive retry = changed %t, err %v", changed, err)
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline || state.KeepalivePersistencePending ||
		!state.LastRegisterAt.Equal(newRegisteredAt) || state.Expires != 7200 {
		t.Fatalf("new registration overwritten: persistent=%+v runtime=%+v", memory.persistent, state)
	}
}

func TestAdmittedKeepaliveCannotOverwriteNewRegistration(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			testAdmittedKeepaliveCannotOverwriteNewRegistration(t, version)
		})
	}
}

func testAdmittedKeepaliveCannotOverwriteNewRegistration(t *testing.T, version GBProtocolVersion) {
	oldRegisteredAt := time.Now().Add(-time.Minute)
	newRegisteredAt := time.Now()
	oldConn := newFlowConnection()
	newConn := newFlowConnection()
	newConn.remote = &net.UDPAddr{IP: net.ParseIP("198.51.100.20"), Port: 15060}
	newTo := mustFlowAddress(t, "sip:"+gb10DeviceID+"@198.51.100.20:15060")
	memory := newFlowMemory(gb10DeviceID)
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: oldRegisteredAt}
	memory.persistent.Expires = 3600
	memory.runtime.setGBVersion(version)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = oldRegisteredAt
		device.Expires = 3600
		device.conn = oldConn
		device.source = oldConn.remote
	})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>94</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Status>OFF</Status></Notify>`)
	request := newFlowRequest(t, oldConn, sip.MethodMessage, "keepalive-old-registration", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("keepalive-old-registration-tx", oldConn),
		DeviceID: gb10DeviceID, Source: oldConn.remote,
		To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060"), Log: slog.Default(),
	}

	api.sipAccessControlMiddleware(ctx)
	if ctx.IsAborted() {
		t.Fatal("active old registration was rejected")
	}

	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: newRegisteredAt}
	memory.persistent.KeepaliveAt = orm.Time{Time: newRegisteredAt}
	memory.persistent.Expires = 7200
	memory.persistent.Address = newConn.remote.String()
	memory.persistent.Transport = "tcp"
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = newRegisteredAt
		device.LastKeepaliveAt = newRegisteredAt
		device.Expires = 7200
		device.Address = newConn.remote.String()
		device.conn = newConn
		device.source = newConn.remote
		device.to = newTo
	})

	api.sipMessageKeepalive(ctx)
	select {
	case payload := <-oldConn.writes:
		assertFlowOK(t, string(payload))
	default:
		t.Fatal("late Keepalive was not acknowledged")
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok {
		t.Fatalf("late Keepalive updated QueryState: %+v", state)
	}
	state := memory.runtime.runtimeSnapshot()
	if !memory.persistent.IsOnline || !state.IsOnline ||
		!memory.persistent.RegisteredAt.Time.Equal(newRegisteredAt) ||
		!memory.persistent.KeepaliveAt.Time.Equal(newRegisteredAt) ||
		!state.LastRegisterAt.Equal(newRegisteredAt) || !state.LastKeepaliveAt.Equal(newRegisteredAt) ||
		memory.persistent.Expires != 7200 || state.Expires != 7200 ||
		memory.persistent.Address != newConn.remote.String() || memory.persistent.Transport != "tcp" ||
		state.Address != newConn.remote.String() || state.Conn != newConn || state.Source.String() != newConn.remote.String() {
		t.Fatalf("late Keepalive overwrote new registration: persistent=%+v runtime=%+v", memory.persistent, state)
	}
}

func TestNewDeviceStatusSupersedesPendingKeepalive(t *testing.T) {
	persistErr := errors.New("keepalive persistence unavailable")
	registeredAt := time.Now().Add(-time.Minute)
	memory := &deviceStatusFailureMemory{
		flowMemory: newFlowMemory(gb10DeviceID),
		changeErr:  persistErr,
	}
	memory.persistent.IsOnline = false
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	registrationClosed := false
	memory.persistent.Ext.GBRegistrationClosed = &registrationClosed
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = false
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
	})
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-before-device-status", body)
	assertFlowOK(t, response)
	pending := memory.runtime.runtimeSnapshot()
	if !pending.KeepalivePersistencePending {
		t.Fatalf("pending Keepalive state = %+v", pending)
	}

	memory.changeErr = nil
	if err := api.applyDeviceStatus(gb10DeviceID, &DeviceStatusData{Online: "OFFLINE"}); err != nil {
		t.Fatal(err)
	}
	state := memory.runtime.runtimeSnapshot()
	if memory.persistent.IsOnline || state.IsOnline || state.KeepalivePersistencePending {
		t.Fatalf("new DeviceStatus did not supersede pending Keepalive: persistent=%+v runtime=%+v", memory.persistent, state)
	}
	changed, err := api.retryPendingKeepalive(gb10DeviceID, pending)
	if err != nil || changed {
		t.Fatalf("superseded Keepalive retry = changed %t, err %v", changed, err)
	}
	if memory.persistent.IsOnline || memory.runtime.runtimeSnapshot().IsOnline {
		t.Fatal("superseded Keepalive restored DeviceStatus offline state")
	}
}

func TestKeepaliveRejectsExpiredPersistedDeviceMissingFromMemory(t *testing.T) {
	now := time.Now()
	adapter, persisted, _ := newCascadeMediaCore(t)
	registeredAt := now.Add(-time.Minute)
	if err := adapter.Store().Device().Update(t.Context(), &ipc.Device{}, func(device *ipc.Device) error {
		device.IsOnline = true
		device.RegisteredAt = orm.Time{Time: registeredAt}
		device.Expires = 10
		return nil
	}, orm.Where("device_id=?", gb10DeviceID)); err != nil {
		t.Fatal(err)
	}
	persisted.IsOnline = true
	persisted.RegisteredAt = orm.Time{Time: registeredAt}
	persisted.Expires = 10
	keepaliveAt := persisted.KeepaliveAt.Time
	memory := &flowMemory{persistent: persisted}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-expired-persisted", body)

	if !strings.Contains(response, "SIP/2.0 403") {
		t.Fatalf("expired persisted Keepalive response = %s", response)
	}
	if memory.runtime != nil || !memory.persistent.KeepaliveAt.Time.Equal(keepaliveAt) {
		t.Fatalf("expired persisted Keepalive changed state: persistent=%+v runtime=%+v", memory.persistent, memory.runtime)
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
		t.Fatalf("expired persisted Keepalive changed query state: %+v", state.DeviceStatus)
	}
}

func TestKeepaliveRestoresPersistedDeviceMissingFromMemory(t *testing.T) {
	adapter, persisted, _ := newCascadeMediaCore(t)
	registeredAt := time.Now()
	if err := adapter.Store().Device().Update(t.Context(), &ipc.Device{}, func(device *ipc.Device) error {
		device.IsOnline = true
		device.RegisteredAt = orm.Time{Time: registeredAt}
		device.Expires = 3600
		return nil
	}, orm.Where("device_id=?", gb10DeviceID)); err != nil {
		t.Fatal(err)
	}
	persisted.IsOnline = true
	persisted.RegisteredAt = orm.Time{Time: registeredAt}
	persisted.Expires = 3600
	memory := &flowMemory{persistent: persisted}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: memory},
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
	}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-persisted", body)
	assertFlowOK(t, response)
	if memory.runtime == nil || !memory.persistent.IsOnline || memory.persistent.KeepaliveAt.IsZero() {
		t.Fatalf("persisted Keepalive was not restored: persistent=%+v runtime=%+v", memory.persistent, memory.runtime)
	}
	if state, ok := api.GetQueryState(gb10DeviceID); !ok || state.DeviceStatus == nil {
		t.Fatalf("persisted Keepalive query state = %+v", state.DeviceStatus)
	}
	if requests := api.metrics.Snapshot().CatalogRequests; requests != 1 {
		t.Fatalf("persisted Keepalive Catalog requests = %d, want 1", requests)
	}
}

func TestKeepaliveRestoresPersistedDeviceStatusOfflineBinding(t *testing.T) {
	adapter, persisted, _ := newCascadeMediaCore(t)
	registeredAt := time.Now()
	registrationClosed := false
	if err := adapter.Store().Device().Update(t.Context(), &ipc.Device{}, func(device *ipc.Device) error {
		device.IsOnline = false
		device.RegisteredAt = orm.Time{Time: registeredAt}
		device.Expires = 3600
		device.Ext.GBRegistrationClosed = &registrationClosed
		return nil
	}, orm.Where("device_id=?", gb10DeviceID)); err != nil {
		t.Fatal(err)
	}
	persisted.IsOnline = false
	persisted.RegisteredAt = orm.Time{Time: registeredAt}
	persisted.Expires = 3600
	persisted.Ext.GBRegistrationClosed = &registrationClosed
	memory := &flowMemory{persistent: persisted}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: memory},
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
	}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-persisted-status-offline", body)

	assertFlowOK(t, response)
	if memory.runtime == nil || !memory.persistent.IsOnline || memory.persistent.Ext.GBRegistrationClosed == nil ||
		*memory.persistent.Ext.GBRegistrationClosed {
		t.Fatalf("persisted DeviceStatus offline binding was not restored: persistent=%+v runtime=%+v", memory.persistent, memory.runtime)
	}
}

func TestKeepaliveDoesNotCreateRuntimeWhenDeviceStoreUnavailable(t *testing.T) {
	memory := &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>OK</Status></Notify>`)

	response := runKeepaliveHandlerForDevice(t, newFlowConnection(), api, gb10DeviceID, "keepalive-store-unavailable", body)
	if !strings.Contains(response, "SIP/2.0 503") {
		t.Fatalf("unavailable Keepalive response = %s", response)
	}
	if memory.runtime != nil || memory.persistent.IsOnline || !memory.persistent.KeepaliveAt.IsZero() {
		t.Fatalf("unavailable Keepalive changed device state: persistent=%+v runtime=%+v", memory.persistent, memory.runtime)
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && state.DeviceStatus != nil {
		t.Fatalf("unavailable Keepalive changed query state: %+v", state.DeviceStatus)
	}
}

func runKeepaliveHandlerForDevice(t *testing.T, conn *flowConnection, api *GB28181API, deviceID, callID string, body []byte) string {
	t.Helper()
	request := newFlowRequest(t, conn, sip.MethodMessage, callID, body)
	api.sipMessageKeepalive(&sip.Context{
		Request:  request,
		Tx:       sip.NewTransaction(callID+"-tx", conn),
		DeviceID: deviceID,
		Source:   conn.remote,
		To:       mustFlowAddress(t, "sip:"+deviceID+"@3402000000"),
		Log:      slog.Default(),
	})
	select {
	case payload := <-conn.writes:
		return string(payload)
	case <-time.After(time.Second):
		t.Fatalf("Keepalive response timeout")
		return ""
	}
}

func TestKeepalivePreservesDocumentedVendorStatusCompatibility(t *testing.T) {
	for _, status := range []string{"", "ON", "OFF"} {
		memory := newFlowMemory(gb10DeviceID)
		api := &GB28181API{svr: &Server{memoryStorer: memory}}
		body := `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID>`
		if status != "" {
			body += `<Status>` + status + `</Status>`
		}
		body += `</Notify>`
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "keepalive-compatible-"+status, []byte(body), api.sipMessageKeepalive)
		assertFlowOK(t, response)
		if memory.persistent.IsOnline != (status == "" || status == "ON") {
			t.Fatalf("Keepalive status %q online = %v", status, memory.persistent.IsOnline)
		}
	}
}

func TestKeepaliveErrorStatusKeepsDeviceOnline(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	body := `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Status>ERROR</Status><Info><DeviceID>34020000001320000002</DeviceID></Info></Notify>`
	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "keepalive-error-online", []byte(body), api.sipMessageKeepalive)
	assertFlowOK(t, response)
	if !memory.persistent.IsOnline || memory.persistent.KeepaliveAt.IsZero() {
		t.Fatalf("ERROR Keepalive device state = %+v", memory.persistent)
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.DeviceStatus == nil || state.DeviceStatus.Online != "ONLINE" || state.DeviceStatus.Status != "ERROR" {
		t.Fatalf("ERROR Keepalive query state = %+v", state.DeviceStatus)
	}
}

func TestAlarmRejectsInvalidEnvelopeBeforeStateAndCallback(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	callbacks := make(chan *AlarmEvent, 1)
	api.SetAlarmHandler(func(context.Context, *AlarmEvent) { callbacks <- &AlarmEvent{} })
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Response><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Response>`},
		{name: "wrong command", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>Alarm</CmdType><SN>0</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "unknown target", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "invalid priority", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>5</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "invalid method", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>8</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "invalid time", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>bad</AlarmTime></Notify>`},
		{name: "invalid coordinate", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Longitude>NaN</Longitude></Notify>`},
		{name: "longitude out of range", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Longitude>181</Longitude></Notify>`},
		{name: "latitude out of range", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Latitude>-91</Latitude></Notify>`},
		{name: "2011 alarm type extension", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><AlarmType>1</AlarmType></Info></Notify>`},
		{name: "2011 event type extension", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><AlarmTypeParam><EventType>1</EventType></AlarmTypeParam></Info></Notify>`},
		{name: "2011 Appendix A.4 extension", body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><doorType><DeviceID>` + gb10ChannelID + `</DeviceID></doorType></Info></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "alarm-invalid-"+test.name, []byte(test.body), api.sipMessageAlarm)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Alarm response = %s", response)
			}
		})
	}
	select {
	case event := <-callbacks:
		t.Fatalf("invalid Alarm invoked callback: %+v", event)
	default:
	}
	if state, ok := api.GetQueryState(gb10DeviceID); ok && len(state.AppendixA4) > 0 {
		t.Fatalf("invalid Alarm changed Appendix A.4 state: %+v", state.AppendixA4)
	}
}

func TestAlarmAppendixA4StateBelongsToReportedChannel(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	defer func() { config = previousConfig }()

	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	memory.runtime.Channels.Store(persistentChannel.ChannelID, &Channel{
		ChannelID: persistentChannel.ChannelID,
		device:    memory.runtime,
	})
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>91</SN><DeviceID>` + persistentChannel.ChannelID +
		`</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime>` +
		`<Info><doorType><DeviceID>` + persistentChannel.ChannelID + `</DeviceID></doorType></Info></Notify>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"alarm-channel-a4-state", body, api.sipMessageAlarm)
	assertFlowOK(t, response)
	channelState, ok := api.GetQueryState(persistentChannel.ChannelID)
	if !ok || len(channelState.AppendixA4) != 1 || channelState.AppendixA4[0].Type != "doorType" {
		t.Fatalf("Alarm channel Appendix A.4 state = %+v", channelState)
	}
	if parentState, ok := api.GetQueryState(persistentDevice.DeviceID); ok && len(parentState.AppendixA4) != 0 {
		t.Fatalf("Alarm channel Appendix A.4 overwrote parent runtime state: %+v", parentState.AppendixA4)
	}
	var parent ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &parent,
		orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if len(parent.Ext.GBAppendixA4) != 1 || parent.Ext.GBAppendixA4[0].Type != "doorType" {
		t.Fatalf("Alarm Appendix A.4 was not persisted on parent: %+v", parent.Ext.GBAppendixA4)
	}
}

func TestAlarmRejectsInvalidStructureBeforeSideEffectsByVersion(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
	}{
		{name: "duplicate SN", version: GBVersion10, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><SN>2</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "unknown field", version: GBVersion11, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Vendor>1</Vendor></Notify>`},
		{name: "out of order", version: GBVersion20, body: `<Notify><CmdType>Alarm</CmdType><DeviceID>` + gb10ChannelID + `</DeviceID><SN>1</SN><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "root attribute", version: GBVersion30, body: `<Notify vendor="x"><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "simple element attribute", version: GBVersion10, body: `<Notify><CmdType>Alarm</CmdType><SN vendor="x">1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "simple element nesting", version: GBVersion11, body: `<Notify><CmdType>Alarm</CmdType><SN><Value>1</Value></SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime></Notify>`},
		{name: "top level AlarmType", version: GBVersion20, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><AlarmType>1</AlarmType></Notify>`},
		{name: "Info AlarmMethod", version: GBVersion20, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><AlarmMethod>2</AlarmMethod></Info></Notify>`},
		{name: "duplicate typed AlarmType", version: GBVersion20, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><AlarmType>1</AlarmType><AlarmType>1</AlarmType></Info></Notify>`},
		{name: "empty typed AlarmType", version: GBVersion20, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><AlarmType> </AlarmType></Info></Notify>`},
		{name: "mixed typed and plain Info", version: GBVersion20, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>2</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><AlarmType>1</AlarmType>legacy</Info></Notify>`},
		{name: "duplicate EventType", version: GBVersion30, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><Info><AlarmType>6</AlarmType><AlarmTypeParam><EventType>1</EventType><EventType>2</EventType></AlarmTypeParam></Info></Notify>`},
		{name: "Info after ExtraInfo", version: GBVersion30, body: `<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-26T01:00:00</AlarmTime><ExtraInfo>modern</ExtraInfo><Info><AlarmType>6</AlarmType></Info></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			callbacks := make(chan *AlarmEvent, 1)
			api.SetAlarmHandler(func(_ context.Context, event *AlarmEvent) { callbacks <- event })
			conn := newFlowConnection()
			response := runFlowHandler(t, conn, api, sip.MethodMessage, "alarm-invalid-structure-"+test.name, []byte(test.body), api.sipMessageAlarm)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Alarm response = %s", response)
			}
			select {
			case payload := <-conn.writes:
				t.Fatalf("invalid Alarm produced an extra SIP message: %s", payload)
			default:
			}
			select {
			case event := <-callbacks:
				t.Fatalf("invalid Alarm invoked callback: %+v", event)
			default:
			}
			if state, ok := api.GetQueryState(gb10DeviceID); ok && len(state.AppendixA4) > 0 {
				t.Fatalf("invalid Alarm changed Appendix A.4 state: %+v", state.AppendixA4)
			}
		})
	}
}

func TestAlarmMessageSendsSeparateBusinessResponseByVersion(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			baseConn := newFlowConnection()
			conn := &tcpFlowConnection{flowConnection: baseConn}
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
			device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
			sipServer := sip.NewServer(platform)
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			memory.runtime.UpdateRuntime(func(current *Device) {
				current.conn = conn
				current.source = baseConn.remote
				current.to = device
			})
			api := &GB28181API{}
			server := &Server{Server: sipServer, gb: api, fromAddress: *platform, memoryStorer: memory}
			api.svr = server

			response := runFlowHandler(t, baseConn, api, sip.MethodMessage, "alarm-business-response-"+string(version), readGB10Fixture(t, "alarm-notify.xml"), api.sipMessageAlarm)
			if !strings.Contains(response, "SIP/2.0 200 OK") || !strings.Contains(response, "Content-Length: 0") || strings.Contains(response, "<Response>") {
				t.Fatalf("%s Alarm SIP acknowledgement must have an empty body:\n%s", version, response)
			}
			select {
			case payload := <-baseConn.writes:
				business := string(payload)
				for _, required := range []string{
					"MESSAGE ", "X-GB-Ver: " + string(version), "<Response>", "<CmdType>Alarm</CmdType>",
					"<SN>4</SN>", "<DeviceID>" + gb10ChannelID + "</DeviceID>", "<Result>OK</Result>",
				} {
					if !strings.Contains(business, required) {
						t.Fatalf("%s Alarm business response missing %q:\n%s", version, required, business)
					}
				}
			case <-time.After(time.Second):
				t.Fatalf("%s Alarm business MESSAGE was not sent", version)
			}
			server.Close()
		})
	}
}

func TestAlarmNotifyDoesNotSendMessageBusinessResponse(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	conn := newFlowConnection()
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	response := runFlowHandler(t, conn, api, sip.MethodNotify, "alarm-notify-no-business-response", readGB10Fixture(t, "alarm-notify.xml"), api.sipNotifyAlarm)
	assertFlowOK(t, response)
	for _, expected := range []string{"Content-Type: Application/MANSCDP+xml", "<Response><CmdType>Alarm</CmdType>", "<Result>OK</Result></Response>"} {
		if !strings.Contains(response, expected) {
			t.Fatalf("2011 Alarm NOTIFY response missing %q:\n%s", expected, response)
		}
	}
	select {
	case payload := <-conn.writes:
		t.Fatalf("Alarm NOTIFY produced an extra business MESSAGE:\n%s", payload)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestAlarmHandlerUsesServiceLifecycleContext(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	lifecycleCtx, cancel := context.WithCancel(t.Context())
	cancel()
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}, lifecycleCtx: lifecycleCtx}
	callbackErr := make(chan error, 1)
	api.SetAlarmHandler(func(ctx context.Context, _ *AlarmEvent) {
		callbackErr <- ctx.Err()
	})

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "alarm-lifecycle-context", readGB10Fixture(t, "alarm-notify.xml"), api.sipNotifyAlarm)
	assertFlowOK(t, response)
	select {
	case err := <-callbackErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Alarm handler context error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm handler callback timeout")
	}
}

func TestAlarmDescriptionWhitespacePreservedByVersion(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })

	const description = "  west gate alarm  "
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			callbacks := make(chan *AlarmEvent, 1)
			api.SetAlarmHandler(func(_ context.Context, event *AlarmEvent) { callbacks <- event })
			body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID +
				`</DeviceID><AlarmPriority> 1 </AlarmPriority><AlarmMethod> 2 </AlarmMethod>` +
				`<AlarmTime> 2026-08-26T01:00:00 </AlarmTime><AlarmDescription>` + description +
				`</AlarmDescription><Longitude> 120 </Longitude><Latitude> 30 </Latitude></Notify>`)

			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodNotify, "alarm-description-whitespace-"+string(version), body, api.sipNotifyAlarm)
			assertFlowOK(t, response)
			select {
			case event := <-callbacks:
				if event.AlarmDescription != description {
					t.Fatalf("AlarmDescription = %q, want %q", event.AlarmDescription, description)
				}
				if event.AlarmPriority != "1" || event.AlarmMethod != "2" || event.AlarmTime != "2026-08-26T01:00:00" || event.Longitude != "120" || event.Latitude != "30" {
					t.Fatalf("normalized Alarm fields = %+v", event)
				}
			case <-time.After(time.Second):
				t.Fatal("Alarm callback timeout")
			}
		})
	}
}

func TestAlarmTypeAndEventTypeRulesByVersion(t *testing.T) {
	tests := []struct {
		name      string
		version   GBProtocolVersion
		method    string
		alarmType string
		eventType *int
		wantErr   bool
	}{
		{name: "2011 rejects later type extension", version: GBVersion10, method: "2", alarmType: "1", wantErr: true},
		{name: "2014 rejects later type extension", version: GBVersion11, method: "2", alarmType: "1", wantErr: true},
		{name: "2014 rejects later event extension", version: GBVersion11, method: "5", eventType: intPointer(1), wantErr: true},
		{name: "2016 device alarm boundary", version: GBVersion20, method: "2", alarmType: "5"},
		{name: "2016 invalid device alarm type", version: GBVersion20, method: "2", alarmType: "6", wantErr: true},
		{name: "2016 video alarm boundary", version: GBVersion20, method: "5", alarmType: "12"},
		{name: "2016 rejects 2022 video content type", version: GBVersion20, method: "5", alarmType: "13", wantErr: true},
		{name: "2022 video content type", version: GBVersion30, method: "5", alarmType: "13"},
		{name: "type requires typed method", version: GBVersion30, method: "1", alarmType: "1", wantErr: true},
		{name: "intrusion entry event", version: GBVersion20, method: "5", alarmType: "6", eventType: intPointer(1)},
		{name: "intrusion exit event", version: GBVersion30, method: "5", alarmType: "6", eventType: intPointer(2)},
		{name: "invalid intrusion event", version: GBVersion30, method: "5", alarmType: "6", eventType: intPointer(3), wantErr: true},
		{name: "event on non-intrusion alarm", version: GBVersion30, method: "5", alarmType: "5", eventType: intPointer(1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			msg := &messageAlarm{
				XMLName: xml.Name{Local: "Notify"}, CmdType: "Alarm", SN: 1, DeviceID: gb10ChannelID,
				AlarmPriority: "1", AlarmMethod: test.method, AlarmTime: "2026-08-26T01:00:00", Info: []alarmInfoXML{{}},
			}
			msg.Info[0].AlarmType = test.alarmType
			msg.Info[0].AlarmTypeParam.EventType = test.eventType
			err := api.validateAlarmEnvelope(&sip.Context{DeviceID: gb10DeviceID}, msg)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateAlarmEnvelope() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestAlarmCenterIdentifierBindingByVersion(t *testing.T) {
	const alarmCenterID = "3402000000"
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(version)
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			base := messageAlarm{
				XMLName: xml.Name{Local: "Notify"}, CmdType: "Alarm", SN: 1,
				DeviceID: alarmCenterID, AlarmPriority: "1", AlarmMethod: "2",
				AlarmTime: "2026-08-26T01:00:00",
			}

			if err := api.validateAlarmEnvelope(&sip.Context{DeviceID: gb10DeviceID}, &base); err != nil {
				t.Fatalf("valid Alarm center identifier rejected: %v", err)
			}

			for _, invalid := range []string{"6501000000", "340200000X"} {
				msg := base
				msg.DeviceID = invalid
				if err := api.validateAlarmEnvelope(&sip.Context{DeviceID: gb10DeviceID}, &msg); err == nil {
					t.Fatalf("invalid Alarm center identifier %q accepted", invalid)
				}
			}
		})
	}
}

func TestAlarmExtensionInfoVersionMatrix(t *testing.T) {
	base := `<Notify><CmdType>Alarm</CmdType><SN>11</SN><DeviceID>` + gb10ChannelID +
		`</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod>` +
		`<AlarmTime>2026-08-26T01:00:00</AlarmTime>`
	tests := []struct {
		name      string
		version   GBProtocolVersion
		extra     string
		wantOK    bool
		wantType  string
		wantEvent *int
	}{
		{name: "2011 multiple plain Info", version: GBVersion10, extra: `<Info>legacy</Info><Info>second</Info>`, wantOK: true},
		{name: "2014 multiple plain Info", version: GBVersion11, extra: `<Info>legacy</Info><Info>second</Info>`, wantOK: true},
		{name: "2016 typed Info", version: GBVersion20, extra: `<Info><AlarmType>6</AlarmType><AlarmTypeParam><EventType>1</EventType></AlarmTypeParam></Info>`, wantOK: true, wantType: "6", wantEvent: intPointer(1)},
		{name: "2016 plain Info", version: GBVersion20, extra: `<Info>legacy</Info>`, wantOK: true},
		{name: "2016 typed then multiple plain Info", version: GBVersion20, extra: `<Info><AlarmType>6</AlarmType></Info><Info>legacy</Info><Info>second</Info>`, wantOK: true, wantType: "6"},
		{name: "2016 rejects structured Info after plain Info", version: GBVersion20, extra: `<Info>legacy</Info><Info><AlarmType>6</AlarmType></Info>`},
		{name: "2016 rejects duplicate structured Info", version: GBVersion20, extra: `<Info><AlarmType>6</AlarmType></Info><Info><AlarmType>6</AlarmType></Info>`},
		{name: "2022 typed Info and ExtraInfo", version: GBVersion30, extra: `<Info><AlarmType>13</AlarmType></Info><ExtraInfo>modern</ExtraInfo>`, wantOK: true, wantType: "13"},
		{name: "2022 rejects multiple Info", version: GBVersion30, extra: `<Info><AlarmType>13</AlarmType></Info><Info><doorType><DeviceID>` + gb10ChannelID + `</DeviceID></doorType></Info>`},
		{name: "2014 rejects typed Info", version: GBVersion11, extra: `<Info><AlarmType>1</AlarmType></Info>`},
		{name: "2016 rejects ExtraInfo", version: GBVersion20, extra: `<ExtraInfo>modern</ExtraInfo>`},
		{name: "2022 rejects plain Info", version: GBVersion30, extra: `<Info>legacy</Info>`},
		{name: "2022 rejects unknown structured Info", version: GBVersion30, extra: `<Info><VendorExtension/></Info>`},
		{name: "2011 Info length", version: GBVersion10, extra: `<Info>` + strings.Repeat("测", 1025) + `</Info>`},
		{name: "2022 ExtraInfo length", version: GBVersion30, extra: `<ExtraInfo>` + strings.Repeat("测", 1025) + `</ExtraInfo>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(test.version)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			var msg messageAlarm
			body := []byte(base + test.extra + `</Notify>`)
			err := validateAlarmStructure(body, test.version)
			if err == nil {
				err = sip.XMLDecode(body, &msg)
			}
			if err == nil {
				err = api.validateAlarmEnvelope(&sip.Context{DeviceID: gb10DeviceID}, &msg)
			}
			if err == nil {
				_, err = api.validateAndDecodeAppendixA4(gb10DeviceID, "Alarm", body)
			}
			if (err == nil) != test.wantOK {
				t.Fatalf("Alarm extension validation error = %v, wantOK %v", err, test.wantOK)
			}
			if !test.wantOK {
				return
			}
			if msg.normalizedAlarmType != test.wantType {
				t.Fatalf("normalized AlarmType = %q, want %q", msg.normalizedAlarmType, test.wantType)
			}
			if test.wantEvent != nil && (msg.normalizedEventType == nil || *msg.normalizedEventType != *test.wantEvent) {
				t.Fatalf("normalized EventType = %v, want %d", msg.normalizedEventType, *test.wantEvent)
			}
		})
	}
}

func TestMediaStatusRejectsInvalidEnvelopeAndTargetBeforeSessionStop(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-invalid"}
	streams.Store(key, stream)
	api := &GB28181API{streams: streams}
	setMediaStatusTestVersion(t, api, GBVersion11)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Response><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Response>`},
		{name: "wrong command", body: `<Notify><CmdType>Keepalive</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`},
		{name: "non-positive SN", body: `<Notify><CmdType>MediaStatus</CmdType><SN>0</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`},
		{name: "missing type", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID></Notify>`},
		{name: "unknown target", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>34020000001320000009</DeviceID><NotifyType>121</NotifyType></Notify>`},
		{name: "duplicate SN", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><SN>2</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`},
		{name: "duplicate type", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType><NotifyType>121</NotifyType></Notify>`},
		{name: "unknown field", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType><Vendor>1</Vendor></Notify>`},
		{name: "root attribute", body: `<Notify vendor="x"><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`},
		{name: "element attribute", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType vendor="x">121</NotifyType></Notify>`},
		{name: "nested type", body: `<Notify><CmdType>MediaStatus</CmdType><SN>1</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType><Value>121</Value></NotifyType></Notify>`},
		{name: "out of order", body: `<Notify><CmdType>MediaStatus</CmdType><DeviceID>` + gb10ChannelID + `</DeviceID><SN>1</SN><NotifyType>121</NotifyType></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "media-status-invalid", []byte(test.body), api.sipMessageMediaStatus)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid MediaStatus response = %s", response)
			}
			if current, ok := streams.Load(key); !ok || current != stream || stream.Stop {
				t.Fatal("invalid MediaStatus stopped history session")
			}
		})
	}
}
