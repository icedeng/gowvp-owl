package api

import (
	"testing"

	"github.com/gowvp/owl/internal/conf"
)

func TestSIPAccessInfoUsesConfiguredHost(t *testing.T) {
	cfg := &conf.Bootstrap{
		Sip:   conf.SIP{Host: "121.37.88.13", ID: "50000000002000000001", Domain: "5000000000", Port: 15060, Password: "secret"},
		Media: conf.Media{SDPIP: "192.168.1.241"},
	}
	got := sipAccessInfo(cfg)
	if got.ServerIP != cfg.Sip.Host || got.ID != cfg.Sip.ID || got.Domain != cfg.Sip.Domain || got.Port != cfg.Sip.Port || got.Password != cfg.Sip.Password {
		t.Fatalf("unexpected access info: %+v", got)
	}
}

func TestSIPAccessInfoFallsBackToSDPIPAndDerivedDomain(t *testing.T) {
	cfg := &conf.Bootstrap{
		Sip:   conf.SIP{ID: "50000000002000000001", Port: 15060},
		Media: conf.Media{SDPIP: "192.168.1.241"},
	}
	got := sipAccessInfo(cfg)
	if got.ServerIP != cfg.Media.SDPIP {
		t.Fatalf("expected SDP IP fallback, got %q", got.ServerIP)
	}
	if got.Domain != "5000000000" {
		t.Fatalf("expected derived domain, got %q", got.Domain)
	}
}
