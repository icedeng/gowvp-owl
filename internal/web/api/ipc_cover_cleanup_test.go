package api

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

func TestRemoveCoversDeletesOnlyRequestedChannels(t *testing.T) {
	dataDir := t.TempDir()
	for _, channelID := range []string{"channel-a", "channel-b", "channel-keep"} {
		if err := writeCover(dataDir, channelID, []byte(channelID)); err != nil {
			t.Fatalf("write cover %s: %v", channelID, err)
		}
	}

	if err := removeCovers(dataDir, []string{"channel-a", "channel-b", "missing", ""}); err != nil {
		t.Fatalf("remove covers: %v", err)
	}
	for _, channelID := range []string{"channel-a", "channel-b"} {
		if _, err := os.Stat(readCoverPath(dataDir, channelID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cover %s still exists or stat failed: %v", channelID, err)
		}
	}
	if body, err := os.ReadFile(readCoverPath(dataDir, "channel-keep")); err != nil || string(body) != "channel-keep" {
		t.Fatalf("unrelated cover changed: body=%q err=%v", body, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, coverDir)); err != nil {
		t.Fatalf("cover directory should remain available: %v", err)
	}
}

func TestSnapshotCoordinatorCancelTasksStopsDetachedTask(t *testing.T) {
	coordinator := newSnapshotCoordinator(1)
	parent, cancelParent := context.WithCancel(context.Background())
	generation := coordinator.taskGeneration("channel-a")
	taskCtx, finish := coordinator.beginTask(parent, "channel-a", generation)
	defer finish()
	cancelParent()

	select {
	case <-taskCtx.Done():
		t.Fatal("snapshot task must outlive the requesting client until explicit cleanup")
	case <-time.After(20 * time.Millisecond):
	}

	coordinator.cancelTasks([]string{"channel-a"})
	select {
	case <-taskCtx.Done():
		if !errors.Is(taskCtx.Err(), context.Canceled) {
			t.Fatalf("unexpected task error: %v", taskCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("device cleanup did not cancel detached snapshot task")
	}
}

func TestSnapshotCoordinatorRejectsTaskQueuedBeforeDelete(t *testing.T) {
	coordinator := newSnapshotCoordinator(1)
	generation := coordinator.taskGeneration("channel-a")
	coordinator.cancelTasks([]string{"channel-a"})

	taskCtx, finish := coordinator.beginTask(context.Background(), "channel-a", generation)
	defer finish()
	select {
	case <-taskCtx.Done():
		if !errors.Is(taskCtx.Err(), context.Canceled) {
			t.Fatalf("unexpected task error: %v", taskCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("task admitted with a stale pre-delete generation")
	}
}

func TestSnapshotCoordinatorBlocksNewTasksUntilDeleteRollback(t *testing.T) {
	coordinator := newSnapshotCoordinator(1)
	coordinator.cancelTasks([]string{"channel-a"})

	generation := coordinator.taskGeneration("channel-a")
	taskCtx, finish := coordinator.beginTask(context.Background(), "channel-a", generation)
	finish()
	if !errors.Is(taskCtx.Err(), context.Canceled) {
		t.Fatalf("new task was not blocked during delete: %v", taskCtx.Err())
	}

	coordinator.allowTasks([]string{"channel-a"})
	generation = coordinator.taskGeneration("channel-a")
	taskCtx, finish = coordinator.beginTask(context.Background(), "channel-a", generation)
	defer finish()
	select {
	case <-taskCtx.Done():
		t.Fatalf("task remained blocked after delete rollback: %v", taskCtx.Err())
	case <-time.After(20 * time.Millisecond):
	}
}

func TestDeleteDeviceCancelsSnapshotTasksAndRemovesChannelCovers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_cover_cleanup", DeviceID: "34020000001320000001", Type: ipc.TypeGB28181}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	channels := []*ipc.Channel{
		{ID: "GBC_cover_a", DID: device.ID, DeviceID: device.DeviceID, ChannelID: "34020000001320000002", Type: ipc.TypeGB28181},
		{ID: "GBC_cover_b", DID: device.ID, DeviceID: device.DeviceID, ChannelID: "34020000001320000003", Type: ipc.TypeGB28181},
	}
	for _, channel := range channels {
		if err := store.Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
	}

	dataDir := t.TempDir()
	for _, channel := range channels {
		if err := writeCover(dataDir, channel.ID, []byte(channel.ID)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCover(dataDir, "unrelated", []byte("keep")); err != nil {
		t.Fatal(err)
	}

	coordinator := newSnapshotCoordinator(1)
	generation := coordinator.taskGeneration(channels[0].ID)
	taskCtx, finish := coordinator.beginTask(context.Background(), channels[0].ID, generation)
	defer finish()
	api := IPCAPI{
		ipc:      ipc.NewCore(store, uniqueid.Core{}, nil),
		uc:       &Usecase{Conf: &conf.Bootstrap{ConfigDir: dataDir}},
		snapshot: coordinator,
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("DELETE", "/devices/"+device.ID, nil)
	ctx.Params = gin.Params{{Key: "id", Value: device.ID}}

	if _, err := api.delDevice(ctx, nil); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	select {
	case <-taskCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("device deletion did not cancel active snapshot task")
	}
	for _, channel := range channels {
		if _, err := os.Stat(readCoverPath(dataDir, channel.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cover %s still exists: %v", channel.ID, err)
		}
	}
	if _, err := os.Stat(readCoverPath(dataDir, "unrelated")); err != nil {
		t.Fatalf("unrelated cover removed: %v", err)
	}
}

func TestDeleteDeviceCleansChannelCreatedBeforeDeleteLock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{ID: "GB_cover_cleanup_race", DeviceID: "34020000001320000011", Type: ipc.TypeGB28181}
	existing := &ipc.Channel{
		ID: "GBC_cover_existing", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000012", Type: ipc.TypeGB28181,
	}
	late := &ipc.Channel{
		ID: "GBC_cover_late", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: "34020000001320000013", Type: ipc.TypeGB28181,
	}
	if err := store.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := store.Channel().Create(t.Context(), existing); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	for _, channel := range []*ipc.Channel{existing, late} {
		if err := writeCover(dataDir, channel.ID, []byte(channel.ID)); err != nil {
			t.Fatal(err)
		}
	}
	protocol := &lateDeleteChannelProtocol{store: store, channel: late}
	coordinator := newSnapshotCoordinator(1)
	generation := coordinator.taskGeneration(late.ID)
	taskCtx, finish := coordinator.beginTask(context.Background(), late.ID, generation)
	defer finish()
	api := IPCAPI{
		ipc: ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{
			ipc.TypeGB28181: protocol,
		}),
		uc:       &Usecase{Conf: &conf.Bootstrap{ConfigDir: dataDir}},
		snapshot: coordinator,
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("DELETE", "/devices/"+device.ID, nil)
	ctx.Params = gin.Params{{Key: "id", Value: device.ID}}

	if _, err := api.delDevice(ctx, nil); err != nil {
		t.Fatalf("delete device: %v", err)
	}
	select {
	case <-taskCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("late channel snapshot task was not cancelled")
	}
	for _, channel := range []*ipc.Channel{existing, late} {
		if _, err := os.Stat(readCoverPath(dataDir, channel.ID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cover %s still exists: %v", channel.ID, err)
		}
	}
}

type lateDeleteChannelProtocol struct {
	store   ipc.Storer
	channel *ipc.Channel
}

func (p *lateDeleteChannelProtocol) LockDeviceDelete(*ipc.Device) func() {
	if err := p.store.Channel().Create(context.Background(), p.channel); err != nil {
		panic(err)
	}
	return func() {}
}

func (*lateDeleteChannelProtocol) ValidateDevice(context.Context, *ipc.Device) error { return nil }
func (*lateDeleteChannelProtocol) InitDevice(context.Context, *ipc.Device) error     { return nil }
func (*lateDeleteChannelProtocol) QueryCatalog(context.Context, *ipc.Device) error   { return nil }
func (*lateDeleteChannelProtocol) StartPlay(context.Context, *ipc.Device, *ipc.Channel) (*ipc.PlayResponse, error) {
	return nil, nil
}
func (*lateDeleteChannelProtocol) StopPlay(context.Context, *ipc.Device, *ipc.Channel) error {
	return nil
}
func (*lateDeleteChannelProtocol) DeleteDevice(context.Context, *ipc.Device) error { return nil }
func (*lateDeleteChannelProtocol) OnStreamNotFound(context.Context, string, string) error {
	return nil
}
func (*lateDeleteChannelProtocol) OnStreamChanged(context.Context, string, string) error { return nil }
