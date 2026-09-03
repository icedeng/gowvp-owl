package gbs

import (
	"net/http"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	testCascadePathB = "34020000002000000004"
	testCascadePathC = "34020000002000000002"
	testCascadePathE = "34020000002000000003"
)

func TestCascadePreferredPathConsumeAndRouteResponse(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.platform.localID = testCascadePathB
	worker.mu.Unlock()
	localID := worker.platform.localID
	recipient, err := sip.ParseSipURI("sip:" + testExposedChannelID + "@local.example")
	if err != nil {
		t.Fatal(err)
	}
	request := sip.NewRequest("", sip.MethodInvite, &recipient, sip.DefaultSipVersion, nil, nil)
	request.AppendHeader(&sip.GenericHeader{
		HeaderName: cascadePreferredPathHeader,
		Contents:   localID + "-" + testCascadePathC + "-" + testCascadePathE,
	})
	remaining, err := consumeCascadePreferredPath(request, worker)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != testCascadePathC+"-"+testCascadePathE {
		t.Fatalf("remaining preferred path = %q", remaining)
	}

	downstream := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil)
	downstream.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: remaining})
	path, err := buildCascadeRoutePath(localID, downstream, remaining)
	if err != nil {
		t.Fatal(err)
	}
	want := localID + "-" + remaining
	if path != want {
		t.Fatalf("route path = %q, want %q", path, want)
	}
	if localOnly, err := buildCascadeRoutePath(localID, nil, ""); err != nil || localOnly != localID {
		t.Fatalf("local route path = %q, %v", localOnly, err)
	}
}

func TestCascadePathRejectsInvalidMismatchAndLoop(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.platform.localID = testCascadePathB
	worker.mu.Unlock()
	localID := worker.platform.localID
	recipient, _ := sip.ParseSipURI("sip:" + testExposedChannelID + "@local.example")
	tests := []struct {
		name  string
		value string
	}{
		{name: "wrong next hop", value: testCascadePathC + "-" + localID},
		{name: "camera id", value: localID + "-34020000001320000001"},
		{name: "duplicate", value: localID + "-" + localID},
		{name: "non digit", value: localID + "-3402000000200000000x"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := sip.NewRequest("", sip.MethodInvite, &recipient, sip.DefaultSipVersion, nil, nil)
			request.AppendHeader(&sip.GenericHeader{HeaderName: cascadePreferredPathHeader, Contents: test.value})
			if _, err := consumeCascadePreferredPath(request, worker); err == nil {
				t.Fatalf("invalid preferred path %q was accepted", test.value)
			}
		})
	}

	downstream := sip.NewResponse("", sip.DefaultSipVersion, http.StatusOK, "OK", nil, nil)
	downstream.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: testCascadePathC})
	if _, err := buildCascadeRoutePath(localID, downstream, testCascadePathE); err == nil {
		t.Fatal("mismatched preferred path was accepted")
	}
	downstream.RemoveHeader(cascadeRoutePathHeader)
	downstream.AppendHeader(&sip.GenericHeader{HeaderName: cascadeRoutePathHeader, Contents: testCascadePathC + "-" + localID})
	if _, err := buildCascadeRoutePath(localID, downstream, ""); err == nil {
		t.Fatal("route loop was accepted")
	}

	worker.mu.Lock()
	worker.effective = GBVersion20
	worker.mu.Unlock()
	request := sip.NewRequest("", sip.MethodInvite, &recipient, sip.DefaultSipVersion, nil, nil)
	request.AppendHeader(&sip.GenericHeader{HeaderName: cascadePreferredPathHeader, Contents: localID})
	if _, err := consumeCascadePreferredPath(request, worker); err == nil {
		t.Fatal("pre-2022 preferred path was accepted")
	}
}

func TestCascadeFailureRoutePathUsesActualOrConfirmedNextHop(t *testing.T) {
	worker := newCascadeWorker(nil, testSharedCascadePlatform(t))
	worker.mu.Lock()
	worker.effective = GBVersion30
	worker.platform.localID = testCascadePathB
	worker.mu.Unlock()

	actual := sip.NewResponse("", sip.DefaultSipVersion, http.StatusNotFound, "Not Found", nil, nil)
	actual.AppendHeader(&sip.GenericHeader{
		HeaderName: cascadeRoutePathHeader,
		Contents:   testCascadePathC + "-" + testCascadePathE,
	})
	upstream := sip.NewResponse("", sip.DefaultSipVersion, http.StatusBadGateway, "Bad Gateway", nil, nil)
	if err := appendCascadeFailureRoutePath(upstream, worker, actual, testCascadePathC+"-"+testCascadePathE); err != nil {
		t.Fatal(err)
	}
	if path, err := singleSIPHeaderValue(upstream, cascadeRoutePathHeader); err != nil || path != testCascadePathB+"-"+testCascadePathC+"-"+testCascadePathE {
		t.Fatalf("actual failure route path = %q, %v", path, err)
	}

	upstream = sip.NewResponse("", sip.DefaultSipVersion, http.StatusBadGateway, "Bad Gateway", nil, nil)
	if err := appendCascadeFailureRoutePath(upstream, worker, nil, testCascadePathC+"-"+testCascadePathE); err != nil {
		t.Fatal(err)
	}
	if path, err := singleSIPHeaderValue(upstream, cascadeRoutePathHeader); err != nil || path != testCascadePathB+"-"+testCascadePathC {
		t.Fatalf("fallback failure route path = %q, %v", path, err)
	}
}
