package openapi

import (
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

func TestMediaBaseURLRequestAndTrustedProxy(t *testing.T) {
	_, trusted, _ := net.ParseCIDR("10.0.0.0/8")
	a := New(&op.Service{Config: op.Config{PublicBaseURL: "https://fallback.example", TrustedProxies: []*net.IPNet{trusted}}})
	for _, tt := range []struct {
		name, target, peer, host, proto, want string
	}{
		{"LAN", "http://192.168.6.20:15123/", "192.168.6.11:2000", "", "", "http://192.168.6.20:15123"},
		{"EasyTier", "http://10.144.0.20:15123/", "192.168.6.11:2000", "", "", "http://10.144.0.20:15123"},
		{"TLS", "https://open.example:8443/", "192.168.6.11:2000", "", "", "https://open.example:8443"},
		{"IPv6", "http://[::1]:15123/", "[::1]:2000", "", "", "http://[::1]:15123"},
		{"untrusted headers", "http://192.168.6.20:15123/", "192.168.6.11:2000", "evil.example", "https", "http://192.168.6.20:15123"},
		{"trusted proxy", "http://owl:15123/", "10.0.0.2:2000", "open.example:8443", "https", "https://open.example:8443"},
		{"invalid forwarded host", "http://owl:15123/", "10.0.0.2:2000", "evil.example/path", "https", "https://owl:15123"},
		{"forwarded chain", "http://owl:15123/", "10.0.0.2:2000", "open.example, evil.example", "https,http", "http://owl:15123"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("GET", tt.target, nil)
			c.Request.RemoteAddr = tt.peer
			c.Request.Header.Set("X-Forwarded-Host", tt.host)
			c.Request.Header.Set("X-Forwarded-Proto", tt.proto)
			if got := a.mediaBaseURL(c); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "http://owl:15123/", nil)
	c.Request.RemoteAddr = "10.0.0.2:2000"
	c.Request.Header.Add("X-Forwarded-Host", "open.example")
	c.Request.Header.Add("X-Forwarded-Host", "evil.example")
	if got := a.mediaBaseURL(c); got != "http://owl:15123" {
		t.Fatal("duplicate forwarded host accepted", got)
	}
	c.Request.Host = "evil.example@other.example"
	if got := a.mediaBaseURL(c); got != a.S.Config.PublicBaseURL {
		t.Fatal("invalid host did not fall back", got)
	}
}

func TestTicketURLFollowsRequestHost(t *testing.T) {
	r, s, p, key := apiFixture(t)
	row := op.Session{ID: "origin-session", AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: "c1", State: "ready", Protocol: "flv", ExpiresAt: time.Now().Add(time.Minute), Revision: p.App.Revision}
	if err := s.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"192.168.6.20:15123", "10.144.0.20:15123"} {
		req := signed(p, key, "POST", "/openapi/v1/live-sessions/"+row.ID+"/tickets", "", []byte(`{}`), op.Random(24))
		req.Host = host
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		if out.Code != 200 || !strings.Contains(out.Body.String(), "http://"+host+"/openmedia/v1/live/"+row.ID+"/stream.live.flv?ticket=") {
			t.Fatal(out.Code, out.Body.String())
		}
	}
}
