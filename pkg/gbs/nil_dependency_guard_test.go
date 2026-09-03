package gbs

import (
	"context"
	"testing"
)

func TestCascadeHelpersRejectNilCoreWithoutPanic(t *testing.T) {
	api := &GB28181API{}
	if _, err := api.loadCascadeChannels(context.Background(), cascadePlatform{sharedChannels: []string{gb10ChannelID}}); err == nil {
		t.Fatal("loadCascadeChannels accepted a nil core")
	}
	if _, err := api.resolveCascadeChannelMediaServer(context.Background(), gb10ChannelID); err == nil {
		t.Fatal("resolveCascadeChannelMediaServer accepted a nil core")
	}
	if err := api.persistChannelActive(context.Background(), gb10DeviceID, gb10ChannelID); err != nil {
		t.Fatalf("persistChannelActive with nil core = %v", err)
	}
	if err := api.persistChannelIdleIfNoActive(context.Background(), gb10DeviceID, gb10ChannelID); err != nil {
		t.Fatalf("persistChannelIdleIfNoActive with nil core = %v", err)
	}
}
