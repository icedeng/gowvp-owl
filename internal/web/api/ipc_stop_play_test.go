package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/core/sms/store/smsdb"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

type stopPlayProtocolStub struct {
	ipc.Protocoler
	stop func(context.Context, *ipc.Device, *ipc.Channel) error
}

func (s *stopPlayProtocolStub) StopPlay(ctx context.Context, device *ipc.Device, channel *ipc.Channel) error {
	return s.stop(ctx, device, channel)
}

type countingCloseRTPDriver struct {
	sms.Driver
	closeCalls       int
	closeStreamCalls int
}

func (d *countingCloseRTPDriver) CloseRTPServer(
	context.Context,
	*sms.MediaServer,
	*zlm.CloseRTPServerRequest,
) (*zlm.CloseRTPServerResponse, error) {
	d.closeCalls++
	return &zlm.CloseRTPServerResponse{Hit: 1}, nil
}

func (d *countingCloseRTPDriver) CloseStreams(
	context.Context,
	*sms.MediaServer,
	*zlm.CloseStreamsRequest,
) (*zlm.CloseStreamsResponse, error) {
	d.closeStreamCalls++
	return &zlm.CloseStreamsResponse{}, nil
}

func TestStopPlayUsesPersistedGBTypeAndLetsProtocolOwnMediaCleanup(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	ipcStore := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID:       "GBD_stop_cleanup_owner",
		Type:     ipc.TypeGB28181,
		DeviceID: "34020000002000000001",
	}
	channel := &ipc.Channel{
		ID:        "legacy_imported_stop_channel",
		DID:       device.ID,
		Type:      ipc.TypeGB28181,
		DeviceID:  device.DeviceID,
		ChannelID: "34020000001320000001",
	}
	if err := ipcStore.Device().Create(t.Context(), device); err != nil {
		t.Fatal(err)
	}
	if err := ipcStore.Channel().Create(t.Context(), channel); err != nil {
		t.Fatal(err)
	}

	smsStore := smsdb.NewDB(db).AutoMigrate(true)
	mediaServer := &sms.MediaServer{ID: sms.DefaultMediaServerID, Type: "counting"}
	if err := smsStore.MediaServer().Create(t.Context(), mediaServer); err != nil {
		t.Fatal(err)
	}
	smsCore := sms.NewCore(smsStore)
	t.Cleanup(smsCore.Close)
	driver := &countingCloseRTPDriver{}
	smsCore.RegisterDriver(mediaServer.Type, driver)

	protocolCalls := 0
	protocol := &stopPlayProtocolStub{stop: func(ctx context.Context, _ *ipc.Device, stopped *ipc.Channel) error {
		protocolCalls++
		_, err := smsCore.CloseRTPServerContext(ctx, mediaServer, zlm.CloseRTPServerRequest{StreamID: stopped.GetStream()})
		return err
	}}
	api := IPCAPI{
		ipc: ipc.NewCore(ipcStore, uniqueid.Core{}, map[string]ipc.Protocoler{
			ipc.TypeGB28181: protocol,
		}),
		uc: &Usecase{SMSAPI: SmsAPI{smsCore: smsCore}},
	}

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/channels/"+channel.ID+"/stop", nil)
	ctx.Params = gin.Params{{Key: "id", Value: channel.ID}}
	if _, err := api.stopPlay(ctx, nil); err != nil {
		t.Fatalf("stop play: %v", err)
	}
	if protocolCalls != 1 {
		t.Fatalf("protocol StopPlay calls = %d, want 1", protocolCalls)
	}
	if driver.closeCalls != 1 {
		t.Fatalf("RTP close calls = %d, want exactly the protocol-owned cleanup", driver.closeCalls)
	}
	if driver.closeStreamCalls != 0 {
		t.Fatalf("close_streams calls = %d, want 0 for persisted GB channel type", driver.closeStreamCalls)
	}
}
