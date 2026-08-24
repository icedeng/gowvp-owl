package gbs

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	gb10DeviceID   = "34020000001320000001"
	gb10ChannelID  = "34020000001320000002"
	gb10PlatformID = "34020000002000000001"
)

func TestGB10CoreSimulationFlow(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	defer func() { config = previousConfig }()

	conn := newFlowConnection()
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	defer sipServer.Close()

	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{
		cfg:              &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses: newMultiResponseCollector(func(item RecordItem) string {
			return item.DeviceID + item.FilePath + item.StartTime + item.EndTime
		}),
	}
	server := &Server{
		Server:       sipServer,
		gb:           api,
		fromAddress:  *platform,
		memoryStorer: memory,
	}
	api.svr = server

	// REGISTER：1.0 声明被保留，平台响应仍声明自己的最高版本。
	register := newFlowRequest(t, conn, sip.MethodRegister, "register-1", nil)
	registerCtx := &sip.Context{Request: register, DeviceID: gb10DeviceID, XGBVer: string(GBVersion10), XGBVerRaw: string(GBVersion10)}
	if got := applyGBProtocolVersion(&memory.persistent.Ext, registerCtx.XGBVer); got != GBVersion10 {
		t.Fatalf("REGISTER effective version = %s", got)
	}
	memory.runtime.setGBVersion(GBVersion10)
	registerResponse := api.newRegisterResponse(registerCtx, 200, "OK")
	if headers := registerResponse.GetHeaders("X-GB-Ver"); len(headers) != 1 || !strings.Contains(headers[0].String(), "3.0") {
		t.Fatalf("REGISTER response X-GB-Ver = %#v", headers)
	}

	// Keepalive：宽松解析 1.0 XML，并更新设备在线状态且不丢失已协商版本。
	keepalive := readGB10Fixture(t, "keepalive.xml")
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "keepalive-1", keepalive, api.sipMessageKeepalive)
	assertFlowOK(t, response)
	if !memory.persistent.IsOnline || memory.persistent.Ext.GBEffectiveVersion != string(GBVersion10) {
		t.Fatalf("Keepalive device state = online:%v version:%q", memory.persistent.IsOnline, memory.persistent.Ext.GBEffectiveVersion)
	}

	// Catalog：处理 1.0 最小目录响应并返回 200。
	catalogBody := readGB10Fixture(t, "catalog-response.xml")
	var catalog MessageDeviceListResponse
	if err := sip.XMLDecode(catalogBody, &catalog); err != nil {
		t.Fatalf("decode Catalog: %v", err)
	}
	if catalog.SumNum != 1 || len(catalog.Item) != 1 || catalog.Item[0].ChannelID != gb10ChannelID {
		t.Fatalf("unexpected Catalog payload: %+v", catalog)
	}
	response = runFlowHandler(t, conn, api, sip.MethodMessage, "catalog-1", catalogBody, api.sipMessageCatalog)
	assertFlowOK(t, response)

	// RecordInfo：聚合一条 1.0 录像记录。
	recordBody := readGB10Fixture(t, "record-info-response.xml")
	recordStart := mustFlowTime(t, "2024-04-01T00:00:00")
	recordEnd := mustFlowTime(t, "2024-04-01T01:00:00")
	recordKey := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 3)
	api.recordResponses.Start(recordKey)
	response = runFlowHandler(t, conn, api, sip.MethodMessage, "record-1", recordBody, api.sipMessageRecordInfo)
	assertFlowOK(t, response)
	recordItems := api.recordResponses.Wait(context.Background(), recordKey)
	records := transRecordItems(recordItems.Items, recordStart.Unix(), recordEnd.Unix())
	if records.TimeNum != 1 {
		t.Fatalf("RecordInfo TimeNum = %d, want 1", records.TimeNum)
	}

	// Alarm：宽松解析报警并发出统一事件。
	alarmEvents := make(chan *AlarmEvent, 1)
	api.SetAlarmHandler(func(_ context.Context, event *AlarmEvent) {
		alarmEvents <- event
	})
	response = runFlowHandler(t, conn, api, sip.MethodMessage, "alarm-1", readGB10Fixture(t, "alarm-notify.xml"), api.sipMessageAlarm)
	assertFlowOK(t, response)
	select {
	case event := <-alarmEvents:
		if event.ChannelID != gb10ChannelID || event.AlarmMethod != "2" {
			t.Fatalf("unexpected Alarm event: %+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm event timeout")
	}

	// 非广播的入向 INVITE 属于平台被叫/级联场景；媒体转发未实现时不能回显 SDP 假装成功。
	sdpBody, err := buildGBSDP(gbSDPInput{
		Version:     GBVersion10,
		SessionName: historyModePlay,
		ChannelID:   gb10ChannelID,
		IP:          "192.0.2.20",
		Port:        30000,
		SSRC:        "0100000001",
	})
	if err != nil {
		t.Fatalf("build INVITE SDP: %v", err)
	}
	response = runFlowHandler(t, conn, api, sip.MethodInvite, "dialog-1", sdpBody, api.sipInviteGeneric)
	if !strings.Contains(response, "SIP/2.0 501 unrecognized inbound media session") {
		t.Fatalf("unexpected generic inbound INVITE response:\n%s", response)
	}
	if _, ok := api.inviteDialogs.Load("dialog-1"); ok {
		t.Fatal("unsupported inbound INVITE created a dialog")
	}
}

func runFlowHandler(t *testing.T, conn *flowConnection, api *GB28181API, method, callID string, body []byte, handler func(*sip.Context)) string {
	t.Helper()
	req := newFlowRequest(t, conn, method, callID, body)
	tx := sip.NewTransaction(callID+"-tx", conn)
	handler(&sip.Context{
		Request:  req,
		Tx:       tx,
		DeviceID: gb10DeviceID,
		Source:   conn.remote,
		To:       mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Log:      slog.Default(),
	})
	select {
	case payload := <-conn.writes:
		return string(payload)
	case <-time.After(time.Second):
		t.Fatalf("%s response timeout", method)
		return ""
	}
}

func newFlowRequest(t *testing.T, conn *flowConnection, method, callID string, body []byte) *sip.Request {
	t.Helper()
	device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	builder := sip.NewHeaderBuilder().
		SetFrom(device).
		SetToWithParam(platform).
		SetMethod(method).
		SetXGBVerValue(string(GBVersion10)).
		AddVia(&sip.ViaHop{
			Host:      "192.0.2.10",
			Port:      sip.NewPort(5060),
			Transport: "UDP",
			Params:    sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
		})
	if len(body) > 0 {
		if method == sip.MethodInvite {
			builder.SetContentType(&sip.ContentTypeSDP)
		} else {
			builder.SetContentType(&sip.ContentTypeXML)
		}
	}
	headers := builder.Build()
	for _, header := range headers {
		if id, ok := header.(*sip.CallID); ok {
			*id = sip.CallID(callID)
		}
	}
	req := sip.NewRequest("", method, platform.URI, sip.DefaultSipVersion, headers, body)
	req.SetConnection(conn)
	req.SetSource(conn.remote)
	req.SetDestination(conn.local)
	return req
}

func mustFlowAddress(t *testing.T, value string) *sip.Address {
	t.Helper()
	uri, err := sip.ParseSipURI(value)
	if err != nil {
		t.Fatalf("parse SIP URI %q: %v", value, err)
	}
	return &sip.Address{URI: &uri, Params: sip.NewParams()}
}

func mustFlowTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02T15:04:05", value, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func readGB10Fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.0", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertFlowOK(t *testing.T, response string) {
	t.Helper()
	if !strings.Contains(response, "SIP/2.0 200 OK") {
		t.Fatalf("unexpected SIP response:\n%s", response)
	}
}

type flowMemory struct {
	persistent *ipc.Device
	runtime    *Device
}

func newFlowMemory(deviceID string) *flowMemory {
	return &flowMemory{
		persistent: &ipc.Device{DeviceID: deviceID},
		runtime:    &Device{IsOnline: true, gbVersion: string(GBVersion10)},
	}
}

func (m *flowMemory) LoadOrStore(_ string, device *Device) {
	if m.runtime == nil {
		m.runtime = device
	}
}
func (m *flowMemory) LoadDeviceToMemory(sip.Connection) {}
func (m *flowMemory) RangeDevices(fn func(string, *Device) bool) {
	fn(m.persistent.DeviceID, m.runtime)
}
func (m *flowMemory) Change(_ string, changePersistent func(*ipc.Device) error, changeRuntime func(*Device)) error {
	if changePersistent != nil {
		if err := changePersistent(m.persistent); err != nil {
			return err
		}
	}
	if changeRuntime != nil {
		changeRuntime(m.runtime)
	}
	return nil
}
func (m *flowMemory) Load(string) (*Device, bool)                { return m.runtime, m.runtime != nil }
func (m *flowMemory) Store(_ string, device *Device)             { m.runtime = device }
func (m *flowMemory) GetChannel(string, string) (*Channel, bool) { return nil, false }

type flowConnection struct {
	local  net.Addr
	remote net.Addr
	writes chan []byte
}

func newFlowConnection() *flowConnection {
	return &flowConnection{
		local:  &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
		writes: make(chan []byte, 16),
	}
}

func (c *flowConnection) Read([]byte) (int, error) { return 0, io.EOF }
func (c *flowConnection) Write(payload []byte) (int, error) {
	c.writes <- append([]byte(nil), payload...)
	return len(payload), nil
}
func (c *flowConnection) Close() error                           { return nil }
func (c *flowConnection) LocalAddr() net.Addr                    { return c.local }
func (c *flowConnection) RemoteAddr() net.Addr                   { return c.remote }
func (c *flowConnection) SetDeadline(time.Time) error            { return nil }
func (c *flowConnection) SetReadDeadline(time.Time) error        { return nil }
func (c *flowConnection) SetWriteDeadline(time.Time) error       { return nil }
func (c *flowConnection) Network() string                        { return "udp" }
func (c *flowConnection) ReadFrom([]byte) (int, net.Addr, error) { return 0, c.remote, io.EOF }
func (c *flowConnection) WriteTo(payload []byte, _ net.Addr) (int, error) {
	return c.Write(payload)
}
