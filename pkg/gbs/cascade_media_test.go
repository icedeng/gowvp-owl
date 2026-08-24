package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/conc"
	"gorm.io/gorm"
)

func cascadeOfferSDP(protocol, address, setup string) []byte {
	attributes := "a=recvonly\r\na=rtpmap:96 PS/90000\r\n"
	if setup != "" {
		attributes += "a=setup:" + setup + "\r\na=connection:new\r\n"
	}
	return []byte("v=0\r\n" +
		"o=" + gb10PlatformID + " 0 0 IN IP4 " + address + "\r\n" +
		"s=Play\r\n" +
		"c=IN IP4 " + address + "\r\n" +
		"t=0 0\r\n" +
		"m=video 30000 " + protocol + " 96\r\n" +
		attributes +
		"y=0100000011\r\n" +
		"f=v/2/5/25/1/0\r\n")
}

func cascadeHistoryOfferSDP(mode, protocol, address, setup string, start, end int64, speed int) []byte {
	attributes := "a=recvonly\r\na=rtpmap:96 PS/90000\r\n"
	if setup != "" {
		attributes += "a=setup:" + setup + "\r\na=connection:new\r\n"
	}
	if speed > 0 {
		attributes += fmt.Sprintf("a=downloadspeed:%d\r\n", speed)
	}
	return []byte("v=0\r\n" +
		"o=" + gb10PlatformID + " 0 0 IN IP4 " + address + "\r\n" +
		"s=" + mode + "\r\n" +
		"u=" + testExposedChannelID + ":0\r\n" +
		"c=IN IP4 " + address + "\r\n" +
		fmt.Sprintf("t=%d %d\r\n", start, end) +
		"m=video 30000 " + protocol + " 96\r\n" +
		attributes +
		"y=1100000011\r\n" +
		"f=v/2/5/25/1/0\r\n")
}

func TestParseCascadeVideoOfferByProtocolVersion(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	offer, err := parseCascadeVideoOffer(cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""), GBVersion10, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !offer.IsUDP || offer.Payload != 96 || offer.Port != 30000 || offer.SSRC != "0100000011" {
		t.Fatalf("UDP cascade offer = %+v", offer)
	}

	if _, err := parseCascadeVideoOffer(cascadeOfferSDP("TCP/RTP/AVP", "192.0.2.30", "passive"), GBVersion11, platform); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("2014 TCP offer error = %v", err)
	}
	tcpOffer, err := parseCascadeVideoOffer(cascadeOfferSDP("TCP/RTP/AVP", "192.0.2.30", "passive"), GBVersion20, platform)
	if err != nil {
		t.Fatal(err)
	}
	if tcpOffer.IsUDP || tcpOffer.Protocol != "TCP/RTP/AVP" {
		t.Fatalf("TCP cascade offer = %+v", tcpOffer)
	}
	if _, err := parseCascadeVideoOffer(cascadeOfferSDP("TCP/RTP/AVP", "192.0.2.30", "active"), GBVersion20, platform); err == nil || !strings.Contains(err.Error(), "setup:passive") {
		t.Fatalf("active receiver offer error = %v", err)
	}
}

func TestParseCascadeVideoOfferEnforcesMediaAddressAllowlist(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	if _, err := parseCascadeVideoOffer(cascadeOfferSDP("RTP/AVP", "198.51.100.20", ""), GBVersion20, platform); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("unexpected media address error = %v", err)
	}
	_, network, err := net.ParseCIDR("198.51.100.0/24")
	if err != nil {
		t.Fatal(err)
	}
	platform.mediaAllowedCIDRs = append(platform.mediaAllowedCIDRs, network)
	if _, err := parseCascadeVideoOffer(cascadeOfferSDP("RTP/AVP", "198.51.100.20", ""), GBVersion20, platform); err != nil {
		t.Fatalf("allowlisted media address rejected: %v", err)
	}
}

func TestParseCascadeHistoryVideoOffer(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	const start, end = int64(1711929600), int64(1711933200)
	playback, err := parseCascadeVideoOffer(cascadeHistoryOfferSDP(historyModePlayback, "RTP/AVP", "192.0.2.30", "", start, end, 0), GBVersion10, platform)
	if err != nil {
		t.Fatal(err)
	}
	if playback.Mode != historyModePlayback || playback.URI != testExposedChannelID+":0" || playback.StartAt.Unix() != start || playback.EndAt.Unix() != end {
		t.Fatalf("Playback offer = %+v", playback)
	}
	download, err := parseCascadeVideoOffer(cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, 4), GBVersion11, platform)
	if err != nil {
		t.Fatal(err)
	}
	if download.Mode != historyModeDownload || download.DownloadSpeed != 4 || download.SSRC != "1100000011" {
		t.Fatalf("Download offer = %+v", download)
	}
	if _, err := parseCascadeVideoOffer(cascadeHistoryOfferSDP(historyModePlayback, "RTP/AVP", "192.0.2.30", "", start, end, 2), GBVersion11, platform); err == nil || !strings.Contains(err.Error(), "only valid") {
		t.Fatalf("Playback downloadspeed error = %v", err)
	}
}

func TestBuildCascadeSDPAnswerPreservesNegotiatedTransport(t *testing.T) {
	offer := &cascadeVideoOffer{Payload: 96, Protocol: "TCP/RTP/AVP", SSRC: "0100000011", IsUDP: false}
	body, err := buildCascadeSDPAnswer(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, offer, 40000)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"m=video 40000 TCP/RTP/AVP 96", "a=sendonly", "a=setup:active", "a=connection:new",
		"a=rtpmap:96 PS/90000", "y=0100000011", "f=v/////a/1/8/1",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("cascade answer missing %q: %s", expected, text)
		}
	}
}

func TestBuildCascadeHistorySDPAnswerPreservesSession(t *testing.T) {
	const start, end = int64(1711929600), int64(1711933200)
	offer := &cascadeVideoOffer{
		Payload: 96, Protocol: "RTP/AVP", SSRC: "1100000011", IsUDP: true,
		Mode: historyModeDownload, URI: testExposedChannelID + ":0",
		StartAt: time.Unix(start, 0), EndAt: time.Unix(end, 0), DownloadSpeed: 4,
		FileSize: 1048576, FileSizeKnown: true,
	}
	body, err := buildCascadeSDPAnswer(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, offer, 40000)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"s=Download", "u=" + testExposedChannelID + ":0", fmt.Sprintf("t=%d %d", start, end),
		"a=downloadspeed:4", "a=filesize:1048576", "m=video 40000 RTP/AVP 96", "a=sendonly", "y=1100000011",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("cascade history answer missing %q: %s", expected, text)
		}
	}
}

func TestCascadeHistorySourcesAreIsolatedAndReferenceCounted(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID, StreamMode: 0}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID, SDPIP: "192.0.2.20"}
	startInputs := make([]*HistoryInput, 0, 2)
	api.cascadeHistory = func(_ context.Context, in *HistoryInput) error {
		startInputs = append(startInputs, in)
		api.streams.Store(in.sessionKey, &Streams{
			DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
			StreamID: in.streamID, sessionKey: in.sessionKey,
			Resp: sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil),
		})
		return nil
	}
	stopped := make([]string, 0, 2)
	api.cascadeStopHistory = func(_ context.Context, in *StopHistoryInput) error {
		stopped = append(stopped, in.sessionKey)
		api.streams.Delete(in.sessionKey)
		return nil
	}
	offer := &cascadeVideoOffer{
		Mode: historyModePlayback, URI: testExposedChannelID + ":0",
		StartAt: time.Unix(1711929600, 0), EndAt: time.Unix(1711933200, 0),
		PreferredPath: testCascadePathC + "-" + testCascadePathE,
	}
	first, err := api.acquireCascadeSource(t.Context(), server, device, channel, offer)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := api.acquireCascadeSource(t.Context(), server, device, channel, offer)
	if err != nil {
		t.Fatal(err)
	}
	if first != shared || first.refs != 2 || len(startInputs) != 1 || startInputs[0].preferredPath != offer.PreferredPath {
		t.Fatalf("identical source reuse = first %p shared %p refs %d starts %d", first, shared, first.refs, len(startInputs))
	}
	otherOffer := *offer
	otherOffer.StartAt = offer.StartAt.Add(time.Hour)
	otherOffer.EndAt = offer.EndAt.Add(time.Hour)
	other, err := api.acquireCascadeSource(t.Context(), server, device, channel, &otherOffer)
	if err != nil {
		t.Fatal(err)
	}
	if other == first || other.key == first.key || other.stream.StreamID == first.stream.StreamID || len(startInputs) != 2 {
		t.Fatalf("different range was not isolated: first=%+v other=%+v starts=%d", first, other, len(startInputs))
	}
	otherPathOffer := *offer
	otherPathOffer.PreferredPath = testCascadePathE
	otherPath, err := api.acquireCascadeSource(t.Context(), server, device, channel, &otherPathOffer)
	if err != nil {
		t.Fatal(err)
	}
	if otherPath == first || otherPath.key == first.key || otherPath.stream.StreamID == first.stream.StreamID || len(startInputs) != 3 {
		t.Fatalf("different preferred path was not isolated: first=%+v other=%+v starts=%d", first, otherPath, len(startInputs))
	}
	if startInputs[2].preferredPath != otherPathOffer.PreferredPath {
		t.Fatalf("playback preferred path = %q, want %q", startInputs[2].preferredPath, otherPathOffer.PreferredPath)
	}
	api.releaseCascadeSource(first, false)
	if len(stopped) != 0 || first.refs != 1 {
		t.Fatalf("shared source released early: refs=%d stopped=%v", first.refs, stopped)
	}
	api.releaseCascadeSource(shared, false)
	api.releaseCascadeSource(other, false)
	api.releaseCascadeSource(otherPath, false)
	if len(stopped) != 3 || stopped[0] == stopped[1] || stopped[0] == stopped[2] || stopped[1] == stopped[2] {
		t.Fatalf("isolated source cleanup = %v", stopped)
	}
}

func TestCascadeSourceAcquireWaitsForPreviousStop(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID}
	offer := &cascadeVideoOffer{
		Mode: historyModePlayback, URI: testExposedChannelID + ":0",
		StartAt: time.Unix(1711929600, 0), EndAt: time.Unix(1711933200, 0),
	}
	var startCalls atomic.Int32
	api.cascadeHistory = func(_ context.Context, in *HistoryInput) error {
		startCalls.Add(1)
		api.streams.Store(in.sessionKey, &Streams{
			DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
			StreamID: in.streamID, sessionKey: in.sessionKey,
			Resp: sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil),
		})
		return nil
	}
	stopStarted := make(chan struct{})
	allowStop := make(chan struct{})
	var stopCalls atomic.Int32
	api.cascadeStopHistory = func(_ context.Context, in *StopHistoryInput) error {
		if stopCalls.Add(1) == 1 {
			close(stopStarted)
			<-allowStop
		}
		api.streams.Delete(in.sessionKey)
		return nil
	}
	first, err := api.acquireCascadeSource(t.Context(), server, device, channel, offer)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		api.releaseCascadeSource(first, false)
		close(released)
	}()
	select {
	case <-stopStarted:
	case <-time.After(time.Second):
		t.Fatal("cascade source stop did not start")
	}
	type acquireResult struct {
		source *cascadeSourceRef
		err    error
	}
	acquired := make(chan acquireResult, 1)
	go func() {
		source, err := api.acquireCascadeSource(t.Context(), server, device, channel, offer)
		acquired <- acquireResult{source: source, err: err}
	}()
	select {
	case result := <-acquired:
		t.Fatalf("acquire reused stopping source: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	close(allowStop)
	<-released
	var replacement *cascadeSourceRef
	select {
	case result := <-acquired:
		if result.err != nil {
			t.Fatal(result.err)
		}
		replacement = result.source
	case <-time.After(time.Second):
		t.Fatal("cascade source acquire did not resume after stop")
	}
	if replacement == first || startCalls.Load() != 2 {
		t.Fatalf("replacement source = %p first = %p starts = %d", replacement, first, startCalls.Load())
	}
	api.releaseCascadeSource(replacement, false)
	if stopCalls.Load() != 2 {
		t.Fatalf("cascade source stop calls = %d", stopCalls.Load())
	}
}

func TestParseCascadeMANSRTSPAndRewriteCSeq(t *testing.T) {
	request, err := parseCascadeMANSRTSP([]byte("PLAY MANSRTSP/1.0\r\nCSeq: 19\r\nScale: -2.0\r\nRange: npt=100-\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if request.cseq != 19 || request.method != "PLAY" {
		t.Fatalf("MANSRTSP request = %+v", request)
	}
	rewritten := string(request.body(7, "MANSRTSP/1.0"))
	for _, expected := range []string{"PLAY MANSRTSP/1.0", "CSeq: 7", "Scale: -2.0", "Range: npt=100-"} {
		if !strings.Contains(rewritten, expected) {
			t.Fatalf("rewritten MANSRTSP missing %q: %s", expected, rewritten)
		}
	}
	for _, invalid := range [][]byte{
		[]byte("PLAY MANSRTSP/1.0\r\nScale: 2\r\n\r\n"),
		[]byte("PAUSE RTSP/1.0\r\nCSeq: 1\r\nScale: 2\r\n\r\n"),
		[]byte("OPTIONS MANSRTSP/1.0\r\nCSeq: 1\r\n\r\n"),
	} {
		if _, err := parseCascadeMANSRTSP(invalid); err == nil {
			t.Fatalf("invalid MANSRTSP accepted: %q", invalid)
		}
	}
}

func TestCascadeHistoryInfoTranslatesDialogControl(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	source := &cascadeSourceRef{
		key: "history:Playback:device:channel:cascade:test", mode: historyModePlayback,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID},
		stream:  &Streams{CseqNo: 7},
	}
	callID := sip.CallID("cascade-history-info")
	api.inviteDialogs.Store(string(callID), &inboundInviteDialog{
		CallID: string(callID), Established: true,
		Cascade: &cascadeMediaSession{worker: worker, source: source}, UpdatedAt: time.Now(),
	})
	var forwarded *ControlHistoryInput
	api.cascadeControlHistory = func(_ context.Context, in *ControlHistoryInput) error {
		forwarded = in
		return nil
	}
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	request := sip.NewRequest("", sip.MethodInfo, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInfo).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
			Build(), []byte("PLAY MANSRTSP/1.0\r\nCSeq: 99\r\nScale: 2.0\r\n\r\n"))
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	api.sipInfoGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-history-info", connection),
		DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		response := string(payload)
		for _, expected := range []string{"SIP/2.0 200 OK", "Content-Type: Application/MANSRTSP", "MANSRTSP/1.0 200 OK", "CSeq: 99"} {
			if !strings.Contains(response, expected) {
				t.Fatalf("cascade INFO response missing %q:\n%s", expected, response)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("cascade INFO response timeout")
	}
	if forwarded == nil || forwarded.sessionKey != source.key || forwarded.Mode != historyModePlayback || !strings.Contains(forwarded.Cmd, "CSeq: 8") {
		t.Fatalf("forwarded cascade INFO = %+v", forwarded)
	}
}

func TestCascadeCancelTerminatesPendingInvite(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	callID := sip.CallID("cascade-pending-cancel")
	cancelled := make(chan struct{})
	_, cancel := context.WithCancel(context.Background())
	session := &cascadeMediaSession{worker: worker, cancel: func() {
		cancel()
		close(cancelled)
	}}
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	invite.SetConnection(connection)
	invite.SetSource(connection.remote)
	invite.SetDestination(connection.local)
	dialog := &inboundInviteDialog{
		CallID: string(callID), Request: invite, Cascade: session,
		InviteTx: sip.NewTransaction("cascade-pending-invite", connection), UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(string(callID), dialog)
	cancelRequest := sip.NewRequest("", sip.MethodCancel, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodCancel).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	cancelRequest.SetConnection(connection)
	cancelRequest.SetSource(connection.remote)
	cancelRequest.SetDestination(connection.local)
	api.sipCancelGeneric(&sip.Context{
		Request: cancelRequest, Tx: sip.NewTransaction("cascade-pending-cancel", connection),
		DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
	})
	statuses := make([]string, 0, 2)
	for range 2 {
		select {
		case payload := <-connection.writes:
			statuses = append(statuses, string(payload))
		case <-time.After(time.Second):
			t.Fatal("cascade CANCEL responses timeout")
		}
	}
	joined := strings.Join(statuses, "\n")
	if !strings.Contains(joined, "SIP/2.0 200 OK") || !strings.Contains(joined, "SIP/2.0 487 Request Terminated") {
		t.Fatalf("cascade CANCEL responses:\n%s", joined)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("cascade pending INVITE context was not cancelled")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); ok {
		t.Fatal("cascade pending dialog survived CANCEL")
	}
}

func TestCascadeDialogControlRejectsDifferentUpstream(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.31"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	ownerPlatform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(server, ownerPlatform)
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	attackerPlatform := ownerPlatform
	attackerPlatform.name = "upstream-two"
	attackerPlatform.serverID = "34020000002000000999"
	attackerPlatform.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.31"), Port: 5060}
	attackerWorker := newCascadeWorker(server, attackerPlatform)
	attackerWorker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	manager := NewCascadeManager(server)
	manager.items[ownerPlatform.name] = worker
	manager.items[attackerPlatform.name] = attackerWorker
	server.cascade = manager

	callID := sip.CallID("cascade-dialog-auth")
	cancelled := false
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID, Established: true,
		Cascade:   &cascadeMediaSession{worker: worker, cancel: func() { cancelled = true }},
		UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(string(callID), dialog)

	attacker := mustFlowAddress(t, "sip:34020000002000000999@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	newRequest := func(method string) *sip.Request {
		request := sip.NewRequest("", method, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(attacker).SetTo(local).SetMethod(method).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.31", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		return request
	}

	cancelRequest := newRequest(sip.MethodCancel)
	api.sipCancelGeneric(&sip.Context{
		Request: cancelRequest, Tx: sip.NewTransaction("cascade-dialog-auth-cancel", connection),
		DeviceID: "34020000002000000999", Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403") {
			t.Fatalf("cross-upstream CANCEL response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-upstream CANCEL response timeout")
	}
	if cancelled {
		t.Fatal("cross-upstream CANCEL terminated cascade session")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); !ok {
		t.Fatal("cross-upstream CANCEL removed cascade dialog")
	}

	byeRequest := newRequest(sip.MethodBYE)
	api.sipByeGeneric(&sip.Context{
		Request: byeRequest, Tx: sip.NewTransaction("cascade-dialog-auth-bye", connection),
		DeviceID: "34020000002000000999", Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 403") {
			t.Fatalf("cross-upstream BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("cross-upstream BYE response timeout")
	}
	if cancelled {
		t.Fatal("cross-upstream BYE terminated cascade session")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); !ok {
		t.Fatal("cross-upstream BYE removed cascade dialog")
	}

	dialog.mu.Lock()
	dialog.Established = false
	dialog.mu.Unlock()
	ackRequest := newRequest(sip.MethodACK)
	api.sipAckGeneric(&sip.Context{
		Request: ackRequest, Tx: sip.NewTransaction("cascade-dialog-auth-ack", connection),
		DeviceID: "34020000002000000999", Source: connection.remote, Log: slog.Default(),
	})
	dialog.mu.Lock()
	established := dialog.Established
	dialog.mu.Unlock()
	if established {
		t.Fatal("cross-upstream ACK established cascade dialog")
	}
}

func TestCascadeEarlyBYERespondsOnce(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })

	callID := sip.CallID("cascade-early-bye")
	api.inviteDialogs.Store(string(callID), &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID,
		Cascade: &cascadeMediaSession{worker: worker}, UpdatedAt: time.Now(),
	})
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	request := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	api.sipByeGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-early-bye", connection),
		DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 481") {
			t.Fatalf("early BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("early BYE response timeout")
	}
	select {
	case payload := <-connection.writes:
		t.Fatalf("early BYE produced duplicate response: %s", payload)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestCloseCleansCascadeMediaSessionsOnce(t *testing.T) {
	api := &GB28181API{
		streams:        &conc.Map[string, *Streams]{},
		cascadeSources: make(map[string]*cascadeSourceRef),
		lifecycleDone:  make(chan struct{}),
	}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	key := "history:Playback:device:channel:cascade:close"
	stream := &Streams{DeviceID: channel.DeviceID, ChannelID: channel.ChannelID, StreamID: "cascade-close", sessionKey: key}
	api.streams.Store(key, stream)
	source := &cascadeSourceRef{key: key, refs: 1, owned: true, channel: channel, stream: stream, mode: historyModePlayback}
	api.cascadeSources[key] = source
	session := &cascadeMediaSession{source: source}
	api.inviteDialogs.Store("cascade-close", &inboundInviteDialog{CallID: "cascade-close", Cascade: session, UpdatedAt: time.Now()})
	stopCalls := 0
	api.cascadeStopHistory = func(_ context.Context, in *StopHistoryInput) error {
		stopCalls++
		api.streams.Delete(in.sessionKey)
		return nil
	}
	api.close()
	api.close()
	if stopCalls != 1 {
		t.Fatalf("cascade close stop calls = %d", stopCalls)
	}
	if _, ok := api.inviteDialogs.Load("cascade-close"); ok {
		t.Fatal("cascade dialog survived close")
	}
	if len(api.cascadeSources) != 0 {
		t.Fatalf("cascade sources survived close: %+v", api.cascadeSources)
	}
	select {
	case <-api.lifecycleDone:
	default:
		t.Fatal("GB28181 lifecycle was not closed")
	}
}

type fakeCascadeMediaResolver struct {
	server *sms.MediaServer
}

func (f fakeCascadeMediaResolver) GetMediaServer(context.Context, string) (*sms.MediaServer, error) {
	if f.server == nil {
		return nil, fmt.Errorf("media server unavailable")
	}
	return f.server, nil
}

type cascadeFlowMemory struct {
	*flowMemory
	channel *Channel
}

func (m *cascadeFlowMemory) GetChannel(deviceID, channelID string) (*Channel, bool) {
	if m.channel == nil || m.flowMemory == nil || m.flowMemory.persistent == nil || m.flowMemory.persistent.DeviceID != deviceID || m.channel.ChannelID != channelID {
		return nil, false
	}
	return m.channel, true
}

func newCascadeMediaCore(t *testing.T) (ipc.Adapter, *ipc.Device, *ipc.Channel) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipcdb.NewDB(db).AutoMigrate(true)
	device := &ipc.Device{
		ID: "GB_cascade_device", DeviceID: gb10DeviceID, Type: ipc.TypeGB28181,
		IsOnline: true, StreamMode: 0,
	}
	channel := &ipc.Channel{
		ID: "GBC_cascade_channel", DID: device.ID, DeviceID: device.DeviceID,
		ChannelID: testCascadeChannelID, Name: "Front Gate", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := db.Create(device).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	return ipc.NewAdapter(store, uniqueid.Core{}), device, channel
}

func TestCascadeRealtimeInviteEstablishesAndReleasesB2BUA(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	runtimeDevice := &Device{
		IsOnline: true, gbVersion: string(GBVersion30), conn: connection, source: connection.remote,
		to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example"),
	}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}, channel: runtimeChannel}
	mediaServer := &sms.MediaServer{ID: sms.DefaultMediaServerID, SDPIP: "192.0.2.20", Type: "zlm"}
	media := &fakeRTPMediaService{
		mediaItems: []zlm.MediaItem{{Stream: persistentChannel.ID}},
		startPort:  40000,
	}
	server := &Server{memoryStorer: memory, mediaService: fakeCascadeMediaResolver{server: mediaServer}}
	api := &GB28181API{
		cfg:  &conf.SIP{ID: gb10DeviceID, Domain: "local.example", Port: 5060},
		core: adapter, sms: media, svr: server, streams: &conc.Map[string, *Streams]{},
		cascadeSources: make(map[string]*cascadeSourceRef),
	}
	server.gb = api
	playCalls := 0
	stopCalls := 0
	createdStreamID := ""
	api.cascadePlay = func(in *PlayInput) error {
		playCalls++
		if in.preferredPath != testCascadePathC+"-"+testCascadePathE {
			t.Fatalf("forwarded X-PreferredPath = %q", in.preferredPath)
		}
		key := resolvePlaySessionKey(in.Channel.DeviceID, in.Channel.ChannelID, in.sessionKey)
		createdStreamID = in.streamID
		downstreamResponse := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil)
		downstreamResponse.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: in.preferredPath})
		api.streams.Store(key, &Streams{
			DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID, StreamID: createdStreamID,
			Resp: downstreamResponse, mediaServer: mediaServer,
		})
		return nil
	}
	api.cascadeStop = func(_ context.Context, in *StopPlayInput) error {
		stopCalls++
		if in.sessionKey == "" {
			t.Fatal("multi-path cascade stop lost isolated session key")
		}
		return nil
	}

	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.platform.localID = testCascadePathB
	worker.mu.Unlock()
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	callID := sip.CallID("cascade-live-dialog")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().
			SetFrom(remote).
			SetTo(local).
			SetMethod(sip.MethodInvite).
			SetCallID(&callID).
			SetContentType(&sip.ContentTypeSDP).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
			Build(), cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	request.AppendHeader(&sip.GenericHeader{
		HeaderName: cascadePreferredPathHeader,
		Contents:   worker.platform.localID + "-" + testCascadePathC + "-" + testCascadePathE,
	})
	api.sipInviteCascade(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-live-invite", connection),
		DeviceID: gb10PlatformID, Source: connection.remote, To: local, Log: slog.Default(),
	}, string(callID), worker)

	select {
	case payload := <-connection.writes:
		response := string(payload)
		for _, expected := range []string{
			"SIP/2.0 200 OK", "m=video 40000 RTP/AVP 96", "a=sendonly", "y=0100000011",
			"X-RoutePath: " + worker.platform.localID + "-" + testCascadePathC + "-" + testCascadePathE,
		} {
			if !strings.Contains(response, expected) {
				t.Fatalf("cascade INVITE response missing %q:\n%s", expected, response)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("cascade INVITE response timeout")
	}
	if playCalls != 1 || media.startCalls != 1 || createdStreamID == "" || media.started.Stream != createdStreamID || media.started.DstURL != "192.0.2.30" || media.started.DstPort != 30000 {
		t.Fatalf("cascade media start = play %d, request %+v", playCalls, media.started)
	}

	dialogValue, ok := api.inviteDialogs.Load(string(callID))
	if !ok {
		t.Fatal("cascade dialog not stored")
	}
	dialog := dialogValue.(*inboundInviteDialog)
	dialog.mu.Lock()
	dialog.Established = true
	dialog.mu.Unlock()
	bye := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	bye.SetConnection(connection)
	bye.SetSource(connection.remote)
	bye.SetDestination(connection.local)
	api.sipByeGeneric(&sip.Context{Request: bye, Tx: sip.NewTransaction("cascade-live-bye", connection), DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default()})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("cascade BYE response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("cascade BYE response timeout")
	}
	if stopCalls != 1 || media.stopped.SSRC != "0100000011" || media.closed.StreamID != createdStreamID {
		t.Fatalf("cascade cleanup = stop %d, stopped %+v, closed %+v", stopCalls, media.stopped, media.closed)
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); ok {
		t.Fatal("cascade dialog survived BYE")
	}
}

func TestCascadeHistoryDialogFourVersionEndToEnd(t *testing.T) {
	const start, end = int64(1711929600), int64(1711933200)
	tests := []struct {
		name           string
		version        GBProtocolVersion
		controlVersion string
		downloadSpeed  int
	}{
		{name: "2011", version: GBVersion10, controlVersion: "MANSRTSP/1.0"},
		{name: "2014", version: GBVersion11, controlVersion: "RTSP/1.0", downloadSpeed: 4},
		{name: "2016", version: GBVersion20, controlVersion: "RTSP/1.0", downloadSpeed: 4},
		{name: "2022", version: GBVersion30, controlVersion: "RTSP/1.0", downloadSpeed: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, persistentDevice, _ := newCascadeMediaCore(t)
			connection := newFlowConnection()
			connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
			runtimeDevice := &Device{
				IsOnline: true, gbVersion: string(test.version), conn: connection, source: connection.remote,
				to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example"),
			}
			runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
			runtimeChannel.init("local.example")
			memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice}, channel: runtimeChannel}
			mediaServer := &sms.MediaServer{ID: sms.DefaultMediaServerID, SDPIP: "192.0.2.20", Type: "zlm"}
			media := &fakeRTPMediaService{
				mediaItems: []zlm.MediaItem{{Stream: "history-source"}},
				startPort:  40000,
			}
			server := &Server{memoryStorer: memory, mediaService: fakeCascadeMediaResolver{server: mediaServer}}
			api := &GB28181API{
				cfg: &conf.SIP{ID: gb10DeviceID, Domain: "local.example", Port: 5060}, core: adapter,
				sms: media, svr: server, streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef),
			}
			server.gb = api
			platform := testSharedCascadePlatform(t)
			platform.version = test.version
			worker := newCascadeWorker(server, platform)
			if test.version == GBVersion30 {
				worker.platform.localID = testCascadePathB
			}
			worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
			manager := NewCascadeManager(server)
			manager.items[platform.name] = worker
			server.cascade = manager

			var started *HistoryInput
			api.cascadeHistory = func(_ context.Context, in *HistoryInput) error {
				started = in
				downstreamResponse := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil)
				if test.version == GBVersion30 {
					wantPath := testCascadePathC + "-" + testCascadePathE
					if in.preferredPath != wantPath {
						t.Fatalf("forwarded history X-PreferredPath = %q, want %q", in.preferredPath, wantPath)
					}
					downstreamResponse.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: wantPath})
				}
				api.streams.Store(in.sessionKey, &Streams{
					DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID, StreamID: "history-source",
					sessionKey: in.sessionKey, FileSize: 1048576, FileSizeKnown: true, CseqNo: 10,
					Resp: downstreamResponse, mediaServer: mediaServer,
				})
				return nil
			}
			stopCalls := 0
			api.cascadeStopHistory = func(_ context.Context, in *StopHistoryInput) error {
				stopCalls++
				api.streams.Delete(in.sessionKey)
				return nil
			}
			var control *ControlHistoryInput
			api.cascadeControlHistory = func(_ context.Context, in *ControlHistoryInput) error {
				control = in
				return nil
			}

			remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
			local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
			callID := sip.CallID("cascade-history-" + test.name)
			invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
					SetContentType(&sip.ContentTypeSDP).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(),
				cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, test.downloadSpeed))
			invite.SetConnection(connection)
			invite.SetSource(connection.remote)
			invite.SetDestination(connection.local)
			if test.version == GBVersion30 {
				invite.AppendHeader(&sip.GenericHeader{
					HeaderName: cascadePreferredPathHeader,
					Contents:   testCascadePathB + "-" + testCascadePathC + "-" + testCascadePathE,
				})
			}
			api.sipInviteGeneric(&sip.Context{
				Request: invite, Tx: sip.NewTransaction("cascade-history-invite-"+test.name, connection),
				DeviceID: gb10PlatformID, Source: connection.remote, To: local, Log: slog.Default(),
			})
			select {
			case payload := <-connection.writes:
				response := string(payload)
				expectedValues := []string{
					"SIP/2.0 200 OK", "s=Download", "u=" + testExposedChannelID + ":0",
					"a=filesize:1048576", "X-GB-Ver: " + string(test.version),
				}
				if test.version == GBVersion30 {
					expectedValues = append(expectedValues, "X-RoutePath: "+testCascadePathB+"-"+testCascadePathC+"-"+testCascadePathE)
				}
				for _, expected := range expectedValues {
					if !strings.Contains(response, expected) {
						t.Fatalf("history INVITE response missing %q:\n%s", expected, response)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("history INVITE response timeout")
			}
			if started == nil || started.Mode != historyModeDownload || started.Transport != historyTransportRTP || started.DownloadSpeed != test.downloadSpeed {
				t.Fatalf("history source input = %+v", started)
			}
			if media.startCalls != 1 || media.started.Stream != "history-source" || media.started.DstURL != "192.0.2.30" || media.started.DstPort != 30000 {
				t.Fatalf("history RTP forwarding = %+v", media.started)
			}

			ack := sip.NewRequest("", sip.MethodACK, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodACK).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			ack.SetConnection(connection)
			ack.SetSource(connection.remote)
			ack.SetDestination(connection.local)
			api.sipAckGeneric(&sip.Context{Request: ack, DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default()})

			infoBody := []byte("PAUSE " + test.controlVersion + "\r\nCSeq: 19\r\n")
			if test.version.AtLeast(GBVersion11) {
				infoBody = append(infoBody, "PauseTime: now\r\n"...)
			}
			infoBody = append(infoBody, "\r\n"...)
			info := sip.NewRequest("", sip.MethodInfo, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInfo).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), infoBody)
			info.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
			info.SetConnection(connection)
			info.SetSource(connection.remote)
			info.SetDestination(connection.local)
			api.sipInfoGeneric(&sip.Context{
				Request: info, Tx: sip.NewTransaction("cascade-history-info-"+test.name, connection),
				DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
			})
			select {
			case payload := <-connection.writes:
				response := string(payload)
				if !strings.Contains(response, "SIP/2.0 200 OK") || !strings.Contains(response, test.controlVersion+" 200 OK") || !strings.Contains(response, "CSeq: 19") {
					t.Fatalf("history INFO response: %s", response)
				}
			case <-time.After(time.Second):
				t.Fatal("history INFO response timeout")
			}
			if control == nil || !strings.Contains(control.Cmd, test.controlVersion) || !strings.Contains(control.Cmd, "CSeq: 11") {
				t.Fatalf("downstream history control = %+v", control)
			}

			bye := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodBYE).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			bye.SetConnection(connection)
			bye.SetSource(connection.remote)
			bye.SetDestination(connection.local)
			api.sipByeGeneric(&sip.Context{
				Request: bye, Tx: sip.NewTransaction("cascade-history-bye-"+test.name, connection),
				DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
			})
			select {
			case payload := <-connection.writes:
				if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
					t.Fatalf("history BYE response: %s", payload)
				}
			case <-time.After(time.Second):
				t.Fatal("history BYE response timeout")
			}
			if stopCalls != 1 || media.stopped.SSRC != "1100000011" || media.closed.StreamID != "history-source" {
				t.Fatalf("history cleanup = stop %d, RTP %+v, source %+v", stopCalls, media.stopped, media.closed)
			}
			if _, ok := api.inviteDialogs.Load(string(callID)); ok {
				t.Fatal("history dialog survived BYE")
			}
		})
	}
}
