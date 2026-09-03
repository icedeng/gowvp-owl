package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/protos"
)

func aiChannelContext(t *testing.T, channelID, action string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/channels/"+channelID+"/ai/"+action, nil)
	ctx.Params = gin.Params{{Key: "id", Value: channelID}}
	return ctx
}

func configureTestAI(api *IPCAPI) *AIWebhookAPI {
	api.uc.Conf = &conf.Bootstrap{}
	aiAPI := NewAIWebhookAPI(api.uc.Conf, event.Core{}, api.ipc)
	api.uc.AIWebhookAPI = aiAPI
	return &api.uc.AIWebhookAPI
}

func TestEnableAIDoesNotPersistDesiredStateWhenMediaRouteFails(t *testing.T) {
	channel := &ipc.Channel{
		ID: "gb_ai_route_failure", Type: ipc.TypeGB28181,
		Config: ipc.StreamConfig{MediaServerID: "missing-edge"},
	}
	api := newChannelMediaServerAPI(t, channel)
	aiAPI := configureTestAI(&api)
	aiAPI.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
		t.Fatal("AI service called before media route validation")
		return nil, nil
	}

	if _, err := api.enableAI(aiChannelContext(t, channel.ID, "enable"), nil); err == nil {
		t.Fatal("enable AI accepted missing media route")
	}
	stored, err := api.ipc.GetChannel(t.Context(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Ext.EnabledAI {
		t.Fatal("failed AI enable persisted enabled_ai=true")
	}
}

func TestEnableAIPersistsOnlyAfterAIServiceStarts(t *testing.T) {
	channel := &ipc.Channel{ID: "gb_ai_commit_order", Type: ipc.TypeGB28181}
	api := newChannelMediaServerAPI(t, channel)
	aiAPI := configureTestAI(&api)
	aiAPI.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
		stored, err := api.ipc.GetChannel(t.Context(), channel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Ext.EnabledAI {
			t.Fatal("enabled_ai committed before AI service start succeeded")
		}
		return &protos.StartCameraResponse{Success: true}, nil
	}

	if _, err := api.enableAI(aiChannelContext(t, channel.ID, "enable"), nil); err != nil {
		t.Fatalf("enable AI: %v", err)
	}
	stored, err := api.ipc.GetChannel(t.Context(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Ext.EnabledAI {
		t.Fatal("successful AI enable did not persist enabled_ai=true")
	}
}

func TestStartAIDetectionRejectsUnsuccessfulResponse(t *testing.T) {
	channel := &ipc.Channel{ID: "gb_ai_rejected_start", Type: ipc.TypeGB28181}
	api := newChannelMediaServerAPI(t, channel)
	aiAPI := configureTestAI(&api)

	tests := []struct {
		name string
		resp *protos.StartCameraResponse
	}{
		{name: "empty response"},
		{name: "business rejection", resp: &protos.StartCameraResponse{Message: "model unavailable"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			aiAPI.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
				return tt.resp, nil
			}

			if _, err := aiAPI.StartAIDetection(t.Context(), channel, "rtsp://127.0.0.1/rtp/"+channel.ID); err == nil {
				t.Fatal("unsuccessful AI response accepted as a running task")
			}
			if _, ok := aiAPI.aiTasks.Load(channel.ID); ok {
				t.Fatal("unsuccessful AI response registered a running task")
			}
		})
	}
}

func TestAddZoneReloadsEnabledAITask(t *testing.T) {
	channel := &ipc.Channel{
		ID: "gb_ai_zone_reload", Type: ipc.TypeGB28181,
		Ext: ipc.DeviceExt{EnabledAI: true},
	}
	api := newChannelMediaServerAPI(t, channel)
	aiAPI := configureTestAI(&api)
	aiAPI.smsCore = api.uc.SMSAPI.smsCore
	aiAPI.smsCoreReady = true
	aiAPI.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
		return &protos.StopCameraResponse{Success: true}, nil
	}
	startCalls := 0
	aiAPI.startCameraFn = func(_ context.Context, request *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
		startCalls++
		if len(request.GetZones()) != 1 || request.GetZones()[0].GetName() != "entrance" {
			t.Fatalf("reloaded AI task with stale zones: %+v", request.GetZones())
		}
		return &protos.StartCameraResponse{Success: true}, nil
	}

	ctx := aiChannelContext(t, channel.ID, "zones")
	if _, err := api.addZone(ctx, &ipc.AddZoneInput{
		Name: "entrance", Coordinates: []float32{0, 0, 1, 0, 1, 1}, Labels: []string{"person"},
	}); err != nil {
		t.Fatalf("add zone: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("AI task restarted %d times after zone add, want 1", startCalls)
	}
}

func TestDisableAIRetainsRunningTaskForRetryWhenStopFails(t *testing.T) {
	channel := &ipc.Channel{ID: "gb_ai_stop_retry", Type: ipc.TypeGB28181, Ext: ipc.DeviceExt{EnabledAI: true}}
	api := newChannelMediaServerAPI(t, channel)
	aiAPI := configureTestAI(&api)
	aiAPI.aiTasks.Store(channel.ID, struct{}{})
	aiAPI.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
		return nil, errors.New("AI stop unavailable")
	}

	if _, err := api.disableAI(aiChannelContext(t, channel.ID, "disable"), nil); err == nil {
		t.Fatal("disable AI reported success when the running task was not stopped")
	}
	stored, err := api.ipc.GetChannel(t.Context(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Ext.EnabledAI {
		t.Fatal("failed stop did not preserve the desired disabled state")
	}
	if _, ok := aiAPI.aiTasks.Load(channel.ID); !ok {
		t.Fatal("failed stop removed running task ownership and prevented sync retry")
	}
}

func TestReloadAITaskDoesNotStartDuplicateWhenStopFails(t *testing.T) {
	channel := &ipc.Channel{ID: "gb_ai_reload_stop", Type: ipc.TypeGB28181}
	api := newChannelMediaServerAPI(t, channel)
	aiAPI := configureTestAI(&api)
	aiAPI.smsCore = api.uc.SMSAPI.smsCore
	aiAPI.smsCoreReady = true
	aiAPI.stopCameraFn = func(context.Context, *protos.StopCameraRequest) (*protos.StopCameraResponse, error) {
		return nil, errors.New("AI stop unavailable")
	}
	startCalls := 0
	aiAPI.startCameraFn = func(context.Context, *protos.StartCameraRequest) (*protos.StartCameraResponse, error) {
		startCalls++
		return &protos.StartCameraResponse{Success: true}, nil
	}

	if err := aiAPI.ReloadAITask(t.Context(), channel); err == nil {
		t.Fatal("reload AI ignored stop failure")
	}
	if startCalls != 0 {
		t.Fatalf("reload AI started %d duplicate task(s) after stop failure", startCalls)
	}
}
