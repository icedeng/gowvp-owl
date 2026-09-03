package gbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestBroadcastNotify11Fixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "broadcast-notify.xml"))
	if err != nil {
		t.Fatal(err)
	}
	var notify broadcastNotify
	if err := sip.XMLDecode(body, &notify); err != nil {
		t.Fatal(err)
	}
	if notify.CmdType != "Broadcast" || notify.SourceID != gb10PlatformID || notify.TargetID != gb10ChannelID {
		t.Fatalf("Broadcast notify = %+v", notify)
	}
}

func TestBroadcastResponse11ResolvesPending(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", "broadcast-response.xml"))
	if err != nil {
		t.Fatal(err)
	}
	pending := &pendingBroadcastResponse{wait: make(chan *broadcastResponse, 1)}
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	api.pendingBroadcast.Store(buildPendingBroadcastKey(gb10ChannelID, 60), pending)
	conn := newFlowConnection()

	response := runFlowHandler(t, conn, api, sip.MethodMessage, "broadcast-1", body, api.sipMessageBroadcastResponse)
	assertFlowOK(t, response)
	select {
	case result := <-pending.wait:
		if result.Result != "OK" || result.DeviceID != gb10ChannelID {
			t.Fatalf("Broadcast response = %+v", result)
		}
	default:
		t.Fatal("Broadcast response did not resolve pending request")
	}
}

func TestBroadcastResponseRejectsInvalidEnvelopeBeforeWait(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	pending := &pendingBroadcastResponse{wait: make(chan *broadcastResponse, 1)}
	api.pendingBroadcast.Store(buildPendingBroadcastKey(gb10ChannelID, 60), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Notify><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Notify>`},
		{name: "wrong command", body: `<Response><CmdType>DeviceControl</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "non-positive SN", body: `<Response><CmdType>Broadcast</CmdType><SN>0</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "invalid result", body: `<Response><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>SUCCESS</Result></Response>`},
		{name: "unknown target", body: `<Response><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>34020000001320000009</DeviceID><Result>OK</Result></Response>`},
		{name: "duplicate result", body: `<Response><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><Result>ERROR</Result></Response>`},
		{name: "unknown field", body: `<Response><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result><Vendor>1</Vendor></Response>`},
		{name: "root attribute", body: `<Response vendor="x"><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "element attribute", body: `<Response><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result vendor="x">OK</Result></Response>`},
		{name: "nested result", body: `<Response><CmdType>Broadcast</CmdType><SN>60</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result><Value>OK</Value></Result></Response>`},
		{name: "out of order", body: `<Response><CmdType>Broadcast</CmdType><DeviceID>` + gb10ChannelID + `</DeviceID><SN>60</SN><Result>OK</Result></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "broadcast-invalid-"+test.name, []byte(test.body), api.sipMessageBroadcastResponse)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid Broadcast response = %s", response)
			}
		})
	}
	select {
	case result := <-pending.wait:
		t.Fatalf("invalid Broadcast resolved pending request: %+v", result)
	default:
	}
}

func TestBroadcastResponseCompletesOnlyAfterSuccessfulSIPOK(t *testing.T) {
	tests := []struct {
		name     string
		writeErr error
		complete bool
	}{
		{name: "success", complete: true},
		{name: "write failure", writeErr: errors.New("write failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.setGBVersion(GBVersion11)
			memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
			api := &GB28181API{svr: &Server{memoryStorer: memory}}
			pending := &pendingBroadcastResponse{wait: make(chan *broadcastResponse, 1)}
			api.pendingBroadcast.Store(buildPendingBroadcastKey(gb10ChannelID, 63), pending)
			body := []byte(`<Response><CmdType>Broadcast</CmdType><SN>63</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>OK</Result></Response>`)
			conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "broadcast-commit-"+test.name, body, api.sipMessageBroadcastResponse, test.writeErr)
			completedBeforeSIP := false
			select {
			case <-pending.wait:
				completedBeforeSIP = true
			default:
			}
			finishBlockingFlowHandler(t, conn, done)
			if completedBeforeSIP {
				t.Fatal("Broadcast response completed before SIP 200 was written")
			}
			completed := false
			select {
			case <-pending.wait:
				completed = true
			default:
			}
			if completed != test.complete {
				t.Fatalf("Broadcast completed = %v, want %v", completed, test.complete)
			}
		})
	}
}

func TestStartVoiceBroadcastFirstBusinessResponseWinsAndCleansRejectedSession(t *testing.T) {
	api, input, peer := newBroadcastStartHarness(t)
	result := make(chan error, 1)
	go func() { result <- api.StartVoice(context.Background(), input) }()

	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	request, err := readAnnexGTestSIPFrame(bufio.NewReader(peer))
	if err != nil {
		t.Fatal(err)
	}
	var notify broadcastNotify
	if err = sip.XMLDecode(cascadeDownstreamSIPBody(request), &notify); err != nil {
		t.Fatal(err)
	}
	if notify.CmdType != "Broadcast" || notify.TargetID != gb10ChannelID || notify.SN <= 0 {
		t.Fatalf("outbound Broadcast notify = %+v", notify)
	}

	// 业务响应可能在 SIP 200 前到达。首个 ERROR 必须获胜，紧随其后的重复 OK 不能翻转结果。
	for index, businessResult := range []string{"ERROR", "OK"} {
		body := []byte(`<?xml version="1.0"?><Response><CmdType>Broadcast</CmdType><SN>` +
			fmt.Sprintf("%d", notify.SN) + `</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Result>` + businessResult + `</Result></Response>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
			fmt.Sprintf("broadcast-public-response-%d", index), body, api.sipMessageBroadcastResponse)
		assertFlowOK(t, response)
	}
	if _, err = peer.Write([]byte(cancelledInviteTestResponse(request, 200, "OK"))); err != nil {
		t.Fatal(err)
	}

	select {
	case err = <-result:
		if err == nil || !strings.Contains(err.Error(), "broadcast rejected: ERROR") {
			t.Fatalf("StartVoice duplicate response result = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StartVoice did not return after rejected Broadcast response")
	}
	assertBroadcastStartFailureCleaned(t, api, buildPendingBroadcastKey(gb10ChannelID, notify.SN))
}

func TestStartVoiceBroadcastBusinessResponseTimeoutCleansSession(t *testing.T) {
	api, input, peer := newBroadcastStartHarness(t)
	input.Timeout = 40 * time.Millisecond
	result := make(chan error, 1)
	go func() { result <- api.StartVoice(context.Background(), input) }()

	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	request, err := readAnnexGTestSIPFrame(bufio.NewReader(peer))
	if err != nil {
		t.Fatal(err)
	}
	var notify broadcastNotify
	if err = sip.XMLDecode(cascadeDownstreamSIPBody(request), &notify); err != nil {
		t.Fatal(err)
	}
	if _, err = peer.Write([]byte(cancelledInviteTestResponse(request, 200, "OK"))); err != nil {
		t.Fatal(err)
	}

	select {
	case err = <-result:
		if err == nil || !strings.Contains(err.Error(), "wait Broadcast response timeout") {
			t.Fatalf("StartVoice Broadcast timeout result = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StartVoice did not return after Broadcast response timeout")
	}
	assertBroadcastStartFailureCleaned(t, api, buildPendingBroadcastKey(gb10ChannelID, notify.SN))
}

func TestOfflineCleanupCancelsStartingBroadcast(t *testing.T) {
	api, input, peer := newBroadcastStartHarness(t)
	result := make(chan error, 1)
	go func() { result <- api.StartVoice(context.Background(), input) }()

	_ = peer.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(peer)
	request, err := readAnnexGTestSIPFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	var notify broadcastNotify
	if err = sip.XMLDecode(cascadeDownstreamSIPBody(request), &notify); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	cleanupOfflineTestDevice(api)
	select {
	case err = <-result:
		if !errors.Is(err, ErrDeviceOffline) {
			t.Fatalf("StartVoice Broadcast error = %v; want %v", err, ErrDeviceOffline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("starting Broadcast did not stop after offline cleanup")
	}
	assertBroadcastStartFailureCleaned(t, api, buildPendingBroadcastKey(gb10ChannelID, notify.SN))
}

func newBroadcastStartHarness(t *testing.T) (*GB28181API, *VoiceInput, net.Conn) {
	t.Helper()
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	localRaw, peer := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go sipServer.ProcessTCPConnection(connection)

	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	if err := channel.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = connection.RemoteAddr()
		device.to = remote
	})

	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{{Tracks: []zlm.MediaTrack{{
		CodecType: 1, CodecIDName: "G711A", Ready: true,
	}}}}}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media,
		streams: &conc.Map[string, *Streams]{},
	}
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	t.Cleanup(func() {
		_ = peer.Close()
		server.Close()
	})
	return api, &VoiceInput{
		Channel: &ipc.Channel{ID: "broadcast-public-stream", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		SMS:     &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit}, Mode: voiceModeBroadcast,
		SourceStream: "microphone",
	}, peer
}

func assertBroadcastStartFailureCleaned(t *testing.T, api *GB28181API, pendingKey string) {
	t.Helper()
	if _, ok := api.pendingBroadcast.Load(pendingKey); ok {
		t.Fatal("failed Broadcast retained pending business response")
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("failed Broadcast retained session")
	}
	if _, ok := api.streams.Load(voiceKey(voiceModeBroadcast, gb10DeviceID, gb10ChannelID)); ok {
		t.Fatal("failed Broadcast retained stream")
	}
	media := api.sms.(*fakeRTPMediaService)
	media.mu.Lock()
	defer media.mu.Unlock()
	if media.startCalls != 0 || media.stopCalls != 0 {
		t.Fatalf("failed Broadcast touched RTP sender: starts=%d stops=%d", media.startCalls, media.stopCalls)
	}
}

func TestBroadcastReceiverInviteStartsAndStopsRTP(t *testing.T) {
	conn := newFlowConnection()
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	defer sipServer.Close()
	media := &fakeRTPMediaService{startPort: 30000}
	api := &GB28181API{
		cfg:     &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		sms:     media,
		streams: &conc.Map[string, *Streams]{},
	}
	api.svr = &Server{Server: sipServer, gb: api, fromAddress: *platform}
	prepareBroadcastInviteTestDevice(api, conn)
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{ID: "local", SDPIP: "192.0.2.20", Type: sms.ProtocolZLMediaKit}, Version: GBVersion11,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)

	body := []byte("v=0\r\n" +
		"o=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\n" +
		"s=Play\r\n" +
		"c=IN IP4 192.0.2.30\r\n" +
		"t=0 0\r\n" +
		"m=audio 8000 RTP/AVP 96\r\n" +
		"a=recvonly\r\n" +
		"a=rtpmap:96 PS/90000\r\n")
	response := runBroadcastInviteFlowHandler(t, conn, api, "broadcast-dialog", body, buildGBInviteSubject(gb10PlatformID, "voice-1", gb10ChannelID))
	assertFlowOK(t, response)
	for _, want := range []string{"m=audio 30000 RTP/AVP 96", "a=sendonly", "a=rtpmap:96 PS/90000", "f=v/////a/1/8/1"} {
		if !strings.Contains(response, want) {
			t.Fatalf("Broadcast response missing %q:\n%s", want, response)
		}
	}
	select {
	case err := <-session.ready:
		t.Fatalf("Broadcast completed before ACK: %v", err)
	default:
	}
	media.mu.Lock()
	start := media.started
	media.mu.Unlock()
	if start.DstURL != "192.0.2.30" || start.DstPort != 8000 || !start.IsUDP || start.Type != broadcastRTPTypePS || start.PT != 96 || !start.OnlyAudio {
		t.Fatalf("unexpected RTP start: %+v", start)
	}
	dialogValue, ok := api.inviteDialogs.Load("broadcast-dialog")
	if !ok {
		t.Fatal("Broadcast dialog not stored")
	}
	dialog := dialogValue.(*inboundInviteDialog)

	ack := newFlowRequest(t, conn, sip.MethodACK, "broadcast-dialog", nil)
	applyInboundDialogTags(t, ack, dialog, true)
	api.sipAckGeneric(&sip.Context{Request: ack, DeviceID: gb10DeviceID})
	select {
	case err := <-session.ready:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("Broadcast ACK did not establish waiting session")
	}
	bye := newFlowRequest(t, conn, sip.MethodBYE, "broadcast-dialog", nil)
	applyInboundDialogTags(t, bye, dialog, true)
	tx := sip.NewTransaction("broadcast-dialog-bye", conn)
	api.sipByeGeneric(&sip.Context{Request: bye, Tx: tx, DeviceID: gb10DeviceID, Source: conn.remote})
	response = string(<-flowResponse(t, conn))
	assertFlowOK(t, response)
	media.mu.Lock()
	stopped := media.stopped
	media.mu.Unlock()
	if stopped.SSRC == "" || stopped.Stream != "microphone" {
		t.Fatalf("unexpected RTP stop: %+v", stopped)
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("BYE did not remove Broadcast session")
	}
	if _, ok := api.inviteDialogs.Load("broadcast-dialog"); ok {
		t.Fatal("BYE did not remove Broadcast dialog")
	}
}

func TestBroadcastInviteRetainsFailedSenderCleanup(t *testing.T) {
	conn := newFlowConnection()
	stopErr := errors.New("stop Broadcast RTP while rejecting INVITE")
	media := &fakeRTPMediaService{startPort: 0, stopErr: stopErr}
	api := &GB28181API{
		cfg:     &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		sms:     media,
		streams: &conc.Map[string, *Streams]{},
	}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{ID: "local", SDPIP: "192.0.2.20", Type: sms.ProtocolZLMediaKit}, Version: GBVersion11,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	body := []byte("v=0\r\n" +
		"o=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\n" +
		"s=Play\r\n" +
		"c=IN IP4 192.0.2.30\r\n" +
		"t=0 0\r\n" +
		"m=audio 8000 RTP/AVP 96\r\n" +
		"a=recvonly\r\n" +
		"a=rtpmap:96 PS/90000\r\n")

	response := runBroadcastInviteFlowHandler(t, conn, api, "broadcast-invalid-media-port", body, buildGBInviteSubject(gb10PlatformID, "voice-1", gb10ChannelID))
	if !strings.Contains(response, "SIP/2.0 500") {
		t.Fatalf("invalid media port response = %s", response)
	}
	select {
	case err := <-session.ready:
		if err == nil || !strings.Contains(err.Error(), "invalid Broadcast RTP port") {
			t.Fatalf("Broadcast completion error = %v", err)
		}
	default:
		t.Fatal("failed Broadcast INVITE did not complete the waiter")
	}
	if current, ok := api.broadcastSessions.Load(gb10ChannelID); !ok || current != session {
		t.Fatal("failed RTP cleanup lost the Broadcast session owner")
	}
	session.mu.Lock()
	stopped, rtpStarted, inviteBusy, ssrc := session.stopped, session.rtpStarted, session.inviteBusy, session.SSRC
	session.mu.Unlock()
	if !stopped || !rtpStarted || inviteBusy || ssrc == "" {
		t.Fatalf("retained Broadcast state = stopped:%v rtp:%v invite_busy:%v ssrc:%q", stopped, rtpStarted, inviteBusy, ssrc)
	}
	media.mu.Lock()
	stoppedRequest := media.stopped
	media.stopErr = nil
	media.mu.Unlock()
	if stoppedRequest.SSRC != ssrc || stoppedRequest.Stream != session.SourceStream {
		t.Fatalf("failed RTP cleanup request = %+v", stoppedRequest)
	}

	if pending := api.cleanupStoppedVoiceSessions(); pending {
		t.Fatal("successful retry retained pending Broadcast cleanup")
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("successful retry retained Broadcast session")
	}
	session.mu.Lock()
	rtpStarted = session.rtpStarted
	session.mu.Unlock()
	if rtpStarted {
		t.Fatal("successful retry retained Broadcast RTP ownership")
	}
}

func TestBroadcastStopDuringRTPStartRetainsFailedSenderCleanup(t *testing.T) {
	conn := newFlowConnection()
	startEntered := make(chan struct{})
	startRelease := make(chan struct{})
	stopErr := errors.New("stop Broadcast RTP after concurrent termination")
	media := &fakeRTPMediaService{
		startPort: 30000, startEntered: startEntered, startRelease: startRelease, stopErr: stopErr,
	}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media,
		streams: &conc.Map[string, *Streams]{},
	}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	api.svr = &Server{gb: api, fromAddress: *platform}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{ID: "local", SDPIP: "192.0.2.20", Type: sms.ProtocolZLMediaKit}, Version: GBVersion11,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	prepareBroadcastInviteTestDevice(api, conn)
	body := []byte("v=0\r\n" +
		"o=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\n" +
		"s=Play\r\n" +
		"c=IN IP4 192.0.2.30\r\n" +
		"t=0 0\r\n" +
		"m=audio 8000 RTP/AVP 96\r\n" +
		"a=recvonly\r\n" +
		"a=rtpmap:96 PS/90000\r\n")
	request := newFlowRequest(t, conn, sip.MethodInvite, "broadcast-concurrent-stop", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(gb10PlatformID, "voice-1", gb10ChannelID)})
	to := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipInviteGeneric(&sip.Context{
			Request: request, Tx: sip.NewTransaction("broadcast-concurrent-stop-tx", conn),
			DeviceID: gb10DeviceID, Source: conn.remote,
			To: to, Log: slog.Default(),
		})
	}()
	select {
	case <-startEntered:
	case <-time.After(time.Second):
		close(startRelease)
		t.Fatal("Broadcast RTP start was not reached")
	}
	if err := api.stopBroadcastSession(session, false); err != nil {
		close(startRelease)
		t.Fatalf("stop Broadcast during RTP start: %v", err)
	}
	if current, ok := api.broadcastSessions.Load(gb10ChannelID); !ok || current != session {
		close(startRelease)
		t.Fatal("concurrent stop removed Broadcast session before RTP start returned")
	}
	close(startRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Broadcast INVITE did not finish after RTP start resumed")
	}
	select {
	case payload := <-conn.writes:
		if !strings.Contains(string(payload), "SIP/2.0 487") {
			t.Fatalf("concurrently stopped Broadcast response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrently stopped Broadcast response timeout")
	}
	if current, ok := api.broadcastSessions.Load(gb10ChannelID); !ok || current != session {
		t.Fatal("failed post-start RTP cleanup lost Broadcast session ownership")
	}
	session.mu.Lock()
	stopped, rtpStarted, inviteBusy, ssrc := session.stopped, session.rtpStarted, session.inviteBusy, session.SSRC
	session.mu.Unlock()
	if !stopped || !rtpStarted || inviteBusy || ssrc == "" {
		t.Fatalf("concurrent stop state = stopped:%v rtp:%v invite_busy:%v ssrc:%q", stopped, rtpStarted, inviteBusy, ssrc)
	}

	media.mu.Lock()
	media.stopErr = nil
	media.mu.Unlock()
	if pending := api.cleanupStoppedVoiceSessions(); pending {
		t.Fatal("successful concurrent-stop retry retained pending Broadcast cleanup")
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("successful concurrent-stop retry retained Broadcast session")
	}
}

func TestBroadcastStopBeforeACKRemovesDialogAndRTP(t *testing.T) {
	conn := newFlowConnection()
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	defer sipServer.Close()
	media := &fakeRTPMediaService{startPort: 30000}
	api := &GB28181API{
		cfg:     &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		sms:     media,
		streams: &conc.Map[string, *Streams]{},
	}
	api.svr = &Server{Server: sipServer, gb: api, fromAddress: *platform}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{ID: "local", SDPIP: "192.0.2.20", Type: sms.ProtocolZLMediaKit}, Version: GBVersion11,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	body := []byte("v=0\r\n" +
		"o=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\n" +
		"s=Play\r\n" +
		"c=IN IP4 192.0.2.30\r\n" +
		"t=0 0\r\n" +
		"m=audio 8000 RTP/AVP 96\r\n" +
		"a=recvonly\r\n" +
		"a=rtpmap:96 PS/90000\r\n")
	response := runBroadcastInviteFlowHandler(t, conn, api, "broadcast-no-ack", body, buildGBInviteSubject(gb10PlatformID, "voice-1", gb10ChannelID))
	assertFlowOK(t, response)

	if err := api.stopBroadcastSession(session, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := api.inviteDialogs.Load("broadcast-no-ack"); ok {
		t.Fatal("stopped Broadcast retained unacknowledged dialog")
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); ok {
		t.Fatal("stopped Broadcast retained session")
	}
	media.mu.Lock()
	stopCalls := media.stopCalls
	stopped := media.stopped
	media.mu.Unlock()
	if stopCalls != 1 || stopped.Stream != "microphone" || stopped.SSRC == "" {
		t.Fatalf("unexpected RTP stop after missing ACK: calls=%d request=%+v", stopCalls, stopped)
	}
	select {
	case err := <-session.ready:
		if err == nil || !strings.Contains(err.Error(), "stopped") {
			t.Fatalf("unexpected stopped completion: %v", err)
		}
	default:
		t.Fatal("stopped Broadcast did not release ACK waiter")
	}
}

func TestBroadcastReceiverInviteRejectsInvalidSDP(t *testing.T) {
	conn := newFlowConnection()
	media := &fakeRTPMediaService{startPort: 30000}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media, streams: &conc.Map[string, *Streams]{}}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{SDPIP: "192.0.2.20"}, Stream: &Streams{}, ready: make(chan error, 1), Version: GBVersion11,
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	body := []byte("v=0\r\no=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 8000 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\n")
	response := runBroadcastInviteFlowHandler(t, conn, api, "broadcast-invalid", body, buildGBInviteSubject(gb10PlatformID, "voice-2", gb10ChannelID))
	if !strings.Contains(response, "SIP/2.0 488") {
		t.Fatalf("unexpected response:\n%s", response)
	}
	select {
	case err := <-session.ready:
		if err == nil || !strings.Contains(err.Error(), "must accept media") {
			t.Fatalf("unexpected error: %v", err)
		}
	default:
		t.Fatal("invalid Broadcast INVITE did not fail waiting session")
	}
	media.mu.Lock()
	defer media.mu.Unlock()
	if media.startCalls != 0 {
		t.Fatalf("invalid SDP started RTP %d times", media.startCalls)
	}
}

func TestBroadcastReceiverInviteRejectsMismatchedSubject(t *testing.T) {
	conn := newFlowConnection()
	media := &fakeRTPMediaService{startPort: 30000}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media, streams: &conc.Map[string, *Streams]{}}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{SDPIP: "192.0.2.20"}, Stream: &Streams{}, ready: make(chan error, 1), Version: GBVersion11,
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	prepareBroadcastInviteTestDevice(api, conn)
	body := []byte("v=0\r\no=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 8000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n")
	request := newFlowRequest(t, conn, sip.MethodInvite, "broadcast-invalid-subject", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: gb10DeviceID + ":voice-1," + gb10ChannelID + ":speaker-1"})
	api.sipInviteGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("broadcast-invalid-subject", conn),
		DeviceID: gb10DeviceID, Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
	})
	response := string(<-flowResponse(t, conn))
	if !strings.Contains(response, "SIP/2.0 400") || !strings.Contains(response, "does not match media source") {
		t.Fatalf("unexpected response:\n%s", response)
	}
	select {
	case err := <-session.ready:
		if err == nil || !strings.Contains(err.Error(), "media source") {
			t.Fatalf("unexpected completion error: %v", err)
		}
	default:
		t.Fatal("invalid Subject did not fail waiting Broadcast session")
	}
	media.mu.Lock()
	defer media.mu.Unlock()
	if media.startCalls != 0 {
		t.Fatalf("invalid Subject started RTP %d times", media.startCalls)
	}
}

func TestBroadcastReceiverInviteRejectsInvalidSDPContentType(t *testing.T) {
	conn := newFlowConnection()
	media := &fakeRTPMediaService{startPort: 30000}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: media, streams: &conc.Map[string, *Streams]{}}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SourceVHost: defaultBroadcastVHost, SourceApp: defaultBroadcastApp, SourceStream: "microphone",
		SMS: &sms.MediaServer{SDPIP: "192.0.2.20"}, Stream: &Streams{}, ready: make(chan error, 1), Version: GBVersion11,
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	prepareBroadcastInviteTestDevice(api, conn)
	body := []byte("v=0\r\no=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 8000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n")
	request := newFlowRequest(t, conn, sip.MethodInvite, "broadcast-invalid-content-type", body)
	request.RemoveHeader("Content-Type")
	request.AppendHeader(&sip.ContentTypeXML)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: gb10PlatformID + ":voice-1," + gb10ChannelID + ":speaker-1"})
	api.sipInviteGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("broadcast-invalid-content-type", conn),
		DeviceID: gb10DeviceID, Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
	})
	response := string(<-flowResponse(t, conn))
	if !strings.Contains(response, "SIP/2.0 415") || !strings.Contains(response, "Content-Type must be application/sdp") {
		t.Fatalf("unexpected response:\n%s", response)
	}
	select {
	case err := <-session.ready:
		if err == nil || !strings.Contains(err.Error(), "Content-Type") {
			t.Fatalf("unexpected completion error: %v", err)
		}
	default:
		t.Fatal("invalid Content-Type did not fail waiting Broadcast session")
	}
	media.mu.Lock()
	defer media.mu.Unlock()
	if media.startCalls != 0 {
		t.Fatalf("invalid Content-Type started RTP %d times", media.startCalls)
	}
}

func TestBroadcastReceiverInviteRequiresSubjectFrom2014(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardYear(), func(t *testing.T) {
			conn := newFlowConnection()
			api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"}, sms: &fakeRTPMediaService{}, streams: &conc.Map[string, *Streams]{}}
			session := &broadcastSession{
				DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
				Stream: &Streams{}, ready: make(chan error, 1), Version: version,
			}
			api.broadcastSessions.Store(gb10ChannelID, session)
			request := newFlowRequest(t, conn, sip.MethodInvite, "broadcast-missing-subject-"+version.StandardYear(), nil)

			matched, err := api.findBroadcastSessionForInvite(gb10DeviceID, request)
			if matched != session {
				t.Fatalf("matched session = %p; want %p", matched, session)
			}
			if version == GBVersion10 {
				if err != nil {
					t.Fatalf("2011 compatibility error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "Subject header is required") {
				t.Fatalf("missing Subject error = %v", err)
			}
		})
	}
}

func runBroadcastInviteFlowHandler(t *testing.T, conn *flowConnection, api *GB28181API, callID string, body []byte, subject string) string {
	t.Helper()
	prepareBroadcastInviteTestDevice(api, conn)
	request := newFlowRequest(t, conn, sip.MethodInvite, callID, body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: subject})
	api.sipInviteGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction(callID+"-tx", conn),
		DeviceID: gb10DeviceID, Source: conn.remote,
		To:  mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Log: slog.Default(),
	})
	return string(<-flowResponse(t, conn))
}

func prepareBroadcastInviteTestDevice(api *GB28181API, conn *flowConnection) {
	if api == nil || conn == nil {
		return
	}
	if api.svr == nil {
		api.svr = &Server{gb: api}
	}
	if api.svr.memoryStorer != nil {
		return
	}
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.source = conn.remote
		device.conn = conn
	})
	api.svr.memoryStorer = memory
}

func TestBroadcastReceiverInviteRejectsInactiveRegistration(t *testing.T) {
	conn := newFlowConnection()
	media := &fakeRTPMediaService{startPort: 30000}
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.LastRegisterAt = time.Now().Add(-time.Minute)
		device.Expires = 10
		device.source = conn.remote
	})
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		sms: media, streams: &conc.Map[string, *Streams]{},
	}
	api.svr = &Server{gb: api, memoryStorer: memory}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, SourceID: gb10PlatformID,
		SMS: &sms.MediaServer{SDPIP: "192.0.2.20"}, Stream: &Streams{}, ready: make(chan error, 1), Version: GBVersion11,
	}
	api.broadcastSessions.Store(gb10ChannelID, session)
	body := []byte("v=0\r\no=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 8000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n")
	request := newFlowRequest(t, conn, sip.MethodInvite, "broadcast-expired-registration", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(gb10PlatformID, "voice-1", gb10ChannelID)})

	api.sipInviteGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("broadcast-expired-registration-tx", conn),
		DeviceID: gb10DeviceID, Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
	})

	response := string(<-flowResponse(t, conn))
	if !strings.Contains(response, "SIP/2.0 403") {
		t.Fatalf("expired registration Broadcast response = %s", response)
	}
	if _, ok := api.broadcastSessions.Load(gb10ChannelID); !ok {
		t.Fatal("unauthorized Broadcast INVITE removed pending session")
	}
	select {
	case err := <-session.ready:
		t.Fatalf("unauthorized Broadcast INVITE completed pending session: %v", err)
	default:
	}
	media.mu.Lock()
	startCalls := media.startCalls
	media.mu.Unlock()
	if startCalls != 0 {
		t.Fatalf("unauthorized Broadcast INVITE started RTP %d times", startCalls)
	}
}

func TestBroadcastReceiverInviteUsesPCMAFor2016And2022(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			body := []byte("v=0\r\no=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 8000 RTP/AVP 8\r\na=recvonly\r\na=rtpmap:8 PCMA/8000\r\n")
			offer, err := parseBroadcastSDPOffer(body, version)
			if err != nil {
				t.Fatal(err)
			}
			if offer.Payload != 8 || offer.Mapping != "PCMA/8000" || offer.RTPType != broadcastRTPTypeES {
				t.Fatalf("unexpected offer: %+v", offer)
			}
			session := &broadcastSession{
				SourceID: gb10PlatformID, Version: version,
				SMS: &sms.MediaServer{SDPIP: "192.0.2.20"},
			}
			answer, err := buildBroadcastSDPAnswer(session, 30000, offer.Payload, offer.Mapping, "0100000001")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(answer), "m=audio 30000 RTP/AVP 8\r\n") || !strings.Contains(string(answer), "a=rtpmap:8 PCMA/8000\r\n") {
				t.Fatalf("unexpected answer:\n%s", answer)
			}
			if _, err := parseBroadcastSDPOffer([]byte(strings.ReplaceAll(string(body), "RTP/AVP 8", "RTP/AVP 96")), version); err == nil {
				t.Fatal("protocol 2.0/3.0 accepted 1.1 PS payload without PS rtpmap")
			}
		})
	}
}

func TestParseBroadcastSDPOfferDirectionAndMediaCount(t *testing.T) {
	base := "v=0\r\no=" + gb10ChannelID + " 0 0 IN IP4 192.0.2.10\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 8000 RTP/AVP 8\r\na=recvonly\r\na=rtpmap:8 PCMA/8000\r\n"
	sessionOnly := strings.Replace(base, "t=0 0\r\n", "t=0 0\r\na=sendonly\r\n", 1)
	sessionOnly = strings.Replace(sessionOnly, "a=recvonly\r\n", "", 1)
	if _, err := parseBroadcastSDPOffer([]byte(sessionOnly), GBVersion20); err == nil || !strings.Contains(err.Error(), "must accept media") {
		t.Fatalf("session sendonly direction error = %v", err)
	}
	mediaOverride := strings.Replace(base, "t=0 0\r\n", "t=0 0\r\na=sendonly\r\n", 1)
	if _, err := parseBroadcastSDPOffer([]byte(mediaOverride), GBVersion20); err != nil {
		t.Fatalf("media direction override rejected: %v", err)
	}
	conflicting := strings.Replace(base, "a=recvonly\r\n", "a=recvonly\r\na=sendonly\r\n", 1)
	if _, err := parseBroadcastSDPOffer([]byte(conflicting), GBVersion20); err == nil || !strings.Contains(err.Error(), "multiple direction") {
		t.Fatalf("conflicting directions error = %v", err)
	}
	conflictingMapping := strings.Replace(base, "a=rtpmap:8 PCMA/8000\r\n", "a=rtpmap:8 PCMA/8000\r\na=rtpmap:8 PCMU/8000\r\n", 1)
	if _, err := parseBroadcastSDPOffer([]byte(conflictingMapping), GBVersion20); err == nil || !strings.Contains(err.Error(), "conflicting SDP rtpmap") {
		t.Fatalf("conflicting payload mapping error = %v", err)
	}
	gb11 := strings.ReplaceAll(base, "RTP/AVP 8", "RTP/AVP 96")
	gb11 = strings.ReplaceAll(gb11, "rtpmap:8 PCMA/8000", "rtpmap:96 PS/90000\r\na=rtpmap:96 H264/90000")
	if _, err := parseBroadcastSDPOffer([]byte(gb11), GBVersion11); err == nil || !strings.Contains(err.Error(), "conflicting SDP rtpmap") {
		t.Fatalf("protocol 1.1 conflicting payload mapping error = %v", err)
	}
	duplicateMedia := base + "m=audio 8001 RTP/AVP 8\r\na=recvonly\r\na=rtpmap:8 PCMA/8000\r\n"
	if _, err := parseBroadcastSDPOffer([]byte(duplicateMedia), GBVersion20); err == nil || !strings.Contains(err.Error(), "exactly one audio") {
		t.Fatalf("duplicate audio media error = %v", err)
	}
}

func TestNewBroadcastSessionRequiresG711Source(t *testing.T) {
	media := &fakeRTPMediaService{mediaItems: []zlm.MediaItem{{Tracks: []zlm.MediaTrack{{CodecType: 1, CodecIDName: "AAC", Ready: true}}}}}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10PlatformID}, sms: media}
	_, err := api.newBroadcastSession(&VoiceInput{
		Channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		SMS:     &sms.MediaServer{Type: sms.ProtocolZLMediaKit}, Mode: voiceModeBroadcast, SourceStream: "aac-only",
	})
	if err == nil || !strings.Contains(err.Error(), "G.711") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakeRTPMediaService struct {
	mu           sync.Mutex
	mediaItems   []zlm.MediaItem
	mediaErr     error
	openPort     int
	openCalls    int
	opened       zlm.OpenRTPServerRequest
	closeCalls   int
	closed       zlm.CloseRTPServerRequest
	closeErr     error
	closeErrs    []error
	startPort    int
	startErr     error
	startCalls   int
	started      zlm.StartSendRTPRequest
	startEntered chan struct{}
	startRelease <-chan struct{}
	startOnce    sync.Once
	talkPort     int
	talkErr      error
	talkCalls    int
	talkStarted  zlm.StartSendRTPTalkRequest
	talkEntered  chan struct{}
	talkRelease  <-chan struct{}
	talkOnce     sync.Once
	stopCalls    int
	stopped      zlm.StopSendRTPRequest
	stopErr      error
	stopErrs     []error
}

func (f *fakeRTPMediaService) OpenRTPServer(_ *sms.MediaServer, in zlm.OpenRTPServerRequest) (*zlm.OpenRTPServerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
	f.opened = in
	if f.openPort <= 0 {
		return nil, errors.New("unexpected OpenRTPServer")
	}
	return &zlm.OpenRTPServerResponse{Port: f.openPort}, nil
}

func (f *fakeRTPMediaService) CloseRTPServer(_ *sms.MediaServer, in zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCalls++
	f.closed = in
	if len(f.closeErrs) > 0 {
		err := f.closeErrs[0]
		f.closeErrs = f.closeErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if f.closeErr != nil {
		return nil, f.closeErr
	}
	return &zlm.CloseRTPServerResponse{Hit: 1}, nil
}

func (f *fakeRTPMediaService) GetMediaInfo(*sms.MediaServer, string, string) ([]zlm.MediaItem, error) {
	if f.mediaItems == nil && f.mediaErr == nil {
		return []zlm.MediaItem{{Tracks: []zlm.MediaTrack{{CodecType: 1, CodecIDName: "G711A", Ready: true}}}}, nil
	}
	return f.mediaItems, f.mediaErr
}

func (f *fakeRTPMediaService) StartSendRTP(_ *sms.MediaServer, in zlm.StartSendRTPRequest) (*zlm.StartSendRTPResponse, error) {
	if f.startEntered != nil {
		f.startOnce.Do(func() { close(f.startEntered) })
	}
	if f.startRelease != nil {
		<-f.startRelease
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls++
	f.started = in
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &zlm.StartSendRTPResponse{LocalPort: f.startPort}, nil
}

func (f *fakeRTPMediaService) StartSendRTPTalk(_ *sms.MediaServer, in zlm.StartSendRTPTalkRequest) (*zlm.StartSendRTPResponse, error) {
	if f.talkEntered != nil {
		f.talkOnce.Do(func() { close(f.talkEntered) })
	}
	if f.talkRelease != nil {
		<-f.talkRelease
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.talkCalls++
	f.talkStarted = in
	if f.talkErr != nil {
		return nil, f.talkErr
	}
	if f.talkPort <= 0 {
		return nil, errors.New("unexpected StartSendRTPTalk")
	}
	return &zlm.StartSendRTPResponse{LocalPort: f.talkPort}, nil
}

func (f *fakeRTPMediaService) StopSendRTP(_ *sms.MediaServer, in zlm.StopSendRTPRequest) (*zlm.StopSendRTPResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopCalls++
	f.stopped = in
	if len(f.stopErrs) > 0 {
		err := f.stopErrs[0]
		f.stopErrs = f.stopErrs[1:]
		if err != nil {
			return nil, err
		}
	}
	if f.stopErr != nil {
		return nil, f.stopErr
	}
	return &zlm.StopSendRTPResponse{}, nil
}
