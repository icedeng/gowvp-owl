package gbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestSIPResponseContextReturnsCallerCancellation(t *testing.T) {
	tx := sip.NewTransaction("context-cancel", nil)
	defer tx.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	if _, err := sipResponseContext(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("SIP response cancellation error = %v; want %v", err, context.Canceled)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("SIP response cancellation took %s", elapsed)
	}
	if response, err := tx.GetResponseContext(context.Background()); response != nil || err != nil {
		t.Fatalf("cancelled SIP transaction remained active: response=%v err=%v", response, err)
	}
}

func TestSIPResponseContextRejectsMissingTransaction(t *testing.T) {
	if _, err := sipResponseContext(context.Background(), nil); err == nil {
		t.Fatal("missing SIP transaction was accepted")
	}
}

func TestSIPResponseContextClosesNonInviteTransactionAfterFinalResponse(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	server := sip.NewServer(local)
	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go server.ProcessTCPConnection(connection)
	defer func() {
		_ = remoteRaw.Close()
		server.Close()
	}()

	callID := sip.CallID("completed-message-transaction")
	request := sip.NewRequest("", sip.MethodMessage, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodMessage).SetSeqNo(26).SetFrom(local).SetTo(remote).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-completed-message"})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())

	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		message, err := readAnnexGTestSIPFrame(bufio.NewReader(remoteRaw))
		if err == nil {
			_, err = remoteRaw.Write([]byte(cancelledInviteTestResponse(message, 200, "OK")))
		}
		remoteErr <- err
	}()

	tx, err := server.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sipResponseContext(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	if err := <-remoteErr; err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if response, err := tx.GetResponseContext(waitCtx); response != nil || err != nil {
		t.Fatalf("completed non-INVITE transaction remained active: response=%v err=%v", response, err)
	}
}

func TestSIPResponseWithTimeoutClosesTransactionAfterFinalResponse(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	server := sip.NewServer(local)
	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go server.ProcessTCPConnection(connection)
	defer func() {
		_ = remoteRaw.Close()
		server.Close()
	}()

	callID := sip.CallID("completed-options-transaction")
	request := sip.NewRequest("", sip.MethodOptions, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodOptions).SetSeqNo(27).SetFrom(local).SetTo(remote).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-completed-options"})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())

	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		message, err := readAnnexGTestSIPFrame(bufio.NewReader(remoteRaw))
		if err == nil {
			_, err = remoteRaw.Write([]byte(cancelledInviteTestResponse(message, 200, "OK")))
		}
		remoteErr <- err
	}()

	tx, err := server.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sipResponseWithTimeout(t.Context(), tx, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := <-remoteErr; err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if response, err := tx.GetResponseContext(waitCtx); response != nil || err != nil {
		t.Fatalf("completed OPTIONS transaction remained active: response=%v err=%v", response, err)
	}
}

func TestSIPResponseContextCancelsPendingInviteTransaction(t *testing.T) {
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	server := sip.NewServer(local)
	defer server.Close()
	callID := sip.CallID("cancel-pending-invite")
	request := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodInvite).SetSeqNo(23).SetFrom(local).SetTo(remote).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-context-cancel"})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(baseConnection.local)
	request.SetDestination(baseConnection.remote)
	tx, err := server.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	invite := string(<-baseConnection.writes)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sipResponseContext(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled INVITE wait error = %v", err)
	}
	cancelPayload := string(<-baseConnection.writes)
	if !strings.HasPrefix(cancelPayload, "CANCEL ") {
		t.Fatalf("cancelled INVITE payload = %s", cancelPayload)
	}
	if cascadeTestHeader(cancelPayload, "Via") != cascadeTestHeader(invite, "Via") {
		t.Fatalf("CANCEL Via = %q, INVITE Via = %q", cascadeTestHeader(cancelPayload, "Via"), cascadeTestHeader(invite, "Via"))
	}
	if cascadeTestHeader(cancelPayload, "CSeq") != "23 CANCEL" {
		t.Fatalf("CANCEL CSeq = %q", cascadeTestHeader(cancelPayload, "CSeq"))
	}
}

func TestSIPResponseContextCancelledInviteAcknowledges487(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	server := sip.NewServer(local)
	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go server.ProcessTCPConnection(connection)
	defer func() {
		_ = remoteRaw.Close()
		server.Close()
	}()

	callID := sip.CallID("cancelled-invite-487")
	request := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodInvite).SetSeqNo(25).SetFrom(local).SetTo(remote).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-cancel-487"})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())

	observed := make(chan []string, 1)
	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(remoteRaw)
		invite, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		cancel, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		if _, err := remoteRaw.Write([]byte(cancelledInviteTestResponse(cancel, 200, "OK"))); err != nil {
			remoteErr <- err
			return
		}
		if _, err := remoteRaw.Write([]byte(cancelledInviteTestResponse(invite, 487, "Request Terminated"))); err != nil {
			remoteErr <- err
			return
		}
		ack, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		observed <- []string{invite, cancel, ack}
	}()

	tx, err := server.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sipInviteResponseContext(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled INVITE wait error = %v", err)
	}

	select {
	case err := <-remoteErr:
		t.Fatal(err)
	case messages := <-observed:
		if !strings.HasPrefix(messages[0], "INVITE ") || !strings.HasPrefix(messages[1], "CANCEL ") || !strings.HasPrefix(messages[2], "ACK ") {
			t.Fatalf("SIP order = %q / %q / %q", firstSIPLine(messages[0]), firstSIPLine(messages[1]), firstSIPLine(messages[2]))
		}
		if cascadeTestHeader(messages[2], "CSeq") != "25 ACK" || cascadeTestHeader(messages[2], "Via") != cascadeTestHeader(messages[0], "Via") {
			t.Fatalf("487 ACK transaction headers = CSeq %q Via %q", cascadeTestHeader(messages[2], "CSeq"), cascadeTestHeader(messages[2], "Via"))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for CANCEL/487/ACK flow")
	}
}

func TestSIPResponseContextCancelledInviteAcknowledgesLate2xxAndSendsBYE(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	server := sip.NewServer(local)
	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go server.ProcessTCPConnection(connection)
	defer func() {
		_ = remoteRaw.Close()
		server.Close()
	}()

	callID := sip.CallID("cancelled-invite-late-2xx")
	request := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodInvite).SetSeqNo(27).SetFrom(local).SetTo(remote).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-cancel-late-2xx"})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())

	observed := make(chan []string, 1)
	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(remoteRaw)
		invite, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		cancel, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		if _, err := remoteRaw.Write([]byte(cancelledInviteTestResponse(cancel, 200, "OK"))); err != nil {
			remoteErr <- err
			return
		}
		if _, err := remoteRaw.Write([]byte(cancelledInviteTestResponse(invite, 200, "OK"))); err != nil {
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
		if _, err := remoteRaw.Write([]byte(cancelledInviteTestResponse(invite, 200, "OK"))); err != nil {
			remoteErr <- err
			return
		}
		retransmittedACK, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		if _, err := remoteRaw.Write([]byte(cancelledInviteTestResponse(bye, 200, "OK"))); err != nil {
			remoteErr <- err
			return
		}
		observed <- []string{invite, cancel, ack, bye, retransmittedACK}
	}()

	tx, err := server.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sipInviteResponseContext(ctx, tx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled INVITE wait error = %v", err)
	}

	select {
	case err := <-remoteErr:
		t.Fatal(err)
	case messages := <-observed:
		if !strings.HasPrefix(messages[0], "INVITE ") || !strings.HasPrefix(messages[1], "CANCEL ") ||
			!strings.HasPrefix(messages[2], "ACK ") || !strings.HasPrefix(messages[3], "BYE ") {
			t.Fatalf("SIP order = %q / %q / %q / %q", firstSIPLine(messages[0]), firstSIPLine(messages[1]), firstSIPLine(messages[2]), firstSIPLine(messages[3]))
		}
		if cascadeTestHeader(messages[2], "CSeq") != "27 ACK" || cascadeTestHeader(messages[3], "CSeq") != "28 BYE" {
			t.Fatalf("late 2xx cleanup CSeq = ACK %q BYE %q", cascadeTestHeader(messages[2], "CSeq"), cascadeTestHeader(messages[3], "CSeq"))
		}
		if messages[4] != messages[2] {
			t.Fatalf("retransmitted late 2xx ACK changed:\nfirst=%s\nsecond=%s", messages[2], messages[4])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for late 2xx ACK/BYE cleanup")
	}
}

func cancelledInviteTestResponse(request string, status int, reason string) string {
	to := cascadeTestHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=cancelled-device"
	}
	return fmt.Sprintf("SIP/2.0 %d %s\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\nContent-Length: 0\r\n\r\n",
		status, reason, cascadeTestHeader(request, "Via"), cascadeTestHeader(request, "From"), to,
		cascadeTestHeader(request, "Call-ID"), cascadeTestHeader(request, "CSeq"))
}

func TestSIPResponseContextCancelsInviteWhenTransactionEndsWithoutResponse(t *testing.T) {
	baseConnection := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: baseConnection}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	server := sip.NewServer(local)
	defer server.Close()
	callID := sip.CallID("closed-pending-invite")
	request := sip.NewRequest("", sip.MethodInvite, remote.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodInvite).SetSeqNo(24).SetFrom(local).SetTo(remote).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.20", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: "z9hG4bK-closed-cancel"})}).Build(), nil)
	request.SetConnection(connection)
	request.SetSource(baseConnection.local)
	request.SetDestination(baseConnection.remote)
	tx, err := server.Request(request)
	if err != nil {
		t.Fatal(err)
	}
	<-baseConnection.writes
	tx.Close()

	if _, err := sipResponseContext(context.Background(), tx); err == nil || !strings.Contains(err.Error(), "response timeout") {
		t.Fatalf("closed INVITE wait error = %v", err)
	}
	cancelPayload := string(<-baseConnection.writes)
	if !strings.HasPrefix(cancelPayload, "CANCEL ") || cascadeTestHeader(cancelPayload, "CSeq") != "24 CANCEL" {
		t.Fatalf("closed INVITE CANCEL payload = %s", cancelPayload)
	}
}

func TestGBOperationsRejectCancelledContextBeforeSideEffects(t *testing.T) {
	api := &GB28181API{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		call func() error
	}{
		{name: "catalog", call: func() error { return api.QueryCatalogContext(ctx, gb10DeviceID) }},
		{name: "device info", call: func() error { return api.QueryDeviceInfoContext(ctx, gb10DeviceID) }},
		{name: "basic config", call: func() error { return api.QueryConfigDownloadBasicContext(ctx, gb10DeviceID) }},
		{name: "play", call: func() error { return api.PlayContext(ctx, nil) }},
		{name: "stop play", call: func() error { return api.StopPlay(ctx, nil) }},
		{name: "history", call: func() error { return api.StartHistory(ctx, nil) }},
		{name: "stop history", call: func() error { return api.StopHistory(ctx, nil) }},
		{name: "history control", call: func() error { return api.ControlHistory(ctx, nil) }},
		{name: "ptz", call: func() error { _, err := api.PTZContext(ctx, nil); return err }},
		{name: "device control", call: func() error { _, err := api.DeviceControl(ctx, nil); return err }},
		{name: "device query", call: func() error { _, err := api.DeviceQuery(ctx, nil); return err }},
		{name: "device config", call: func() error { _, err := api.SetDeviceConfig(ctx, nil); return err }},
		{name: "record query", call: func() error { _, err := api.QueryRecordList(ctx, nil); return err }},
		{name: "subscribe", call: func() error { return api.Subscribe(ctx, nil) }},
		{name: "time sync", call: func() error { return api.SyncTime(ctx, nil) }},
		{name: "options", call: func() error { return api.ProbeOptions(ctx, nil) }},
		{name: "upgrade", call: func() error { _, err := api.Upgrade(ctx, nil); return err }},
		{name: "snapshot", call: func() error { _, err := api.QuerySnapshotContext(ctx, "", "", ""); return err }},
		{name: "voice", call: func() error { return api.StartVoice(ctx, nil) }},
		{name: "stop voice", call: func() error { return api.StopVoice(ctx, nil) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled operation error = %v; want %v", err, context.Canceled)
			}
		})
	}
}
