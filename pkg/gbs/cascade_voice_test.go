package gbs

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestCascadeBroadcastVersionProfile(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		payload int
		mapping string
		allowed bool
	}{
		{version: GBVersion10},
		{version: GBVersion11, payload: 96, mapping: "PS/90000", allowed: true},
		{version: GBVersion20, payload: 8, mapping: "PCMA/8000", allowed: true},
		{version: GBVersion30, payload: 8, mapping: "PCMA/8000", allowed: true},
	}
	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			payload, mapping, allowed := cascadeBroadcastProfile(tt.version)
			if payload != tt.payload || mapping != tt.mapping || allowed != tt.allowed {
				t.Fatalf("cascadeBroadcastProfile(%s) = %d, %q, %v", tt.version, payload, mapping, allowed)
			}
		})
	}
}

func TestCascadeBroadcastBuildsUpstreamSourceAndDownstreamSession(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.40"), Port: 5060}
	runtimeDevice := &Device{
		IsOnline: true, gbVersion: string(GBVersion11), conn: connection, source: connection.remote,
		to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example"),
	}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}, channel: runtimeChannel}
	mediaServer := &sms.MediaServer{ID: sms.DefaultMediaServerID, SDPIP: "192.0.2.20", Type: sms.ProtocolZLMediaKit}
	media := &fakeRTPMediaService{
		openPort:   30000,
		startPort:  31000,
		mediaItems: []zlm.MediaItem{{Tracks: []zlm.MediaTrack{{CodecType: 1, CodecIDName: "G711A", Ready: true}}}},
	}
	from := mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example")
	server := &Server{memoryStorer: memory, mediaService: fakeCascadeMediaResolver{server: mediaServer}, fromAddress: *from}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, core: adapter, svr: server, sms: media,
		streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef),
	}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	const upstreamSourceID = "34020000001360000021"
	var upstreamInvite, upstreamACK, businessResponse, upstreamBYE *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		switch request.Method() {
		case sip.MethodInvite:
			upstreamInvite = request
			answer := []byte("v=0\r\no=" + upstreamSourceID + " 0 0 IN IP4 192.0.2.30\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 40000 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\ny=0100000002\r\nf=v/////a/1/8/1\r\n")
			response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", answer)
			response.AppendHeader(&sip.ContentTypeSDP)
			return response, nil
		case sip.MethodBYE:
			upstreamBYE = request
			return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
		case sip.MethodMessage:
			businessResponse = request
			return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
		default:
			t.Fatalf("unexpected upstream request method: %s", request.Method())
			return nil, nil
		}
	}
	worker.send = func(request *sip.Request) error {
		upstreamACK = request
		return nil
	}
	var downstream *VoiceInput
	api.cascadeBroadcastNotify = func(_ context.Context, _ *Channel, input *VoiceInput) error {
		copyInput := *input
		downstream = &copyInput
		value, ok := api.broadcastSessions.Load(testCascadeChannelID)
		if !ok {
			t.Fatal("downstream Broadcast notification sent before session was stored")
		}
		session := value.(*broadcastSession)
		body := []byte("v=0\r\no=" + testCascadeChannelID + " 0 0 IN IP4 192.0.2.40\r\ns=Play\r\nc=IN IP4 192.0.2.40\r\nt=0 0\r\nm=audio 41000 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n")
		invite := newCascadeBroadcastReceiverRequest(t, connection, sip.MethodInvite, "cascade-voice-receiver", session.SourceID, session.ChannelID, body)
		api.sipInviteGeneric(&sip.Context{
			Request: invite, Tx: sip.NewTransaction("cascade-voice-receiver", connection),
			DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
		})
		select {
		case response := <-connection.writes:
			if !strings.Contains(string(response), "SIP/2.0 200 OK") || !strings.Contains(string(response), "a=sendonly") {
				t.Fatalf("downstream Broadcast INVITE response: %s", response)
			}
		case <-time.After(time.Second):
			t.Fatal("downstream Broadcast INVITE response timeout")
		}
		dialogValue, ok := api.inviteDialogs.Load("cascade-voice-receiver")
		if !ok {
			t.Fatal("downstream Broadcast dialog not stored")
		}
		ack := newCascadeBroadcastReceiverRequest(t, connection, sip.MethodACK, "cascade-voice-receiver", session.SourceID, session.ChannelID, nil)
		applyInboundDialogTags(t, ack, dialogValue.(*inboundInviteDialog), true)
		api.sipAckGeneric(&sip.Context{Request: ack, DeviceID: gb10DeviceID, Source: connection.remote})
		return nil
	}

	request := cascadeQueryEnvelope{
		CmdType: "Broadcast", SN: 77, SourceID: upstreamSourceID, TargetID: testExposedChannelID,
	}
	if err := api.forwardCascadeBroadcast(t.Context(), worker, request); err != nil {
		t.Fatal(err)
	}
	if upstreamInvite == nil || upstreamInvite.Recipient().User().String() != upstreamSourceID {
		t.Fatalf("upstream source INVITE = %#v", upstreamInvite)
	}
	inviteBody := string(upstreamInvite.Body())
	for _, expected := range []string{
		"m=audio 30000 RTP/AVP 96", "a=recvonly", "a=rtpmap:96 PS/90000",
		"Subject: " + upstreamSourceID + ":", "," + testExposedChannelID + ":0",
	} {
		if !strings.Contains(upstreamInvite.String(), expected) && !strings.Contains(inviteBody, expected) {
			t.Fatalf("upstream source INVITE missing %q:\n%s", expected, upstreamInvite.String())
		}
	}
	if upstreamACK == nil || upstreamACK.Method() != sip.MethodACK {
		t.Fatalf("upstream source ACK = %#v", upstreamACK)
	}
	if downstream == nil || downstream.Channel == nil || downstream.Channel.ChannelID != persistentChannel.ChannelID || downstream.SourceID != gb10DeviceID || downstream.SourceApp != cascadeSourceApp || downstream.SourceStream == "" {
		t.Fatalf("downstream Broadcast input = %+v", downstream)
	}
	if businessResponse == nil {
		t.Fatal("upstream Broadcast did not receive a business response")
	}
	businessBody := string(businessResponse.Body())
	for _, expected := range []string{"<Response>", "<CmdType>Broadcast</CmdType>", "<SN>77</SN>", "<DeviceID>" + testExposedChannelID + "</DeviceID>", "<Result>OK</Result>"} {
		if !strings.Contains(businessBody, expected) {
			t.Fatalf("Broadcast business response missing %q: %s", expected, businessBody)
		}
	}

	value, ok := api.broadcastSessions.Load(testCascadeChannelID)
	if !ok {
		t.Fatal("cascade Broadcast session was not retained")
	}
	session := value.(*broadcastSession)
	if session.Cascade == nil || session.SourceStream != downstream.SourceStream {
		t.Fatalf("cascade Broadcast session = %+v", session)
	}
	media.mu.Lock()
	started := media.started
	media.mu.Unlock()
	if started.Stream != downstream.SourceStream || started.DstURL != "192.0.2.40" || started.DstPort != 41000 || started.Type != broadcastRTPTypePS || !started.OnlyAudio {
		t.Fatalf("downstream Broadcast RTP = %+v", started)
	}
	bye := newCascadeBroadcastReceiverRequest(t, connection, sip.MethodBYE, "cascade-voice-receiver", session.SourceID, session.ChannelID, nil)
	dialogValue, ok := api.inviteDialogs.Load("cascade-voice-receiver")
	if !ok {
		t.Fatal("downstream Broadcast dialog not retained")
	}
	applyInboundDialogTags(t, bye, dialogValue.(*inboundInviteDialog), true)
	api.sipByeGeneric(&sip.Context{
		Request: bye, Tx: sip.NewTransaction("cascade-voice-receiver-bye", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("downstream Broadcast BYE response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("downstream Broadcast BYE response timeout")
	}
	if upstreamBYE == nil {
		t.Fatal("stopping cascade Broadcast did not terminate upstream source dialog")
	}
	media.mu.Lock()
	closed := media.closed
	media.mu.Unlock()
	if closed.StreamID != downstream.SourceStream {
		t.Fatalf("closed upstream voice receiver = %+v", closed)
	}
}

func newCascadeBroadcastReceiverRequest(t *testing.T, connection sip.Connection, method, callID, sourceID, channelID string, body []byte) *sip.Request {
	t.Helper()
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example")
	local := mustFlowAddress(t, "sip:"+sourceID+"@local.example")
	call := sip.CallID(callID)
	builder := sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(method).SetCallID(&call).
		AddVia(&sip.ViaHop{Host: "192.0.2.40", Port: sip.NewPort(5060), Transport: "UDP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})})
	if len(body) > 0 {
		builder.SetContentType(&sip.ContentTypeSDP)
	}
	request := sip.NewRequest("", method, local.URI, sip.DefaultSipVersion, builder.Build(), body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: buildGBInviteSubject(sourceID, "0100000003", channelID)})
	request.SetConnection(connection)
	request.SetSource(connection.RemoteAddr())
	request.SetDestination(connection.LocalAddr())
	return request
}

func applyInboundDialogTags(t *testing.T, request *sip.Request, dialog *inboundInviteDialog, established bool) {
	t.Helper()
	if request == nil || dialog == nil {
		t.Fatal("dialog tag test input is nil")
	}
	dialog.mu.Lock()
	remoteTag := dialog.RemoteTag
	toTag := dialog.InitialToTag
	if established {
		toTag = dialog.LocalTag
	}
	dialog.mu.Unlock()
	from, ok := request.From()
	if !ok || from == nil || from.Address == nil {
		t.Fatal("dialog request From is unavailable")
	}
	to, ok := request.To()
	if !ok || to == nil || to.Address == nil {
		t.Fatal("dialog request To is unavailable")
	}
	fromAddress := &sip.Address{DisplayName: from.DisplayName, URI: from.Address.Clone(), Params: sip.NewParams()}
	if remoteTag != "" {
		fromAddress.Params.Add("tag", sip.String{Str: remoteTag})
	}
	toAddress := &sip.Address{DisplayName: to.DisplayName, URI: to.Address.Clone(), Params: sip.NewParams()}
	if toTag != "" {
		toAddress.Params.Add("tag", sip.String{Str: toTag})
	}
	request.RemoveHeader("From")
	request.AppendHeader(&sip.FromHeader{DisplayName: fromAddress.DisplayName, Address: fromAddress.URI, Params: fromAddress.Params})
	request.RemoveHeader("To")
	request.AppendHeader(&sip.ToHeader{DisplayName: toAddress.DisplayName, Address: toAddress.URI, Params: toAddress.Params})
}

func TestCascadeBroadcast2011ReturnsBusinessError(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion10
	worker.mu.Unlock()
	var response *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		response = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	err := api.forwardCascadeBroadcast(t.Context(), worker, cascadeQueryEnvelope{
		CmdType: "Broadcast", SN: 78, SourceID: "34020000001360000021", TargetID: testExposedChannelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response == nil || !strings.Contains(string(response.Body()), "<Result>ERROR</Result>") {
		t.Fatalf("2011 Broadcast response = %#v", response)
	}
}

func TestCascadeBroadcastMiddlewareRoutesSharedNotify(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion10
	worker.mu.Unlock()
	responses := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		responses <- request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{}
	connection := newFlowConnection()
	connection.remote = platform.remote
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@"+platform.remoteDomain)
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@"+platform.localDomain)
	callID := sip.CallID("cascade-broadcast-notify")
	body, err := sip.XMLEncode(broadcastNotify{
		CmdType: "Broadcast", SN: 79, SourceID: "34020000001360000021", TargetID: testExposedChannelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest("", sip.MethodMessage, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodMessage).SetCallID(&callID).SetContentType(&sip.ContentTypeXML).Build(), body)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-broadcast-notify", connection),
		DeviceID: platform.serverID, Source: connection.remote, Log: slog.Default(),
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("Broadcast notify SIP response: %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("Broadcast notify SIP response timeout")
	}
	select {
	case response := <-responses:
		if !strings.Contains(string(response.Body()), "<Result>ERROR</Result>") {
			t.Fatalf("2011 Broadcast business response: %s", response.Body())
		}
	case <-time.After(time.Second):
		t.Fatal("Broadcast business response timeout")
	}
}

func TestCascadeBroadcastRejectsUnsharedTarget(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	if cascadeBroadcastTargetAllowed(platform, "34020000001320000099") {
		t.Fatal("unshared Broadcast target was accepted")
	}
	if !cascadeBroadcastTargetAllowed(platform, testExposedChannelID) {
		t.Fatal("shared Broadcast target was rejected")
	}
	if _, _, err := resolveCascadeBroadcastChannel(nil, platform, testExposedChannelID); err == nil {
		t.Fatal("nil API resolved a cascade Broadcast channel")
	}
}

func TestCascadeBroadcastInviteSelectsSessionBySubjectReceiver(t *testing.T) {
	api := &GB28181API{}
	first := &broadcastSession{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	second := &broadcastSession{DeviceID: gb10DeviceID, ChannelID: "34020000001320000012"}
	api.broadcastSessions.Store(first.ChannelID, first)
	api.broadcastSessions.Store(second.ChannelID, second)
	if got := api.findBroadcastSessionForInvite(gb10DeviceID, gb10DeviceID+":1,"+second.ChannelID+":0"); got != second {
		t.Fatalf("selected Broadcast session = %p, want %p", got, second)
	}
	if got := api.findBroadcastSessionForInvite(gb10DeviceID, gb10DeviceID+":1,34020000001320000099:0"); got != nil {
		t.Fatalf("unknown Subject receiver selected session: %+v", got)
	}
}

func TestCascadeBroadcastReceiverCancelStopsBothLegs(t *testing.T) {
	connection := newFlowConnection()
	media := &fakeRTPMediaService{}
	api := &GB28181API{sms: media, streams: &conc.Map[string, *Streams]{}}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(session.ChannelID, session)
	invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-cancel", []byte("offer"))
	inviteTx := sip.NewTransaction("cascade-voice-invite", connection)
	dialog := &inboundInviteDialog{
		CallID: "cascade-voice-cancel", DeviceID: gb10DeviceID, Request: invite,
		Broadcast: session, InviteTx: inviteTx,
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)
	cancel := newFlowRequest(t, connection, sip.MethodCancel, dialog.CallID, nil)
	cancel.RemoveHeader("From")
	if from, ok := invite.From(); ok {
		cancel.AppendHeader(from.Clone())
	}
	api.sipCancelGeneric(&sip.Context{
		Request: cancel, Tx: sip.NewTransaction("cascade-voice-cancel", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	})

	responses := make([]string, 0, 2)
	for len(responses) < 2 {
		select {
		case payload := <-connection.writes:
			responses = append(responses, string(payload))
		case <-time.After(time.Second):
			t.Fatalf("Broadcast CANCEL responses = %v", responses)
		}
	}
	joined := strings.Join(responses, "\n")
	if !strings.Contains(joined, "SIP/2.0 200 OK") || !strings.Contains(joined, "SIP/2.0 487 Request Terminated") {
		t.Fatalf("Broadcast CANCEL responses:\n%s", joined)
	}
	if _, ok := api.inviteDialogs.Load(dialog.CallID); ok {
		t.Fatal("Broadcast CANCEL left inbound dialog")
	}
	if _, ok := api.broadcastSessions.Load(session.ChannelID); ok {
		t.Fatal("Broadcast CANCEL left media session")
	}
	select {
	case err := <-session.ready:
		if err == nil || !strings.Contains(err.Error(), "cancelled") {
			t.Fatalf("Broadcast CANCEL completion = %v", err)
		}
	default:
		t.Fatal("Broadcast CANCEL did not unblock waiting session")
	}
}

func TestCascadeBroadcastUpstreamBYEStopsReceiverSession(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	media := &fakeRTPMediaService{}
	api := &GB28181API{sms: media, streams: &conc.Map[string, *Streams]{}}
	const callID = "cascade-voice-upstream"
	source := &cascadeVoiceSourceSession{
		worker: worker, server: &sms.MediaServer{}, streamID: "cascade-voice-source",
		sourceID: "34020000001360000021", callID: callID, opened: true,
	}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID,
		Stream: &Streams{}, Cascade: source, ready: make(chan error, 1),
	}
	source.broadcast = session
	api.cascadeVoiceDialogs.Store(callID, source)
	api.broadcastSessions.Store(session.ChannelID, session)

	wrongRemote := mustFlowAddress(t, "sip:34020000001360000022@remote.example")
	local := mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example")
	call := sip.CallID(callID)
	wrongRequest := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(wrongRemote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&call).Build(), nil)
	wrongRequest.SetConnection(connection)
	wrongRequest.SetSource(connection.remote)
	wrongRequest.SetDestination(connection.local)
	api.sipByeGeneric(&sip.Context{
		Request: wrongRequest, Tx: sip.NewTransaction("cascade-voice-wrong-bye", connection),
		DeviceID: "34020000001360000022", Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403 cascade voice dialog source mismatch") {
			t.Fatalf("cross-source BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-source BYE response timeout")
	}
	if _, ok := api.cascadeVoiceDialogs.Load(callID); !ok {
		t.Fatal("cross-source BYE removed source dialog")
	}

	remote := mustFlowAddress(t, "sip:"+source.sourceID+"@remote.example")
	request := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&call).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	api.sipByeGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-voice-upstream-bye", connection),
		DeviceID: source.sourceID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("upstream BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream BYE response timeout")
	}
	if _, ok := api.cascadeVoiceDialogs.Load(callID); ok {
		t.Fatal("upstream BYE left source dialog")
	}
	if _, ok := api.broadcastSessions.Load(session.ChannelID); ok {
		t.Fatal("upstream BYE left receiver session")
	}
	media.mu.Lock()
	closed := media.closed
	media.mu.Unlock()
	if closed.StreamID != source.streamID {
		t.Fatalf("upstream BYE closed receiver = %+v", closed)
	}
}

func TestCascadeBroadcastUpstreamInviteSupportsDigest(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.password = "secret"
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	const sourceID = "34020000001360000021"
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		if len(request.GetHeaders("Authorization")) == 0 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{
				HeaderName: "WWW-Authenticate",
				Contents:   `Digest realm="3402000000",qop="auth",nonce="voice-nonce"`,
			})
			return response, nil
		}
		answer := []byte("v=0\r\no=" + sourceID + " 0 0 IN IP4 192.0.2.30\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 40000 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\n")
		response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", answer)
		response.AppendHeader(&sip.ContentTypeSDP)
		return response, nil
	}
	worker.send = func(*sip.Request) error { return nil }
	media := &fakeRTPMediaService{
		openPort:   30000,
		mediaItems: []zlm.MediaItem{{Tracks: []zlm.MediaTrack{{CodecType: 1, CodecIDName: "G711A", Ready: true}}}},
	}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media}
	source, err := api.startCascadeVoiceSource(t.Context(), worker, &sms.MediaServer{SDPIP: "192.0.2.20"}, cascadeQueryEnvelope{
		CmdType: "Broadcast", SN: 80, SourceID: sourceID, TargetID: testExposedChannelID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = api.stopCascadeVoiceSource(source, false) }()
	if len(requests) != 2 {
		t.Fatalf("Digest INVITE requests = %d", len(requests))
	}
	firstCallID, _ := requests[0].CallID()
	secondCallID, _ := requests[1].CallID()
	if normalizeCallID(firstCallID) == "" || normalizeCallID(firstCallID) != normalizeCallID(secondCallID) {
		t.Fatalf("Digest INVITE Call-ID changed: %v / %v", firstCallID, secondCallID)
	}
	firstCSeq, _ := requests[0].CSeq()
	secondCSeq, _ := requests[1].CSeq()
	if firstCSeq.SeqNo != 1 || secondCSeq.SeqNo != 2 {
		t.Fatalf("Digest INVITE CSeq = %d / %d", firstCSeq.SeqNo, secondCSeq.SeqNo)
	}
	authHeaders := requests[1].GetHeaders("Authorization")
	if len(authHeaders) != 1 {
		t.Fatalf("Digest Authorization headers = %v", authHeaders)
	}
	auth := sip.AuthFromValue(authHeaders[0].String())
	if auth.Get("username") != gb10DeviceID || auth.Get("uri") != "sip:"+sourceID+"@remote.example" || auth.Get("response") == "" {
		t.Fatalf("Digest Authorization = %s", authHeaders[0].String())
	}
}
