package api

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/core/sms/store/smsdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

type channelMediaBindingCoordinatorStub struct {
	beforeUnlock func()
}

func (s channelMediaBindingCoordinatorStub) LockChannelMedia(context.Context, *ipc.Channel) (func(), error) {
	if s.beforeUnlock != nil {
		s.beforeUnlock()
	}
	return func() {}, nil
}

func TestChannelMediaServerIDUsesTrimmedBindingAndDefault(t *testing.T) {
	tests := []struct {
		name    string
		channel *ipc.Channel
		want    string
	}{
		{name: "nil channel", want: sms.DefaultMediaServerID},
		{name: "empty binding", channel: &ipc.Channel{}, want: sms.DefaultMediaServerID},
		{name: "trimmed binding", channel: &ipc.Channel{Config: ipc.StreamConfig{MediaServerID: " edge-zlm-1 "}}, want: "edge-zlm-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := channelMediaServerID(test.channel); got != test.want {
				t.Fatalf("channelMediaServerID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildRTSPURLUsesPersistedGBTypeWhenIDHasNoLegacyPrefix(t *testing.T) {
	channel := &ipc.Channel{
		ID:        "legacy_imported_rtsp_channel",
		DID:       "GBD_imported",
		DeviceID:  "34020000002000000001",
		ChannelID: "34020000001320000001",
		Type:      ipc.TypeGB28181,
	}
	api := newChannelMediaServerAPI(t, channel)

	got, err := api.buildRTSPURL(t.Context(), channel.ID)
	if err != nil {
		t.Fatalf("build RTSP URL: %v", err)
	}
	want := "rtsp://127.0.0.1:0/rtp/" + channel.ID
	if got != want {
		t.Fatalf("RTSP URL = %q, want %q", got, want)
	}
}

func TestBuildRTSPURLUsesBoundMediaServerHost(t *testing.T) {
	channel := &ipc.Channel{
		ID:        "ch_bound_media_rtsp",
		DID:       "gb_bound_media_rtsp",
		DeviceID:  "34020000002000000001",
		ChannelID: "34020000001320000003",
		Type:      ipc.TypeGB28181,
		Config:    ipc.StreamConfig{MediaServerID: "edge-zlm-1"},
	}
	api := newChannelMediaServerAPI(t, channel)

	got, err := api.buildRTSPURL(t.Context(), channel.ID)
	if err != nil {
		t.Fatalf("build RTSP URL: %v", err)
	}
	want := "rtsp://192.0.2.20:0/rtp/" + channel.ID
	if got != want {
		t.Fatalf("RTSP URL = %q, want bound media server URL %q", got, want)
	}
}

func TestPlayValidatesBoundGBMediaServerInsteadOfDefaultConfig(t *testing.T) {
	channel := &ipc.Channel{
		ID:        "ch_bound_media_sdp",
		DID:       "gb_bound_media_sdp",
		DeviceID:  "34020000002000000001",
		ChannelID: "34020000001320000002",
		Type:      ipc.TypeGB28181,
		Config:    ipc.StreamConfig{MediaServerID: "edge-zlm-1"},
	}
	api := newChannelMediaServerAPI(t, channel)
	api.uc.Conf = &conf.Bootstrap{Media: conf.Media{SDPIP: "127.0.0.1"}}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/channels/"+channel.ID+"/play", nil)
	ctx.Params = gin.Params{{Key: "id", Value: channel.ID}}
	if _, err := api.play(ctx, nil); err == nil || !strings.Contains(err.Error(), "流媒体服务离线") {
		t.Fatalf("play error = %v, want bound edge node offline error instead of default SDP rejection", err)
	}
}

func newChannelMediaServerAPI(t *testing.T, channel *ipc.Channel) IPCAPI {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	ipcStore := ipcdb.NewDB(db).AutoMigrate(true)
	if err := ipcStore.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}
	smsStore := smsdb.NewDB(db).AutoMigrate(true)
	for _, server := range []*sms.MediaServer{
		{ID: "local"},
		{ID: "edge-zlm-1", IP: "192.0.2.20", SDPIP: "192.0.2.20"},
		{ID: "edge-stream-ip", IP: "192.0.2.21", StreamIP: "198.51.100.21", Ports: sms.MediaServerPorts{RTSP: 8554}},
		{ID: "edge-ipv6", StreamIP: "2001:db8::20", Ports: sms.MediaServerPorts{RTSP: 9554}},
	} {
		if err := smsStore.MediaServer().Create(t.Context(), server); err != nil {
			t.Fatal(err)
		}
	}
	smsCore := sms.NewCore(smsStore)
	t.Cleanup(func() {
		smsCore.Close()
		_ = sqlDB.Close()
	})
	return IPCAPI{
		ipc: ipc.NewCore(ipcStore, uniqueid.Core{}, nil),
		uc:  &Usecase{SMSAPI: SmsAPI{smsCore: smsCore}},
	}
}

func TestBuildChannelRTSPURLPreservesProtocolPaths(t *testing.T) {
	tests := []struct {
		name    string
		channel *ipc.Channel
		want    string
	}{
		{
			name:    "onvif",
			channel: &ipc.Channel{ID: "onvif_camera_1", Type: ipc.TypeOnvif, Config: ipc.StreamConfig{MediaServerID: "edge-zlm-1"}},
			want:    "rtsp://192.0.2.20:0/live/onvif_camera_1",
		},
		{
			name:    "rtsp custom app",
			channel: &ipc.Channel{ID: "rtsp_camera_1", Type: ipc.TypeRTSP, App: "surveillance", Stream: "entrance", Config: ipc.StreamConfig{MediaServerID: "edge-zlm-1"}},
			want:    "rtsp://192.0.2.20:0/surveillance/entrance",
		},
		{
			name:    "rtmp default app",
			channel: &ipc.Channel{ID: "rtmp_camera_1", Type: ipc.TypeRTMP, Stream: "warehouse", Config: ipc.StreamConfig{MediaServerID: "edge-zlm-1"}},
			want:    "rtsp://192.0.2.20:0/live/warehouse",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api := newChannelMediaServerAPI(t, test.channel)
			got, err := buildChannelRTSPURL(t.Context(), api.uc.SMSAPI.smsCore, test.channel)
			if err != nil {
				t.Fatalf("build channel RTSP URL: %v", err)
			}
			if got != test.want {
				t.Fatalf("RTSP URL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildChannelRTSPURLPrefersStreamIPAndFormatsIPv6(t *testing.T) {
	tests := []struct {
		name          string
		mediaServerID string
		want          string
	}{
		{name: "stream IP", mediaServerID: "edge-stream-ip", want: "rtsp://198.51.100.21:8554/rtp/gb_rtsp_host"},
		{name: "IPv6", mediaServerID: "edge-ipv6", want: "rtsp://[2001:db8::20]:9554/rtp/gb_rtsp_host"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &ipc.Channel{ID: "gb_rtsp_host", Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: test.mediaServerID}}
			api := newChannelMediaServerAPI(t, channel)
			got, err := buildChannelRTSPURL(t.Context(), api.uc.SMSAPI.smsCore, channel)
			if err != nil {
				t.Fatalf("build channel RTSP URL: %v", err)
			}
			if got != test.want {
				t.Fatalf("RTSP URL = %q, want %q", got, test.want)
			}
		})
	}
}

func TestBuildChannelRTSPURLRejectsMissingBoundMediaServer(t *testing.T) {
	channel := &ipc.Channel{ID: "gb_missing_media", Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: "missing-edge"}}
	api := newChannelMediaServerAPI(t, channel)
	if _, err := buildChannelRTSPURL(t.Context(), api.uc.SMSAPI.smsCore, channel); err == nil {
		t.Fatal("missing bound media server accepted")
	}
}

func TestBuildChannelRTSPURLRejectsUnsupportedTypeBeforeMediaLookup(t *testing.T) {
	channel := &ipc.Channel{ID: "unsupported_channel", Type: "unsupported", Config: ipc.StreamConfig{MediaServerID: "missing-edge"}}
	api := newChannelMediaServerAPI(t, channel)
	if _, err := buildChannelRTSPURL(t.Context(), api.uc.SMSAPI.smsCore, channel); err == nil || !strings.Contains(err.Error(), "不支持的通道类型") {
		t.Fatalf("unsupported channel error = %v, want unsupported type", err)
	}
}

func TestMediaServerPublishHostPrefersExplicitStreamAddress(t *testing.T) {
	tests := []struct {
		name     string
		server   *sms.MediaServer
		fallback string
		want     string
	}{
		{name: "stream IP", server: &sms.MediaServer{StreamIP: "198.51.100.20", IP: "192.0.2.20"}, fallback: "owl.example", want: "198.51.100.20"},
		{name: "IP fallback", server: &sms.MediaServer{IP: "192.0.2.20"}, fallback: "owl.example", want: "192.0.2.20"},
		{name: "loopback fallback", server: &sms.MediaServer{StreamIP: "127.0.0.1", IP: "::1"}, fallback: "owl.example", want: "owl.example"},
		{name: "IPv6", server: &sms.MediaServer{StreamIP: "2001:db8::20"}, fallback: "owl.example", want: "2001:db8::20"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := mediaServerPublishHost(test.server, test.fallback); got != test.want {
				t.Fatalf("mediaServerPublishHost() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMediaServerPublishHostFormatsIPv6RTMPAddress(t *testing.T) {
	addr := fmt.Sprintf("rtmp://%s/%s/%s", net.JoinHostPort(mediaServerPublishHost(&sms.MediaServer{StreamIP: "2001:db8::20"}, "owl.example"), "1935"), "live", "camera")
	if addr != "rtmp://[2001:db8::20]:1935/live/camera" {
		t.Fatalf("RTMP address = %q, want IPv6 host formatting", addr)
	}
}

func TestFillRTMPPushAddrUsesBoundMediaServerHost(t *testing.T) {
	channel := &ipc.Channel{
		ID: "rtmp_bound_publish", Type: ipc.TypeRTMP, App: "live", Stream: "camera",
		Config: ipc.StreamConfig{MediaServerID: "edge-stream-ip", IsAuthDisabled: true},
	}
	api := newChannelMediaServerAPI(t, channel)
	api.uc.Conf = &conf.Bootstrap{}
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("GET", "http://owl.example/channels", nil)
	api.fillRTMPPushAddr(ctx, []*ipc.Channel{channel})
	if channel.Config.PushAddr != "rtmp://198.51.100.21:1935/live/camera" {
		t.Fatalf("push address = %q, want bound media server host", channel.Config.PushAddr)
	}
}

func TestStartAITaskUsesBoundMediaServerBeforeCallingAIService(t *testing.T) {
	channel := &ipc.Channel{ID: "gb_ai_missing_media", Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: "missing-edge"}}
	api := newChannelMediaServerAPI(t, channel)
	aiAPI := AIWebhookAPI{}
	err := aiAPI.startAITask(t.Context(), api.uc.SMSAPI.smsCore, channel)
	if err == nil || !strings.Contains(err.Error(), "build AI RTSP URL") {
		t.Fatalf("start AI task error = %v, want bound media routing error", err)
	}
	if strings.Contains(err.Error(), "AI service not initialized") {
		t.Fatalf("start AI task reached AI service before validating bound media server: %v", err)
	}
}

func channelMediaServerContext(t *testing.T, channelID string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("PUT", "/channels/"+channelID+"/media_server", nil)
	ctx.Params = gin.Params{{Key: "id", Value: channelID}}
	return ctx
}

func TestEditChannelMediaServerOnlyUpdatesBinding(t *testing.T) {
	channel := &ipc.Channel{
		ID: "GBC_media_binding", DeviceID: "34020000002000000001", ChannelID: "34020000001320000001",
		Name: "Front Gate", Type: ipc.TypeGB28181, IsOnline: true,
		Config: ipc.StreamConfig{MediaServerID: "local"},
	}
	api := newChannelMediaServerAPI(t, channel)

	out, err := api.editChannelMediaServer(channelMediaServerContext(t, channel.ID), &editChannelMediaServerInput{MediaServerID: " edge-zlm-1 "})
	if err != nil {
		t.Fatalf("edit media server: %v", err)
	}
	updated, ok := out.(*ipc.Channel)
	if !ok {
		t.Fatalf("response type = %T, want *ipc.Channel", out)
	}
	if updated.Config.MediaServerID != "edge-zlm-1" {
		t.Fatalf("media server = %q, want edge-zlm-1", updated.Config.MediaServerID)
	}
	if updated.Name != channel.Name || updated.ChannelID != channel.ChannelID || !updated.IsOnline {
		t.Fatalf("unrelated channel fields changed: %+v", updated)
	}
}

func TestEditChannelMediaServerRejectsActiveSession(t *testing.T) {
	channel := &ipc.Channel{
		ID: "GBC_media_active", DeviceID: "34020000002000000001", ChannelID: "34020000001320000002",
		Type: ipc.TypeGB28181, IsPlaying: true, Config: ipc.StreamConfig{MediaServerID: "local"},
	}
	api := newChannelMediaServerAPI(t, channel)
	if _, err := api.editChannelMediaServer(channelMediaServerContext(t, channel.ID), &editChannelMediaServerInput{MediaServerID: "edge-zlm-1"}); err == nil {
		t.Fatal("active media session accepted binding change")
	}
	unchanged, err := api.ipc.GetChannel(t.Context(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Config.MediaServerID != "local" {
		t.Fatalf("rejected edit changed media server to %q", unchanged.Config.MediaServerID)
	}
}

func TestEditChannelMediaServerRechecksActiveSessionInsideProtocolLock(t *testing.T) {
	channel := &ipc.Channel{
		ID: "GBC_media_race", DeviceID: "34020000002000000001", ChannelID: "34020000001320000003",
		Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: "local"},
	}
	api := newChannelMediaServerAPI(t, channel)
	api.channelMediaBindingCoordinator = channelMediaBindingCoordinatorStub{beforeUnlock: func() {
		if _, err := api.ipc.EditChannelPlaying(t.Context(), channel.ID, true); err != nil {
			t.Fatal(err)
		}
	}}

	if _, err := api.editChannelMediaServer(channelMediaServerContext(t, channel.ID), &editChannelMediaServerInput{MediaServerID: "edge-zlm-1"}); err == nil {
		t.Fatal("binding change did not recheck the active media session after acquiring the protocol lock")
	}
	unchanged, err := api.ipc.GetChannel(t.Context(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Config.MediaServerID != "local" {
		t.Fatalf("rejected concurrent edit changed media server to %q", unchanged.Config.MediaServerID)
	}
}
