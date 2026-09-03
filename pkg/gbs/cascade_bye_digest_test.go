package gbs

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestInboundCascadeBYERetriesDigestChallengeFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, challenge := range cascadeMessageDigestChallenges() {
			t.Run(string(version)+"/"+challenge.name, func(t *testing.T) {
				repeatChallenge := version == GBVersion30 && challenge.status == http.StatusUnauthorized
				platform := testCascadeTCPPlatform(t, string(version))
				platform.password = "cascade-secret"
				platform.signalDigestSeed = "async-bye-signal-seed"
				local := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.20:5060")
				remote := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.30:5060")
				remote.Params.Add("tag", sip.String{Str: "async-bye-remote"})
				sipServer := sip.NewServer(local)
				api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
					Enabled: true, Seed: "global-signal-seed", Algorithm: "MD5", Encoding: "base64",
					Window: conf.Duration(10 * time.Minute),
				}}}
				server := &Server{Server: sipServer, gb: api, fromAddress: *local}
				api.svr = server
				worker := newCascadeWorker(server, platform)
				worker.mu.Lock()
				worker.effective = version
				worker.mu.Unlock()

				client, registrar := net.Pipe()
				clientConn := &cascadeTestTCPConn{
					Conn:   client,
					local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41200},
					remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
				}
				worker.dialTCP = func(context.Context, string) (net.Conn, error) { return clientConn, nil }
				t.Cleanup(func() {
					worker.cancel()
					worker.closeTCPConnection()
					_ = registrar.Close()
					sipServer.Close()
				})

				var identityCtx context.Context
				if version == GBVersion30 {
					policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
					if err != nil {
						t.Fatal(err)
					}
					worker.platform.monitorUserIdentity = policy
					identity, err := parseMonitorUserIdentity(strings.Join([]string{
						testRemoteGatewayID, testTrustedGatewayID, testRemoteUserID, "remoteorg", "dispatcher", "level2",
					}, "-"))
					if err != nil {
						t.Fatal(err)
					}
					identityCtx = withMonitorUserIdentity(context.Background(), identity)
				}

				callID := sip.CallID("async-cascade-bye-digest-" + string(version) + "-" + challenge.name)
				invite := sip.NewRequest("", sip.MethodInvite, local.URI, sip.DefaultSipVersion,
					sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(remote).SetMethod(sip.MethodInvite).
						SetSeqNo(23).SetCallID(&callID).
						AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
				invite.SetSource(clientConn.remote)
				invite.SetDestination(clientConn.local)
				response := sip.NewResponseFromRequest("", invite, http.StatusOK, "OK", nil)
				dialog := &inboundInviteDialog{
					CallID: callIDFromRequest(invite), DeviceID: gb10PlatformID, Established: true, LocalCSeq: 23,
					Request: invite, Response: response,
					Cascade: &cascadeMediaSession{worker: worker, identityCtx: identityCtx},
				}

				firstCh := make(chan string, 1)
				secondCh := make(chan string, 1)
				registrarErr := make(chan error, 1)
				releaseChallenge := make(chan struct{})
				go func() {
					reader := bufio.NewReader(registrar)
					first, err := readCascadeTestTCPMessage(reader)
					if err != nil {
						registrarErr <- err
						return
					}
					firstCh <- first
					<-releaseChallenge
					extra := fmt.Sprintf("%s: Digest realm=\"%s\",nonce=\"%s\",algorithm=MD5,qop=\"auth\"",
						challenge.challengeHeader, challenge.realm, challenge.nonce)
					if _, err := io.WriteString(registrar, cascadeTestTCPResponse(first, challenge.status, challenge.reason, extra)); err != nil {
						registrarErr <- err
						return
					}
					second, err := readCascadeTestTCPMessage(reader)
					if err != nil {
						registrarErr <- err
						return
					}
					secondCh <- second
					finalStatus, finalReason, finalExtra := http.StatusOK, "OK", ""
					if repeatChallenge {
						finalStatus, finalReason = challenge.status, challenge.reason
						finalExtra = fmt.Sprintf("%s: Digest realm=\"%s\",nonce=\"%s-repeated\",algorithm=MD5,qop=\"auth\"",
							challenge.challengeHeader, challenge.realm, challenge.nonce)
					}
					if _, err := io.WriteString(registrar, cascadeTestTCPResponse(second, finalStatus, finalReason, finalExtra)); err != nil {
						registrarErr <- err
						return
					}
					if repeatChallenge {
						if err := registrar.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
							registrarErr <- err
							return
						}
						third, err := readCascadeTestTCPMessage(reader)
						if err == nil {
							registrarErr <- fmt.Errorf("repeated Digest challenge produced a third BYE: %s", third)
							return
						}
						if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
							registrarErr <- fmt.Errorf("wait for unexpected third BYE: %w", err)
							return
						}
					}
					registrarErr <- nil
				}()

				byeResult := make(chan error, 1)
				go func() { byeResult <- api.sendInboundDialogBYEContext(t.Context(), dialog) }()
				var first string
				select {
				case first = <-firstCh:
				case err := <-registrarErr:
					t.Fatal(err)
				case <-time.After(time.Second):
					t.Fatal("initial cascade BYE write timeout")
				}
				select {
				case err := <-byeResult:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("cascade BYE waited for the Digest challenge")
				}

				// 热更新会先停止业务任务；已经写出的清理 BYE 仍需完成一次认证应答。
				worker.stopOperations()
				close(releaseChallenge)
				var second string
				select {
				case second = <-secondCh:
				case err := <-registrarErr:
					if err == nil {
						t.Fatal("authenticated cascade BYE was not sent")
					}
					t.Fatal(err)
				case <-time.After(time.Second):
					t.Fatal("authenticated cascade BYE timeout")
				}
				assertAsyncCascadeBYEDigestRetry(t, first, second, challenge, version, platform.signalDigestSeed)
				if err := <-registrarErr; err != nil {
					t.Fatal(err)
				}
				if dialog.LocalCSeq != 25 {
					t.Fatalf("authenticated cascade BYE dialog CSeq = %d, want 25", dialog.LocalCSeq)
				}
			})
		}
	}
}

func TestCascadeWorkerStopCancelsAndWaitsResponseTasks(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	started := make(chan struct{})
	finished := make(chan struct{})
	if !worker.startResponseTask(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(finished)
	}) {
		t.Fatal("response task did not start")
	}
	<-started

	// 本测试未启动注册循环，预先完成 run 的退出信号以单独验证响应任务生命周期。
	close(worker.done)
	stopDone := make(chan struct{})
	go func() {
		worker.stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("worker stop did not cancel the pending response task")
	}
	select {
	case <-finished:
	default:
		t.Fatal("worker stop returned before the response task exited")
	}
	if worker.startResponseTask(func(context.Context) {}) {
		t.Fatal("stopped worker accepted another response task")
	}
}

func assertAsyncCascadeBYEDigestRetry(
	t *testing.T,
	first, second string,
	challenge cascadeMessageDigestChallenge,
	version GBProtocolVersion,
	signalSeed string,
) {
	t.Helper()
	if !strings.HasPrefix(first, "BYE ") || !strings.HasPrefix(second, "BYE ") {
		t.Fatalf("cascade BYE methods = %q / %q", strings.Fields(first), strings.Fields(second))
	}
	if got := cascadeTestHeader(first, "X-GB-Ver"); got != string(version) {
		t.Fatalf("initial cascade BYE X-GB-Ver = %q, want %q", got, version)
	}
	if got := cascadeTestHeader(second, "X-GB-Ver"); got != string(version) {
		t.Fatalf("authenticated cascade BYE X-GB-Ver = %q, want %q", got, version)
	}
	for _, name := range []string{"Call-ID", "From", "To", "Contact"} {
		if firstValue, secondValue := cascadeTestHeader(first, name), cascadeTestHeader(second, name); firstValue == "" || firstValue != secondValue {
			t.Fatalf("cascade BYE retry %s = %q / %q", name, firstValue, secondValue)
		}
	}
	if version == GBVersion30 {
		firstIdentity := cascadeTestHeader(first, monitorUserIdentityHeaderName)
		secondIdentity := cascadeTestHeader(second, monitorUserIdentityHeaderName)
		if firstIdentity == "" || firstIdentity != secondIdentity {
			t.Fatalf("cascade BYE retry Monitor-User-Identity = %q / %q", firstIdentity, secondIdentity)
		}
	}
	if cascadeTestHeader(first, "Authorization") != "" || cascadeTestHeader(first, "Proxy-Authorization") != "" {
		t.Fatalf("initial cascade BYE unexpectedly carried credentials: %s", first)
	}
	authorizationValue := cascadeTestHeader(second, challenge.authorizeHeader)
	otherHeader := "Authorization"
	if challenge.authorizeHeader == otherHeader {
		otherHeader = "Proxy-Authorization"
	}
	if authorizationValue == "" || cascadeTestHeader(second, otherHeader) != "" {
		t.Fatalf("authenticated cascade BYE credentials = %q / %q", authorizationValue, cascadeTestHeader(second, otherHeader))
	}
	auth := sip.AuthFromValue(authorizationValue)
	requestLine := strings.Fields(second)
	if len(requestLine) < 2 {
		t.Fatalf("invalid authenticated cascade BYE request line: %q", second)
	}
	expected := sip.CalcResponse(
		gb10DeviceID, challenge.realm, "cascade-secret", sip.MethodBYE, requestLine[1], challenge.nonce,
		"auth", auth.Get("cnonce"), "00000001",
	)
	if auth.Get("username") != gb10DeviceID || auth.Get("response") != expected || auth.Get("nc") != "00000001" {
		t.Fatalf("authenticated cascade BYE credentials = %s", authorizationValue)
	}
	firstCSeq := strings.Fields(cascadeTestHeader(first, "CSeq"))
	secondCSeq := strings.Fields(cascadeTestHeader(second, "CSeq"))
	if len(firstCSeq) != 2 || len(secondCSeq) != 2 {
		t.Fatalf("cascade BYE CSeq = %v / %v", firstCSeq, secondCSeq)
	}
	firstSeq, firstErr := strconv.ParseUint(firstCSeq[0], 10, 32)
	secondSeq, secondErr := strconv.ParseUint(secondCSeq[0], 10, 32)
	if firstErr != nil || secondErr != nil || secondSeq != firstSeq+1 || firstCSeq[1] != sip.MethodBYE || secondCSeq[1] != sip.MethodBYE {
		t.Fatalf("cascade BYE CSeq = %v / %v", firstCSeq, secondCSeq)
	}
	firstBranch := cascadeTestViaBranch(cascadeTestHeader(first, "Via"))
	secondBranch := cascadeTestViaBranch(cascadeTestHeader(second, "Via"))
	if firstBranch == "" || secondBranch == "" || firstBranch == secondBranch {
		t.Fatalf("cascade BYE Via branches = %q / %q", firstBranch, secondBranch)
	}
	if err := verifyCascadeTestSignalDigest(first, signalSeed); err != nil {
		t.Fatal(err)
	}
	if err := verifyCascadeTestSignalDigest(second, signalSeed); err != nil {
		t.Fatal(err)
	}
}

func cascadeTestViaBranch(value string) string {
	for parameter := range strings.SplitSeq(value, ";") {
		name, branch, ok := strings.Cut(strings.TrimSpace(parameter), "=")
		if ok && strings.EqualFold(name, "branch") {
			return strings.TrimSpace(branch)
		}
	}
	return ""
}
