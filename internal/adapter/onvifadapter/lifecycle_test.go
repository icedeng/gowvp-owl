package onvifadapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/onvif"
	devicemodel "github.com/gowvp/onvif/device"
	media "github.com/gowvp/onvif/media"
	xsdonvif "github.com/gowvp/onvif/xsd/onvif"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func testAdapter(t *testing.T) (*Adapter, *gorm.DB, *ipc.Device, ipc.Core) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "onvif_test", DeviceID: "onvif_test", Type: ipc.TypeOnvif, IP: "192.0.2.1", Port: 80, Username: "admin", Password: "old", IsOnline: true}
	if err := db.Create(device).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&ipc.Channel{ID: "ch_main", DID: device.ID, DeviceID: device.ID, ChannelID: "main", Type: ipc.TypeOnvif, IsOnline: true}).Error; err != nil {
		t.Fatal(err)
	}
	a := &Adapter{adapter: ipc.NewAdapter(store, uniqueid.Core{}), client: &http.Client{Timeout: time.Second}}
	a.connectFn = func(onvif.DeviceParams) (*onvif.Device, error) { return &onvif.Device{}, nil }
	a.infoFn = func(context.Context, *onvif.Device) (devicemodel.GetDeviceInformationResponse, error) {
		return devicemodel.GetDeviceInformationResponse{}, nil
	}
	a.profilesFn = func(context.Context, *onvif.Device) (media.GetProfilesResponse, error) {
		return media.GetProfilesResponse{Profiles: []xsdonvif.Profile{{Token: "main"}}}, nil
	}
	core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{ipc.TypeOnvif: a})
	return a, db, device, core
}

func assertOnline(t *testing.T, db *gorm.DB, id string, want bool) {
	t.Helper()
	var device ipc.Device
	var channel ipc.Channel
	if err := db.First(&device, "id=?", id).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&channel, "did=?", id).Error; err != nil {
		t.Fatal(err)
	}
	if device.IsOnline != want || channel.IsOnline != want {
		t.Fatalf("online device=%v channel=%v, want %v", device.IsOnline, channel.IsOnline, want)
	}
}

func TestONVIFStatusTransactionFailureRetriesWithoutNewTransition(t *testing.T) {
	a, db, device, _ := testAdapter(t)
	// 主键和协议 ID 不同，状态更新也必须命中正确设备。
	if err := db.Model(device).Update("device_id", "legacy-protocol-id").Error; err != nil {
		t.Fatal(err)
	}
	a.sendHeartbeat(t.Context(), device.ID)
	runtime, _ := a.devices.Load(device.ID)
	runtime.KeepaliveAt = orm.Time{Time: time.Now().Add(-2 * time.Minute)}
	if err := db.Exec("CREATE TRIGGER fail_status BEFORE UPDATE OF is_online ON channels BEGIN SELECT RAISE(ABORT, 'injected status failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	a.checkStatuses(t.Context(), time.Now(), heartbeatTimeout)
	assertOnline(t, db, device.ID, true)
	if !runtime.IsOnline {
		t.Fatal("failed transaction confirmed offline in memory")
	}
	if err := db.Exec("DROP TRIGGER fail_status").Error; err != nil {
		t.Fatal(err)
	}
	a.checkStatuses(t.Context(), time.Now(), heartbeatTimeout)
	assertOnline(t, db, device.ID, false)
	if runtime.IsOnline {
		t.Fatal("successful retry did not confirm offline")
	}
	a.sendHeartbeat(t.Context(), device.ID)
	assertOnline(t, db, device.ID, true)
}

func TestONVIFFailedInitializationAndProfileSaveRetry(t *testing.T) {
	a, db, device, _ := testAdapter(t)
	attempts := 0
	a.connectFn = func(onvif.DeviceParams) (*onvif.Device, error) {
		attempts++
		if attempts == 1 {
			return &onvif.Device{}, errors.New("unreachable")
		}
		return &onvif.Device{}, nil
	}
	a.heartbeatRound(t.Context())
	assertOnline(t, db, device.ID, false)
	runtime, _ := a.devices.Load(device.ID)
	if runtime.Device != nil {
		t.Fatal("cached partially initialized SDK client")
	}
	if err := db.Exec("CREATE TRIGGER fail_profile BEFORE UPDATE OF name ON channels BEGIN SELECT RAISE(ABORT, 'injected profile failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	a.heartbeatRound(t.Context())
	if !runtime.profilesPending {
		t.Fatal("failed profile save was acknowledged")
	}
	if err := db.Exec("DROP TRIGGER fail_profile").Error; err != nil {
		t.Fatal(err)
	}
	a.heartbeatRound(t.Context())
	assertOnline(t, db, device.ID, true)
	if runtime.profilesPending || attempts != 2 {
		t.Fatalf("pending=%v connect attempts=%d", runtime.profilesPending, attempts)
	}
}

func TestONVIFCredentialEditUsesCommittedConfiguration(t *testing.T) {
	a, db, device, core := testAdapter(t)
	a.sendHeartbeat(t.Context(), device.ID)
	old, _ := a.devices.Load(device.ID)
	password := "new"
	var used []string
	a.connectFn = func(params onvif.DeviceParams) (*onvif.Device, error) {
		used = append(used, params.Password)
		return &onvif.Device{}, nil
	}
	if _, err := core.EditDevice(t.Context(), &ipc.EditDeviceInput{DeviceID: device.DeviceID, IP: device.IP, Port: device.Port, Username: device.Username, Password: &password}, device.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := a.devices.Load(device.ID)
	if current == old || current.config.password != "new" {
		t.Fatal("credential edit retained old connection")
	}
	// 旧请求不能把内存恢复为旧密码。
	if err := a.InitDevice(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	current, _ = a.devices.Load(device.ID)
	if current.config.password != "new" {
		t.Fatal("stale init overwrote credentials")
	}
	for _, value := range used {
		if value != "new" {
			t.Fatal("connected with old password")
		}
	}
	if err := db.Exec("CREATE TRIGGER fail_edit BEFORE UPDATE OF password ON devices BEGIN SELECT RAISE(ABORT, 'injected edit failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	password = "uncommitted"
	if _, err := core.EditDevice(t.Context(), &ipc.EditDeviceInput{DeviceID: device.DeviceID, IP: device.IP, Port: device.Port, Username: device.Username, Password: &password}, device.ID); err == nil {
		t.Fatal("edit unexpectedly succeeded")
	}
	after, _ := a.devices.Load(device.ID)
	if after != current || after.config.password != "new" {
		t.Fatal("failed edit changed runtime")
	}
}

func TestONVIFDeleteSerializesWithHeartbeatAndRejectsLateInit(t *testing.T) {
	a, db, device, core := testAdapter(t)
	a.sendHeartbeat(t.Context(), device.ID)
	entered, release := make(chan struct{}), make(chan struct{})
	a.infoFn = func(context.Context, *onvif.Device) (devicemodel.GetDeviceInformationResponse, error) {
		close(entered)
		<-release
		return devicemodel.GetDeviceInformationResponse{}, nil
	}
	heartbeatDone := make(chan struct{})
	go func() { a.sendHeartbeat(t.Context(), device.ID); close(heartbeatDone) }()
	<-entered
	deleted := make(chan error, 1)
	go func() { _, err := core.DelDevice(t.Context(), device.ID); deleted <- err }()
	select {
	case err := <-deleted:
		t.Fatalf("delete bypassed heartbeat lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-heartbeatDone
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delete deadlocked")
	}
	if err := a.InitDevice(t.Context(), device); err == nil {
		t.Fatal("late init revived deleted device")
	}
	if _, ok := a.devices.Load(device.ID); ok {
		t.Fatal("deleted runtime retained")
	}
	var count int64
	if err := db.Model(&ipc.Channel{}).Where("did=?", device.ID).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("channels resurrected: count=%d err=%v", count, err)
	}
}

func TestONVIFFailedDeleteIsRecoveredByNextRound(t *testing.T) {
	a, db, device, core := testAdapter(t)
	a.sendHeartbeat(t.Context(), device.ID)
	if err := db.Exec("CREATE TRIGGER fail_delete BEFORE DELETE ON devices BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := core.DelDevice(t.Context(), device.ID); err == nil {
		t.Fatal("delete unexpectedly succeeded")
	}
	if _, ok := a.devices.Load(device.ID); ok {
		t.Fatal("cleanup was not executed")
	}
	a.heartbeatRound(t.Context())
	if _, ok := a.devices.Load(device.ID); !ok {
		t.Fatal("persisted device was not recovered")
	}
	assertOnline(t, db, device.ID, true)
}

func TestONVIFConcurrentHeartbeatAndStatusChecks(t *testing.T) {
	a, _, device, _ := testAdapter(t)
	var calls atomic.Int32
	a.infoFn = func(context.Context, *onvif.Device) (devicemodel.GetDeviceInformationResponse, error) {
		calls.Add(1)
		return devicemodel.GetDeviceInformationResponse{}, nil
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				a.sendHeartbeat(t.Context(), device.ID)
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for range 100 {
			a.checkStatuses(t.Context(), time.Now(), heartbeatTimeout)
		}
	}()
	workers.Wait()
	if calls.Load() != 80 {
		t.Fatalf("heartbeat calls=%d", calls.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestONVIFHostCancellationReachesSDKHTTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	transport := lifecycleTransport{ctx: ctx, base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	result := make(chan error, 1)
	go func() {
		_, err := onvif.NewDevice(onvif.DeviceParams{Xaddr: "example.invalid:80", HttpClient: &http.Client{Transport: transport}})
		result <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled SDK request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("SDK request did not cancel")
	}
}

func TestONVIFCommittedEditRetriesFailedConnectionInitialization(t *testing.T) {
	a, _, device, core := testAdapter(t)
	a.sendHeartbeat(t.Context(), device.ID)
	calls := 0
	a.connectFn = func(params onvif.DeviceParams) (*onvif.Device, error) {
		if params.Password != "new" {
			t.Fatal("attempt used stale credentials")
		}
		calls++
		if calls == 2 {
			return nil, errors.New("connection lost after validation")
		}
		return &onvif.Device{}, nil
	}
	password := "new"
	if _, err := core.EditDevice(t.Context(), &ipc.EditDeviceInput{DeviceID: device.DeviceID, IP: device.IP, Port: device.Port, Username: device.Username, Password: &password}, device.ID); err != nil {
		t.Fatal(err)
	}
	runtime, ok := a.devices.Load(device.ID)
	if !ok || runtime.Device != nil || runtime.config.password != "new" {
		t.Fatal("failed post-commit init retained old client")
	}
	a.heartbeatRound(t.Context())
	runtime, _ = a.devices.Load(device.ID)
	if runtime.Device == nil || runtime.profilesPending || calls != 3 {
		t.Fatalf("retry failed: calls=%d", calls)
	}
}
