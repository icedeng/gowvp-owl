package gbs

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func testCascadePlatform(t *testing.T, version string) cascadePlatform {
	t.Helper()
	platform, err := normalizeCascadePlatform(conf.SIPUpstream{
		Name: "provincial", Enabled: true,
		ServerID: gb10PlatformID, Host: "192.0.2.30", Port: 5060,
		Domain: "remote.example", LocalID: gb10DeviceID, LocalDomain: "local.example",
		LocalHost: "192.0.2.20", Password: "cascade-secret",
		Version: version, Expires: 3600, KeepaliveInterval: conf.Duration(30 * time.Second),
	}, conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}, "")
	if err != nil {
		t.Fatal(err)
	}
	return platform
}

func testCascadeTCPPlatform(t *testing.T, version string) cascadePlatform {
	t.Helper()
	platform, err := normalizeCascadePlatform(conf.SIPUpstream{
		Name: "provincial-tcp", Enabled: true,
		ServerID: gb10PlatformID, Host: "192.0.2.30", Port: 5060, Transport: "tcp",
		Domain: "remote.example", LocalID: gb10DeviceID, LocalDomain: "local.example",
		LocalHost: "192.0.2.20", Password: "cascade-secret",
		Version: version, Expires: 3600, KeepaliveInterval: conf.Duration(30 * time.Second),
	}, conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}, "")
	if err != nil {
		t.Fatal(err)
	}
	return platform
}

func testCascadeTLSPlatform(t *testing.T, version string) cascadePlatform {
	t.Helper()
	platform, err := normalizeCascadePlatform(conf.SIPUpstream{
		Name: "provincial-tls", Enabled: true,
		ServerID: gb10PlatformID, Host: "192.0.2.30", Transport: "tls",
		Domain: "remote.example", LocalID: gb10DeviceID, LocalDomain: "local.example",
		LocalHost: "192.0.2.20", Password: "cascade-secret", TLSServerName: "sip.example.com",
		Version: version, Expires: 3600, KeepaliveInterval: conf.Duration(30 * time.Second),
	}, conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060, EnableTLS: true, TLSPort: 5061}, "")
	if err != nil {
		t.Fatal(err)
	}
	return platform
}

func TestCascadeRegisterDigestAndVersionNegotiation(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "2.0"))
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		if len(requests) == 1 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{
				HeaderName: "WWW-Authenticate",
				Contents:   `Digest realm="3402000000",qop="auth",nonce="cascade-nonce",opaque="registrar-token"`,
			})
			version := sip.XGBVer("1.1")
			response.AppendHeader(&version)
			return response, nil
		}
		response := newCascadeRegisterSuccessResponse(t, request, 3600)
		version := sip.XGBVer("1.1")
		response.AppendHeader(&version)
		return response, nil
	}

	if err := worker.register(t.Context(), worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("REGISTER request count = %d", len(requests))
	}
	if headers := requests[1].GetHeaders("X-GB-Ver"); len(headers) != 1 || !strings.Contains(headers[0].String(), "1.1") {
		t.Fatalf("authenticated REGISTER X-GB-Ver = %v; want negotiated 1.1 from 401", headers)
	}
	firstCallID, _ := requests[0].CallID()
	secondCallID, _ := requests[1].CallID()
	if firstCallID == nil || secondCallID == nil || *firstCallID != *secondCallID {
		t.Fatalf("Digest retry changed Call-ID: %v / %v", firstCallID, secondCallID)
	}
	firstCSeq, _ := requests[0].CSeq()
	secondCSeq, _ := requests[1].CSeq()
	if firstCSeq == nil || secondCSeq == nil || firstCSeq.SeqNo != 1 || secondCSeq.SeqNo != 2 {
		t.Fatalf("REGISTER CSeq = %v / %v", firstCSeq, secondCSeq)
	}
	assertCascadeRegisterAddressing(t, requests[0])
	authorizationHeaders := requests[1].GetHeaders("Authorization")
	if len(authorizationHeaders) != 1 {
		t.Fatal("authenticated REGISTER missing Authorization")
	}
	auth := sip.AuthFromValue(authorizationHeaders[0].String())
	expected := sip.CalcResponse(
		gb10DeviceID, "3402000000", "cascade-secret", sip.MethodRegister,
		requests[1].Recipient().String(), "cascade-nonce", "auth", auth.Get("cnonce"), "00000001",
	)
	if auth.Get("username") != gb10DeviceID || auth.Get("response") != expected || auth.Get("nc") != "00000001" || auth.Get("opaque") != "registrar-token" {
		t.Fatalf("unexpected Digest Authorization: %s", authorizationHeaders[0].String())
	}
	if worker.effective != GBVersion11 {
		t.Fatalf("negotiated version = %s; want 1.1", worker.effective)
	}
	status := worker.snapshot()
	if !status.Registered || status.State != "registered" || status.NegotiatedVersion != "1.1" || status.ExpiresAt.IsZero() || worker.accepted != 3600 {
		t.Fatalf("cascade status = %+v", status)
	}
	remaining := time.Until(status.ExpiresAt)
	if remaining < 3595*time.Second || remaining > 3600*time.Second {
		t.Fatalf("cascade accepted expiry remaining = %s", remaining)
	}
}

func TestCascadeRegisterRetriesProxyDigestFourVersionMatrix(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			requests := make([]*sip.Request, 0, 2)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests = append(requests, request)
				if len(requests) == 1 {
					response := sip.NewResponseFromRequest("", request, http.StatusProxyAuthRequired, "Proxy Authentication Required", nil)
					response.AppendHeader(&sip.GenericHeader{
						HeaderName: "Proxy-Authenticate",
						Contents:   `Digest realm="proxy.example",qop="auth",nonce="proxy-nonce",algorithm=MD5`,
					})
					return response, nil
				}
				return newCascadeRegisterSuccessResponse(t, request, 3600), nil
			}

			if err := worker.register(t.Context(), 3600); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatalf("REGISTER request count = %d, want 2", len(requests))
			}
			if got := requests[0].GetHeaders("Proxy-Authorization"); len(got) != 0 {
				t.Fatalf("initial REGISTER Proxy-Authorization = %v", got)
			}
			proxyHeaders := requests[1].GetHeaders("Proxy-Authorization")
			if len(proxyHeaders) != 1 || len(requests[1].GetHeaders("Authorization")) != 0 {
				t.Fatalf("authenticated REGISTER headers = proxy:%v authorization:%v", proxyHeaders, requests[1].GetHeaders("Authorization"))
			}
			auth := sip.AuthFromValue(proxyHeaders[0].String())
			expected := sip.CalcResponse(
				gb10DeviceID, "proxy.example", "cascade-secret", sip.MethodRegister,
				requests[1].Recipient().String(), "proxy-nonce", "auth", auth.Get("cnonce"), "00000001",
			)
			if auth.Get("username") != gb10DeviceID || auth.Get("response") != expected || auth.Get("nc") != "00000001" {
				t.Fatalf("Proxy-Authorization = %s", proxyHeaders[0].String())
			}
			firstCallID, _ := requests[0].CallID()
			secondCallID, _ := requests[1].CallID()
			firstCSeq, _ := requests[0].CSeq()
			secondCSeq, _ := requests[1].CSeq()
			if firstCallID == nil || secondCallID == nil || *firstCallID != *secondCallID ||
				firstCSeq == nil || secondCSeq == nil || firstCSeq.SeqNo+1 != secondCSeq.SeqNo {
				t.Fatalf("REGISTER retry identity = Call-ID %v/%v CSeq %v/%v", firstCallID, secondCallID, firstCSeq, secondCSeq)
			}
		})
	}
}

func TestCascadeRegisterCombinesProxyAndRegistrarDigestFourVersionMatrix(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			requests := make([]*sip.Request, 0, 3)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests = append(requests, request)
				switch len(requests) {
				case 1:
					response := sip.NewResponseFromRequest("", request, http.StatusProxyAuthRequired, "Proxy Authentication Required", nil)
					response.AppendHeader(&sip.GenericHeader{HeaderName: "Proxy-Authenticate", Contents: `Digest realm="proxy.example",nonce="proxy-nonce"`})
					return response, nil
				case 2:
					if len(request.GetHeaders("Proxy-Authorization")) != 1 {
						t.Fatal("proxy-authenticated REGISTER lost Proxy-Authorization")
					}
					response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
					response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",nonce="registrar-nonce"`})
					return response, nil
				default:
					return newCascadeRegisterSuccessResponse(t, request, 3600), nil
				}
			}

			if err := worker.register(t.Context(), 3600); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 3 {
				t.Fatalf("REGISTER request count = %d, want 3", len(requests))
			}
			final := requests[2]
			if len(final.GetHeaders("Proxy-Authorization")) != 1 || len(final.GetHeaders("Authorization")) != 1 {
				t.Fatalf("final REGISTER credentials = proxy:%v registrar:%v", final.GetHeaders("Proxy-Authorization"), final.GetHeaders("Authorization"))
			}
			for index, request := range requests {
				cseq, ok := request.CSeq()
				if !ok || cseq.SeqNo != uint32(index+1) {
					t.Fatalf("REGISTER %d CSeq = %v", index+1, cseq)
				}
				if index > 0 {
					previousCallID, _ := requests[index-1].CallID()
					callID, _ := request.CallID()
					if previousCallID == nil || callID == nil || *previousCallID != *callID {
						t.Fatalf("REGISTER %d changed Call-ID: %v/%v", index+1, previousCallID, callID)
					}
				}
			}
		})
	}
}

func TestCascadeRegisterRejectsAmbiguousDigestChallengeFourVersionMatrix(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			requests := 0
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests++
				response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
				response.AppendHeader(&sip.GenericHeader{
					HeaderName: "WWW-Authenticate",
					Contents:   `Digest realm="3402000000",realm="attacker.example",nonce="registrar-nonce"`,
				})
				return response, nil
			}

			if err := worker.register(t.Context(), 3600); err == nil {
				t.Fatal("ambiguous cascade REGISTER Digest challenge was accepted")
			}
			if requests != 1 {
				t.Fatalf("REGISTER request count = %d, want 1", requests)
			}
		})
	}
}

func TestCascadeRegisterDigestChallengesRetryOnlyOnceFourVersionMatrix(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		for _, challenge := range []struct {
			name   string
			status int
			reason string
			header string
			value  string
		}{
			{name: "registrar", status: http.StatusUnauthorized, reason: "Unauthorized", header: "WWW-Authenticate", value: `Digest realm="3402000000",nonce="registrar-nonce"`},
			{name: "proxy", status: http.StatusProxyAuthRequired, reason: "Proxy Authentication Required", header: "Proxy-Authenticate", value: `Digest realm="proxy.example",nonce="proxy-nonce"`},
		} {
			t.Run(version+"/"+challenge.name, func(t *testing.T) {
				worker := newCascadeWorker(nil, testCascadePlatform(t, version))
				requests := 0
				worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
					requests++
					response := sip.NewResponseFromRequest("", request, challenge.status, challenge.reason, nil)
					response.AppendHeader(&sip.GenericHeader{HeaderName: challenge.header, Contents: challenge.value})
					return response, nil
				}

				if err := worker.register(t.Context(), 3600); err == nil {
					t.Fatal("repeated cascade REGISTER Digest challenge was accepted")
				}
				if requests != 2 {
					t.Fatalf("REGISTER request count = %d, want 2", requests)
				}
			})
		}
	}
}

func TestCascadeKeepaliveRetriesDigestChallengeFourVersionMatrix(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		for _, challenge := range cascadeMessageDigestChallenges() {
			t.Run(version+"/"+challenge.name, func(t *testing.T) {
				worker, requests := newCascadeMessageDigestTestWorker(t, version, challenge, false)
				before := time.Now().Add(-time.Hour)
				worker.updateStatus(func(state *CascadePlatformStatus) {
					state.State = "registered"
					state.Registered = true
					state.LastKeepaliveAt = before
				})

				if err := worker.keepalive(t.Context()); err != nil {
					t.Fatalf("Digest challenged Keepalive failed: %v", err)
				}
				assertCascadeMessageDigestRetry(t, *requests, challenge, version)
				status := worker.snapshot()
				if !status.Registered || status.State != "registered" || !status.LastKeepaliveAt.After(before) {
					t.Fatalf("Keepalive status after authenticated success = %+v", status)
				}
			})
		}
	}
}

func TestCascadeMessageRetriesDigestChallengeFourVersionMatrix(t *testing.T) {
	body := []byte(`<Response><CmdType>Catalog</CmdType><SN>100</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`)
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		for _, challenge := range cascadeMessageDigestChallenges() {
			t.Run(version+"/"+challenge.name, func(t *testing.T) {
				worker, requests := newCascadeMessageDigestTestWorker(t, version, challenge, false)
				if err := worker.sendMessage(t.Context(), body); err != nil {
					t.Fatalf("Digest challenged MESSAGE failed: %v", err)
				}
				assertCascadeMessageDigestRetry(t, *requests, challenge, version)
				if got := string((*requests)[0].Body()); got != string(body) {
					t.Fatalf("MESSAGE body = %q, want %q", got, body)
				}
			})
		}
	}
}

func TestCascadeMessageDigestRetriesOnlyOnceFourVersionMatrix(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		for _, challenge := range cascadeMessageDigestChallenges() {
			t.Run(version+"/"+challenge.name, func(t *testing.T) {
				worker, requests := newCascadeMessageDigestTestWorker(t, version, challenge, true)
				err := worker.sendMessage(t.Context(), []byte(`<Response><CmdType>Catalog</CmdType></Response>`))
				if err == nil || !strings.Contains(err.Error(), strconv.Itoa(challenge.status)) {
					t.Fatalf("repeated MESSAGE Digest challenge error = %v", err)
				}
				if len(*requests) != 2 {
					t.Fatalf("MESSAGE request count = %d, want 2", len(*requests))
				}
			})
		}
	}
}

func TestCascadeKeepaliveDigestFailureDoesNotRefreshStateFourVersionMatrix(t *testing.T) {
	challenge := cascadeMessageDigestChallenges()[0]
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker, requests := newCascadeMessageDigestTestWorker(t, version, challenge, true)
			before := time.Now().Add(-time.Hour)
			worker.updateStatus(func(state *CascadePlatformStatus) {
				state.State = "registered"
				state.Registered = true
				state.LastKeepaliveAt = before
			})

			if err := worker.keepalive(t.Context()); err == nil {
				t.Fatal("repeated Keepalive Digest challenge was accepted")
			}
			if len(*requests) != 2 {
				t.Fatalf("Keepalive request count = %d, want 2", len(*requests))
			}
			status := worker.snapshot()
			if !status.Registered || status.State != "registered" || !status.LastKeepaliveAt.Equal(before) {
				t.Fatalf("failed Keepalive changed status = %+v", status)
			}
		})
	}
}

func TestCascadeMessageDigestRetryReplacesSignalDigestHeaders(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, string(GBVersion30)))
	now := time.Date(2026, 8, 30, 10, 0, 0, 0, time.Local)
	security, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "cascade-message-note-seed", Algorithm: "MD5", Encoding: "base64",
		Now: func() time.Time {
			current := now
			now = now.Add(time.Second)
			return current
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if err := security.Sign(request); err != nil {
			t.Fatal(err)
		}
		cloned, ok := request.Clone().(*sip.Request)
		if !ok || cloned == nil {
			t.Fatal("clone signed cascade MESSAGE failed")
		}
		requests = append(requests, cloned)
		if len(requests) == 1 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{
				HeaderName: "WWW-Authenticate",
				Contents:   `Digest realm="remote.example",nonce="signed-message-nonce",qop="auth"`,
			})
			return response, nil
		}
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}

	if err := worker.sendMessage(t.Context(), []byte(`<Response><CmdType>Catalog</CmdType></Response>`)); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("signed MESSAGE request count = %d, want 2", len(requests))
	}
	firstDate := firstSingleHeaderValue(requests[0], "Date")
	secondDate := firstSingleHeaderValue(requests[1], "Date")
	firstNote := firstSingleHeaderValue(requests[0], "Note")
	secondNote := firstSingleHeaderValue(requests[1], "Note")
	if len(requests[0].GetHeaders("Date")) != 1 || len(requests[1].GetHeaders("Date")) != 1 ||
		len(requests[0].GetHeaders("Note")) != 1 || len(requests[1].GetHeaders("Note")) != 1 ||
		firstDate == "" || secondDate == "" || firstDate == secondDate || firstNote == "" || firstNote == secondNote {
		t.Fatalf("MESSAGE signal Digest retry Date/Note = %q %q / %q %q", firstDate, firstNote, secondDate, secondNote)
	}
}

func TestCascadeMessageRejectsAmbiguousDigestChallengeFourVersionMatrix(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			requests := 0
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests++
				response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
				response.AppendHeader(&sip.GenericHeader{
					HeaderName: "WWW-Authenticate",
					Contents:   `Digest realm="remote.example",realm="attacker.example",nonce="message-nonce"`,
				})
				return response, nil
			}

			if err := worker.sendMessage(t.Context(), []byte(`<Response><CmdType>Catalog</CmdType></Response>`)); err == nil {
				t.Fatal("ambiguous MESSAGE Digest challenge was accepted")
			}
			if requests != 1 {
				t.Fatalf("MESSAGE request count = %d, want 1", requests)
			}
		})
	}
}

type cascadeMessageDigestChallenge struct {
	name            string
	status          int
	reason          string
	challengeHeader string
	authorizeHeader string
	realm           string
	nonce           string
}

func cascadeMessageDigestChallenges() []cascadeMessageDigestChallenge {
	return []cascadeMessageDigestChallenge{
		{
			name: "registrar", status: http.StatusUnauthorized, reason: "Unauthorized",
			challengeHeader: "WWW-Authenticate", authorizeHeader: "Authorization",
			realm: "remote.example", nonce: "message-registrar-nonce",
		},
		{
			name: "proxy", status: http.StatusProxyAuthRequired, reason: "Proxy Authentication Required",
			challengeHeader: "Proxy-Authenticate", authorizeHeader: "Proxy-Authorization",
			realm: "proxy.example", nonce: "message-proxy-nonce",
		},
	}
}

func newCascadeMessageDigestTestWorker(t *testing.T, version string, challenge cascadeMessageDigestChallenge, repeat bool) (*cascadeWorker, *[]*sip.Request) {
	t.Helper()
	platform := testCascadePlatform(t, version)
	policy, err := newMonitorUserIdentityPolicy(testMonitorUserIdentityConfig())
	if err != nil {
		t.Fatal(err)
	}
	platform.monitorUserIdentity = policy
	worker := newCascadeWorker(nil, platform)
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		cloned, ok := request.Clone().(*sip.Request)
		if !ok || cloned == nil {
			t.Fatal("clone cascade MESSAGE request failed")
		}
		requests = append(requests, cloned)
		if len(requests) > 1 && !repeat {
			return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
		}
		response := sip.NewResponseFromRequest("", request, challenge.status, challenge.reason, nil)
		response.AppendHeader(&sip.GenericHeader{
			HeaderName: challenge.challengeHeader,
			Contents:   fmt.Sprintf(`Digest realm="%s",nonce="%s",algorithm=MD5,qop="auth"`, challenge.realm, challenge.nonce),
		})
		return response, nil
	}
	return worker, &requests
}

func assertCascadeMessageDigestRetry(t *testing.T, requests []*sip.Request, challenge cascadeMessageDigestChallenge, version string) {
	t.Helper()
	if len(requests) != 2 {
		t.Fatalf("MESSAGE request count = %d, want 2", len(requests))
	}
	first, second := requests[0], requests[1]
	if len(first.GetHeaders("Authorization")) != 0 || len(first.GetHeaders("Proxy-Authorization")) != 0 {
		t.Fatalf("initial MESSAGE credentials = Authorization:%v Proxy-Authorization:%v", first.GetHeaders("Authorization"), first.GetHeaders("Proxy-Authorization"))
	}
	authHeaders := second.GetHeaders(challenge.authorizeHeader)
	otherHeader := "Authorization"
	if challenge.authorizeHeader == otherHeader {
		otherHeader = "Proxy-Authorization"
	}
	if len(authHeaders) != 1 || len(second.GetHeaders(otherHeader)) != 0 {
		t.Fatalf("authenticated MESSAGE credentials = %s:%v %s:%v", challenge.authorizeHeader, authHeaders, otherHeader, second.GetHeaders(otherHeader))
	}
	auth := sip.AuthFromValue(authHeaders[0].String())
	expected := sip.CalcResponse(
		gb10DeviceID, challenge.realm, "cascade-secret", sip.MethodMessage,
		second.Recipient().String(), challenge.nonce, "auth", auth.Get("cnonce"), "00000001",
	)
	if auth.Get("username") != gb10DeviceID || auth.Get("response") != expected || auth.Get("nc") != "00000001" {
		t.Fatalf("authenticated MESSAGE credentials = %s", authHeaders[0].String())
	}
	firstCallID, _ := first.CallID()
	secondCallID, _ := second.CallID()
	firstCSeq, _ := first.CSeq()
	secondCSeq, _ := second.CSeq()
	if firstCallID == nil || secondCallID == nil || *firstCallID != *secondCallID ||
		firstCSeq == nil || secondCSeq == nil || firstCSeq.MethodName != sip.MethodMessage ||
		secondCSeq.SeqNo != firstCSeq.SeqNo+1 || secondCSeq.MethodName != sip.MethodMessage {
		t.Fatalf("MESSAGE retry identity = Call-ID %v/%v CSeq %v/%v", firstCallID, secondCallID, firstCSeq, secondCSeq)
	}
	firstVia, firstViaOK := first.ViaHop()
	secondVia, secondViaOK := second.ViaHop()
	firstBranch, firstBranchOK := sipParamString(firstVia, "branch")
	secondBranch, secondBranchOK := sipParamString(secondVia, "branch")
	if !firstViaOK || !secondViaOK || !firstBranchOK || !secondBranchOK || firstBranch == secondBranch ||
		firstVia.Host != secondVia.Host || firstVia.Transport != secondVia.Transport {
		t.Fatalf("MESSAGE retry Via = %v/%v branches %q/%q", firstVia, secondVia, firstBranch, secondBranch)
	}
	if first.Recipient().String() != second.Recipient().String() || string(first.Body()) != string(second.Body()) {
		t.Fatal("authenticated MESSAGE changed target URI or XML body")
	}
	if firstSingleHeaderValue(first, "X-GB-Ver") != version || firstSingleHeaderValue(second, "X-GB-Ver") != version {
		t.Fatalf("MESSAGE X-GB-Ver = %q/%q, want %q", firstSingleHeaderValue(first, "X-GB-Ver"), firstSingleHeaderValue(second, "X-GB-Ver"), version)
	}
	firstIdentity := firstSingleHeaderValue(first, monitorUserIdentityHeaderName)
	secondIdentity := firstSingleHeaderValue(second, monitorUserIdentityHeaderName)
	if firstIdentity == "" || firstIdentity != secondIdentity {
		t.Fatalf("MESSAGE Monitor-User-Identity = %q/%q", firstIdentity, secondIdentity)
	}
}

func sipParamString(via *sip.ViaHop, name string) (string, bool) {
	if via == nil || via.Params == nil {
		return "", false
	}
	value, ok := via.Params.Get(name)
	if !ok || value == nil {
		return "", false
	}
	return value.String(), true
}

func TestCascadeRegisterRejectsInvalidVersionOnEveryResponseBranch(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		values  []string
		wantErr string
	}{
		{name: "redirect malformed", status: http.StatusFound, values: []string{"2022"}, wantErr: "invalid X-GB-Ver"},
		{name: "challenge duplicate", status: http.StatusUnauthorized, values: []string{"3.0", "3.0"}, wantErr: "multiple X-GB-Ver"},
		{name: "interval malformed", status: statusIntervalTooBrief, values: []string{"3"}, wantErr: "invalid X-GB-Ver"},
		{name: "success malformed", status: http.StatusOK, values: []string{"3.0.0"}, wantErr: "invalid X-GB-Ver"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, string(GBVersion30)))
			calls := 0
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				calls++
				var response *sip.Response
				if test.status == http.StatusOK {
					response = newCascadeRegisterSuccessResponse(t, request, 3600)
				} else {
					response = sip.NewResponseFromRequest("", request, test.status, http.StatusText(test.status), nil)
				}
				for _, value := range test.values {
					response.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: value})
				}
				return response, nil
			}

			err := worker.register(t.Context(), 3600)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("REGISTER error = %v, want %q", err, test.wantErr)
			}
			if calls != 1 || worker.effective != GBVersion30 || worker.snapshot().Registered {
				t.Fatalf("invalid response changed cascade state: calls=%d effective=%s status=%+v", calls, worker.effective, worker.snapshot())
			}
		})
	}
}

func assertCascadeRegisterAddressing(t *testing.T, request *sip.Request) {
	t.Helper()
	if got := request.Recipient().String(); got != "sip:"+gb10PlatformID+"@remote.example" {
		t.Fatalf("REGISTER Request-URI = %s", got)
	}
	from, ok := request.From()
	if !ok || from == nil || from.Address == nil || from.Address.String() != "sip:"+gb10DeviceID+"@local.example" {
		t.Fatalf("REGISTER From = %v", from)
	}
	to, ok := request.To()
	if !ok || to == nil || to.Address == nil || to.Address.String() != "sip:"+gb10DeviceID+"@local.example" {
		t.Fatalf("REGISTER To = %v", to)
	}
	contact, ok := request.Contact()
	if !ok || contact == nil || contact.Address == nil || contact.Address.String() != "sip:"+gb10DeviceID+"@192.0.2.20:5060" {
		t.Fatalf("REGISTER Contact = %v", contact)
	}
}

func newCascadeRegisterSuccessResponse(t *testing.T, request *sip.Request, expires int) *sip.Response {
	t.Helper()
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	contact, ok := request.Contact()
	if !ok || contact == nil {
		t.Fatal("REGISTER request is missing Contact")
	}
	response.AppendHeader(contact.Clone())
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: strconv.Itoa(expires)})
	response.AppendHeader(&sip.GenericHeader{
		HeaderName: "Date",
		Contents:   sip.FormatGBTime(time.Now(), "2006-01-02T15:04:05.000"),
	})
	return response
}

func TestCascadeRegisterSupportsLegacyDigestWithoutQOP(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "1.0"))
	var authenticated *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		if len(request.GetHeaders("Authorization")) == 0 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",nonce="legacy-nonce"`})
			return response, nil
		}
		authenticated = request
		return newCascadeRegisterSuccessResponse(t, request, 3600), nil
	}
	if err := worker.register(t.Context(), 3600); err != nil {
		t.Fatal(err)
	}
	auth := sip.AuthFromValue(authenticated.GetHeaders("Authorization")[0].String())
	if auth.Get("qop") != "" || auth.Get("cnonce") != "" || auth.Get("nc") != "" {
		t.Fatalf("legacy Digest unexpectedly contains qop fields: %s", authenticated.GetHeaders("Authorization")[0].String())
	}
}

func TestCascadeDigestSupportsSHA256AndRejectsUnsupportedAlgorithm(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	request := worker.newRegisterRequest(3600, nil)
	response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
	response.AppendHeader(&sip.GenericHeader{
		HeaderName: "WWW-Authenticate",
		Contents:   `Digest realm="3402000000",nonce="sha256-nonce",algorithm=SHA-256,qop="auth,auth-int"`,
	})
	auth, err := cascadeDigestAuthorization(response, request, gb10DeviceID, "cascade-secret")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := sip.CalcResponseWithAlgorithm(
		"SHA-256", gb10DeviceID, "3402000000", "cascade-secret", sip.MethodRegister,
		request.Recipient().String(), "sha256-nonce", "auth", auth.Get("cnonce"), "00000001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Algorithm() != "SHA-256" || auth.Get("response") != expected || auth.QOP() != "auth" {
		t.Fatalf("SHA-256 Digest Authorization = %s", auth.String())
	}

	unsupported := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
	unsupported.AppendHeader(&sip.GenericHeader{
		HeaderName: "WWW-Authenticate",
		Contents:   `Digest realm="3402000000",nonce="sm3-nonce",algorithm=SM3,qop="auth"`,
	})
	if _, err := cascadeDigestAuthorization(unsupported, request, gb10DeviceID, "cascade-secret"); err == nil {
		t.Fatal("unsupported Digest algorithm accepted")
	}
}

func TestCascadeRegisterOverTCPReusesConnectionForDigest(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	defer sipServer.Close()
	worker := newCascadeWorker(&Server{Server: sipServer}, platform)
	defer worker.closeTCPConnection()

	registrarErr := make(chan error, 1)
	allowRegistrarClose := make(chan struct{})
	dialCalls := 0
	worker.dialTCP = func(_ context.Context, _ string) (net.Conn, error) {
		dialCalls++
		client, registrar := net.Pipe()
		clientConn := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		go func() {
			defer registrar.Close()
			reader := bufio.NewReader(registrar)
			for index := 0; index < 3; index++ {
				request, readErr := readCascadeTestTCPMessage(reader)
				if readErr != nil {
					registrarErr <- readErr
					return
				}
				if !strings.Contains(request, "Via: SIP/2.0/TCP") || !strings.Contains(request, ";transport=tcp") {
					registrarErr <- fmt.Errorf("TCP REGISTER missing transport markers: %s", request)
					return
				}
				status, reason := http.StatusUnauthorized, "Unauthorized"
				extra := `WWW-Authenticate: Digest realm="3402000000",qop="auth",nonce="tcp-nonce"`
				if index == 1 {
					if !strings.Contains(request, "Authorization: Digest") {
						registrarErr <- fmt.Errorf("authenticated TCP REGISTER missing Authorization")
						return
					}
					status, reason = http.StatusOK, "OK"
					extra = "Expires: 3600\r\nX-GB-Ver: 3.0"
				} else if index == 2 {
					if !strings.HasPrefix(request, "MESSAGE ") || !strings.Contains(request, "<CmdType>Keepalive</CmdType>") {
						registrarErr <- fmt.Errorf("unexpected TCP keepalive request: %s", request)
						return
					}
					status, reason, extra = http.StatusOK, "OK", ""
				}
				if _, writeErr := io.WriteString(registrar, cascadeTestTCPResponse(request, status, reason, extra)); writeErr != nil {
					registrarErr <- writeErr
					return
				}
			}
			registrarErr <- nil
			<-allowRegistrarClose
		}()
		return clientConn, nil
	}

	if err := worker.register(t.Context(), worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	if dialCalls != 1 {
		t.Fatalf("TCP cascade dial calls = %d, want 1", dialCalls)
	}
	if status := worker.snapshot(); !status.Registered || status.Address != "192.0.2.30:5060" || status.NegotiatedVersion != "3.0" {
		t.Fatalf("TCP cascade status = %+v", status)
	}
	manager := NewCascadeManager(nil)
	manager.items[worker.platform.name] = worker
	if matched, ok := manager.matchRegistered(gb10PlatformID, &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}); !ok || matched != worker {
		t.Fatalf("registered TCP upstream source match = %v, %v", matched, ok)
	}
	keepaliveDone := make(chan error, 1)
	go func() { keepaliveDone <- worker.keepalive(t.Context()) }()
	select {
	case err := <-keepaliveDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("TCP keepalive timed out")
	}
	if dialCalls != 1 {
		t.Fatalf("TCP keepalive opened another connection: dials=%d", dialCalls)
	}
	if err := <-registrarErr; err != nil {
		t.Fatal(err)
	}
	close(allowRegistrarClose)
}

func TestCascadeTCPResponseTimeoutInvalidatesConnectionAndRedials(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	defer sipServer.Close()
	worker := newCascadeWorker(&Server{Server: sipServer}, platform)
	defer worker.closeTCPConnection()

	firstClosed := make(chan error, 1)
	secondDone := make(chan error, 1)
	allowSecondClose := make(chan struct{})
	dialCalls := 0
	worker.dialTCP = func(_ context.Context, _ string) (net.Conn, error) {
		dialCalls++
		client, registrar := net.Pipe()
		clientConn := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41100 + dialCalls},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		switch dialCalls {
		case 1:
			go func() {
				defer registrar.Close()
				request, readErr := readCascadeTestTCPMessage(bufio.NewReader(registrar))
				if readErr != nil {
					firstClosed <- fmt.Errorf("read first request: %w", readErr)
					return
				}
				if !strings.HasPrefix(request, "MESSAGE ") {
					firstClosed <- fmt.Errorf("unexpected first request: %s", request)
					return
				}
				buffer := make([]byte, 1)
				_, readErr = registrar.Read(buffer)
				if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, net.ErrClosed) {
					firstClosed <- fmt.Errorf("timed-out TCP connection close: %w", readErr)
					return
				}
				firstClosed <- nil
			}()
		case 2:
			go func() {
				defer registrar.Close()
				request, readErr := readCascadeTestTCPMessage(bufio.NewReader(registrar))
				if readErr != nil {
					secondDone <- readErr
					return
				}
				if _, writeErr := io.WriteString(registrar, cascadeTestTCPResponse(request, http.StatusOK, "OK", "")); writeErr != nil {
					secondDone <- writeErr
					return
				}
				secondDone <- nil
				<-allowSecondClose
			}()
		default:
			_ = registrar.Close()
			_ = client.Close()
			return nil, fmt.Errorf("unexpected TCP redial %d", dialCalls)
		}
		return clientConn, nil
	}

	firstCtx, cancelFirst := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err = worker.keepalive(firstCtx)
	cancelFirst()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Keepalive error = %v, want context deadline exceeded", err)
	}
	worker.connMu.Lock()
	firstConnection, firstRemote := worker.tcpConn, worker.tcpRemote
	worker.connMu.Unlock()
	if firstConnection != nil || firstRemote != "" {
		t.Fatalf("timed-out TCP connection remained cached: connection=%v remote=%q", firstConnection, firstRemote)
	}
	select {
	case closeErr := <-firstClosed:
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed-out TCP connection was not closed")
	}

	secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
	err = worker.keepalive(secondCtx)
	cancelSecond()
	if err != nil {
		t.Fatal(err)
	}
	if dialCalls != 2 {
		t.Fatalf("TCP dial calls = %d, want 2", dialCalls)
	}
	worker.connMu.Lock()
	secondConnection := worker.tcpConn
	worker.connMu.Unlock()
	if secondConnection == nil {
		t.Fatal("redialed TCP connection was not cached")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	close(allowSecondClose)
}

func TestCascadeTCPWriteTimeoutInvalidatesConnectionAndRedials(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	defer sipServer.Close()
	worker := newCascadeWorker(&Server{Server: sipServer}, platform)
	defer worker.closeTCPConnection()

	dialCalls := 0
	var peers []net.Conn
	secondDone := make(chan error, 1)
	allowSecondClose := make(chan struct{})
	defer close(allowSecondClose)
	defer func() {
		for _, peer := range peers {
			_ = peer.Close()
		}
	}()
	worker.dialTCP = func(_ context.Context, _ string) (net.Conn, error) {
		dialCalls++
		client, registrar := net.Pipe()
		peers = append(peers, registrar)
		clientConn := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41500 + dialCalls},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		if dialCalls == 2 {
			go func() {
				request, readErr := readCascadeTestTCPMessage(bufio.NewReader(registrar))
				if readErr != nil {
					secondDone <- readErr
					return
				}
				_, writeErr := io.WriteString(registrar, cascadeTestTCPResponse(request, http.StatusOK, "OK", ""))
				secondDone <- writeErr
				<-allowSecondClose
			}()
		} else if dialCalls != 1 {
			return nil, fmt.Errorf("unexpected TCP redial %d", dialCalls)
		}
		return clientConn, nil
	}

	firstCtx, cancelFirst := context.WithTimeout(t.Context(), 100*time.Millisecond)
	err = worker.keepalive(firstCtx)
	cancelFirst()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked Keepalive write error = %v, want context deadline exceeded", err)
	}
	worker.connMu.Lock()
	cachedAfterTimeout := worker.tcpConn
	worker.connMu.Unlock()
	if cachedAfterTimeout != nil {
		t.Fatal("write-blocked TCP connection remained cached")
	}

	secondCtx, cancelSecond := context.WithTimeout(t.Context(), time.Second)
	err = worker.keepalive(secondCtx)
	cancelSecond()
	if err != nil {
		t.Fatal(err)
	}
	if dialCalls != 2 {
		t.Fatalf("TCP dial calls = %d, want 2", dialCalls)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestCascadeTCPInviteTimeoutCancelsBeforeInvalidatingConnection(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	defer sipServer.Close()
	worker := newCascadeWorker(&Server{Server: sipServer}, platform)
	defer worker.closeTCPConnection()

	registrarDone := make(chan error, 1)
	worker.dialTCP = func(_ context.Context, _ string) (net.Conn, error) {
		client, registrar := net.Pipe()
		clientConn := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41200},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		go func() {
			defer registrar.Close()
			reader := bufio.NewReader(registrar)
			invite, readErr := readCascadeTestTCPMessage(reader)
			if readErr != nil {
				registrarDone <- readErr
				return
			}
			cancel, readErr := readCascadeTestTCPMessage(reader)
			if readErr != nil {
				registrarDone <- readErr
				return
			}
			if !strings.HasPrefix(invite, "INVITE ") || !strings.HasPrefix(cancel, "CANCEL ") {
				registrarDone <- fmt.Errorf("unexpected INVITE timeout sequence: %q / %q", invite, cancel)
				return
			}
			if cascadeTestHeader(invite, "Via") != cascadeTestHeader(cancel, "Via") ||
				cascadeTestHeader(invite, "Call-ID") != cascadeTestHeader(cancel, "Call-ID") {
				registrarDone <- fmt.Errorf("CANCEL did not preserve INVITE transaction identity")
				return
			}
			buffer := make([]byte, 1)
			_, readErr = registrar.Read(buffer)
			if !errors.Is(readErr, io.EOF) && !errors.Is(readErr, net.ErrClosed) {
				registrarDone <- fmt.Errorf("timed-out INVITE connection close: %w", readErr)
				return
			}
			registrarDone <- nil
		}()
		return clientConn, nil
	}

	callID := sip.CallID("cascade-timeout-invite")
	invite := worker.newRequest(sip.MethodInvite, &sip.ContentTypeSDP, []byte("offer"), &callID, 1, -1, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	_, err = worker.exchangeRequest(ctx, invite)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("INVITE error = %v, want context deadline exceeded", err)
	}
	worker.connMu.Lock()
	connection, remote := worker.tcpConn, worker.tcpRemote
	worker.connMu.Unlock()
	if connection != nil || remote != "" {
		t.Fatalf("timed-out INVITE connection remained cached: connection=%v remote=%q", connection, remote)
	}
	select {
	case registrarErr := <-registrarDone:
		if registrarErr != nil {
			t.Fatal(registrarErr)
		}
	case <-time.After(time.Second):
		t.Fatal("CANCEL was not sent before invalidating the timed-out INVITE connection")
	}
}

func TestCascadeRegisterOverTLSUsesVerifiedPersistentConnection(t *testing.T) {
	platform := testCascadeTLSPlatform(t, "3.0")
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.20:5061")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	defer sipServer.Close()
	worker := newCascadeWorker(&Server{Server: sipServer}, platform)
	defer worker.closeTCPConnection()
	if worker.platform.tlsConfig == nil || worker.platform.tlsConfig.InsecureSkipVerify || worker.platform.tlsConfig.ServerName != "sip.example.com" {
		t.Fatalf("TLS verification config = %+v", worker.platform.tlsConfig)
	}

	registrarErr := make(chan error, 1)
	allowRegistrarClose := make(chan struct{})
	dialCalls := 0
	worker.dialTLS = func(_ context.Context, address, serverName string) (net.Conn, error) {
		dialCalls++
		if address != "192.0.2.30:5061" || serverName != "sip.example.com" {
			return nil, fmt.Errorf("TLS dial target = %s server_name=%s", address, serverName)
		}
		client, registrar := net.Pipe()
		clientConn := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41002},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5061},
		}
		go func() {
			defer registrar.Close()
			reader := bufio.NewReader(registrar)
			for index := 0; index < 3; index++ {
				request, readErr := readCascadeTestTCPMessage(reader)
				if readErr != nil {
					registrarErr <- readErr
					return
				}
				if !strings.Contains(request, "Via: SIP/2.0/TLS") || !strings.Contains(request, "<sips:"+gb10DeviceID+"@192.0.2.20:5061;transport=tls>") {
					registrarErr <- fmt.Errorf("TLS request missing transport markers: %s", request)
					return
				}
				status, reason := http.StatusUnauthorized, "Unauthorized"
				extra := `WWW-Authenticate: Digest realm="3402000000",qop="auth",nonce="tls-nonce"`
				if index == 1 {
					status, reason, extra = http.StatusOK, "OK", "Expires: 3600\r\nX-GB-Ver: 3.0"
				} else if index == 2 {
					if !strings.HasPrefix(request, "MESSAGE ") {
						registrarErr <- fmt.Errorf("unexpected TLS keepalive request: %s", request)
						return
					}
					status, reason, extra = http.StatusOK, "OK", ""
				}
				if _, writeErr := io.WriteString(registrar, cascadeTestTCPResponse(request, status, reason, extra)); writeErr != nil {
					registrarErr <- writeErr
					return
				}
			}
			registrarErr <- nil
			<-allowRegistrarClose
		}()
		return clientConn, nil
	}

	if err := worker.register(t.Context(), worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	manager := NewCascadeManager(nil)
	manager.items[worker.platform.name] = worker
	source := &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5061}
	if _, ok := manager.matchRegistered(gb10PlatformID, source); ok {
		t.Fatal("TLS upstream was authorized without its verified connection")
	}
	worker.connMu.Lock()
	verifiedConnection := worker.tcpConn
	worker.connMu.Unlock()
	if matched, ok := manager.matchRegistered(gb10PlatformID, source, verifiedConnection); !ok || matched != worker {
		t.Fatalf("verified TLS upstream source match = %v, %v", matched, ok)
	}
	if err := worker.keepalive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if dialCalls != 1 {
		t.Fatalf("TLS cascade dial calls = %d, want 1", dialCalls)
	}
	if err := <-registrarErr; err != nil {
		t.Fatal(err)
	}
	close(allowRegistrarClose)
}

func TestCascadeTLSDialerVerifiesServerCertificate(t *testing.T) {
	registrar := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer registrar.Close()
	certificate, err := x509.ParseCertificate(registrar.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	serverName := ""
	if len(certificate.DNSNames) > 0 {
		serverName = certificate.DNSNames[0]
	} else if len(certificate.IPAddresses) > 0 {
		serverName = certificate.IPAddresses[0].String()
	}
	if serverName == "" {
		t.Fatal("test TLS certificate has no verifiable DNS name or IP address")
	}
	address := registrar.Listener.Addr().String()
	caFile := t.TempDir() + "/upstream-ca.pem"
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := cascadeTLSClientConfig(conf.SIPUpstream{TLSCA: caFile, TLSServerName: serverName}, "ignored.example")
	if err != nil {
		t.Fatal(err)
	}
	if tlsConfig.RootCAs == nil || len(tlsConfig.RootCAs.Subjects()) != 1 {
		t.Fatalf("custom TLS CA pool subjects = %d, want 1", len(tlsConfig.RootCAs.Subjects()))
	}

	trusted := testCascadeTLSPlatform(t, "3.0")
	trusted.tlsConfig = tlsConfig
	worker := newCascadeWorker(nil, trusted)
	connection, err := worker.dialTLS(t.Context(), address, serverName)
	if err != nil {
		t.Fatalf("trusted TLS certificate rejected: %v", err)
	}
	_ = connection.Close()

	untrusted := testCascadeTLSPlatform(t, "3.0")
	untrusted.tlsConfig.ServerName = serverName
	untrustedWorker := newCascadeWorker(nil, untrusted)
	if connection, err := untrustedWorker.dialTLS(t.Context(), address, serverName); err == nil {
		_ = connection.Close()
		t.Fatal("untrusted TLS certificate was accepted")
	}
}

func TestCascadeTCPDateNoteSignalDigestLifecycle(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	platform.signalDigestSeed = "upstream-note-seed"
	localURI, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	defer sipServer.Close()
	api := &GB28181API{cfg: &conf.SIP{
		Password: "global-password",
		SignalDigest: conf.SIPSignalDigest{
			Enabled: true, Required: true, Seed: "global-note-seed", Algorithm: "MD5",
			Encoding: "base64", Window: conf.Duration(10 * time.Minute),
		},
	}}
	server := &Server{Server: sipServer, gb: api}
	api.svr = server
	worker := newCascadeWorker(server, platform)
	defer worker.closeTCPConnection()

	registrarErr := make(chan error, 1)
	allowRegistrarClose := make(chan struct{})
	worker.dialTCP = func(_ context.Context, _ string) (net.Conn, error) {
		client, registrar := net.Pipe()
		clientConn := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41001},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		go func() {
			defer registrar.Close()
			reader := bufio.NewReader(registrar)
			register, readErr := readCascadeTestTCPMessage(reader)
			if readErr != nil {
				registrarErr <- readErr
				return
			}
			if !strings.HasPrefix(register, "REGISTER ") || hasSignalDigestHeaders(register) {
				registrarErr <- fmt.Errorf("REGISTER must not carry Date+Note: %s", register)
				return
			}
			if _, writeErr := io.WriteString(registrar, cascadeTestTCPResponse(register, http.StatusOK, "OK", "Expires: 3600")); writeErr != nil {
				registrarErr <- writeErr
				return
			}

			keepalive, readErr := readCascadeTestTCPMessage(reader)
			if readErr != nil {
				registrarErr <- readErr
				return
			}
			if !strings.HasPrefix(keepalive, "MESSAGE ") || !strings.Contains(keepalive, "<CmdType>Keepalive</CmdType>") {
				registrarErr <- fmt.Errorf("unexpected secured keepalive: %s", keepalive)
				return
			}
			if verifyErr := verifyCascadeTestSignalDigest(keepalive, platform.signalDigestSeed); verifyErr != nil {
				registrarErr <- verifyErr
				return
			}
			response := cascadeTestSignedTCPResponse(keepalive, http.StatusOK, "OK", platform.signalDigestSeed)
			if _, writeErr := io.WriteString(registrar, response); writeErr != nil {
				registrarErr <- writeErr
				return
			}
			registrarErr <- nil
			<-allowRegistrarClose
		}()
		return clientConn, nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := worker.register(ctx, worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	if err := worker.keepalive(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-registrarErr; err != nil {
		t.Fatal(err)
	}
	close(allowRegistrarClose)
	if status := worker.snapshot(); !status.Registered || status.LastKeepaliveAt.IsZero() {
		t.Fatalf("secured cascade status = %+v", status)
	}
}

func TestCascadeOversizedUDPRequestUsesTCP(t *testing.T) {
	platform := testCascadePlatform(t, string(GBVersion30))
	localURI, err := sip.ParseSipURI("sip:" + platform.localID + "@" + platform.localDomain)
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	worker := newCascadeWorker(&Server{Server: sipServer}, platform)
	var dialAddress string
	received := make(chan string, 1)
	registrarErr := make(chan error, 1)
	worker.dialTCP = func(_ context.Context, address string) (net.Conn, error) {
		dialAddress = address
		client, remote := net.Pipe()
		connection := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: platform.contactPort("tcp")},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		go func() {
			defer remote.Close()
			message, readErr := readCascadeTestTCPMessage(bufio.NewReader(remote))
			if readErr != nil {
				registrarErr <- readErr
				return
			}
			received <- message
			_, writeErr := io.WriteString(remote, cascadeTestTCPResponse(message, http.StatusOK, "OK", ""))
			registrarErr <- writeErr
		}()
		return connection, nil
	}
	t.Cleanup(func() {
		worker.closeTCPConnection()
		sipServer.Close()
	})

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := worker.sendMessage(ctx, []byte(strings.Repeat("x", 1301))); err != nil {
		t.Fatal(err)
	}
	if dialAddress != "192.0.2.30:5060" {
		t.Fatalf("oversized request TCP target = %q", dialAddress)
	}
	if err := <-registrarErr; err != nil {
		t.Fatal(err)
	}
	message := <-received
	if !strings.Contains(message, "Via: SIP/2.0/TCP") {
		t.Fatalf("oversized request Via transport = %s", message)
	}
	if contact := cascadeTestHeader(message, "Contact"); !strings.Contains(contact, "transport=tcp") {
		t.Fatalf("oversized request Contact = %q", contact)
	}
	if startLine := strings.SplitN(message, "\r\n", 2)[0]; !strings.Contains(startLine, "transport=tcp") {
		t.Fatalf("oversized request URI = %q", startLine)
	}
}

func TestCascadeSignedOversizedUDPRequestUsesTCP(t *testing.T) {
	platform := testCascadePlatform(t, string(GBVersion30))
	platform.signalDigestSeed = "upstream-note-seed"
	localURI, err := sip.ParseSipURI("sip:" + platform.localID + "@" + platform.localDomain)
	if err != nil {
		t.Fatal(err)
	}
	sipServer := sip.NewServer(&sip.Address{URI: &localURI, Params: sip.NewParams()})
	api := &GB28181API{cfg: &conf.SIP{
		Password: "global-password",
		SignalDigest: conf.SIPSignalDigest{
			Enabled: true, Required: true, Seed: "global-note-seed", Algorithm: "MD5",
			Encoding: "base64", Window: conf.Duration(10 * time.Minute),
		},
	}}
	server := &Server{Server: sipServer, gb: api}
	api.svr = server
	worker := newCascadeWorker(server, platform)
	security, err := worker.signalDigestSecurity()
	if err != nil {
		t.Fatal(err)
	}
	var body []byte
	for size := 1; size <= cascadeReliableTransportThreshold; size++ {
		candidate := worker.newKeepaliveRequest([]byte(strings.Repeat("x", size)))
		unsignedLength := len(candidate.String())
		if err := security.Sign(candidate); err != nil {
			t.Fatal(err)
		}
		if unsignedLength <= cascadeReliableTransportThreshold && len(candidate.String()) > cascadeReliableTransportThreshold {
			body = []byte(strings.Repeat("x", size))
			break
		}
	}
	if len(body) == 0 {
		t.Fatal("failed to construct a request that only exceeds the UDP threshold after signing")
	}

	received := make(chan string, 1)
	registrarErr := make(chan error, 1)
	worker.dialTCP = func(_ context.Context, _ string) (net.Conn, error) {
		client, remote := net.Pipe()
		connection := &cascadeTestTCPConn{
			Conn:   client,
			local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: platform.contactPort("tcp")},
			remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		}
		go func() {
			defer remote.Close()
			message, readErr := readCascadeTestTCPMessage(bufio.NewReader(remote))
			if readErr != nil {
				registrarErr <- readErr
				return
			}
			received <- message
			response := cascadeTestSignedTCPResponse(message, http.StatusOK, "OK", platform.signalDigestSeed)
			_, writeErr := io.WriteString(remote, response)
			registrarErr <- writeErr
		}()
		return connection, nil
	}
	t.Cleanup(func() {
		worker.closeTCPConnection()
		sipServer.Close()
	})

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := worker.sendMessage(ctx, body); err != nil {
		t.Fatal(err)
	}
	if err := <-registrarErr; err != nil {
		t.Fatal(err)
	}
	message := <-received
	if len(message) <= cascadeReliableTransportThreshold {
		t.Fatalf("signed request length = %d", len(message))
	}
	if !strings.Contains(message, "Via: SIP/2.0/TCP") {
		t.Fatalf("signed oversized request Via transport = %s", message)
	}
	if err := verifyCascadeTestSignalDigest(message, platform.signalDigestSeed); err != nil {
		t.Fatal(err)
	}
}

type cascadeTestTCPConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (c *cascadeTestTCPConn) LocalAddr() net.Addr  { return c.local }
func (c *cascadeTestTCPConn) RemoteAddr() net.Addr { return c.remote }

func readCascadeTestTCPMessage(reader *bufio.Reader) (string, error) {
	var message strings.Builder
	bodyLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		message.WriteString(line)
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			break
		}
		name, value, found := strings.Cut(trimmed, ":")
		if found && (strings.EqualFold(strings.TrimSpace(name), "Content-Length") || strings.EqualFold(strings.TrimSpace(name), "l")) {
			bodyLength, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return "", err
			}
		}
	}
	if bodyLength > 0 {
		body := make([]byte, bodyLength)
		if _, err := io.ReadFull(reader, body); err != nil {
			return "", err
		}
		message.Write(body)
	}
	return message.String(), nil
}

func cascadeTestTCPResponse(request string, status int, reason, extra string) string {
	to := cascadeTestHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=tcp-registrar"
	}
	if status >= 200 && status < 300 && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(request)), "REGISTER ") {
		appendHeader := func(name, value string) {
			if cascadeTestHeader(extra, name) != "" {
				return
			}
			if extra != "" {
				extra += "\r\n"
			}
			extra += name + ": " + value
		}
		appendHeader("Contact", cascadeTestHeader(request, "Contact"))
		appendHeader("Expires", cascadeTestHeader(request, "Expires"))
		appendHeader("Date", sip.FormatGBTime(time.Now(), "2006-01-02T15:04:05.000"))
	}
	if extra != "" {
		extra += "\r\n"
	}
	return fmt.Sprintf("SIP/2.0 %d %s\r\nVia: %s\r\nFrom: %s\r\nTo: %s\r\nCall-ID: %s\r\nCSeq: %s\r\n%sContent-Length: 0\r\n\r\n",
		status, reason, cascadeTestHeader(request, "Via"), cascadeTestHeader(request, "From"), to,
		cascadeTestHeader(request, "Call-ID"), cascadeTestHeader(request, "CSeq"), extra)
}

func cascadeTestHeader(message, name string) string {
	for line := range strings.SplitSeq(message, "\r\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func hasSignalDigestHeaders(message string) bool {
	return cascadeTestHeader(message, "Date") != "" || cascadeTestHeader(message, "Note") != ""
}

func verifyCascadeTestSignalDigest(message, seed string) error {
	date := cascadeTestHeader(message, "Date")
	note := cascadeTestHeader(message, "Note")
	if date == "" || note == "" {
		return fmt.Errorf("secured cascade message is missing Date or Note")
	}
	body := ""
	if _, value, ok := strings.Cut(message, "\r\n\r\n"); ok {
		body = value
	}
	digest := md5.Sum([]byte(cascadeTestHeader(message, "From") + cascadeTestHeader(message, "To") +
		cascadeTestHeader(message, "Call-ID") + date + seed + body))
	expected := base64.StdEncoding.EncodeToString(digest[:])
	auth := sip.AuthFromValue(note)
	if auth.Get("nonce") != expected || !strings.EqualFold(auth.Algorithm(), "MD5") {
		return fmt.Errorf("invalid cascade Date+Note digest: %s", note)
	}
	return nil
}

func cascadeTestSignedTCPResponse(request string, status int, reason, seed string) string {
	to := cascadeTestHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=tcp-registrar"
	}
	date := time.Now().In(time.FixedZone("CST", 8*60*60)).Format("2006-01-02T15:04:05")
	digest := md5.Sum([]byte(cascadeTestHeader(request, "From") + to + cascadeTestHeader(request, "Call-ID") + date + seed))
	extra := fmt.Sprintf("Date: %s\r\nNote: Digest nonce=\"%s\",algorithm=MD5", date, base64.StdEncoding.EncodeToString(digest[:]))
	return cascadeTestTCPResponse(request, status, reason, extra)
}

func TestCascadeRegisterFollows2022Redirect(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	requests := make([]*sip.Request, 0, 3)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		switch len(requests) {
		case 1:
			response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
			redirectURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.31:5070")
			if err != nil {
				t.Fatal(err)
			}
			response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
			version := sip.XGBVer(GBVersion30)
			response.AppendHeader(&version)
			return response, nil
		case 2:
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",qop="auth",nonce="redirect-nonce"`})
			version := sip.XGBVer(GBVersion30)
			response.AppendHeader(&version)
			return response, nil
		default:
			response := newCascadeRegisterSuccessResponse(t, request, 3600)
			version := sip.XGBVer(GBVersion30)
			response.AppendHeader(&version)
			return response, nil
		}
	}

	if err := worker.register(t.Context(), worker.platform.expires); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("redirect REGISTER request count = %d", len(requests))
	}
	for _, request := range requests[1:] {
		if got := request.Recipient().String(); got != "sip:"+gb10PlatformID+"@192.0.2.31:5070" {
			t.Fatalf("redirect REGISTER Request-URI = %s", got)
		}
		if got := request.Destination().String(); got != "192.0.2.31:5070" {
			t.Fatalf("redirect REGISTER destination = %s", got)
		}
		if got, err := singleSIPHeaderValue(request, "X-GB-Ver"); err != nil || got != string(GBVersion30) {
			t.Fatalf("redirect REGISTER X-GB-Ver = %q, %v", got, err)
		}
	}
	auth := sip.AuthFromValue(requests[2].GetHeaders("Authorization")[0].String())
	if auth.Get("uri") != requests[2].Recipient().String() {
		t.Fatalf("redirect Digest uri = %q; want %q", auth.Get("uri"), requests[2].Recipient().String())
	}
	if status := worker.snapshot(); status.Address != "192.0.2.31:5070" || !status.Registered {
		t.Fatalf("redirect registration status = %+v", status)
	}
	manager := NewCascadeManager(nil)
	manager.items[worker.platform.name] = worker
	if matched, ok := manager.matchRegistered(gb10PlatformID, &net.UDPAddr{IP: net.ParseIP("192.0.2.31"), Port: 5070}); !ok || matched != worker {
		t.Fatalf("redirected upstream source match = %v, %v", matched, ok)
	}
	if _, ok := manager.matchRegistered(gb10PlatformID, worker.platform.remote); ok {
		t.Fatal("configured upstream source remained authorized after redirect")
	}

	var keepalive *sip.Request
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		keepalive = request
		return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
	}
	if err := worker.keepalive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if keepalive == nil || keepalive.Recipient().String() != "sip:"+gb10PlatformID+"@192.0.2.31:5070" || keepalive.Destination().String() != "192.0.2.31:5070" {
		t.Fatalf("redirect keepalive target = %#v", keepalive)
	}
}

func TestCascadeRegisterRejectsUnsafeRedirect(t *testing.T) {
	tests := []struct {
		name    string
		contact string
		want    string
	}{
		{name: "different server", contact: "sip:34020000002000009999@192.0.2.31:5070"},
		{name: "empty password component", contact: "sip:" + gb10PlatformID + ":@192.0.2.31:5070", want: "password"},
		{name: "sips transport conflict", contact: "sips:" + gb10PlatformID + "@192.0.2.31:5071;transport=tcp"},
		{name: "transport unsupported", contact: "sip:" + gb10PlatformID + "@192.0.2.31:5070;transport=ws"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
				redirectURI, err := sip.ParseSipURI(test.contact)
				if err != nil {
					t.Fatal(err)
				}
				response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
				return response, nil
			}
			want := test.want
			if want == "" {
				want = "redirect"
			}
			if err := worker.register(t.Context(), worker.platform.expires); err == nil || !strings.Contains(strings.ToLower(err.Error()), want) {
				t.Fatalf("unsafe redirect error = %v", err)
			}
		})
	}
}

func TestCascadeRegisterRedirectSupportsTCPTransport(t *testing.T) {
	request := sip.NewRequest("", sip.MethodRegister, &sip.URI{FHost: "remote.example"}, sip.DefaultSipVersion, nil, nil)
	response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
	redirectURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.31:5070;transport=tcp")
	if err != nil {
		t.Fatal(err)
	}
	response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
	uri, remote, err := cascadeRegisterRedirectTarget(response, gb10PlatformID, "udp")
	if err != nil {
		t.Fatal(err)
	}
	if uri == nil || remote == nil || cascadeTransportForAddr(remote) != "tcp" || remote.String() != "192.0.2.31:5070" {
		t.Fatalf("TCP redirect target = %v / %v", uri, remote)
	}
}

func TestCascadeRegisterRedirectSupportsSIPS(t *testing.T) {
	request := sip.NewRequest("", sip.MethodRegister, &sip.URI{FHost: "remote.example"}, sip.DefaultSipVersion, nil, nil)
	response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
	redirectURI, err := sip.ParseSipURI("sips:" + gb10PlatformID + "@192.0.2.31")
	if err != nil {
		t.Fatal(err)
	}
	response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
	uri, remote, err := cascadeRegisterRedirectTarget(response, gb10PlatformID, "udp")
	if err != nil {
		t.Fatal(err)
	}
	if uri == nil || !uri.FIsEncrypted || remote == nil || cascadeTransportForAddr(remote) != "tls" || remote.String() != "192.0.2.31:5061" {
		t.Fatalf("SIPS redirect target = %v / %v", uri, remote)
	}
}

func TestCascadeRegisterRedirectRejectsSIPSDowngrade(t *testing.T) {
	request := sip.NewRequest("", sip.MethodRegister, &sip.URI{FHost: "remote.example"}, sip.DefaultSipVersion, nil, nil)
	response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
	redirectURI, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.31:5060;transport=udp")
	if err != nil {
		t.Fatal(err)
	}
	response.AppendHeader(&sip.ContactHeader{Address: &redirectURI, Params: sip.NewParams()})
	if _, _, err := cascadeRegisterRedirectTarget(response, gb10PlatformID, "tls"); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("SIPS downgrade error = %v", err)
	}
}

func TestCascadeRegisterRedirectUpdatesRequestTransport(t *testing.T) {
	tests := []struct {
		name          string
		platform      func(*testing.T, string) cascadePlatform
		redirect      string
		wantTransport string
	}{
		{name: "udp to tcp", platform: testCascadePlatform, redirect: "tcp", wantTransport: "TCP"},
		{name: "tcp to udp", platform: testCascadeTCPPlatform, redirect: "udp", wantTransport: "UDP"},
		{name: "udp to tls", platform: testCascadePlatform, redirect: "tls", wantTransport: "TLS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newCascadeWorker(nil, test.platform(t, "3.0"))
			requests := make([]*sip.Request, 0, 2)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests = append(requests, request)
				if len(requests) == 1 {
					response := sip.NewResponseFromRequest("", request, http.StatusMovedPermanently, "Moved Permanently", nil)
					uri, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.31:5070;transport=" + test.redirect)
					if err != nil {
						t.Fatal(err)
					}
					response.AppendHeader(&sip.ContactHeader{Address: &uri, Params: sip.NewParams()})
					return response, nil
				}
				return newCascadeRegisterSuccessResponse(t, request, 3600), nil
			}
			if err := worker.register(t.Context(), worker.platform.expires); err != nil {
				t.Fatal(err)
			}
			if len(requests) != 2 {
				t.Fatalf("redirect request count = %d", len(requests))
			}
			via, _ := requests[1].ViaHop()
			contact, _ := requests[1].Contact()
			if via == nil || via.Transport != test.wantTransport || cascadeTransportForAddr(requests[1].Destination()) != strings.ToLower(test.wantTransport) {
				t.Fatalf("redirect transport = via %v destination %v", via, requests[1].Destination())
			}
			wantParam := strings.ToLower(test.wantTransport)
			hasTransportContact := contact != nil && contact.Address != nil && strings.Contains(strings.ToLower(contact.Address.String()), "transport="+wantParam)
			if hasTransportContact != (test.wantTransport != "UDP") {
				t.Fatalf("redirect Contact = %v", contact)
			}
		})
	}
}

func TestCascadeKeepaliveUsesNegotiatedVersion(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	worker.effective = GBVersion20
	var request *sip.Request
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		request = in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	if err := worker.keepalive(t.Context()); err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Method() != sip.MethodMessage {
		t.Fatalf("Keepalive request = %#v", request)
	}
	from, _ := request.From()
	to, _ := request.To()
	if from == nil || from.Address.String() != "sip:"+gb10DeviceID+"@local.example" || to == nil || to.Address.String() != "sip:"+gb10PlatformID+"@remote.example" {
		t.Fatalf("Keepalive From/To = %v / %v", from, to)
	}
	if got := request.GetHeaders("X-GB-Ver"); len(got) != 1 || !strings.Contains(got[0].String(), "2.0") {
		t.Fatalf("Keepalive X-GB-Ver = %v", got)
	}
	var body MessageNotify
	if err := sip.XMLDecode(request.Body(), &body); err != nil {
		t.Fatal(err)
	}
	if body.CmdType != "Keepalive" || body.DeviceID != gb10DeviceID || body.Status != "OK" {
		t.Fatalf("Keepalive body = %+v", body)
	}
}

func TestCascadeUnregisterUsesDigestAndExpiresZero(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "1.1"))
	worker.updateStatus(func(status *CascadePlatformStatus) {
		status.State = "registered"
		status.Registered = true
	})
	requests := make([]*sip.Request, 0, 2)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		if len(request.GetHeaders("Authorization")) == 0 {
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",nonce="logout-nonce"`})
			return response, nil
		}
		return newCascadeRegisterSuccessResponse(t, request, 0), nil
	}

	worker.unregisterOnStop()
	if len(requests) != 2 {
		t.Fatalf("unregister request count = %d", len(requests))
	}
	for _, request := range requests {
		headers := request.GetHeaders("Expires")
		if len(headers) != 1 || !strings.HasSuffix(headers[0].String(), ": 0") {
			t.Fatalf("unregister Expires = %v", headers)
		}
		assertCascadeRegisterAddressing(t, request)
	}
	if len(requests[1].GetHeaders("Authorization")) != 1 {
		t.Fatal("authenticated unregister missing Authorization")
	}
	status := worker.snapshot()
	if status.State != "stopped" || status.Registered || !status.ExpiresAt.IsZero() {
		t.Fatalf("unregister status = %+v", status)
	}
}

func TestCascadeUnregistersActiveBindingAfterKeepaliveFailure(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			worker.updateStatus(func(status *CascadePlatformStatus) {
				status.State = "retrying"
				status.Registered = false
				status.ExpiresAt = time.Now().Add(time.Minute)
			})
			var requests []*sip.Request
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				requests = append(requests, request)
				return newCascadeRegisterSuccessResponse(t, request, 0), nil
			}

			worker.unregisterOnStop()
			if len(requests) != 1 {
				t.Fatalf("unregister request count = %d, want 1", len(requests))
			}
			headers := requests[0].GetHeaders("Expires")
			if len(headers) != 1 || !strings.HasSuffix(headers[0].String(), ": 0") {
				t.Fatalf("unregister Expires = %v", headers)
			}
			status := worker.snapshot()
			if status.State != "stopped" || status.Registered || !status.ExpiresAt.IsZero() {
				t.Fatalf("unregister status = %+v", status)
			}
		})
	}
}

func TestCascadeWorkerRunRetriesKeepaliveFailureAndUnregistersOnStop(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			worker.platform.keepaliveInterval = time.Millisecond
			keepaliveFailed := make(chan struct{})
			retryStarted := make(chan struct{})
			allowRetrySuccess := make(chan struct{})
			positiveRegisters := 0
			keepalives := 0
			unregisters := 0
			worker.exchange = func(ctx context.Context, request *sip.Request) (*sip.Response, error) {
				switch request.Method() {
				case sip.MethodRegister:
					headers := request.GetHeaders("Expires")
					if len(headers) == 1 && strings.HasSuffix(headers[0].String(), ": 0") {
						unregisters++
						return newCascadeRegisterSuccessResponse(t, request, 0), nil
					}
					positiveRegisters++
					if positiveRegisters == 2 {
						close(retryStarted)
						<-allowRetrySuccess
					}
					return newCascadeRegisterSuccessResponse(t, request, worker.platform.expires), nil
				case sip.MethodMessage:
					keepalives++
					if keepalives == 1 {
						close(keepaliveFailed)
						return nil, errors.New("synthetic Keepalive failure")
					}
					<-ctx.Done()
					return nil, ctx.Err()
				default:
					return nil, fmt.Errorf("unexpected cascade method %s", request.Method())
				}
			}

			worker.start()
			select {
			case <-keepaliveFailed:
			case <-time.After(time.Second):
				t.Fatal("initial cascade Keepalive did not fail")
			}
			select {
			case <-retryStarted:
			case <-time.After(time.Second):
				t.Fatal("cascade registration retry did not start")
			}
			close(allowRetrySuccess)
			deadline := time.Now().Add(time.Second)
			for !worker.snapshot().Registered {
				if time.Now().After(deadline) {
					t.Fatalf("cascade retry did not commit: %+v", worker.snapshot())
				}
				time.Sleep(time.Millisecond)
			}

			worker.stop()
			if positiveRegisters != 2 || keepalives < 1 || unregisters != 1 {
				t.Fatalf("cascade lifecycle requests = REGISTER:%d Keepalive:%d unregister:%d", positiveRegisters, keepalives, unregisters)
			}
			status := worker.snapshot()
			if status.State != "stopped" || status.Registered || !status.ExpiresAt.IsZero() {
				t.Fatalf("cascade lifecycle final status = %+v", status)
			}
		})
	}
}

func TestCascadeRegisterSuccessAfterStopIsTrackedOnlyForUnregister(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			unregisters := 0
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				headers := request.GetHeaders("Expires")
				if len(headers) == 1 && strings.HasSuffix(headers[0].String(), ": 0") {
					unregisters++
					return newCascadeRegisterSuccessResponse(t, request, 0), nil
				}
				return newCascadeRegisterSuccessResponse(t, request, worker.platform.expires), nil
			}

			worker.beginStop()
			err := worker.register(t.Context(), worker.platform.expires)
			if !errors.Is(err, errCascadeWorkerStopping) {
				t.Fatalf("late REGISTER success error = %v", err)
			}
			status := worker.snapshot()
			if status.State != "stopping" || status.Registered || status.ExpiresAt.IsZero() {
				t.Fatalf("late REGISTER success status = %+v", status)
			}

			worker.unregisterOnStop()
			if unregisters != 1 {
				t.Fatalf("late REGISTER unregister count = %d, want 1", unregisters)
			}
			status = worker.snapshot()
			if status.State != "stopped" || status.Registered || !status.ExpiresAt.IsZero() {
				t.Fatalf("late REGISTER final status = %+v", status)
			}
		})
	}
}

func TestCascadeKeepaliveDoesNotReviveExpiredRegistration(t *testing.T) {
	for _, version := range []string{"1.0", "1.1", "2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			worker := newCascadeWorker(nil, testCascadePlatform(t, version))
			before := time.Now().Add(-time.Hour)
			worker.updateStatus(func(state *CascadePlatformStatus) {
				state.State = "registered"
				state.Registered = true
				state.ExpiresAt = time.Now().Add(-time.Second)
				state.LastKeepaliveAt = before
			})
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}

			if err := worker.keepalive(t.Context()); err != nil {
				t.Fatal(err)
			}
			status := worker.snapshot()
			if status.State != "expired" || status.Registered || !status.LastKeepaliveAt.After(before) {
				t.Fatalf("expired cascade Keepalive status = %+v", status)
			}
		})
	}
}

func TestCascadeExpiredRegistrationIsNotUsable(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	worker.updateStatus(func(state *CascadePlatformStatus) {
		state.State = "registered"
		state.Registered = true
		state.ExpiresAt = time.Now().Add(-time.Second)
	})
	manager := NewCascadeManager(nil)
	manager.items[worker.platform.name] = worker

	if _, ok := manager.matchRegistered(worker.platform.serverID, worker.platform.remote); ok {
		t.Fatal("expired cascade registration matched inbound platform")
	}
	if workers := manager.registeredWorkers(GBVersion10); len(workers) != 0 {
		t.Fatalf("expired cascade registration remained in registered workers: %d", len(workers))
	}
	statuses := manager.Statuses()
	if len(statuses) != 1 || statuses[0].Registered || statuses[0].State != "expired" {
		t.Fatalf("expired cascade status = %+v", statuses)
	}
}

func TestNormalizeCascadePlatformsRejectsUnsafeConfiguration(t *testing.T) {
	local := conf.SIP{ID: gb10DeviceID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060}
	base := conf.SIPUpstream{
		Name: "same", Enabled: true, ServerID: gb10PlatformID,
		Host: "192.0.2.30", LocalID: gb10DeviceID, Version: "1.1",
	}
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{base, base}, ""); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate upstream error = %v", err)
	}
	invalid := base
	invalid.Version = "9.9"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("invalid version error = %v", err)
	}
	invalid = base
	invalid.ServerID = "not-a-gb-id"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "server_id") {
		t.Fatalf("invalid server ID error = %v", err)
	}
	invalid = base
	invalid.Transport = "sctp"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("invalid transport error = %v", err)
	}
	invalid = base
	invalid.Transport = "tls"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "local SIP-TLS listener") {
		t.Fatalf("TLS without local listener error = %v", err)
	}
	invalid = base
	invalid.Transport = "tls"
	invalid.TLSCert = "client.crt"
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "together") {
		t.Fatalf("incomplete TLS client credentials error = %v", err)
	}
	invalid = base
	invalid.Transport = "tls"
	invalid.TLSCA = t.TempDir() + "/invalid-ca.pem"
	if err := os.WriteFile(invalid.TLSCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "valid certificate") {
		t.Fatalf("invalid TLS CA error = %v", err)
	}
	invalid = base
	local.Port = 0
	if _, err := normalizeCascadePlatforms(local, []conf.SIPUpstream{invalid}, ""); err == nil || !strings.Contains(err.Error(), "local SIP port") {
		t.Fatalf("invalid local SIP port error = %v", err)
	}
}

func TestCascadeAcceptedExpiresRejectsInvalidResponse(t *testing.T) {
	response := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil)
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "0"})
	if _, err := cascadeAcceptedExpires(response, 3600); err == nil {
		t.Fatal("zero accepted expiry should fail")
	}
	response.RemoveHeader("Expires")
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "7200"})
	if _, err := cascadeAcceptedExpires(response, 3600); err == nil || !strings.Contains(err.Error(), "exceeds requested") {
		t.Fatalf("extended accepted expiry error = %v", err)
	}
	response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
	if _, err := cascadeAcceptedExpires(response, 3600); err == nil || !strings.Contains(err.Error(), "multiple Expires") {
		t.Fatalf("duplicate accepted expiry error = %v", err)
	}
}

func TestNormalizeCascadeRegisterExpiryByVersion(t *testing.T) {
	local := conf.SIP{ID: gb10DeviceID, Domain: "3402000000", Host: "192.0.2.20", Port: 5060}
	base := conf.SIPUpstream{
		Name: "expiry", Enabled: true, ServerID: gb10PlatformID, Host: "192.0.2.30",
		LocalID: gb10DeviceID, LocalHost: "192.0.2.20",
	}
	for _, test := range []struct {
		version     GBProtocolVersion
		expires     int
		want        int
		wantErrPart string
	}{
		{version: GBVersion10, expires: 0, want: defaultLegacyRegisterExpires},
		{version: GBVersion11, expires: 0, want: defaultRegisterExpires},
		{version: GBVersion20, expires: minimumStandardRegisterTTL - 1, wantErrPart: "between 3600"},
		{version: GBVersion30, expires: 7 * 86400, want: 7 * 86400},
	} {
		t.Run(test.version.StandardYear()+"-"+strconv.Itoa(test.expires), func(t *testing.T) {
			input := base
			input.Version = string(test.version)
			input.Expires = test.expires
			platform, err := normalizeCascadePlatform(input, local, "")
			if test.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErrPart) {
					t.Fatalf("normalize expiry error = %v, want %q", err, test.wantErrPart)
				}
				return
			}
			if err != nil || platform.expires != test.want {
				t.Fatalf("normalize expiry = %d, %v; want %d", platform.expires, err, test.want)
			}
		})
	}
}

func TestCascadeRegisterRetries423WithMinExpires(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, string(GBVersion30)))
	requests := make([]*sip.Request, 0, 3)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests = append(requests, request)
		switch len(requests) {
		case 1:
			response := sip.NewResponseFromRequest("", request, statusIntervalTooBrief, "Interval Too Brief", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Min-Expires", Contents: "7200"})
			version := sip.XGBVer(GBVersion11)
			response.AppendHeader(&version)
			return response, nil
		case 2:
			response := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",nonce="min-expiry"`})
			version := sip.XGBVer(GBVersion11)
			response.AppendHeader(&version)
			return response, nil
		default:
			response := newCascadeRegisterSuccessResponse(t, request, 7200)
			version := sip.XGBVer(GBVersion11)
			response.AppendHeader(&version)
			return response, nil
		}
	}

	if err := worker.register(t.Context(), 3600); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 {
		t.Fatalf("REGISTER 423 request count = %d", len(requests))
	}
	for index, request := range requests {
		want := "7200"
		wantVersion := string(GBVersion11)
		if index == 0 {
			want = "3600"
			wantVersion = string(GBVersion30)
		}
		if got, err := singleSIPHeaderValue(request, "Expires"); err != nil || got != want {
			t.Fatalf("REGISTER request %d Expires = %q, %v; want %s", index+1, got, err, want)
		}
		if got, err := singleSIPHeaderValue(request, "X-GB-Ver"); err != nil || got != wantVersion {
			t.Fatalf("REGISTER request %d X-GB-Ver = %q, %v; want %s", index+1, got, err, wantVersion)
		}
	}
	if worker.effective != GBVersion11 || worker.accepted != 7200 || !worker.snapshot().Registered {
		t.Fatalf("REGISTER 423 status = %+v accepted=%d", worker.snapshot(), worker.accepted)
	}
}

func TestValidateCascadeRegisterSuccess(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, string(GBVersion30)))
	newExchange := func() (*sip.Request, *sip.Response) {
		request := worker.newRegisterRequest(3600, nil)
		return request, newCascadeRegisterSuccessResponse(t, request, 3600)
	}
	tests := []struct {
		name   string
		mutate func(*sip.Request, *sip.Response)
		want   string
	}{
		{name: "missing date", mutate: func(_ *sip.Request, response *sip.Response) { response.RemoveHeader("Date") }, want: "invalid Date"},
		{name: "duplicate date", mutate: func(_ *sip.Request, response *sip.Response) {
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Date", Contents: "2026-08-29T12:00:00.000"})
		}, want: "multiple Date"},
		{name: "invalid date", mutate: func(_ *sip.Request, response *sip.Response) {
			response.RemoveHeader("Date")
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Date", Contents: "2026-08-29 12:00:00"})
		}, want: "invalid Date"},
		{name: "missing contact", mutate: func(_ *sip.Request, response *sip.Response) { response.RemoveHeader("Contact") }, want: "missing the requested Contact"},
		{name: "mismatched contact", mutate: func(_ *sip.Request, response *sip.Response) {
			response.RemoveHeader("Contact")
			uri, err := sip.ParseSipURI("sip:" + gb10DeviceID + "@192.0.2.99:5060")
			if err != nil {
				t.Fatal(err)
			}
			response.AppendHeader(&sip.ContactHeader{Address: &uri, Params: sip.NewParams()})
		}, want: "missing the requested Contact"},
		{name: "duplicate binding", mutate: func(_ *sip.Request, response *sip.Response) {
			contact, _ := response.Contact()
			response.AppendHeader(contact.Clone())
		}, want: "duplicate binding"},
		{name: "missing expires", mutate: func(_ *sip.Request, response *sip.Response) { response.RemoveHeader("Expires") }, want: "missing accepted Expires"},
		{name: "duplicate expires", mutate: func(_ *sip.Request, response *sip.Response) {
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
		}, want: "multiple Expires"},
		{name: "extended expires", mutate: func(_ *sip.Request, response *sip.Response) {
			response.RemoveHeader("Expires")
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "7200"})
		}, want: "exceeds requested"},
		{name: "below standard minimum", mutate: func(_ *sip.Request, response *sip.Response) {
			response.RemoveHeader("Expires")
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3599"})
		}, want: "minimum 3600"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, response := newExchange()
			test.mutate(request, response)
			if _, err := validateCascadeRegisterSuccess(response, request, GBVersion30, 3600); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("REGISTER success validation error = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("contact expires overrides header", func(t *testing.T) {
		request := worker.newRegisterRequest(7200, nil)
		response := newCascadeRegisterSuccessResponse(t, request, 3600)
		contact, _ := response.Contact()
		contact.Params.Add("expires", sip.String{Str: "7200"})
		accepted, err := validateCascadeRegisterSuccess(response, request, GBVersion30, 7200)
		if err != nil || accepted != 7200 {
			t.Fatalf("Contact expires precedence = %d, %v", accepted, err)
		}
	})

	t.Run("unregister permits removed contact", func(t *testing.T) {
		request := worker.newRegisterRequest(0, nil)
		response := newCascadeRegisterSuccessResponse(t, request, 0)
		response.RemoveHeader("Contact")
		if accepted, err := validateCascadeRegisterSuccess(response, request, GBVersion30, 0); err != nil || accepted != 0 {
			t.Fatalf("unregister success = %d, %v", accepted, err)
		}
	})
}

func TestCascadeManagerSerializesConcurrentApplyAndClose(t *testing.T) {
	manager := NewCascadeManager(nil)
	local := conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}
	upstream := conf.SIPUpstream{
		Name: "provincial", Enabled: true, ServerID: gb10PlatformID,
		Domain: "remote.example", Host: "192.0.2.30", Port: 5060,
		LocalDomain: "local.example", Version: "1.1",
	}

	done := make(chan error, 2)
	go func() { done <- manager.Apply(local, []conf.SIPUpstream{upstream}) }()
	go func() { done <- manager.Apply(local, []conf.SIPUpstream{upstream}) }()
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent cascade Apply deadlocked")
		}
	}

	closed := make(chan struct{})
	go func() {
		manager.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cascade Close deadlocked")
	}
}

func TestCascadeManagerRejectsApplyAfterClose(t *testing.T) {
	manager := NewCascadeManager(nil)
	manager.Close()
	local := conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}
	upstream := conf.SIPUpstream{
		Name: "provincial", Enabled: true, ServerID: gb10PlatformID,
		Domain: "remote.example", Host: "192.0.2.30", Port: 5060,
		LocalDomain: "local.example", Version: "1.1",
	}

	if err := manager.Apply(local, []conf.SIPUpstream{upstream}); !errors.Is(err, ErrServiceStopped) {
		t.Fatalf("Apply after Close error = %v, want %v", err, ErrServiceStopped)
	}
	if statuses := manager.Statuses(); len(statuses) != 0 {
		t.Fatalf("Apply after Close recreated cascade workers: %+v", statuses)
	}
	manager.Close()
}

func TestCascadeWorkerOperationsStopBeforeRegistrationContext(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	t.Cleanup(worker.cancel)

	worker.stopOperations()
	select {
	case <-worker.operationContext().Done():
	case <-time.After(time.Second):
		t.Fatal("cascade worker business context was not canceled")
	}
	select {
	case <-worker.ctx.Done():
		t.Fatal("stopping cascade business tasks canceled registration context")
	default:
	}
}

func TestCascadeManagerApplyWaitsForWorkerBusinessTaskCancellation(t *testing.T) {
	server := &Server{}
	api := &GB28181API{svr: server}
	server.gb = api
	manager := NewCascadeManager(server)
	server.cascade = manager
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	close(worker.done)
	manager.items[worker.platform.name] = worker
	started := make(chan struct{})
	finished := make(chan struct{})
	if !api.startCascadeLifecycleTask(context.Background(), worker, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(finished)
	}) {
		t.Fatal("cascade worker business task did not start")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade worker business task start timeout")
	}

	local := conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}
	if err := manager.Apply(local, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("CascadeManager.Apply returned without canceling old worker business task")
	}
}

func TestCascadeWorkerAvailableTracksManagerOwnership(t *testing.T) {
	server := &Server{}
	manager := NewCascadeManager(server)
	server.cascade = manager
	api := &GB28181API{svr: server}
	worker := newCascadeWorker(server, testSharedCascadePlatform(t))
	replacement := newCascadeWorker(server, testSharedCascadePlatform(t))
	t.Cleanup(worker.cancel)
	t.Cleanup(replacement.cancel)
	manager.items[worker.platform.name] = worker

	if !api.cascadeWorkerAvailable(worker) {
		t.Fatal("current cascade worker reported unavailable")
	}
	manager.items[worker.platform.name] = replacement
	if api.cascadeWorkerAvailable(worker) {
		t.Fatal("replaced cascade worker reported available")
	}
}

func TestCascadeWorkerStopClearsPendingInviteTransactions(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	close(worker.done)
	worker.inviteTx = map[string]*sip.Transaction{
		"dialog-a": sip.NewTransaction("cascade-pending-a", newFlowConnection()),
		"dialog-b": sip.NewTransaction("cascade-pending-b", newFlowConnection()),
	}

	worker.stop()
	worker.inviteTxMu.Lock()
	remaining := len(worker.inviteTx)
	worker.inviteTxMu.Unlock()
	if remaining != 0 {
		t.Fatalf("pending cascade INVITE transactions after stop = %d", remaining)
	}
	worker.closeInviteTransactions()
}
