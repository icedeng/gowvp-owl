package onvifadapter

import (
	"errors"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestONVIFDeviceNotificationsRollbackAndRetry(t *testing.T) {
	a, db, device, _ := testAdapter(t)
	if err := op.Migrate(db); err != nil {
		t.Fatal(err)
	}
	cache := ipccache.NewCache(ipcdb.NewDB(db.Session(&gorm.Session{SkipDefaultTransaction: true})))
	cache.SetDeviceStatusHandler(op.EnqueueDeviceStatusChange)
	a.adapter = ipc.NewAdapter(cache, uniqueid.Core{})
	a.sendHeartbeat(t.Context(), device.ID)
	runtime, _ := a.devices.Load(device.ID)
	runtime.KeepaliveAt = orm.Time{Time: time.Now().Add(-2 * time.Minute)}
	marker := errors.New("notification unavailable")
	cache.SetDeviceStatusHandler(func(*gorm.DB, bool, *ipc.Device, string) error { return marker })
	a.checkStatuses(t.Context(), time.Now(), heartbeatTimeout)
	assertOnline(t, db, device.ID, true)
	if !runtime.IsOnline {
		t.Fatal("通知失败后运行态不应确认离线")
	}
	cache.SetDeviceStatusHandler(op.EnqueueDeviceStatusChange)
	a.checkStatuses(t.Context(), time.Now(), heartbeatTimeout)
	a.checkStatuses(t.Context(), time.Now(), heartbeatTimeout)
	assertOnline(t, db, device.ID, false)
	a.sendHeartbeat(t.Context(), device.ID)
	a.sendHeartbeat(t.Context(), device.ID)
	assertOnline(t, db, device.ID, true)
	var boxes []op.Outbox
	if err := db.Order("id").Find(&boxes).Error; err != nil || len(boxes) != 2 || boxes[0].EventType != "device.offline" || boxes[1].EventType != "device.online" {
		t.Fatal("ONVIF 上下线通知丢失或重复", boxes, err)
	}
}
