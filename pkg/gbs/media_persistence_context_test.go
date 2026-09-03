package gbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
)

func TestCloseMediaPersistenceUsesBoundedContext(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	base := ipcdb.NewDB(db).AutoMigrate(true)
	contexts := make(chan context.Context, 1)
	store := &mediaContextStore{
		Storer: base,
		channel: &mediaContextChannelStore{
			ChannelStorer: base.Channel(),
			contexts:      contexts,
		},
	}
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		core:            ipc.NewAdapter(store, uniqueid.Core{}),
		streams:         &conc.Map[string, *Streams]{},
		lifecycleDone:   make(chan struct{}),
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
	}
	api.streams.Store("play:test", &Streams{DeviceID: gb10DeviceID, ChannelID: "34020000001320000002"})
	api.beginClose()

	api.closeRemainingMediaSessions()
	select {
	case ctx := <-contexts:
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("shutdown media persistence context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > gbShutdownPersistenceTimeout {
			t.Fatalf("shutdown media persistence deadline remaining = %s", remaining)
		}
		if err := ctx.Err(); err != nil {
			t.Fatalf("shutdown media persistence context was canceled before cleanup: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown media persistence did not update channel state")
	}
}

func TestOfflineDeviceMediaCleanupUsesShutdownContext(t *testing.T) {
	type cleanupContextKey struct{}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithValue(context.Background(), cleanupContextKey{}, "shutdown"), time.Second)
	defer shutdownCancel()
	media := &contextAwareStopRTPMediaService{fakeRTPMediaService: &fakeRTPMediaService{}}
	api := &GB28181API{
		sms:                    media,
		streams:                &conc.Map[string, *Streams]{},
		lifecycleClosed:        true,
		shutdownPersistenceCtx: shutdownCtx,
	}
	const streamKey = "play:offline-device"
	api.streams.Store(streamKey, &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "offline-device-stream",
		mediaServer: &sms.MediaServer{},
	})

	api.cleanupOfflineDeviceRuntime(gb10DeviceID)
	if len(media.closeContexts) != 1 {
		t.Fatalf("offline device CloseRTPServerContext calls = %d, want 1", len(media.closeContexts))
	}
	if marker := media.closeContexts[0].Value(cleanupContextKey{}); marker != "shutdown" {
		t.Fatalf("offline device media cleanup context marker = %v", marker)
	}
	if _, exists := api.streams.Load(streamKey); exists {
		t.Fatal("offline device media cleanup retained stream")
	}
}

type mediaContextStore struct {
	ipc.Storer
	channel ipc.ChannelStorer
}

func (s *mediaContextStore) Channel() ipc.ChannelStorer { return s.channel }

type mediaContextChannelStore struct {
	ipc.ChannelStorer
	contexts chan<- context.Context
}

func (s *mediaContextChannelStore) Update(ctx context.Context, _ *ipc.Channel, _ func(*ipc.Channel) error, _ ...orm.QueryOption) error {
	s.contexts <- ctx
	return nil
}
