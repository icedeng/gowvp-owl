package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func testDeviceConfigBasicParam2014() *BasicParam {
	return &BasicParam{
		Name: "IPC", DeviceID: testExposedChannelID, SIPServerID: gb10PlatformID,
		SIPServerIP: "192.0.2.20", SIPServerPort: 5060, DomainName: "3402000000",
		Expiration: 3600, Password: "secret", HeartBeatInterval: 60, HeartBeatCount: 3,
	}
}

func TestCascadeDeviceConfigVersionAndPayloadValidation(t *testing.T) {
	base := DeviceConfigRequest{
		XMLName: xml.Name{Local: "Control"}, CmdType: "DeviceConfig", SN: 1, DeviceID: testExposedChannelID,
		BasicParam: testDeviceConfigBasicParam2014(),
	}
	if err := validateCascadeDeviceConfigRequest(&base, GBVersion10); err == nil {
		t.Fatal("1.0 cascade DeviceConfig was accepted")
	}
	if err := validateCascadeDeviceConfigRequest(&base, GBVersion11); err != nil {
		t.Fatalf("1.1 cascade DeviceConfig rejected: %v", err)
	}
	svac := base
	svac.BasicParam = nil
	svac.SVACEncodeConfig = &SVACEncodeConfig{InnerXML: `<SVCParam><SVCFlag>1</SVCFlag><SVCSTMMode>1</SVCSTMMode></SVCParam>`}
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
	invalid = base
	invalid.BasicParam = testDeviceConfigBasicParam2014()
	invalid.BasicParam.Expiration = minimumStandardRegisterTTL - 1
	if err := validateCascadeDeviceConfigRequest(&invalid, GBVersion11); err == nil {
		t.Fatal("cascade BasicParam accepted expiration below the standard minimum")
	}
	invalid = extended
	invalid.FrameMirror = &FrameMirror{InnerXML: `<!DOCTYPE test><Value>1</Value>`}
	if err := validateCascadeDeviceConfigRequest(&invalid, GBVersion30); err == nil {
		t.Fatal("unsafe cascade DeviceConfig fragment was accepted")
	}
	invalid = extended
	invalid.FrameMirror = nil
	invalid.VideoAlarmRecord = &VideoAlarmRecord{InnerXML: `<RecordEnable>2</RecordEnable><StreamNumber>0</StreamNumber>`}
	if err := validateCascadeDeviceConfigRequest(&invalid, GBVersion30); err == nil {
		t.Fatal("invalid structured cascade DeviceConfig fragment was accepted")
	}
	for _, section := range []DeviceConfigRequest{
		{PictureMask: &PictureMask{InnerXML: `<On>1</On><SumNum>1</SumNum>`}},
		{FrameMirror: &FrameMirror{InnerXML: `4`}},
		{AlarmReport: &AlarmReport{InnerXML: `<MotionDetection>1</MotionDetection>`}},
	} {
		invalid = base
		invalid.BasicParam = nil
		invalid.PictureMask = section.PictureMask
		invalid.FrameMirror = section.FrameMirror
		invalid.AlarmReport = section.AlarmReport
		if err := validateCascadeDeviceConfigRequest(&invalid, GBVersion30); err == nil {
			t.Fatalf("invalid structured cascade DeviceConfig section was accepted: %+v", section)
		}
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
		BasicParam: testDeviceConfigBasicParam2014(),
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
		BasicParam: testDeviceConfigBasicParam2014(),
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

func TestCascadeMiddlewareDoesNotDispatchDeviceConfigWhenSIPOKFails(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	dispatched := make(chan struct{}, 1)
	api.cascadeDeviceConfig = func(_ context.Context, _ *ipc.Channel, _ *DeviceConfigRequest) (string, error) {
		dispatched <- struct{}{}
		return "OK", nil
	}
	body, err := sip.XMLEncode(DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 47, DeviceID: testExposedChannelID,
		BasicParam: testDeviceConfigBasicParam2014(),
	})
	if err != nil {
		t.Fatal(err)
	}
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("cascade DeviceConfig SIP OK write failed"),
	}
	request := newFlowRequest(t, base, sip.MethodMessage, "cascade-device-config-sip-ok-failure", body)
	request.SetConnection(connection)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-device-config-sip-ok-failure", connection),
		DeviceID: worker.platform.serverID, Source: base.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	done := make(chan struct{})
	go func() {
		api.sipCascadeMessageMiddleware(ctx)
		close(done)
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("cascade DeviceConfig SIP response write did not start")
	}
	select {
	case <-dispatched:
		close(connection.release)
		<-done
		t.Fatal("cascade DeviceConfig dispatched before SIP OK completed")
	default:
	}
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cascade DeviceConfig handler did not return after SIP OK write failure")
	}
	select {
	case <-dispatched:
		t.Fatal("cascade DeviceConfig dispatched after SIP OK write failure")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCascadeMiddlewareRejectsMalformedDeviceConfigBeforeSIPOK(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	server := &Server{}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()
	dispatched := make(chan struct{}, 1)
	api.cascadeDeviceConfig = func(_ context.Context, _ *ipc.Channel, _ *DeviceConfigRequest) (string, error) {
		dispatched <- struct{}{}
		return "OK", nil
	}
	body := []byte(`<Control><CmdType>DeviceConfig</CmdType><SN>48</SN><DeviceID>` + testExposedChannelID + `</DeviceID><BasicParam><Name>IPC</Name></BasicParam><Unknown>1</Unknown></Control>`)
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-device-config-malformed", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-device-config-malformed", connection),
		DeviceID: worker.platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("malformed DeviceConfig SIP response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("malformed DeviceConfig SIP response timeout")
	}
	select {
	case <-dispatched:
		t.Fatal("malformed DeviceConfig was dispatched")
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
		BasicParam: &BasicParam{Name: "IPC", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
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
		ExtraInfo:  []string{" first ", "second"},
		BasicParam: &BasicParam{}, VideoParamConfig: &VideoParamConfigWrite{}, AudioParamConfig: &AudioParamConfigWrite{},
		SVACEncodeConfig: &SVACEncodeConfig{}, SVACDecodeConfig: &SVACDecodeConfig{}, VideoParamAttribute: &VideoParamAttribute{},
		VideoRecordPlan: &VideoRecordPlan{}, VideoAlarmRecord: &VideoAlarmRecord{}, PictureMask: &PictureMask{}, FrameMirror: &FrameMirror{},
		AlarmReport: &AlarmReport{}, OSDConfig: &OSDConfig{}, SnapShotConfig: &SnapShot{},
	}
	in := cascadeDeviceConfigInput(channel, request)
	if in == nil || in.DeviceID != channel.DeviceID || in.TargetID != channel.ChannelID || in.BasicParam != request.BasicParam ||
		len(in.ExtraInfo) != 2 || in.ExtraInfo[0] != " first " ||
		in.VideoParamConfig != request.VideoParamConfig || in.AudioParamConfig != request.AudioParamConfig ||
		in.SVACEncodeConfig != request.SVACEncodeConfig || in.SVACDecodeConfig != request.SVACDecodeConfig ||
		in.VideoParamAttribute != request.VideoParamAttribute || in.VideoRecordPlan != request.VideoRecordPlan ||
		in.VideoAlarmRecord != request.VideoAlarmRecord || in.PictureMask != request.PictureMask || in.FrameMirror != request.FrameMirror ||
		in.AlarmReport != request.AlarmReport || in.OSDConfig != request.OSDConfig || in.SnapShotConfig != request.SnapShotConfig {
		t.Fatalf("cascade DeviceConfig input = %+v", in)
	}
	request.ExtraInfo[0] = "changed"
	if in.ExtraInfo[0] != " first " {
		t.Fatalf("cascade DeviceConfig ExtraInfo aliases request: %#v", in.ExtraInfo)
	}
}

func TestCascadeDeviceConfigExtraInfoVersionAndStructure(t *testing.T) {
	base := DeviceConfigRequest{
		XMLName: xml.Name{Local: "Control"}, CmdType: "DeviceConfig", SN: 1, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Name: "IPC"}, ExtraInfo: []string{" first ", "second"},
	}
	if err := validateCascadeDeviceConfigRequest(&base, GBVersion30); err != nil {
		t.Fatalf("3.0 DeviceConfig ExtraInfo rejected: %v", err)
	}
	if err := validateCascadeDeviceConfigRequest(&base, GBVersion20); err == nil || !strings.Contains(err.Error(), "protocol 3.0") {
		t.Fatalf("2.0 DeviceConfig ExtraInfo error = %v", err)
	}
	base.ExtraInfo = []string{strings.Repeat("界", 1025)}
	if err := validateCascadeDeviceConfigRequest(&base, GBVersion30); err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("oversized DeviceConfig ExtraInfo error = %v", err)
	}

	valid := `<Control><CmdType>DeviceConfig</CmdType><SN>1</SN><DeviceID>` + testExposedChannelID + `</DeviceID><BasicParam><Name>IPC</Name></BasicParam><ExtraInfo> first </ExtraInfo><ExtraInfo>second</ExtraInfo></Control>`
	if err := validateDeviceConfigRequestStructure([]byte(valid), GBVersion30); err != nil {
		t.Fatalf("valid 3.0 DeviceConfig structure rejected: %v", err)
	}
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
	}{
		{name: "legacy ExtraInfo", version: GBVersion20, body: valid},
		{name: "legacy Info spelling", version: GBVersion30, body: strings.Replace(valid, "<ExtraInfo> first </ExtraInfo>", "<Info> first </Info>", 1)},
		{name: "misspelled ExtralInfo", version: GBVersion30, body: strings.Replace(valid, "ExtraInfo", "ExtralInfo", 2)},
		{name: "duplicate section", version: GBVersion30, body: strings.Replace(valid, "</BasicParam>", "</BasicParam><BasicParam><Name>IPC2</Name></BasicParam>", 1)},
		{name: "unknown section", version: GBVersion30, body: strings.Replace(valid, "<ExtraInfo>", "<Unknown>1</Unknown><ExtraInfo>", 1)},
		{name: "out of order", version: GBVersion30, body: strings.Replace(strings.Replace(valid, "<BasicParam><Name>IPC</Name></BasicParam>", "", 1), "</Control>", "<BasicParam><Name>IPC</Name></BasicParam></Control>", 1)},
		{name: "root attribute", version: GBVersion30, body: strings.Replace(valid, "<Control>", `<Control version="3.0">`, 1)},
		{name: "nested simple field", version: GBVersion30, body: strings.Replace(valid, "<SN>1</SN>", "<SN><Value>1</Value></SN>", 1)},
		{name: "oversized", version: GBVersion30, body: strings.Replace(valid, "second", strings.Repeat("界", 1025), 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateDeviceConfigRequestStructure([]byte(test.body), test.version); err == nil {
				t.Fatal("invalid DeviceConfig structure was accepted")
			}
		})
	}
}

func TestDeviceConfigRequestStructureRequiresComplete2014BasicParam(t *testing.T) {
	parts := []string{
		`<Name>IPC</Name>`,
		`<DeviceID>` + testExposedChannelID + `</DeviceID>`,
		`<SIPServerID>` + gb10PlatformID + `</SIPServerID>`,
		`<SIPServerIP>192.0.2.20</SIPServerIP>`,
		`<SIPServerPort>5060</SIPServerPort>`,
		`<DomainName>3402000000</DomainName>`,
		`<Expiration>3600</Expiration>`,
		`<Password>secret</Password>`,
		`<HeartBeatInterval>60</HeartBeatInterval>`,
		`<HeartBeatCount>3</HeartBeatCount>`,
	}
	prefix := `<Control><CmdType>DeviceConfig</CmdType><SN>1</SN><DeviceID>` + testExposedChannelID + `</DeviceID><BasicParam>`
	suffix := `</BasicParam></Control>`
	valid := prefix + strings.Join(parts, "") + suffix
	if err := validateDeviceConfigRequestStructure([]byte(valid), GBVersion11); err != nil {
		t.Fatalf("complete 2014 BasicParam rejected: %v", err)
	}
	for _, omitted := range parts {
		body := prefix + strings.Replace(strings.Join(parts, ""), omitted, "", 1) + suffix
		if err := validateDeviceConfigRequestStructure([]byte(body), GBVersion11); err == nil {
			t.Fatalf("2014 BasicParam missing %s was accepted", omitted)
		}
	}
	if err := validateDeviceConfigRequestStructure([]byte(prefix+`<Name>IPC</Name>`+suffix), GBVersion20); err != nil {
		t.Fatalf("2016 optional BasicParam fields were treated as required: %v", err)
	}
}

func TestCascadeDeviceConfigVideoParamAttributeNumPresence(t *testing.T) {
	item := `<Item><StreamNumber>0</StreamNumber><VideoFormat>H.265</VideoFormat><Resolution>1920x1080</Resolution><FrameRate>25</FrameRate><BitRateType>1</BitRateType></Item>`
	for _, test := range []struct {
		name    string
		section string
		valid   bool
	}{
		{name: "matching", section: `<VideoParamAttribute Num="1">` + item + `</VideoParamAttribute>`, valid: true},
		{name: "missing", section: `<VideoParamAttribute>` + item + `</VideoParamAttribute>`},
		{name: "mismatch", section: `<VideoParamAttribute Num="0">` + item + `</VideoParamAttribute>`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`<Control><CmdType>DeviceConfig</CmdType><SN>1</SN><DeviceID>` + testExposedChannelID + `</DeviceID>` + test.section + `</Control>`)
			var request DeviceConfigRequest
			if err := sip.XMLDecode(body, &request); err != nil {
				t.Fatal(err)
			}
			if request.VideoParamAttribute == nil {
				t.Fatal("VideoParamAttribute was not decoded")
			}
			err := validateCascadeDeviceConfigRequest(&request, GBVersion30)
			if (err == nil) != test.valid {
				t.Fatalf("validation error = %v, want valid %v", err, test.valid)
			}
		})
	}
}
