package owl

import (
	"context"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/migrations"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/event/store/eventdb"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"strings"
	"testing"
	"time"
)

func TestAlarmSameGatewayOwnershipAndRevocationAtCommit(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	defer sql.Close()
	if err = migrations.Apply(db); err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&ipc.Device{}, &ipc.Channel{}); err != nil {
		t.Fatal(err)
	}
	evcore := event.NewCore(eventdb.NewDB(db).AutoMigrate(true), nil)
	dev := ipc.Device{ID: "device", DeviceID: "34020000001320000001"}
	_ = db.Create(&dev).Error
	gateway := store.Gateway{Base: store.Base{ID: "gateway"}, StableID: "b1", Name: "B", AdminState: "enabled", DesiredConfig: core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig()}}
	if err = db.Create(&gateway).Error; err != nil {
		t.Fatal(err)
	}
	pk := gateway.ID
	binding := store.Binding{Base: store.Base{ID: "binding"}, BindingFields: store.BindingFields{SubjectID: dev.ID, IngressType: "gateway", GatewayID: &pk, BindingRevision: 3}}
	if err = db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}
	host := New(db, nil, nil)
	calls := 0
	revoke := false
	host.SetAlarmSink(func(ctx context.Context, a protocol.AlarmRecord, did, cid string) error {
		calls++
		if !a.Historical {
			t.Fatal("迟到报警仍可取证")
		}
		if revoke {
			if e := db.Model(&store.Binding{}).Where("id = ?", binding.ID).Update("gateway_id", "other").Error; e != nil {
				return e
			}
		}
		key := a.ID
		_, e := evcore.CreateAlarmAndNotify(ctx, &event.AddEventInput{DID: did, CID: cid, Model: "GB28181", Label: "报警", StartedAt: orm.Now(), EndedAt: orm.Now(), Historical: a.Historical, SourceKey: &key}, event.AlarmIdentity{DeviceCode: a.DeviceCode})
		return e
	})
	a := protocol.AlarmRecord{ID: strings.Repeat("a", 64), SubjectID: dev.ID, DeviceCode: dev.DeviceID, BindingRevision: "2", ReceivedAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), Description: "移动侦测"}
	if err = host.ReceiveAlarm(t.Context(), "other", a); err == nil || calls != 0 {
		t.Fatal("跨网关补报被接纳", err, calls)
	}
	if err = host.ReceiveAlarm(t.Context(), pk, a); err != nil {
		t.Fatal(err)
	}
	a.BindingRevision = "4"
	if err = host.ReceiveAlarm(t.Context(), pk, a); err == nil {
		t.Fatal("未来绑定版本被接纳")
	}
	a.BindingRevision = "2"
	a.ID = strings.Repeat("b", 64)
	revoke = true
	if err = host.ReceiveAlarm(t.Context(), pk, a); err == nil {
		t.Fatal("归属在提交前改变仍可入库")
	}
	var n int64
	db.Model(&event.Event{}).Count(&n)
	if n != 1 {
		t.Fatal("撤销后仍产生事件", n)
	}
}
