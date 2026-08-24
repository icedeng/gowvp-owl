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
	streamID string
	active   bool
	reason   string
}

type capturingGBMediaLifecycle struct {
	events []capturedGBMediaEvent
}

func (c *capturingGBMediaLifecycle) OnMediaStreamChanged(_ context.Context, streamID string, active bool, reason string) error {
	c.events = append(c.events, capturedGBMediaEvent{streamID: streamID, active: active, reason: reason})
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
			Regist: active,
			App:    "rtp",
			Stream: "cascade-0123456789abcdef",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if len(lifecycle.events) != 2 {
		t.Fatalf("GB media lifecycle events = %d, want 2", len(lifecycle.events))
	}
	if event := lifecycle.events[0]; event.streamID != "cascade-0123456789abcdef" || !event.active || event.reason != "stream_registered" {
		t.Fatalf("registered event = %+v", event)
	}
	if event := lifecycle.events[1]; event.streamID != "cascade-0123456789abcdef" || event.active || event.reason != "stream_unregistered" {
		t.Fatalf("unregistered event = %+v", event)
	}
}
