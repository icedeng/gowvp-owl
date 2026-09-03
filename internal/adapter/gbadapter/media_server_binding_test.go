package gbadapter

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/core/sms/store/smsdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

var mediaServerResolverDatabaseSequence atomic.Uint64

func newMediaServerResolverAdapter(t *testing.T) (*Adapter, *ipc.Core, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), mediaServerResolverDatabaseSequence.Add(1))))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	ipcStore := ipcdb.NewDB(db).AutoMigrate(true)
	channel := &ipc.Channel{
		ID: "GBC_media_resolver", DeviceID: "34020000002000000001", ChannelID: "34020000001320000001",
		Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: "local"},
	}
	if err := ipcStore.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	smsStore := smsdb.NewDB(db).AutoMigrate(true)
	for _, server := range []*sms.MediaServer{{ID: "local"}, {ID: "edge-zlm-1"}, {ID: "voice-zlm-1"}} {
		if err := smsStore.MediaServer().Create(t.Context(), server); err != nil {
			t.Fatal(err)
		}
	}
	core := ipc.NewCore(ipcStore, uniqueid.Core{}, nil)
	smsCore := sms.NewCore(smsStore)
	t.Cleanup(func() {
		smsCore.Close()
		_ = sqlDB.Close()
	})
	adapter := NewAdapter(ipc.NewAdapter(ipcStore, uniqueid.Core{}), nil, smsCore)
	return adapter, &core, channel.ID
}

func TestMediaServerResolverReadsLatestChannelBinding(t *testing.T) {
	adapter, core, channelID := newMediaServerResolverAdapter(t)
	resolver := adapter.mediaServerResolver(channelID, "")
	if _, err := core.EditChannelConfig(t.Context(), channelID, func(config *ipc.StreamConfig) {
		config.MediaServerID = "edge-zlm-1"
	}); err != nil {
		t.Fatal(err)
	}

	server, err := resolver(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if server.ID != "edge-zlm-1" {
		t.Fatalf("resolved media server = %q, want latest binding edge-zlm-1", server.ID)
	}
}

func TestMediaServerResolverPreservesExplicitVoiceSource(t *testing.T) {
	adapter, core, channelID := newMediaServerResolverAdapter(t)
	resolver := adapter.mediaServerResolver(channelID, " voice-zlm-1 ")
	if _, err := core.EditChannelConfig(t.Context(), channelID, func(config *ipc.StreamConfig) {
		config.MediaServerID = "edge-zlm-1"
	}); err != nil {
		t.Fatal(err)
	}

	server, err := resolver(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if server.ID != "voice-zlm-1" {
		t.Fatalf("resolved explicit voice media server = %q, want voice-zlm-1", server.ID)
	}
}
