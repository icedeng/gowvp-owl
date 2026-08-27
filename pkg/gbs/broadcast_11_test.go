package gbs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	response := runFlowHandler(t, conn, api, sip.MethodInvite, "broadcast-dialog", body, api.sipInviteGeneric)
	assertFlowOK(t, response)
	for _, want := range []string{"m=audio 30000 RTP/AVP 96", "a=sendonly", "a=rtpmap:96 PS/90000", "f=v/////a/1/8/1"} {
		if !strings.Contains(response, want) {
			t.Fatalf("Broadcast response missing %q:\n%s", want, response)
		}
	}
	select {
	case err := <-session.ready:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("Broadcast INVITE did not complete waiting session")
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
	response := runFlowHandler(t, conn, api, sip.MethodInvite, "broadcast-invalid", body, api.sipInviteGeneric)
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
	mu          sync.Mutex
	mediaItems  []zlm.MediaItem
	mediaErr    error
	openPort    int
	openCalls   int
	closeCalls  int
	closed      zlm.CloseRTPServerRequest
	startPort   int
	startErr    error
	startCalls  int
	started     zlm.StartSendRTPRequest
	talkPort    int
	talkErr     error
	talkCalls   int
	talkStarted zlm.StartSendRTPTalkRequest
	stopCalls   int
	stopped     zlm.StopSendRTPRequest
}

func (f *fakeRTPMediaService) OpenRTPServer(*sms.MediaServer, zlm.OpenRTPServerRequest) (*zlm.OpenRTPServerResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openCalls++
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
	return &zlm.CloseRTPServerResponse{Hit: 1}, nil
}

func (f *fakeRTPMediaService) GetMediaInfo(*sms.MediaServer, string, string) ([]zlm.MediaItem, error) {
	if f.mediaItems == nil && f.mediaErr == nil {
		return []zlm.MediaItem{{Tracks: []zlm.MediaTrack{{CodecType: 1, CodecIDName: "G711A", Ready: true}}}}, nil
	}
	return f.mediaItems, f.mediaErr
}

func (f *fakeRTPMediaService) StartSendRTP(_ *sms.MediaServer, in zlm.StartSendRTPRequest) (*zlm.StartSendRTPResponse, error) {
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
	return &zlm.StopSendRTPResponse{}, nil
}
