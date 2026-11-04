package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

type ptzProtocolStub struct {
	ipc.Protocoler
	control func(*ipc.PTZControlInput) error
}

func (p *ptzProtocolStub) PTZControl(_ context.Context, _ *ipc.Device, _ *ipc.Channel, in *ipc.PTZControlInput) error {
	return p.control(in)
}

func TestPTZProbeSeparatesSignalingFromMotion(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			store := ipcdb.NewDB(db).AutoMigrate(true)
			dev := &ipc.Device{ID: "ptz-device", Type: ipc.TypeGB28181}
			ch := &ipc.Channel{ID: "ptz-channel", DID: dev.ID, Type: ipc.TypeGB28181}
			if err := store.Device().Create(t.Context(), dev); err != nil {
				t.Fatal(err)
			}
			if err := store.Channel().Create(t.Context(), ch); err != nil {
				t.Fatal(err)
			}
			calls := 0
			protocol := &ptzProtocolStub{control: func(in *ipc.PTZControlInput) error {
				calls++
				if in.Action != "stop" {
					t.Fatalf("probe moved camera: %+v", in)
				}
				if !accepted {
					return errors.New("device rejected command")
				}
				return nil
			}}
			api := IPCAPI{ipc: ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeGB28181: protocol})}
			out, err := api.probeChannelPTZ(t.Context(), ch.ID, nil)
			if err != nil {
				t.Fatal(err)
			}
			if out.SignalingOK != accepted || out.PTZCapable || out.PTZVerified || out.VerifiedNow {
				t.Fatalf("probe falsely verified hardware: %+v", out)
			}
			if !accepted && !strings.Contains(out.Message, "rejected") {
				t.Fatalf("lost device error: %+v", out)
			}
			stored, err := api.ipc.GetChannel(t.Context(), ch.ID)
			if err != nil || stored.Ext.PTZVerified {
				t.Fatalf("probe persisted motion verification: %+v, %v", stored, err)
			}
			if _, err := api.probeChannelPTZ(t.Context(), ch.ID, &ptzProbeInput{Action: "left"}); err == nil || calls != 1 {
				t.Fatalf("probe allowed moving action: calls=%d, err=%v", calls, err)
			}
		})
	}
}

func TestPTZStatusIgnoresLegacyStopVerification(t *testing.T) {
	ch := &ipc.Channel{Type: ipc.TypeGB28181, Ext: ipc.DeviceExt{PTZVerified: true}}
	dev := &ipc.Device{Type: ipc.TypeGB28181, Ext: ipc.DeviceExt{PTZVerified: true}, Children: []*ipc.Channel{ch}}
	fillDeviceRecordMode([]*ipc.Device{dev})
	if dev.PTZCapable || dev.PTZVerified || dev.Ext.PTZVerified || ch.PTZCapable || ch.PTZVerified || ch.Ext.PTZVerified {
		t.Fatal("legacy stop response still claims motion capability")
	}
	ch.PTZType = 3
	fillChannelPTZStatus(ch)
	if ch.PTZCapable {
		t.Fatal("fixed camera reported as PTZ capable")
	}
}
