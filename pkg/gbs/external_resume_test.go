package gbs

import (
	"context"
	"errors"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
	"net"
	"testing"
	"time"
)

type resumeTestConnection struct {
	*flowConnection
	commitErr error
	commits   int
}

func (c *resumeTestConnection) RoutingScope() string     { return "gateway:verified:flow" }
func (c *resumeTestConnection) AdvertisedAddr() net.Addr { return c.LocalAddr() }
func (c *resumeTestConnection) OpenSignalingTransport(context.Context, string, net.Addr) (sip.Connection, error) {
	return nil, errors.New("unused")
}
func (c *resumeTestConnection) RegistrationCommitted(context.Context, string, int) error {
	c.commits++
	return c.commitErr
}

func TestResumeExternalDeviceClearsOfflineAndSignalsSubscriptionsAfterCommit(t *testing.T) {
	for _, failure := range []string{"success", "storage", "binding"} {
		t.Run(failure, func(t *testing.T) {
			api, memory, base := newRegisterHandlerTestAPI(t, true)
			api.svr.gb = api
			if err := api.core.Store().Device().Update(t.Context(), &ipc.Device{}, func(d *ipc.Device) error { d.Address = "192.0.2.10:5060"; return nil }, orm.Where("device_id = ?", gb10DeviceID)); err != nil {
				t.Fatal(err)
			}
			// 本例只检查续接提交边界，目录网络查询由人工设备联调。
			api.lifecycleClosed = true
			api.manualSubscriptionRecoveryWake = make(chan string, 1)
			api.deviceOfflineTombstones.Store(gb10DeviceID, struct{}{})
			api.deviceDeletionTombstones.Store(gb10DeviceID, struct{}{})
			c := &resumeTestConnection{flowConnection: base}
			if failure == "storage" {
				memory.changeErr = errors.New("db unavailable")
			}
			if failure == "binding" {
				c.commitErr = errors.New("B binding not acknowledged")
			}
			err := api.svr.ResumeExternalDevice(t.Context(), gb10DeviceID, c, time.Now().Add(time.Hour), time.Now())
			_, offline := api.deviceOfflineTombstones.Load(gb10DeviceID)
			if failure != "success" {
				if err == nil || !offline || len(api.manualSubscriptionRecoveryWake) != 0 {
					t.Fatal("failed resume published recovery", err, offline)
				}
				return
			}
			if err != nil || offline || c.commits != 1 || len(api.manualSubscriptionRecoveryWake) != 1 {
				t.Fatal(err, offline, c.commits)
			}
			if memory.persistent.Expires < 3500 || !memory.persistent.IsOnline || memory.persistent.Ext.GBRegistrationClosed == nil || *memory.persistent.Ext.GBRegistrationClosed {
				t.Fatal("registration did not resume", memory.persistent)
			}
			if _, deleted := api.deviceDeletionTombstones.Load(gb10DeviceID); deleted {
				t.Fatal("deletion marker retained")
			}
		})
	}
}
