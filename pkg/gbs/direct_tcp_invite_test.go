package gbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestStartHistoryDirectTCPUsesRealSIPAndFileTransfer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	payload := []byte("0123456789")
	fileTransferErr := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			fileTransferErr <- acceptErr
			return
		}
		defer conn.Close()
		_, writeErr := conn.Write(payload)
		fileTransferErr <- writeErr
	}()

	fixture := newCascadeDownstreamSIPFixture(t, GBVersion11)
	adapter, _, inputChannel := newCascadeMediaCore(t)
	storageDir := t.TempDir()
	manager := NewDirectTCPDownloadManager(DirectTCPDownloadOptions{
		StorageDir: storageDir, MaxFileSize: 1024,
		DialTimeout: time.Second, FirstByteTimeout: time.Second, IdleTimeout: 100 * time.Millisecond, TotalTimeout: 2 * time.Second,
		AllowAddressMismatch: true, AllowedAddressCIDRs: []string{"127.0.0.0/8"}, AllowUnsafeAddresses: true,
	})
	t.Cleanup(manager.Shutdown)
	fixture.api.boot = &conf.Bootstrap{Media: conf.Media{SDPIP: "192.0.2.20"}}
	fixture.api.core = adapter
	fixture.api.streams = &conc.Map[string, *Streams]{}
	fixture.api.directDownloads = manager
	fixture.api.directPolicy = directTCPRuntimePolicy{
		Enabled: true, OfferPort: 9, Allowlist: map[string]struct{}{gb10DeviceID: {}},
	}

	sessionID := make(chan string, 1)
	sipObserved := make(chan []string, 1)
	sipErr := make(chan error, 1)
	go func() {
		_ = fixture.peer.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(fixture.peer)
		invite, readErr := readAnnexGTestSIPFrame(reader)
		if readErr != nil {
			sipErr <- readErr
			return
		}
		ssrc := directTCPSDPLineValue([]byte(invite), "y")
		callID := annexGTestSIPHeader(invite, "Call-ID")
		host, port, splitErr := net.SplitHostPort(listener.Addr().String())
		if splitErr != nil {
			sipErr <- splitErr
			return
		}
		body := "v=0\r\n" +
			"o=" + gb10DeviceID + " 0 0 IN IP4 " + host + "\r\n" +
			"s=Download\r\n" +
			"c=IN IP4 " + host + "\r\n" +
			"t=0 0\r\n" +
			"m=video " + port + " tcp 96\r\n" +
			"a=sendonly\r\n" +
			fmt.Sprintf("a=filesize:%d\r\n", len(payload)) +
			"y=" + ssrc + "\r\n"
		if _, writeErr := io.WriteString(fixture.peer, directTCPTestResponse(invite, body, "Application/SDP")); writeErr != nil {
			sipErr <- writeErr
			return
		}
		ack, readErr := readAnnexGTestSIPFrame(reader)
		if readErr != nil {
			sipErr <- readErr
			return
		}
		sessionID <- callID
		bye, readErr := readAnnexGTestSIPFrame(reader)
		if readErr != nil {
			sipErr <- readErr
			return
		}
		if _, writeErr := io.WriteString(fixture.peer, annexGTestSIPResponse(bye, 200, "OK", "")); writeErr != nil {
			sipErr <- writeErr
			return
		}
		sipObserved <- []string{invite, ack, bye}
	}()

	err = fixture.api.StartHistory(t.Context(), &HistoryInput{
		Channel: inputChannel, Mode: historyModeDownload, Transport: historyTransportDirectTCP,
		StartAt: time.Unix(1700000000, 0), EndAt: time.Unix(1700000060, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	select {
	case id = <-sessionID:
	case err = <-sipErr:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for direct TCP session")
	}
	state, err := manager.Wait(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != directTCPStatusCompleted || state.Received != int64(len(payload)) || !state.SizeVerified || state.Output == "" {
		t.Fatalf("direct TCP state = %+v", state)
	}
	written, err := os.ReadFile(filepath.Join(storageDir, state.Output))
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(payload) {
		t.Fatalf("downloaded payload = %q", written)
	}
	select {
	case err = <-fileTransferErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("direct TCP file sender did not finish")
	}
	select {
	case err = <-sipErr:
		t.Fatal(err)
	case messages := <-sipObserved:
		if !strings.HasPrefix(messages[0], "INVITE ") || !strings.HasPrefix(messages[1], "ACK ") || !strings.HasPrefix(messages[2], "BYE ") {
			t.Fatalf("SIP order = %q / %q / %q", firstSIPLine(messages[0]), firstSIPLine(messages[1]), firstSIPLine(messages[2]))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for direct TCP BYE")
	}
	if _, ok := fixture.api.streams.Load(historyKey(historyModeDownload, gb10DeviceID, testCascadeChannelID)); ok {
		t.Fatal("completed direct TCP session remained in stream registry")
	}
}

func TestOfflineCleanupCancelsStartingDirectTCPHistory(t *testing.T) {
	fixture := newCascadeDownstreamSIPFixture(t, GBVersion11)
	adapter, _, inputChannel := newCascadeMediaCore(t)
	manager := NewDirectTCPDownloadManager(DirectTCPDownloadOptions{
		StorageDir: t.TempDir(), MaxFileSize: 1024,
		DialTimeout: time.Second, FirstByteTimeout: time.Second, IdleTimeout: time.Second, TotalTimeout: 2 * time.Second,
		AllowAddressMismatch: true, AllowedAddressCIDRs: []string{"127.0.0.0/8"}, AllowUnsafeAddresses: true,
	})
	t.Cleanup(manager.Shutdown)
	fixture.api.boot = &conf.Bootstrap{Media: conf.Media{SDPIP: "192.0.2.20"}}
	fixture.api.core = adapter
	fixture.api.streams = &conc.Map[string, *Streams]{}
	fixture.api.directDownloads = manager
	fixture.api.directPolicy = directTCPRuntimePolicy{
		Enabled: true, OfferPort: 9, Allowlist: map[string]struct{}{gb10DeviceID: {}},
	}

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
		startDone <- fixture.api.StartHistory(context.Background(), &HistoryInput{
			Channel: inputChannel, Mode: historyModeDownload, Transport: historyTransportDirectTCP,
			StartAt: time.Unix(1700000000, 0), EndAt: time.Unix(1700000060, 0),
		})
	}()
	waitForStartingMediaInvite(t, inviteReceived, remoteErr, "direct TCP Download")

	cleanupOfflineTestDevice(fixture.api)
	select {
	case err := <-startDone:
		if !errors.Is(err, ErrDeviceOffline) {
			t.Fatalf("direct TCP StartHistory error = %v; want %v", err, ErrDeviceOffline)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("starting direct TCP Download did not stop after offline cleanup")
	}
	if fixture.api.streams.Len() != 0 {
		t.Fatal("offline cleanup retained a starting direct TCP stream")
	}
	if _, ok := manager.FindByChannel(gb10DeviceID, testCascadeChannelID); ok {
		t.Fatal("offline cleanup allowed a late direct TCP download session")
	}
}

func TestDirectTCPInviteRejectsInvalidSDPContentTypeAfterACKAndBYE(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, fromAddress: *local}
	api := &GB28181API{
		cfg:  &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		boot: &conf.Bootstrap{Media: conf.Media{SDPIP: "192.0.2.20"}},
	}
	server.gb = api
	api.svr = server

	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	device := &Device{
		IsOnline: true, gbVersion: string(GBVersion11), conn: connection, source: connection.RemoteAddr(),
		to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@remote.example"),
	}
	channel := &Channel{ChannelID: gb10ChannelID, device: device}
	if err := channel.init("remote.example"); err != nil {
		t.Fatal(err)
	}
	server.memoryStorer = &flowMemory{
		persistent: &ipc.Device{DeviceID: gb10DeviceID},
		runtime:    device,
	}

	go sipServer.ProcessTCPConnection(connection)
	defer func() {
		_ = remoteRaw.Close()
		sipServer.Close()
	}()

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
		ssrc := directTCPSDPLineValue([]byte(invite), "y")
		body := "v=0\r\n" +
			"o=" + gb10DeviceID + " 0 0 IN IP4 192.0.2.30\r\n" +
			"s=Download\r\n" +
			"c=IN IP4 192.0.2.30\r\n" +
			"t=0 0\r\n" +
			"m=video 9000 tcp 96\r\n" +
			"a=sendonly\r\n" +
			"a=filesize:10\r\n" +
			"y=" + ssrc + "\r\n"
		if _, err := remoteRaw.Write([]byte(directTCPTestResponse(invite, body, ""))); err != nil {
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

	input := &HistoryInput{
		Channel: &ipc.Channel{ID: "channel-record", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		Mode:    historyModeDownload, StartAt: time.Unix(1700000000, 0), EndAt: time.Unix(1700000060, 0),
	}
	stream := &Streams{}
	_, err := api.sipInviteDirectTCPHistory(t.Context(), channel, input, stream, 30000)
	if err == nil || !strings.Contains(err.Error(), "Content-Type") {
		t.Fatalf("error = %v", err)
	}
	if stream.Resp != nil || stream.DirectSessionID != "" {
		t.Fatalf("invalid response established stream: %+v", stream)
	}

	select {
	case err := <-remoteErr:
		t.Fatal(err)
	case messages := <-observed:
		if !strings.HasPrefix(messages[0], "INVITE ") || !strings.HasPrefix(messages[1], "ACK ") || !strings.HasPrefix(messages[2], "BYE ") {
			t.Fatalf("SIP order = %q / %q / %q", firstSIPLine(messages[0]), firstSIPLine(messages[1]), firstSIPLine(messages[2]))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for INVITE/ACK/BYE")
	}
}

func TestDirectTCPInviteAccepts202AndSendsACK(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, fromAddress: *local}
	api := &GB28181API{
		cfg:  &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		boot: &conf.Bootstrap{Media: conf.Media{SDPIP: "192.0.2.20"}},
	}
	server.gb = api
	api.svr = server

	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	device := &Device{
		IsOnline: true, gbVersion: string(GBVersion11), conn: connection, source: connection.RemoteAddr(),
		to: mustFlowAddress(t, "sip:"+gb10DeviceID+"@remote.example"),
	}
	channel := &Channel{ChannelID: gb10ChannelID, device: device}
	if err := channel.init("remote.example"); err != nil {
		t.Fatal(err)
	}
	server.memoryStorer = &flowMemory{
		persistent: &ipc.Device{DeviceID: gb10DeviceID},
		runtime:    device,
	}

	go sipServer.ProcessTCPConnection(connection)
	defer func() {
		_ = remoteRaw.Close()
		sipServer.Close()
	}()

	observedACK := make(chan string, 1)
	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(remoteRaw)
		invite, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		ssrc := directTCPSDPLineValue([]byte(invite), "y")
		body := "v=0\r\n" +
			"o=" + gb10DeviceID + " 0 0 IN IP4 192.0.2.30\r\n" +
			"s=Download\r\n" +
			"c=IN IP4 192.0.2.30\r\n" +
			"t=0 0\r\n" +
			"m=video 9000 tcp 96\r\n" +
			"a=sendonly\r\n" +
			"a=filesize:10\r\n" +
			"y=" + ssrc + "\r\n"
		response := strings.Replace(directTCPTestResponse(invite, body, "Application/SDP"), "SIP/2.0 200 OK", "SIP/2.0 202 Accepted", 1)
		if _, err := remoteRaw.Write([]byte(response)); err != nil {
			remoteErr <- err
			return
		}
		ack, err := readAnnexGTestSIPFrame(reader)
		if err != nil {
			remoteErr <- err
			return
		}
		observedACK <- ack
	}()

	input := &HistoryInput{
		Channel: &ipc.Channel{ID: "channel-record", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID},
		Mode:    historyModeDownload, StartAt: time.Unix(1700000000, 0), EndAt: time.Unix(1700000060, 0),
	}
	stream := &Streams{}
	offer, err := api.sipInviteDirectTCPHistory(t.Context(), channel, input, stream, 30000)
	if err != nil {
		t.Fatal(err)
	}
	if offer.Address != "192.0.2.30:9000" || offer.FileSize != 10 || stream.Resp == nil {
		t.Fatalf("offer = %+v, stream = %+v", offer, stream)
	}

	select {
	case err := <-remoteErr:
		t.Fatal(err)
	case ack := <-observedACK:
		if !strings.HasPrefix(ack, "ACK ") {
			t.Fatalf("SIP request = %s", firstSIPLine(ack))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ACK")
	}
}

func directTCPTestResponse(request, body, contentType string) string {
	to := annexGTestSIPHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=direct-tcp-device"
	}
	var builder strings.Builder
	builder.WriteString("SIP/2.0 200 OK\r\n")
	for _, name := range []string{"Via", "From"} {
		fmt.Fprintf(&builder, "%s: %s\r\n", name, annexGTestSIPHeader(request, name))
	}
	fmt.Fprintf(&builder, "To: %s\r\n", to)
	for _, name := range []string{"Call-ID", "CSeq"} {
		fmt.Fprintf(&builder, "%s: %s\r\n", name, annexGTestSIPHeader(request, name))
	}
	if contentType != "" {
		fmt.Fprintf(&builder, "Content-Type: %s\r\n", contentType)
	}
	fmt.Fprintf(&builder, "Content-Length: %d\r\n\r\n%s", len(body), body)
	return builder.String()
}

func firstSIPLine(message string) string {
	line, _, _ := strings.Cut(message, "\r\n")
	return line
}
