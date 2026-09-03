package sms

import (
	"strings"
	"testing"
)

func TestStreamDriversIncludeMediaServerRouteInProxiedURLs(t *testing.T) {
	server := &MediaServer{ID: "edge-zlm-1"}
	for _, test := range []struct {
		name string
		addr StreamLiveAddr
	}{
		{name: "zlm", addr: NewZLMDriver().GetStreamLiveAddr(t.Context(), server, "http://owl", "owl", "rtp", "stream-1", "token")},
		{name: "lalmax", addr: (&LalmaxDriver{}).GetStreamLiveAddr(t.Context(), server, "http://owl", "owl", "rtp", "stream-1", "token")},
	} {
		t.Run(test.name, func(t *testing.T) {
			for label, value := range map[string]string{
				"flv": test.addr.FLV, "ws-flv": test.addr.WSFLV, "hls": test.addr.HLS, "webrtc": test.addr.WebRTC,
			} {
				if !strings.Contains(value, "/proxy/sms/_media/edge-zlm-1/") {
					t.Fatalf("%s URL %q does not carry the selected media server route", label, value)
				}
			}
		})
	}
}
