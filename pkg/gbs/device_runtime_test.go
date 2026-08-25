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
