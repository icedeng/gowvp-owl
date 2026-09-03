package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

type capturedGBMediaEvent struct {
	mediaServerID string
	streamID      string
	active        bool
	reason        string
}

type capturingGBMediaLifecycle struct {
	events []capturedGBMediaEvent
}

type mediaAwareProtocol struct {
	lateDeleteChannelProtocol
	notFound []string
	changed  []string
}

func (p *mediaAwareProtocol) OnStreamNotFoundOnMediaServer(_ context.Context, mediaServerID, _, _ string) error {
	p.notFound = append(p.notFound, mediaServerID)
	return nil
}

func (p *mediaAwareProtocol) OnStreamChangedOnMediaServer(_ context.Context, mediaServerID, _, _ string) error {
	p.changed = append(p.changed, mediaServerID)
	return nil
}

func (c *capturingGBMediaLifecycle) OnMediaServerStreamChanged(_ context.Context, mediaServerID, streamID string, active bool, reason string) error {
	c.events = append(c.events, capturedGBMediaEvent{mediaServerID: mediaServerID, streamID: streamID, active: active, reason: reason})
	return nil
}

func TestOnStreamChangedForwardsUnpersistedCascadeStreamLifecycle(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	lifecycle := &capturingGBMediaLifecycle{}
	webhook := WebHookAPI{
		ipcCore:       ipc.NewCore(store, uniqueid.Core{}, nil),
		recordingCore: recording.Core{},
		log:           slog.Default(),
		gbs:           lifecycle,
	}

	for _, active := range []bool{true, false} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest("POST", "/webhook/on_stream_changed", nil)
		if _, err := webhook.onStreamChanged(ctx, &onStreamChangedInput{
			Regist:        active,
			App:           "rtp",
			Stream:        "cascade-0123456789abcdef",
			MediaServerID: "edge-zlm-1",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if len(lifecycle.events) != 2 {
		t.Fatalf("GB media lifecycle events = %d, want 2", len(lifecycle.events))
	}
	if event := lifecycle.events[0]; event.mediaServerID != "edge-zlm-1" || event.streamID != "cascade-0123456789abcdef" || !event.active || event.reason != "stream_registered" {
		t.Fatalf("registered event = %+v", event)
	}
	if event := lifecycle.events[1]; event.mediaServerID != "edge-zlm-1" || event.streamID != "cascade-0123456789abcdef" || event.active || event.reason != "stream_unregistered" {
		t.Fatalf("unregistered event = %+v", event)
	}
}

func TestZLMWebhookDispatchesMediaServerAwareGBHooks(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	protocol := &mediaAwareProtocol{}
	webhook := WebHookAPI{
		ipcCore:       ipc.NewCore(store, uniqueid.Core{}, nil),
		recordingCore: recording.Core{},
		log:           slog.Default(),
		protocols:     map[string]ipc.Protocoler{ipc.TypeGB28181: protocol},
	}

	changedCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	changedCtx.Request = httptest.NewRequest("POST", "/webhook/on_stream_changed", nil)
	if _, err := webhook.onStreamChanged(changedCtx, &onStreamChangedInput{
		Regist:        false,
		App:           "rtp",
		Stream:        "ch-media-aware",
		MediaServerID: "edge-zlm-2",
	}); err != nil {
		t.Fatal(err)
	}

	notFoundCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	notFoundCtx.Request = httptest.NewRequest("POST", "/webhook/on_stream_not_found", nil)
	if _, err := webhook.onStreamNotFound(notFoundCtx, &onStreamNotFoundInput{
		App:           "rtp",
		Stream:        "ch-media-aware",
		Schema:        "rtsp",
		MediaServerID: "edge-zlm-3",
	}); err != nil {
		t.Fatal(err)
	}

	if len(protocol.changed) != 1 || protocol.changed[0] != "edge-zlm-2" {
		t.Fatalf("media-aware stream change IDs = %v", protocol.changed)
	}
	if len(protocol.notFound) != 1 || protocol.notFound[0] != "edge-zlm-3" {
		t.Fatalf("media-aware stream-not-found IDs = %v", protocol.notFound)
	}
}
