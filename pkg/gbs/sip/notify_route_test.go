package sip

import (
	"strings"
	"testing"
)

func TestRequestRouteKeyAllowsEmptyTerminatedNotify(t *testing.T) {
	target, err := ParseSipURI("sip:34020000002000000001@192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodNotify, &target, DefaultSipVersion, []Header{
		&GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"},
		func() Header { value := ContentLength(0); return &value }(),
	}, nil)
	key, err := requestRouteKey(request)
	if err != nil || key != MethodNotify {
		t.Fatalf("terminated NOTIFY route = %q, %v", key, err)
	}

	request.RemoveHeader("Subscription-State")
	request.AppendHeader(&GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := requestRouteKey(request); err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("active empty NOTIFY error = %v", err)
	}

	message := NewRequest("", MethodMessage, &target, DefaultSipVersion, []Header{
		func() Header { value := ContentLength(0); return &value }(),
	}, nil)
	if _, err := requestRouteKey(message); err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("empty MESSAGE error = %v", err)
	}
}
