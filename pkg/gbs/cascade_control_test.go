package gbs

import (
	"context"
	"encoding/xml"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestValidateCascadeDeviceControlVersionAndScope(t *testing.T) {
	ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionLeft, Speed: 40})
	if err != nil {
		t.Fatal(err)
	}
	base := deviceControlA23Request{
		XMLName: xml.Name{Local: "Control"}, CmdType: ptzCmdTypeDeviceControl, SN: 1,
		DeviceID: testExposedChannelID, PTZCmd: ptz,
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		if err := validateCascadeDeviceControl(&base, version, version); err != nil {
			t.Fatalf("base PTZ rejected for %s: %v", version, err)
		}
	}
	badChecksum := base
	badChecksum.PTZCmd = ptz[:14] + "00"
	if err := validateCascadeDeviceControl(&badChecksum, GBVersion20, GBVersion20); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("bad PTZ checksum error = %v", err)
	}
	drag := base
	drag.PTZCmd = ""
	drag.DragZoomIn = &deviceControlA23DragZoom{Length: 100, Width: 100}
	if err := validateCascadeDeviceControl(&drag, GBVersion10, GBVersion11); err == nil {
		t.Fatal("1.0 upstream accepted 1.1 DragZoom")
	}
	if err := validateCascadeDeviceControl(&drag, GBVersion11, GBVersion11); err != nil {
		t.Fatalf("1.1 DragZoom rejected: %v", err)
	}
	precise := base
	precise.PTZCmd = ""
	pan := 120.5
	precise.PTZPreciseCtrl = &deviceControlA23PTZPrecise{Pan: &pan}
	if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion20); err == nil {
		t.Fatal("2.0 downstream accepted 3.0 precise PTZ")
	}
	if err := validateCascadeDeviceControl(&precise, GBVersion30, GBVersion30); err != nil {
		t.Fatalf("3.0 precise PTZ rejected: %v", err)
	}
	reboot := base
	reboot.PTZCmd = ""
	reboot.TeleBoot = "Boot"
	if err := validateCascadeDeviceControl(&reboot, GBVersion30, GBVersion30); err == nil || !strings.Contains(err.Error(), "device-scoped") {
		t.Fatalf("device-scoped control error = %v", err)
	}
}

func TestCascadeDeviceControlRoutesSharedChannelAndMapsResponse(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	runtimeDevice := &Device{
		IsOnline: true, gbVersion: string(GBVersion11), conn: connection, source: connection.remote,
		to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example"),
	}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}, channel: runtimeChannel}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{cfg: &conf.SIP{}, core: adapter, svr: server}
	server.gb = api
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(server, platform)
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	responses := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		responses <- request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	var forwardedChannel *ipc.Channel
	var forwarded *deviceControlA23Request
	api.cascadeDeviceControl = func(_ context.Context, channel *ipc.Channel, request *deviceControlA23Request) (string, error) {
		forwardedChannel = channel
		copy := *request
		forwarded = &copy
		return ptzResultOK, nil
	}
	ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionRight, Speed: 40})
	if err != nil {
		t.Fatal(err)
	}
	body, err := sip.XMLEncode(deviceControlA23Request{
		CmdType: ptzCmdTypeDeviceControl, SN: 73, DeviceID: testExposedChannelID, PTZCmd: ptz,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-control", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-control", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("cascade control SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade control SIP response timeout")
	}
	select {
	case response := <-responses:
		text := string(response.Body())
		for _, expected := range []string{
			"<CmdType>DeviceControl</CmdType>", "<SN>73</SN>",
			"<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Result>OK</Result>",
		} {
			if !strings.Contains(text, expected) {
				t.Fatalf("cascade control business response missing %q: %s", expected, text)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("cascade control business response timeout")
	}
	if forwardedChannel == nil || forwardedChannel.ChannelID != persistentChannel.ChannelID || forwarded == nil || forwarded.DeviceID != testExposedChannelID || forwarded.SN != 73 {
		t.Fatalf("forwarded cascade control = channel %+v request %+v", forwardedChannel, forwarded)
	}
}
