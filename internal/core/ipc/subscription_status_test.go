package ipc_test

import (
	"context"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/domain/uniqueid/store/uniqueiddb"
)

type subscriptionStatusProtocol struct {
	ipc.Protocoler
	states       []ipc.SubscriptionState
	seenDeviceID string
}

func (p *subscriptionStatusProtocol) SubscriptionStates(_ context.Context, device *ipc.Device) ([]ipc.SubscriptionState, error) {
	p.seenDeviceID = device.DeviceID
	return append([]ipc.SubscriptionState(nil), p.states...), nil
}

func TestCoreSubscriptionStatesResolveDeviceAndProtocol(t *testing.T) {
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_subscription_status", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	protocol := &subscriptionStatusProtocol{states: []ipc.SubscriptionState{{
		DeviceID: device.DeviceID, TargetID: device.DeviceID, Event: "alarm", Status: "active",
		ExpiresAt: time.Now().Add(time.Hour),
	}}}
	uni := uniqueid.NewCore(uniqueiddb.NewDB(db).AutoMigrate(true), 5)
	core := ipc.NewCore(store, uni, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})

	states, err := core.SubscriptionStates(t.Context(), device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if protocol.seenDeviceID != device.DeviceID || len(states) != 1 || states[0].Event != "alarm" {
		t.Fatalf("subscription states = %+v, seen device = %q", states, protocol.seenDeviceID)
	}
	protocol.states[0].Event = "mutated"
	if states[0].Event != "alarm" {
		t.Fatalf("core result aliases protocol state: %+v", states)
	}
}
