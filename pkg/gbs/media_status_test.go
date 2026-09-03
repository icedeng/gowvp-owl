package gbs

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
)

type blockingCloseRTPMediaService struct {
	*fakeRTPMediaService
	started chan struct{}
	release chan struct{}
}

func setMediaStatusTestVersion(t *testing.T, api *GB28181API, version GBProtocolVersion) {
	t.Helper()
	if api.svr == nil {
		api.svr = &Server{}
	}
	if api.svr.memoryStorer == nil {
		memory := newFlowMemory(gb10DeviceID)
		memory.runtime.setGBVersion(version)
		api.svr.memoryStorer = memory
		return
	}
	device, ok := api.svr.memoryStorer.Load(gb10DeviceID)
	if !ok || device == nil {
		device = &Device{IsOnline: true}
		api.svr.memoryStorer.Store(gb10DeviceID, device)
	}
	device.setGBVersion(version)
}

func (f *blockingCloseRTPMediaService) CloseRTPServer(_ *sms.MediaServer, _ zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error) {
	close(f.started)
	<-f.release
	return &zlm.CloseRTPServerResponse{Hit: 1}, nil
}

func TestMediaStatusRespondsBeforeSlowMediaCleanup(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "media-status-notify.xml"))
	if err != nil {
		t.Fatal(err)
	}
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-slow-cleanup",
		StreamID: "slow-cleanup-stream", mediaServer: &sms.MediaServer{ID: sms.DefaultMediaServerID},
	}
	streams.Store(key, stream)
	media := &blockingCloseRTPMediaService{
		fakeRTPMediaService: &fakeRTPMediaService{}, started: make(chan struct{}), release: make(chan struct{}),
	}
	api := &GB28181API{streams: streams, sms: media}
	setMediaStatusTestVersion(t, api, GBVersion11)
	conn := newFlowConnection()
	request := newFlowRequest(t, conn, sip.MethodMessage, "media-status-slow-cleanup", body)
	done := make(chan struct{})
	go func() {
		api.sipMessageMediaStatus(&sip.Context{
			Request: request, Tx: sip.NewTransaction("media-status-slow-cleanup-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote,
		})
		close(done)
	}()
	select {
	case <-media.started:
	case <-time.After(time.Second):
		t.Fatal("MediaStatus media cleanup did not start")
	}
	select {
	case payload := <-conn.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("MediaStatus response = %s", payload)
		}
	case <-time.After(200 * time.Millisecond):
		close(media.release)
		<-done
		t.Fatal("MediaStatus response waited for media cleanup")
	}
	close(media.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MediaStatus handler did not finish after media cleanup")
	}
}

func TestMediaStatusCommitsStreamOnlyAfterSuccessfulSIPOK(t *testing.T) {
	body := []byte(`<Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-write-failure"}
	streams.Store(key, stream)
	api := &GB28181API{streams: streams}
	setMediaStatusTestVersion(t, api, GBVersion11)
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("MediaStatus SIP OK write failed"),
	}
	request := newFlowRequest(t, base, sip.MethodMessage, stream.CallID, body)
	request.SetConnection(connection)
	done := make(chan struct{})
	go func() {
		api.sipMessageMediaStatus(&sip.Context{
			Request: request, Tx: sip.NewTransaction("media-status-write-failure-tx", connection),
			DeviceID: gb10DeviceID, Source: base.remote,
		})
		close(done)
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("MediaStatus SIP OK write did not start")
	}
	if current, ok := streams.Load(key); !ok || current != stream || stream.Stop {
		close(connection.release)
		t.Fatalf("MediaStatus committed before SIP OK completed: present=%v stop=%v", ok, stream.Stop)
	}
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MediaStatus handler did not return after SIP OK write failure")
	}
	if current, ok := streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatalf("MediaStatus committed after SIP OK write failure: present=%v stop=%v", ok, stream.Stop)
	}

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, stream.CallID, body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := streams.Load(key); ok || !stream.Stop || stream.EndReason != "media_status" {
		t.Fatalf("retried MediaStatus did not commit: present=%v stop=%v reason=%q", ok, stream.Stop, stream.EndReason)
	}
}

func TestMediaStatusSignalsDirectDownloadOnlyAfterSuccessfulSIPOK(t *testing.T) {
	releaseSender := make(chan struct{})
	address := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("direct media"))
		<-releaseSender
	})
	defer close(releaseSender)
	manager := newTestDirectTCPManager(t)
	if err := manager.Start(context.Background(), DirectTCPDownloadRequest{
		SessionID:    "direct-media-status-write-failure",
		DeviceID:     gb10DeviceID,
		ChannelID:    gb10ChannelID,
		Address:      address,
		RegisteredIP: net.ParseIP("127.0.0.1"),
	}); err != nil {
		t.Fatal(err)
	}
	waitDirectTCPBytes(t, manager, "direct-media-status-write-failure")
	api := &GB28181API{directDownloads: manager}
	setMediaStatusTestVersion(t, api, GBVersion11)
	body := []byte(`<Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10DeviceID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("direct MediaStatus SIP OK write failed"),
	}
	request := newFlowRequest(t, base, sip.MethodMessage, "direct-media-status-write-failure", body)
	request.SetConnection(connection)
	done := make(chan struct{})
	go func() {
		api.sipMessageMediaStatus(&sip.Context{
			Request: request, Tx: sip.NewTransaction("direct-media-status-write-failure-tx", connection),
			DeviceID: gb10DeviceID, Source: base.remote,
		})
		close(done)
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("direct MediaStatus SIP OK write did not start")
	}
	manager.mu.RLock()
	session := manager.active["direct-media-status-write-failure"]
	manager.mu.RUnlock()
	if session == nil {
		close(connection.release)
		t.Fatal("direct download ended before SIP OK completed")
	}
	if session.wasSenderFinished() {
		close(connection.release)
		t.Fatal("direct download was signalled before SIP OK completed")
	}
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("direct MediaStatus handler did not return after SIP OK write failure")
	}
	if session.wasSenderFinished() {
		t.Fatal("direct download was signalled after SIP OK write failure")
	}

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "direct-media-status-write-failure", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	state := waitDirectTCPState(t, manager, "direct-media-status-write-failure")
	if state.Status != directTCPStatusCompleted || state.EndReason != "media_status" {
		t.Fatalf("retried direct MediaStatus state = %+v", state)
	}
}

func TestMediaStatus11FinishesHistoryIdempotently(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "media-status-notify.xml"))
	if err != nil {
		t.Fatal(err)
	}
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{
		DeviceID:  gb10DeviceID,
		ChannelID: gb10ChannelID,
		CallID:    "media-status-1",
	}
	streams.Store(key, stream)
	api := &GB28181API{streams: streams}
	setMediaStatusTestVersion(t, api, GBVersion11)
	conn := newFlowConnection()

	response := runFlowHandler(t, conn, api, sip.MethodMessage, "media-status-1", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := streams.Load(key); ok {
		t.Fatal("MediaStatus did not remove history session")
	}
	if stream.Status != 1 || !stream.Stop {
		t.Fatalf("MediaStatus stream state = status:%d stop:%v", stream.Status, stream.Stop)
	}

	response = runFlowHandler(t, conn, api, sip.MethodMessage, "media-status-1", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
}

func TestMediaStatus2011StopsHistorySession(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-2011"}
	streams.Store(key, stream)
	api, _ := newVersionGateAPI(GBVersion10)
	api.streams = streams
	body := []byte(`<Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, stream.CallID, body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := streams.Load(key); ok || !stream.Stop || stream.EndReason != "media_status" {
		t.Fatal("2011 MediaStatus did not stop history session")
	}
}

func TestMediaStatus2011ProductionMessageRoute(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion10)
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-2011-route"}
	streams.Store(key, stream)

	api := &GB28181API{streams: streams}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	sipServer.Message(api.sipAccessControlMiddleware, api.sipCascadeMessageMiddleware).Handle("MediaStatus", api.sipMessageMediaStatus)

	serverConn, clientConn := net.Pipe()
	go sipServer.ProcessTCPConnection(sip.NewTCPConnection(serverConn))
	t.Cleanup(func() {
		_ = clientConn.Close()
		sipServer.Close()
	})
	body := `<Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`
	request := fmt.Sprintf("MESSAGE sip:%s@3402000000 SIP/2.0\r\nVia: SIP/2.0/TCP 192.0.2.10:5060;branch=z9hG4bK-media-status-2011-route\r\nFrom: <sip:%s@3402000000>;tag=media-status-2011-route\r\nTo: <sip:%s@3402000000>\r\nCall-ID: %s\r\nCSeq: 1 MESSAGE\r\nMax-Forwards: 70\r\nX-GB-Ver: 1.0\r\nContent-Type: Application/MANSCDP+xml\r\nContent-Length: %d\r\n\r\n%s",
		gb10PlatformID, gb10DeviceID, gb10PlatformID, stream.CallID, len(body), body)
	if err := clientConn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := clientConn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4096)
	n, err := clientConn.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	assertFlowOK(t, string(response[:n]))
	// 200 OK 写出后 handler 才提交本地终态；关闭服务会等待当前请求完成。
	sipServer.Close()
	if _, ok := streams.Load(key); ok || !stream.Stop || stream.EndReason != "media_status" {
		t.Fatal("2011 MediaStatus did not pass the production MESSAGE route")
	}
}

func TestMediaStatusEndsThirdPartyDeviceDialog(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "media-status-device-bye")
	setMediaStatusTestVersion(t, fixture.api, GBVersion11)
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	fixture.api.streams.Store(key, &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "media-status-device-bye",
		CallID: "media-status-device-bye", Resp: fixture.response,
	})
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)

	response := runFlowHandler(t, newFlowConnection(), fixture.api, sip.MethodMessage, "media-status-device-bye", body, fixture.api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	select {
	case payload := <-fixture.flow.writes:
		bye := string(payload)
		if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: media-status-device-bye") || !strings.Contains(bye, "CSeq: 2 BYE") {
			t.Fatalf("MediaStatus device BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("MediaStatus did not end third-party device dialog")
	}
}

func TestMediaStatus11SignalsDirectTCPDownload(t *testing.T) {
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("direct media"))
		<-release
	})
	defer close(release)
	manager := newTestDirectTCPManager(t)
	if err := manager.Start(context.Background(), DirectTCPDownloadRequest{
		SessionID:    "direct-media-status",
		DeviceID:     gb10DeviceID,
		ChannelID:    gb10ChannelID,
		Address:      address,
		RegisteredIP: net.ParseIP("127.0.0.1"),
	}); err != nil {
		t.Fatal(err)
	}
	waitDirectTCPBytes(t, manager, "direct-media-status")
	api := &GB28181API{directDownloads: manager}
	setMediaStatusTestVersion(t, api, GBVersion11)
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10DeviceID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "direct-media-status", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	state := waitDirectTCPState(t, manager, "direct-media-status")
	if state.Status != directTCPStatusCompleted || state.EndReason != "media_status" {
		t.Fatalf("direct MediaStatus state = %+v", state)
	}
}

func TestMediaStatus11UnknownNotifyTypeDoesNotStop(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	streams.Store(key, &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "media-status-2"})
	api := &GB28181API{streams: streams}
	setMediaStatusTestVersion(t, api, GBVersion11)
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>999</NotifyType></Notify>`)

	response := runFlowHandler(t, conn, api, sip.MethodMessage, "media-status-2", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := streams.Load(key); !ok {
		t.Fatal("unknown MediaStatus type stopped the session")
	}
}

func TestMediaStatusCannotStopAnotherDevicesHistorySession(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "shared-call-id"}
	streams.Store(key, stream)
	api := &GB28181API{streams: streams}
	setMediaStatusTestVersion(t, api, GBVersion11)
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	request := newFlowRequest(t, conn, sip.MethodMessage, "shared-call-id", body)
	api.sipMessageMediaStatus(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cross-device-media-status", conn), DeviceID: "34020000001320009999", Source: conn.remote,
	})
	select {
	case response := <-conn.writes:
		if !strings.Contains(string(response), "SIP/2.0 400") {
			t.Fatalf("cross-device MediaStatus response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("MediaStatus response timeout")
	}
	if current, ok := streams.Load(key); !ok || current != stream || stream.Stop {
		t.Fatal("cross-device MediaStatus stopped another device's history session")
	}
}

func TestMediaStatusForwardsCascadeHistoryDialog(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID) + ":cascade:test"
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "downstream-history", sessionKey: key}
	streams.Store(key, stream)
	server := &Server{}
	api := &GB28181API{streams: streams, svr: server}
	server.gb = api
	setMediaStatusTestVersion(t, api, GBVersion10)
	worker, dialog := newMediaStatusCascadeDialog(t, server, stream, "upstream-history", testExposedChannelID)
	worker.mu.Lock()
	worker.effective = GBVersion10
	worker.mu.Unlock()
	api.inviteDialogs.Store(dialog.CallID, dialog)
	var forwarded *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		forwarded = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Notify><CmdType>MediaStatus</CmdType><SN>41</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "downstream-history", body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if forwarded == nil || forwarded.Method() != sip.MethodMessage {
		t.Fatalf("forwarded MediaStatus request = %v", forwarded)
	}
	via, ok := forwarded.ViaHop()
	if !ok || via == nil || !strings.EqualFold(via.ProtocolName, "SIP") || via.ProtocolVersion != "2.0" ||
		strings.TrimSpace(via.Transport) == "" || strings.TrimSpace(via.Host) == "" || via.Params == nil {
		t.Fatalf("forwarded MediaStatus Via = %#v", forwarded.GetHeaders("Via"))
	}
	callID, _ := forwarded.CallID()
	cseq, _ := forwarded.CSeq()
	if callID == nil || normalizeCallID(callID) != dialog.CallID || cseq == nil || cseq.SeqNo != 2 || cseq.MethodName != sip.MethodMessage {
		t.Fatalf("forwarded MediaStatus dialog = call-id %v, cseq %+v", callID, cseq)
	}
	if headers := forwarded.GetHeaders("X-GB-Ver"); len(headers) != 1 || headers[0].String() != "X-GB-Ver: 1.0" {
		t.Fatalf("forwarded 2011 MediaStatus X-GB-Ver = %v", headers)
	}
	var got MediaStatusNotify
	if err := sip.XMLDecode(forwarded.Body(), &got); err != nil {
		t.Fatal(err)
	}
	if got.CmdType != "MediaStatus" || got.SN <= 0 || got.SN == 41 || got.DeviceID != testExposedChannelID || got.NotifyType != mediaStatusHistoryFinished {
		t.Fatalf("forwarded MediaStatus body = %+v", got)
	}
	if _, ok := streams.Load(key); ok || !stream.Stop || stream.EndReason != "media_status" {
		t.Fatalf("cascade MediaStatus stream state = present %v, stop %v, reason %q", ok, stream.Stop, stream.EndReason)
	}
}

func TestCascadeMediaStatusValidatesMappingBeforeAllocatingDialogCSeq(t *testing.T) {
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "downstream-media-status-missing-map",
	}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker, dialog := newMediaStatusCascadeDialog(t, server, stream, "upstream-media-status-missing-map", testExposedChannelID)
	delete(worker.platform.channelIDMap, stream.ChannelID)
	delete(worker.platform.exposedChannelMap, testExposedChannelID)
	initialCSeq := dialog.LocalCSeq
	exchangeCalls := 0
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		exchangeCalls++
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	status, reason := api.forwardCascadeMediaStatusDialog(dialog, dialog.Cascade, MediaStatusNotify{
		CmdType: "MediaStatus", NotifyType: mediaStatusHistoryFinished,
	})
	if status != http.StatusBadGateway || !strings.Contains(reason, "mapping") {
		t.Fatalf("missing MediaStatus mapping response = %d %q", status, reason)
	}
	if dialog.LocalCSeq != initialCSeq {
		t.Fatalf("missing MediaStatus mapping consumed dialog CSeq: got %d, want %d", dialog.LocalCSeq, initialCSeq)
	}
	if exchangeCalls != 0 {
		t.Fatalf("missing MediaStatus mapping exchange calls = %d, want 0", exchangeCalls)
	}
}

func TestCascadeMediaStatusLocalFailureDoesNotConsumeDialogCSeq(t *testing.T) {
	newFixture := func(t *testing.T, callID string) (*GB28181API, *cascadeWorker, *inboundInviteDialog) {
		t.Helper()
		stream := &Streams{
			DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "downstream-" + callID,
		}
		server := &Server{}
		api := &GB28181API{svr: server}
		server.gb = api
		worker, dialog := newMediaStatusCascadeDialog(t, server, stream, callID, testExposedChannelID)
		return api, worker, dialog
	}

	t.Run("invalid dialog", func(t *testing.T) {
		api, worker, dialog := newFixture(t, "upstream-media-status-invalid-dialog")
		dialog.Response.RemoveHeader("From")
		initialCSeq := dialog.LocalCSeq
		exchangeCalls := 0
		worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
			exchangeCalls++
			return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
		}

		status, reason := api.forwardCascadeMediaStatusDialog(dialog, dialog.Cascade, MediaStatusNotify{
			CmdType: "MediaStatus", NotifyType: mediaStatusHistoryFinished,
		})
		if status != http.StatusBadGateway || !strings.Contains(reason, "build") {
			t.Fatalf("invalid MediaStatus dialog response = %d %q", status, reason)
		}
		if dialog.LocalCSeq != initialCSeq {
			t.Fatalf("invalid MediaStatus dialog consumed CSeq: got %d, want %d", dialog.LocalCSeq, initialCSeq)
		}
		if exchangeCalls != 0 {
			t.Fatalf("invalid MediaStatus dialog exchange calls = %d, want 0", exchangeCalls)
		}
	})

	t.Run("identity forwarding", func(t *testing.T) {
		api, worker, dialog := newFixture(t, "upstream-media-status-invalid-identity")
		policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
		if err != nil {
			t.Fatal(err)
		}
		worker.platform.monitorUserIdentity = policy
		dialog.Cascade.identityCtx = withMonitorUserIdentity(context.Background(), &monitorUserIdentity{
			Gateways:     []string{testTrustedGatewayID},
			UserID:       testRemoteUserID,
			Organization: "remoteorg",
			Category:     "dispatcher",
			Rank:         "level2",
		})
		initialCSeq := dialog.LocalCSeq
		exchangeCalls := 0
		worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
			exchangeCalls++
			return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
		}

		status, reason := api.forwardCascadeMediaStatusDialog(dialog, dialog.Cascade, MediaStatusNotify{
			CmdType: "MediaStatus", NotifyType: mediaStatusHistoryFinished,
		})
		if status != http.StatusForbidden || !strings.Contains(reason, "identity") {
			t.Fatalf("invalid MediaStatus identity response = %d %q", status, reason)
		}
		if dialog.LocalCSeq != initialCSeq {
			t.Fatalf("invalid MediaStatus identity consumed CSeq: got %d, want %d", dialog.LocalCSeq, initialCSeq)
		}
		if exchangeCalls != 0 {
			t.Fatalf("invalid MediaStatus identity exchange calls = %d, want 0", exchangeCalls)
		}
	})
}

func TestCascadeMediaStatusRetriesDigestChallengeFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, challenge := range cascadeMessageDigestChallenges() {
			t.Run(string(version)+"/"+challenge.name, func(t *testing.T) {
				stream := &Streams{
					DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "downstream-media-status-digest",
					sessionKey: historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID) + ":cascade:digest",
				}
				server := &Server{}
				api := &GB28181API{svr: server}
				server.gb = api
				worker, dialog := newMediaStatusCascadeDialog(t, server, stream, "upstream-media-status-digest", testExposedChannelID)
				worker.mu.Lock()
				worker.effective = version
				worker.mu.Unlock()
				worker.platform.password = "cascade-secret"
				policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
				if err != nil {
					t.Fatal(err)
				}
				worker.platform.monitorUserIdentity = policy
				requests := make([]*sip.Request, 0, 2)
				worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
					cloned, ok := request.Clone().(*sip.Request)
					if !ok || cloned == nil {
						t.Fatal("clone cascade MediaStatus MESSAGE failed")
					}
					requests = append(requests, cloned)
					if len(requests) > 1 {
						return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
					}
					response := sip.NewResponseFromRequest("", request, challenge.status, challenge.reason, nil)
					response.AppendHeader(&sip.GenericHeader{
						HeaderName: challenge.challengeHeader,
						Contents:   fmt.Sprintf(`Digest realm="%s",nonce="%s",qop="auth"`, challenge.realm, challenge.nonce),
					})
					return response, nil
				}

				status, reason := api.forwardCascadeMediaStatusDialog(dialog, dialog.Cascade, MediaStatusNotify{CmdType: "MediaStatus", NotifyType: mediaStatusHistoryFinished})
				if status != http.StatusOK || reason != "OK" {
					t.Fatalf("Digest challenged cascade MediaStatus = %d %s", status, reason)
				}
				assertCascadeMessageDigestRetry(t, requests, challenge, string(version))
				if !dialog.Cascade.mediaStatusForwarded {
					t.Fatal("authenticated cascade MediaStatus was not committed")
				}
				if dialog.LocalCSeq != 3 {
					t.Fatalf("authenticated cascade MediaStatus dialog CSeq = %d, want 3", dialog.LocalCSeq)
				}
			})
		}
	}
}

func TestMediaStatusRetriesOnlyFailedCascadeDialogs(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID) + ":cascade:shared"
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "downstream-shared", sessionKey: key}
	streams.Store(key, stream)
	server := &Server{}
	api := &GB28181API{streams: streams, svr: server}
	server.gb = api
	setMediaStatusTestVersion(t, api, GBVersion11)
	firstWorker, firstDialog := newMediaStatusCascadeDialog(t, server, stream, "upstream-first", testExposedChannelID)
	secondWorker, secondDialog := newMediaStatusCascadeDialog(t, server, stream, "upstream-second", "44010000001320000911")
	api.inviteDialogs.Store(firstDialog.CallID, firstDialog)
	api.inviteDialogs.Store(secondDialog.CallID, secondDialog)
	firstCalls, secondCalls := 0, 0
	firstWorker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		firstCalls++
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	secondWorker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		secondCalls++
		status := http.StatusServiceUnavailable
		if secondCalls > 1 {
			status = http.StatusOK
		}
		return sip.NewResponseFromRequest("", request, status, http.StatusText(status), nil), nil
	}
	body := []byte(`<Notify><CmdType>MediaStatus</CmdType><SN>42</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)

	firstResponse := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "downstream-shared", body, api.sipMessageMediaStatus)
	if !strings.Contains(firstResponse, "SIP/2.0 503") {
		t.Fatalf("first cascade MediaStatus response = %s", firstResponse)
	}
	if _, ok := streams.Load(key); !ok || stream.Stop || firstCalls != 1 || secondCalls != 1 {
		t.Fatalf("failed cascade MediaStatus state = present %v, stop %v, calls %d/%d", ok, stream.Stop, firstCalls, secondCalls)
	}

	secondResponse := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "downstream-shared", body, api.sipMessageMediaStatus)
	assertFlowOK(t, secondResponse)
	if _, ok := streams.Load(key); ok || !stream.Stop || firstCalls != 1 || secondCalls != 2 {
		t.Fatalf("retried cascade MediaStatus state = present %v, stop %v, calls %d/%d", ok, stream.Stop, firstCalls, secondCalls)
	}
}

func TestMediaStatusRetainsLocalStreamWhenSIPOKFailsAfterCascadeForward(t *testing.T) {
	streams := &conc.Map[string, *Streams]{}
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID) + ":cascade:ack-failure"
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "downstream-ack-failure", sessionKey: key}
	streams.Store(key, stream)
	server := &Server{}
	api := &GB28181API{streams: streams, svr: server}
	server.gb = api
	setMediaStatusTestVersion(t, api, GBVersion11)
	worker, dialog := newMediaStatusCascadeDialog(t, server, stream, "upstream-ack-failure", testExposedChannelID)
	api.inviteDialogs.Store(dialog.CallID, dialog)
	forwardCalls := 0
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		forwardCalls++
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	body := []byte(`<Notify><CmdType>MediaStatus</CmdType><SN>44</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)
	base := newFlowConnection()
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("cascade MediaStatus SIP OK write failed"),
	}
	request := newFlowRequest(t, base, sip.MethodMessage, stream.CallID, body)
	request.SetConnection(connection)
	done := make(chan struct{})
	go func() {
		api.sipMessageMediaStatus(&sip.Context{
			Request: request, Tx: sip.NewTransaction("cascade-media-status-ack-failure-tx", connection),
			DeviceID: gb10DeviceID, Source: base.remote,
		})
		close(done)
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("cascade MediaStatus SIP OK write did not start")
	}
	if current, ok := streams.Load(key); !ok || current != stream || stream.Stop || forwardCalls != 1 {
		close(connection.release)
		t.Fatalf("cascade state before SIP OK = present:%v stop:%v forwards:%d", ok, stream.Stop, forwardCalls)
	}
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cascade MediaStatus handler did not return after SIP OK write failure")
	}
	if current, ok := streams.Load(key); !ok || current != stream || stream.Stop || forwardCalls != 1 {
		t.Fatalf("cascade state after SIP OK failure = present:%v stop:%v forwards:%d", ok, stream.Stop, forwardCalls)
	}

	response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, stream.CallID, body, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := streams.Load(key); ok || !stream.Stop || forwardCalls != 1 {
		t.Fatalf("retried cascade state = present:%v stop:%v forwards:%d", ok, stream.Stop, forwardCalls)
	}
}

func TestCascadeMediaStatusWaitsForAllUpstreamBYEsBeforeDeviceBYE(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "downstream-cascade-media-status")
	setMediaStatusTestVersion(t, fixture.api, GBVersion11)
	key := historyKey(historyModePlayback, gb10DeviceID, gb10ChannelID) + ":cascade:terminal"
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "cascade-media-status-source",
		CallID: "downstream-cascade-media-status", sessionKey: key, Resp: fixture.response,
	}
	fixture.api.streams.Store(key, stream)
	source := &cascadeSourceRef{
		key: key, refs: 2, owned: true, stopDone: make(chan struct{}),
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, stream: stream, mode: historyModePlayback,
	}
	fixture.api.cascadeSources = map[string]*cascadeSourceRef{key: source}
	firstWorker, firstDialog := newMediaStatusCascadeDialog(t, fixture.server, stream, "upstream-media-status-first", testExposedChannelID)
	secondWorker, secondDialog := newMediaStatusCascadeDialog(t, fixture.server, stream, "upstream-media-status-second", "44010000001320000911")
	firstDialog.Cascade.source = source
	secondDialog.Cascade.source = source
	firstWorker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	secondWorker.exchange = firstWorker.exchange
	fixture.api.inviteDialogs.Store(firstDialog.CallID, firstDialog)
	fixture.api.inviteDialogs.Store(secondDialog.CallID, secondDialog)
	body := []byte(`<Notify><CmdType>MediaStatus</CmdType><SN>43</SN><DeviceID>` + gb10ChannelID + `</DeviceID><NotifyType>121</NotifyType></Notify>`)

	response := runFlowHandler(t, newFlowConnection(), fixture.api, sip.MethodMessage, stream.CallID, body, fixture.api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if current, ok := fixture.api.streams.Load(key); !ok || current != stream || !fixture.api.mediaStreamStopping(stream) {
		t.Fatal("cascade MediaStatus did not retain terminal stream ownership until upstream BYEs")
	}
	fixture.api.cascadeMediaMu.Lock()
	ended, mediaStatusFinished, refs := source.ended, source.mediaStatusFinished, source.refs
	fixture.api.cascadeMediaMu.Unlock()
	if !ended || !mediaStatusFinished || refs != 2 || fixture.api.cascadeSourceUsable(source) {
		t.Fatalf("cascade source terminal state = ended:%v media_status:%v refs:%d usable:%v", ended, mediaStatusFinished, refs, fixture.api.cascadeSourceUsable(source))
	}
	select {
	case payload := <-fixture.flow.writes:
		t.Fatalf("MediaStatus ended downstream dialog before upstream BYE: %s", payload)
	case <-time.After(50 * time.Millisecond):
	}

	fixture.api.inviteDialogs.CompareAndDelete(firstDialog.CallID, firstDialog)
	fixture.api.stopCascadeMediaSession(firstDialog.Cascade, false, false)
	select {
	case payload := <-fixture.flow.writes:
		t.Fatalf("first of two upstream BYEs ended shared downstream dialog: %s", payload)
	case <-time.After(50 * time.Millisecond):
	}

	fixture.api.inviteDialogs.CompareAndDelete(secondDialog.CallID, secondDialog)
	fixture.api.stopCascadeMediaSession(secondDialog.Cascade, false, false)
	select {
	case payload := <-fixture.flow.writes:
		bye := string(payload)
		if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+stream.CallID) {
			t.Fatalf("cascade MediaStatus downstream BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("last upstream BYE did not end MediaStatus downstream dialog")
	}
	if _, ok := fixture.api.cascadeSources[key]; ok {
		t.Fatal("cascade MediaStatus source remained after the last upstream BYE")
	}
	if _, ok := fixture.api.streams.Load(key); ok {
		t.Fatal("cascade MediaStatus stream remained after downstream BYE cleanup")
	}
}

func TestCascadeMediaStatusDoesNotEndConcurrentReplacementSource(t *testing.T) {
	ended := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "reused-cascade-stream-id"}
	replacementStream := &Streams{DeviceID: ended.DeviceID, ChannelID: ended.ChannelID, StreamID: ended.StreamID}
	replacement := &cascadeSourceRef{key: "replacement", refs: 1, stream: replacementStream}
	api := &GB28181API{cascadeSources: map[string]*cascadeSourceRef{replacement.key: replacement}}

	api.markCascadeSourcesMediaStatusFinished(ended)
	api.cascadeMediaMu.Lock()
	replacementEnded, replacementFinished := replacement.ended, replacement.mediaStatusFinished
	api.cascadeMediaMu.Unlock()
	if replacementEnded || replacementFinished {
		t.Fatalf("MediaStatus marked concurrent replacement source: ended=%v media_status=%v", replacementEnded, replacementFinished)
	}
}

func TestCascadeMediaStatusCleanerKeepsGracePeriod(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "cascade-media-status-grace-downstream")
	now := time.Now()
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "cascade-media-status-grace-source",
		CallID: "cascade-media-status-grace-downstream", Resp: fixture.response,
	}
	source := &cascadeSourceRef{
		key: "cascade-media-status-grace", refs: 1, owned: true, ended: true, mediaStatusFinished: true,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, stream: stream, mode: historyModePlayback,
	}
	_, dialog := newMediaStatusCascadeDialog(t, fixture.server, stream, "cascade-media-status-grace-upstream", testExposedChannelID)
	dialog.Cascade.source = source
	dialog.UpdatedAt = now.Add(-mediaStatusCascadeDialogGraceTTL + time.Second)
	fixture.api.cascadeSources = map[string]*cascadeSourceRef{source.key: source}
	fixture.api.inviteDialogs.Store(dialog.CallID, dialog)

	fixture.api.cleanupInviteDialogs(now)

	if actual, ok := fixture.api.inviteDialogs.Load(dialog.CallID); !ok || actual != dialog {
		t.Fatal("MediaStatus cascade dialog was removed during its grace period")
	}
	fixture.api.cascadeMediaMu.Lock()
	refs, current := source.refs, fixture.api.cascadeSources[source.key]
	fixture.api.cascadeMediaMu.Unlock()
	if refs != 1 || current != source {
		t.Fatalf("MediaStatus cascade source changed during grace period: refs=%d current=%p", refs, current)
	}
	if dialog.LocalCSeq != 1 {
		t.Fatalf("MediaStatus grace-period dialog sent upstream BYE: CSeq=%d", dialog.LocalCSeq)
	}
	select {
	case payload := <-fixture.flow.writes:
		t.Fatalf("MediaStatus grace-period dialog sent downstream BYE: %s", payload)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCascadeMediaStatusCleanerExpiresAllUpstreamsOnce(t *testing.T) {
	fixture := newSignalDigestDialogFixture(t, "cascade-media-status-cleaner-downstream")
	now := time.Now()
	stream := &Streams{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "cascade-media-status-cleaner-source",
		CallID: "cascade-media-status-cleaner-downstream", Resp: fixture.response,
	}
	source := &cascadeSourceRef{
		key: "cascade-media-status-cleaner", refs: 2, owned: true, ended: true, mediaStatusFinished: true,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, stream: stream, mode: historyModePlayback,
	}
	_, first := newMediaStatusCascadeDialog(t, fixture.server, stream, "cascade-media-status-cleaner-first", testExposedChannelID)
	_, second := newMediaStatusCascadeDialog(t, fixture.server, stream, "cascade-media-status-cleaner-second", "44010000001320000911")
	for _, dialog := range []*inboundInviteDialog{first, second} {
		dialog.Cascade.source = source
		dialog.UpdatedAt = now.Add(-mediaStatusCascadeDialogGraceTTL - time.Second)
		fixture.api.inviteDialogs.Store(dialog.CallID, dialog)
	}
	fixture.api.cascadeSources = map[string]*cascadeSourceRef{source.key: source}

	fixture.api.cleanupInviteDialogs(now)

	for _, dialog := range []*inboundInviteDialog{first, second} {
		if _, ok := fixture.api.inviteDialogs.Load(dialog.CallID); ok {
			t.Fatalf("expired MediaStatus cascade dialog %s was retained", dialog.CallID)
		}
		if dialog.LocalCSeq != 2 {
			t.Fatalf("expired MediaStatus cascade dialog %s did not attempt upstream BYE: CSeq=%d", dialog.CallID, dialog.LocalCSeq)
		}
	}
	fixture.api.cascadeMediaMu.Lock()
	refs, stopping := source.refs, source.stopping
	_, current := fixture.api.cascadeSources[source.key]
	fixture.api.cascadeMediaMu.Unlock()
	if refs != 0 || !stopping || current {
		t.Fatalf("expired MediaStatus cascade source = refs:%d stopping:%v current:%v", refs, stopping, current)
	}
	select {
	case payload := <-fixture.flow.writes:
		bye := string(payload)
		if !strings.HasPrefix(bye, "BYE ") || !strings.Contains(bye, "Call-ID: "+stream.CallID) {
			t.Fatalf("MediaStatus cleaner downstream BYE:\n%s", bye)
		}
	case <-time.After(time.Second):
		t.Fatal("MediaStatus cleaner did not end the downstream dialog")
	}
	select {
	case payload := <-fixture.flow.writes:
		t.Fatalf("MediaStatus cleaner sent duplicate downstream BYE: %s", payload)
	case <-time.After(50 * time.Millisecond):
	}
}

func newMediaStatusCascadeDialog(t *testing.T, server *Server, stream *Streams, callID, exposedID string) (*cascadeWorker, *inboundInviteDialog) {
	t.Helper()
	platform := testSharedCascadePlatform(t)
	platform.channelIDMap[stream.ChannelID] = exposedID
	platform.exposedChannelMap[exposedID] = stream.ChannelID
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	remote := mustFlowAddress(t, "sip:"+worker.platform.serverID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+exposedID+"@local.example")
	contact := mustFlowAddress(t, "sip:"+worker.platform.serverID+"@192.0.2.30:5060")
	remote.Params.Add("tag", sip.String{Str: "remote-" + callID})
	requestCallID := sip.CallID(callID)
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion, sip.NewHeaderBuilder().
		SetFrom(remote).SetTo(local).SetContact(contact).SetMethod(sip.MethodInvite).SetSeqNo(1).SetCallID(&requestCallID).
		AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "UDP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	request.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	mode := historyModePlayback
	if strings.Contains(stream.sessionKey, "history:"+historyModeDownload+":") {
		mode = historyModeDownload
	}
	session := &cascadeMediaSession{
		worker: worker, source: &cascadeSourceRef{stream: stream, mode: mode}, identityCtx: context.Background(),
	}
	return worker, &inboundInviteDialog{
		CallID: callID, DeviceID: worker.platform.serverID, LocalCSeq: 1, Request: request, Response: response,
		Established: true, Cascade: session,
	}
}
