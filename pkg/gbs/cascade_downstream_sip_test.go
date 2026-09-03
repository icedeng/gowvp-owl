package gbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
)

type cascadeDownstreamSIPFixture struct {
	api     *GB28181API
	channel *ipc.Channel
	peer    net.Conn
	server  *sip.Server
}

type cascadeDownstreamSIPObservation struct {
	request string
	ack     string
	err     error
}

func newCascadeDownstreamSIPFixture(t *testing.T, version GBProtocolVersion) *cascadeDownstreamSIPFixture {
	t.Helper()
	localAddress := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(localAddress)
	local, remote := net.Pipe()
	localConn := &cascadeDownstreamTCPConn{
		Conn: local,
		local: &net.TCPAddr{
			IP: net.ParseIP("192.0.2.20"), Port: 5060,
		},
		remote: &net.TCPAddr{
			IP: net.ParseIP("192.0.2.30"), Port: 5060,
		},
	}
	peer := &cascadeDownstreamTCPConn{Conn: remote, local: localConn.remote, remote: localConn.local}
	connection := sip.NewTCPConnection(localConn)
	runtimeDevice := &Device{
		IsOnline:  true,
		gbVersion: string(version),
		conn:      connection,
		source:    localConn.remote,
		to:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.30:5060"),
	}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	if err := runtimeChannel.init("192.0.2.30:5060"); err != nil {
		t.Fatal(err)
	}
	persistentDevice := &ipc.Device{DeviceID: gb10DeviceID, IsOnline: true}
	persistentChannel := &ipc.Channel{
		DeviceID:  gb10DeviceID,
		ChannelID: testCascadeChannelID,
		IsOnline:  true,
	}
	memory := &cascadeFlowMemory{
		flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice},
		channel:    runtimeChannel,
	}
	cfg := &conf.SIP{
		ID: gb10PlatformID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060,
	}
	server := &Server{
		Server: sipServer, memoryStorer: memory, fromAddress: *localAddress,
	}
	api := &GB28181API{cfg: cfg, svr: server}
	server.gb = api
	sipServer.Message().Handle("DeviceConfig", api.handleDeviceConfig)
	sipServer.Message().Handle("DeviceControl", api.sipMessageDeviceControl)
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = peer.Close()
		sipServer.Close()
	})
	return &cascadeDownstreamSIPFixture{
		api: api, channel: persistentChannel, peer: peer, server: sipServer,
	}
}

func TestSendCascadeDeviceConfigDownstreamUsesRealSIPPath(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion11)
	identity := cascadeDownstreamTestIdentity()
	ctx, cancel := context.WithTimeout(
		withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
		2*time.Second,
	)
	defer cancel()

	observed := make(chan cascadeDownstreamSIPObservation, 1)
	go observeCascadeDownstreamSIP(fixture.peer, GBVersion11, "DeviceConfig", func(frame string) ([]byte, error) {
		var request DeviceConfigRequest
		if err := sip.XMLDecode(cascadeDownstreamSIPBody(frame), &request); err != nil {
			return nil, err
		}
		if request.SN <= 0 || request.DeviceID != testCascadeChannelID || request.BasicParam == nil {
			return nil, fmt.Errorf("downstream DeviceConfig mapping = %+v", request)
		}
		if request.BasicParam.DeviceID != testCascadeChannelID || request.BasicParam.SIPServerID != gb10PlatformID {
			return nil, fmt.Errorf("completed downstream BasicParam = %+v", request.BasicParam)
		}
		return sip.XMLEncode(DeviceConfigResponse{
			CmdType: "DeviceConfig", SN: int(request.SN), DeviceID: request.DeviceID, Result: "OK",
		})
	}, observed)

	result, err := fixture.api.sendCascadeDeviceConfigDownstream(ctx, fixture.channel, &DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 91, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Name: "IPC", Expiration: 3600, HeartBeatInterval: 60, HeartBeatCount: 3},
	})
	if err != nil || result != "OK" {
		t.Fatalf("real downstream DeviceConfig result = %q, err=%v", result, err)
	}
	assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), GBVersion11, identity, true)
	if countSyncMap(&fixture.api.pendingDeviceConfig) != 0 {
		t.Fatal("downstream DeviceConfig pending state survived completion")
	}
}

func TestSendCascadeDeviceConfigDownstreamPreserves2022ExtraInfo(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion30)
	identity := cascadeDownstreamTestIdentity()
	ctx, cancel := context.WithTimeout(
		withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
		2*time.Second,
	)
	defer cancel()

	observed := make(chan cascadeDownstreamSIPObservation, 1)
	go observeCascadeDownstreamSIP(fixture.peer, GBVersion30, "DeviceConfig", func(frame string) ([]byte, error) {
		body := string(cascadeDownstreamSIPBody(frame))
		if strings.Count(body, "<ExtraInfo>") != 2 || !strings.Contains(body, "<ExtraInfo> first </ExtraInfo>") || strings.Contains(body, "ExtralInfo") {
			return nil, fmt.Errorf("downstream DeviceConfig ExtraInfo XML = %s", body)
		}
		var request DeviceConfigRequest
		if err := sip.XMLDecode([]byte(body), &request); err != nil {
			return nil, err
		}
		return sip.XMLEncode(DeviceConfigResponse{
			CmdType: "DeviceConfig", SN: int(request.SN), DeviceID: request.DeviceID, Result: "OK",
		})
	}, observed)

	result, err := fixture.api.sendCascadeDeviceConfigDownstream(ctx, fixture.channel, &DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 92, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Name: "IPC"}, ExtraInfo: []string{" first ", "second"},
	})
	if err != nil || result != "OK" {
		t.Fatalf("2022 downstream DeviceConfig result = %q, err=%v", result, err)
	}
	assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), GBVersion30, identity, true)
}

func TestSendCascadeDeviceConfigDownstreamRejectsExtraInfoDowngrade(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion20)
	result, err := fixture.api.sendCascadeDeviceConfigDownstream(t.Context(), fixture.channel, &DeviceConfigRequest{
		CmdType: "DeviceConfig", SN: 93, DeviceID: testExposedChannelID,
		BasicParam: &BasicParam{Name: "IPC"}, ExtraInfo: []string{"2022-only"},
	})
	if err == nil || !strings.Contains(err.Error(), "protocol 3.0") || result != "ERROR" {
		t.Fatalf("DeviceConfig ExtraInfo downgrade result = %q, err=%v", result, err)
	}
}

func TestPlayUsesRealSIPPathByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			fixture := newCascadeDownstreamSIPFixture(t, version)
			adapter, _, inputChannel := newCascadeMediaCore(t)
			media := &fakeRTPMediaService{openPort: 30000}
			mediaServer := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
			fixture.api.core = adapter
			fixture.api.sms = media
			fixture.api.streams = &conc.Map[string, *Streams]{}

			observed := make(chan []string, 1)
			remoteErr := make(chan error, 1)
			go func() {
				_ = fixture.peer.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(fixture.peer)
				invite, err := readAnnexGTestSIPFrame(reader)
				if err != nil {
					remoteErr <- err
					return
				}
				ssrc := directTCPSDPLineValue([]byte(invite), "y")
				body := "v=0\r\n" +
					"o=" + gb10DeviceID + " 0 0 IN IP4 192.0.2.30\r\n" +
					"s=Play\r\n" +
					"c=IN IP4 192.0.2.30\r\n" +
					"t=0 0\r\n" +
					"m=video 9000 RTP/AVP 96\r\n" +
					"a=sendonly\r\n" +
					"a=rtpmap:96 PS/90000\r\n" +
					"y=" + ssrc + "\r\n"
				if _, err = io.WriteString(fixture.peer, directTCPTestResponse(invite, body, "Application/SDP")); err != nil {
					remoteErr <- err
					return
				}
				ack, err := readAnnexGTestSIPFrame(reader)
				if err != nil {
					remoteErr <- err
					return
				}
				bye, err := readAnnexGTestSIPFrame(reader)
				if err != nil {
					remoteErr <- err
					return
				}
				observed <- []string{invite, ack, bye}
			}()

			if err := fixture.api.PlayContext(t.Context(), &PlayInput{
				Channel: inputChannel, SMS: mediaServer, StreamMode: 0,
			}); err != nil {
				t.Fatal(err)
			}
			if err := fixture.api.StopPlay(t.Context(), &StopPlayInput{Channel: inputChannel}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-remoteErr:
				t.Fatal(err)
			case messages := <-observed:
				if !strings.HasPrefix(messages[0], "INVITE ") || !strings.HasPrefix(messages[1], "ACK ") || !strings.HasPrefix(messages[2], "BYE ") {
					t.Fatalf("SIP order = %q / %q / %q", firstSIPLine(messages[0]), firstSIPLine(messages[1]), firstSIPLine(messages[2]))
				}
				if got := annexGTestSIPHeader(messages[0], "X-GB-Ver"); got != string(version) {
					t.Fatalf("Play X-GB-Ver = %q, want %q", got, version)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for Play INVITE/ACK/BYE")
			}
			media.mu.Lock()
			openCalls, closeCalls := media.openCalls, media.closeCalls
			media.mu.Unlock()
			if openCalls != 1 || closeCalls != 1 {
				t.Fatalf("RTP server calls = open:%d close:%d", openCalls, closeCalls)
			}
			if _, ok := fixture.api.streams.Load(resolvePlaySessionKey(gb10DeviceID, testCascadeChannelID, "")); ok {
				t.Fatal("stopped Play session remained in stream registry")
			}
		})
	}
}

func TestStartHistoryRTPUsesRealSIPPathByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			fixture := newCascadeDownstreamSIPFixture(t, version)
			adapter, _, inputChannel := newCascadeMediaCore(t)
			media := &fakeRTPMediaService{openPort: 30000}
			mediaServer := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
			fixture.api.core = adapter
			fixture.api.sms = media
			fixture.api.streams = &conc.Map[string, *Streams]{}

			observed := make(chan []string, 1)
			remoteErr := make(chan error, 1)
			go func() {
				_ = fixture.peer.SetDeadline(time.Now().Add(3 * time.Second))
				reader := bufio.NewReader(fixture.peer)
				invite, err := readAnnexGTestSIPFrame(reader)
				if err != nil {
					remoteErr <- err
					return
				}
				ssrc := directTCPSDPLineValue([]byte(invite), "y")
				body := "v=0\r\n" +
					"o=" + gb10DeviceID + " 0 0 IN IP4 192.0.2.30\r\n" +
					"s=Playback\r\n" +
					"c=IN IP4 192.0.2.30\r\n" +
					"t=0 0\r\n" +
					"m=video 9000 RTP/AVP 96\r\n" +
					"a=sendonly\r\n" +
					"a=rtpmap:96 PS/90000\r\n" +
					"y=" + ssrc + "\r\n"
				if _, err = io.WriteString(fixture.peer, directTCPTestResponse(invite, body, "Application/SDP")); err != nil {
					remoteErr <- err
					return
				}
				ack, err := readAnnexGTestSIPFrame(reader)
				if err != nil {
					remoteErr <- err
					return
				}
				bye, err := readAnnexGTestSIPFrame(reader)
				if err != nil {
					remoteErr <- err
					return
				}
				if _, err = io.WriteString(fixture.peer, annexGTestSIPResponse(bye, http.StatusOK, "OK", "")); err != nil {
					remoteErr <- err
					return
				}
				observed <- []string{invite, ack, bye}
			}()

			if err := fixture.api.StartHistory(t.Context(), &HistoryInput{
				Channel: inputChannel, SMS: mediaServer, StreamMode: 0, Mode: historyModePlayback,
				StartAt: time.Unix(1700000000, 0), EndAt: time.Unix(1700000060, 0),
			}); err != nil {
				t.Fatal(err)
			}
			if err := fixture.api.StopHistory(t.Context(), &StopHistoryInput{Channel: inputChannel, Mode: historyModePlayback}); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-remoteErr:
				t.Fatal(err)
			case messages := <-observed:
				if !strings.HasPrefix(messages[0], "INVITE ") || !strings.HasPrefix(messages[1], "ACK ") || !strings.HasPrefix(messages[2], "BYE ") {
					t.Fatalf("SIP order = %q / %q / %q", firstSIPLine(messages[0]), firstSIPLine(messages[1]), firstSIPLine(messages[2]))
				}
				if !strings.Contains(messages[0], "\r\nu="+testCascadeChannelID+":3\r\n") {
					t.Fatalf("Playback INVITE did not select time-based history URI:\n%s", messages[0])
				}
				if got := annexGTestSIPHeader(messages[0], "X-GB-Ver"); got != string(version) {
					t.Fatalf("Playback X-GB-Ver = %q, want %q", got, version)
				}
				invitedSSRC, err := strconv.ParseUint(directTCPSDPLineValue([]byte(messages[0]), "y"), 10, 64)
				if err != nil {
					t.Fatalf("Playback INVITE SSRC: %v", err)
				}
				media.mu.Lock()
				opened := media.opened
				media.mu.Unlock()
				if opened.SSRC != invitedSSRC {
					t.Fatalf("Playback RTP filter SSRC = %d, want INVITE SSRC %d", opened.SSRC, invitedSSRC)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("timed out waiting for Playback INVITE/ACK/BYE")
			}
			media.mu.Lock()
			openCalls, closeCalls := media.openCalls, media.closeCalls
			media.mu.Unlock()
			if openCalls != 1 || closeCalls != 1 {
				t.Fatalf("RTP server calls = open:%d close:%d", openCalls, closeCalls)
			}
			if _, ok := fixture.api.streams.Load(historyKey(historyModePlayback, gb10DeviceID, testCascadeChannelID)); ok {
				t.Fatal("stopped Playback session remained in stream registry")
			}
		})
	}
}

func TestOfflineCleanupCancelsStartingRTPHistory(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion20)
	adapter, _, inputChannel := newCascadeMediaCore(t)
	media := &fakeRTPMediaService{openPort: 30000}
	mediaServer := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
	fixture.api.core = adapter
	fixture.api.sms = media
	fixture.api.streams = &conc.Map[string, *Streams]{}

	inviteReceived := make(chan struct{})
	remoteErr := make(chan error, 1)
	go func() {
		_ = fixture.peer.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(fixture.peer)
		if _, err := readAnnexGTestSIPFrame(reader); err != nil {
			remoteErr <- err
			return
		}
		close(inviteReceived)
		_, _ = io.Copy(io.Discard, reader)
	}()

	startDone := make(chan error, 1)
	go func() {
		startDone <- fixture.api.StartHistory(t.Context(), &HistoryInput{
			Channel: inputChannel, SMS: mediaServer, StreamMode: 0, Mode: historyModeDownload,
			StartAt: time.Unix(1700000000, 0), EndAt: time.Unix(1700000060, 0),
		})
	}()

	select {
	case err := <-remoteErr:
		t.Fatal(err)
	case <-inviteReceived:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for Download INVITE")
	}

	unlock := fixture.api.lockRegisterOperation(gb10DeviceID)
	fixture.api.cleanupOfflineDeviceRuntime(gb10DeviceID)
	unlock()

	select {
	case err := <-startDone:
		if !errors.Is(err, ErrDeviceOffline) {
			t.Fatalf("StartHistory error = %v; want %v", err, ErrDeviceOffline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("starting Download did not stop after offline cleanup")
	}
	if fixture.api.streams.Len() != 0 {
		t.Fatal("offline cleanup retained a starting history stream")
	}
	if _, ok := fixture.api.RTPDownloadByChannel(gb10DeviceID, testCascadeChannelID); ok {
		t.Fatal("offline cleanup allowed a late RTP download state")
	}
}

func TestOfflineCleanupCancelsStartingPlay(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion20)
	adapter, _, inputChannel := newCascadeMediaCore(t)
	media := &fakeRTPMediaService{openPort: 30000}
	mediaServer := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
	fixture.api.core = adapter
	fixture.api.sms = media
	fixture.api.streams = &conc.Map[string, *Streams]{}

	inviteReceived := make(chan struct{})
	remoteErr := make(chan error, 1)
	go func() {
		_ = fixture.peer.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(fixture.peer)
		if _, err := readAnnexGTestSIPFrame(reader); err != nil {
			remoteErr <- err
			return
		}
		close(inviteReceived)
		_, _ = io.Copy(io.Discard, reader)
	}()

	startDone := make(chan error, 1)
	go func() {
		startDone <- fixture.api.PlayContext(t.Context(), &PlayInput{
			Channel: inputChannel, SMS: mediaServer, StreamMode: 0,
		})
	}()
	waitForStartingMediaInvite(t, inviteReceived, remoteErr, "Play")

	cleanupOfflineTestDevice(fixture.api)
	select {
	case err := <-startDone:
		if !errors.Is(err, ErrDeviceOffline) {
			t.Fatalf("Play error = %v; want %v", err, ErrDeviceOffline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("starting Play did not stop after offline cleanup")
	}
	if fixture.api.streams.Len() != 0 {
		t.Fatal("offline cleanup retained a starting Play stream")
	}
}

func TestOfflineCleanupCancelsStartingTalk(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion20)
	adapter, _, inputChannel := newCascadeMediaCore(t)
	media := &fakeRTPMediaService{
		openPort: 30000,
		mediaItems: []zlm.MediaItem{{Tracks: []zlm.MediaTrack{{
			CodecType: 1, CodecIDName: "G711A", Ready: true,
		}}}},
	}
	mediaServer := &sms.MediaServer{ID: "local", Type: sms.ProtocolZLMediaKit, SDPIP: "192.0.2.20"}
	fixture.api.core = adapter
	fixture.api.sms = media
	fixture.api.streams = &conc.Map[string, *Streams]{}

	inviteReceived := make(chan struct{})
	invitedSSRC := make(chan string, 1)
	remoteErr := make(chan error, 1)
	go func() {
		_ = fixture.peer.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(fixture.peer)
		invite, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		invitedSSRC <- directTCPSDPLineValue([]byte(invite), "y")
		close(inviteReceived)
		_, _ = io.Copy(io.Discard, reader)
	}()

	startDone := make(chan error, 1)
	go func() {
		startDone <- fixture.api.StartVoice(t.Context(), &VoiceInput{
			Channel: inputChannel, SMS: mediaServer, StreamMode: 0, Mode: voiceModeTalk,
			SourceStream: "microphone",
		})
	}()
	waitForStartingMediaInvite(t, inviteReceived, remoteErr, "Talk")
	wantSSRC, err := strconv.ParseUint(<-invitedSSRC, 10, 64)
	if err != nil {
		t.Fatalf("Talk INVITE SSRC: %v", err)
	}
	media.mu.Lock()
	opened := media.opened
	media.mu.Unlock()
	if opened.SSRC != wantSSRC {
		t.Fatalf("Talk RTP filter SSRC = %d, want INVITE SSRC %d", opened.SSRC, wantSSRC)
	}

	cleanupOfflineTestDevice(fixture.api)
	select {
	case err := <-startDone:
		if !errors.Is(err, ErrDeviceOffline) {
			t.Fatalf("StartVoice Talk error = %v; want %v", err, ErrDeviceOffline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("starting Talk did not stop after offline cleanup")
	}
	if fixture.api.streams.Len() != 0 || syncMapLen(&fixture.api.talkSessions) != 0 {
		t.Fatal("offline cleanup retained a starting Talk session")
	}
}

func waitForStartingMediaInvite(t *testing.T, inviteReceived <-chan struct{}, remoteErr <-chan error, name string) {
	t.Helper()
	select {
	case err := <-remoteErr:
		t.Fatal(err)
	case <-inviteReceived:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s INVITE", name)
	}
}

func cleanupOfflineTestDevice(api *GB28181API) {
	unlock := api.lockRegisterOperation(gb10DeviceID)
	api.cleanupOfflineDeviceRuntime(gb10DeviceID)
	unlock()
}

func TestUpgradeUsesRealSIPPathAndTracksFinalResult(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion30)
	identity := cascadeDownstreamTestIdentity()
	ctx, cancel := context.WithTimeout(
		withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
		2*time.Second,
	)
	defer cancel()

	sessionID := "upgrade-real-sip-session-00000001"
	observed := make(chan cascadeDownstreamSIPObservation, 1)
	go observeCascadeDownstreamSIP(fixture.peer, GBVersion30, "DeviceControl", func(frame string) ([]byte, error) {
		var request deviceControlA23Request
		if err := sip.XMLDecode(cascadeDownstreamSIPBody(frame), &request); err != nil {
			return nil, err
		}
		if request.SN <= 0 || request.DeviceID != testCascadeChannelID || request.DeviceUpgrade == nil {
			return nil, fmt.Errorf("downstream DeviceUpgrade mapping = %+v", request)
		}
		upgrade := request.DeviceUpgrade
		if upgrade.SessionID != sessionID || upgrade.Firmware != " V1.2.4 " ||
			upgrade.FileURL != " https://example.invalid/fw.bin " || upgrade.Manufacturer != " Vendor " {
			return nil, fmt.Errorf("downstream DeviceUpgrade payload = %+v", upgrade)
		}
		return sip.XMLEncode(deviceControlResponse{
			CmdType: ptzCmdTypeDeviceControl, SN: request.SN, DeviceID: request.DeviceID, Result: ptzResultOK,
		})
	}, observed)

	output, err := fixture.api.Upgrade(ctx, &UpgradeInput{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID,
		Firmware: " V1.2.4 ", FileURL: " https://example.invalid/fw.bin ", Manufacturer: " Vendor ",
		SessionID: sessionID, Timeout: time.Second,
	})
	if err != nil || output == nil || output.Result != ptzResultOK || output.SessionID != sessionID {
		t.Fatalf("Upgrade output = %+v, err=%v", output, err)
	}
	assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), GBVersion30, identity, true)
	state, ok := fixture.api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "accepted" || state.ChannelID != testCascadeChannelID {
		t.Fatalf("accepted Upgrade state = %+v, %v", state, ok)
	}

	body := []byte(`<Notify><CmdType>DeviceUpgradeResult</CmdType><SN>93</SN><DeviceID>` + testCascadeChannelID +
		`</DeviceID><SessionID>` + sessionID + `</SessionID><UpgradeResult>OK</UpgradeResult>` +
		`<Firmware>V1.2.5</Firmware></Notify>`)
	response := runFlowHandler(t, newFlowConnection(), fixture.api, sip.MethodMessage, "upgrade-real-sip-result", body, fixture.api.sipMessageDeviceUpgradeResult)
	assertFlowOK(t, response)
	state, ok = fixture.api.UpgradeState(gb10DeviceID, sessionID)
	if !ok || state.Status != "completed" || state.Result != ptzResultOK || state.Firmware != "V1.2.5" {
		t.Fatalf("completed Upgrade state = %+v, %v", state, ok)
	}
}

func TestSendCascadeDeviceControlDownstreamUsesRealSIPPathByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			fixture := newCascadeDownstreamSIPFixture(t, version)
			identity := cascadeDownstreamTestIdentity()
			ctx, cancel := context.WithTimeout(
				withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
				2*time.Second,
			)
			defer cancel()
			ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionRight, Speed: 40})
			if err != nil {
				t.Fatal(err)
			}
			observed := make(chan cascadeDownstreamSIPObservation, 1)
			go observeCascadeDownstreamSIP(fixture.peer, version, "DeviceControl", func(frame string) ([]byte, error) {
				body := cascadeDownstreamSIPBody(frame)
				var request deviceControlA23Request
				if err := decodeDeviceControlRequest(body, &request); err != nil {
					return nil, err
				}
				if request.SN <= 0 || request.DeviceID != testCascadeChannelID || request.PTZCmd != ptz {
					return nil, fmt.Errorf("downstream DeviceControl mapping = %+v", request)
				}
				values := deviceControlTextInfo(&request)
				if len(values) != 2 || values[0] != " extension " || values[1] != "" {
					return nil, fmt.Errorf("downstream DeviceControl text extensions = %#v", values)
				}
				text := string(body)
				if version == GBVersion30 {
					if strings.Count(text, "<ExtraInfo>") != 2 || strings.Contains(text, "<Info>") {
						return nil, fmt.Errorf("downstream 2022 DeviceControl XML = %s", text)
					}
				} else if strings.Count(text, "<Info>") != 2 || strings.Contains(text, "<ExtraInfo>") {
					return nil, fmt.Errorf("downstream legacy DeviceControl XML = %s", text)
				}
				return nil, nil
			}, observed)

			result, err := fixture.api.sendCascadeDeviceControlDownstream(ctx, fixture.channel, &deviceControlA23Request{
				CmdType: ptzCmdTypeDeviceControl, SN: 92, DeviceID: testExposedChannelID, PTZCmd: ptz,
				ExtraInfo: []string{" extension ", ""},
			})
			if err != nil || result != ptzResultOK {
				t.Fatalf("real downstream DeviceControl result = %q, err=%v", result, err)
			}
			assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), version, identity, false)
			if countSyncMap(&fixture.api.pendingDeviceControl) != 0 {
				t.Fatal("downstream DeviceControl pending state survived completion")
			}
		})
	}
}

func TestDeviceControlNoResponseActionsCompleteAfterSIPOK(t *testing.T) {
	ptz, err := encodePTZCommand(&PTZInput{Action: PTZActionRight, Speed: 40})
	if err != nil {
		t.Fatal(err)
	}
	pan := 120.5
	tests := []struct {
		name    string
		version GBProtocolVersion
		input   *DeviceControlInput
	}{
		{name: "2011 camera", version: GBVersion10, input: &DeviceControlInput{TargetID: testCascadeChannelID, Action: deviceControlActionCameraControl, PTZCmd: ptz, ControlPriority: intPointer(5)}},
		{name: "2011 tele boot", version: GBVersion10, input: &DeviceControlInput{Action: deviceControlActionTeleBoot}},
		{name: "2014 drag zoom in", version: GBVersion11, input: &DeviceControlInput{TargetID: testCascadeChannelID, Action: deviceControlActionDragZoomIn, DragZoom: &DragZoomParam{Length: 1920, Width: 1080, MidPointX: 960, MidPointY: 540, LengthX: 320, LengthY: 180}}},
		{name: "2014 drag zoom out", version: GBVersion11, input: &DeviceControlInput{TargetID: testCascadeChannelID, Action: deviceControlActionDragZoomOut, DragZoom: &DragZoomParam{Length: 1920, Width: 1080, MidPointX: 960, MidPointY: 540, LengthX: 320, LengthY: 180}}},
		{name: "2016 iframe", version: GBVersion20, input: &DeviceControlInput{TargetID: testCascadeChannelID, Action: deviceControlActionIFrameSend}},
		{name: "2022 precise ptz", version: GBVersion30, input: &DeviceControlInput{TargetID: testCascadeChannelID, Action: deviceControlActionPTZPrecise, PTZPrecise: &PTZPreciseParam{Pan: &pan}, ExtraInfo: []string{" extension "}}},
		{name: "2022 format sd card", version: GBVersion30, input: &DeviceControlInput{Action: deviceControlActionFormatSDCard}},
		{name: "2022 target track", version: GBVersion30, input: &DeviceControlInput{TargetID: testCascadeChannelID, Action: deviceControlActionTargetTrack, TargetTrack: &TargetTrackParam{Mode: "Auto"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCascadeDownstreamSIPFixture(t, test.version)
			identity := cascadeDownstreamTestIdentity()
			ctx, cancel := context.WithTimeout(
				withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
				2*time.Second,
			)
			defer cancel()
			test.input.DeviceID = gb10DeviceID
			test.input.Timeout = time.Second
			expectedTarget := test.input.TargetID
			if expectedTarget == "" {
				expectedTarget = gb10DeviceID
			}
			observed := make(chan cascadeDownstreamSIPObservation, 1)
			go observeCascadeDownstreamSIP(fixture.peer, test.version, "DeviceControl", func(frame string) ([]byte, error) {
				var request deviceControlA23Request
				if err := sip.XMLDecode(cascadeDownstreamSIPBody(frame), &request); err != nil {
					return nil, err
				}
				if request.DeviceID != expectedTarget || deviceControlRequiresBusinessResponse(&request) {
					return nil, fmt.Errorf("downstream no-response DeviceControl mapping = %+v", request)
				}
				if test.input.ControlPriority != nil && (request.Info == nil || request.Info.ControlPriority == nil || *request.Info.ControlPriority != *test.input.ControlPriority) {
					return nil, fmt.Errorf("downstream ControlPriority mapping = %+v", request)
				}
				if len(test.input.ExtraInfo) != 0 && (len(request.ExtraInfo) != 1 || request.ExtraInfo[0] != " extension ") {
					return nil, fmt.Errorf("downstream ExtraInfo mapping = %#v", request.ExtraInfo)
				}
				if test.input.Action == deviceControlActionIFrameSend {
					if test.version == GBVersion20 && (request.IFameCmd != "Send" || request.IFrameCmd != "") {
						return nil, fmt.Errorf("2016 force-I-frame field mapping = %+v", request)
					}
					if test.version == GBVersion30 && (request.IFrameCmd != "Send" || request.IFameCmd != "") {
						return nil, fmt.Errorf("2022 force-I-frame field mapping = %+v", request)
					}
				}
				return nil, nil
			}, observed)

			output, err := fixture.api.DeviceControl(ctx, test.input)
			if err != nil || output.Result != ptzResultOK || output.TargetID != expectedTarget {
				t.Fatalf("DeviceControl output = %+v, err=%v", output, err)
			}
			assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), test.version, identity, false)
			if countSyncMap(&fixture.api.pendingDeviceControl) != 0 {
				t.Fatal("no-response DeviceControl registered pending state")
			}
		})
	}
}

func TestPTZContextCompletesAfterSIPOK(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion10)
	identity := cascadeDownstreamTestIdentity()
	ctx, cancel := context.WithTimeout(
		withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
		2*time.Second,
	)
	defer cancel()
	observed := make(chan cascadeDownstreamSIPObservation, 1)
	go observeCascadeDownstreamSIP(fixture.peer, GBVersion10, "DeviceControl", func(frame string) ([]byte, error) {
		var request deviceControlA23Request
		if err := sip.XMLDecode(cascadeDownstreamSIPBody(frame), &request); err != nil {
			return nil, err
		}
		if request.DeviceID != testCascadeChannelID || request.PTZCmd == "" {
			return nil, fmt.Errorf("downstream PTZ mapping = %+v", request)
		}
		return nil, nil
	}, observed)

	output, err := fixture.api.PTZContext(ctx, &PTZInput{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, Action: PTZActionRight, Speed: 40, Timeout: time.Millisecond,
	})
	if err != nil || output.Result != ptzResultOK {
		t.Fatalf("PTZContext output = %+v, err=%v", output, err)
	}
	assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), GBVersion10, identity, false)
	if countSyncMap(&fixture.api.pendingDeviceControl) != 0 {
		t.Fatal("PTZ registered pending DeviceControl state")
	}
}

func TestDeviceControlBusinessResponseActionsStillWait(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion10)
	identity := cascadeDownstreamTestIdentity()
	ctx, cancel := context.WithTimeout(
		withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
		2*time.Second,
	)
	defer cancel()
	release := make(chan struct{})
	sipOK := make(chan struct{})
	observed := make(chan cascadeDownstreamSIPObservation, 1)
	go observeCascadeDownstreamSIP(fixture.peer, GBVersion10, "DeviceControl", func(frame string) ([]byte, error) {
		var request deviceControlA23Request
		if err := sip.XMLDecode(cascadeDownstreamSIPBody(frame), &request); err != nil {
			return nil, err
		}
		if request.DeviceID != testCascadeChannelID || request.RecordCmd != "Record" {
			return nil, fmt.Errorf("downstream record mapping = %+v", request)
		}
		close(sipOK)
		<-release
		return sip.XMLEncode(deviceControlResponse{
			CmdType: ptzCmdTypeDeviceControl, SN: request.SN, DeviceID: request.DeviceID, Result: ptzResultOK,
		})
	}, observed)
	type controlResult struct {
		output *DeviceControlOutput
		err    error
	}
	result := make(chan controlResult, 1)
	go func() {
		output, err := fixture.api.DeviceControl(ctx, &DeviceControlInput{
			DeviceID: gb10DeviceID, TargetID: testCascadeChannelID, Action: deviceControlActionRecordStart, Timeout: time.Second,
		})
		result <- controlResult{output: output, err: err}
	}()
	select {
	case <-sipOK:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("record control did not receive SIP 200 OK")
	}
	select {
	case early := <-result:
		close(release)
		t.Fatalf("record control returned before business response: %+v", early)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	completed := <-result
	if completed.err != nil || completed.output.Result != ptzResultOK {
		t.Fatalf("record control output = %+v, err=%v", completed.output, completed.err)
	}
	assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), GBVersion10, identity, true)
}

func TestSendCascadeDeviceControlDownstreamWaitsForBusinessResponse(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion10)
	identity := cascadeDownstreamTestIdentity()
	ctx, cancel := context.WithTimeout(
		withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID),
		2*time.Second,
	)
	defer cancel()
	release := make(chan struct{})
	sipOK := make(chan struct{})
	observed := make(chan cascadeDownstreamSIPObservation, 1)
	go observeCascadeDownstreamSIP(fixture.peer, GBVersion10, "DeviceControl", func(frame string) ([]byte, error) {
		var request deviceControlA23Request
		if err := sip.XMLDecode(cascadeDownstreamSIPBody(frame), &request); err != nil {
			return nil, err
		}
		if request.DeviceID != testCascadeChannelID || request.RecordCmd != "Record" {
			return nil, fmt.Errorf("cascade downstream record mapping = %+v", request)
		}
		close(sipOK)
		<-release
		return sip.XMLEncode(deviceControlResponse{
			CmdType: ptzCmdTypeDeviceControl, SN: request.SN, DeviceID: request.DeviceID, Result: ptzResultOK,
		})
	}, observed)
	type cascadeControlResult struct {
		result string
		err    error
	}
	completed := make(chan cascadeControlResult, 1)
	go func() {
		result, err := fixture.api.sendCascadeDeviceControlDownstream(ctx, fixture.channel, &deviceControlA23Request{
			CmdType: ptzCmdTypeDeviceControl, SN: 93, DeviceID: testExposedChannelID, RecordCmd: "Record",
		})
		completed <- cascadeControlResult{result: result, err: err}
	}()
	select {
	case <-sipOK:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("cascade record control did not receive SIP 200 OK")
	}
	select {
	case early := <-completed:
		close(release)
		t.Fatalf("cascade record returned before business response: %+v", early)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	result := <-completed
	if result.err != nil || result.result != ptzResultOK {
		t.Fatalf("cascade record result = %q, err=%v", result.result, result.err)
	}
	assertCascadeDownstreamSIPObservation(t, awaitCascadeDownstreamSIPObservation(t, observed), GBVersion10, identity, true)
}

func observeCascadeDownstreamSIP(
	peer net.Conn,
	version GBProtocolVersion,
	cmdType string,
	responseBody func(string) ([]byte, error),
	result chan<- cascadeDownstreamSIPObservation,
) {
	observation := cascadeDownstreamSIPObservation{}
	reader := bufio.NewReader(peer)
	request, err := readAnnexGTestSIPFrame(reader)
	if err != nil {
		observation.err = err
		result <- observation
		return
	}
	observation.request = request
	if _, err = io.WriteString(peer, annexGTestSIPResponse(request, http.StatusOK, "OK", "")); err != nil {
		observation.err = err
		result <- observation
		return
	}
	body, err := responseBody(request)
	if err != nil {
		observation.err = err
		result <- observation
		return
	}
	if len(body) == 0 {
		result <- observation
		return
	}
	if _, err = io.WriteString(peer, cascadeDownstreamBusinessMessage(version, cmdType, body)); err != nil {
		observation.err = err
		result <- observation
		return
	}
	observation.ack, observation.err = readAnnexGTestSIPFrame(reader)
	result <- observation
}

func assertCascadeDownstreamSIPObservation(
	t *testing.T,
	observation cascadeDownstreamSIPObservation,
	version GBProtocolVersion,
	identity *monitorUserIdentity,
	wantBusinessACK bool,
) {
	t.Helper()
	if observation.err != nil {
		t.Fatal(observation.err)
	}
	if !strings.HasPrefix(observation.request, "MESSAGE ") {
		t.Fatalf("downstream SIP request = %s", observation.request)
	}
	if got := annexGTestSIPHeader(observation.request, "X-GB-Ver"); got != string(version) {
		t.Fatalf("downstream X-GB-Ver = %q, want %q", got, version)
	}
	wantIdentity := testLocalGatewayID + "-" + identity.String()
	if got := annexGTestSIPHeader(observation.request, monitorUserIdentityHeaderName); got != wantIdentity {
		t.Fatalf("downstream Monitor-User-Identity = %q, want %q", got, wantIdentity)
	}
	if wantBusinessACK && !strings.HasPrefix(observation.ack, "SIP/2.0 200 OK") {
		t.Fatalf("downstream business response ACK = %s", observation.ack)
	}
	if !wantBusinessACK && observation.ack != "" {
		t.Fatalf("no-response control received unexpected business response ACK = %s", observation.ack)
	}
}

func awaitCascadeDownstreamSIPObservation(t *testing.T, observed <-chan cascadeDownstreamSIPObservation) cascadeDownstreamSIPObservation {
	t.Helper()
	select {
	case observation := <-observed:
		return observation
	case <-time.After(2 * time.Second):
		t.Fatal("downstream SIP peer did not finish")
		return cascadeDownstreamSIPObservation{}
	}
}

func cascadeDownstreamBusinessMessage(version GBProtocolVersion, cmdType string, body []byte) string {
	return fmt.Sprintf(
		"MESSAGE sip:%s@192.0.2.20:5060 SIP/2.0\r\n"+
			"Via: SIP/2.0/TCP 192.0.2.30:5060;branch=z9hG4bK-downstream-%s\r\n"+
			"From: <sip:%s@192.0.2.30:5060>;tag=downstream\r\n"+
			"To: <sip:%s@192.0.2.20:5060>\r\n"+
			"Call-ID: downstream-%s@192.0.2.30\r\n"+
			"CSeq: 1 MESSAGE\r\n"+
			"Max-Forwards: 70\r\n"+
			"Content-Type: Application/MANSCDP+xml\r\n"+
			"X-GB-Ver: %s\r\n"+
			"Content-Length: %d\r\n\r\n%s",
		gb10PlatformID, strings.ToLower(cmdType), gb10DeviceID, gb10PlatformID,
		strings.ToLower(cmdType), version, len(body), body,
	)
}

func cascadeDownstreamSIPBody(frame string) []byte {
	_, body, _ := strings.Cut(frame, "\r\n\r\n")
	return []byte(body)
}

func cascadeDownstreamTestIdentity() *monitorUserIdentity {
	return &monitorUserIdentity{
		Gateways: []string{testRemoteGatewayID}, UserID: testRemoteUserID,
		Organization: "remoteorg", Category: "dispatcher", Rank: "level2",
	}
}

func countSyncMap(values interface {
	Range(func(key, value any) bool)
}) int {
	count := 0
	values.Range(func(_, _ any) bool {
		count++
		return true
	})
	return count
}

type cascadeDownstreamTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (c *cascadeDownstreamTCPConn) LocalAddr() net.Addr  { return c.local }
func (c *cascadeDownstreamTCPConn) RemoteAddr() net.Addr { return c.remote }
