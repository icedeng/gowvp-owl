package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/protos"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

func TestAISyncScansBeyond999AndRestoresAfterPythonRestart(t *testing.T) {
	last := &ipc.Channel{ID: "ch_1000", IsOnline: true, Type: ipc.TypeGB28181, Ext: ipc.DeviceExt{EnabledAI: true}}
	api := newChannelMediaServerAPI(t, last)
	ai := configureTestAI(&api)
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s_scan?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	store := ipcdb.NewDB(db).AutoMigrate(true)
	channels := make([]*ipc.Channel, 1001)
	for i := range channels {
		channels[i] = &ipc.Channel{ID: fmt.Sprintf("ch_%04d", i), IsOnline: true, Type: ipc.TypeGB28181}
	}
	channels[1000] = last
	if err := db.CreateInBatches(channels, 100).Error; err != nil {
		t.Fatal(err)
	}
	ai.ipcCore = ipc.NewCore(store, uniqueid.Core{}, nil)
	starts := 0
	ai.startCameraFn = func(_ context.Context, req *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
		if req.CameraId != last.ID {
			t.Fatalf("unexpected start: %s", req.CameraId)
		}
		starts++
		return &protos.StartCameraResponse{Success: true}, nil
	}
	ai.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
		t.Fatal("running task beyond first page was stopped")
		return nil, nil
	}
	actual := &protos.StatusResponse{IsReady: true}
	ai.statusFn = func(context.Context, *protos.StatusRequest) (*protos.StatusResponse, error) { return actual, nil }
	ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
	actual.Cameras = []*protos.CameraStatus{{CameraId: last.ID, Status: "running"}}
	ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
	// Python 重启丢失任务，即使 Go 没重启，也必须再次启动。
	actual.Cameras = nil
	ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
	if starts != 2 {
		t.Fatalf("starts = %d, want 2", starts)
	}
}

func TestAISyncDoesNothingWithIncompleteSnapshots(t *testing.T) {
	for _, failure := range []string{"second_page", "rpc", "nil", "not_ready"} {
		t.Run(failure, func(t *testing.T) {
			api := newChannelMediaServerAPI(t, &ipc.Channel{ID: "disabled"})
			ai := configureTestAI(&api)
			ai.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
				t.Fatal("start with incomplete snapshot")
				return nil, nil
			}
			ai.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
				t.Fatal("stop with incomplete snapshot")
				return nil, nil
			}
			ai.statusFn = func(context.Context, *protos.StatusRequest) (*protos.StatusResponse, error) {
				switch failure {
				case "rpc":
					return nil, errors.New("status unavailable")
				case "nil":
					return nil, nil
				case "not_ready":
					return &protos.StatusResponse{}, nil
				default:
					t.Fatal("status fetched after failed pagination")
					return nil, nil
				}
			}
			if failure == "second_page" {
				ai.listChannelsFn = func(_ context.Context, after string, size int) ([]*ipc.Channel, error) {
					if after != "" {
						return nil, errors.New("page failed")
					}
					page := make([]*ipc.Channel, size)
					for i := range page {
						page[i] = &ipc.Channel{ID: fmt.Sprintf("ch_%04d", i)}
					}
					return page, nil
				}
			}
			ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
		})
	}
}

func TestAISyncRecoversTerminalAndStopsUndesiredTasks(t *testing.T) {
	for _, state := range []string{"error", "stopped", "stopping", "offline", "disabled", "deleted"} {
		t.Run(state, func(t *testing.T) {
			channel := &ipc.Channel{ID: "ch_sync", Type: ipc.TypeGB28181, IsOnline: state != "offline", Ext: ipc.DeviceExt{EnabledAI: state != "disabled"}}
			api := newChannelMediaServerAPI(t, channel)
			ai := configureTestAI(&api)
			id := channel.ID
			if state == "deleted" {
				id = "missing"
			}
			ai.statusFn = func(context.Context, *protos.StatusRequest) (*protos.StatusResponse, error) {
				return &protos.StatusResponse{IsReady: true, Cameras: []*protos.CameraStatus{{CameraId: id, Status: state}}}, nil
			}
			starts, stops := 0, 0
			ai.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
				starts++
				return &protos.StartCameraResponse{Success: true}, nil
			}
			ai.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
				stops++
				return &protos.StopCameraResponse{Success: true}, nil
			}
			ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
			if state == "offline" || state == "disabled" || state == "deleted" {
				if stops != 1 {
					t.Fatalf("stops = %d", stops)
				}
			} else if starts != 1 || stops != 0 {
				t.Fatalf("terminal recovery: starts=%d stops=%d", starts, stops)
			}
		})
	}
}

func TestAIStopRejectsUnconfirmedResponse(t *testing.T) {
	api := newChannelMediaServerAPI(t, &ipc.Channel{ID: "ch_stop"})
	ai := configureTestAI(&api)
	for _, resp := range []*protos.StopCameraResponse{nil, {Success: false, Message: "still stopping"}} {
		ai.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) { return resp, nil }
		if err := ai.StopAIDetection(t.Context(), "ch_stop"); err == nil {
			t.Fatal("accepted unconfirmed stop")
		}
	}
}

func TestAIReloadDoesNotUndoConcurrentDisable(t *testing.T) {
	channel := &ipc.Channel{ID: "ch_reload", Ext: ipc.DeviceExt{EnabledAI: false}}
	api := newChannelMediaServerAPI(t, channel)
	ai := configureTestAI(&api)
	stale := *channel
	stale.Ext.EnabledAI = true
	if err := ai.ReloadAITask(t.Context(), &stale); err != nil {
		t.Fatal(err)
	}
}

func TestAISyncKeepsEnabledChannelWaitingUntilCapacityIsReleased(t *testing.T) {
	channel := &ipc.Channel{ID: "waiting", IsOnline: true, Type: ipc.TypeGB28181, Ext: ipc.DeviceExt{EnabledAI: true}}
	api := newChannelMediaServerAPI(t, channel)
	ai := configureTestAI(&api)
	ai.conf.Server.AI.MaxTasks = 1
	ai.statusFn = func(context.Context, *protos.StatusRequest) (*protos.StatusResponse, error) {
		return &protos.StatusResponse{IsReady: true, Cameras: []*protos.CameraStatus{{CameraId: "deleted", Status: "stopping"}}}, nil
	}
	stopConfirmed := false
	ai.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
		return &protos.StopCameraResponse{Success: stopConfirmed}, nil
	}
	starts := 0
	ai.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
		starts++
		return &protos.StartCameraResponse{Success: true}, nil
	}
	ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
	stored, err := api.ipc.GetChannel(t.Context(), channel.ID)
	if err != nil || !stored.Ext.EnabledAI || starts != 0 {
		t.Fatalf("capacity wait changed desired state or started task: %v starts=%d", err, starts)
	}
	stopConfirmed = true
	ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
	if starts != 1 {
		t.Fatalf("starts after capacity release = %d, want 1", starts)
	}
}

func TestAISyncRechecksDesiredStateAfterConcurrentDisable(t *testing.T) {
	channel := &ipc.Channel{ID: "ch_disable", IsOnline: true, Type: ipc.TypeGB28181, Ext: ipc.DeviceExt{EnabledAI: true}}
	api := newChannelMediaServerAPI(t, channel)
	ai := configureTestAI(&api)
	ai.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
		return &protos.StopCameraResponse{Success: true}, nil
	}
	ai.statusFn = func(context.Context, *protos.StatusRequest) (*protos.StatusResponse, error) {
		// 数据库分页快照已经完成，此时用户关闭 AI。
		if _, err := api.disableAI(aiChannelContext(t, channel.ID, "disable"), nil); err != nil {
			t.Fatal(err)
		}
		return &protos.StatusResponse{IsReady: true}, nil
	}
	ai.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
		t.Fatal("stale snapshot undid manual disable")
		return nil, nil
	}
	ai.syncAITasks(t.Context(), api.uc.SMSAPI.smsCore)
}
