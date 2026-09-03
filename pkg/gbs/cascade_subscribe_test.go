package gbs

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestRemovingCascadeSubscriptionDoesNotWaitForNotifyResponse(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-notify-removal")
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	worker := newCascadeWorker(&Server{}, platform)
	started := make(chan struct{})
	release := make(chan struct{})
	worker.exchange = func(ctx context.Context, req *sip.Request) (*sip.Response, error) {
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return sip.NewResponseFromRequest("", req, http.StatusOK, "OK", nil), nil
		}
	}
	sub := &eventSubscription{
		Key: "cascade-notify-removal", CmdType: "Alarm", DeviceID: platform.localID,
		ExpiresAt: time.Now().Add(time.Minute), To: remote, GBVersion: string(GBVersion30),
		Event: "presence", DialogRequest: request, Response: response, Cascade: worker,
	}
	api := &GB28181API{}
	api.eventSubscribers.Store(sub.Key, sub)
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- api.sendCascadeEventNotify(sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade NOTIFY did not start")
	}

	removeDone := make(chan struct{})
	go func() {
		api.removeCascadeEventSubscriptions(worker)
		close(removeDone)
	}()
	select {
	case <-removeDone:
	case <-time.After(200 * time.Millisecond):
		close(release)
		<-sendDone
		<-removeDone
		t.Fatal("removing cascade subscription waited for in-flight NOTIFY response")
	}
	if _, exists := api.eventSubscribers.Load(sub.Key); exists {
		t.Fatal("cascade subscription remained after worker removal")
	}

	worker.cancel()
	select {
	case err := <-sendDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cascade NOTIFY error = %v; want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled cascade NOTIFY did not stop")
	}
}

func TestCascadeEventNotifyHonorsCallerCancellation(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-notify-caller-cancel")
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	worker := newCascadeWorker(&Server{}, platform)
	started := make(chan struct{})
	worker.exchange = func(ctx context.Context, _ *sip.Request) (*sip.Response, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	sub := &eventSubscription{
		Key: "cascade-notify-caller-cancel", CmdType: "Catalog", DeviceID: platform.localID,
		ExpiresAt: time.Now().Add(time.Minute), To: remote, GBVersion: string(GBVersion30),
		Event: "Catalog", DialogRequest: request, Response: response, Cascade: worker,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	api := &GB28181API{}
	api.eventSubscribers.Store(sub.Key, sub)
	go func() {
		done <- api.sendCascadeEventNotifyContext(ctx, sub, "Catalog", []byte(`<Notify><CmdType>Catalog</CmdType></Notify>`))
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("cascade NOTIFY did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled cascade NOTIFY error = %v; want %v", err, context.Canceled)
		}
	case <-time.After(time.Second):
		t.Fatal("caller cancellation did not stop cascade NOTIFY")
	}
	if actual, exists := api.eventSubscribers.Load(sub.Key); !exists || actual != sub {
		t.Fatal("caller cancellation removed the valid cascade subscription")
	}
}

func TestCascadeEventNotifyResponseUpdatesSubscriptionLifecycle(t *testing.T) {
	tests := []struct {
		name          string
		version       GBProtocolVersion
		status        int
		retryAfter    string
		wantErr       bool
		wantRemoved   bool
		wantCancelled int
	}{
		{name: "accepted 202", version: GBVersion11, status: http.StatusAccepted},
		{name: "2016 dialog missing 481", version: GBVersion20, status: 481, wantErr: true, wantRemoved: true, wantCancelled: 1},
		{name: "2016 server error 500", version: GBVersion20, status: http.StatusInternalServerError, wantErr: true, wantRemoved: true, wantCancelled: 1},
		{name: "2016 service unavailable with retry", version: GBVersion20, status: http.StatusServiceUnavailable, retryAfter: "30", wantErr: true},
		{name: "2016 proxy authentication required", version: GBVersion20, status: http.StatusProxyAuthRequired, wantErr: true},
		{name: "2022 bad event 489", version: GBVersion30, status: 489, wantErr: true, wantRemoved: true, wantCancelled: 1},
		{name: "2022 server error 500", version: GBVersion30, status: http.StatusInternalServerError, wantErr: true},
		{name: "2022 service unavailable 503", version: GBVersion30, status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
			remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
			callID := sip.CallID("cascade-notify-response-" + tt.name)
			request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
			response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
			worker := newCascadeWorker(&Server{}, platform)
			worker.exchange = func(_ context.Context, actual *sip.Request) (*sip.Response, error) {
				response := sip.NewResponseFromRequest("", actual, tt.status, http.StatusText(tt.status), nil)
				if tt.retryAfter != "" {
					response.AppendHeader(&sip.GenericHeader{HeaderName: "Retry-After", Contents: tt.retryAfter})
				}
				return response, nil
			}
			const downstreamKey = "notify-response-downstream"
			sub := &eventSubscription{
				Key: "cascade-notify-response-" + tt.name, CmdType: "Alarm", DeviceID: platform.localID,
				ExpiresAt: time.Now().Add(time.Minute), To: remote, GBVersion: string(tt.version),
				Event: "presence", DialogRequest: request, Response: response, Cascade: worker,
				DownstreamKeys: []string{downstreamKey},
			}
			wantExpiresAt := sub.ExpiresAt
			wantDialogRequest := sub.DialogRequest
			wantResponse := sub.Response
			cancelled := 0
			api := &GB28181API{
				cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
					downstreamKey: {Input: SubscribeInput{DeviceID: "downstream-device", Event: "Alarm"}, Refs: 1},
				},
				cascadeSubscribe: func(_ context.Context, input *SubscribeInput) error {
					if input == nil || !input.Cancel || input.Expires != 0 {
						t.Fatalf("downstream cancellation = %+v", input)
					}
					cancelled++
					return nil
				},
			}
			api.eventSubscribers.Store(sub.Key, sub)

			err := api.sendCascadeEventNotifyContext(t.Context(), sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
			if (err != nil) != tt.wantErr {
				t.Fatalf("NOTIFY error = %v, wantErr %v", err, tt.wantErr)
			}
			actual, exists := api.eventSubscribers.Load(sub.Key)
			if tt.wantRemoved {
				if exists {
					t.Fatalf("failed NOTIFY retained subscription: %T", actual)
				}
				if _, exists := api.cascadeSubscriptions[downstreamKey]; exists {
					t.Fatal("failed NOTIFY retained downstream subscription")
				}
			} else if !exists || actual != sub {
				t.Fatal("NOTIFY response removed a subscription that must be retained")
			} else if !sub.ExpiresAt.Equal(wantExpiresAt) || sub.DialogRequest != wantDialogRequest || sub.Response != wantResponse || len(sub.DownstreamKeys) != 1 || sub.DownstreamKeys[0] != downstreamKey {
				t.Fatal("retained cascade subscription state changed after NOTIFY response")
			}
			if cancelled != tt.wantCancelled {
				t.Fatalf("downstream cancellation count = %d, want %d", cancelled, tt.wantCancelled)
			}
		})
	}
}

func TestCascadeEventNotifyRetriesDigestChallengeFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, challenge := range []struct {
			name            string
			status          int
			challengeHeader string
			authorizeHeader string
		}{
			{name: "www", status: http.StatusUnauthorized, challengeHeader: "WWW-Authenticate", authorizeHeader: "Authorization"},
			{name: "proxy", status: http.StatusProxyAuthRequired, challengeHeader: "Proxy-Authenticate", authorizeHeader: "Proxy-Authorization"},
		} {
			t.Run(string(version)+"/"+challenge.name, func(t *testing.T) {
				platform := testSharedCascadePlatform(t)
				platform.password = "notify-secret"
				local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
				remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
				callID := sip.CallID("cascade-notify-digest-" + string(version) + "-" + challenge.name)
				subscribe := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
					sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
						AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
				response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
				worker := newCascadeWorker(&Server{}, platform)
				requests := make([]*sip.Request, 0, 2)
				worker.exchange = func(_ context.Context, actual *sip.Request) (*sip.Response, error) {
					requests = append(requests, actual)
					if len(requests) == 1 {
						challengeResponse := sip.NewResponseFromRequest("", actual, challenge.status, http.StatusText(challenge.status), nil)
						challengeResponse.AppendHeader(&sip.GenericHeader{
							HeaderName: challenge.challengeHeader,
							Contents:   `Digest realm="remote.example",nonce="notify-nonce",algorithm=MD5,qop="auth"`,
						})
						return challengeResponse, nil
					}
					if len(actual.GetHeaders(challenge.authorizeHeader)) != 1 {
						t.Fatalf("authenticated NOTIFY %s headers = %v", challenge.authorizeHeader, actual.GetHeaders(challenge.authorizeHeader))
					}
					auth := sip.AuthFromValue(actual.GetHeaders(challenge.authorizeHeader)[0].String())
					if auth.Get("username") != platform.localID || auth.Get("uri") != actual.Recipient().String() || auth.Get("response") == "" {
						t.Fatalf("authenticated NOTIFY credentials = %s", actual.GetHeaders(challenge.authorizeHeader)[0].String())
					}
					return sip.NewResponseFromRequest("", actual, http.StatusOK, "OK", nil), nil
				}
				sub := &eventSubscription{
					CmdType: "Alarm", DeviceID: platform.localID, ExpiresAt: time.Now().Add(time.Minute),
					To: remote, GBVersion: string(version), Event: "presence",
					DialogRequest: subscribe, Response: response, Cascade: worker,
				}

				if _, err := (&GB28181API{}).sendCascadeEventNotifyRequestContext(t.Context(), sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`)); err != nil {
					t.Fatalf("Digest challenged NOTIFY failed: %v", err)
				}
				if len(requests) != 2 {
					t.Fatalf("NOTIFY request count = %d, want 2", len(requests))
				}
				firstCSeq, _ := requests[0].CSeq()
				secondCSeq, _ := requests[1].CSeq()
				if firstCSeq == nil || secondCSeq == nil || secondCSeq.SeqNo != firstCSeq.SeqNo+1 {
					t.Fatalf("NOTIFY CSeq retry = %v -> %v, want increment by one", firstCSeq, secondCSeq)
				}
				firstCallID, _ := requests[0].CallID()
				secondCallID, _ := requests[1].CallID()
				if normalizeCallID(firstCallID) == "" || normalizeCallID(firstCallID) != normalizeCallID(secondCallID) {
					t.Fatalf("NOTIFY retry Call-ID changed: %v -> %v", firstCallID, secondCallID)
				}
				if string(requests[1].Body()) != string(requests[0].Body()) ||
					firstSingleHeaderValue(requests[1], "Event") != firstSingleHeaderValue(requests[0], "Event") ||
					firstSingleHeaderValue(requests[1], "Subscription-State") != firstSingleHeaderValue(requests[0], "Subscription-State") {
					t.Fatal("authenticated NOTIFY changed event payload or subscription state")
				}
			})
		}
	}
}

func TestCascadeEventNotifyDigestChallengeRetriesOnlyOnce(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.password = "notify-secret"
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-notify-repeated-digest")
	subscribe := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
	worker := newCascadeWorker(&Server{}, platform)
	calls := 0
	worker.exchange = func(_ context.Context, actual *sip.Request) (*sip.Response, error) {
		calls++
		challenge := sip.NewResponseFromRequest("", actual, http.StatusUnauthorized, "Unauthorized", nil)
		challenge.AppendHeader(&sip.GenericHeader{
			HeaderName: "WWW-Authenticate",
			Contents:   fmt.Sprintf(`Digest realm="remote.example",nonce="notify-nonce-%d",qop="auth"`, calls),
		})
		return challenge, nil
	}
	sub := &eventSubscription{
		Key: "cascade-notify-repeated-digest", CmdType: "Alarm", DeviceID: platform.localID,
		ExpiresAt: time.Now().Add(time.Minute), To: remote, GBVersion: string(GBVersion30),
		Event: "presence", DialogRequest: subscribe, Response: response, Cascade: worker,
	}
	api := &GB28181API{}
	api.eventSubscribers.Store(sub.Key, sub)

	if err := api.sendCascadeEventNotifyContext(t.Context(), sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`)); err == nil {
		t.Fatal("repeated Digest challenge unexpectedly succeeded")
	}
	if calls != 2 {
		t.Fatalf("repeated Digest challenge request count = %d, want 2", calls)
	}
	if actual, exists := api.eventSubscribers.Load(sub.Key); !exists || actual != sub {
		t.Fatal("repeated Digest challenge removed retained subscription")
	}
}

func TestCascadeEventNotifyRejectsAmbiguousDigestChallengeFourVersionMatrix(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, challenge := range []struct {
			name  string
			value string
		}{
			{name: "missing-scheme", value: `realm="remote.example",nonce="notify-nonce"`},
			{name: "duplicate-realm", value: `Digest realm="first.example",realm="second.example",nonce="notify-nonce"`},
			{name: "duplicate-nonce", value: `Digest realm="remote.example",nonce="first",nonce="second"`},
			{name: "duplicate-algorithm", value: `Digest realm="remote.example",nonce="notify-nonce",algorithm=MD5,algorithm=SHA-256`},
			{name: "duplicate-qop", value: `Digest realm="remote.example",nonce="notify-nonce",qop="auth",qop="auth"`},
		} {
			t.Run(string(version)+"/"+challenge.name, func(t *testing.T) {
				platform := testSharedCascadePlatform(t)
				platform.password = "notify-secret"
				worker := newCascadeWorker(&Server{}, platform)
				sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", platform.localID)
				sub.GBVersion = string(version)
				calls := 0
				worker.exchange = func(_ context.Context, actual *sip.Request) (*sip.Response, error) {
					calls++
					if calls == 1 {
						response := sip.NewResponseFromRequest("", actual, http.StatusUnauthorized, "Unauthorized", nil)
						response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: challenge.value})
						return response, nil
					}
					return sip.NewResponseFromRequest("", actual, http.StatusOK, "OK", nil), nil
				}

				if err := (&GB28181API{}).sendCascadeEventNotify(sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`)); err == nil {
					t.Fatal("ambiguous Digest challenge unexpectedly succeeded")
				}
				if calls != 1 {
					t.Fatalf("ambiguous Digest challenge request count = %d, want 1", calls)
				}
			})
		}
	}
}

func TestFailedCascadeEventNotifyDoesNotDeleteReplacementSubscription(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-notify-replaced")
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	response := sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
	worker := newCascadeWorker(&Server{}, platform)
	worker.exchange = func(_ context.Context, actual *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", actual, 481, "Call/Transaction Does Not Exist", nil), nil
	}
	oldSub := &eventSubscription{
		Key: "cascade-notify-replaced", CmdType: "Alarm", DeviceID: platform.localID,
		ExpiresAt: time.Now().Add(time.Minute), To: remote, GBVersion: string(GBVersion30),
		Event: "presence", DialogRequest: request, Response: response, Cascade: worker,
	}
	replacement := &eventSubscription{Key: oldSub.Key, ExpiresAt: time.Now().Add(time.Minute)}
	api := &GB28181API{}
	api.eventSubscribers.Store(oldSub.Key, replacement)

	if err := api.sendCascadeEventNotifyContext(t.Context(), oldSub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`)); err == nil {
		t.Fatal("481 NOTIFY response unexpectedly succeeded")
	}
	if actual, exists := api.eventSubscribers.Load(oldSub.Key); !exists || actual != replacement {
		t.Fatal("old NOTIFY failure deleted the replacement subscription")
	}
}

func TestCascadeEventNotifyPreservesTCPDialogRouting(t *testing.T) {
	platform := testCascadeTCPPlatform(t, "3.0")
	worker := newCascadeWorker(nil, platform)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	remote.Params.Add("tag", sip.String{Str: "remote-tag"})
	contact := mustFlowAddress(t, "sip:"+platform.serverID+"@contact.example:5070;transport=tcp")
	callID := sip.CallID("cascade-tcp-notify-dialog")
	subscribe := sip.NewRequest("", sip.MethodSubscribe, local.URI.Clone(), sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetContact(contact).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "remote.example", Port: sip.NewPort(5060), Transport: "TCP", Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	proxy, _ := sip.ParseURI("sip:proxy.example;lr;transport=tcp")
	subscribe.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{proxy}})
	response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
	sub := &eventSubscription{
		CmdType: "Alarm", DeviceID: platform.localID, ExpiresAt: time.Now().Add(time.Minute),
		To: contact, GBVersion: string(GBVersion30), Event: "presence",
		DialogRequest: subscribe, Response: response, Cascade: worker,
	}
	var request *sip.Request
	worker.exchange = func(_ context.Context, actual *sip.Request) (*sip.Response, error) {
		request = actual
		return sip.NewResponseFromRequest("", actual, http.StatusOK, "OK", nil), nil
	}
	if err := (&GB28181API{}).sendCascadeEventNotify(sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`)); err != nil {
		t.Fatal(err)
	}
	if request == nil || request.Recipient().Host() != "contact.example" {
		t.Fatalf("cascade TCP NOTIFY target = %v", request)
	}
	via, _ := request.ViaHop()
	if via == nil || !strings.EqualFold(via.Transport, "TCP") {
		t.Fatalf("cascade NOTIFY Via = %v", via)
	}
	route, ok := request.GetHeaders("Route")[0].(*sip.RouteHeader)
	if !ok || len(route.Addresses) != 1 || route.Addresses[0].Host() != "proxy.example" {
		t.Fatalf("cascade NOTIFY Route = %#v", request.GetHeaders("Route"))
	}
	requestCallID, _ := request.CallID()
	cseq, _ := request.CSeq()
	if requestCallID == nil || *requestCallID != callID || cseq == nil || cseq.SeqNo != 1 || cseq.MethodName != sip.MethodNotify {
		t.Fatalf("cascade NOTIFY dialog = Call-ID %v, CSeq %+v", requestCallID, cseq)
	}
}

func TestCascadeDownstreamSubscriptionDoesNotBlockUnrelatedKey(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.TargetID == "target-1" && !input.Cancel {
			close(firstStarted)
			<-releaseFirst
		}
		return nil
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{
			"key-1": {DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60},
		})
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first downstream subscription did not start")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{
			"key-2": {DeviceID: "device-2", TargetID: "target-2", Event: "Catalog", Expires: 60},
		})
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("unrelated downstream subscription failed: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		close(releaseFirst)
		<-firstDone
		<-secondDone
		t.Fatal("unrelated downstream subscription waited for global subscription lock")
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first downstream subscription failed: %v", err)
	}
	api.cascadeSubscriptionOpMu.Lock()
	operationCount := len(api.cascadeSubscriptionOps)
	api.cascadeSubscriptionOpMu.Unlock()
	if operationCount != 0 {
		t.Fatalf("released cascade subscription operations retained %d entries", operationCount)
	}
}

func TestCascadeDownstreamSubscriptionFailedRefreshDoesNotCommitLongerExpiry(t *testing.T) {
	const key = "failed-refresh-expiry"
	original := SubscribeInput{DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60}
	api := &GB28181API{
		cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
			key: {Input: original, Refs: 1},
		},
		cascadeSubscribe: func(context.Context, *SubscribeInput) error {
			return errors.New("downstream refresh failed")
		},
	}
	renewed := original
	renewed.Expires = 600
	_, err := api.syncCascadeDownstreamSubscriptions(t.Context(), []string{key}, map[string]SubscribeInput{key: renewed})
	if err == nil {
		t.Fatal("failed downstream refresh unexpectedly succeeded")
	}
	api.cascadeSubscriptionMu.Lock()
	state := api.cascadeSubscriptions[key]
	api.cascadeSubscriptionMu.Unlock()
	if state == nil || state.Refs != 1 || state.Input != original {
		t.Fatalf("failed downstream refresh committed state: %+v", state)
	}
}

func TestCascadeDownstreamSuccessfulRefreshDoesNotClearConcurrentTermination(t *testing.T) {
	const key = "refresh-concurrent-termination"
	input := SubscribeInput{DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60}
	state := &cascadeDownstreamSubscription{Input: input, Refs: 1}
	dialog := &outgoingSubscriptionDialog{}
	api := &GB28181API{
		cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{key: state},
	}
	api.outgoingSubscriptions.Store(key, dialog)
	terminatedAt := time.Now()
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
		if !api.terminateOutgoingSubscription(key, dialog, false, 0, terminatedAt) {
			t.Fatal("concurrent terminated NOTIFY did not remove the outgoing dialog")
		}
		return nil
	}
	if _, err := api.syncCascadeDownstreamSubscriptions(t.Context(), []string{key}, map[string]SubscribeInput{key: input}); err != nil {
		t.Fatal(err)
	}
	api.cascadeSubscriptionMu.Lock()
	retryBlocked := state.RetryBlocked
	retryAt := state.RetryAt
	api.cascadeSubscriptionMu.Unlock()
	if !retryBlocked || !retryAt.IsZero() {
		t.Fatalf("successful refresh cleared concurrent termination policy: blocked=%v retryAt=%v", retryBlocked, retryAt)
	}
}

func TestCascadeDownstreamSubscriptionReleaseHonorsCallerCancellationWhileWaitingForLock(t *testing.T) {
	const key = "release-cancelled-while-waiting"
	api := &GB28181API{
		cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
			key: {Input: SubscribeInput{DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60}, Refs: 1},
		},
	}
	cancelCalls := 0
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input != nil && input.Cancel {
			cancelCalls++
		}
		return nil
	}

	unlock, err := api.lockCascadeSubscriptionOperation(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	releaseDone := make(chan struct{})
	go func() {
		api.releaseCascadeDownstreamSubscription(ctx, key)
		close(releaseDone)
	}()
	select {
	case <-releaseDone:
	case <-time.After(200 * time.Millisecond):
		unlock()
		<-releaseDone
		t.Fatal("cancelled downstream subscription release kept waiting for the operation lock")
	}

	api.cascadeSubscriptionMu.Lock()
	state := api.cascadeSubscriptions[key]
	api.cascadeSubscriptionMu.Unlock()
	if state == nil || state.Refs != 1 {
		t.Fatalf("cancelled release changed downstream state: %+v", state)
	}
	if cancelCalls != 0 {
		t.Fatalf("cancelled release sent %d downstream cancellations", cancelCalls)
	}

	unlock()
	api.cascadeSubscriptionOpMu.Lock()
	operationCount := len(api.cascadeSubscriptionOps)
	api.cascadeSubscriptionOpMu.Unlock()
	if operationCount != 0 {
		t.Fatalf("cancelled release retained %d operation lock entries", operationCount)
	}
}

func TestCascadeDownstreamSubscriptionRollbackDetachesCallerCancellation(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	ctx, cancel := context.WithCancel(withMonitorUserIdentityRoute(
		t.Context(), cascadeDownstreamTestIdentity(), testLocalGatewayID,
	))
	defer cancel()
	cancelContextErr := make(chan error, 1)
	api.cascadeSubscribe = func(callCtx context.Context, input *SubscribeInput) error {
		switch {
		case input.Cancel:
			cancelContextErr <- callCtx.Err()
			return callCtx.Err()
		case input.TargetID == "target-2":
			cancel()
			return context.Canceled
		default:
			return nil
		}
	}

	_, err := api.syncCascadeDownstreamSubscriptions(ctx, nil, map[string]SubscribeInput{
		"key-1": {DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60},
		"key-2": {DeviceID: "device-2", TargetID: "target-2", Event: "Catalog", Expires: 60},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("downstream subscription failure = %v, want context cancellation", err)
	}
	if err := <-cancelContextErr; err != nil {
		t.Fatalf("rollback cancellation inherited caller error: %v", err)
	}
	api.cascadeSubscriptionMu.Lock()
	remaining := len(api.cascadeSubscriptions)
	api.cascadeSubscriptionMu.Unlock()
	if remaining != 0 {
		t.Fatalf("rollback retained %d downstream subscriptions", remaining)
	}
}

func TestEventSubscriptionOperationsSerializePerKeyWithoutCrossKeyBlocking(t *testing.T) {
	api := &GB28181API{}
	unlockFirst, err := api.lockEventSubscriptionOperation(context.Background(), "event-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = api.lockEventSubscriptionOperation(ctx, "event-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same event subscription lock error = %v; want %v", err, context.DeadlineExceeded)
	}
	otherCtx, otherCancel := context.WithTimeout(context.Background(), time.Second)
	defer otherCancel()
	unlockSecond, err := api.lockEventSubscriptionOperation(otherCtx, "event-2")
	if err != nil {
		t.Fatalf("unrelated event subscription was blocked: %v", err)
	}
	unlockSecond()
	unlockFirst()

	api.eventSubscriptionMu.Lock()
	count := len(api.eventSubscriptionOps)
	api.eventSubscriptionMu.Unlock()
	if count != 0 {
		t.Fatalf("released event subscription operations retained %d entries", count)
	}
}

func TestEventSubscriptionCleanupStopsWhileWaitingForLock(t *testing.T) {
	const (
		key           = "event-cleanup-cancelled"
		downstreamKey = "event-cleanup-downstream"
	)
	api := &GB28181API{
		cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
			downstreamKey: {Input: SubscribeInput{DeviceID: "device-1", Event: "Catalog"}, Refs: 1},
		},
	}
	sub := &eventSubscription{
		Key: key, ExpiresAt: time.Now().Add(-time.Second), DownstreamKeys: []string{downstreamKey},
	}
	api.eventSubscribers.Store(key, sub)
	unlock, err := api.lockEventSubscriptionOperation(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		api.cleanupEventSubscriptionsContext(ctx, time.Now())
		close(done)
	}()
	waitForEventSubscriptionLockWaiter(t, api, key)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled event subscription cleanup remained blocked on the operation lock")
	}
	if actual, exists := api.eventSubscribers.Load(key); !exists || actual != sub {
		t.Fatal("cancelled cleanup removed the event subscription")
	}
	api.cascadeSubscriptionMu.Lock()
	state := api.cascadeSubscriptions[downstreamKey]
	api.cascadeSubscriptionMu.Unlock()
	if state == nil || state.Refs != 1 {
		t.Fatalf("cancelled cleanup changed downstream reference state: %+v", state)
	}
}

func TestFailedCascadeEventNotifyStopsWhileWaitingForSubscriptionLock(t *testing.T) {
	const (
		key           = "cascade-notify-detach-cancelled"
		downstreamKey = "cascade-notify-detach-downstream"
	)
	worker := newCascadeWorker(&Server{}, testSharedCascadePlatform(t))
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		return sip.NewResponseFromRequest("", request, 481, "Call/Transaction Does Not Exist", nil), nil
	}
	sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
	sub.Key = key
	sub.DownstreamKeys = []string{downstreamKey}
	wantExpiresAt := sub.ExpiresAt
	wantDialogRequest := sub.DialogRequest
	wantResponse := sub.Response
	cancelled := 0
	api := &GB28181API{
		cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
			downstreamKey: {Input: SubscribeInput{DeviceID: "device-1", Event: "Alarm"}, Refs: 1},
		},
		cascadeSubscribe: func(_ context.Context, input *SubscribeInput) error {
			if input != nil && input.Cancel {
				cancelled++
			}
			return nil
		},
	}
	api.eventSubscribers.Store(key, sub)
	unlock, err := api.lockEventSubscriptionOperation(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- api.sendCascadeEventNotifyContext(ctx, sub, "Alarm", []byte(`<Notify><CmdType>Alarm</CmdType></Notify>`))
	}()
	waitForEventSubscriptionLockWaiter(t, api, key)
	cancel()
	select {
	case err = <-done:
		if err == nil {
			t.Fatal("481 NOTIFY response unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled NOTIFY failure cleanup remained blocked on the subscription lock")
	}
	if actual, exists := api.eventSubscribers.Load(key); !exists || actual != sub {
		t.Fatal("cancelled NOTIFY failure cleanup removed the subscription")
	}
	if !sub.ExpiresAt.Equal(wantExpiresAt) || sub.DialogRequest != wantDialogRequest || sub.Response != wantResponse || len(sub.DownstreamKeys) != 1 || sub.DownstreamKeys[0] != downstreamKey {
		t.Fatal("cancelled NOTIFY failure cleanup changed the subscription dialog")
	}
	api.cascadeSubscriptionMu.Lock()
	state := api.cascadeSubscriptions[downstreamKey]
	api.cascadeSubscriptionMu.Unlock()
	if state == nil || state.Refs != 1 || cancelled != 0 {
		t.Fatalf("cancelled NOTIFY failure cleanup changed downstream state: state=%+v cancellations=%d", state, cancelled)
	}
}

func TestRemovingCascadeEventSubscriptionsStopsWhileWaitingForLock(t *testing.T) {
	const (
		key           = "cascade-removal-cancelled"
		downstreamKey = "cascade-removal-downstream"
	)
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	api := &GB28181API{
		cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
			downstreamKey: {Input: SubscribeInput{DeviceID: "device-1", Event: "Alarm"}, Refs: 1},
		},
	}
	sub := &eventSubscription{
		Key: key, Cascade: worker, ExpiresAt: time.Now().Add(time.Minute), DownstreamKeys: []string{downstreamKey},
	}
	api.eventSubscribers.Store(key, sub)
	unlock, err := api.lockEventSubscriptionOperation(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		api.removeCascadeEventSubscriptionsContext(ctx, worker)
		close(done)
	}()
	waitForEventSubscriptionLockWaiter(t, api, key)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled cascade subscription removal remained blocked on the event lock")
	}
	if actual, exists := api.eventSubscribers.Load(key); !exists || actual != sub {
		t.Fatal("cancelled cascade removal deleted the event subscription")
	}
	api.cascadeSubscriptionMu.Lock()
	state := api.cascadeSubscriptions[downstreamKey]
	api.cascadeSubscriptionMu.Unlock()
	if state == nil || state.Refs != 1 {
		t.Fatalf("cancelled cascade removal changed downstream reference state: %+v", state)
	}
}

func waitForEventSubscriptionLockWaiter(t *testing.T, api *GB28181API, key string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		api.eventSubscriptionMu.Lock()
		entry := api.eventSubscriptionOps[key]
		refs := 0
		if entry != nil {
			refs = entry.refs
		}
		api.eventSubscriptionMu.Unlock()
		if refs >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("event subscription operation did not begin waiting for the lock")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClosingCascadeSubscriptionsWaitsForPendingCreate(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	createStarted := make(chan struct{})
	releaseCreate := make(chan struct{})
	cancelled := make(chan struct{})
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.Cancel {
			close(cancelled)
			return nil
		}
		close(createStarted)
		<-releaseCreate
		return nil
	}

	createDone := make(chan error, 1)
	go func() {
		_, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{
			"pending-key": {DeviceID: "device-1", TargetID: "target-1", Event: "Alarm", Expires: 60},
		})
		createDone <- err
	}()
	select {
	case <-createStarted:
	case <-time.After(time.Second):
		t.Fatal("pending downstream subscription did not start")
	}

	closeDone := make(chan struct{})
	go func() {
		api.closeCascadeDownstreamSubscriptions()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("cascade subscription close returned before pending create settled")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseCreate)
	if err := <-createDone; err != nil {
		t.Fatalf("pending downstream subscription failed: %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("cascade subscription close did not finish")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("pending downstream subscription was not cancelled after creation")
	}
	api.cascadeSubscriptionMu.Lock()
	remaining := len(api.cascadeSubscriptions)
	api.cascadeSubscriptionMu.Unlock()
	if remaining != 0 {
		t.Fatalf("cascade subscriptions retained %d entries after close", remaining)
	}
}

func TestAlarmSubscriptionFilterValidationAndMatch(t *testing.T) {
	valid := subscribeEventRequest{
		CmdType: "Alarm", SN: 1, DeviceID: gb10DeviceID,
		StartAlarmPriority: "1", EndAlarmPriority: "3", AlarmMethod: "2/5",
		AlarmType: "2", StartAlarmTime: "2026-08-25T08:00:00", EndAlarmTime: "2026-08-25T09:00:00",
	}
	if err := validateSubscribeEventRequest(valid, "Alarm", GBVersion30); err != nil {
		t.Fatalf("valid Alarm subscription rejected: %v", err)
	}
	filter := subscriptionFilterFromRequest(valid)
	if filter.AlarmMethod != "25" {
		t.Fatalf("normalized AlarmMethod = %q", filter.AlarmMethod)
	}
	matching := []byte(`<Notify><CmdType>Alarm</CmdType><SN>2</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>2</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmType>2</AlarmType><AlarmTime>2026-08-25T08:30:00</AlarmTime></Notify>`)
	if !alarmMatchesSubscription(filter, matching) {
		t.Fatal("matching Alarm event was filtered")
	}
	if alarmMatchesSubscription(filter, []byte(strings.ReplaceAll(string(matching), "<AlarmPriority>2</AlarmPriority>", "<AlarmPriority>4</AlarmPriority>"))) {
		t.Fatal("out-of-range Alarm priority was forwarded")
	}
	if alarmMatchesSubscription(filter, []byte(strings.ReplaceAll(string(matching), "<AlarmMethod>5</AlarmMethod>", "<AlarmMethod>6</AlarmMethod>"))) {
		t.Fatal("non-matching Alarm method was forwarded")
	}
	if err := validateSubscribeEventRequest(valid, "Alarm", GBVersion20); err != nil {
		t.Fatalf("2.0 AlarmType subscription rejected: %v", err)
	}
	if err := validateSubscribeEventRequest(valid, "Alarm", GBVersion11); err == nil || !strings.Contains(err.Error(), "2016") {
		t.Fatalf("1.1 AlarmType validation error = %v", err)
	}
	invalidRange := valid
	invalidRange.StartAlarmPriority, invalidRange.EndAlarmPriority = "4", "1"
	if err := validateSubscribeEventRequest(invalidRange, "Alarm", GBVersion30); err == nil {
		t.Fatal("invalid Alarm priority range was accepted")
	}
	for _, method := range []string{"/2", "2/", "2//5", "25/7", "2/57", "2/2", "0/2", "8"} {
		invalid := valid
		invalid.AlarmMethod = method
		if err := validateSubscribeEventRequest(invalid, "Alarm", GBVersion30); err == nil {
			t.Fatalf("invalid AlarmMethod %q accepted", method)
		}
	}
	invalidType := valid
	invalidType.AlarmMethod, invalidType.AlarmType = "6", "3"
	if err := validateSubscribeEventRequest(invalidType, "Alarm", GBVersion30); err == nil {
		t.Fatal("invalid AlarmType accepted for AlarmMethod 6")
	}
}

func TestSubscriptionRequestFieldsMatchEventType(t *testing.T) {
	start := "2026-08-25T08:00:00"
	end := "2026-08-25T09:00:00"
	if err := validateSubscribeEventRequest(subscribeEventRequest{
		CmdType: "Catalog", SN: 1, DeviceID: gb10DeviceID, StartTime: start, EndTime: end,
	}, "Catalog", GBVersion10); err != nil {
		t.Fatalf("valid Catalog time range rejected: %v", err)
	}
	if err := validateSubscribeEventRequest(subscribeEventRequest{
		CmdType: "Alarm", SN: 1, DeviceID: gb10DeviceID, StartTime: start, EndTime: end,
	}, "Alarm", GBVersion10); err != nil {
		t.Fatalf("2011 Alarm StartTime compatibility rejected: %v", err)
	}

	interval := 5
	tests := []struct {
		name    string
		cmdType string
		version GBProtocolVersion
		request subscribeEventRequest
	}{
		{name: "Catalog reversed range", cmdType: "Catalog", version: GBVersion10, request: subscribeEventRequest{StartTime: end, EndTime: start}},
		{name: "Catalog alarm field", cmdType: "Catalog", version: GBVersion10, request: subscribeEventRequest{AlarmMethod: "2"}},
		{name: "Catalog interval", cmdType: "Catalog", version: GBVersion10, request: subscribeEventRequest{Interval: &interval}},
		{name: "Alarm interval", cmdType: "Alarm", version: GBVersion20, request: subscribeEventRequest{Interval: &interval}},
		{name: "Alarm conflicting time", cmdType: "Alarm", version: GBVersion10, request: subscribeEventRequest{StartAlarmTime: start, StartTime: end}},
		{name: "MobilePosition alarm field", cmdType: "MobilePosition", version: GBVersion20, request: subscribeEventRequest{AlarmMethod: "2"}},
		{name: "MobilePosition Catalog time", cmdType: "MobilePosition", version: GBVersion20, request: subscribeEventRequest{StartTime: start}},
		{name: "PTZPosition interval", cmdType: "PTZPosition", version: GBVersion30, request: subscribeEventRequest{Interval: &interval}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.request.CmdType = test.cmdType
			test.request.SN = 1
			test.request.DeviceID = gb10DeviceID
			if err := validateSubscribeEventRequest(test.request, test.cmdType, test.version); err == nil {
				t.Fatal("event-specific invalid fields were accepted")
			}
		})
	}

	input := copyCascadeSubscribeInput(subscribeEventRequest{
		StartTime: start, EndTime: end,
	}, gb10DeviceID, gb10DeviceID, "Catalog", 600)
	if input.StartTime != start || input.EndTime != end || input.StartAlarmTime != "" || input.Interval != 0 {
		t.Fatalf("Catalog subscription forwarding input = %+v", input)
	}
}

func TestAlarmMethodFilterFormatByVersion(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		input   string
		want    string
	}{
		{version: GBVersion10, input: "1/2", want: "12"},
		{version: GBVersion11, input: "12", want: "12"},
		{version: GBVersion20, input: "1/2", want: "12"},
		{version: GBVersion30, input: "12", want: "1/2"},
		{version: GBVersion30, input: "1/2", want: "1/2"},
		{version: GBVersion10, input: "52", want: "25"},
		{version: GBVersion30, input: "5/2", want: "2/5"},
	}
	for _, test := range tests {
		got, err := formatAlarmMethodFilter(test.version, test.input)
		if err != nil || got != test.want {
			t.Fatalf("formatAlarmMethodFilter(%s, %q) = %q, %v; want %q", test.version, test.input, got, err, test.want)
		}
	}
}

func TestOutgoingSubscriptionKeyCanonicalizesAlarmMethodSet(t *testing.T) {
	base := SubscribeInput{
		DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm",
		StartAlarmPriority: "1", EndAlarmPriority: "3", AlarmType: "2", Interval: 10,
	}
	keys := make(map[string]struct{})
	for _, method := range []string{"25", "52", "2/5", "5/2"} {
		input := base
		input.AlarmMethod = method
		keys[buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)] = struct{}{}
	}
	if len(keys) != 1 {
		t.Fatalf("equivalent AlarmMethod sets produced %d subscription keys: %v", len(keys), keys)
	}
}

func TestSubscribeAlarmMethodXMLByVersion(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		input   string
		want    string
	}{
		{version: GBVersion10, input: "2/5", want: "25"},
		{version: GBVersion11, input: "2/5", want: "25"},
		{version: GBVersion20, input: "2/5", want: "25"},
		{version: GBVersion30, input: "25", want: "2/5"},
	}
	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			baseConnection := newFlowConnection()
			connection := &tcpFlowConnection{flowConnection: baseConnection}
			platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
			device := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
			sipServer := sip.NewServer(platform)
			t.Cleanup(sipServer.Close)
			runtime := &Device{IsOnline: true, gbVersion: string(test.version)}
			runtime.UpdateRuntime(func(current *Device) {
				current.conn = connection
				current.source = baseConnection.remote
				current.to = device
			})
			memory := &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}
			server := &Server{Server: sipServer, fromAddress: *platform, memoryStorer: memory}
			api := &GB28181API{svr: server}
			server.gb = api

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- api.Subscribe(ctx, &SubscribeInput{
					DeviceID: gb10DeviceID, Event: "Alarm", Expires: 60,
					AlarmMethod: test.input,
				})
			}()

			select {
			case payload := <-baseConnection.writes:
				if body := string(payload); !strings.Contains(body, "<AlarmMethod>"+test.want+"</AlarmMethod>") {
					t.Fatalf("%s SUBSCRIBE AlarmMethod payload =\n%s", test.version, body)
				}
			case err := <-done:
				t.Fatalf("%s SUBSCRIBE ended before send: %v", test.version, err)
			case <-time.After(time.Second):
				t.Fatalf("%s SUBSCRIBE was not sent", test.version)
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("%s SUBSCRIBE cancellation error = %v", test.version, err)
			}
		})
	}
}

func TestCascadeAlarmSubscriptionBridgesRenewalAndCancel(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	connection := newFlowConnection()
	connection.remote = platform.remote
	runtimeDevice := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtimeChannel := &Channel{ChannelID: testCascadeChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{
		flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice},
		channel:    runtimeChannel,
	}
	server := &Server{Server: sip.NewServer(local), memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server, cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.mu.Unlock()

	calls := make([]SubscribeInput, 0, 3)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	body, err := sip.XMLEncode(subscribeEventRequest{
		CmdType: "Alarm", SN: 18, DeviceID: testExposedChannelID,
		StartAlarmPriority: "1", EndAlarmPriority: "3", AlarmMethod: "2/5", AlarmType: "2",
	})
	if err != nil {
		t.Fatal(err)
	}
	callID := sip.CallID("cascade-alarm-bridge")
	from := remote.Clone()
	from.Params.Add("tag", sip.String{Str: "upstream-tag"})
	makeRequest := func(expires string, cseq uint32, localTag string) *sip.Request {
		request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(from).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		applyInboundSubscribeDialog(t, request, localTag, cseq)
		return request
	}
	localTag := ""
	invoke := func(expires string, cseq uint32) {
		ctx := &sip.Context{
			Request: makeRequest(expires, cseq, localTag), Tx: sip.NewTransaction("cascade-alarm-bridge-"+expires, connection),
			DeviceID: platform.serverID, Source: connection.remote, To: remote, XGBVer: string(GBVersion30),
		}
		ctx.Set(cascadeWorkerContextKey, worker)
		api.sipSubscribeEvent(ctx)
		select {
		case response := <-connection.writes:
			if !strings.Contains(string(response), "SIP/2.0 200 OK") {
				t.Fatalf("SUBSCRIBE %s response = %s", expires, response)
			}
		case <-time.After(time.Second):
			t.Fatalf("SUBSCRIBE %s response timeout", expires)
		}
		if localTag == "" {
			api.eventSubscribers.Range(func(_, value any) bool {
				if subscription, ok := value.(*eventSubscription); ok && subscription != nil {
					localTag = subscription.LocalTag
				}
				return false
			})
			if localTag == "" {
				t.Fatal("initial subscription missing local dialog tag")
			}
		}
	}

	invoke("120", 1)
	invoke("240", 2)
	invoke("0", 3)
	if len(calls) != 3 {
		t.Fatalf("downstream subscription calls = %d; want 3", len(calls))
	}
	for index, call := range calls[:2] {
		if call.Cancel || call.DeviceID != gb10DeviceID || call.TargetID != gb10DeviceID || call.Event != "Alarm" || call.AlarmMethod != "25" || call.AlarmType != "2" {
			t.Fatalf("downstream call %d = %+v", index, call)
		}
	}
	if calls[0].Expires != 120 || calls[1].Expires != 240 || !calls[2].Cancel {
		t.Fatalf("downstream lifecycle = %+v", calls)
	}
	if len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("downstream references remain after cancel: %+v", api.cascadeSubscriptions)
	}
}

func TestCascadePositionSubscriptionTargetsSharedChannel(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	memory := &cascadeFlowMemory{flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtime}}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	worker := newCascadeWorker(api.svr, testSharedCascadePlatform(t))
	interval := 5
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "MobilePosition", SN: 1, DeviceID: testExposedChannelID, Interval: &interval,
	}, "MobilePosition", testExposedChannelID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("position downstream targets = %d", len(desired))
	}
	for _, input := range desired {
		if input.DeviceID != gb10DeviceID || input.TargetID != testCascadeChannelID || input.Interval != 5 {
			t.Fatalf("position downstream input = %+v", input)
		}
	}
}

func TestCascadePlatformPositionSubscriptionAggregatesByParent(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	secondID := "34020000001320000012"
	if err := adapter.Store().Channel().Create(t.Context(), &ipc.Channel{
		ID: "GBC_cascade_channel_2", DID: persistentDevice.ID, DeviceID: persistentDevice.DeviceID,
		ChannelID: secondID, Name: "Back Gate", Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	platform := testSharedCascadePlatform(t)
	platform.sharedChannels = append(platform.sharedChannels, secondID)
	platform.channelIDMap[secondID] = secondID
	platform.exposedChannelMap[secondID] = secondID
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}}}
	worker := newCascadeWorker(api.svr, platform)
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "PTZPosition", SN: 1, DeviceID: platform.localID,
	}, "PTZPosition", platform.localID, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("platform PTZ subscriptions = %d; want one parent subscription", len(desired))
	}
	for _, input := range desired {
		if input.TargetID != persistentDevice.DeviceID {
			t.Fatalf("platform PTZ target = %+v", input)
		}
	}
}

func TestCascadeCatalogSubscriptionBridgesDownstreamParent(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}},
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
	}
	worker := newCascadeWorker(api.svr, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "Catalog", SN: 1, DeviceID: platform.localID,
	}, "Catalog", platform.localID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("Catalog downstream subscriptions = %d; want one parent", len(desired))
	}
	var downstream SubscribeInput
	for _, input := range desired {
		downstream = input
	}
	if downstream.DeviceID != persistentDevice.DeviceID || downstream.TargetID != persistentDevice.DeviceID || downstream.Event != "Catalog" || downstream.Expires != 600 {
		t.Fatalf("Catalog downstream input = %+v", downstream)
	}
	calls := make([]SubscribeInput, 0, 2)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	keys, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), keys)
	if len(calls) != 2 || calls[0].Cancel || !calls[1].Cancel {
		t.Fatalf("Catalog downstream lifecycle = %+v", calls)
	}
}

func TestCascadeDownstreamSubscriptionRespectsDisabledDeviceCapabilityBeforeCustomHook(t *testing.T) {
	tests := []struct {
		name     string
		version  GBProtocolVersion
		event    string
		disabled string
	}{
		{name: "catalog", version: GBVersion11, event: "Catalog", disabled: "directory_notify"},
		{name: "mobile position", version: GBVersion20, event: "MobilePosition", disabled: "mobile_position"},
		{name: "PTZ position", version: GBVersion30, event: "PTZPosition", disabled: "ptz_position"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, persistentDevice, _ := newCascadeMediaCore(t)
			runtime := &Device{IsOnline: true}
			runtime.setGBProfile(test.version, []string{test.disabled})
			api := &GB28181API{
				core:                 adapter,
				svr:                  &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}},
				cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
			}
			worker := newCascadeWorker(api.svr, testSharedCascadePlatform(t))
			worker.effective = test.version
			t.Cleanup(worker.cancel)
			desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
				CmdType: test.event, SN: 1, DeviceID: worker.platform.localID,
			}, test.event, worker.platform.localID, 600)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
				calls++
				return nil
			}

			if _, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired); err == nil {
				t.Fatalf("disabled %s cascade subscription was accepted", test.disabled)
			}
			if calls != 0 {
				t.Fatalf("disabled %s reached custom subscription hook %d times", test.disabled, calls)
			}
			if len(api.cascadeSubscriptions) != 0 {
				t.Fatalf("disabled %s retained downstream subscription state", test.disabled)
			}
		})
	}
}

func TestCascadeDownstreamSubscriptionSyncRollsBackAfterSIPResponseFailure(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Catalog", Expires: 60}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	desired := map[string]SubscribeInput{key: input}
	var calls []SubscribeInput
	api.cascadeSubscribe = func(_ context.Context, request *SubscribeInput) error {
		calls = append(calls, *request)
		return nil
	}

	current, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	api.rollbackCascadeDownstreamSubscriptionSync(t.Context(), current, nil)
	if len(api.cascadeSubscriptions) != 0 || len(calls) != 2 || calls[0].Cancel || !calls[1].Cancel {
		t.Fatalf("new downstream rollback state=%+v calls=%+v", api.cascadeSubscriptions, calls)
	}

	calls = nil
	previous, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	previousSnapshot := api.snapshotCascadeDownstreamSubscriptions(previous)
	current, err = api.syncCascadeDownstreamSubscriptions(t.Context(), previous, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.rollbackCascadeDownstreamSubscriptionSync(t.Context(), current, previousSnapshot)
	state := api.cascadeSubscriptions[key]
	if state == nil || state.Refs != 1 || len(calls) != 3 || calls[0].Cancel || !calls[1].Cancel || calls[2].Cancel {
		t.Fatalf("removed downstream rollback state=%+v calls=%+v", state, calls)
	}
}

func TestCatalogSubscriptionEnvelopeAcceptsDirectoryTargets(t *testing.T) {
	tests := []struct {
		name     string
		deviceID string
		valid    bool
	}{
		{name: "province", deviceID: "65", valid: true},
		{name: "city", deviceID: "6501", valid: true},
		{name: "county", deviceID: "650102", valid: true},
		{name: "grassroots", deviceID: "65010211", valid: true},
		{name: "system", deviceID: "34020000002000000002", valid: true},
		{name: "business group", deviceID: "34020000002150000001", valid: true},
		{name: "virtual organization", deviceID: "34020000002160000001", valid: true},
		{name: "wildcard compatibility", deviceID: "*", valid: true},
		{name: "odd administrative length", deviceID: "65010"},
		{name: "non decimal", deviceID: "65010x"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSubscribeEventEnvelope(subscribeEventRequest{
				CmdType: "Catalog", SN: 1, DeviceID: test.deviceID,
			}, "Catalog")
			if (err == nil) != test.valid {
				t.Fatalf("Catalog subscription target %q error = %v", test.deviceID, err)
			}
		})
	}
	if err := validateSubscribeEventEnvelope(subscribeEventRequest{
		CmdType: "Alarm", SN: 1, DeviceID: "650102",
	}, "Alarm"); err == nil {
		t.Fatal("non-Catalog subscription accepted administrative target")
	}
}

func TestCatalogSubscriptionAdministrativeTargetVersionBoundary(t *testing.T) {
	request := subscribeEventRequest{CmdType: "Catalog", SN: 1, DeviceID: "650102"}
	for _, test := range []struct {
		version GBProtocolVersion
		wantOK  bool
	}{
		{version: GBVersion10},
		{version: GBVersion11, wantOK: true},
		{version: GBVersion20, wantOK: true},
		{version: GBVersion30, wantOK: true},
	} {
		t.Run(string(test.version), func(t *testing.T) {
			err := validateSubscribeEventRequest(request, "Catalog", test.version)
			if (err == nil) != test.wantOK {
				t.Fatalf("protocol %s administrative Catalog subscription error = %v, want success = %v", test.version, err, test.wantOK)
			}
		})
	}
}

func TestCascadeCatalogSubscriptionSupportsSharedChannelTarget(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}},
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
	}
	worker := newCascadeWorker(api.svr, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	if !cascadeSubscriptionTargetAllowed(platform, "Catalog", testExposedChannelID) {
		t.Fatal("shared channel Catalog subscription target was rejected")
	}
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, subscribeEventRequest{
		CmdType: "Catalog", SN: 1, DeviceID: testExposedChannelID,
	}, "Catalog", testExposedChannelID, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired) != 1 {
		t.Fatalf("shared channel Catalog subscriptions = %d; want one", len(desired))
	}
	for _, downstream := range desired {
		if downstream.DeviceID != persistentDevice.DeviceID || downstream.TargetID != testCascadeChannelID || downstream.Event != "Catalog" {
			t.Fatalf("shared channel Catalog downstream input = %+v", downstream)
		}
	}
}

func TestCascadeCatalogSubscriptionSupportsVisibleDirectoryTargets11And20(t *testing.T) {
	adapter, _, channel := newCascadeMediaCore(t)
	const groupID = "34020000002150000001"
	channel.Ext.GBCatalog = &ipc.GBCatalogExt{
		ParentID: groupID, BusinessGroupID: groupID, CivilCode: "340200",
	}
	if err := adapter.Store().Channel().EditGB28181Config(t.Context(), channel); err != nil {
		t.Fatal(err)
	}

	platform := testSharedCascadePlatform(t)
	api := &GB28181API{core: adapter}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})

	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20} {
		for _, test := range []struct {
			name     string
			targetID string
			status   string
		}{
			{name: "administrative", targetID: "340200", status: "SIP/2.0 200"},
			{name: "business-group", targetID: groupID, status: "SIP/2.0 200"},
			{name: "unshared", targetID: "34020000002160000099", status: "SIP/2.0 404"},
		} {
			t.Run(version.StandardYear()+"/"+test.name, func(t *testing.T) {
				worker := newCascadeWorker(nil, platform)
				worker.mu.Lock()
				worker.effective = version
				worker.mu.Unlock()

				connection := newFlowConnection()
				local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
				remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
				callID := sip.CallID("cascade-directory-" + version.StandardYear() + "-" + test.name)
				body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Catalog", SN: 33, DeviceID: test.targetID})
				if err != nil {
					t.Fatal(err)
				}
				request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
					sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
						AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=1894"})
				request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
				request.SetConnection(connection)
				request.SetSource(connection.remote)
				ctx := &sip.Context{
					Request: request, Tx: sip.NewTransaction(string(callID), connection),
					DeviceID: platform.serverID, Source: connection.remote, To: remote,
				}
				ctx.Set(cascadeWorkerContextKey, worker)
				api.sipSubscribeEvent(ctx)
				response := <-flowResponse(t, connection)
				if !strings.Contains(response, test.status) {
					t.Fatalf("%s directory target %s response:\n%s", version.StandardName(), test.targetID, response)
				}
			})
		}
	}
}

func TestCascadeSubscribeUsesNegotiatedVersionWhenHeaderMissing(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(nil, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	api := &GB28181API{}
	connection := newFlowConnection()
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-catalog-without-version-header")
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Catalog", SN: 31, DeviceID: platform.localID})
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=1894"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-catalog-without-version-header", connection),
		DeviceID: platform.serverID, Source: connection.remote, To: remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, connection)
	if !strings.Contains(response, "Event: Catalog;id=1894") {
		t.Fatalf("negotiated 1.1 response used wrong Event header:\n%s", response)
	}
	if !strings.Contains(response, "Content-Length: 0") || strings.Contains(response, "<Response>") {
		t.Fatalf("negotiated 1.1 Catalog response should have an empty body:\n%s", response)
	}
	var subscription *eventSubscription
	api.eventSubscribers.Range(func(_, value any) bool {
		subscription, _ = value.(*eventSubscription)
		return false
	})
	if subscription == nil || subscription.GBVersion != string(GBVersion11) {
		t.Fatalf("stored subscription version = %v; want %s", subscription, GBVersion11)
	}
}

func TestCascadeCatalogSubscribeRejectsLegacyEventFrom2014(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardYear(), func(t *testing.T) {
			platform := testSharedCascadePlatform(t)
			platform.version = version
			worker := newCascadeWorker(nil, platform)
			connection := newFlowConnection()
			local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
			remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
			callID := sip.CallID("cascade-catalog-event-" + version.StandardYear())
			body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Catalog", SN: 32, DeviceID: platform.localID})
			if err != nil {
				t.Fatal(err)
			}
			request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
				sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
					AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
			request.SetConnection(connection)
			request.SetSource(connection.remote)
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("cascade-catalog-event-"+version.StandardYear(), connection),
				DeviceID: platform.serverID, Source: connection.remote, To: remote,
			}
			ctx.Set(cascadeWorkerContextKey, worker)
			api := &GB28181API{}
			api.sipSubscribeEvent(ctx)
			response := <-flowResponse(t, connection)
			if version == GBVersion10 {
				assertFlowOK(t, response)
				return
			}
			if !strings.Contains(response, "SIP/2.0 400") || !strings.Contains(response, "Catalog;id=num") {
				t.Fatalf("legacy Event response:\n%s", response)
			}
			hasSubscription := false
			api.eventSubscribers.Range(func(_, _ any) bool {
				hasSubscription = true
				return false
			})
			if hasSubscription {
				t.Fatal("invalid interdomain Event created subscription state")
			}
		})
	}
}

func TestCascadeCatalogSubscribeRejectsInterdomainEventBefore2014(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	platform.version = GBVersion10
	worker := newCascadeWorker(nil, platform)
	connection := newFlowConnection()
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	callID := sip.CallID("cascade-catalog-2011-interdomain-event")
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Catalog", SN: 33, DeviceID: platform.localID})
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(remote).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=1894"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction(string(callID), connection),
		DeviceID: platform.serverID, Source: connection.remote, To: remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api := &GB28181API{}
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, connection)
	if !strings.Contains(response, "SIP/2.0 400") || !strings.Contains(response, "2011 Catalog Event must use presence") {
		t.Fatalf("2011 interdomain Event response:\n%s", response)
	}
	hasSubscription := false
	api.eventSubscribers.Range(func(_, _ any) bool {
		hasSubscription = true
		return false
	})
	if hasSubscription {
		t.Fatal("invalid 2011 Catalog Event created subscription state")
	}
}

func TestCascadeSubscriptionReconcilesNewSharedParent(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	api := &GB28181API{
		core: adapter, svr: &Server{memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime}},
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
	}
	worker := newCascadeWorker(api.svr, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()
	calls := make([]SubscribeInput, 0, 2)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	request := subscribeEventRequest{CmdType: "Catalog", SN: 1, DeviceID: platform.localID}
	desired, err := api.desiredCascadeDownstreamSubscriptions(t.Context(), worker, request, "Catalog", platform.localID, 600)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	subscription := &eventSubscription{
		Key: "catalog-topology", CmdType: "Catalog", DeviceID: platform.localID, Cascade: worker,
		ExpiresAt: time.Now().Add(10 * time.Minute), DownstreamKeys: keys,
	}
	api.eventSubscribers.Store(subscription.Key, subscription)

	secondDeviceID := "34020000002000000002"
	secondChannelID := "34020000001320000012"
	if err := adapter.Store().Device().Create(t.Context(), &ipc.Device{
		ID: "GB_cascade_device_2", DeviceID: secondDeviceID, Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Store().Channel().Create(t.Context(), &ipc.Channel{
		ID: "GBC_cascade_channel_2", DID: "GB_cascade_device_2", DeviceID: secondDeviceID,
		ChannelID: secondChannelID, Type: ipc.TypeGB28181, IsOnline: true,
	}); err != nil {
		t.Fatal(err)
	}
	worker.platform.sharedChannels = append(worker.platform.sharedChannels, secondChannelID)
	worker.platform.channelIDMap[secondChannelID] = secondChannelID
	worker.platform.exposedChannelMap[secondChannelID] = secondChannelID

	api.reconcileCascadeDownstreamSubscriptions(t.Context())
	if len(calls) != 2 {
		t.Fatalf("topology reconciliation calls = %d; want initial + new parent", len(calls))
	}
	if calls[0].DeviceID != persistentDevice.DeviceID || calls[1].DeviceID != secondDeviceID || calls[0].Cancel || calls[1].Cancel {
		t.Fatalf("topology reconciliation calls = %+v", calls)
	}
	subscription.mu.Lock()
	keyCount := len(subscription.DownstreamKeys)
	subscription.mu.Unlock()
	if keyCount != 2 {
		t.Fatalf("reconciled downstream keys = %d", keyCount)
	}
}

func TestCascadeDownstreamSubscriptionReferenceCounting(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	calls := make([]SubscribeInput, 0, 2)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 120}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	desired := map[string]SubscribeInput{key: input}
	first, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	second, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || api.cascadeSubscriptions[key].Refs != 2 {
		t.Fatalf("shared downstream state = calls %d, state %+v", len(calls), api.cascadeSubscriptions[key])
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), first)
	if len(calls) != 1 || api.cascadeSubscriptions[key].Refs != 1 {
		t.Fatalf("first release cancelled shared downstream: calls %d, state %+v", len(calls), api.cascadeSubscriptions[key])
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), second)
	if len(calls) != 2 || !calls[1].Cancel || len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("last release did not cancel downstream: calls %+v, states %+v", calls, api.cascadeSubscriptions)
	}
}

func TestCascadeDownstreamSubscriptionReusesEquivalentAlarmMethodOrder(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	calls := make([]SubscribeInput, 0, 2)
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		calls = append(calls, *input)
		return nil
	}
	firstInput := SubscribeInput{
		DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 120,
		AlarmMethod: "25", AlarmType: "2",
	}
	secondInput := firstInput
	secondInput.AlarmMethod = "5/2"
	firstKey := buildOutgoingSubscriptionKey(firstInput.DeviceID, firstInput.TargetID, firstInput.Event, &firstInput)
	secondKey := buildOutgoingSubscriptionKey(secondInput.DeviceID, secondInput.TargetID, secondInput.Event, &secondInput)
	if firstKey != secondKey {
		t.Fatalf("equivalent AlarmMethod order keys differ: %q != %q", firstKey, secondKey)
	}
	first, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{firstKey: firstInput})
	if err != nil {
		t.Fatal(err)
	}
	second, err := api.syncCascadeDownstreamSubscriptions(t.Context(), nil, map[string]SubscribeInput{secondKey: secondInput})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || api.cascadeSubscriptions[firstKey].Refs != 2 {
		t.Fatalf("equivalent AlarmMethod subscriptions were not reused: calls=%+v state=%+v", calls, api.cascadeSubscriptions[firstKey])
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), first)
	if len(calls) != 1 || api.cascadeSubscriptions[firstKey].Refs != 1 {
		t.Fatalf("first equivalent subscription release cancelled shared state: calls=%+v state=%+v", calls, api.cascadeSubscriptions[firstKey])
	}
	api.releaseCascadeDownstreamSubscriptions(t.Context(), second)
	if len(calls) != 2 || !calls[1].Cancel || len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("last equivalent subscription release did not cancel: calls=%+v states=%+v", calls, api.cascadeSubscriptions)
	}
}

func TestRemovingCascadeWorkerReleasesDownstreamSubscription(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	cancelled := 0
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.Cancel {
			cancelled++
		}
		return nil
	}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Alarm", Expires: 120}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{Input: input, Refs: 1}
	sub := &eventSubscription{
		Cascade: worker, ExpiresAt: time.Now().Add(time.Minute), DownstreamKeys: []string{key},
	}
	api.eventSubscribers.Store("worker-subscription", sub)
	api.removeCascadeEventSubscriptions(worker)
	if cancelled != 1 || len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("worker removal = cancelled %d, refs %+v", cancelled, api.cascadeSubscriptions)
	}
	if _, exists := api.eventSubscribers.Load("worker-subscription"); exists {
		t.Fatal("worker subscription remained after removal")
	}
}

func TestClosingCascadeSubscriptionsSendsExistingDialogCancellation(t *testing.T) {
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	localRaw, remoteRaw := net.Pipe()
	connection := sip.NewTCPConnection(localRaw)
	go sipServer.ProcessTCPConnection(connection)
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleDone: make(chan struct{}), lifecycleCtx: lifecycleCtx, lifecycleCancel: lifecycleCancel,
		cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription),
	}
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtime.UpdateRuntime(func(current *Device) {
		current.conn = connection
		current.source = connection.RemoteAddr()
		current.to = remote
	})
	memory := &flowMemory{persistent: &ipc.Device{DeviceID: gb10DeviceID}, runtime: runtime}
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	t.Cleanup(func() {
		api.close()
		_ = remoteRaw.Close()
		sipServer.Close()
	})

	callID := sip.CallID("shutdown-subscription-dialog")
	initial := sip.NewRequest("", sip.MethodSubscribe, remote.URI.Clone(), sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(local).SetTo(remote).SetContact(local).SetCallID(&callID).
			SetMethod(sip.MethodSubscribe).SetSeqNo(51).AddVia(&sip.ViaHop{
			Host: "192.0.2.20", Port: sip.NewPort(5060), Transport: "TCP",
			Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
		}).Build(), nil)
	initial.SetConnection(connection)
	initial.SetSource(connection.LocalAddr())
	initial.SetDestination(connection.RemoteAddr())
	response := sip.NewResponseFromRequest("", initial, http.StatusOK, "OK", nil)
	to, ok := response.To()
	if !ok || to == nil {
		t.Fatal("subscription response missing To")
	}
	if to.Params == nil {
		to.Params = sip.NewParams()
	}
	to.Params.Add("tag", sip.String{Str: "shutdown-remote-tag"})
	response.AppendHeader(&sip.ContactHeader{Address: remote.URI.Clone(), Params: sip.NewParams()})
	response.SetConnection(connection)
	response.SetSource(connection.RemoteAddr())
	response.SetDestination(connection.LocalAddr())

	identity := &monitorUserIdentity{
		Gateways: []string{testRemoteGatewayID}, UserID: testRemoteUserID,
		Organization: "remoteorg", Category: "dispatcher", Rank: "level2",
	}
	identityCtx := withMonitorUserIdentityRoute(t.Context(), identity, testLocalGatewayID)
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Catalog", Expires: 60}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input) + monitorUserIdentitySubscriptionKey(identityCtx)
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	api.outgoingSubscriptions.Store(key, &outgoingSubscriptionDialog{
		response: response, requestBody: body, eventValue: "Catalog;id=52",
		deviceID: gb10DeviceID, targetID: gb10DeviceID, identity: identity.clone(), localGatewayID: testLocalGatewayID,
		expiresAt: time.Now().Add(time.Minute),
	})
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{
		Input: input, Identity: identity.clone(), LocalGatewayID: testLocalGatewayID, Refs: 1,
	}

	requestBody := make(chan string, 1)
	remoteErr := make(chan error, 1)
	go func() {
		_ = remoteRaw.SetDeadline(time.Now().Add(3 * time.Second))
		request, err := readAnnexGTestSIPFrame(bufio.NewReader(remoteRaw))
		if err == nil {
			requestBody <- request
			_, err = remoteRaw.Write([]byte(annexGTestSIPResponse(request, http.StatusOK, "OK", "")))
		}
		remoteErr <- err
	}()

	server.Close()
	if err := <-remoteErr; err != nil {
		t.Fatal(err)
	}
	cancellation := <-requestBody
	for _, expected := range []string{
		"SUBSCRIBE sip:" + gb10DeviceID + "@192.0.2.10:5060 SIP/2.0",
		"Call-ID: shutdown-subscription-dialog",
		"CSeq: 52 SUBSCRIBE",
		"Expires: 0",
		string(body),
		testRemoteUserID,
	} {
		if !strings.Contains(cancellation, expected) {
			t.Fatalf("shutdown subscription cancellation missing %q:\n%s", expected, cancellation)
		}
	}
	if _, exists := api.outgoingSubscriptions.Load(key); exists {
		t.Fatal("shutdown retained outgoing subscription dialog")
	}
	api.cascadeSubscriptionMu.Lock()
	_, exists := api.cascadeSubscriptions[key]
	api.cascadeSubscriptionMu.Unlock()
	if exists {
		t.Fatal("shutdown retained downstream subscription state")
	}
}

func TestClosingCascadeSubscriptionsStopsAtShutdownDeadline(t *testing.T) {
	const key = "shutdown-subscription-lock-deadline"
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	api := &GB28181API{
		lifecycleClosed:        true,
		shutdownPersistenceCtx: shutdownCtx,
		cascadeSubscriptions: map[string]*cascadeDownstreamSubscription{
			key: {Input: SubscribeInput{DeviceID: gb10DeviceID, Event: "Catalog"}, Refs: 1},
		},
	}
	unlock, err := api.lockCascadeSubscriptionOperation(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	shutdownCancel()

	done := make(chan struct{})
	go func() {
		api.closeCascadeDownstreamSubscriptions()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		unlock()
		<-done
		t.Fatal("cascade subscription close exceeded the shutdown cleanup deadline")
	}
	api.cascadeSubscriptionMu.Lock()
	_, exists := api.cascadeSubscriptions[key]
	api.cascadeSubscriptionMu.Unlock()
	if exists {
		t.Fatal("deadline-limited close retained downstream subscription state")
	}
	unlock()
}

func TestCascadeSubscriptionAtExpirationBoundaryReleasesDownstreamReference(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	now := time.Now()
	cancelled := 0
	api.cascadeSubscribe = func(_ context.Context, input *SubscribeInput) error {
		if input.Cancel {
			cancelled++
		}
		return nil
	}
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Catalog", Expires: 60}
	key := buildOutgoingSubscriptionKey(input.DeviceID, input.TargetID, input.Event, &input)
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{Input: input, Refs: 1}
	api.eventSubscribers.Store("expired-catalog", &eventSubscription{
		CmdType: "Catalog", ExpiresAt: now, DownstreamKeys: []string{key},
	})
	api.cleanupEventSubscriptions(now)
	if cancelled != 1 || len(api.cascadeSubscriptions) != 0 {
		t.Fatalf("expired cleanup = cancelled %d, refs %+v", cancelled, api.cascadeSubscriptions)
	}
	if _, exists := api.eventSubscribers.Load("expired-catalog"); exists {
		t.Fatal("expired Catalog subscription remained after cleanup")
	}
}

func TestCascadeSubscriptionRenewalWinsConcurrentCleanup(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	platform := testSharedCascadePlatform(t)
	local := mustFlowAddress(t, "sip:"+platform.localID+"@local.example")
	remote := mustFlowAddress(t, "sip:"+platform.serverID+"@remote.example")
	connection := newFlowConnection()
	connection.remote = platform.remote
	runtime := &Device{IsOnline: true, gbVersion: string(GBVersion11)}
	server := &Server{
		Server:       sip.NewServer(local),
		memoryStorer: &flowMemory{persistent: persistentDevice, runtime: runtime},
	}
	api := &GB28181API{core: adapter, svr: server, cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	server.gb = api
	worker := newCascadeWorker(server, platform)
	worker.mu.Lock()
	worker.effective = GBVersion11
	worker.mu.Unlock()

	callID := sip.CallID("cascade-catalog-renew-cleanup")
	from := remote.Clone()
	from.Params.Add("tag", sip.String{Str: "upstream-tag"})
	body, err := sip.XMLEncode(subscribeEventRequest{CmdType: "Catalog", SN: 33, DeviceID: platform.localID})
	if err != nil {
		t.Fatal(err)
	}
	localTag := ""
	makeContext := func(expires, txID string, cseq uint32) *sip.Context {
		request := sip.NewRequest("", sip.MethodSubscribe, local.URI, sip.DefaultSipVersion,
			sip.NewHeaderBuilder().SetFrom(from).SetTo(local).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
				AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), body)
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + platform.localID})
		request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
		request.SetConnection(connection)
		request.SetSource(connection.remote)
		applyInboundSubscribeDialog(t, request, localTag, cseq)
		ctx := &sip.Context{
			Request: request, Tx: sip.NewTransaction(txID, connection), DeviceID: platform.serverID,
			Source: connection.remote, To: remote, XGBVer: string(GBVersion11),
		}
		ctx.Set(cascadeWorkerContextKey, worker)
		return ctx
	}
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error { return nil }
	api.sipSubscribeEvent(makeContext("60", "catalog-initial", 1))
	assertFlowOK(t, <-flowResponse(t, connection))
	var subscription *eventSubscription
	api.eventSubscribers.Range(func(_, value any) bool {
		subscription, _ = value.(*eventSubscription)
		return false
	})
	if subscription == nil {
		t.Fatal("initial Catalog subscription missing")
	}
	localTag = subscription.LocalTag
	subscription.mu.Lock()
	subscription.ExpiresAt = time.Now().Add(-time.Second)
	subscription.mu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	api.cascadeSubscribe = func(context.Context, *SubscribeInput) error {
		close(entered)
		<-release
		return nil
	}
	renewDone := make(chan struct{})
	go func() {
		api.sipSubscribeEvent(makeContext("120", "catalog-renew", 2))
		close(renewDone)
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("renewal did not enter downstream refresh")
	}
	cleanupDone := make(chan struct{})
	go func() {
		api.cleanupEventSubscriptions(time.Now())
		close(cleanupDone)
	}()
	select {
	case <-cleanupDone:
		t.Fatal("cleanup bypassed in-flight renewal serialization")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-renewDone:
	case <-time.After(time.Second):
		t.Fatal("renewal did not finish")
	}
	assertFlowOK(t, <-flowResponse(t, connection))
	select {
	case <-cleanupDone:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not finish after renewal")
	}
	if _, exists := api.eventSubscribers.Load(subscription.Key); !exists {
		t.Fatal("renewed Catalog subscription was removed by concurrent cleanup")
	}
}

func TestCascadeAlarmFilterSuppressesNotify(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	t.Cleanup(worker.cancel)
	requests := make(chan *sip.Request, 1)
	worker.exchange = func(_ context.Context, in *sip.Request) (*sip.Response, error) {
		requests <- in
		return sip.NewResponseFromRequest("", in, http.StatusOK, "OK", nil), nil
	}
	sub := newCascadeEventSubscriptionForTest(t, worker, "Alarm", testExposedChannelID)
	sub.Filter = eventSubscriptionFilter{StartAlarmPriority: "1", EndAlarmPriority: "2", AlarmMethod: "5"}
	api := &GB28181API{}
	t.Cleanup(func() {
		api.beginClose()
		api.lifecycleWG.Wait()
	})
	api.eventSubscribers.Store("filtered-alarm", sub)
	api.publishEventNotify("Alarm", gb10DeviceID, []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>`+testCascadeChannelID+`</DeviceID><AlarmPriority>3</AlarmPriority><AlarmMethod>5</AlarmMethod></Notify>`))
	select {
	case request := <-requests:
		t.Fatalf("non-matching Alarm was sent upstream: %s", request)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCascadeMobilePositionSubscriptionVersionConversion(t *testing.T) {
	tests := []struct {
		name          string
		version       GBProtocolVersion
		body          string
		wantBatch     bool
		wantDeviceID  bool
		forbiddenText []string
	}{
		{
			name: "2016 single to 2022 batch", version: GBVersion30,
			body:      `<Notify><CmdType>MobilePosition</CmdType><SN>11</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude><Speed>10</Speed></Notify>`,
			wantBatch: true, wantDeviceID: true,
		},
		{
			name: "2022 batch to 2016 single", version: GBVersion20,
			body:          `<Notify><CmdType>MobilePosition</CmdType><SN>12</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-28T10:00:01</Time><SumNum>1</SumNum><DeviceList Num="1"><Item><DeviceID>` + testCascadeChannelID + `</DeviceID><CaptureTime>2026-08-28T10:00:00</CaptureTime><Longitude>120.5</Longitude><Latitude>30.2</Latitude><Height>8</Height></Item></DeviceList></Notify>`,
			forbiddenText: []string{"<DeviceID>", "<SumNum>", "<DeviceList", "<Height>"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
			worker.mu.Lock()
			worker.effective = test.version
			worker.mu.Unlock()
			t.Cleanup(worker.cancel)
			sent := make(chan *sip.Request, 1)
			worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
				sent <- request
				return sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil), nil
			}
			sub := newCascadeEventSubscriptionForTest(t, worker, "MobilePosition", testExposedChannelID)
			api := &GB28181API{}
			t.Cleanup(func() {
				api.beginClose()
				api.lifecycleWG.Wait()
			})
			api.eventSubscribers.Store(test.name, sub)
			api.publishEventNotifyForTarget("MobilePosition", gb10DeviceID, testCascadeChannelID, []byte(test.body))
			var request *sip.Request
			select {
			case request = <-sent:
			case <-time.After(time.Second):
				t.Fatal("MobilePosition subscription NOTIFY was not sent")
			}
			text := string(request.Body())
			if got := strings.Contains(text, "<SumNum>1</SumNum>"); got != test.wantBatch {
				t.Fatalf("batch structure present = %v, want %v: %s", got, test.wantBatch, text)
			}
			if got := strings.Contains(text, "<DeviceID>"+testExposedChannelID+"</DeviceID>"); got != test.wantDeviceID {
				t.Fatalf("mapped DeviceID present = %v, want %v: %s", got, test.wantDeviceID, text)
			}
			for _, forbidden := range test.forbiddenText {
				if strings.Contains(text, forbidden) {
					t.Fatalf("converted MobilePosition contains %q: %s", forbidden, text)
				}
			}
		})
	}
}

func TestRewriteCascadeMobilePositionFiltersUnsharedBatchItems(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	unsharedID := "34020000001320000099"
	body := []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>13</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-08-28T10:00:01</Time><SumNum>2</SumNum><DeviceList><Item><DeviceID>` + testCascadeChannelID + `</DeviceID><CaptureTime>2026-08-28T10:00:00</CaptureTime><Longitude>120.5</Longitude><Latitude>30.2</Latitude></Item><Item><DeviceID>` + unsharedID + `</DeviceID><CaptureTime>2026-08-28T10:00:00</CaptureTime><Longitude>121.5</Longitude><Latitude>31.2</Latitude></Item></DeviceList></Notify>`)
	outputs, err := rewriteCascadeMobilePositionForVersion(platform, body, gb10DeviceID, GBVersion30, platform.localID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 1 {
		t.Fatalf("converted outputs = %d, want 1", len(outputs))
	}
	text := string(outputs[0].body)
	if !strings.Contains(text, "<SumNum>1</SumNum>") || !strings.Contains(text, testExposedChannelID) || strings.Contains(text, unsharedID) {
		t.Fatalf("filtered 2022 MobilePosition = %s", text)
	}
}

func TestRewriteCascadeMobilePositionRejectsUnsupportedExtensionsAndVersions(t *testing.T) {
	platform := testSharedCascadePlatform(t)
	base := `<Notify><CmdType>MobilePosition</CmdType><SN>14</SN><Time>2026-08-28T10:00:00</Time><Longitude>120.5</Longitude><Latitude>30.2</Latitude>%s</Notify>`
	for _, extension := range []string{"<Info>vendor</Info>", "<ExtraInfo>vendor</ExtraInfo>", `<Info><doorType><DeviceID>` + testCascadeChannelID + `</DeviceID></doorType></Info>`} {
		if _, err := rewriteCascadeMobilePositionForVersion(platform, []byte(fmt.Sprintf(base, extension)), testCascadeChannelID, GBVersion30, testExposedChannelID); err == nil {
			t.Fatalf("unsupported MobilePosition extension %q was accepted", extension)
		}
	}
	if _, err := rewriteCascadeMobilePositionForVersion(platform, []byte(fmt.Sprintf(base, "")), testCascadeChannelID, GBVersion11, testExposedChannelID); err == nil {
		t.Fatal("GB/T 28181-2014 MobilePosition target was accepted")
	}
}

func TestRewriteCascadeAlarmInfoVersionMatrix(t *testing.T) {
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-29T10:00:00</AlarmTime><Info><AlarmType>13</AlarmType><doorEventType><DeviceID>` + testCascadeChannelID + `</DeviceID></doorEventType></Info><ExtraInfo>modern</ExtraInfo></Notify>`)
	tests := []struct {
		name               string
		version            GBProtocolVersion
		wantInfo           int
		wantExtraInfo      int
		wantTyped          bool
		wantAppendixA4     bool
		wantAlarmTypeCount int
	}{
		{name: "2011", version: GBVersion10, wantInfo: 1},
		{name: "2014", version: GBVersion11, wantInfo: 1},
		{name: "2016 drops type 13 but preserves plain extension", version: GBVersion20, wantInfo: 1},
		{name: "2022", version: GBVersion30, wantInfo: 1, wantExtraInfo: 1, wantTyped: true, wantAppendixA4: true, wantAlarmTypeCount: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			converted, err := rewriteCascadeAlarmInfoForVersion(body, test.version)
			if err != nil {
				t.Fatal(err)
			}
			text := string(converted)
			if got := strings.Count(text, "<Info>"); got != test.wantInfo {
				t.Fatalf("Info count = %d, want %d: %s", got, test.wantInfo, text)
			}
			if got := strings.Count(text, "<ExtraInfo>"); got != test.wantExtraInfo {
				t.Fatalf("ExtraInfo count = %d, want %d: %s", got, test.wantExtraInfo, text)
			}
			if got := strings.Contains(text, "<Info><AlarmType>13</AlarmType>"); got != test.wantTyped {
				t.Fatalf("typed Info present = %v, want %v: %s", got, test.wantTyped, text)
			}
			if got := strings.Contains(text, "<doorEventType>"); got != test.wantAppendixA4 {
				t.Fatalf("Appendix A.4 present = %v, want %v: %s", got, test.wantAppendixA4, text)
			}
			if got := strings.Count(text, "<AlarmType>13</AlarmType>"); got != test.wantAlarmTypeCount {
				t.Fatalf("AlarmType count = %d, want %d: %s", got, test.wantAlarmTypeCount, text)
			}
		})
	}

	valid2016 := []byte(`<Notify><CmdType>Alarm</CmdType><SN>2</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-29T10:00:00</AlarmTime><Info><AlarmType>12</AlarmType><doorEventType><DeviceID>` + testCascadeChannelID + `</DeviceID></doorEventType></Info><ExtraInfo>modern</ExtraInfo></Notify>`)
	converted, err := rewriteCascadeAlarmInfoForVersion(valid2016, GBVersion20)
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, "<Info><AlarmType>12</AlarmType></Info>") || !strings.Contains(text, "<Info>modern</Info>") || strings.Contains(text, "<ExtraInfo>") || strings.Contains(text, "<doorEventType>") {
		t.Fatalf("2016 Alarm conversion = %s", text)
	}

	legacy2016 := []byte(`<Notify><CmdType>Alarm</CmdType><SN>3</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-29T10:00:00</AlarmTime><Info><AlarmType>12</AlarmType></Info><Info>legacy</Info></Notify>`)
	converted, err = rewriteCascadeAlarmInfoForVersion(legacy2016, GBVersion30)
	if err != nil {
		t.Fatal(err)
	}
	text = string(converted)
	if !strings.Contains(text, "<Info><AlarmType>12</AlarmType></Info>") || !strings.Contains(text, "<ExtraInfo>legacy</ExtraInfo>") || strings.Contains(text, "<Info>legacy</Info>") {
		t.Fatalf("2022 Alarm conversion = %s", text)
	}

	for _, invalid := range []string{
		`<Notify><CmdType>Alarm</CmdType><SN>4</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-29T10:00:00</AlarmTime><AlarmType>1</AlarmType></Notify>`,
		`<Notify><CmdType>Alarm</CmdType><SN>5</SN><DeviceID>` + testCascadeChannelID + `</DeviceID><AlarmPriority>1</AlarmPriority><AlarmMethod>5</AlarmMethod><AlarmTime>2026-08-29T10:00:00</AlarmTime><Info><AlarmMethod>5</AlarmMethod><AlarmType>1</AlarmType></Info></Notify>`,
	} {
		if _, err := rewriteCascadeAlarmInfoForVersion([]byte(invalid), GBVersion30); err == nil {
			t.Fatalf("invalid Alarm structure was converted: %s", invalid)
		}
	}
}

func newCascadeEventSubscriptionForTest(t *testing.T, worker *cascadeWorker, cmdType, deviceID string) *eventSubscription {
	t.Helper()
	remoteURI, _ := sip.ParseSipURI("sip:" + gb10PlatformID + "@remote.example")
	localURI, _ := sip.ParseSipURI("sip:" + gb10DeviceID + "@local.example")
	callID := sip.CallID("cascade-filter-subscribe")
	subscribe := sip.NewRequest("", sip.MethodSubscribe, &localURI, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetFrom(&sip.Address{URI: &remoteURI, Params: sip.NewParams().Add("tag", sip.String{Str: "remote-tag"})}).
			SetTo(&sip.Address{URI: &localURI, Params: sip.NewParams()}).SetMethod(sip.MethodSubscribe).SetCallID(&callID).
			AddVia(&sip.ViaHop{Host: "192.0.2.30", Port: sip.NewPort(5060), Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	return &eventSubscription{
		CmdType: cmdType, DeviceID: deviceID, Event: "presence", ExpiresAt: time.Now().Add(time.Hour),
		To: &sip.Address{URI: &remoteURI, Params: sip.NewParams()}, GBVersion: string(GBVersion30),
		DialogRequest: subscribe, Response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		Contact: worker.contactAddress(), Cascade: worker,
	}
}
