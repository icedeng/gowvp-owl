package gbs

import (
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestKeepaliveHistoryStaysInsideDeviceDeleteLock(t *testing.T) {
	db := newHistoryDeleteRaceTestDB(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	historyEntered := make(chan struct{})
	releaseHistory := make(chan struct{})
	var historyOnce sync.Once
	if err := db.Callback().Create().Before("gorm:create").Register("test:block_device_history", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "device_history_records" {
			return
		}
		historyOnce.Do(func() { close(historyEntered) })
		<-releaseHistory
	}); err != nil {
		t.Fatal(err)
	}

	registeredAt := time.Now()
	memory := newFlowMemory(gb10DeviceID)
	memory.persistent.IsOnline = true
	memory.persistent.RegisteredAt = orm.Time{Time: registeredAt}
	memory.persistent.Expires = 3600
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = registeredAt
		device.Expires = 3600
		device.conn = newFlowConnection()
		device.source = &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060}
	})
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{})}
	api.svr = &Server{gb: api, memoryStorer: memory}
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>Keepalive</CmdType><SN>105</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Status>OK</Status></Notify>`)
	ctx := &sip.Context{
		Request:  newFlowRequest(t, connection, sip.MethodMessage, "keepalive-history-delete-race", body),
		Tx:       sip.NewTransaction("keepalive-history-delete-race-tx", connection),
		DeviceID: gb10DeviceID,
		Source:   connection.remote,
		To:       mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Log:      slog.Default(),
	}

	handlerDone := make(chan struct{})
	go func() {
		api.sipMessageKeepalive(ctx)
		close(handlerDone)
	}()
	select {
	case <-historyEntered:
	case <-time.After(time.Second):
		t.Fatal("Keepalive history write did not start")
	}
	assertHistoryHoldsDeviceDeleteLock(t, api, releaseHistory, handlerDone, nil)
}

func TestRegisterHistoryStaysInsideDeviceDeleteLock(t *testing.T) {
	db := newHistoryDeleteRaceTestDB(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_register_history", DeviceID: gb10DeviceID, Type: ipc.TypeGB28181}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	historyEntered := make(chan struct{})
	releaseHistory := make(chan struct{})
	var historyOnce sync.Once
	if err := db.Callback().Create().Before("gorm:create").Register("test:block_device_history", func(tx *gorm.DB) {
		if tx.Statement == nil || tx.Statement.Schema == nil || tx.Statement.Schema.Table != "device_history_records" {
			return
		}
		historyOnce.Do(func() { close(historyEntered) })
		<-releaseHistory
	}); err != nil {
		t.Fatal(err)
	}

	connection := newFlowConnection()
	memory := &registerHandlerTestMemory{flowMemory: newFlowMemory(gb10DeviceID)}
	api := &GB28181API{
		cfg:              &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		core:             ipc.NewAdapter(store, uniqueid.Core{}),
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:  newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
		lifecycleDone:    make(chan struct{}),
	}
	api.svr = &Server{gb: api, memoryStorer: memory}
	ctx := newRegisterHandlerTestContext(t, connection, "register-history-delete-race", 3600)
	handlerDone := make(chan struct{})
	go func() {
		api.handlerRegister(ctx)
		close(handlerDone)
	}()
	select {
	case <-historyEntered:
	case <-time.After(time.Second):
		t.Fatal("REGISTER history write did not start")
	}
	assertHistoryHoldsDeviceDeleteLock(t, api, releaseHistory, handlerDone, api.beginClose)
}

func newHistoryDeleteRaceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s-%s?mode=memory&cache=shared", t.Name(), sip.RandString(12))
	db, err := gorm.Open(sqlite.Open(dsn))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close history delete race test database: %v", err)
		}
	})
	return db
}

func assertHistoryHoldsDeviceDeleteLock(
	t *testing.T,
	api *GB28181API,
	releaseHistory chan struct{},
	handlerDone <-chan struct{},
	beforeRelease func(),
) {
	t.Helper()
	deleteLock := make(chan func(), 1)
	go func() { deleteLock <- api.lockRegisterOperation(gb10DeviceID) }()
	acquiredDuringHistory := false
	select {
	case unlock := <-deleteLock:
		acquiredDuringHistory = true
		unlock()
	case <-time.After(50 * time.Millisecond):
	}
	if beforeRelease != nil {
		beforeRelease()
	}
	close(releaseHistory)
	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("Keepalive handler did not finish after history write resumed")
	}
	if !acquiredDuringHistory {
		select {
		case unlock := <-deleteLock:
			unlock()
		case <-time.After(time.Second):
			t.Fatal("device delete lock remained blocked after Keepalive completed")
		}
	}
	if acquiredDuringHistory {
		t.Fatal("device delete lock was released before Keepalive history persistence completed")
	}
}
