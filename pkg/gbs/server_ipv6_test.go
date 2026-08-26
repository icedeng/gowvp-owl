package gbs

import (
	"errors"
	"net"
	"testing"
)

func TestResolveSIPAdvertiseHostHonorsConfiguration(t *testing.T) {
	called := false
	host, err := resolveSIPAdvertiseHost(" [2001:db8::20] ", "192.0.2.30", func() (net.IP, error) {
		called = true
		return nil, errors.New("must not be called")
	})
	if err != nil || host != "2001:db8::20" || called {
		t.Fatalf("configured SIP advertise host = %q, called=%v, err=%v", host, called, err)
	}
}

func TestResolveSIPAdvertiseHostUsesResolvedAddressBeforeFallback(t *testing.T) {
	host, err := resolveSIPAdvertiseHost("", "192.0.2.30", func() (net.IP, error) {
		return net.ParseIP("2001:db8::20"), nil
	})
	if err != nil || host != "2001:db8::20" {
		t.Fatalf("resolved SIP advertise host = %q, %v", host, err)
	}
}

func TestResolveSIPAdvertiseHostFallsBackAfterDetectionFailure(t *testing.T) {
	host, err := resolveSIPAdvertiseHost("", " [2001:db8::30] ", func() (net.IP, error) {
		return nil, errors.New("no usable interface")
	})
	if err != nil || host != "2001:db8::30" {
		t.Fatalf("fallback SIP advertise host = %q, %v", host, err)
	}
}

func TestResolveSIPAdvertiseHostRejectsMalformedBrackets(t *testing.T) {
	for _, host := range []string{"[2001:db8::20", "2001:db8::20]", "[example.com]"} {
		if _, err := resolveSIPAdvertiseHost(host, "", func() (net.IP, error) { return nil, nil }); err == nil {
			t.Fatalf("malformed SIP advertise host %q was accepted", host)
		}
	}
}
