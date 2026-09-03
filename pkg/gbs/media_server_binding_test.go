package gbs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestPlayResolvesMediaServerAfterAcquiringChannelLock(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	if err := channel.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	api := &GB28181API{svr: &Server{memoryStorer: memory}, streams: &conc.Map[string, *Streams]{}}

	unlock, err := memory.runtime.lockMediaContext(context.Background(), gb10ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	resolverCalled := make(chan struct{}, 1)
	wantErr := errors.New("resolved after lock")
	playDone := make(chan error, 1)
	go func() {
		playDone <- api.PlayContext(context.Background(), &PlayInput{
			Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, ID: "stream-media-binding"},
			ResolveMediaServer: func(context.Context) (*sms.MediaServer, error) {
				resolverCalled <- struct{}{}
				return nil, wantErr
			},
		})
	}()

	select {
	case <-resolverCalled:
		unlock()
		t.Fatal("media server was resolved before the channel lock was acquired")
	case <-time.After(20 * time.Millisecond):
	}
	unlock()

	select {
	case err := <-playDone:
		if !errors.Is(err, wantErr) {
			t.Fatalf("play error = %v, want %v", err, wantErr)
		}
	case <-time.After(time.Second):
		t.Fatal("play did not resolve the media server after the channel lock was released")
	}
}

func TestPlayRejectsInvalidStreamModeBeforeResolvingMediaServer(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	if err := channel.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	api := &GB28181API{svr: &Server{memoryStorer: memory}, streams: &conc.Map[string, *Streams]{}}

	resolverCalls := 0
	err := api.PlayContext(t.Context(), &PlayInput{
		Channel:    &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, ID: "invalid-stream-mode"},
		StreamMode: 3,
		ResolveMediaServer: func(context.Context) (*sms.MediaServer, error) {
			resolverCalls++
			return nil, errors.New("must not resolve media server")
		},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid RTP stream mode") {
		t.Fatalf("PlayContext error = %v", err)
	}
	if resolverCalls != 0 {
		t.Fatalf("media server resolver calls = %d, want 0", resolverCalls)
	}
	if _, ok := api.streams.Load(resolvePlaySessionKey(gb10DeviceID, gb10ChannelID, "")); ok {
		t.Fatal("invalid stream mode left a play session behind")
	}
}
