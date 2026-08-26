package gbs

import (
	"context"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestCascadeDeviceConfigVersionAndPayloadValidation(t *testing.T) {
	base := DeviceConfigRequest{
		XMLName: xml.Name{Local: "Control"}, CmdType: "DeviceConfig", SN: 1, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
	}
	if err := validateCascadeDeviceConfigRequest(&base, GBVersion10); err == nil {
		t.Fatal("1.0 cascade DeviceConfig was accepted")
	}
	if err := validateCascadeDeviceConfigRequest(&base, GBVersion11); err != nil {
		t.Fatalf("1.1 cascade DeviceConfig rejected: %v", err)
	}
	svac := base
	svac.BasicParam = nil
	svac.SVACEncodeConfig = &SVACEncodeConfig{InnerXML: `<SVCParam>1</SVCParam>`}
	if err := validateCascadeDeviceConfigRequest(&svac, GBVersion11); err != nil {
		t.Fatalf("1.1 cascade SVAC DeviceConfig rejected: %v", err)
	}

	extended := base
	extended.BasicParam = nil
	extended.FrameMirror = &FrameMirror{InnerXML: "1"}
	if err := validateCascadeDeviceConfigRequest(&extended, GBVersion20); err == nil {
		t.Fatal("2.0 accepted 3.0 DeviceConfig section")
	}
	if err := validateCascadeDeviceConfigRequest(&extended, GBVersion30); err != nil {
		t.Fatalf("3.0 cascade DeviceConfig rejected: %v", err)
	}

	invalid := base
	invalid.BasicParam = &BasicParam{Expiration: 3600, HeartBeatInterval: 0, HeartBeatCount: 3}
	if err := validateCascadeDeviceConfigRequest(&invalid, GBVersion11); err == nil {
		t.Fatal("invalid cascade BasicParam was accepted")
	}
	invalid = extended
	invalid.FrameMirror = &FrameMirror{InnerXML: `<!DOCTYPE test><Value>1</Value>`}
	if err := validateCascadeDeviceConfigRequest(&invalid, GBVersion30); err == nil {
		t.Fatal("unsafe cascade DeviceConfig fragment was accepted")
	}
}

func TestCascadeDeviceConfigMapsTargetAndReturnsUpstreamResult(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	var downstreamChannel *ipc.Channel
	var downstream *DeviceConfigRequest
	api.cascadeDeviceConfig = func(_ context.Context, target *ipc.Channel, request *DeviceConfigRequest) (string, error) {
		downstreamChannel = target
		copyRequest := *request
		downstream = &copyRequest
		return "OK", nil
	}
	var response *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		response = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body, err := sip.XMLEncode(DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 45, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	api.forwardCascadeDeviceConfig(worker, body, t.Context())
	if downstreamChannel == nil || downstreamChannel.DeviceID != channel.DeviceID || downstreamChannel.ChannelID != channel.ChannelID || downstream == nil {
		t.Fatalf("downstream DeviceConfig target/request = %+v / %+v", downstreamChannel, downstream)
	}
	if response == nil {
		t.Fatal("cascade DeviceConfig did not return a business response")
	}
	text := string(response.Body())
	for _, expected := range []string{"<CmdType>DeviceConfig</CmdType>", "<SN>45</SN>", "<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Result>OK</Result>"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("cascade DeviceConfig response missing %q: %s", expected, text)
		}
	}
}

func TestCascadeMiddlewareAcceptsDeviceConfigAndDispatchesOnce(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	dispatched := make(chan *DeviceConfigRequest, 1)
	api.cascadeDeviceConfig = func(_ context.Context, _ *ipc.Channel, request *DeviceConfigRequest) (string, error) {
		copyRequest := *request
		dispatched <- &copyRequest
		return "OK", nil
	}
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body, err := sip.XMLEncode(DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 46, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-device-config", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-device-config", connection),
		DeviceID: worker.platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200") {
			t.Fatalf("cascade DeviceConfig SIP response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade DeviceConfig SIP response timeout")
	}
	select {
	case request := <-dispatched:
		if request.SN != 46 || request.DeviceID != testExposedChannelID {
			t.Fatalf("dispatched DeviceConfig = %+v", request)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade DeviceConfig was not dispatched")
	}
	select {
	case request := <-dispatched:
		t.Fatalf("cascade DeviceConfig dispatched more than once: %+v", request)
	default:
	}
}

func TestCascadeMiddlewareAcknowledgesThenRejectsDeviceConfigAfterVersionDowngrade(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion10
	worker.mu.Unlock()
	dispatched := make(chan struct{}, 1)
	api.cascadeDeviceConfig = func(_ context.Context, _ *ipc.Channel, _ *DeviceConfigRequest) (string, error) {
		dispatched <- struct{}{}
		return "OK", nil
	}
	businessResponse := make(chan string, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		businessResponse <- string(request.Body())
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body, err := sip.XMLEncode(DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 47, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-device-config-old-version", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-device-config-old-version", connection),
		DeviceID: worker.platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200") {
			t.Fatalf("old-version DeviceConfig SIP response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("old-version DeviceConfig SIP response timeout")
	}
	select {
	case body := <-businessResponse:
		if !strings.Contains(body, "<Result>ERROR</Result>") {
			t.Fatalf("old-version DeviceConfig business response = %s", body)
		}
	case <-time.After(time.Second):
		t.Fatal("old-version DeviceConfig business response timeout")
	}
	select {
	case <-dispatched:
		t.Fatal("1.0 cascade DeviceConfig was sent downstream")
	default:
	}
}

func TestCascadeDeviceConfigInputMapsAllSections(t *testing.T) {
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	request := &DeviceConfigRequest{
		BasicParam: &BasicParam{}, VideoParamConfig: &VideoParamConfigWrite{}, AudioParamConfig: &AudioParamConfigWrite{},
		SVACEncodeConfig: &SVACEncodeConfig{}, SVACDecodeConfig: &SVACDecodeConfig{}, VideoParamAttribute: &VideoParamAttribute{},
		VideoRecordPlan: &VideoRecordPlan{}, VideoAlarmRecord: &VideoAlarmRecord{}, PictureMask: &PictureMask{}, FrameMirror: &FrameMirror{},
		AlarmReport: &AlarmReport{}, OSDConfig: &OSDConfig{}, SnapShotConfig: &SnapShot{},
	}
	in := cascadeDeviceConfigInput(channel, request)
	if in == nil || in.DeviceID != channel.DeviceID || in.TargetID != channel.ChannelID || in.BasicParam != request.BasicParam ||
		in.VideoParamConfig != request.VideoParamConfig || in.AudioParamConfig != request.AudioParamConfig ||
		in.SVACEncodeConfig != request.SVACEncodeConfig || in.SVACDecodeConfig != request.SVACDecodeConfig ||
		in.VideoParamAttribute != request.VideoParamAttribute || in.VideoRecordPlan != request.VideoRecordPlan ||
		in.VideoAlarmRecord != request.VideoAlarmRecord || in.PictureMask != request.PictureMask || in.FrameMirror != request.FrameMirror ||
		in.AlarmReport != request.AlarmReport || in.OSDConfig != request.OSDConfig || in.SnapShotConfig != request.SnapShotConfig {
		t.Fatalf("cascade DeviceConfig input = %+v", in)
	}
}
