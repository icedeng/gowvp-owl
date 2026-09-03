package gbs

import (
	"bufio"
	"context"
	"errors"
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
		"f=v/2/5/25/1/0a///\r\n")
}

type failFirstFlowResponseConnection struct {
	*flowConnection
	failed atomic.Bool
}

func (c *failFirstFlowResponseConnection) WriteTo(payload []byte, destination net.Addr) (int, error) {
	if c.failed.CompareAndSwap(false, true) {
		return 0, errors.New("test first INVITE final response write failed")
	}
	return c.flowConnection.WriteTo(payload, destination)
}

func TestRemoveCascadeMediaSessionsOnlyStopsMatchingWorker(t *testing.T) {
	workerA := &cascadeWorker{}
	workerB := &cascadeWorker{}
	sourceA := &cascadeSourceRef{key: "source-a", refs: 1}
	sourceB := &cascadeSourceRef{key: "source-b", refs: 1}
	api := &GB28181API{cascadeSources: map[string]*cascadeSourceRef{
		sourceA.key: sourceA,
		sourceB.key: sourceB,
	}}
	var cancelledA, cancelledB atomic.Bool
	sessionA := &cascadeMediaSession{worker: workerA, source: sourceA, cancel: func() { cancelledA.Store(true) }}
	sessionB := &cascadeMediaSession{worker: workerB, source: sourceB, cancel: func() { cancelledB.Store(true) }}
	dialogA := &inboundInviteDialog{CallID: "worker-a", Cascade: sessionA}
	dialogB := &inboundInviteDialog{CallID: "worker-b", Cascade: sessionB}
	api.inviteDialogs.Store(dialogA.CallID, dialogA)
	api.inviteDialogs.Store(dialogB.CallID, dialogB)

	api.removeCascadeMediaSessions(workerA)

	if _, ok := api.inviteDialogs.Load(dialogA.CallID); ok {
		t.Fatal("removed worker retained cascade media dialog")
	}
	if value, ok := api.inviteDialogs.Load(dialogB.CallID); !ok || value != dialogB {
		t.Fatalf("unrelated worker dialog = %#v, %v", value, ok)
	}
	if !cancelledA.Load() || cancelledB.Load() {
		t.Fatalf("session cancellation = worker A %v, worker B %v", cancelledA.Load(), cancelledB.Load())
	}
	api.cascadeMediaMu.Lock()
	_, sourceAExists := api.cascadeSources[sourceA.key]
	retainedSourceB := api.cascadeSources[sourceB.key]
	api.cascadeMediaMu.Unlock()
	if sourceAExists || retainedSourceB != sourceB {
		t.Fatalf("cascade sources after removal = A exists %v, B %#v", sourceAExists, retainedSourceB)
	}
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
		"u=" + testExposedChannelID + ":3\r\n" +
		"c=IN IP4 " + address + "\r\n" +
		fmt.Sprintf("t=%d %d\r\n", start, end) +
		"m=video 30000 " + protocol + " 96\r\n" +
		attributes +
		"y=1100000011\r\n" +
		"f=v/2/5/25/1/0a///\r\n")
}

func TestGBInviteSubjectValidation(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		senderID   string
		receiverID string
		prefix     byte
		wantError  string
	}{
		{name: "live", value: testExposedChannelID + ":0100000011," + gb10PlatformID + ":window-live", senderID: testExposedChannelID, receiverID: gb10PlatformID, prefix: '0'},
		{name: "history", value: testExposedChannelID + ":1100000011," + gb10PlatformID + ":window-history", senderID: testExposedChannelID, receiverID: gb10PlatformID, prefix: '1'},
		{name: "broadcast sequence has no video prefix rule", value: gb10PlatformID + ":voice-1," + testCascadeChannelID + ":speaker-1", senderID: gb10PlatformID, receiverID: testCascadeChannelID},
		{name: "malformed", value: testExposedChannelID + ":0100000011", wantError: "must use"},
		{name: "invalid sender id", value: "sender:0100000011," + gb10PlatformID + ":window", wantError: "sender id"},
		{name: "empty sender sequence", value: testExposedChannelID + ":," + gb10PlatformID + ":window", wantError: "sender sequence"},
		{name: "long sender sequence", value: testExposedChannelID + ":123456789012345678901," + gb10PlatformID + ":window", wantError: "1 to 20"},
		{name: "invalid receiver id", value: testExposedChannelID + ":0100000011,receiver:window", wantError: "receiver id"},
		{name: "empty receiver sequence", value: testExposedChannelID + ":0100000011," + gb10PlatformID + ":", wantError: "receiver sequence"},
		{name: "sender mismatch", value: gb10DeviceID + ":0100000011," + gb10PlatformID + ":window", senderID: testExposedChannelID, receiverID: gb10PlatformID, prefix: '0', wantError: "does not match media source"},
		{name: "receiver mismatch", value: testExposedChannelID + ":0100000011," + gb10DeviceID + ":window", senderID: testExposedChannelID, receiverID: gb10PlatformID, prefix: '0', wantError: "does not match media receiver"},
		{name: "live prefix", value: testExposedChannelID + ":1100000011," + gb10PlatformID + ":window", senderID: testExposedChannelID, receiverID: gb10PlatformID, prefix: '0', wantError: "start with 0"},
		{name: "history prefix", value: testExposedChannelID + ":0100000011," + gb10PlatformID + ":window", senderID: testExposedChannelID, receiverID: gb10PlatformID, prefix: '1', wantError: "start with 1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			subject, err := parseGBInviteSubject(test.value)
			if err == nil {
				err = validateGBInviteSubject(subject, test.senderID, test.receiverID, test.prefix)
			}
			if test.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestOptionalGBInviteSubjectAllowsMissingAndRejectsDuplicates(t *testing.T) {
	request := newFlowRequest(t, newFlowConnection(), sip.MethodInvite, "subject-header", []byte("offer"))
	if subject, err := optionalGBInviteSubject(request); err != nil || subject != nil {
		t.Fatalf("missing Subject = %+v, %v", subject, err)
	}
	if _, err := requiredGBInviteSubject(request); err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("required Subject error = %v", err)
	}
	value := testExposedChannelID + ":0100000011," + gb10PlatformID + ":window"
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: value})
	if subject, err := optionalGBInviteSubject(request); err != nil || subject == nil || subject.ReceiverID != gb10PlatformID {
		t.Fatalf("valid Subject = %+v, %v", subject, err)
	}
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: value})
	if _, err := optionalGBInviteSubject(request); err == nil || !strings.Contains(err.Error(), "multiple") {
		t.Fatalf("duplicate Subject error = %v", err)
	}
}

func TestCascadeInviteRequiresSubjectFrom2014(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardYear(), func(t *testing.T) {
			connection := newFlowConnection()
			connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
			platform := testSharedCascadePlatform(t)
			platform.version = version
			worker := newCascadeWorker(nil, platform)
			remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
			local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
			callID := sip.CallID("cascade-subject-" + version.StandardYear())
			request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
					SetContentType(&sip.ContentTypeSDP).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(),
				cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
			request.SetConnection(connection)
			request.SetSource(connection.remote)
			request.SetDestination(connection.local)

			api := &GB28181API{}
			api.sipInviteCascade(&sip.Context{
				Request: request, Tx: sip.NewTransaction(string(callID), connection),
				DeviceID: gb10PlatformID, Source: connection.remote, To: local, Log: slog.Default(),
			}, string(callID), worker)

			if version == GBVersion10 {
				select {
				case payload := <-connection.writes:
					if response := string(payload); strings.Contains(response, "Subject header is required") {
						t.Fatalf("2011 compatibility rejected missing Subject:\n%s", response)
					}
				case <-time.After(time.Second):
					t.Fatal("2011 compatibility response timeout")
				}
				return
			}
			select {
			case payload := <-connection.writes:
				response := string(payload)
				if !strings.Contains(response, "SIP/2.0 400") || !strings.Contains(response, "Subject header is required") {
					t.Fatalf("missing Subject response:\n%s", response)
				}
			case <-time.After(time.Second):
				t.Fatal("missing Subject response timeout")
			}
			if _, ok := api.inviteDialogs.Load(string(callID)); ok {
				t.Fatal("missing Subject created cascade dialog")
			}
		})
	}
}

func TestStoreCascadeInviteDialogReplacesSameSubjectSender(t *testing.T) {
	api := &GB28181API{}
	subject := &gbInviteSubject{
		SenderID: testExposedChannelID, SenderSequence: "0100000011",
		ReceiverID: gb10PlatformID, ReceiverSequence: "window-a",
	}
	old := &inboundInviteDialog{
		CallID: "old-subject", Subject: subject,
		Cascade: &cascadeMediaSession{},
	}
	api.inviteDialogs.Store(old.CallID, old)

	replacement := &inboundInviteDialog{
		CallID: "new-subject",
		Subject: &gbInviteSubject{
			SenderID: testExposedChannelID, SenderSequence: "0100000011",
			ReceiverID: "34020000002000000099", ReceiverSequence: "window-b",
		},
		Cascade: &cascadeMediaSession{},
	}
	actual, loaded, replaced := api.storeCascadeInviteDialog(replacement.CallID, replacement)
	if loaded || actual != replacement || len(replaced) != 1 || replaced[0] != old {
		t.Fatalf("Subject replacement = actual:%p loaded:%v replaced:%v", actual, loaded, replaced)
	}
	if actual, exists := api.inviteDialogs.Load(old.CallID); !exists || actual != old {
		t.Fatal("old dialog was removed before its SIP state reached a terminal response")
	}
	if actual, exists := api.inviteDialogs.Load(replacement.CallID); !exists || actual != replacement {
		t.Fatal("replacement Subject dialog was not stored")
	}
	api.terminateSupersededCascadeDialog(old)
	if _, exists := api.inviteDialogs.Load(old.CallID); exists {
		t.Fatal("old dialog with the same Subject sender identifier survived termination")
	}

	differentSequence := &inboundInviteDialog{
		CallID:  "different-sequence",
		Subject: &gbInviteSubject{SenderID: testExposedChannelID, SenderSequence: "0100000012"},
		Cascade: &cascadeMediaSession{},
	}
	_, loaded, replaced = api.storeCascadeInviteDialog(differentSequence.CallID, differentSequence)
	if loaded || len(replaced) != 0 {
		t.Fatalf("different Subject sequence replaced a dialog: loaded:%v replaced:%v", loaded, replaced)
	}
	if _, exists := api.inviteDialogs.Load(replacement.CallID); !exists {
		t.Fatal("different Subject sequence removed the previous dialog")
	}

	withoutSubject := &inboundInviteDialog{CallID: "without-subject", Cascade: &cascadeMediaSession{}}
	_, loaded, replaced = api.storeCascadeInviteDialog(withoutSubject.CallID, withoutSubject)
	if loaded || len(replaced) != 0 {
		t.Fatalf("missing Subject changed compatibility behavior: loaded:%v replaced:%v", loaded, replaced)
	}
}

func TestCascadeSubjectReplacementTerminatesPendingInvite(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "pending-replacement-remote"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	callID := sip.CallID("cascade-subject-pending-old")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetSeqNo(10).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	var cancelled atomic.Int32
	subject := &gbInviteSubject{SenderID: testExposedChannelID, SenderSequence: "0100000011"}
	old := &inboundInviteDialog{
		CallID: string(callID), Request: request, Subject: subject,
		Cascade:  &cascadeMediaSession{cancel: func() { cancelled.Add(1) }},
		InviteTx: sip.NewTransaction("cascade-subject-pending-old-tx", connection), UpdatedAt: time.Now(),
	}
	api := &GB28181API{}
	api.inviteDialogs.Store(old.CallID, old)
	replacement := &inboundInviteDialog{
		CallID:  "cascade-subject-pending-new",
		Subject: &gbInviteSubject{SenderID: subject.SenderID, SenderSequence: subject.SenderSequence},
		Cascade: &cascadeMediaSession{},
	}
	_, loaded, replaced := api.storeCascadeInviteDialog(replacement.CallID, replacement)
	if loaded || len(replaced) != 1 || replaced[0] != old {
		t.Fatalf("pending Subject replacement = loaded:%v replaced:%v", loaded, replaced)
	}
	api.terminateSupersededCascadeDialog(old)
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 487 Request Terminated") {
			t.Fatalf("pending replacement response: %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("pending replacement 487 response timeout")
	}
	if cancelled.Load() != 1 {
		t.Fatalf("pending replacement cancellation count = %d; want 1", cancelled.Load())
	}
	if _, exists := api.inviteDialogs.Load(old.CallID); exists {
		t.Fatal("pending replaced dialog survived termination")
	}
}

func TestCascadeSubjectReplacementWaitsForACKBeforeBYE(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "2.0")
	serverAddress := mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example")
	sipServer := sip.NewServer(serverAddress)
	api := &GB28181API{}
	server := &Server{Server: sipServer, gb: api, fromAddress: *serverAddress}
	api.svr = server
	worker := newCascadeWorker(server, platform)
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	client, upstream := net.Pipe()
	var dialCalls atomic.Int32
	worker.dialTCP = func(context.Context, string) (net.Conn, error) {
		dialCalls.Add(1)
		return client, nil
	}
	t.Cleanup(func() {
		worker.cancel()
		worker.closeTCPConnection()
		_ = upstream.Close()
		sipServer.Close()
	})

	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "replacement-remote-tag"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	callID := sip.CallID("cascade-subject-ack-old")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(remote).SetMethod(sip.MethodInvite).SetSeqNo(10).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetSource(&net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	request.SetDestination(&net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	responseTo, ok := response.To()
	if !ok || responseTo == nil {
		t.Fatal("replacement response missing To header")
	}
	responseTo.Params.Add("tag", sip.String{Str: "replacement-local-tag"})
	var cancelled atomic.Bool
	subject := &gbInviteSubject{SenderID: testExposedChannelID, SenderSequence: "0100000011"}
	old := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID,
		RemoteTag: "replacement-remote-tag", LocalTag: "replacement-local-tag", TagsBound: true,
		InitialRemoteCSeq: 10, InitialRemoteCSeqSet: true,
		Request: request, Response: response, Subject: subject,
		Cascade: &cascadeMediaSession{worker: worker, cancel: func() { cancelled.Store(true) }}, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(old.CallID, old)
	replacement := &inboundInviteDialog{
		CallID:  "cascade-subject-ack-new",
		Subject: &gbInviteSubject{SenderID: subject.SenderID, SenderSequence: subject.SenderSequence},
		Cascade: &cascadeMediaSession{},
	}
	_, loaded, replaced := api.storeCascadeInviteDialog(replacement.CallID, replacement)
	if loaded || len(replaced) != 1 || replaced[0] != old {
		t.Fatalf("waiting-ACK Subject replacement = loaded:%v replaced:%v", loaded, replaced)
	}
	api.terminateSupersededCascadeDialog(old)
	if !cancelled.Load() {
		t.Fatal("waiting-ACK replacement did not stop local media")
	}
	if actual, exists := api.inviteDialogs.Load(old.CallID); !exists || actual != old {
		t.Fatal("waiting-ACK replaced dialog was removed before ACK")
	}
	if dialCalls.Load() != 0 {
		t.Fatalf("waiting-ACK replacement sent BYE before ACK; dial calls = %d", dialCalls.Load())
	}

	ackTo := local.Clone()
	ackTo.Params.Add("tag", sip.String{Str: old.LocalTag})
	ack := sip.NewRequest("", sip.MethodACK, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(ackTo).SetMethod(sip.MethodACK).SetSeqNo(10).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	ack.SetSource(request.Source())
	ack.SetDestination(request.Destination())
	messageCh := make(chan string, 1)
	readErrCh := make(chan error, 1)
	go func() {
		message, err := readCascadeTestTCPMessage(bufio.NewReader(upstream))
		if err != nil {
			readErrCh <- err
			return
		}
		messageCh <- message
	}()
	api.sipAckGeneric(&sip.Context{Request: ack, DeviceID: gb10PlatformID, Source: request.Source(), Log: slog.Default()})
	select {
	case message := <-messageCh:
		if !strings.HasPrefix(message, "BYE ") {
			t.Fatalf("replacement post-ACK request = %s", message)
		}
	case err := <-readErrCh:
		t.Fatal(err)
	case <-time.After(time.Second):
		t.Fatal("replacement post-ACK BYE timeout")
	}
	if _, exists := api.inviteDialogs.Load(old.CallID); exists {
		t.Fatal("waiting-ACK replaced dialog survived ACK and BYE")
	}
}

func TestCascadeSubjectReplacementDoesNotWaitForBlockedBYE(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "2.0")
	serverAddress := mustFlowAddress(t, "sip:"+gb10DeviceID+"@local.example")
	sipServer := sip.NewServer(serverAddress)
	api := &GB28181API{}
	server := &Server{Server: sipServer, gb: api, fromAddress: *serverAddress}
	api.svr = server
	worker := newCascadeWorker(server, platform)
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	client, upstream := net.Pipe()
	dialed := make(chan struct{}, 1)
	worker.dialTCP = func(context.Context, string) (net.Conn, error) {
		dialed <- struct{}{}
		return client, nil
	}
	t.Cleanup(func() {
		worker.cancel()
		worker.closeTCPConnection()
		_ = upstream.Close()
		sipServer.Close()
	})

	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "established-replacement-remote"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	callID := sip.CallID("cascade-subject-established-old")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(remote).SetMethod(sip.MethodInvite).SetSeqNo(10).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetSource(&net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	request.SetDestination(&net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	responseTo, ok := response.To()
	if !ok || responseTo == nil {
		t.Fatal("established replacement response missing To header")
	}
	responseTo.Params.Add("tag", sip.String{Str: "established-replacement-local"})
	stopped := make(chan struct{})
	subject := &gbInviteSubject{SenderID: testExposedChannelID, SenderSequence: "0100000011"}
	old := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID, Established: true,
		Request: request, Response: response, Subject: subject,
		Cascade: &cascadeMediaSession{worker: worker, cancel: func() { close(stopped) }}, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(old.CallID, old)
	replacement := &inboundInviteDialog{
		CallID:  "cascade-subject-established-new",
		Subject: &gbInviteSubject{SenderID: subject.SenderID, SenderSequence: subject.SenderSequence},
		Cascade: &cascadeMediaSession{},
	}
	_, loaded, replaced := api.storeCascadeInviteDialog(replacement.CallID, replacement)
	if loaded || len(replaced) != 1 || replaced[0] != old {
		t.Fatalf("established Subject replacement = loaded:%v replaced:%v", loaded, replaced)
	}
	done := make(chan struct{})
	go func() {
		api.terminateSupersededCascadeDialog(old)
		close(done)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("established replacement did not stop local media")
	}
	select {
	case <-dialed:
	case <-time.After(time.Second):
		t.Fatal("established replacement did not start upstream BYE")
	}
	if _, exists := api.inviteDialogs.Load(old.CallID); exists {
		t.Fatal("established replaced dialog remained visible during blocked BYE")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("established replacement waited for blocked upstream BYE")
	}
	message, err := readCascadeTestTCPMessage(bufio.NewReader(upstream))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(message, "BYE ") {
		t.Fatalf("established replacement request = %s", message)
	}
	api.lifecycleWG.Wait()
}

func TestCascadeInviteRejectsInvalidSDPContentTypeBeforeStartingMedia(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	callID := sip.CallID("cascade-invalid-content-type")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
			SetContentType(&sip.ContentTypeXML).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(),
		cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: testExposedChannelID + ":0100000011," + gb10PlatformID + ":window"})

	api := &GB28181API{}
	api.sipInviteCascade(&sip.Context{
		Request: request, Tx: sip.NewTransaction(string(callID), connection),
		DeviceID: gb10PlatformID, Source: connection.remote, To: local, Log: slog.Default(),
	}, string(callID), worker)

	select {
	case payload := <-connection.writes:
		response := string(payload)
		if !strings.Contains(response, "SIP/2.0 415") || !strings.Contains(response, "Content-Type must be application/sdp") {
			t.Fatalf("invalid Content-Type response:\n%s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid Content-Type response timeout")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); ok {
		t.Fatal("invalid Content-Type created cascade dialog")
	}
}

func TestCascadeInviteRejectsAppendixOClientLegWithoutSDP(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	callID := sip.CallID("cascade-appendix-o-client-leg")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: testExposedChannelID + ":1100000011," + gb10PlatformID + ":window"})

	api := &GB28181API{}
	api.sipInviteCascade(&sip.Context{
		Request: request, Tx: sip.NewTransaction(string(callID), connection),
		DeviceID: gb10PlatformID, Source: connection.remote, To: local, Log: slog.Default(),
	}, string(callID), worker)

	select {
	case payload := <-connection.writes:
		response := string(payload)
		if !strings.Contains(response, "SIP/2.0 415") || !strings.Contains(response, "Content-Type must be application/sdp") {
			t.Fatalf("Appendix O client-leg INVITE response:\n%s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("Appendix O client-leg INVITE response timeout")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); ok {
		t.Fatal("Appendix O client-leg INVITE created a media-sender cascade dialog")
	}
}

func TestCascadeInviteRejectsMismatchedSubjectBeforeStartingMedia(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	callID := sip.CallID("cascade-invalid-subject")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
			SetContentType(&sip.ContentTypeSDP).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(),
		cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subject", Contents: testExposedChannelID + ":0100000011," + gb10DeviceID + ":window"})

	api := &GB28181API{}
	api.sipInviteCascade(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-invalid-subject", connection),
		DeviceID: gb10PlatformID, Source: connection.remote, To: local, Log: slog.Default(),
	}, string(callID), worker)

	select {
	case payload := <-connection.writes:
		response := string(payload)
		if !strings.Contains(response, "SIP/2.0 400") || !strings.Contains(response, "does not match media receiver") {
			t.Fatalf("invalid Subject response:\n%s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid Subject response timeout")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); ok {
		t.Fatal("invalid Subject created cascade dialog")
	}
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
	tcpOfferBody := string(cascadeOfferSDP("TCP/RTP/AVP", "192.0.2.30", "passive"))
	missingConnection := strings.Replace(tcpOfferBody, "a=connection:new\r\n", "", 1)
	if _, err := parseCascadeVideoOffer([]byte(missingConnection), GBVersion20, platform); err != nil {
		t.Fatalf("optional connection attribute rejected: %v", err)
	}
	sessionSetup := strings.Replace(tcpOfferBody, "a=setup:passive\r\n", "", 1)
	sessionSetup = strings.Replace(sessionSetup, "t=0 0\r\n", "t=0 0\r\na=setup:passive\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(sessionSetup), GBVersion20, platform); err != nil {
		t.Fatalf("session-level setup rejected: %v", err)
	}
	mediaSetupOverride := strings.Replace(tcpOfferBody, "t=0 0\r\n", "t=0 0\r\na=setup:active\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(mediaSetupOverride), GBVersion20, platform); err != nil {
		t.Fatalf("media-level setup override rejected: %v", err)
	}
	duplicateSetup := strings.Replace(tcpOfferBody, "a=setup:passive\r\n", "a=setup:passive\r\na=setup:passive\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(duplicateSetup), GBVersion20, platform); err == nil {
		t.Fatal("duplicate setup accepted")
	}
	duplicateConnection := strings.Replace(tcpOfferBody, "a=connection:new\r\n", "a=connection:new\r\na=connection:new\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(duplicateConnection), GBVersion20, platform); err == nil {
		t.Fatal("duplicate connection accepted")
	}
	reusedConnection := strings.Replace(tcpOfferBody, "a=connection:new\r\n", "a=connection:existing\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(reusedConnection), GBVersion20, platform); err == nil {
		t.Fatal("unsupported existing connection accepted")
	}
	udpWithTCPAttributes := strings.Replace(
		string(cascadeOfferSDP("RTP/AVP", "192.0.2.30", "")),
		"a=recvonly\r\n", "a=recvonly\r\na=setup:passive\r\na=connection:new\r\n", 1,
	)
	if _, err := parseCascadeVideoOffer([]byte(udpWithTCPAttributes), GBVersion20, platform); err == nil {
		t.Fatal("UDP offer with TCP attributes accepted")
	}
	liveBody := string(cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	sessionOnlyDirection := strings.Replace(liveBody, "t=0 0\r\n", "t=0 0\r\na=sendonly\r\n", 1)
	sessionOnlyDirection = strings.Replace(sessionOnlyDirection, "a=recvonly\r\n", "", 1)
	if _, err := parseCascadeVideoOffer([]byte(sessionOnlyDirection), GBVersion20, platform); err == nil || !strings.Contains(err.Error(), "must accept media") {
		t.Fatalf("session sendonly direction error = %v", err)
	}
	mediaOverride := strings.Replace(liveBody, "t=0 0\r\n", "t=0 0\r\na=sendonly\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(mediaOverride), GBVersion20, platform); err != nil {
		t.Fatalf("media direction override rejected: %v", err)
	}
	conflictingDirections := strings.Replace(liveBody, "a=recvonly\r\n", "a=recvonly\r\na=sendonly\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(conflictingDirections), GBVersion20, platform); err == nil || !strings.Contains(err.Error(), "multiple direction") {
		t.Fatalf("conflicting directions error = %v", err)
	}
	duplicateMedia := liveBody + "m=video 30001 RTP/AVP 96\r\na=recvonly\r\na=rtpmap:96 PS/90000\r\n"
	if _, err := parseCascadeVideoOffer([]byte(duplicateMedia), GBVersion20, platform); err == nil || !strings.Contains(err.Error(), "exactly one video") {
		t.Fatalf("duplicate video media error = %v", err)
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
	if playback.Mode != historyModePlayback || playback.URI != testExposedChannelID+":3" || playback.HistorySourceID != testExposedChannelID || playback.RecordType != 3 || playback.StartAt.Unix() != start || playback.EndAt.Unix() != end {
		t.Fatalf("Playback offer = %+v", playback)
	}
	for recordType := 0; recordType <= 3; recordType++ {
		body := strings.Replace(
			string(cascadeHistoryOfferSDP(historyModePlayback, "RTP/AVP", "192.0.2.30", "", start, end, 0)),
			testExposedChannelID+":3", fmt.Sprintf("%s:%d", testExposedChannelID, recordType), 1,
		)
		offer, parseErr := parseCascadeVideoOffer([]byte(body), GBVersion10, platform)
		if parseErr != nil || offer.RecordType != recordType || offer.HistorySourceID != testExposedChannelID {
			t.Fatalf("record type %d offer = %+v, err = %v", recordType, offer, parseErr)
		}
	}
	for _, uri := range []string{
		testExposedChannelID,
		testExposedChannelID + ":",
		testExposedChannelID + ":4",
		testExposedChannelID + ":manual",
		"http://" + testExposedChannelID + "/record.mp4",
	} {
		body := strings.Replace(
			string(cascadeHistoryOfferSDP(historyModePlayback, "RTP/AVP", "192.0.2.30", "", start, end, 0)),
			testExposedChannelID+":3", uri, 1,
		)
		if _, parseErr := parseCascadeVideoOffer([]byte(body), GBVersion10, platform); parseErr == nil {
			t.Fatalf("invalid history URI %q accepted", uri)
		}
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
	historyWithRealtimeSSRC := strings.Replace(
		string(cascadeHistoryOfferSDP(historyModePlayback, "RTP/AVP", "192.0.2.30", "", start, end, 0)),
		"y=1100000011", "y=0100000011", 1,
	)
	if _, err := parseCascadeVideoOffer([]byte(historyWithRealtimeSSRC), GBVersion10, platform); err == nil || !strings.Contains(err.Error(), "history") {
		t.Fatalf("history realtime SSRC error = %v", err)
	}
	for _, test := range []struct {
		name    string
		mode    string
		version GBProtocolVersion
	}{
		{name: "zero playback", mode: historyModePlayback, version: GBVersion11},
		{name: "zero download", mode: historyModeDownload, version: GBVersion11},
		{name: "unsupported 2011", mode: historyModeDownload, version: GBVersion10},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Replace(
				string(cascadeHistoryOfferSDP(test.mode, "RTP/AVP", "192.0.2.30", "", start, end, 0)),
				"a=recvonly\r\n", "a=recvonly\r\na=downloadspeed:0\r\n", 1,
			)
			if _, parseErr := parseCascadeVideoOffer([]byte(body), test.version, platform); parseErr == nil {
				t.Fatalf("%s accepted explicit zero downloadspeed", test.name)
			}
		})
	}
	duplicateDownloadSpeed := strings.Replace(
		string(cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, 4)),
		"a=downloadspeed:4\r\n", "a=downloadspeed:4\r\na=downloadspeed:4\r\n", 1,
	)
	if _, err := parseCascadeVideoOffer([]byte(duplicateDownloadSpeed), GBVersion11, platform); err == nil {
		t.Fatal("duplicate downloadspeed accepted")
	}
	sessionDownloadSpeed := strings.Replace(
		string(cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, 4)),
		"a=downloadspeed:4\r\n", "", 1,
	)
	sessionDownloadSpeed = strings.Replace(sessionDownloadSpeed, "m=video", "a=downloadspeed:2\r\nm=video", 1)
	sessionOffer, err := parseCascadeVideoOffer([]byte(sessionDownloadSpeed), GBVersion11, platform)
	if err != nil || sessionOffer.DownloadSpeed != 2 {
		t.Fatalf("session-level downloadspeed offer = %+v, err = %v", sessionOffer, err)
	}
	mediaOverride := strings.Replace(
		string(cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, 4)),
		"m=video", "a=downloadspeed:2\r\nm=video", 1,
	)
	overriddenOffer, err := parseCascadeVideoOffer([]byte(mediaOverride), GBVersion11, platform)
	if err != nil || overriddenOffer.DownloadSpeed != 4 {
		t.Fatalf("media-level downloadspeed override = %+v, err = %v", overriddenOffer, err)
	}
	duplicateSessionDownloadSpeed := strings.Replace(sessionDownloadSpeed, "a=downloadspeed:2\r\n", "a=downloadspeed:2\r\na=downloadspeed:2\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(duplicateSessionDownloadSpeed), GBVersion11, platform); err == nil || !strings.Contains(err.Error(), "multiple downloadspeed") {
		t.Fatalf("duplicate session downloadspeed error = %v", err)
	}
}

func TestParseCascadeLiveOfferRejectsHistorySSRC(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	body := strings.Replace(
		string(cascadeOfferSDP("RTP/AVP", "192.0.2.30", "")),
		"y=0100000011", "y=1100000011", 1,
	)
	if _, err := parseCascadeVideoOffer([]byte(body), GBVersion10, platform); err == nil || !strings.Contains(err.Error(), "realtime") {
		t.Fatalf("live history SSRC error = %v", err)
	}
}

func TestParseCascadeOfferRejectsConflictingRTPMap(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	base := string(cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		conflicting := strings.Replace(
			base,
			"a=rtpmap:96 PS/90000\r\n",
			"a=rtpmap:96 PS/90000\r\na=rtpmap:96 H264/90000\r\n",
			1,
		)
		if _, err := parseCascadeVideoOffer([]byte(conflicting), version, platform); err == nil {
			t.Fatalf("protocol %s accepted conflicting rtpmap for the same payload", version)
		}
	}

	duplicate := strings.Replace(
		base,
		"a=rtpmap:96 PS/90000\r\n",
		"a=rtpmap:96 PS/90000\r\na=rtpmap:96 PS/90000\r\n",
		1,
	)
	if _, err := parseCascadeVideoOffer([]byte(duplicate), GBVersion10, platform); err != nil {
		t.Fatalf("identical duplicate rtpmap rejected: %v", err)
	}
}

func TestParseCascadeOfferValidatesMediaDescriptionByVersion(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	base := string(cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	for _, test := range []struct {
		name        string
		version     GBProtocolVersion
		description string
	}{
		{name: "incomplete", version: GBVersion10, description: "v/2/5/25/1/0"},
		{name: "2022 H265 on 2011", version: GBVersion10, description: "v/5/5/25/1/0a///"},
		{name: "2022 AAC on 2016", version: GBVersion20, description: "v/2/5/25/1/0a/6/4/3"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := strings.Replace(base, "f=v/2/5/25/1/0a///", "f="+test.description, 1)
			if _, err := parseCascadeVideoOffer([]byte(body), test.version, platform); err == nil {
				t.Fatalf("invalid %s media description accepted", test.description)
			}
		})
	}
	duplicate := strings.Replace(base, "f=v/2/5/25/1/0a///\r\n", "f=v/2/5/25/1/0a///\r\nf=v/2/5/25/1/0a///\r\n", 1)
	if _, err := parseCascadeVideoOffer([]byte(duplicate), GBVersion10, platform); err == nil {
		t.Fatal("duplicate cascade f field accepted")
	}
	valid2022 := strings.Replace(base, "f=v/2/5/25/1/0a///", "f=v/5/5/25/1/0a/6/4/3", 1)
	if _, err := parseCascadeVideoOffer([]byte(valid2022), GBVersion30, platform); err != nil {
		t.Fatalf("valid 2022 media description rejected: %v", err)
	}
	customResolution2022 := strings.Replace(base, "f=v/2/5/25/1/0a///", "f=v/5/1920x1080/25/1/0a/6/4/3", 1)
	if _, err := parseCascadeVideoOffer([]byte(customResolution2022), GBVersion30, platform); err != nil {
		t.Fatalf("valid 2022 custom resolution rejected: %v", err)
	}
	if _, err := parseCascadeVideoOffer([]byte(customResolution2022), GBVersion20, platform); err == nil {
		t.Fatal("2016 accepted 2022 custom resolution")
	}
}

func TestParseCascadeOfferRejectsDuplicateSessionFields(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	const start, end = int64(1711929600), int64(1711933200)
	live := string(cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	history := string(cascadeHistoryOfferSDP(historyModePlayback, "RTP/AVP", "192.0.2.30", "", start, end, 0))
	tests := []struct {
		name string
		body string
	}{
		{name: "session name", body: strings.Replace(live, "s=Play\r\n", "s=Play\r\ns=Play\r\n", 1)},
		{name: "timing", body: strings.Replace(live, "t=0 0\r\n", "t=0 0\r\nt=0 0\r\n", 1)},
		{name: "SSRC", body: strings.Replace(live, "y=0100000011\r\n", "y=0100000011\r\ny=0100000011\r\n", 1)},
		{name: "history URI", body: strings.Replace(history, "u="+testExposedChannelID+":3\r\n", "u="+testExposedChannelID+":3\r\nu="+testExposedChannelID+":3\r\n", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseCascadeVideoOffer([]byte(test.body), GBVersion10, platform); err == nil {
				t.Fatalf("duplicate %s accepted", test.name)
			}
		})
	}
}

func TestBuildCascadeSDPAnswerPreservesNegotiatedTransport(t *testing.T) {
	offer := &cascadeVideoOffer{Version: GBVersion20, Payload: 96, Protocol: "TCP/RTP/AVP", SSRC: "0100000011", IsUDP: false}
	body, err := buildCascadeSDPAnswer(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, offer, 40000, "v/2/6/25/1/4096a/3/3/1")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"m=video 40000 TCP/RTP/AVP 96", "a=sendonly", "a=setup:active", "a=connection:new",
		"a=rtpmap:96 PS/90000", "y=0100000011", "f=v/2/6/25/1/4096a/3/3/1",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("cascade answer missing %q: %s", expected, text)
		}
	}

	offer.Version = GBVersion10
	if _, err := buildCascadeSDPAnswer(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, offer, 40000, "v/5/6/25/1/4096a///"); err == nil || !strings.Contains(err.Error(), "video codec") {
		t.Fatalf("2011 cascade answer accepted H.265 media description: %v", err)
	}
	offer.Version = GBVersion30
	if _, err := buildCascadeSDPAnswer(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, offer, 40000, "v/5/6/25/1/4096a///"); err != nil {
		t.Fatalf("2022 cascade answer rejected H.265 media description: %v", err)
	}
}

func TestBuildCascadeHistorySDPAnswerPreservesSession(t *testing.T) {
	const start, end = int64(1711929600), int64(1711933200)
	offer := &cascadeVideoOffer{
		Version: GBVersion11, Payload: 96, Protocol: "RTP/AVP", SSRC: "1100000011", IsUDP: true,
		Mode: historyModeDownload, URI: testExposedChannelID + ":3",
		StartAt: time.Unix(start, 0), EndAt: time.Unix(end, 0), DownloadSpeed: 4,
		FileSize: 1048576, FileSizeKnown: true,
	}
	body, err := buildCascadeSDPAnswer(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, offer, 40000, "")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, expected := range []string{
		"s=Download", "u=" + testExposedChannelID + ":3", fmt.Sprintf("t=%d %d", start, end),
		"a=downloadspeed:4", "a=filesize:1048576", "m=video 40000 RTP/AVP 96", "a=sendonly", "y=1100000011", "f=v/////a///",
	} {
		if !strings.Contains(text, expected) {
			t.Fatalf("cascade history answer missing %q: %s", expected, text)
		}
	}
	if _, err := buildCascadeSDPAnswer(gb10DeviceID, &sms.MediaServer{SDPIP: "192.0.2.20"}, offer, 40000, "v/2"); err == nil || !strings.Contains(err.Error(), "media description") {
		t.Fatalf("invalid cascade media description error = %v", err)
	}
}

func TestCascadeHistorySourcesAreIsolatedAndReferenceCounted(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID, StreamMode: 0}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID, SDPIP: "192.0.2.20"}
	actualServer := &sms.MediaServer{ID: "edge-zlm-1", SDPIP: "192.0.2.21"}
	startInputs := make([]*HistoryInput, 0, 2)
	api.cascadeHistory = func(_ context.Context, in *HistoryInput) error {
		startInputs = append(startInputs, in)
		api.streams.Store(in.sessionKey, &Streams{
			DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
			StreamID: in.streamID, sessionKey: in.sessionKey, mediaServer: actualServer,
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
		Mode: historyModePlayback, URI: testExposedChannelID + ":3",
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
	if first.server != actualServer {
		t.Fatalf("cascade source media server = %v, want actual stream server %v", first.server, actualServer)
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

func TestCascadeMediaHooksRespectDisabledDeviceCapabilitiesBeforeSourceRegistration(t *testing.T) {
	tests := []struct {
		name       string
		version    GBProtocolVersion
		disabled   string
		streamMode int8
		offer      cascadeVideoOffer
	}{
		{
			name: "RTP over TCP", version: GBVersion20, disabled: "rtp_over_tcp", streamMode: 1,
			offer: cascadeVideoOffer{Mode: historyModePlay},
		},
		{
			name: "download speed", version: GBVersion11, disabled: "download_speed",
			offer: cascadeVideoOffer{
				Mode: historyModeDownload, StartAt: time.Unix(1711929600, 0), EndAt: time.Unix(1711933200, 0), DownloadSpeed: 4,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, persistentDevice, channel := newCascadeMediaCore(t)
			runtime := &Device{IsOnline: true}
			runtime.setGBProfile(test.version, []string{test.disabled})
			memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtime}}
			server := &Server{memoryStorer: memory}
			api := &GB28181API{
				core: adapter, svr: server, streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef),
			}
			server.gb = api
			device := &ipc.Device{DeviceID: persistentDevice.DeviceID, StreamMode: test.streamMode}
			mediaServer := &sms.MediaServer{ID: sms.DefaultMediaServerID}
			calls := 0
			api.cascadePlay = func(context.Context, *PlayInput) error {
				calls++
				return nil
			}
			api.cascadeHistory = func(context.Context, *HistoryInput) error {
				calls++
				return nil
			}

			if _, err := api.acquireCascadeSource(t.Context(), mediaServer, device, channel, &test.offer); err == nil {
				t.Fatalf("disabled %s cascade media source was accepted", test.disabled)
			}
			if calls != 0 {
				t.Fatalf("disabled %s reached custom media hook %d times", test.disabled, calls)
			}
			if len(api.cascadeSources) != 0 {
				t.Fatalf("disabled %s retained %d cascade source entries", test.disabled, len(api.cascadeSources))
			}
		})
	}
}

func TestCascadeRealtimeSourceStartUsesCallerContext(t *testing.T) {
	api := &GB28181API{
		streams:        &conc.Map[string, *Streams]{},
		cascadeSources: make(map[string]*cascadeSourceRef),
	}
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID, StreamMode: 0}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID, SDPIP: "192.0.2.20"}
	started := make(chan struct{})
	api.cascadePlay = func(ctx context.Context, _ *PlayInput) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := api.acquireCascadeSource(ctx, server, device, channel, &cascadeVideoOffer{Mode: historyModePlay})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade live source did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cascade live source error = %v; want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled cascade live source did not stop")
	}

	api.cascadeMediaMu.Lock()
	count := len(api.cascadeSources)
	api.cascadeMediaMu.Unlock()
	if count != 0 {
		t.Fatalf("cancelled cascade live source retained %d entries", count)
	}
}

func TestCascadeSourceStartPreservesFailedDownstreamRouteResponse(t *testing.T) {
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID, StreamMode: 0}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID, SDPIP: "192.0.2.20"}
	downstream := sip.NewResponse("", sip.DefaultSipVersion, http.StatusNotFound, "Not Found", nil, nil)
	downstream.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: testCascadePathC})

	t.Run("live", func(t *testing.T) {
		api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
		api.cascadePlay = func(_ context.Context, input *PlayInput) error {
			input.routeResponse = downstream
			return errors.New("downstream live rejected")
		}
		_, err := api.acquireCascadeSource(t.Context(), server, device, channel, &cascadeVideoOffer{Mode: historyModePlay})
		if err == nil || cascadeDownstreamInviteResponse(err) != downstream {
			t.Fatalf("live downstream response = %p, error = %v", cascadeDownstreamInviteResponse(err), err)
		}
	})

	t.Run("history", func(t *testing.T) {
		api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
		api.cascadeHistory = func(_ context.Context, input *HistoryInput) error {
			input.routeResponse = downstream
			return errors.New("downstream history rejected")
		}
		_, err := api.acquireCascadeSource(t.Context(), server, device, channel, &cascadeVideoOffer{
			Mode: historyModePlayback, StartAt: time.Unix(1, 0), EndAt: time.Unix(2, 0),
		})
		if err == nil || cascadeDownstreamInviteResponse(err) != downstream {
			t.Fatalf("history downstream response = %p, error = %v", cascadeDownstreamInviteResponse(err), err)
		}
	})
}

func TestCascadeSourceAcquireWaitsForPreviousStop(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID}
	offer := &cascadeVideoOffer{
		Mode: historyModePlayback, URI: testExposedChannelID + ":3",
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

func TestCascadeSourceStartsDifferentKeysWithoutGlobalBlocking(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID}
	firstStart := time.Unix(1711929600, 0)
	firstOffer := &cascadeVideoOffer{
		Mode: historyModePlayback, URI: testExposedChannelID + ":3",
		StartAt: firstStart, EndAt: firstStart.Add(time.Hour),
	}
	secondOffer := *firstOffer
	secondOffer.StartAt = firstOffer.StartAt.Add(2 * time.Hour)
	secondOffer.EndAt = firstOffer.EndAt.Add(2 * time.Hour)
	firstStarted := make(chan struct{})
	allowFirst := make(chan struct{})
	api.cascadeHistory = func(ctx context.Context, in *HistoryInput) error {
		if in.StartAt.Equal(firstStart) {
			close(firstStarted)
			select {
			case <-allowFirst:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		api.streams.Store(in.sessionKey, &Streams{
			DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
			StreamID: in.streamID, sessionKey: in.sessionKey,
			Resp: sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil),
		})
		return nil
	}
	api.cascadeStopHistory = func(_ context.Context, in *StopHistoryInput) error {
		api.streams.Delete(in.sessionKey)
		return nil
	}
	type acquireResult struct {
		source *cascadeSourceRef
		err    error
	}
	firstResult := make(chan acquireResult, 1)
	go func() {
		source, err := api.acquireCascadeSource(t.Context(), server, device, channel, firstOffer)
		firstResult <- acquireResult{source: source, err: err}
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first cascade source did not start")
	}
	secondResult := make(chan acquireResult, 1)
	go func() {
		source, err := api.acquireCascadeSource(t.Context(), server, device, channel, &secondOffer)
		secondResult <- acquireResult{source: source, err: err}
	}()
	var second *cascadeSourceRef
	select {
	case result := <-secondResult:
		if result.err != nil {
			t.Fatal(result.err)
		}
		second = result.source
	case <-time.After(time.Second):
		close(allowFirst)
		<-firstResult
		t.Fatal("different cascade source was blocked by a slow start")
	}
	close(allowFirst)
	first := <-firstResult
	if first.err != nil {
		t.Fatal(first.err)
	}
	if first.source == nil || second == nil || first.source == second {
		t.Fatalf("cascade sources = first %p second %p", first.source, second)
	}
	api.releaseCascadeSource(first.source, false)
	api.releaseCascadeSource(second, false)
}

func TestCascadeSourceConcurrentSameKeyStartsOnce(t *testing.T) {
	api := &GB28181API{streams: &conc.Map[string, *Streams]{}, cascadeSources: make(map[string]*cascadeSourceRef)}
	channel := &ipc.Channel{ID: "persistent-stream", DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	device := &ipc.Device{DeviceID: gb10DeviceID}
	server := &sms.MediaServer{ID: sms.DefaultMediaServerID}
	start := time.Unix(1711929600, 0)
	offer := &cascadeVideoOffer{
		Mode: historyModePlayback, URI: testExposedChannelID + ":3",
		StartAt: start, EndAt: start.Add(time.Hour),
	}
	startEntered := make(chan struct{})
	allowStart := make(chan struct{})
	var startCalls atomic.Int32
	api.cascadeHistory = func(ctx context.Context, in *HistoryInput) error {
		if startCalls.Add(1) == 1 {
			close(startEntered)
		}
		select {
		case <-allowStart:
		case <-ctx.Done():
			return ctx.Err()
		}
		api.streams.Store(in.sessionKey, &Streams{
			DeviceID: in.Channel.DeviceID, ChannelID: in.Channel.ChannelID,
			StreamID: in.streamID, sessionKey: in.sessionKey,
			Resp: sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil),
		})
		return nil
	}
	api.cascadeStopHistory = func(_ context.Context, in *StopHistoryInput) error {
		api.streams.Delete(in.sessionKey)
		return nil
	}
	type acquireResult struct {
		source *cascadeSourceRef
		err    error
	}
	results := make(chan acquireResult, 2)
	acquire := func() {
		source, err := api.acquireCascadeSource(t.Context(), server, device, channel, offer)
		results <- acquireResult{source: source, err: err}
	}
	go acquire()
	<-startEntered
	go acquire()
	select {
	case result := <-results:
		close(allowStart)
		t.Fatalf("same-key acquire returned before the shared start completed: %+v", result)
	case <-time.After(50 * time.Millisecond):
	}
	if calls := startCalls.Load(); calls != 1 {
		close(allowStart)
		t.Fatalf("same-key cascade start calls before release = %d", calls)
	}
	close(allowStart)
	first := <-results
	second := <-results
	if first.err != nil || second.err != nil {
		t.Fatalf("same-key acquire errors = %v, %v", first.err, second.err)
	}
	if first.source == nil || first.source != second.source || first.source.refs != 2 || startCalls.Load() != 1 {
		t.Fatalf("same-key sources = first %p second %p refs %d starts %d", first.source, second.source, first.source.refs, startCalls.Load())
	}
	api.releaseCascadeSource(first.source, false)
	api.releaseCascadeSource(second.source, false)
}

func TestReleaseCascadeSourceUsesShutdownCleanupContext(t *testing.T) {
	type cleanupContextKey struct{}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithValue(context.Background(), cleanupContextKey{}, "shutdown"), time.Second)
	defer shutdownCancel()
	api := &GB28181API{
		lifecycleClosed:        true,
		shutdownPersistenceCtx: shutdownCtx,
		cascadeSources:         make(map[string]*cascadeSourceRef),
	}
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	source := &cascadeSourceRef{
		key: "history:Playback:shutdown-cleanup", refs: 1, owned: true,
		channel: channel, stream: &Streams{StreamID: "shutdown-cleanup-stream"}, mode: historyModePlayback,
		stopDone: make(chan struct{}),
	}
	api.cascadeSources[source.key] = source
	stopDone := source.stopDone
	contexts := make(chan context.Context, 1)
	api.cascadeStopHistory = func(ctx context.Context, _ *StopHistoryInput) error {
		contexts <- ctx
		return errors.New("simulated cascade stop failure")
	}

	api.releaseCascadeSource(source, false)
	cleanupCtx := <-contexts
	if marker := cleanupCtx.Value(cleanupContextKey{}); marker != "shutdown" {
		t.Fatalf("cascade cleanup context marker = %v", marker)
	}
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cascade cleanup context was already cancelled: %v", err)
	}
	if _, ok := api.cascadeSources[source.key]; ok {
		t.Fatal("failed cascade stop retained a permanently stopping source")
	}
	select {
	case <-stopDone:
	default:
		t.Fatal("cascade source stop waiters were not released")
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
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	defer lifecycleCancel()
	api := &GB28181API{svr: server, lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	source := &cascadeSourceRef{
		key: "history:Playback:device:channel:cascade:test", mode: historyModePlayback,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID},
		stream:  &Streams{CseqNo: 7},
	}
	callID := sip.CallID("cascade-history-info")
	updatedAt := time.Now().Add(-time.Minute)
	dialog := &inboundInviteDialog{
		CallID: string(callID), Established: true,
		Cascade: &cascadeMediaSession{worker: worker, source: source}, UpdatedAt: updatedAt,
	}
	api.inviteDialogs.Store(string(callID), dialog)
	var forwarded *ControlHistoryInput
	var forwardedCtx context.Context
	api.cascadeControlHistory = func(ctx context.Context, in *ControlHistoryInput) error {
		forwardedCtx = ctx
		forwarded = in
		return nil
	}
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	request := sip.NewRequest("", sip.MethodInfo, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInfo).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
			Build(), []byte("PLAY RTSP/1.0\r\nCSeq: 99\r\nScale: 2.0\r\n\r\n"))
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
		for _, expected := range []string{"SIP/2.0 200 OK", "Content-Type: Application/MANSRTSP", "RTSP/1.0 200 OK", "CSeq: 99"} {
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
	if forwardedCtx == nil {
		t.Fatal("cascade INFO did not receive a request context")
	}
	dialog.mu.Lock()
	actualUpdatedAt := dialog.UpdatedAt
	dialog.mu.Unlock()
	if !actualUpdatedAt.After(updatedAt) {
		t.Fatalf("confirmed INFO did not refresh dialog activity: got %v, initial %v", actualUpdatedAt, updatedAt)
	}
	lifecycleCancel()
	select {
	case <-forwardedCtx.Done():
	default:
		t.Fatal("cascade INFO context did not observe GB service cancellation")
	}
}

func TestCascadeHistoryInfoRespectsDownstreamVersionBeforeCustomHook(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	memory := &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.effective = GBVersion30
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	stream := &Streams{
		CseqNo: 7,
		S:      time.Unix(1711929600, 0),
		E:      time.Unix(1711933200, 0),
	}
	source := &cascadeSourceRef{
		key: "history:Playback:device:channel:cascade:downstream-version", mode: historyModePlayback,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}, stream: stream,
	}
	callID := sip.CallID("cascade-history-info-downstream-version")
	dialog := &inboundInviteDialog{
		CallID: string(callID), Established: true,
		Cascade: &cascadeMediaSession{worker: worker, source: source}, UpdatedAt: time.Now().Add(-time.Minute),
	}
	api.inviteDialogs.Store(string(callID), dialog)
	calls := 0
	api.cascadeControlHistory = func(context.Context, *ControlHistoryInput) error {
		calls++
		return nil
	}
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	request := sip.NewRequest("", sip.MethodInfo, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInfo).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
			Build(), []byte("PLAY RTSP/1.0\r\nCSeq: 99\r\nScale: -2.0\r\nRange: npt=100-\r\n\r\n"))
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)

	api.sipInfoGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-history-info-downstream-version", connection),
		DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
	})

	select {
	case payload := <-connection.writes:
		if response := string(payload); !strings.Contains(response, "SIP/2.0 502") {
			t.Fatalf("incompatible cascade INFO response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("incompatible cascade INFO response timeout")
	}
	if calls != 0 {
		t.Fatalf("2022 reverse playback reached 2014 downstream hook %d times", calls)
	}
	if stream.CseqNo != 7 {
		t.Fatalf("rejected cascade INFO consumed downstream CSeq: %d", stream.CseqNo)
	}
}

func TestCascadeHistoryInfoStopsWithWorkerOperations(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	source := &cascadeSourceRef{
		key: "history:Playback:device:channel:cascade:worker-cancel", mode: historyModePlayback,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID},
		stream:  &Streams{CseqNo: 7},
	}
	callID := sip.CallID("cascade-history-info-worker-cancel")
	dialog := &inboundInviteDialog{
		CallID: string(callID), Established: true,
		Cascade: &cascadeMediaSession{worker: worker, source: source}, UpdatedAt: time.Now().Add(-time.Minute),
	}
	api.inviteDialogs.Store(string(callID), dialog)
	started := make(chan struct{})
	api.cascadeControlHistory = func(ctx context.Context, _ *ControlHistoryInput) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	request := sip.NewRequest("", sip.MethodInfo, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInfo).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
			Build(), []byte("PLAY RTSP/1.0\r\nCSeq: 99\r\nScale: 2.0\r\n\r\n"))
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	done := make(chan struct{})
	go func() {
		defer close(done)
		api.sipInfoGeneric(&sip.Context{
			Request: request, Tx: sip.NewTransaction("cascade-history-info-worker-cancel", connection),
			DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade INFO control did not start")
	}
	worker.stopOperations()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cascade INFO control did not stop with worker operations")
	}
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 502") {
			t.Fatalf("cancelled cascade INFO response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled cascade INFO response timeout")
	}
}

func TestPendingCascadeHistoryInfoDoesNotRefreshDialogExpiry(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })

	callID := sip.CallID("cascade-pending-history-info")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "pending-info-remote"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	local.Params.Add("tag", sip.String{Str: "pending-info-local"})
	updatedAt := time.Now().Add(-pendingInviteDialogTTL - time.Second)
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID,
		RemoteTag: "pending-info-remote", LocalTag: "pending-info-local", TagsBound: true,
		Cascade: &cascadeMediaSession{worker: worker, source: &cascadeSourceRef{
			key: "history:Playback:device:channel:cascade:pending-info", mode: historyModePlayback,
			channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}, stream: &Streams{},
		}},
		UpdatedAt: updatedAt,
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)

	request := sip.NewRequest("", sip.MethodInfo, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(local).SetMethod(sip.MethodInfo).SetSeqNo(11).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(),
		[]byte("PLAY RTSP/1.0\r\nCSeq: 1\r\nScale: 1.0\r\n\r\n"))
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)

	api.sipInfoGeneric(&sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-pending-history-info", connection),
		DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
	})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 481 history dialog is not established") {
			t.Fatalf("pending INFO response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("pending INFO response timeout")
	}
	dialog.mu.Lock()
	actualUpdatedAt := dialog.UpdatedAt
	dialog.mu.Unlock()
	if !actualUpdatedAt.Equal(updatedAt) {
		t.Fatalf("rejected pending INFO refreshed dialog expiry: got %v, want %v", actualUpdatedAt, updatedAt)
	}
}

func TestCascadeHistoryInfoPreservesDownstreamBusinessResponse(t *testing.T) {
	for _, test := range []struct {
		name       string
		downstream func(uint32) string
		expected   []string
		unexpected []string
	}{
		{
			name: "success headers",
			downstream: func(cseq uint32) string {
				return fmt.Sprintf("RTSP/1.0 200 OK\r\nCSeq: %d\r\nRange: npt=100-\r\nRTP-Info: seq=18139;rtptime=3119600838\r\nScale: -2.0\r\n\r\n", cseq)
			},
			expected: []string{
				"SIP/2.0 200 OK", "RTSP/1.0 200 OK", "CSeq: 99", "Range: npt=100-",
				"RTP-Info: seq=18139;rtptime=3119600838", "Scale: -2.0",
			},
		},
		{
			name: "business failure",
			downstream: func(cseq uint32) string {
				return fmt.Sprintf("RTSP/1.0 500 Server Error\r\nCSeq: %d\r\n\r\n", cseq)
			},
			expected:   []string{"SIP/2.0 200 OK", "RTSP/1.0 500 Server Error", "CSeq: 99"},
			unexpected: []string{"SIP/2.0 502", "RTSP/1.0 200 OK"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCascadeDownstreamSIPFixture(t, GBVersion11)
			fixture.api.streams = &conc.Map[string, *Streams]{}
			runtimeChannel, ok := fixture.api.svr.memoryStorer.GetChannel(gb10DeviceID, testCascadeChannelID)
			if !ok {
				t.Fatal("downstream runtime channel is unavailable")
			}

			local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			remote := mustFlowAddress(t, "sip:"+testCascadeChannelID+"@192.0.2.30:5060")
			local.Params.Add("tag", sip.String{Str: "downstream-local-tag"})
			callID := sip.CallID("cascade-history-downstream-" + strings.ReplaceAll(test.name, " ", "-"))
			invite := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetContact(local).SetMethod(sip.MethodInvite).
					SetSeqNo(1).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			invite.SetConnection(runtimeChannel.Conn())
			invite.SetSource(runtimeChannel.Conn().LocalAddr())
			invite.SetDestination(runtimeChannel.Source())
			dialogResponse := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
			dialogResponse.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
			dialogResponse.SetConnection(runtimeChannel.Conn())

			key := "history:Playback:device:channel:cascade:production"
			stream := &Streams{
				DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, CseqNo: 1, Resp: dialogResponse,
			}
			fixture.api.streams.Store(key, stream)
			source := &cascadeSourceRef{
				key: key, mode: historyModePlayback, channel: fixture.channel, stream: stream,
			}

			peerResult := make(chan error, 1)
			go func() {
				request, err := readAnnexGTestSIPFrame(bufio.NewReader(fixture.peer))
				if err != nil {
					peerResult <- err
					return
				}
				command, err := parseCascadeMANSRTSP(cascadeDownstreamSIPBody(request))
				if err != nil {
					peerResult <- err
					return
				}
				if command.version != "RTSP/1.0" || command.cseq == 99 {
					peerResult <- fmt.Errorf("downstream history command was not normalized: %s", command.body(command.cseq, command.version))
					return
				}
				body := test.downstream(command.cseq)
				var response strings.Builder
				response.WriteString("SIP/2.0 200 OK\r\n")
				for _, name := range []string{"Via", "From", "To", "Call-ID", "CSeq"} {
					fmt.Fprintf(&response, "%s: %s\r\n", name, annexGTestSIPHeader(request, name))
				}
				fmt.Fprintf(&response, "Content-Type: Application/MANSRTSP\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				_, err = fixture.peer.Write([]byte(response.String()))
				peerResult <- err
			}()

			upstreamConnection := newFlowConnection()
			upstreamConnection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
			worker := newCascadeWorker(fixture.api.svr, testSharedCascadePlatform(t))
			worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
			upstreamCallID := sip.CallID("cascade-history-upstream-" + strings.ReplaceAll(test.name, " ", "-"))
			fixture.api.inviteDialogs.Store(string(upstreamCallID), &inboundInviteDialog{
				CallID: string(upstreamCallID), Established: true,
				Cascade: &cascadeMediaSession{worker: worker, source: source}, UpdatedAt: time.Now(),
			})
			upstreamRemote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
			upstreamLocal := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
			request := sip.NewRequest("", sip.MethodInfo, upstreamLocal.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(upstreamRemote).SetTo(upstreamLocal).SetMethod(sip.MethodInfo).SetCallID(&upstreamCallID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).
					Build(), []byte("PLAY RTSP/1.0\r\nCSeq: 99\r\nScale: -2.0\r\n\r\n"))
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
			request.SetConnection(upstreamConnection)
			request.SetSource(upstreamConnection.remote)
			request.SetDestination(upstreamConnection.local)
			fixture.api.sipInfoGeneric(&sip.Context{
				Request: request, Tx: sip.NewTransaction(string(upstreamCallID), upstreamConnection),
				DeviceID: gb10PlatformID, Source: upstreamConnection.remote, Log: slog.Default(),
			})

			select {
			case payload := <-upstreamConnection.writes:
				response := string(payload)
				for _, expected := range test.expected {
					if !strings.Contains(response, expected) {
						t.Fatalf("cascade INFO response missing %q:\n%s", expected, response)
					}
				}
				for _, unexpected := range test.unexpected {
					if strings.Contains(response, unexpected) {
						t.Fatalf("cascade INFO response unexpectedly contains %q:\n%s", unexpected, response)
					}
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cascade INFO response timeout")
			}
			if err := <-peerResult; err != nil {
				t.Fatal(err)
			}
		})
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
	remote.Params.Add("tag", sip.String{Str: "pending-remote-tag"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	inviteBranch := sip.GenerateBranch()
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: inviteBranch})}).Build(), nil)
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
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: inviteBranch})}).Build(), nil)
	cancelRequest.SetConnection(connection)
	cancelRequest.SetSource(connection.remote)
	cancelRequest.SetDestination(connection.local)
	failingConnection := &blockingFlowResponseConnection{
		flowConnection: connection,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("CANCEL SIP OK write failed"),
	}
	cancelRequest.SetConnection(failingConnection)
	failedDone := make(chan struct{})
	go func() {
		api.sipCancelGeneric(&sip.Context{
			Request: cancelRequest, Tx: sip.NewTransaction("cascade-pending-cancel-write-failure", failingConnection),
			DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
		})
		close(failedDone)
	}()
	select {
	case <-failingConnection.started:
	case <-time.After(time.Second):
		close(failingConnection.release)
		t.Fatal("cascade CANCEL SIP response write did not start")
	}
	if dialog.mu.TryLock() {
		dialog.mu.Unlock()
		close(failingConnection.release)
		<-failedDone
		t.Fatal("cascade INVITE final response could commit while CANCEL response was pending")
	}
	select {
	case <-cancelled:
		close(failingConnection.release)
		<-failedDone
		t.Fatal("cascade pending INVITE was cancelled before SIP OK completed")
	default:
	}
	close(failingConnection.release)
	select {
	case <-failedDone:
	case <-time.After(time.Second):
		t.Fatal("cascade CANCEL handler did not return after SIP OK write failure")
	}
	if dialog.Cancelled {
		t.Fatal("cascade dialog was marked cancelled after SIP OK write failure")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); !ok {
		t.Fatal("cascade pending dialog was removed after SIP OK write failure")
	}

	retriedCancel := cancelRequest.Clone().(*sip.Request)
	retriedCancel.SetConnection(connection)
	retriedCancel.SetSource(connection.remote)
	retriedCancel.SetDestination(connection.local)
	api.sipCancelGeneric(&sip.Context{
		Request: retriedCancel, Tx: sip.NewTransaction("cascade-pending-cancel", connection),
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

func TestCascadeCancelRetainsPendingInviteUntil487WriteSucceeds(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	inviteConnection := &failFirstFlowResponseConnection{flowConnection: connection}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	callID := sip.CallID("cascade-pending-cancel-487-retry")
	cancelled := make(chan struct{})
	_, cancel := context.WithCancel(context.Background())
	session := &cascadeMediaSession{worker: worker, cancel: func() {
		cancel()
		close(cancelled)
	}}
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "pending-retry-remote-tag"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	inviteBranch := sip.GenerateBranch()
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetSeqNo(7).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: inviteBranch})}).Build(), nil)
	invite.SetConnection(inviteConnection)
	invite.SetSource(connection.remote)
	invite.SetDestination(connection.local)
	inviteTx := sip.NewTransaction("cascade-pending-invite-487-retry", inviteConnection)
	t.Cleanup(inviteTx.Close)
	dialog := &inboundInviteDialog{
		CallID: string(callID), Request: invite, Cascade: session,
		InviteTx: inviteTx, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(string(callID), dialog)

	cancelRequest := sip.NewRequest("", sip.MethodCancel, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodCancel).SetSeqNo(7).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: inviteBranch})}).Build(), nil)
	cancelRequest.SetConnection(connection)
	cancelRequest.SetSource(connection.remote)
	cancelRequest.SetDestination(connection.local)
	cancelTx := sip.NewTransaction("cascade-pending-cancel-487-retry", connection)
	t.Cleanup(cancelTx.Close)
	api.sipCancelGeneric(&sip.Context{
		Request: cancelRequest, Tx: cancelTx,
		DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
	})

	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("CANCEL response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("CANCEL 200 response timeout")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("CANCEL did not stop the pending cascade session")
	}
	if current, ok := api.inviteDialogs.Load(string(callID)); !ok || current != dialog {
		t.Fatal("failed 487 write lost the cancelled INVITE dialog")
	}
	dialog.mu.Lock()
	termination := dialog.TerminationResponse
	dialog.mu.Unlock()
	if termination == nil || termination.StatusCode() != 487 {
		t.Fatalf("retained termination response = %#v", termination)
	}

	retransmission := invite.Clone().(*sip.Request)
	retransmission.SetConnection(inviteConnection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	api.sipInviteCascade(&sip.Context{
		Request: retransmission, Tx: inviteTx,
		DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
	}, string(callID), worker)

	select {
	case payload := <-connection.writes:
		response := string(payload)
		if !strings.Contains(response, "SIP/2.0 487 Request Terminated") || !strings.Contains(strings.ToLower(response), ";tag=") {
			t.Fatalf("retried INVITE termination response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("retried INVITE 487 response timeout")
	}
	if _, ok := api.inviteDialogs.Load(string(callID)); ok {
		t.Fatal("successful 487 retry retained the cancelled INVITE dialog")
	}
}

func TestCascadeCancelRejectsDifferentInviteTransaction(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	callID := sip.CallID("cascade-cancel-transaction")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "cancel-remote-tag"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetSeqNo(7).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-owner"})}).Build(), nil)
	invite.SetConnection(connection)
	invite.SetSource(connection.remote)
	invite.SetDestination(connection.local)
	cancelled := false
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID, RemoteTag: "cancel-remote-tag", TagsBound: true,
		InitialRemoteCSeq: 7, InitialRemoteCSeqSet: true, RemoteCSeq: 7, RemoteCSeqSet: true, RemoteMethod: sip.MethodInvite,
		Request: invite, Cascade: &cascadeMediaSession{worker: worker, cancel: func() { cancelled = true }}, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)
	makeCancel := func(recipient *sip.URI, branch string, cseq uint) *sip.Request {
		request := sip.NewRequest("", sip.MethodCancel, recipient, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodCancel).SetSeqNo(cseq).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: branch})}).Build(), nil)
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		return request
	}
	assertRejected := func(name string, request *sip.Request) {
		t.Helper()
		api.sipCancelGeneric(&sip.Context{Request: request, Tx: sip.NewTransaction(name, connection), DeviceID: gb10PlatformID, Source: connection.remote})
		select {
		case payload := <-connection.writes:
			if !strings.Contains(string(payload), "SIP/2.0 481") {
				t.Fatalf("%s response = %s", name, payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s response timeout", name)
		}
		if cancelled {
			t.Fatalf("%s cancelled owner transaction", name)
		}
		if _, exists := api.inviteDialogs.Load(dialog.CallID); !exists {
			t.Fatalf("%s removed owner dialog", name)
		}
	}
	assertRejected("cancel-wrong-cseq", makeCancel(local.URI, "z9hG4bK-owner", 8))
	assertRejected("cancel-wrong-branch", makeCancel(local.URI, "z9hG4bK-other", 7))
	other := local.URI.Clone()
	other.FUser = sip.String{Str: "34020000001320000912"}
	assertRejected("cancel-wrong-uri", makeCancel(other, "z9hG4bK-owner", 7))
	wrongSentBy := makeCancel(local.URI, "z9hG4bK-owner", 7)
	wrongSentByVia, ok := wrongSentBy.ViaHop()
	if !ok || wrongSentByVia == nil {
		t.Fatal("CANCEL missing Via")
	}
	wrongSentByVia.Host = "192.0.2.31"
	assertRejected("cancel-wrong-via-sent-by", wrongSentBy)
}

func TestCascadeInviteRetransmissionRequiresSameTransaction(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	callID := sip.CallID("cascade-invite-retransmission")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "invite-remote-tag"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetSeqNo(7).SetCallID(&callID).
			AddVia(&sip.ViaHop{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP", Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-owner"})}).Build(), cascadeOfferSDP("RTP/AVP", "192.0.2.30", ""))
	invite.SetConnection(connection)
	invite.SetSource(connection.remote)
	invite.SetDestination(connection.local)
	response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", []byte("owner response"))
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID, RemoteTag: "invite-remote-tag", TagsBound: true,
		InitialRemoteCSeq: 7, InitialRemoteCSeqSet: true, RemoteCSeq: 7, RemoteCSeqSet: true, RemoteMethod: sip.MethodInvite,
		Request: invite, Response: response, Cascade: &cascadeMediaSession{worker: worker}, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)

	assertResponse := func(name string, request *sip.Request, status string, bodyExpected bool) {
		t.Helper()
		api.sipInviteCascade(&sip.Context{Request: request, Tx: sip.NewTransaction(name, connection), DeviceID: gb10PlatformID, Source: connection.remote}, dialog.CallID, worker)
		select {
		case payload := <-connection.writes:
			got := string(payload)
			if !strings.Contains(got, status) || strings.Contains(got, "owner response") != bodyExpected {
				t.Fatalf("%s response = %s", name, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s response timeout", name)
		}
	}
	retransmission := invite.Clone().(*sip.Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	assertResponse("same-invite", retransmission, "SIP/2.0 200 OK", true)

	wrongCSeq := invite.Clone().(*sip.Request)
	cseq, ok := wrongCSeq.CSeq()
	if !ok || cseq == nil {
		t.Fatal("INVITE missing CSeq")
	}
	cseq.SeqNo++
	wrongCSeq.SetConnection(connection)
	wrongCSeq.SetSource(connection.remote)
	wrongCSeq.SetDestination(connection.local)
	assertResponse("different-invite", wrongCSeq, "SIP/2.0 491 Call-ID already in use", false)
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

func TestCascadeDialogControlRejectsMismatchedTags(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	callID := sip.CallID("cascade-dialog-tags")
	cancelled := false
	dialog := &inboundInviteDialog{
		CallID: "cascade-dialog-tags", DeviceID: gb10PlatformID,
		RemoteTag: "owner-remote", InitialToTag: "", LocalTag: "owner-local", TagsBound: true, Established: true,
		Cascade: &cascadeMediaSession{worker: worker, cancel: func() { cancelled = true }}, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	newRequest := func(method, fromTag, toTag string) *sip.Request {
		from := remote.Clone()
		from.Params.Add("tag", sip.String{Str: fromTag})
		to := local.Clone()
		if toTag != "" {
			to.Params.Add("tag", sip.String{Str: toTag})
		}
		request := sip.NewRequest("", method, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(from).SetToWithParam(to).SetMethod(method).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		return request
	}
	assertRejected := func(method string, request *sip.Request, invoke func(*sip.Context)) {
		invoke(&sip.Context{
			Request: request, Tx: sip.NewTransaction("cascade-dialog-tags-"+strings.ToLower(method), connection),
			DeviceID: gb10PlatformID, Source: connection.remote, Log: slog.Default(),
		})
		select {
		case payload := <-connection.writes:
			if !strings.Contains(string(payload), "SIP/2.0 481") {
				t.Fatalf("mismatched-tag %s response: %s", method, payload)
			}
		case <-time.After(time.Second):
			t.Fatalf("mismatched-tag %s response timeout", method)
		}
	}
	if !inboundDialogTagsMatch(dialog, newRequest(sip.MethodCancel, "owner-remote", ""), false) ||
		!inboundDialogTagsMatch(dialog, newRequest(sip.MethodACK, "owner-remote", "owner-local"), true) {
		t.Fatal("owner dialog tags were not accepted")
	}

	assertRejected(sip.MethodCancel, newRequest(sip.MethodCancel, "other-remote", ""), api.sipCancelGeneric)
	assertRejected(sip.MethodBYE, newRequest(sip.MethodBYE, "owner-remote", "other-local"), api.sipByeGeneric)
	info := newRequest(sip.MethodInfo, "owner-remote", "other-local")
	info.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	assertRejected(sip.MethodInfo, info, api.sipInfoGeneric)

	dialog.mu.Lock()
	dialog.Established = false
	dialog.mu.Unlock()
	api.sipAckGeneric(&sip.Context{
		Request: newRequest(sip.MethodACK, "owner-remote", "other-local"), DeviceID: gb10PlatformID,
		Source: connection.remote, Log: slog.Default(),
	})
	dialog.mu.Lock()
	established := dialog.Established
	dialog.mu.Unlock()
	if established || cancelled {
		t.Fatalf("mismatched dialog tags changed state: established=%v cancelled=%v", established, cancelled)
	}
	if _, ok := api.inviteDialogs.Load(dialog.CallID); !ok {
		t.Fatal("mismatched dialog tags removed the owner dialog")
	}
}

func TestInboundInviteDialogRejectsACKBeforeFinalResponse(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardYear(), func(t *testing.T) {
			connection := newFlowConnection()
			connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
			server := &Server{}
			api := &GB28181API{svr: server}
			server.gb = api
			platform := testSharedCascadePlatform(t)
			platform.version = version
			worker := newCascadeWorker(server, platform)
			worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })

			callID := sip.CallID("cascade-early-ack-" + version.StandardYear())
			dialog := &inboundInviteDialog{
				CallID: string(callID), DeviceID: gb10PlatformID,
				RemoteTag: "remote-tag", TagsBound: true,
				InitialRemoteCSeq: 10, InitialRemoteCSeqSet: true,
				Cascade: &cascadeMediaSession{worker: worker}, UpdatedAt: time.Now(),
			}
			api.inviteDialogs.Store(dialog.CallID, dialog)

			remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
			remote.Params.Add("tag", sip.String{Str: dialog.RemoteTag})
			local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
			ack := sip.NewRequest("", sip.MethodACK, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(local).SetMethod(sip.MethodACK).SetSeqNo(10).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			ack.SetConnection(connection)
			ack.SetSource(connection.remote)
			ack.SetDestination(connection.local)

			api.sipAckGeneric(&sip.Context{Request: ack, DeviceID: gb10PlatformID, Source: connection.remote})
			dialog.mu.Lock()
			established := dialog.Established
			dialog.mu.Unlock()
			if established {
				t.Fatal("ACK sent before the final INVITE response established the dialog")
			}
		})
	}
}

func TestInboundInviteDialogEnforcesRemoteCSeq(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	callID := sip.CallID("cascade-dialog-cseq")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "remote-cseq-tag"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetSeqNo(10).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-cseq-invite"})}).Build(), nil)
	invite.SetConnection(connection)
	invite.SetSource(connection.remote)
	invite.SetDestination(connection.local)
	source := &cascadeSourceRef{
		key: "history:Playback:device:channel:cascade:cseq", mode: historyModePlayback,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}, stream: &Streams{},
	}
	stopped := false
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID,
		RemoteTag: "remote-cseq-tag", InitialToTag: "", LocalTag: "local-cseq-tag", TagsBound: true,
		InitialRemoteCSeq: 10, InitialRemoteCSeqSet: true, RemoteCSeq: 10, RemoteCSeqSet: true, RemoteMethod: sip.MethodInvite,
		Request: invite, Cascade: &cascadeMediaSession{worker: worker, source: source, cancel: func() { stopped = true }}, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)
	newRequest := func(method string, cseq uint32, established bool) *sip.Request {
		to := local.Clone()
		if established {
			to.Params.Add("tag", sip.String{Str: dialog.LocalTag})
		}
		request := sip.NewRequest("", method, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(to).SetMethod(method).SetSeqNo(uint(cseq)).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		request.SetDestination(connection.local)
		return request
	}
	response := func(t *testing.T) string {
		t.Helper()
		select {
		case payload := <-connection.writes:
			return string(payload)
		case <-time.After(time.Second):
			t.Fatal("dialog CSeq response timeout")
			return ""
		}
	}

	api.sipAckGeneric(&sip.Context{Request: newRequest(sip.MethodACK, 11, true), DeviceID: gb10PlatformID, Source: connection.remote})
	dialog.mu.Lock()
	if dialog.Established {
		dialog.mu.Unlock()
		t.Fatal("ACK with wrong CSeq established dialog")
	}
	dialog.mu.Unlock()
	api.sipAckGeneric(&sip.Context{Request: newRequest(sip.MethodACK, 10, true), DeviceID: gb10PlatformID, Source: connection.remote})
	dialog.mu.Lock()
	established := dialog.Established
	dialog.mu.Unlock()
	if !established {
		t.Fatal("ACK with INVITE CSeq did not establish dialog")
	}

	controlCalls := 0
	api.cascadeControlHistory = func(context.Context, *ControlHistoryInput) error {
		controlCalls++
		return nil
	}
	info := newRequest(sip.MethodInfo, 11, true)
	info.SetBody([]byte("PLAY RTSP/1.0\r\nCSeq: 1\r\nScale: 1.0\r\n\r\n"), true)
	info.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	infoCtx := &sip.Context{Request: info, Tx: sip.NewTransaction("cascade-dialog-cseq-info", connection), DeviceID: gb10PlatformID, Source: connection.remote}
	api.sipInfoGeneric(infoCtx)
	if got := response(t); !strings.Contains(got, "SIP/2.0 200 OK") {
		t.Fatalf("valid INFO response = %s", got)
	}
	duplicate := info.Clone().(*sip.Request)
	duplicate.SetConnection(connection)
	duplicate.SetSource(connection.remote)
	duplicate.SetDestination(connection.local)
	api.sipInfoGeneric(&sip.Context{Request: duplicate, Tx: sip.NewTransaction("cascade-dialog-cseq-info-duplicate", connection), DeviceID: gb10PlatformID, Source: connection.remote})
	if got := response(t); !strings.Contains(got, "SIP/2.0 200 OK") {
		t.Fatalf("duplicate INFO response = %s", got)
	}
	if controlCalls != 1 {
		t.Fatalf("duplicate INFO downstream calls = %d; want 1", controlCalls)
	}
	altered := info.Clone().(*sip.Request)
	altered.SetBody([]byte("PLAY RTSP/1.0\r\nCSeq: 2\r\nScale: 2.0\r\n\r\n"), true)
	altered.SetConnection(connection)
	altered.SetSource(connection.remote)
	altered.SetDestination(connection.local)
	api.sipInfoGeneric(&sip.Context{Request: altered, Tx: sip.NewTransaction("cascade-dialog-cseq-info-altered", connection), DeviceID: gb10PlatformID, Source: connection.remote})
	if got := response(t); !strings.Contains(got, "SIP/2.0 500 CSeq out of order") {
		t.Fatalf("altered same-CSeq INFO response = %s", got)
	}
	if controlCalls != 1 {
		t.Fatalf("altered same-CSeq INFO downstream calls = %d; want 1", controlCalls)
	}
	unsupported := newRequest(sip.MethodInfo, 12, true)
	unsupported.SetBody([]byte("PLAY RTSP/1.0\r\nCSeq: 3\r\nScale: 1.0\r\n\r\n"), true)
	unsupported.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	unsupported.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	unsupportedCtx := &sip.Context{Request: unsupported, Tx: sip.NewTransaction("cascade-dialog-cseq-info-unsupported", connection), DeviceID: gb10PlatformID, Source: connection.remote}
	api.sipInfoGeneric(unsupportedCtx)
	if got := response(t); !strings.Contains(got, "SIP/2.0 415 Content-Type must be Application/MANSRTSP") {
		t.Fatalf("unsupported INFO response = %s", got)
	}
	unsupportedDuplicate := unsupported.Clone().(*sip.Request)
	unsupportedDuplicate.SetConnection(connection)
	unsupportedDuplicate.SetSource(connection.remote)
	unsupportedDuplicate.SetDestination(connection.local)
	api.sipInfoGeneric(&sip.Context{Request: unsupportedDuplicate, Tx: sip.NewTransaction("cascade-dialog-cseq-info-unsupported-duplicate", connection), DeviceID: gb10PlatformID, Source: connection.remote})
	if got := response(t); !strings.Contains(got, "SIP/2.0 415 Content-Type must be Application/MANSRTSP") {
		t.Fatalf("duplicate unsupported INFO response = %s", got)
	}
	if controlCalls != 1 {
		t.Fatalf("unsupported INFO downstream calls = %d; want 1", controlCalls)
	}

	replayedBYE := newRequest(sip.MethodBYE, 12, true)
	api.sipByeGeneric(&sip.Context{Request: replayedBYE, Tx: sip.NewTransaction("cascade-dialog-cseq-bye-replay", connection), DeviceID: gb10PlatformID, Source: connection.remote})
	if got := response(t); !strings.Contains(got, "SIP/2.0 500 CSeq out of order") || !strings.Contains(got, "Retry-After: 0") {
		t.Fatalf("replayed BYE response = %s", got)
	}
	if stopped {
		t.Fatal("replayed BYE stopped cascade media")
	}
	if _, exists := api.inviteDialogs.Load(dialog.CallID); !exists {
		t.Fatal("replayed BYE removed dialog")
	}
	validBYE := newRequest(sip.MethodBYE, 13, true)
	failingConnection := &blockingFlowResponseConnection{
		flowConnection: connection,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("inbound BYE SIP OK write failed"),
	}
	validBYE.SetConnection(failingConnection)
	failedDone := make(chan struct{})
	go func() {
		api.sipByeGeneric(&sip.Context{Request: validBYE, Tx: sip.NewTransaction("cascade-dialog-cseq-bye-write-failure", failingConnection), DeviceID: gb10PlatformID, Source: connection.remote})
		close(failedDone)
	}()
	select {
	case <-failingConnection.started:
	case <-time.After(time.Second):
		close(failingConnection.release)
		t.Fatal("valid BYE SIP response write did not start")
	}
	if stopped {
		close(failingConnection.release)
		<-failedDone
		t.Fatal("valid BYE stopped cascade media before SIP OK completed")
	}
	close(failingConnection.release)
	select {
	case <-failedDone:
	case <-time.After(time.Second):
		t.Fatal("valid BYE handler did not return after SIP OK write failure")
	}
	if stopped {
		t.Fatal("valid BYE stopped cascade media after SIP OK write failure")
	}
	if _, exists := api.inviteDialogs.Load(dialog.CallID); !exists {
		t.Fatal("valid BYE write failure removed dialog")
	}

	retriedBYE := validBYE.Clone().(*sip.Request)
	retriedBYE.SetConnection(connection)
	retriedBYE.SetSource(connection.remote)
	retriedBYE.SetDestination(connection.local)
	api.sipByeGeneric(&sip.Context{Request: retriedBYE, Tx: sip.NewTransaction("cascade-dialog-cseq-bye-valid-retry", connection), DeviceID: gb10PlatformID, Source: connection.remote})
	if got := response(t); !strings.Contains(got, "SIP/2.0 200 OK") {
		t.Fatalf("retried valid BYE response = %s", got)
	}
	if !stopped {
		t.Fatal("valid BYE did not stop cascade media")
	}
	if _, exists := api.inviteDialogs.Load(dialog.CallID); exists {
		t.Fatal("valid BYE did not remove dialog")
	}
}

func TestCascadeHistoryTeardownCommitsOnlyAfterSuccessfulSIPOK(t *testing.T) {
	connection := newFlowConnection()
	connection.remote = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	worker.updateStatus(func(state *CascadePlatformStatus) { state.Registered = true })
	callID := sip.CallID("cascade-history-teardown-write-failure")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "history-teardown-remote"})
	local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
	invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetSeqNo(10).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	invite.SetConnection(connection)
	invite.SetSource(connection.remote)
	invite.SetDestination(connection.local)
	stopped := false
	source := &cascadeSourceRef{
		key: "history:Download:device:channel:cascade:teardown", mode: historyModeDownload,
		channel: &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}, stream: &Streams{},
	}
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID, RemoteTag: "history-teardown-remote", LocalTag: "history-teardown-local",
		TagsBound: true, Established: true, InitialRemoteCSeq: 10, InitialRemoteCSeqSet: true,
		RemoteCSeq: 10, RemoteCSeqSet: true, RemoteMethod: sip.MethodInvite, Request: invite,
		Cascade: &cascadeMediaSession{worker: worker, source: source, cancel: func() { stopped = true }}, UpdatedAt: time.Now(),
	}
	api.inviteDialogs.Store(dialog.CallID, dialog)
	controlCalls := 0
	api.cascadeControlHistory = func(context.Context, *ControlHistoryInput) error {
		controlCalls++
		return nil
	}
	to := local.Clone()
	to.Params.Add("tag", sip.String{Str: dialog.LocalTag})
	request := sip.NewRequest("", sip.MethodInfo, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(to).SetMethod(sip.MethodInfo).SetSeqNo(11).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(),
		[]byte("TEARDOWN RTSP/1.0\r\nCSeq: 1\r\n\r\n"))
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Content-Type", Contents: "Application/MANSRTSP"})
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	failingConnection := &blockingFlowResponseConnection{
		flowConnection: connection,
		started:        make(chan struct{}, 1),
		release:        make(chan struct{}),
		writeErr:       errors.New("TEARDOWN SIP OK write failed"),
	}
	request.SetConnection(failingConnection)
	done := make(chan struct{})
	go func() {
		api.sipInfoGeneric(&sip.Context{Request: request, Tx: sip.NewTransaction("cascade-history-teardown-write-failure", failingConnection), DeviceID: gb10PlatformID, Source: connection.remote})
		close(done)
	}()
	select {
	case <-failingConnection.started:
	case <-time.After(time.Second):
		close(failingConnection.release)
		t.Fatal("TEARDOWN SIP response write did not start")
	}
	if stopped {
		close(failingConnection.release)
		<-done
		t.Fatal("TEARDOWN stopped media before SIP OK completed")
	}
	close(failingConnection.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("TEARDOWN handler did not return after SIP OK write failure")
	}
	if stopped || controlCalls != 1 {
		t.Fatalf("TEARDOWN write failure state: stopped=%v control_calls=%d", stopped, controlCalls)
	}
	if _, ok := api.inviteDialogs.Load(dialog.CallID); !ok {
		t.Fatal("TEARDOWN write failure removed dialog")
	}

	retried := request.Clone().(*sip.Request)
	retried.SetConnection(connection)
	retried.SetSource(connection.remote)
	retried.SetDestination(connection.local)
	api.sipInfoGeneric(&sip.Context{Request: retried, Tx: sip.NewTransaction("cascade-history-teardown-retry", connection), DeviceID: gb10PlatformID, Source: connection.remote})
	select {
	case payload := <-connection.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("retried TEARDOWN response = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("retried TEARDOWN response timeout")
	}
	if !stopped || controlCalls != 1 {
		t.Fatalf("retried TEARDOWN state: stopped=%v control_calls=%d", stopped, controlCalls)
	}
	if _, ok := api.inviteDialogs.Load(dialog.CallID); ok {
		t.Fatal("retried TEARDOWN left dialog")
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

func TestTerminateCascadeMediaDetachesBeforeUpstreamBYEWriteCompletes(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.30:5060")
	remote.Params.Add("tag", sip.String{Str: "cascade-terminal-remote"})
	sipServer := sip.NewServer(local)
	media := &fakeRTPMediaService{}
	api := &GB28181API{
		cfg: &conf.SIP{ID: gb10DeviceID, Domain: "3402000000"}, sms: media,
		cascadeSources: make(map[string]*cascadeSourceRef),
	}
	server := &Server{Server: sipServer, gb: api, fromAddress: *local}
	api.svr = server

	localRaw, peer := net.Pipe()
	observed := &cascadeObservedWriteConn{Conn: localRaw, started: make(chan struct{}, 1)}
	platform := testSharedCascadePlatform(t)
	platform.transport = "tcp"
	platform.remote = &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}
	worker := newCascadeWorker(server, platform)
	worker.dialTCP = func(context.Context, string) (net.Conn, error) { return observed, nil }
	t.Cleanup(func() {
		worker.cancel()
		_ = peer.Close()
		server.Close()
	})

	callID := sip.CallID("cascade-terminal-media-loss")
	request := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetSeqNo(1).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetSource(platform.remote)
	request.SetDestination(&net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, StreamID: "cascade-terminal-source"}
	source := &cascadeSourceRef{
		key: "play:" + gb10DeviceID + ":" + testCascadeChannelID, refs: 1, stopDone: make(chan struct{}),
		server: &sms.MediaServer{}, stream: stream, mode: historyModePlay,
	}
	session := &cascadeMediaSession{
		worker: worker, source: source, server: source.server, ssrc: "0100000011",
		vhost: cascadeSourceVHost, app: cascadeSourceApp, stream: stream.StreamID,
	}
	dialog := &inboundInviteDialog{
		CallID: string(callID), DeviceID: gb10PlatformID, Request: request, Response: response,
		Established: true, Cascade: session,
	}
	api.cascadeSources[source.key] = source
	api.inviteDialogs.Store(dialog.CallID, dialog)

	done := make(chan struct{})
	go func() {
		api.terminateCascadeSessionsForStream(stream)
		close(done)
	}()
	select {
	case <-observed.started:
	case <-time.After(3 * time.Second):
		t.Fatal("cascade upstream BYE write was not attempted")
	}

	if _, ok := api.inviteDialogs.Load(dialog.CallID); ok {
		t.Fatal("lost cascade media retained dialog while upstream BYE write was blocked")
	}
	api.cascadeMediaMu.Lock()
	_, sourceRetained := api.cascadeSources[source.key]
	refs, ended := source.refs, source.ended
	api.cascadeMediaMu.Unlock()
	if sourceRetained || refs != 0 || !ended {
		t.Fatalf("lost cascade source remained active: retained=%v refs=%d ended=%v", sourceRetained, refs, ended)
	}
	media.mu.Lock()
	stopCalls, stopped := media.stopCalls, media.stopped
	media.mu.Unlock()
	if stopCalls != 1 || stopped.SSRC != session.ssrc || stopped.Stream != stream.StreamID {
		t.Fatalf("cascade RTP sender cleanup = calls=%d request=%+v", stopCalls, stopped)
	}

	_ = peer.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cascade termination did not return after blocked BYE write was released")
	}
}

func TestAcquireCascadeSourceDoesNotReuseSourceMarkedEnded(t *testing.T) {
	channel := &ipc.Channel{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID}
	offer := &cascadeVideoOffer{Mode: historyModePlay}
	key := cascadeSourceKey(channel, offer)
	source := &cascadeSourceRef{
		key: key, refs: 1, ended: true, stopDone: make(chan struct{}),
		stream: &Streams{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, StreamID: "ended-cascade-source"},
	}
	api := &GB28181API{cascadeSources: map[string]*cascadeSourceRef{key: source}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	got, err := api.acquireCascadeSource(ctx, &sms.MediaServer{}, &ipc.Device{}, channel, offer)
	if got != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ended cascade source acquisition = source=%p err=%v", got, err)
	}
	api.cascadeMediaMu.Lock()
	refs := source.refs
	api.cascadeMediaMu.Unlock()
	if refs != 1 {
		t.Fatalf("ended cascade source refs = %d; want 1", refs)
	}
}

func TestCascadeSourceUsableRejectsSourceEndedAfterAcquire(t *testing.T) {
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, StreamID: "cascade-ended-after-acquire"}
	source := &cascadeSourceRef{key: "cascade-ended-after-acquire-key", refs: 1, stream: stream}
	api := &GB28181API{cascadeSources: map[string]*cascadeSourceRef{source.key: source}}
	if !api.cascadeSourceUsable(source) {
		t.Fatal("active cascade source was rejected")
	}

	api.markCascadeSourcesEnded(stream)
	if api.cascadeSourceUsable(source) {
		t.Fatal("source ended after acquisition remained usable")
	}
}

func TestCascadeStreamTerminationDoesNotEndReplacementGeneration(t *testing.T) {
	ended := &Streams{DeviceID: gb10DeviceID, ChannelID: testCascadeChannelID, StreamID: "reused-cascade-stream-id"}
	replacementStream := &Streams{DeviceID: ended.DeviceID, ChannelID: ended.ChannelID, StreamID: ended.StreamID}
	source := &cascadeSourceRef{
		key: "replacement-generation", refs: 1, stream: replacementStream, stopDone: make(chan struct{}),
	}
	session := &cascadeMediaSession{source: source}
	dialog := &inboundInviteDialog{CallID: "replacement-generation-dialog", Cascade: session}
	api := &GB28181API{cascadeSources: map[string]*cascadeSourceRef{source.key: source}}
	api.inviteDialogs.Store(dialog.CallID, dialog)

	api.terminateCascadeSessionsForStream(ended)

	if actual, ok := api.inviteDialogs.Load(dialog.CallID); !ok || actual != dialog {
		t.Fatal("old stream termination removed replacement cascade dialog")
	}
	api.cascadeMediaMu.Lock()
	current, refs, sourceEnded := api.cascadeSources[source.key], source.refs, source.ended
	api.cascadeMediaMu.Unlock()
	if current != source || refs != 1 || sourceEnded {
		t.Fatalf("replacement cascade source changed: current=%p refs=%d ended=%v", current, refs, sourceEnded)
	}
}

type cascadeObservedWriteConn struct {
	net.Conn
	started chan struct{}
}

func (c *cascadeObservedWriteConn) Write(payload []byte) (int, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	return c.Conn.Write(payload)
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
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s-%s?mode=memory&cache=shared", t.Name(), sip.RandString(12))))
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
	// GORM 会对带 default 标签的零值写入数据库默认值 1；该夹具明确验证 UDP。
	if err := db.Model(device).UpdateColumn("stream_mode", 0).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	return ipc.NewAdapter(store, uniqueid.Core{}), device, channel
}

func TestResolveCascadeChannelUsesPreferredDirectPlatform(t *testing.T) {
	adapter, _, _ := newCascadeMediaCore(t)
	cameraID := "34020000001320000078"
	directD := "34020000002000000005"
	for _, device := range []*ipc.Device{
		{ID: "GB_route_c", DeviceID: testCascadePathC, Type: ipc.TypeGB28181, IsOnline: true},
		{ID: "GB_route_d", DeviceID: directD, Type: ipc.TypeGB28181, IsOnline: true},
	} {
		if err := adapter.Store().Device().Create(t.Context(), device); err != nil {
			t.Fatal(err)
		}
	}
	for _, channel := range []*ipc.Channel{
		{ID: "GBC_route_c", DID: "GB_route_c", DeviceID: testCascadePathC, ChannelID: cameraID, Type: ipc.TypeGB28181, IsOnline: true},
		{ID: "GBC_route_d", DID: "GB_route_d", DeviceID: directD, ChannelID: cameraID, Type: ipc.TypeGB28181, IsOnline: true},
	} {
		if err := adapter.Store().Channel().Create(t.Context(), channel); err != nil {
			t.Fatal(err)
		}
	}

	platform := testSharedCascadePlatform(t)
	platform.localID = testCascadePathB
	mappedC := "34020000002000000006"
	platform.exposedChannelMap[mappedC] = testCascadePathC
	api := &GB28181API{core: adapter}
	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "direct C", path: testCascadePathC + "-" + testCascadePathE, want: testCascadePathC},
		{name: "mapped C", path: mappedC + "-" + testCascadePathE, want: testCascadePathC},
		{name: "direct D", path: directD + "-" + testCascadePathE, want: directD},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel, device, err := api.resolveCascadeChannel(cameraID, test.path, platform)
			if err != nil {
				t.Fatal(err)
			}
			if channel.DeviceID != test.want || device.DeviceID != test.want {
				t.Fatalf("preferred route channel/device = %s/%s, want %s", channel.DeviceID, device.DeviceID, test.want)
			}
		})
	}
	if _, _, err := api.resolveCascadeChannel(cameraID, "34020000002000000009-"+testCascadePathE, platform); err == nil || !strings.Contains(err.Error(), "preferred path") {
		t.Fatalf("unavailable preferred route error = %v", err)
	}
}

func TestCascadeRealtimeInviteEstablishesAndReleasesB2BUA(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	routeDevice := &ipc.Device{ID: "GB_cascade_path_c", DeviceID: testCascadePathC, Type: ipc.TypeGB28181, IsOnline: true}
	routeChannel := &ipc.Channel{
		ID: "GBC_cascade_path_c", DID: routeDevice.ID, DeviceID: routeDevice.DeviceID,
		ChannelID: testCascadeChannelID, Name: "Front Gate via C", Type: ipc.TypeGB28181, IsOnline: true,
	}
	if err := adapter.Store().Device().Create(t.Context(), routeDevice); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Store().Channel().Create(t.Context(), routeChannel); err != nil {
		t.Fatal(err)
	}
	persistentChannel = routeChannel
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
	api.cascadePlay = func(_ context.Context, in *PlayInput) error {
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
		key := resolvePlaySessionKey(in.Channel.DeviceID, in.Channel.ChannelID, in.sessionKey)
		api.streams.Delete(key)
		_, err := media.CloseRTPServer(mediaServer, zlm.CloseRTPServerRequest{StreamID: createdStreamID})
		return err
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
	request.AppendHeader(&sip.GenericHeader{
		HeaderName: "Subject",
		Contents:   testExposedChannelID + ":0100000011," + gb10PlatformID + ":window-live",
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
	remote.Params.Add("tag", sip.String{Str: dialog.RemoteTag})
	local.Params.Add("tag", sip.String{Str: dialog.LocalTag})
	bye := sip.NewRequest("", sip.MethodBYE, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(local).SetMethod(sip.MethodBYE).SetCallID(&callID).
			SetSeqNo(2).
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

func TestResolveCascadeChannelRejectsExpiredRuntimeRegistration(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	runtime := &Device{
		IsOnline:       true,
		LastRegisterAt: time.Now().Add(-time.Minute),
		Expires:        10,
	}
	memory := &flowMemory{persistent: persistentDevice, runtime: runtime}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}

	_, _, err := api.resolveCascadeChannel(testCascadeChannelID, "", testSharedCascadePlatform(t))
	if !errors.Is(err, ErrDeviceOffline) {
		t.Fatalf("expired runtime cascade channel result = %v; want %v", err, ErrDeviceOffline)
	}
}

func TestCascadeHistoryDialogFourVersionEndToEnd(t *testing.T) {
	const start, end = int64(1711929600), int64(1711933200)
	tests := []struct {
		name           string
		version        GBProtocolVersion
		controlVersion string
		downloadSpeed  int
		omitSSRC       bool
	}{
		{name: "2011", version: GBVersion10, controlVersion: "MANSRTSP/1.0"},
		{name: "2011_missing_y", version: GBVersion10, controlVersion: "MANSRTSP/1.0", omitSSRC: true},
		{name: "2014", version: GBVersion11, controlVersion: "RTSP/1.0", downloadSpeed: 4},
		{name: "2016", version: GBVersion20, controlVersion: "RTSP/1.0", downloadSpeed: 4},
		{name: "2022", version: GBVersion30, controlVersion: "RTSP/1.0", downloadSpeed: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, persistentDevice, _ := newCascadeMediaCore(t)
			if test.version == GBVersion30 {
				routeDevice := &ipc.Device{ID: "GB_history_path_c", DeviceID: testCascadePathC, Type: ipc.TypeGB28181, IsOnline: true}
				routeChannel := &ipc.Channel{
					ID: "GBC_history_path_c", DID: routeDevice.ID, DeviceID: routeDevice.DeviceID,
					ChannelID: testCascadeChannelID, Name: "History via C", Type: ipc.TypeGB28181, IsOnline: true,
				}
				if err := adapter.Store().Device().Create(t.Context(), routeDevice); err != nil {
					t.Fatal(err)
				}
				if err := adapter.Store().Channel().Create(t.Context(), routeChannel); err != nil {
					t.Fatal(err)
				}
			}
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
			if test.omitSSRC {
				api.cfg.Domain = "3402000000"
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
				_, err := media.CloseRTPServer(mediaServer, zlm.CloseRTPServerRequest{StreamID: "history-source"})
				return err
			}
			var control *ControlHistoryInput
			api.cascadeControlHistory = func(_ context.Context, in *ControlHistoryInput) error {
				control = in
				return nil
			}

			remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@remote.example")
			remote.Params.Add("tag", sip.String{Str: "history-remote-tag-" + test.name})
			local := mustFlowAddress(t, "sip:"+testExposedChannelID+"@local.example")
			callID := sip.CallID("cascade-history-" + test.name)
			offerBody := cascadeHistoryOfferSDP(historyModeDownload, "RTP/AVP", "192.0.2.30", "", start, end, test.downloadSpeed)
			if test.omitSSRC {
				offerBody = []byte(strings.Replace(string(offerBody), "y=1100000011\r\n", "", 1))
			}
			invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodInvite).SetCallID(&callID).
					SetContentType(&sip.ContentTypeSDP).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(),
				offerBody)
			invite.SetConnection(connection)
			invite.SetSource(connection.remote)
			invite.SetDestination(connection.local)
			if test.version.AtLeast(GBVersion11) {
				invite.AppendHeader(&sip.GenericHeader{
					HeaderName: "Subject",
					Contents:   buildGBInviteSubject(testExposedChannelID, "1100000001", worker.platform.serverID),
				})
			}
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
			negotiatedSSRC := "1100000011"
			select {
			case payload := <-connection.writes:
				response := string(payload)
				expectedValues := []string{
					"SIP/2.0 200 OK", "s=Download", "u=" + testExposedChannelID + ":3",
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
				if test.omitSSRC {
					ssrc := directTCPSDPLineValue([]byte(response), "y")
					if !validGBSSRC(ssrc) || ssrc[0] != '1' {
						t.Fatalf("generated history SSRC = %q", ssrc)
					}
					negotiatedSSRC = ssrc
				}
			case <-time.After(time.Second):
				t.Fatal("history INVITE response timeout")
			}
			if started == nil || started.Mode != historyModeDownload || started.Transport != historyTransportRTP || started.DownloadSpeed != test.downloadSpeed || started.RecordType == nil || *started.RecordType != 3 {
				t.Fatalf("history source input = %+v", started)
			}
			if media.startCalls != 1 || media.started.Stream != "history-source" || media.started.DstURL != "192.0.2.30" || media.started.DstPort != 30000 {
				t.Fatalf("history RTP forwarding = %+v", media.started)
			}
			dialogValue, ok := api.inviteDialogs.Load(string(callID))
			if !ok {
				t.Fatal("history dialog not stored")
			}
			dialog := dialogValue.(*inboundInviteDialog)
			local.Params.Add("tag", sip.String{Str: dialog.LocalTag})

			ack := sip.NewRequest("", sip.MethodACK, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(local).SetMethod(sip.MethodACK).SetCallID(&callID).
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
				sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(local).SetMethod(sip.MethodInfo).SetCallID(&callID).
					SetSeqNo(2).
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
				sip.NewHeaderBuilder().SetFrom(remote).SetToWithParam(local).SetMethod(sip.MethodBYE).SetCallID(&callID).
					SetSeqNo(3).
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
			if stopCalls != 1 || media.stopped.SSRC != negotiatedSSRC || media.closed.StreamID != "history-source" {
				t.Fatalf("history cleanup = stop %d, RTP %+v, source %+v", stopCalls, media.stopped, media.closed)
			}
			if _, ok := api.inviteDialogs.Load(string(callID)); ok {
				t.Fatal("history dialog survived BYE")
			}
		})
	}
}
