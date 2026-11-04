package api

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
)

func TestHistoryPlaybackAddressUsesBoundNodeAndIsolatedToken(t *testing.T) {
	api := newChannelMediaServerAPI(t, &ipc.Channel{ID: "ch-history-test", Type: ipc.TypeGB28181, Config: ipc.StreamConfig{MediaServerID: "edge-zlm-1"}})
	api.uc.Conf = &conf.Bootstrap{Server: conf.Server{HTTP: conf.ServerHTTP{JwtSecret: testPlaySecret, Port: 15123}}}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "http://example.test:15123/channels/ch-history-test/history/start", nil)
	out, err := api.historyPlaybackOutput(c, "ch-history-test")
	if err != nil {
		t.Fatal(err)
	}
	if out.Stream == "ch-history-test" || out.App != "rtp" || len(out.Items) != 1 {
		t.Fatalf("unexpected history output: %+v", out)
	}
	parsed, err := url.Parse(out.Items[0].FLV)
	if err != nil || !strings.Contains(parsed.Path, "/rtp/"+out.Stream+".live.flv") {
		t.Fatalf("unexpected FLV path: %s, %v", out.Items[0].FLV, err)
	}
	verify, _ := gin.CreateTestContext(httptest.NewRecorder())
	verify.Request = httptest.NewRequest("GET", parsed.String(), nil)
	path := "/" + sms.MediaServerProxyRoute("edge-zlm-1") + "/rtp/" + out.Stream + ".live.flv"
	if err := api.uc.verifyPlayToken(verify, path); err != nil {
		t.Fatal(err)
	}
	if err := api.uc.verifyPlayToken(verify, "/"+sms.MediaServerProxyRoute("edge-zlm-1")+"/rtp/ch-history-test.live.flv"); err == nil {
		t.Fatal("history token authorized live stream")
	}
	if err := api.uc.verifyPlayToken(verify, "/"+sms.MediaServerProxyRoute("local")+"/rtp/"+out.Stream+".live.flv"); err == nil {
		t.Fatal("history token authorized another node")
	}
}
