package gbs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
)

type observingCascadeVoiceMediaResolver struct {
	called chan string
	err    error
}

func (r *observingCascadeVoiceMediaResolver) GetMediaServer(_ context.Context, id string) (*sms.MediaServer, error) {
	r.called <- id
	return nil, r.err
}

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
			answer := []byte("v=0\r\no=" + upstreamSourceID + " 0 0 IN IP4 192.0.2.30\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 40000 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\ny=" + directTCPSDPLineValue(request.Body(), "y") + "\r\nf=v/////a/1/8/1\r\n")
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
		"Subject: " + upstreamSourceID + ":", "," + testExposedChannelID + ":",
	} {
		if !strings.Contains(upstreamInvite.String(), expected) && !strings.Contains(inviteBody, expected) {
			t.Fatalf("upstream source INVITE missing %q:\n%s", expected, upstreamInvite.String())
		}
	}
	subjectHeaders := upstreamInvite.GetHeaders("Subject")
	if len(subjectHeaders) != 1 {
		t.Fatalf("upstream source INVITE Subject = %v", subjectHeaders)
	}
	subject := strings.TrimSpace(strings.TrimPrefix(subjectHeaders[0].String(), "Subject:"))
	parts := strings.Split(subject, ",")
	if len(parts) != 2 {
		t.Fatalf("upstream source INVITE Subject = %q", subject)
	}
	sender := strings.SplitN(parts[0], ":", 2)
	receiver := strings.SplitN(parts[1], ":", 2)
	if len(sender) != 2 || len(receiver) != 2 || sender[1] == "" || sender[1] != receiver[1] {
		t.Fatalf("upstream source INVITE Subject sequences = %q", subject)
	}
	if upstreamACK == nil || upstreamACK.Method() != sip.MethodACK {
		t.Fatalf("upstream source ACK = %#v", upstreamACK)
	}
	if got := firstSingleHeaderValue(upstreamACK, "X-GB-Ver"); got != string(GBVersion11) {
		t.Fatalf("upstream source ACK X-GB-Ver = %q, want %q", got, GBVersion11)
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
	if got := firstSingleHeaderValue(upstreamBYE, "X-GB-Ver"); got != string(GBVersion11) {
		t.Fatalf("upstream source BYE X-GB-Ver = %q, want %q", got, GBVersion11)
	}
	media.mu.Lock()
	closed := media.closed
	media.mu.Unlock()
	if closed.StreamID != downstream.SourceStream {
		t.Fatalf("closed upstream voice receiver = %+v", closed)
	}
}

func TestCascadeBroadcastResolvesLatestMediaBindingInsideChannelLock(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	runtimeDevice := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	if err := runtimeChannel.init("local.example"); err != nil {
		t.Fatal(err)
	}
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}, channel: runtimeChannel}
	resolver := &observingCascadeVoiceMediaResolver{called: make(chan string, 1), err: errors.New("stop after media resolution")}
	server := &Server{memoryStorer: memory, mediaService: resolver}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, core: adapter, svr: server,
		sms: &fakeRTPMediaService{}, streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef),
	}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if request.Method() != sip.MethodMessage {
			t.Fatalf("unexpected request while media resolution fails: %s", request.Method())
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	unlock, err := runtimeDevice.lockMediaContext(t.Context(), testCascadeChannelID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- api.forwardCascadeBroadcast(t.Context(), worker, cascadeQueryEnvelope{
			CmdType: "Broadcast", SN: 78, SourceID: "34020000001360000022", TargetID: testExposedChannelID,
		})
	}()

	select {
	case id := <-resolver.called:
		unlock()
		t.Fatalf("media server %q was resolved before acquiring the channel lock", id)
	case <-time.After(20 * time.Millisecond):
	}
	var updated ipc.Channel
	if err := adapter.Store().Channel().Update(t.Context(), &updated, func(channel *ipc.Channel) error {
		channel.Config.MediaServerID = "edge-zlm-1"
		return nil
	}, orm.Where("id = ?", persistentChannel.ID)); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()

	select {
	case id := <-resolver.called:
		if id != "edge-zlm-1" {
			t.Fatalf("resolved media server = %q, want latest binding edge-zlm-1", id)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade Broadcast did not resolve media server after lock release")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade Broadcast did not finish after media resolution failure")
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
	initialCSeq := dialog.InitialRemoteCSeq
	initialCSeqSet := dialog.InitialRemoteCSeqSet
	remoteCSeq := dialog.RemoteCSeq
	remoteCSeqSet := dialog.RemoteCSeqSet
	dialogRequest := dialog.Request
	if established {
		toTag = dialog.LocalTag
	}
	dialog.mu.Unlock()
	if !initialCSeqSet {
		if cseq, ok := dialogRequest.CSeq(); ok && cseq != nil {
			initialCSeq = cseq.SeqNo
			initialCSeqSet = true
		}
	}
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
	if cseq, ok := request.CSeq(); ok && cseq != nil && initialCSeqSet {
		if request.IsAck() || request.IsCancel() {
			cseq.SeqNo = initialCSeq
		} else if remoteCSeqSet {
			cseq.SeqNo = remoteCSeq + 1
		} else {
			cseq.SeqNo = initialCSeq + 1
		}
	}
}

func applyOutboundDialogTags(t *testing.T, request *sip.Request, response *sip.Response) {
	t.Helper()
	if request == nil || response == nil {
		t.Fatal("outbound dialog tag test input is nil")
	}
	from, ok := request.From()
	if !ok || from == nil || from.Address == nil {
		t.Fatal("outbound dialog request From is unavailable")
	}
	to, ok := request.To()
	if !ok || to == nil || to.Address == nil {
		t.Fatal("outbound dialog request To is unavailable")
	}
	request.RemoveHeader("From")
	request.AppendHeader(&sip.FromHeader{
		DisplayName: from.DisplayName, Address: from.Address.Clone(),
		Params: sip.NewParams().Add("tag", sip.String{Str: sipResponseToTag(response)}),
	})
	request.RemoveHeader("To")
	request.AppendHeader(&sip.ToHeader{
		DisplayName: to.DisplayName, Address: to.Address.Clone(),
		Params: sip.NewParams().Add("tag", sip.String{Str: sipResponseFromTag(response)}),
	})
}

func TestCascadeBroadcast2011ReturnsBusinessError(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
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

func TestCascadeBroadcastMiddlewareRejectsMalformedNotifyBeforeSIPOK(t *testing.T) {
	base := `<Notify><CmdType>Broadcast</CmdType><SN>79</SN><SourceID>34020000001360000021</SourceID><TargetID>` + testExposedChannelID + `</TargetID></Notify>`
	tests := []struct {
		name string
		body string
	}{
		{name: "unknown field", body: strings.Replace(base, "</Notify>", "<Vendor>1</Vendor></Notify>", 1)},
		{name: "duplicate target", body: strings.Replace(base, "</Notify>", "<TargetID>"+testExposedChannelID+"</TargetID></Notify>", 1)},
		{name: "root attribute", body: strings.Replace(base, "<Notify>", `<Notify vendor="1">`, 1)},
		{name: "root namespace", body: strings.Replace(strings.Replace(base, "<Notify>", `<gb:Notify xmlns:gb="urn:vendor">`, 1), "</Notify>", "</gb:Notify>", 1)},
		{name: "simple field attribute", body: strings.Replace(base, "<SourceID>", `<SourceID vendor="1">`, 1)},
		{name: "simple field nesting", body: strings.Replace(base, "34020000001360000021", "<Value>34020000001360000021</Value>", 1)},
		{name: "out of order", body: `<Notify><CmdType>Broadcast</CmdType><SourceID>34020000001360000021</SourceID><SN>79</SN><TargetID>` + testExposedChannelID + `</TargetID></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			worker := newCascadeWorker(nil, platform)
			worker.mu.Lock()
			worker.effective = GBVersion11
			worker.mu.Unlock()
			forwarded := false
			worker.exchange = func(context.Context, *sip.Request) (*sip.Response, error) {
				forwarded = true
				return nil, nil
			}
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-malformed-broadcast-"+test.name, []byte(test.body))
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("cascade-malformed-broadcast-"+test.name, connection),
				DeviceID: platform.serverID, Source: connection.remote,
			}
			ctx.Set(cascadeWorkerContextKey, worker)
			(&GB28181API{}).sipCascadeMessageMiddleware(ctx)
			select {
			case response := <-connection.writes:
				if !strings.Contains(string(response), "SIP/2.0 400") || strings.Contains(string(response), "SIP/2.0 200") {
					t.Fatalf("malformed Broadcast response = %s", response)
				}
			case <-time.After(time.Second):
				t.Fatal("malformed Broadcast response timeout")
			}
			if forwarded {
				t.Fatal("malformed Broadcast was forwarded")
			}
		})
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
	first := &broadcastSession{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, SourceID: gb10PlatformID}
	second := &broadcastSession{DeviceID: gb10DeviceID, ChannelID: "34020000001320000012", SourceID: gb10PlatformID}
	api.broadcastSessions.Store(first.ChannelID, first)
	api.broadcastSessions.Store(second.ChannelID, second)
	request := newFlowRequest(t, newFlowConnection(), sip.MethodInvite, "broadcast-subject", []byte("offer"))
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: gb10PlatformID + ":voice-1," + second.ChannelID + ":speaker-1"})
	if got, err := api.findBroadcastSessionForInvite(gb10DeviceID, request); err != nil || got != second {
		t.Fatalf("selected Broadcast session = %p, want %p", got, second)
	}
	request.RemoveHeader("Subject")
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: gb10PlatformID + ":voice-1,34020000001320000099:speaker-1"})
	if got, err := api.findBroadcastSessionForInvite(gb10DeviceID, request); err != nil || got != nil {
		t.Fatalf("unknown Subject receiver selected session: %+v", got)
	}
	request.RemoveHeader("Subject")
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: gb10DeviceID + ":voice-1," + second.ChannelID + ":speaker-1"})
	if got, err := api.findBroadcastSessionForInvite(gb10DeviceID, request); got != second || err == nil || !strings.Contains(err.Error(), "media source") {
		t.Fatalf("mismatched Subject result = %+v, %v", got, err)
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
	cancel.RemoveHeader("Via")
	sip.CopyHeaders("Via", invite, cancel)
	if inviteCSeq, ok := invite.CSeq(); ok && inviteCSeq != nil {
		if cancelCSeq, ok := cancel.CSeq(); ok && cancelCSeq != nil {
			cancelCSeq.SeqNo = inviteCSeq.SeqNo
		}
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

func TestCascadeBroadcastReceiverCancelRetainsDialogUntil487Retry(t *testing.T) {
	connection := newFlowConnection()
	inviteConnection := &failFirstFlowResponseConnection{flowConnection: connection}
	media := &fakeRTPMediaService{}
	api := &GB28181API{sms: media, streams: &conc.Map[string, *Streams]{}}
	session := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, SourceID: gb10PlatformID,
		Stream: &Streams{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}, ready: make(chan error, 1),
	}
	api.broadcastSessions.Store(session.ChannelID, session)
	invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-cancel-487-retry", []byte("offer"))
	invite.SetConnection(inviteConnection)
	inviteTx := sip.NewTransaction("cascade-voice-invite-487-retry", inviteConnection)
	t.Cleanup(inviteTx.Close)
	dialog := &inboundInviteDialog{
		CallID: "cascade-voice-cancel-487-retry", DeviceID: gb10DeviceID, Request: invite,
		Broadcast: session, InviteTx: inviteTx,
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)
	cancel := newFlowRequest(t, connection, sip.MethodCancel, dialog.CallID, nil)
	cancel.RemoveHeader("From")
	if from, ok := invite.From(); ok {
		cancel.AppendHeader(from.Clone())
	}
	cancel.RemoveHeader("Via")
	sip.CopyHeaders("Via", invite, cancel)
	if inviteCSeq, ok := invite.CSeq(); ok && inviteCSeq != nil {
		if cancelCSeq, ok := cancel.CSeq(); ok && cancelCSeq != nil {
			cancelCSeq.SeqNo = inviteCSeq.SeqNo
		}
	}
	cancelTx := sip.NewTransaction("cascade-voice-cancel-487-retry", connection)
	t.Cleanup(cancelTx.Close)
	api.sipCancelGeneric(&sip.Context{
		Request: cancel, Tx: cancelTx,
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	})

	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("Broadcast CANCEL response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("Broadcast CANCEL 200 response timeout")
	}
	if current, ok := api.inviteDialogs.Load(dialog.CallID); !ok || current != dialog {
		t.Fatal("failed Broadcast 487 write lost the cancelled dialog")
	}

	retransmission := invite.Clone().(*sip.Request)
	retransmission.SetConnection(inviteConnection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	api.sipInviteBroadcast(&sip.Context{
		Request: retransmission, Tx: inviteTx,
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	}, dialog.CallID, session)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 487 Request Terminated") {
			t.Fatalf("Broadcast retried INVITE termination = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("Broadcast retried INVITE 487 timeout")
	}
	if _, ok := api.inviteDialogs.Load(dialog.CallID); ok {
		t.Fatal("successful Broadcast 487 retry retained the cancelled dialog")
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

func TestCascadeVoiceBYECommitsOnlyAfterSuccessfulSIPOK(t *testing.T) {
	base := newFlowConnection()
	base.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	connection := &blockingFlowResponseConnection{
		flowConnection: base,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("cascade voice BYE SIP OK write failed"),
	}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	const callID = "cascade-voice-bye-write-failure"
	source := &cascadeVoiceSourceSession{
		worker: worker, streamID: "cascade-voice-write-failure", sourceID: "34020000001360000021", callID: callID,
	}
	api := &GB28181API{}
	api.cascadeVoiceDialogs.Store(callID, source)
	remote := mustFlowAddress(t, "sip:"+source.sourceID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example")
	call := sip.CallID(callID)
	request := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&call).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(base.remote)
	request.SetDestination(base.local)
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipByeGeneric(&sip.Context{
			Request: request, Tx: sip.NewTransaction("cascade-voice-bye-write-failure-tx", connection),
			DeviceID: source.sourceID, Source: base.remote, Log: slog.Default(),
		})
	}()

	select {
	case <-connection.started:
	case <-time.After(time.Second):
		close(connection.release)
		t.Fatal("cascade voice BYE SIP response write did not start")
	}
	source.mu.Lock()
	upstreamEnd := source.upstreamEnd
	source.mu.Unlock()
	if upstreamEnd {
		close(connection.release)
		<-done
		t.Fatal("cascade voice BYE committed before SIP OK completed")
	}
	close(connection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cascade voice BYE handler did not return after SIP OK write failure")
	}
	source.mu.Lock()
	upstreamEnd = source.upstreamEnd
	source.mu.Unlock()
	if upstreamEnd {
		t.Fatal("cascade voice BYE committed after SIP OK write failure")
	}
	if current, ok := api.cascadeVoiceDialogs.Load(callID); !ok || current != source {
		t.Fatal("cascade voice dialog was removed after SIP OK write failure")
	}
}

func TestCascadeVoiceBYERejectsMismatchedDialogTags(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	remote := mustFlowAddress(t, "sip:"+worker.platform.serverID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+worker.platform.localID+"@local.example")
	callID := sip.CallID("cascade-voice-tags")
	invite := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetMethod(sip.MethodInvite).SetCallID(&callID).Build(), nil)
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
	response.SetSource(connection.remote)
	source := &cascadeVoiceSourceSession{worker: worker, callID: string(callID), sourceID: worker.platform.serverID, response: response}
	api := &GB28181API{}
	api.cascadeVoiceDialogs.Store(source.callID, source)
	makeBYE := func() *sip.Request {
		request := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&callID).Build(), nil)
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		return request
	}
	wrong := makeBYE()
	api.sipByeGeneric(&sip.Context{
		Request: wrong, Tx: sip.NewTransaction("cascade-voice-tags-wrong", connection),
		DeviceID: worker.platform.serverID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403") {
			t.Fatalf("mismatched cascade voice BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("mismatched cascade voice BYE response timeout")
	}
	if _, ok := api.cascadeVoiceDialogs.Load(source.callID); !ok {
		t.Fatal("mismatched cascade voice BYE removed dialog")
	}

	correct := makeBYE()
	applyOutboundDialogTags(t, correct, response)
	api.sipByeGeneric(&sip.Context{
		Request: correct, Tx: sip.NewTransaction("cascade-voice-tags-correct", connection),
		DeviceID: worker.platform.serverID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("owner cascade voice BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("owner cascade voice BYE response timeout")
	}
	if _, ok := api.cascadeVoiceDialogs.Load(source.callID); ok {
		t.Fatal("owner cascade voice BYE left dialog")
	}
}

func TestStopCascadeVoiceSourceClosesRTPWhenIdentityForwardingFails(t *testing.T) {
	media := &fakeRTPMediaService{}
	connection := newFlowConnection()
	invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-identity-cleanup", []byte("offer"))
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)

	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	var sentCSeq []uint32
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if cseq, ok := request.CSeq(); ok && cseq != nil {
			sentCSeq = append(sentCSeq, cseq.SeqNo)
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	worker.platform.monitorUserIdentity = &monitorUserIdentityPolicy{
		localGatewayID: "34020000002110000001",
		remoteGateway:  "34030000002110000001",
		local: &monitorUserIdentity{
			Gateways: []string{"34020000002110000001"}, UserID: "34020000003000000001",
			Organization: "340200", Category: "operator", Rank: "level1",
		},
		maxHops: 8,
	}
	source := &cascadeVoiceSourceSession{
		worker: worker, server: &sms.MediaServer{}, streamID: "cascade-voice-identity-cleanup",
		callID: "cascade-voice-identity-cleanup", response: response, opened: true,
		identity: &monitorUserIdentity{
			Gateways: []string{"34040000002110000001"}, UserID: "34020000003000000001",
			Organization: "340200", Category: "operator", Rank: "level1",
		},
	}
	api := &GB28181API{sms: media}
	api.cascadeVoiceDialogs.Store(source.callID, source)

	err := api.stopCascadeVoiceSource(source, true)
	if err == nil || !strings.Contains(err.Error(), "immediate gateway mismatch") {
		t.Fatalf("identity forwarding failure = %v", err)
	}
	media.mu.Lock()
	closeCalls, closed := media.closeCalls, media.closed
	media.mu.Unlock()
	if closeCalls != 1 || closed.StreamID != source.streamID {
		t.Fatalf("RTP cleanup after identity failure = calls:%d request:%+v", closeCalls, closed)
	}
	if current, ok := api.cascadeVoiceDialogs.Load(source.callID); !ok || current != source {
		t.Fatal("identity forwarding failure removed the retryable cascade voice dialog")
	}
	source.identity = nil
	if err := api.stopCascadeVoiceSource(source, true); err != nil {
		t.Fatalf("retry cascade voice cleanup: %v", err)
	}
	if len(sentCSeq) != 1 || sentCSeq[0] != 2 {
		t.Fatalf("cascade voice BYE after identity retry CSeq = %v, want [2]", sentCSeq)
	}
	if _, ok := api.cascadeVoiceDialogs.Load(source.callID); ok {
		t.Fatal("successful cascade voice cleanup retained dialog")
	}
	media.mu.Lock()
	closeCalls = media.closeCalls
	media.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("retry repeated successful RTP cleanup %d times", closeCalls)
	}
}

func TestStartCascadeVoiceSourceRetainsPreDialogCleanupFailure(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	closeErr := errors.New("close pre-dialog cascade voice receiver")
	media := &fakeRTPMediaService{openPort: 30000, closeErr: closeErr}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media, streams: &conc.Map[string, *Streams]{}}

	_, err := api.startCascadeVoiceSource(t.Context(), worker, &sms.MediaServer{}, cascadeQueryEnvelope{
		CmdType: "Broadcast", SN: 81, SourceID: "34020000001360000021", TargetID: testExposedChannelID,
	})
	if err == nil || !strings.Contains(err.Error(), "输入为空") || !errors.Is(err, closeErr) {
		t.Fatalf("pre-dialog cascade voice error = %v", err)
	}
	var retained *cascadeVoiceSourceSession
	api.pendingCascadeVoiceCleanups.Range(func(key, value any) bool {
		source, keyOK := key.(*cascadeVoiceSourceSession)
		current, valueOK := value.(*cascadeVoiceSourceSession)
		if keyOK && valueOK && source == current {
			retained = source
		}
		return false
	})
	if retained == nil {
		t.Fatal("failed pre-dialog RTP cleanup lost cascade voice source ownership")
	}
	retained.mu.Lock()
	opened, stopping, callID := retained.opened, retained.stopping, retained.callID
	retained.mu.Unlock()
	if !opened || !stopping || callID != "" {
		t.Fatalf("retained pre-dialog source = opened:%v stopping:%v call_id:%q", opened, stopping, callID)
	}

	media.mu.Lock()
	media.closeErr = nil
	media.mu.Unlock()
	startVoiceRetryCleanerForTest(t, api)
	waitForVoiceCleanup(t, func() bool {
		_, exists := api.pendingCascadeVoiceCleanups.Load(retained)
		return !exists
	})
	retained.mu.Lock()
	opened = retained.opened
	retained.mu.Unlock()
	if opened {
		t.Fatal("successful retry retained pre-dialog RTP receiver ownership")
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("pre-dialog cascade voice close calls = %d, want 2", closeCalls)
	}
}

func TestStartCascadeVoiceSourceRejectsNilSIPResponse(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	worker.exchange = func(context.Context, *sip.Request) (*sip.Response, error) {
		return nil, nil
	}
	media := &fakeRTPMediaService{openPort: 30000}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media}

	_, err := api.startCascadeVoiceSource(t.Context(), worker, &sms.MediaServer{SDPIP: "192.0.2.20"}, cascadeQueryEnvelope{
		CmdType: "Broadcast", SN: 82, SourceID: "34020000001360000021", TargetID: testExposedChannelID,
	})
	if err == nil || !strings.Contains(err.Error(), "returned no SIP response") {
		t.Fatalf("nil cascade voice response error = %v", err)
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("nil-response cascade voice close calls = %d, want 1", closeCalls)
	}
	pending := false
	api.pendingCascadeVoiceCleanups.Range(func(_, _ any) bool {
		pending = true
		return false
	})
	if pending {
		t.Fatal("successful nil-response cleanup retained terminal source ownership")
	}
}

func TestStopCascadeVoiceSourceUsesShutdownCleanupContextForBYE(t *testing.T) {
	type cleanupContextKey struct{}
	shutdownCtx, shutdownCancel := context.WithTimeout(
		context.WithValue(context.Background(), cleanupContextKey{}, "shutdown"), time.Second,
	)
	defer shutdownCancel()

	connection := newFlowConnection()
	invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-shutdown-bye", []byte("offer"))
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	var marker any
	worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
		marker = ctx.Value(cleanupContextKey{})
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	source := &cascadeVoiceSourceSession{
		worker: worker, callID: "cascade-voice-shutdown-bye", response: response,
	}
	api := &GB28181API{lifecycleClosed: true, shutdownPersistenceCtx: shutdownCtx}
	api.cascadeVoiceDialogs.Store(source.callID, source)

	if err := api.stopCascadeVoiceSource(source, true); err != nil {
		t.Fatal(err)
	}
	if marker != "shutdown" {
		t.Fatalf("cascade voice BYE cleanup context marker = %v", marker)
	}
}

func TestStopCascadeVoiceSourceRetriesDigestChallengeThreeVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		for _, challenge := range cascadeMessageDigestChallenges() {
			t.Run(string(version)+"/"+challenge.name, func(t *testing.T) {
				platform := testSharedCascadePlatform(t)
				platform.password = "cascade-secret"
				policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
				if err != nil {
					t.Fatal(err)
				}
				platform.monitorUserIdentity = policy
				worker := newCascadeWorker(nil, platform)
				worker.mu.Lock()
				worker.effective = version
				worker.mu.Unlock()

				connection := newFlowConnection()
				invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-bye-digest-"+string(version)+"-"+challenge.name, []byte("offer"))
				response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
				requests := make([]*sip.Request, 0, 2)
				worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
					cloned, ok := request.Clone().(*sip.Request)
					if !ok || cloned == nil {
						t.Fatal("clone cascade Broadcast source BYE failed")
					}
					requests = append(requests, cloned)
					if len(requests) == 1 {
						challenged := sip.NewResponseFromRequest("", request, challenge.status, challenge.reason, nil)
						challenged.AppendHeader(&sip.GenericHeader{
							HeaderName: challenge.challengeHeader,
							Contents:   fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, challenge.realm, challenge.nonce),
						})
						return challenged, nil
					}
					return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
				}
				media := &fakeRTPMediaService{}
				source := &cascadeVoiceSourceSession{
					worker: worker, server: &sms.MediaServer{}, streamID: "cascade-voice-bye-digest",
					callID: "cascade-voice-bye-digest", response: response, opened: true,
				}
				api := &GB28181API{sms: media}
				api.cascadeVoiceDialogs.Store(source.callID, source)

				if err := api.stopCascadeVoiceSource(source, true); err != nil {
					t.Fatal(err)
				}
				assertCascadeVoiceBYEDigestRetry(t, requests, challenge, version)
				media.mu.Lock()
				closeCalls := media.closeCalls
				media.mu.Unlock()
				if closeCalls != 1 {
					t.Fatalf("Digest BYE CloseRTPServer calls = %d, want 1", closeCalls)
				}
			})
		}
	}
}

func TestStopCascadeVoiceSourceDigestRetriesOnlyOnceThreeVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		for _, challenge := range cascadeMessageDigestChallenges() {
			t.Run(string(version)+"/"+challenge.name, func(t *testing.T) {
				platform := testSharedCascadePlatform(t)
				platform.password = "cascade-secret"
				worker := newCascadeWorker(nil, platform)
				worker.mu.Lock()
				worker.effective = version
				worker.mu.Unlock()
				connection := newFlowConnection()
				invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-bye-repeated-digest-"+string(version)+"-"+challenge.name, nil)
				response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
				calls := 0
				worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
					calls++
					challenged := sip.NewResponseFromRequest("", request, challenge.status, challenge.reason, nil)
					challenged.AppendHeader(&sip.GenericHeader{
						HeaderName: challenge.challengeHeader,
						Contents:   fmt.Sprintf(`Digest realm="%s",nonce="%s-%d",algorithm=MD5`, challenge.realm, challenge.nonce, calls),
					})
					return challenged, nil
				}
				media := &fakeRTPMediaService{}
				source := &cascadeVoiceSourceSession{
					worker: worker, server: &sms.MediaServer{}, streamID: "cascade-voice-bye-repeated-digest",
					response: response, opened: true,
				}
				api := &GB28181API{sms: media}

				if err := api.stopCascadeVoiceSource(source, true); err == nil {
					t.Fatal("repeated Digest challenge unexpectedly succeeded")
				}
				if calls != 2 {
					t.Fatalf("repeated Digest challenge BYE count = %d, want 2", calls)
				}
				media.mu.Lock()
				closeCalls := media.closeCalls
				media.mu.Unlock()
				if closeCalls != 1 {
					t.Fatalf("repeated Digest challenge CloseRTPServer calls = %d, want 1", closeCalls)
				}
			})
		}
	}
}

func TestStopCascadeVoiceSourceDigestRetryReplacesSignalDigestHeaders(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.password = "cascade-secret"
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()

	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	security, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "cascade-bye-note-seed", Algorithm: "MD5", Encoding: "base64",
		Now: func() time.Time {
			current := now
			now = now.Add(time.Second)
			return current
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	connection := newFlowConnection()
	invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-bye-signal-digest", nil)
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if err := security.Sign(request); err != nil {
			t.Fatal(err)
		}
		cloned, ok := request.Clone().(*sip.Request)
		if !ok || cloned == nil {
			t.Fatal("clone signed cascade Broadcast source BYE failed")
		}
		requests = append(requests, cloned)
		if len(requests) == 1 {
			challenged := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			challenged.AppendHeader(&sip.GenericHeader{
				HeaderName: "WWW-Authenticate",
				Contents:   `Digest realm="remote.example",nonce="signed-bye-nonce",qop="auth"`,
			})
			return challenged, nil
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	source := &cascadeVoiceSourceSession{worker: worker, response: response}
	api := &GB28181API{}

	if err := api.stopCascadeVoiceSource(source, true); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("signed BYE request count = %d, want 2", len(requests))
	}
	firstDate := firstSingleHeaderValue(requests[0], "Date")
	secondDate := firstSingleHeaderValue(requests[1], "Date")
	firstNote := firstSingleHeaderValue(requests[0], "Note")
	secondNote := firstSingleHeaderValue(requests[1], "Note")
	if len(requests[0].GetHeaders("Date")) != 1 || len(requests[1].GetHeaders("Date")) != 1 ||
		len(requests[0].GetHeaders("Note")) != 1 || len(requests[1].GetHeaders("Note")) != 1 ||
		firstDate == "" || secondDate == "" || firstDate == secondDate || firstNote == "" || firstNote == secondNote {
		t.Fatalf("BYE signal Digest retry Date/Note = %q %q / %q %q", firstDate, firstNote, secondDate, secondNote)
	}
}

func assertCascadeVoiceBYEDigestRetry(t *testing.T, requests []*sip.Request, challenge cascadeMessageDigestChallenge, version GBProtocolVersion) {
	t.Helper()
	if len(requests) != 2 {
		t.Fatalf("BYE request count = %d, want 2", len(requests))
	}
	first, second := requests[0], requests[1]
	if first.Method() != sip.MethodBYE || second.Method() != sip.MethodBYE {
		t.Fatalf("Digest retry methods = %s/%s, want BYE", first.Method(), second.Method())
	}
	if len(first.GetHeaders("Authorization")) != 0 || len(first.GetHeaders("Proxy-Authorization")) != 0 {
		t.Fatalf("initial BYE credentials = Authorization:%v Proxy-Authorization:%v", first.GetHeaders("Authorization"), first.GetHeaders("Proxy-Authorization"))
	}
	authHeaders := second.GetHeaders(challenge.authorizeHeader)
	otherHeader := "Authorization"
	if challenge.authorizeHeader == otherHeader {
		otherHeader = "Proxy-Authorization"
	}
	if len(authHeaders) != 1 || len(second.GetHeaders(otherHeader)) != 0 {
		t.Fatalf("authenticated BYE credentials = %s:%v %s:%v", challenge.authorizeHeader, authHeaders, otherHeader, second.GetHeaders(otherHeader))
	}
	auth := sip.AuthFromValue(authHeaders[0].String())
	expected := sip.CalcResponse(
		gb10DeviceID, challenge.realm, "cascade-secret", sip.MethodBYE,
		second.Recipient().String(), challenge.nonce, "auth", auth.Get("cnonce"), "00000001",
	)
	if auth.Get("username") != gb10DeviceID || auth.Get("response") != expected || auth.Get("nc") != "00000001" {
		t.Fatalf("authenticated BYE credentials = %s", authHeaders[0].String())
	}
	firstCallID, _ := first.CallID()
	secondCallID, _ := second.CallID()
	firstCSeq, _ := first.CSeq()
	secondCSeq, _ := second.CSeq()
	if firstCallID == nil || secondCallID == nil || *firstCallID != *secondCallID ||
		firstCSeq == nil || secondCSeq == nil || firstCSeq.MethodName != sip.MethodBYE ||
		secondCSeq.SeqNo != firstCSeq.SeqNo+1 || secondCSeq.MethodName != sip.MethodBYE {
		t.Fatalf("BYE retry identity = Call-ID %v/%v CSeq %v/%v", firstCallID, secondCallID, firstCSeq, secondCSeq)
	}
	firstVia, firstViaOK := first.ViaHop()
	secondVia, secondViaOK := second.ViaHop()
	firstBranch, firstBranchOK := sipParamString(firstVia, "branch")
	secondBranch, secondBranchOK := sipParamString(secondVia, "branch")
	if !firstViaOK || !secondViaOK || !firstBranchOK || !secondBranchOK || firstBranch == secondBranch ||
		firstVia.Host != secondVia.Host || firstVia.Transport != secondVia.Transport {
		t.Fatalf("BYE retry Via = %v/%v branches %q/%q", firstVia, secondVia, firstBranch, secondBranch)
	}
	if first.Recipient().String() != second.Recipient().String() ||
		firstSingleHeaderValue(first, "From") != firstSingleHeaderValue(second, "From") ||
		firstSingleHeaderValue(first, "To") != firstSingleHeaderValue(second, "To") {
		t.Fatal("authenticated BYE changed Request-URI or dialog tags")
	}
	if firstSingleHeaderValue(first, "X-GB-Ver") != string(version) ||
		firstSingleHeaderValue(second, "X-GB-Ver") != string(version) {
		t.Fatalf("BYE X-GB-Ver = %q/%q, want %q",
			firstSingleHeaderValue(first, "X-GB-Ver"), firstSingleHeaderValue(second, "X-GB-Ver"), version)
	}
	firstIdentity := firstSingleHeaderValue(first, monitorUserIdentityHeaderName)
	secondIdentity := firstSingleHeaderValue(second, monitorUserIdentityHeaderName)
	if firstIdentity == "" || firstIdentity != secondIdentity {
		t.Fatalf("BYE Monitor-User-Identity = %q/%q", firstIdentity, secondIdentity)
	}
}

func TestOutboundDeviceBYERejectsMismatchedDialogTags(t *testing.T) {
	connection := newFlowConnection()
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@local.example")
	callID := sip.CallID("device-outbound-tags")
	invite := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetMethod(sip.MethodInvite).SetCallID(&callID).Build(), nil)
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
	response.SetSource(connection.remote)
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}}
	streamKey := "history:Playback:" + gb10DeviceID + ":" + gb10ChannelID
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, StreamID: "outbound-tags", CallID: string(callID), Resp: response}
	api.streams.Store(streamKey, stream)
	makeBYE := func() *sip.Request {
		request := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&callID).Build(), nil)
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		return request
	}
	wrong := makeBYE()
	api.sipByeGeneric(&sip.Context{
		Request: wrong, Tx: sip.NewTransaction("device-outbound-tags-wrong", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 481") {
			t.Fatalf("mismatched device BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("mismatched device BYE response timeout")
	}
	if _, ok := api.streams.Load(streamKey); !ok {
		t.Fatal("mismatched device BYE removed stream")
	}

	wrongSource := makeBYE()
	applyOutboundDialogTags(t, wrongSource, response)
	wrongSource.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.99"), Port: 5060})
	api.sipByeGeneric(&sip.Context{
		Request: wrongSource, Tx: sip.NewTransaction("device-outbound-source-wrong", connection),
		DeviceID: gb10DeviceID, Source: wrongSource.Source(), Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 481") {
			t.Fatalf("mismatched device BYE source response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("mismatched device BYE source response timeout")
	}
	if _, ok := api.streams.Load(streamKey); !ok {
		t.Fatal("mismatched device BYE source removed stream")
	}

	correct := makeBYE()
	applyOutboundDialogTags(t, correct, response)
	api.sipByeGeneric(&sip.Context{
		Request: correct, Tx: sip.NewTransaction("device-outbound-tags-correct", connection),
		DeviceID: gb10DeviceID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("owner device BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("owner device BYE response timeout")
	}
}

func TestCascadeBroadcastUpstreamInviteSupportsDigest(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.password = " "
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
		answer := []byte("v=0\r\no=" + sourceID + " 0 0 IN IP4 192.0.2.30\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 40000 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\ny=" + directTCPSDPLineValue(request.Body(), "y") + "\r\n")
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

func TestCascadeBroadcastUpstreamInviteRetriesProxyDigestThreeVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			platform.password = "voice-secret"
			worker := newCascadeWorker(nil, platform)
			worker.mu.Lock()
			worker.effective = version
			worker.mu.Unlock()
			const sourceID = "34020000001360000021"
			requests := make([]*sip.Request, 0, 2)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests = append(requests, request)
				if len(requests) == 1 {
					response := sip.NewResponseFromRequest("", request, http.StatusProxyAuthRequired, "Proxy Authentication Required", nil)
					response.AppendHeader(&sip.GenericHeader{
						HeaderName: "Proxy-Authenticate",
						Contents:   `Digest realm="3402000000",qop="auth",nonce="voice-proxy-nonce"`,
					})
					return response, nil
				}
				payload, mapping, allowed := cascadeBroadcastProfile(version)
				if !allowed {
					t.Fatalf("version %s unexpectedly has no Broadcast profile", version)
				}
				answer := []byte(fmt.Sprintf(
					"v=0\r\no=%s 0 0 IN IP4 192.0.2.30\r\ns=Play\r\nc=IN IP4 192.0.2.30\r\nt=0 0\r\nm=audio 40000 RTP/AVP %d\r\na=sendonly\r\na=rtpmap:%d %s\r\ny=%s\r\n",
					sourceID, payload, payload, mapping, directTCPSDPLineValue(request.Body(), "y"),
				))
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
				CmdType: "Broadcast", SN: 81, SourceID: sourceID, TargetID: testExposedChannelID,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = api.stopCascadeVoiceSource(source, false) }()
			if len(requests) != 2 {
				t.Fatalf("Proxy Digest INVITE requests = %d, want 2", len(requests))
			}
			if headers := requests[1].GetHeaders("Proxy-Authorization"); len(headers) != 1 {
				t.Fatalf("Proxy-Authorization headers = %v", headers)
			} else {
				auth := sip.AuthFromValue(headers[0].String())
				if auth.Get("username") != gb10DeviceID || auth.Get("uri") != "sip:"+sourceID+"@remote.example" || auth.Get("response") == "" {
					t.Fatalf("Proxy-Authorization = %s", headers[0].String())
				}
			}
			if len(requests[1].GetHeaders("Authorization")) != 0 {
				t.Fatal("proxy-authenticated INVITE also contains Authorization")
			}
			firstCallID, _ := requests[0].CallID()
			secondCallID, _ := requests[1].CallID()
			firstCSeq, _ := requests[0].CSeq()
			secondCSeq, _ := requests[1].CSeq()
			if normalizeCallID(firstCallID) == "" || normalizeCallID(firstCallID) != normalizeCallID(secondCallID) ||
				firstCSeq == nil || secondCSeq == nil || secondCSeq.SeqNo != firstCSeq.SeqNo+1 {
				t.Fatalf("Proxy Digest INVITE dialog changed: Call-ID %v/%v CSeq %v/%v", firstCallID, secondCallID, firstCSeq, secondCSeq)
			}
			if string(requests[0].Body()) != string(requests[1].Body()) ||
				firstSingleHeaderValue(requests[0], "Subject") != firstSingleHeaderValue(requests[1], "Subject") {
				t.Fatal("Proxy Digest INVITE changed SDP or Subject")
			}
		})
	}
}

func TestCascadeBroadcastUpstreamInviteDigestRetriesOnlyOnceThreeVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		for _, challenge := range []struct {
			name   string
			status int
			header string
		}{
			{name: "www", status: http.StatusUnauthorized, header: "WWW-Authenticate"},
			{name: "proxy", status: http.StatusProxyAuthRequired, header: "Proxy-Authenticate"},
		} {
			t.Run(string(version)+"/"+challenge.name, func(t *testing.T) {
				platform := testSharedCascadePlatform(t)
				platform.password = "voice-secret"
				worker := newCascadeWorker(nil, platform)
				worker.mu.Lock()
				worker.effective = version
				worker.mu.Unlock()
				calls := 0
				worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
					calls++
					response := sip.NewResponseFromRequest("", request, challenge.status, http.StatusText(challenge.status), nil)
					response.AppendHeader(&sip.GenericHeader{
						HeaderName: challenge.header,
						Contents:   fmt.Sprintf(`Digest realm="3402000000",qop="auth",nonce="voice-nonce-%d"`, calls),
					})
					return response, nil
				}
				media := &fakeRTPMediaService{openPort: 30000}
				api := &GB28181API{cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media}
				if _, err := api.startCascadeVoiceSource(t.Context(), worker, &sms.MediaServer{SDPIP: "192.0.2.20"}, cascadeQueryEnvelope{
					CmdType: "Broadcast", SN: 82, SourceID: "34020000001360000021", TargetID: testExposedChannelID,
				}); err == nil {
					t.Fatal("repeated Digest challenge unexpectedly succeeded")
				}
				if calls != 2 {
					t.Fatalf("repeated Digest challenge INVITE count = %d, want 2", calls)
				}
				media.mu.Lock()
				closeCalls := media.closeCalls
				media.mu.Unlock()
				if closeCalls != 1 {
					t.Fatalf("repeated Digest challenge CloseRTPServer calls = %d, want 1", closeCalls)
				}
			})
		}
	}
}

func TestCascadeBroadcastRejectsInvalidSDPResponseAfterACKAndBYE(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		contentType []sip.Header
		body        func(*sip.Request) []byte
		want        string
	}{
		{
			name:   "missing Content-Type",
			status: http.StatusAccepted,
			body:   validCascadeVoiceAnswer,
			want:   "Content-Type",
		},
		{
			name:        "duplicate Content-Type",
			contentType: []sip.Header{&sip.ContentTypeSDP, &sip.ContentTypeSDP},
			body:        validCascadeVoiceAnswer,
			want:        "Content-Type",
		},
		{
			name:        "empty SDP",
			contentType: []sip.Header{&sip.ContentTypeSDP},
			body:        func(*sip.Request) []byte { return nil },
			want:        "body is empty",
		},
		{
			name:        "mismatched SSRC",
			contentType: []sip.Header{&sip.ContentTypeSDP},
			body: func(request *sip.Request) []byte {
				body := validCascadeVoiceAnswer(request)
				offered := directTCPSDPLineValue(request.Body(), "y")
				mismatched := "0100000000"
				if offered == mismatched {
					mismatched = "0100000001"
				}
				return []byte(strings.Replace(string(body), "y="+offered+"\r\n", "y="+mismatched+"\r\n", 1))
			},
			want: "does not match offer",
		},
		{
			name:        "duplicate SSRC",
			contentType: []sip.Header{&sip.ContentTypeSDP},
			body: func(request *sip.Request) []byte {
				return append(validCascadeVoiceAnswer(request), "y=0100000000\r\n"...)
			},
			want: "multiple y fields",
		},
		{
			name:        "duplicate audio media",
			contentType: []sip.Header{&sip.ContentTypeSDP},
			body: func(request *sip.Request) []byte {
				return append(validCascadeVoiceAnswer(request), "m=audio 40001 RTP/AVP 96\r\na=sendonly\r\na=rtpmap:96 PS/90000\r\n"...)
			},
			want: "exactly one audio media",
		},
		{
			name:        "session recvonly direction",
			contentType: []sip.Header{&sip.ContentTypeSDP},
			body: func(request *sip.Request) []byte {
				body := strings.Replace(string(validCascadeVoiceAnswer(request)), "t=0 0\r\n", "t=0 0\r\na=recvonly\r\n", 1)
				return []byte(strings.Replace(body, "a=sendonly\r\n", "", 1))
			},
			want: "must send media",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := test.status
			if status == 0 {
				status = http.StatusOK
			}
			platform := testSharedCascadePlatform(t)
			worker := newCascadeWorker(nil, platform)
			worker.mu.Lock()
			worker.effective = GBVersion11
			worker.mu.Unlock()
			var ack, bye *sip.Request
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				switch request.Method() {
				case sip.MethodInvite:
					response := sip.NewResponseFromRequest("", request, status, http.StatusText(status), test.body(request))
					for _, header := range test.contentType {
						response.AppendHeader(header)
					}
					return response, nil
				case sip.MethodBYE:
					bye = request
					return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
				default:
					t.Fatalf("unexpected request method: %s", request.Method())
					return nil, nil
				}
			}
			worker.send = func(request *sip.Request) error {
				ack = request
				return nil
			}
			media := &fakeRTPMediaService{openPort: 30000}
			api := &GB28181API{cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media}
			_, err := api.startCascadeVoiceSource(t.Context(), worker, &sms.MediaServer{SDPIP: "192.0.2.20"}, cascadeQueryEnvelope{
				CmdType: "Broadcast", SN: 81, SourceID: "34020000001360000021", TargetID: testExposedChannelID,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if ack == nil || ack.Method() != sip.MethodACK {
				t.Fatalf("ACK = %#v", ack)
			}
			if bye == nil || bye.Method() != sip.MethodBYE {
				t.Fatalf("BYE = %#v", bye)
			}
			media.mu.Lock()
			closeCalls := media.closeCalls
			media.mu.Unlock()
			if closeCalls != 1 {
				t.Fatalf("CloseRTPServer calls = %d", closeCalls)
			}
		})
	}
}

func TestCascadeWorkerSends2xxACKThroughInviteTransaction(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.signalDigestSeed = "cascade-voice-ack-seed"
	remote := &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	platform.remote = remote
	platform.transport = "tcp"
	localAddress := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(localAddress)
	defer sipServer.Close()
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Required: true, Seed: "global-cascade-voice-ack-seed", Algorithm: "MD5",
		Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}}
	server := &Server{Server: sipServer, gb: api}
	api.svr = server
	worker := newCascadeWorker(server, platform)

	originalConnection := newFlowConnection()
	originalConnection.local = &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000}
	originalConnection.remote = remote
	workerConnection := newFlowConnection()
	workerConnection.local = &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 42000}
	workerConnection.remote = remote
	worker.tcpConn = workerConnection
	worker.tcpRemote = "tcp|" + remote.String()

	callID := sip.CallID("cascade-invite-ack-transaction")
	invite := worker.newRequest(sip.MethodInvite, &sip.ContentTypeSDP, []byte("offer"), &callID, 1, -1, nil)
	invite.SetConnection(originalConnection)
	invite.SetSource(originalConnection.local)
	invite.SetDestination(remote)
	tx := sip.NewTransaction("cascade-invite-ack-transaction", originalConnection)
	defer tx.Close()
	if err := tx.Request(invite); err != nil {
		t.Fatal(err)
	}
	<-originalConnection.writes

	response := sip.NewResponseFromRequest("", invite, http.StatusAccepted, "Accepted", []byte("answer"))
	response.SetConnection(originalConnection)
	response.SetSource(remote)
	worker.rememberInviteTransaction(response, tx)
	ack, err := sip.NewRequestFromResponseChecked(sip.MethodACK, response)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.sendRequest(ack); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-originalConnection.writes:
		if !strings.HasPrefix(string(payload), "ACK ") {
			t.Fatalf("original transaction payload = %s", payload)
		}
		if err := verifyCascadeTestSignalDigest(string(payload), platform.signalDigestSeed); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK was not sent through original INVITE transaction")
	}
	select {
	case payload := <-workerConnection.writes:
		t.Fatalf("ACK bypassed original INVITE transaction: %s", payload)
	default:
	}
}

func TestCascadeWorkerACKSigningFailurePreservesInviteTransaction(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.signalDigestSeed = "cascade-voice-ack-retry-seed"
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Required: true, Algorithm: "unsupported", Encoding: "base64",
	}}}
	worker := newCascadeWorker(&Server{gb: api}, platform)
	connection := newFlowConnection()
	invite := newFlowRequest(t, connection, sip.MethodInvite, "cascade-voice-ack-sign-retry", nil)
	tx := sip.NewTransaction("cascade-voice-ack-sign-retry", connection)
	defer tx.Close()
	if err := tx.Request(invite); err != nil {
		t.Fatal(err)
	}
	<-connection.writes
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
	worker.rememberInviteTransaction(response, tx)
	ack, err := sip.NewRequestFromResponseChecked(sip.MethodACK, response)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.sendRequest(ack); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("invalid ACK signal Digest error = %v", err)
	}
	api.cfg.SignalDigest.Algorithm = "MD5"
	if err := worker.sendRequest(ack); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-connection.writes:
		if !strings.HasPrefix(string(payload), "ACK ") {
			t.Fatalf("retried ACK payload = %s", payload)
		}
		if err := verifyCascadeTestSignalDigest(string(payload), platform.signalDigestSeed); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("ACK transaction was lost after signing failure")
	}
}

func TestRemoveCascadeVoiceSessionsOnlyStopsMatchingWorker(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	api := newGB28181API(&conf.Bootstrap{}, adapter, nil)
	t.Cleanup(api.beginClose)
	media := &fakeRTPMediaService{}
	mediaServer := &sms.MediaServer{}
	api.sms = media
	workerA := &cascadeWorker{}
	workerB := &cascadeWorker{}

	broadcastSource := &cascadeVoiceSourceSession{worker: workerA, server: mediaServer, callID: "worker-a-broadcast", opened: true}
	broadcast := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID,
		Cascade: broadcastSource, ready: make(chan error, 1),
	}
	broadcastSource.broadcast = broadcast
	rawSource := &cascadeVoiceSourceSession{worker: workerA, server: mediaServer, callID: "worker-a-raw", opened: true}
	retainedSource := &cascadeVoiceSourceSession{worker: workerB, callID: "worker-b", opened: true}
	api.cascadeVoiceDialogs.Store(broadcastSource.callID, broadcastSource)
	api.cascadeVoiceDialogs.Store(rawSource.callID, rawSource)
	api.cascadeVoiceDialogs.Store(retainedSource.callID, retainedSource)
	api.broadcastSessions.Store(broadcast.ChannelID, broadcast)

	api.removeCascadeVoiceSessions(workerA)

	for _, callID := range []string{broadcastSource.callID, rawSource.callID} {
		if _, ok := api.cascadeVoiceDialogs.Load(callID); ok {
			t.Fatalf("removed worker retained cascade voice dialog %q", callID)
		}
	}
	if value, ok := api.cascadeVoiceDialogs.Load(retainedSource.callID); !ok || value != retainedSource {
		t.Fatalf("unrelated worker voice dialog = %#v, %v", value, ok)
	}
	broadcast.mu.Lock()
	broadcastStopped := broadcast.stopped
	broadcast.mu.Unlock()
	rawSource.mu.Lock()
	rawOpened := rawSource.opened
	rawSource.mu.Unlock()
	retainedSource.mu.Lock()
	retainedOpened := retainedSource.opened
	retainedSource.mu.Unlock()
	if !broadcastStopped || rawOpened || !retainedOpened {
		t.Fatalf("voice session states = broadcast stopped %v, raw opened %v, retained opened %v",
			broadcastStopped, rawOpened, retainedOpened)
	}
	if _, ok := api.broadcastSessions.Load(broadcast.ChannelID); ok {
		t.Fatal("removed worker retained downstream broadcast session")
	}
}

func TestCloseCascadeVoiceSessionsStopsAllSourcesOnce(t *testing.T) {
	media := &fakeRTPMediaService{}
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	byeCalls := 0
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if request.Method() != sip.MethodBYE {
			return nil, fmt.Errorf("unexpected cascade voice cleanup method %s", request.Method())
		}
		byeCalls++
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	api := &GB28181API{sms: media, streams: &conc.Map[string, *Streams]{}}
	makeSource := func(callID string, upstreamEnded bool) *cascadeVoiceSourceSession {
		connection := newFlowConnection()
		invite := newFlowRequest(t, connection, sip.MethodInvite, callID, nil)
		response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
		return &cascadeVoiceSourceSession{
			worker: worker, server: &sms.MediaServer{}, streamID: "stream-" + callID,
			callID: callID, response: response, opened: true, upstreamEnd: upstreamEnded,
			done: make(chan struct{}),
		}
	}

	rawA := makeSource("cascade-close-raw-a", false)
	rawB := makeSource("cascade-close-raw-b", false)
	upstreamEnded := makeSource("cascade-close-ended", true)
	broadcastSource := makeSource("cascade-close-broadcast", false)
	broadcast := &broadcastSession{
		DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID,
		Cascade: broadcastSource, Stream: &Streams{}, ready: make(chan error, 1),
	}
	broadcastSource.broadcast = broadcast
	for _, source := range []*cascadeVoiceSourceSession{rawA, rawB, upstreamEnded, broadcastSource} {
		api.cascadeVoiceDialogs.Store(source.callID, source)
	}
	api.broadcastSessions.Store(broadcast.ChannelID, broadcast)

	api.closeCascadeVoiceSessions()
	api.closeCascadeVoiceSessions()

	if byeCalls != 3 {
		t.Fatalf("cascade voice shutdown BYE calls = %d, want 3", byeCalls)
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 4 {
		t.Fatalf("cascade voice shutdown CloseRTPServer calls = %d, want 4", closeCalls)
	}
	for _, source := range []*cascadeVoiceSourceSession{rawA, rawB, upstreamEnded, broadcastSource} {
		if _, ok := api.cascadeVoiceDialogs.Load(source.callID); ok {
			t.Fatalf("cascade voice shutdown retained dialog %q", source.callID)
		}
		source.mu.Lock()
		opened, stopping := source.opened, source.stopping
		source.mu.Unlock()
		if opened || !stopping {
			t.Fatalf("cascade voice shutdown source %q = opened:%v stopping:%v", source.callID, opened, stopping)
		}
		select {
		case <-source.done:
		default:
			t.Fatalf("cascade voice shutdown source %q did not signal completion", source.callID)
		}
	}
	if _, ok := api.broadcastSessions.Load(broadcast.ChannelID); ok {
		t.Fatal("cascade voice shutdown retained downstream broadcast session")
	}
	broadcast.mu.Lock()
	broadcastStopped := broadcast.stopped
	broadcast.mu.Unlock()
	if !broadcastStopped {
		t.Fatal("cascade voice shutdown did not stop downstream broadcast session")
	}
}

func TestStartCascadeVoiceSourceStopsWithWorker(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	exchangeStarted := make(chan struct{})
	worker.exchange = func(ctx context.Context, _ *sip.Request) (*sip.Response, error) {
		close(exchangeStarted)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	media := &fakeRTPMediaService{openPort: 30000}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media}
	done := make(chan error, 1)
	go func() {
		_, err := api.startCascadeVoiceSource(context.Background(), worker, &sms.MediaServer{SDPIP: "192.0.2.20"}, cascadeQueryEnvelope{
			CmdType: "Broadcast", SN: 82, SourceID: "34020000001360000021", TargetID: testExposedChannelID,
		})
		done <- err
	}()
	select {
	case <-exchangeStarted:
	case <-time.After(time.Second):
		t.Fatal("cascade voice INVITE did not start")
	}
	worker.cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cascade voice source error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade voice source did not stop with worker")
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("CloseRTPServer calls = %d, want 1", closeCalls)
	}
}

func TestStartCascadeVoiceSourceStopsWhenReceiverTerminates(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	const sourceID = "34020000001360000021"
	byeCalls := 0
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		switch request.Method() {
		case sip.MethodInvite:
			response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", validCascadeVoiceAnswer(request))
			response.AppendHeader(&sip.ContentTypeSDP)
			return response, nil
		case sip.MethodBYE:
			byeCalls++
			return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
		default:
			return nil, fmt.Errorf("unexpected cascade voice request: %s", request.Method())
		}
	}
	worker.send = func(*sip.Request) error { return nil }
	media := &fakeRTPMediaService{openPort: 30000, mediaErr: errors.New("media source not ready")}
	api := &GB28181API{cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media}
	done := make(chan error, 1)
	go func() {
		_, err := api.startCascadeVoiceSource(context.Background(), worker, &sms.MediaServer{SDPIP: "192.0.2.20"}, cascadeQueryEnvelope{
			CmdType: "Broadcast", SN: 83, SourceID: sourceID, TargetID: testExposedChannelID,
		})
		done <- err
	}()

	var source *cascadeVoiceSourceSession
	deadline := time.Now().Add(time.Second)
	for source == nil && time.Now().Before(deadline) {
		api.cascadeVoiceDialogs.Range(func(_, value any) bool {
			source, _ = value.(*cascadeVoiceSourceSession)
			return false
		})
		if source == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if source == nil {
		t.Fatal("cascade voice source did not enter media wait state")
	}
	api.terminateCascadeVoiceSource(source.streamID)

	select {
	case err := <-done:
		if !errors.Is(err, errCascadeVoiceSourceStopped) {
			t.Fatalf("terminated cascade voice source error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminated cascade voice source remained blocked in media wait")
	}
	if _, ok := api.cascadeVoiceDialogs.Load(source.callID); ok {
		t.Fatal("terminated cascade voice source retained dialog")
	}
	media.mu.Lock()
	closeCalls := media.closeCalls
	media.mu.Unlock()
	if closeCalls != 1 || byeCalls != 1 {
		t.Fatalf("terminated cascade voice cleanup = close:%d BYE:%d", closeCalls, byeCalls)
	}
}

func TestAttachCascadeVoiceBroadcastIsAtomicWithTermination(t *testing.T) {
	api := &GB28181API{}
	source := &cascadeVoiceSourceSession{done: make(chan struct{})}
	session := &broadcastSession{ChannelID: testCascadeChannelID}
	if !api.attachCascadeVoiceBroadcast(source, session) {
		t.Fatal("active cascade voice source rejected downstream session")
	}
	if got := source.beginTermination(false); got != session {
		t.Fatalf("termination selected session %p, want %p", got, session)
	}
	second := &broadcastSession{ChannelID: testCascadeChannelID + "-second"}
	if api.attachCascadeVoiceBroadcast(source, second) {
		t.Fatal("terminating cascade voice source accepted a new downstream session")
	}
	if _, ok := api.broadcastSessions.Load(second.ChannelID); ok {
		t.Fatal("rejected downstream session leaked into broadcast index")
	}
}

func validCascadeVoiceAnswer(request *sip.Request) []byte {
	return []byte("v=0\r\n" +
		"o=34020000001360000021 0 0 IN IP4 192.0.2.30\r\n" +
		"s=Play\r\n" +
		"c=IN IP4 192.0.2.30\r\n" +
		"t=0 0\r\n" +
		"m=audio 40000 RTP/AVP 96\r\n" +
		"a=sendonly\r\n" +
		"a=rtpmap:96 PS/90000\r\n" +
		"y=" + directTCPSDPLineValue(request.Body(), "y") + "\r\n")
}
