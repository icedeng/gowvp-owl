package gbs

import (
	"net"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestOfflineCheckWithPostgresTimestampPrecision(t *testing.T) {
	now := time.Now().Truncate(time.Second).Add(123456789 * time.Nanosecond)
	for _, test := range []struct {
		name         string
		registeredAt time.Time
		keepaliveAt  time.Time
		wantOnline   bool
		wantClosed   bool
	}{
		{name: "heartbeat_timeout", registeredAt: now.Add(-15 * time.Minute), keepaliveAt: now.Add(-14 * time.Minute)},
		{name: "registration_expired", registeredAt: now.Add(-2 * time.Hour), keepaliveAt: now, wantClosed: true},
		{name: "recent_heartbeat", registeredAt: now.Add(-15 * time.Minute), keepaliveAt: now.Add(-time.Minute), wantOnline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			memory := newPostgresPrecisionOfflineMemory(t, test.registeredAt, test.keepaliveAt)
			api := &GB28181API{}
			server := &Server{gb: api, memoryStorer: memory}
			api.svr = server

			server.checkOfflineDevices(now)

			state := memory.runtime.runtimeSnapshot()
			if memory.persistent.IsOnline != test.wantOnline || state.IsOnline != test.wantOnline {
				t.Fatalf("online state = persistent %t, runtime %t; want %t", memory.persistent.IsOnline, state.IsOnline, test.wantOnline)
			}
			if persistedRegistrationClosed(memory.persistent) != test.wantClosed || state.RegistrationClosed != test.wantClosed {
				t.Fatalf("registration closed = persistent %t, runtime %t; want %t", persistedRegistrationClosed(memory.persistent), state.RegistrationClosed, test.wantClosed)
			}
		})
	}
}

func TestOfflineCheckPreservesActivityWithPostgresTimestampPrecision(t *testing.T) {
	old := time.Now().Add(-15 * time.Minute).Truncate(time.Second).Add(123456789 * time.Nanosecond)
	for _, test := range []struct {
		name     string
		register bool
		runtime  bool
		advance  time.Duration
	}{
		{name: "new_runtime_heartbeat_same_microsecond", runtime: true, advance: time.Nanosecond},
		{name: "new_runtime_registration_same_microsecond", register: true, runtime: true, advance: time.Nanosecond},
		{name: "new_persisted_heartbeat", advance: time.Microsecond},
		{name: "new_persisted_registration", register: true, advance: time.Microsecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			memory := newPostgresPrecisionOfflineMemory(t, old, old)
			expected := memory.runtime.runtimeSnapshot()
			newActivity := old.Add(test.advance)
			memory.beforeChange = func() {
				if test.register {
					memory.persistent.RegisteredAt.Time = postgresOfflineTimestamp(t, newActivity)
				} else {
					memory.persistent.KeepaliveAt.Time = postgresOfflineTimestamp(t, newActivity)
				}
				if test.runtime {
					memory.runtime.UpdateRuntime(func(device *Device) {
						if test.register {
							device.LastRegisterAt = newActivity
						} else {
							device.LastKeepaliveAt = newActivity
						}
					})
				}
			}
			api := &GB28181API{}
			server := &Server{gb: api, memoryStorer: memory}
			api.svr = server

			changed, err := server.setDeviceOfflineIfCurrent(gb10DeviceID, expected, false)
			if err != nil || changed || !memory.persistent.IsOnline || !memory.runtime.runtimeSnapshot().IsOnline {
				t.Fatalf("stale scan changed fresh activity: changed=%t, err=%v, persistent=%t, runtime=%t", changed, err, memory.persistent.IsOnline, memory.runtime.runtimeSnapshot().IsOnline)
			}
		})
	}
}

type postgresPrecisionOfflineMemory struct {
	*flowMemory
	beforeChange func()
}

func (m *postgresPrecisionOfflineMemory) Change(deviceID string, persistent func(*ipc.Device) error, runtime func(*Device)) error {
	if m.beforeChange != nil {
		beforeChange := m.beforeChange
		m.beforeChange = nil
		beforeChange()
	}
	return m.flowMemory.Change(deviceID, persistent, runtime)
}

func newPostgresPrecisionOfflineMemory(t *testing.T, registeredAt, keepaliveAt time.Time) *postgresPrecisionOfflineMemory {
	t.Helper()
	return &postgresPrecisionOfflineMemory{flowMemory: &flowMemory{
		persistent: &ipc.Device{
			DeviceID: gb10DeviceID, IsOnline: true, Expires: 3600,
			RegisteredAt: orm.Time{Time: postgresOfflineTimestamp(t, registeredAt)},
			KeepaliveAt:  orm.Time{Time: postgresOfflineTimestamp(t, keepaliveAt)},
		},
		runtime: &Device{
			IsOnline: true, Expires: 3600, LastRegisterAt: registeredAt, LastKeepaliveAt: keepaliveAt,
			conn: newFlowConnection(), source: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
		},
	}}
}

func postgresOfflineTimestamp(t *testing.T, observedAt time.Time) time.Time {
	t.Helper()
	// 使用项目实际使用的 pgx 编解码，复现 PostgreSQL 往返后的微秒精度。
	codec := &pgtype.TimestamptzCodec{}
	types := pgtype.NewMap()
	value := pgtype.Timestamptz{Time: observedAt, Valid: true}
	encoded, err := codec.PlanEncode(types, pgtype.TimestamptzOID, pgtype.BinaryFormatCode, value).Encode(value, nil)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := codec.DecodeValue(types, pgtype.TimestamptzOID, pgtype.BinaryFormatCode, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return decoded.(time.Time)
}
