package gbs

import (
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/conf"
)

func TestGetSSRCUsesValidatedDomainCode(t *testing.T) {
	api := &GB28181API{cfg: &conf.SIP{Domain: "3402000000"}}

	live, err := api.getSSRC(0)
	if err != nil {
		t.Fatal(err)
	}
	history, err := api.getSSRC(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 10 || !strings.HasPrefix(live, "020000") {
		t.Fatalf("live SSRC = %q", live)
	}
	if len(history) != 10 || !strings.HasPrefix(history, "120000") {
		t.Fatalf("history SSRC = %q", history)
	}
	if live == history {
		t.Fatalf("live and history SSRC must differ: %q", live)
	}
}

func TestGetSSRCDerivesDomainFromPlatformID(t *testing.T) {
	api := &GB28181API{cfg: &conf.SIP{ID: "34020000002000000001"}}
	ssrc, err := api.getSSRC(0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ssrc, "020000") {
		t.Fatalf("derived-domain SSRC = %q", ssrc)
	}
}

func TestGetSSRCRejectsInvalidConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		api        *GB28181API
		streamType int
	}{
		{name: "nil API", api: nil, streamType: 0},
		{name: "nil config", api: &GB28181API{}, streamType: 0},
		{name: "short domain", api: &GB28181API{cfg: &conf.SIP{Domain: "3402"}}, streamType: 0},
		{name: "non-numeric domain", api: &GB28181API{cfg: &conf.SIP{Domain: "local.test"}}, streamType: 0},
		{name: "stream type", api: &GB28181API{cfg: &conf.SIP{Domain: "3402000000"}}, streamType: 2},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.api.getSSRC(test.streamType); err == nil {
				t.Fatal("getSSRC succeeded, want error")
			}
		})
	}
}
