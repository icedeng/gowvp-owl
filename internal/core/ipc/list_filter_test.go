package ipc_test

import (
	"context"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func newListFilterCore(t *testing.T) (ipc.Core, *gorm.DB) {
	t.Helper()
	db := openIPCTestDatabase(t)
	store := ipcdb.NewDB(db).AutoMigrate(true)
	return ipc.NewCore(store, uniqueid.Core{}, nil), db
}

func TestFindDeviceFiltersTypeAndOnlineState(t *testing.T) {
	core, db := newListFilterCore(t)
	devices := []ipc.Device{
		{ID: "gb-online", DeviceID: "34020000002000000001", Type: ipc.TypeGB28181, IsOnline: true},
		{ID: "gb-offline", DeviceID: "34020000002000000002", Type: ipc.TypeGB28181, IsOnline: false},
		{ID: "onvif-online", DeviceID: "onvif-device-1", Type: ipc.TypeOnvif, IsOnline: true},
	}
	if err := db.Create(&devices).Error; err != nil {
		t.Fatal(err)
	}

	items, total, err := core.FindDevice(context.Background(), &ipc.FindDeviceInput{
		PagerFilter: web.PagerFilter{Page: 1, Size: 10},
		Type:        ipc.TypeGB28181,
		IsOnline:    "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != "gb-online" {
		t.Fatalf("devices = %#v, total = %d; want gb-online only", items, total)
	}
}

func TestFindChannelFiltersPlayingState(t *testing.T) {
	core, db := newListFilterCore(t)
	channels := []ipc.Channel{
		{ID: "rtmp-playing", ChannelID: "rtmp-playing", Type: ipc.TypeRTMP, IsOnline: true, IsPlaying: true},
		{ID: "rtmp-idle", ChannelID: "rtmp-idle", Type: ipc.TypeRTMP, IsOnline: true, IsPlaying: false},
		{ID: "rtsp-playing", ChannelID: "rtsp-playing", Type: ipc.TypeRTSP, IsOnline: true, IsPlaying: true},
	}
	if err := db.Create(&channels).Error; err != nil {
		t.Fatal(err)
	}

	items, total, err := core.FindChannel(context.Background(), &ipc.FindChannelInput{
		PagerFilter: web.PagerFilter{Page: 1, Size: 10},
		Type:        ipc.TypeRTMP,
		IsPlaying:   "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != "rtmp-playing" {
		t.Fatalf("channels = %#v, total = %d; want rtmp-playing only", items, total)
	}
}
