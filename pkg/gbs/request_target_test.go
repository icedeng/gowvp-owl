package gbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestChannelInitRejectsMalformedSIPTarget(t *testing.T) {
	channel := &Channel{ChannelID: "bad\r\nVia: injected"}
	if err := channel.init("3402000000"); err == nil {
		t.Fatal("malformed channel SIP target was accepted")
	}
	if channel.To() != nil {
		t.Fatalf("malformed channel retained target: %+v", channel.To())
	}
}

func TestInvalidCatalogSnapshotDoesNotReplaceRuntimeChannels(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	existing := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	if err := existing.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(existing.ChannelID, existing)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}

	api.saveCatalogChannels(gb10DeviceID, []Channels{{ChannelID: "bad\r\nVia: injected"}})
	if _, ok := memory.runtime.Channels.Load(existing.ChannelID); !ok {
		t.Fatal("invalid Catalog snapshot removed the existing runtime channel")
	}
}

func TestLoadChannelsSkipsInvalidPersistedTarget(t *testing.T) {
	device := &Device{Address: "192.0.2.10:5060"}
	device.LoadChannels(
		&ipc.Channel{ChannelID: gb10ChannelID},
		&ipc.Channel{ChannelID: "bad\r\nVia: injected"},
	)
	if _, ok := device.Channels.Load(gb10ChannelID); !ok {
		t.Fatal("valid persisted channel was not restored")
	}
	if _, ok := device.Channels.Load("bad\r\nVia: injected"); ok {
		t.Fatal("invalid persisted channel was restored")
	}
}

func TestWrapRequestRejectsIncompleteTarget(t *testing.T) {
	local, err := sip.ParseSipURI("sip:34020000002000000001@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.DefaultConfig().Sip
	api := &GB28181API{cfg: &cfg}
	server := &Server{
		Server:      sip.NewServer(&sip.Address{URI: &local, Params: sip.NewParams()}),
		gb:          api,
		fromAddress: sip.Address{URI: &local, Params: sip.NewParams()},
	}
	api.svr = server
	t.Cleanup(server.Close)

	tests := []struct {
		name   string
		target *Channel
		want   string
	}{
		{name: "missing URI", target: &Channel{}, want: "target URI"},
		{name: "missing connection", target: validRequestTarget(t, nil), want: "connection"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := server.wrapRequest(test.target, sip.MethodMessage, &sip.ContentTypeXML, nil); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("incomplete target error = %v", err)
			}
		})
	}
}

func TestWrapRequestRejectsStoppedService(t *testing.T) {
	local, err := sip.ParseSipURI("sip:34020000002000000001@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	api := &GB28181API{lifecycleDone: make(chan struct{})}
	server := &Server{
		Server:      sip.NewServer(&sip.Address{URI: &local, Params: sip.NewParams()}),
		gb:          api,
		fromAddress: sip.Address{URI: &local, Params: sip.NewParams()},
	}
	api.svr = server
	t.Cleanup(server.Close)
	api.close()

	if _, err = server.wrapRequest(&Channel{}, sip.MethodMessage, &sip.ContentTypeXML, nil); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("stopped service error = %v; want %v", err, ErrServiceStopped)
	}
}

func TestPrepareDialogRequestTransportUsesCurrentTCPConnection(t *testing.T) {
	oldBase := newFlowConnection()
	oldConnection := &tcpFlowConnection{flowConnection: oldBase}
	currentBase := newFlowConnection()
	currentConnection := &tcpFlowConnection{flowConnection: currentBase}
	target := validRequestTarget(t, currentConnection)
	target.device.UpdateRuntime(func(device *Device) {
		device.conn = currentConnection
		device.source = currentBase.remote
	})
	requestURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest("", sip.MethodBYE, &requestURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodBYE).
			AddVia(&sip.ViaHop{Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetConnection(oldConnection)
	request.SetDestination(oldBase.remote)

	if err := prepareDialogRequestTransport(request, target); err != nil {
		t.Fatal(err)
	}
	if request.GetConnection() != currentConnection {
		t.Fatal("dialog request retained stale TCP connection")
	}
	if request.Destination() != oldBase.remote {
		t.Fatal("dialog request route destination was replaced by registration fallback")
	}
}

func TestDialogCleanupRequestAllowedDuringShutdown(t *testing.T) {
	local, err := sip.ParseSipURI("sip:34020000002000000001@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.DefaultConfig().Sip
	api := &GB28181API{cfg: &cfg, lifecycleDone: make(chan struct{})}
	server := &Server{
		Server:      sip.NewServer(&sip.Address{URI: &local, Params: sip.NewParams()}),
		gb:          api,
		fromAddress: sip.Address{URI: &local, Params: sip.NewParams()},
	}
	api.svr = server
	t.Cleanup(func() {
		api.close()
		server.Close()
	})
	baseConn := newFlowConnection()
	conn := &tcpFlowConnection{flowConnection: baseConn}
	target := validRequestTarget(t, conn)
	requestURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	newBYE := func() *sip.Request {
		callID := sip.CallID("shutdown-cleanup-dialog")
		request := sip.NewRequest("", sip.MethodBYE, &requestURI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(&server.fromAddress).SetTo(target.To()).SetMethod(sip.MethodBYE).SetCallID(&callID).
				AddVia(&sip.ViaHop{Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
		request.SetConnection(conn)
		request.SetDestination(baseConn.remote)
		return request
	}
	api.beginClose()
	if _, err := server.requestDialogContext(context.Background(), target, newBYE()); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("ordinary shutdown dialog request error = %v; want %v", err, ErrServiceStopped)
	}
	if _, err := server.requestDialogCleanupContext(api.mediaPersistenceContext(), target, newBYE()); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-baseConn.writes:
		if !strings.HasPrefix(string(payload), "BYE ") {
			t.Fatalf("cleanup request = %s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown cleanup BYE was not sent")
	}
}

func TestRequestFromResponseLocalFailureDoesNotConsumeDialogCSeq(t *testing.T) {
	newFixture := func(t *testing.T, conn sip.Connection) (*Server, *Channel, *sip.Response) {
		t.Helper()
		localURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@127.0.0.1:5060")
		if err != nil {
			t.Fatal(err)
		}
		remoteURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.10:5060")
		if err != nil {
			t.Fatal(err)
		}
		local := sip.Address{URI: &localURI, Params: sip.NewParams()}
		remote := sip.Address{URI: &remoteURI, Params: sip.NewParams()}
		cfg := conf.DefaultConfig().Sip
		api := &GB28181API{cfg: &cfg}
		sipServer := sip.NewServer(&local)
		server := &Server{Server: sipServer, gb: api, fromAddress: local}
		api.svr = server
		t.Cleanup(server.Close)

		target := validRequestTarget(t, conn)
		if conn != nil {
			target.device.source = conn.RemoteAddr()
		}
		callID := sip.CallID("request-from-response-local-failure")
		invite := sip.NewRequest("", sip.MethodInvite, &remoteURI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(&local).SetTo(&remote).SetMethod(sip.MethodInvite).SetSeqNo(17).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "127.0.0.1", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
		invite.SetSource(&net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5060})
		invite.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060})
		invite.SetConnection(conn)
		response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
		response.SetConnection(conn)
		return server, target, response
	}
	assertNextCSeq := func(t *testing.T, response *sip.Response) {
		t.Helper()
		request, err := sip.NewRequestFromResponseChecked(sip.MethodInfo, response)
		if err != nil {
			t.Fatal(err)
		}
		cseq, _ := request.CSeq()
		if cseq == nil || cseq.SeqNo != 18 {
			t.Fatalf("dialog CSeq after local failure = %+v, want 18", cseq)
		}
	}

	t.Run("missing transport", func(t *testing.T) {
		server, target, response := newFixture(t, nil)
		if _, err := server.requestFromResponseContext(t.Context(), target, sip.MethodInfo, response); err == nil || !strings.Contains(err.Error(), "connection") {
			t.Fatalf("missing transport error = %v", err)
		}
		assertNextCSeq(t, response)
	})

	t.Run("canceled context", func(t *testing.T) {
		conn := newFlowConnection()
		server, target, response := newFixture(t, conn)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := server.requestFromResponseContext(ctx, target, sip.MethodInfo, response); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request error = %v", err)
		}
		assertNextCSeq(t, response)
	})

	t.Run("identity forwarding", func(t *testing.T) {
		conn := newFlowConnection()
		server, target, response := newFixture(t, conn)
		ctx := withMonitorUserIdentity(t.Context(), &monitorUserIdentity{Gateways: []string{testTrustedGatewayID}, UserID: testRemoteUserID})
		if _, err := server.requestFromResponseContext(ctx, target, sip.MethodInfo, response); err == nil || !strings.Contains(err.Error(), "forwarding gateway") {
			t.Fatalf("identity forwarding error = %v", err)
		}
		assertNextCSeq(t, response)
		select {
		case payload := <-conn.writes:
			t.Fatalf("identity failure wrote request: %s", payload)
		default:
		}
	})

	t.Run("request preparation", func(t *testing.T) {
		conn := newFlowConnection()
		server, target, response := newFixture(t, conn)
		prepareErr := errors.New("prepare dialog payload")
		if _, err := server.requestFromResponsePreparedContext(t.Context(), target, sip.MethodInfo, response, func(*sip.Request) error {
			return prepareErr
		}); !errors.Is(err, prepareErr) {
			t.Fatalf("request preparation error = %v", err)
		}
		assertNextCSeq(t, response)
	})

	t.Run("signal digest configuration", func(t *testing.T) {
		conn := newFlowConnection()
		server, target, response := newFixture(t, conn)
		server.gb.cfg.SignalDigest.Enabled = true
		server.gb.cfg.SignalDigest.Seed = "digest-seed"
		server.gb.cfg.SignalDigest.Algorithm = "unsupported"
		if _, err := server.requestFromResponseContext(t.Context(), target, sip.MethodInfo, response); err == nil || !strings.Contains(err.Error(), "algorithm") {
			t.Fatalf("signal digest preparation error = %v", err)
		}
		assertNextCSeq(t, response)
	})

	t.Run("successful write commits candidate", func(t *testing.T) {
		baseConn := newFlowConnection()
		conn := &tcpFlowConnection{flowConnection: baseConn}
		server, target, response := newFixture(t, conn)
		tx, err := server.requestFromResponseContext(t.Context(), target, sip.MethodInfo, response)
		if err != nil {
			t.Fatal(err)
		}
		tx.Close()
		select {
		case payload := <-baseConn.writes:
			if !strings.Contains(string(payload), "CSeq: 18 INFO") {
				t.Fatalf("written dialog request = %s", payload)
			}
		case <-time.After(time.Second):
			t.Fatal("prepared dialog request was not written")
		}
		next, err := sip.NewRequestFromResponseChecked(sip.MethodInfo, response)
		if err != nil {
			t.Fatal(err)
		}
		cseq, _ := next.CSeq()
		if cseq == nil || cseq.SeqNo != 19 {
			t.Fatalf("dialog CSeq after successful write = %+v, want 19", cseq)
		}
	})
}

func TestConsumeDialogResponseAsyncClosesTransactionAfterFinalResponse(t *testing.T) {
	localURI, err := sip.ParseSipURI("sip:34020000002000000001@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.DefaultConfig().Sip
	api := &GB28181API{cfg: &cfg, lifecycleDone: make(chan struct{})}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	server := &Server{Server: sipServer, gb: api, fromAddress: sip.Address{URI: &localURI, Params: sip.NewParams()}}
	api.svr = server
	if err := sipServer.StartUDPServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	remote, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = remote.Close()
		server.Close()
	})

	target := validRequestTarget(t, sipServer.UDPConn())
	target.device.source = remote.LocalAddr()
	requestURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@127.0.0.1:5060")
	if err != nil {
		t.Fatal(err)
	}
	callID := sip.CallID("async-dialog-final-response")
	request := sip.NewRequest("", sip.MethodBYE, &requestURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(&server.fromAddress).SetTo(target.To()).SetMethod(sip.MethodBYE).SetCallID(&callID).
			AddVia(&sip.ViaHop{Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	request.SetConnection(sipServer.UDPConn())
	request.SetDestination(remote.LocalAddr())

	tx, err := server.requestDialogContext(t.Context(), target, request)
	if err != nil {
		t.Fatal(err)
	}
	api.consumeDialogResponseAsync(tx)
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	serverAddress, ok := sipServer.UDPConn().LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("SIP UDP address = %T", sipServer.UDPConn().LocalAddr())
	}
	if _, err := remote.WriteToUDP([]byte(response.String()), serverAddress); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		replacement, err := sipServer.Request(request)
		if err != nil {
			t.Fatal(err)
		}
		if replacement != tx {
			replacement.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fire-and-forget dialog transaction remained registered after its final response")
}

func TestDeviceTCPRequestsRespectWriteContext(t *testing.T) {
	tests := []struct {
		name string
		send func(context.Context, *Server, Targeter, sip.Connection) error
	}{
		{
			name: "initial request",
			send: func(ctx context.Context, server *Server, target Targeter, _ sip.Connection) error {
				_, err := server.wrapRequestContext(ctx, target, sip.MethodMessage, &sip.ContentTypeXML, []byte("<Query/>"))
				return err
			},
		},
		{
			name: "dialog request",
			send: func(ctx context.Context, server *Server, target Targeter, conn sip.Connection) error {
				requestURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.10:5060")
				if err != nil {
					return err
				}
				callID := sip.CallID("blocked-dialog-write")
				request := sip.NewRequest("", sip.MethodBYE, &requestURI, sip.DefaultSipVersion,
					sip.NewHeaderBuilder().SetFrom(&server.fromAddress).SetTo(target.To()).SetMethod(sip.MethodBYE).SetCallID(&callID).
						AddVia(&sip.ViaHop{Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
				request.SetConnection(conn)
				request.SetDestination(conn.RemoteAddr())
				_, err = server.requestDialogContext(ctx, target, request)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			localURI, err := sip.ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
			if err != nil {
				t.Fatal(err)
			}
			cfg := conf.DefaultConfig().Sip
			api := &GB28181API{cfg: &cfg}
			server := &Server{
				Server:      sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()}),
				gb:          api,
				fromAddress: sip.Address{URI: &localURI, Params: sip.NewParams()},
			}
			api.svr = server
			localRaw, remoteRaw := net.Pipe()
			conn := sip.NewTCPConnection(&cascadeTestTCPConn{
				Conn:   localRaw,
				local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41500},
				remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060},
			})
			t.Cleanup(func() {
				_ = remoteRaw.Close()
				_ = conn.Close()
				server.Close()
			})
			target := validRequestTarget(t, conn)
			target.device.source = conn.RemoteAddr()

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			err = test.send(ctx, server, target, conn)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("blocked TCP write error = %v; want %v", err, context.DeadlineExceeded)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("blocked TCP write took %s", elapsed)
			}
		})
	}
}

func TestDeviceOversizedUDPMessageUsesRequestScopedTCP(t *testing.T) {
	testDeviceOversizedUDPRequestUsesRequestScopedTCP(t, sip.MethodMessage, "")
}

func TestDeviceOversizedUDPNotifyUsesRequestScopedTCP(t *testing.T) {
	testDeviceOversizedUDPRequestUsesRequestScopedTCP(t, sip.MethodNotify, "")
}

func TestDeviceOversizedUDPRoutedNotifyUsesRequestScopedTCP(t *testing.T) {
	testDeviceOversizedUDPRequestUsesRequestScopedTCP(t, sip.MethodNotify, "loose")
}

func TestDeviceOversizedUDPStrictRoutedNotifyUsesRequestScopedTCP(t *testing.T) {
	testDeviceOversizedUDPRequestUsesRequestScopedTCP(t, sip.MethodNotify, "strict")
}

func testDeviceOversizedUDPRequestUsesRequestScopedTCP(t *testing.T, method, routeMode string) {
	t.Helper()
	localURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	cfg := conf.DefaultConfig().Sip
	api := &GB28181API{cfg: &cfg}
	server := &Server{
		Server:      sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()}),
		gb:          api,
		fromAddress: sip.Address{URI: &localURI, Params: sip.NewParams()},
	}
	api.svr = server
	udp := newFlowConnection()
	target := validRequestTarget(t, udp)
	target.device.UpdateRuntime(func(device *Device) {
		device.conn = udp
		device.source = udp.remote
	})
	destination := net.Addr(udp.remote)
	var opts []RequestOption
	if routeMode == "loose" {
		routeURI, parseErr := sip.ParseSipURI("sip:proxy@192.0.2.30:5080;lr;transport=udp")
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		destination = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5080}
		opts = append(opts, func(request *sip.Request) {
			request.AppendHeader(&sip.RouteHeader{Addresses: []*sip.URI{&routeURI}})
			request.SetDestination(destination)
		})
	} else if routeMode == "strict" {
		strictURI, parseErr := sip.ParseSipURI("sip:strict-proxy@192.0.2.30:5080;transport=udp")
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		looseURI, parseErr := sip.ParseSipURI("sip:loose-proxy@192.0.2.31:5090;lr;transport=udp")
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		contactURI, parseErr := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.10:5060;transport=udp")
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		destination = &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5080}
		subscribe := newFlowRequest(t, udp, sip.MethodSubscribe, "strict-route-device-notify", []byte("query"))
		subscribe.SetSource(destination)
		subscribe.AppendHeader(&sip.ContactHeader{Address: &contactURI, Params: sip.NewParams()})
		subscribe.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{&strictURI, &looseURI}})
		response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
		dialogNotify, dialogErr := sip.NewRequestFromServerDialogChecked(sip.MethodNotify, subscribe, response, 2)
		if dialogErr != nil {
			t.Fatal(dialogErr)
		}
		opts = append(opts, func(request *sip.Request) {
			applyServerSubscriptionDialog(request, dialogNotify)
		})
	}

	received := make(chan string, 1)
	peerErr := make(chan error, 1)
	server.dialDeviceTCP = func(_ context.Context, address string) (net.Conn, error) {
		if address != destination.String() {
			return nil, fmt.Errorf("device TCP target = %q", address)
		}
		client, remote := net.Pipe()
		go func() {
			defer remote.Close()
			message, readErr := readCascadeTestTCPMessage(bufio.NewReader(remote))
			if readErr != nil {
				peerErr <- readErr
				return
			}
			received <- message
			if deadlineErr := remote.SetReadDeadline(time.Now().Add(time.Second)); deadlineErr != nil {
				peerErr <- deadlineErr
				return
			}
			if _, writeErr := io.WriteString(remote, cascadeTestTCPResponse(message, http.StatusOK, "OK", "")); writeErr != nil {
				peerErr <- writeErr
				return
			}
			var probe [1]byte
			if _, readErr = remote.Read(probe[:]); !errors.Is(readErr, io.EOF) {
				peerErr <- fmt.Errorf("request-scoped TCP connection was not closed after final response: %w", readErr)
				return
			}
			peerErr <- nil
		}()
		return &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 42001},
			remote: &net.TCPAddr{IP: append(net.IP(nil), destination.(*net.UDPAddr).IP...), Port: destination.(*net.UDPAddr).Port},
		}, nil
	}
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	tx, err := server.wrapRequestContext(ctx, target, method, &sip.ContentTypeXML, []byte(strings.Repeat("x", 1301)), opts...)
	if err != nil {
		t.Fatalf("send oversized device request: %v", err)
	}
	response, err := sipResponseContext(ctx, tx)
	if err != nil {
		t.Fatalf("wait oversized device response: %v", err)
	}
	if response.StatusCode() != http.StatusOK {
		t.Fatalf("device response = %d", response.StatusCode())
	}
	message := <-received
	if !strings.Contains(message, "Via: SIP/2.0/TCP") {
		t.Fatalf("oversized device request Via = %s", message)
	}
	if contact := cascadeTestHeader(message, "Contact"); !strings.Contains(contact, "transport=tcp") {
		t.Fatalf("oversized device request Contact = %q", contact)
	}
	startLine := strings.SplitN(message, "\r\n", 2)[0]
	if routeMode == "loose" {
		if strings.Contains(startLine, "transport=tcp") {
			t.Fatalf("routed oversized device request changed remote target URI = %q", startLine)
		}
		if route := cascadeTestHeader(message, "Route"); !strings.Contains(route, "lr") || !strings.Contains(route, "transport=tcp") {
			t.Fatalf("routed oversized device request Route = %q", route)
		}
	} else if routeMode == "strict" {
		if !strings.Contains(startLine, "strict-proxy@192.0.2.30:5080;transport=tcp") {
			t.Fatalf("strict-routed oversized device request URI = %q", startLine)
		}
		if route := cascadeTestHeader(message, "Route"); !strings.Contains(route, "loose-proxy@192.0.2.31:5090;lr;transport=udp") || strings.Contains(route, "loose-proxy@192.0.2.31:5090;lr;transport=tcp") {
			t.Fatalf("strict-routed oversized device request Route = %q", route)
		}
	} else if !strings.Contains(startLine, "transport=tcp") {
		t.Fatalf("oversized device request URI = %q", startLine)
	}
	select {
	case payload := <-udp.writes:
		t.Fatalf("oversized request used registered UDP connection: %s", payload)
	default:
	}
	if err := <-peerErr; err != nil {
		t.Fatal(err)
	}
}

func validRequestTarget(t *testing.T, conn sip.Connection) *Channel {
	t.Helper()
	channel := &Channel{ChannelID: "34020000001320000001", device: &Device{conn: conn}}
	if err := channel.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	return channel
}
