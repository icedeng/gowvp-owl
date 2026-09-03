package gbs

import (
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestDeviceRuntimeStateConcurrentReadWrite(t *testing.T) {
	device := &Device{}
	uri, err := sip.ParseSipURI("sip:34020000001320000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	address := &sip.Address{URI: &uri, Params: sip.NewParams()}
	source := &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060}

	var group sync.WaitGroup
	for writer := 0; writer < 2; writer++ {
		writer := writer
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 1000; i++ {
				device.UpdateRuntime(func(runtime *Device) {
					runtime.IsOnline = i%2 == 0
					runtime.Password = fmt.Sprintf("password-%d-%d", writer, i)
					runtime.Address = source.String()
					runtime.source = source
					runtime.to = address
					runtime.LastKeepaliveAt = time.Unix(int64(i), 0)
				})
			}
		}()
	}
	for reader := 0; reader < 8; reader++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 1000; i++ {
				state := device.runtimeSnapshot()
				_ = state.IsOnline
				_ = device.PasswordValue()
				_ = device.Source()
				_ = device.To()
			}
		}()
	}
	group.Wait()
}

func TestDeviceIsOnlineNowRequiresActiveRegistrationBinding(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name         string
		online       bool
		registeredAt time.Time
		expires      int
		want         bool
	}{
		{name: "active", online: true, registeredAt: now, expires: 3600, want: true},
		{name: "offline", registeredAt: now, expires: 3600},
		{name: "expired", online: true, registeredAt: now.Add(-time.Minute), expires: 10},
		{name: "legacy_missing_binding_metadata", online: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := &Device{
				IsOnline:       test.online,
				LastRegisterAt: test.registeredAt,
				Expires:        test.expires,
			}
			if got := device.IsOnlineNow(); got != test.want {
				t.Fatalf("IsOnlineNow() = %t; want %t", got, test.want)
			}
		})
	}
}

func TestTransDeviceStatusAcceptsStandardOfflineValue(t *testing.T) {
	if got := transDeviceStatus("OFFLINE"); got != "OFF" {
		t.Fatalf("transDeviceStatus(OFFLINE) = %q; want OFF", got)
	}
	if got := transDeviceStatus("OFFILE"); got != "OFF" {
		t.Fatalf("legacy transDeviceStatus(OFFILE) = %q; want OFF", got)
	}
}

func TestNewDeviceRestoresRegistrationExpiry(t *testing.T) {
	now := orm.Now()
	device := NewDevice(nil, &ipc.Device{
		ID: "GB_runtime", DeviceID: gb10DeviceID, Address: "192.0.2.10:5060",
		RegisteredAt: now, KeepaliveAt: now, Expires: 3600,
	})
	if device == nil {
		t.Fatal("NewDevice returned nil")
	}
	if got := device.runtimeSnapshot().Expires; got != 3600 {
		t.Fatalf("restored Expires = %d, want 3600", got)
	}
}

func TestNewDeviceRestoresOfflineStatusWithoutClosingExplicitRegistration(t *testing.T) {
	registeredAt := time.Now()
	registrationClosed := false
	device := NewDevice(nil, &ipc.Device{
		ID:           "GB_runtime_offline_status",
		DeviceID:     gb10DeviceID,
		Address:      "192.0.2.10:5060",
		IsOnline:     false,
		RegisteredAt: orm.Time{Time: registeredAt},
		Expires:      3600,
		Ext: ipc.DeviceExt{
			GBRegistrationClosed: &registrationClosed,
		},
	})
	if device == nil {
		t.Fatal("NewDevice returned nil")
	}
	state := device.runtimeSnapshot()
	if state.IsOnline || state.RegistrationClosed || !runtimeRegistrationBindingActive(state, registeredAt.Add(time.Second)) {
		t.Fatalf("restored DeviceStatus offline binding = %+v", state)
	}

	legacy := NewDevice(nil, &ipc.Device{
		ID:           "GB_runtime_legacy_offline",
		DeviceID:     gb10DeviceID,
		Address:      "192.0.2.10:5060",
		IsOnline:     false,
		RegisteredAt: orm.Time{Time: registeredAt},
		Expires:      3600,
	})
	if legacy == nil || !legacy.runtimeSnapshot().RegistrationClosed {
		t.Fatalf("legacy offline record did not preserve closed-binding compatibility: %+v", legacy)
	}
}
