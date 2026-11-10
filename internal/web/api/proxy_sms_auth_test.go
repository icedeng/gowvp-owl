package api

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/ixugo/goddd/pkg/web"
)

func TestVerifyPlayTokenRequiresCompleteBinding(t *testing.T) {
	for _, data := range []map[string]any{
		{"app": "live", "stream": "camera01"},
		{"stream": "camera01", "media_server_id": "local"},
		{"app": "live", "media_server_id": "local"},
		{"app": "..", "stream": "camera01", "media_server_id": "local"},
		{"app": "live", "stream": "camera01/other", "media_server_id": "local"},
	} {
		token, err := web.NewToken(data, testPlaySecret+"_play")
		if err != nil {
			t.Fatal(err)
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/?token="+token, nil)
		if err := newTestUsecase().verifyPlayToken(c, "/_media/local/live/camera01.live.flv"); err == nil {
			t.Fatalf("accepted incomplete or invalid binding: %v", data)
		}
	}
}

func TestProxySMSGeneratedPlaybackLinks(t *testing.T) {
	for _, check := range []struct {
		driver sms.Driver
		app    string
	}{{&sms.ZLMDriver{}, "live"}, {&sms.LalmaxDriver{}, "live"}, {&sms.LalmaxDriver{}, ""}} {
		driver, app := check.driver, check.app
		t.Run(driver.Protocol()+"/app="+app, func(t *testing.T) {
			proxyServer, server := newSMSProxyTestServer(t, driver.Protocol(), func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Upgrade") == "websocket" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_, _ = io.WriteString(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
					return
				}
				if r.URL.Path == "/index/api/webrtc" {
					body, _ := io.ReadAll(r.Body)
					if r.Method != http.MethodPost || string(body) != "sdp offer" || r.URL.Query().Get("app") != app {
						t.Errorf("WebRTC signaling changed: %s %q", r.Method, body)
					}
				}
				_, _ = w.Write([]byte("media"))
			})
			token := makePlayTokenForMedia(t, app, "camera01", server.ID)
			addr := driver.GetStreamLiveAddr(t.Context(), server, proxyServer.URL, "example.test", app, "camera01", token)
			for _, check := range []struct{ method, address, body string }{
				{http.MethodGet, addr.FLV, ""},
				{http.MethodHead, addr.FLV, ""},
				{http.MethodPost, strings.Replace(addr.WebRTC, "webrtc://", "http://", 1), "sdp offer"},
				{http.MethodGet, strings.Replace(addr.WSFLV, "ws://", "http://", 1), "upgrade"},
			} {
				req, _ := http.NewRequest(check.method, check.address, strings.NewReader(check.body))
				req.Header.Set("Origin", "https://third-party.example")
				if check.body == "upgrade" {
					req.Header.Set("Upgrade", "websocket")
					req.Header.Set("Connection", "Upgrade")
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				want := http.StatusOK
				if check.body == "upgrade" {
					want = http.StatusSwitchingProtocols
				}
				if resp.StatusCode != want {
					t.Fatalf("%s %s returned %d, want %d", check.method, check.address, resp.StatusCode, want)
				}
			}
			// 不能用另一个驱动的路径绕过目标节点的 app/stream 边界。
			wrongPath := "/camera01.flv"
			if driver.Protocol() == sms.ProtocolLalmax {
				wrongPath = "/other/camera01.live.flv"
			}
			resp, err := http.Get(proxyServer.URL + "/proxy/sms/" + sms.MediaServerProxyRoute(server.ID) + wrongPath + "?token=" + token)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("wrong driver path returned %d", resp.StatusCode)
			}
		})
	}
}

func TestProxySMSHLSPropagatesCredentialsToThirdPartyResources(t *testing.T) {
	for _, driver := range []sms.Driver{&sms.ZLMDriver{}, &sms.LalmaxDriver{}} {
		t.Run(driver.Protocol(), func(t *testing.T) {
			prefix := "/live/camera01/"
			if driver.Protocol() == sms.ProtocolLalmax {
				prefix = "/camera01/"
			}
			proxyServer, server := newSMSProxyTestServer(t, driver.Protocol(), func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("token") == "" || r.URL.Query().Get("token") == "stale" {
					t.Error("upstream resource lost current playback credential")
				}
				if strings.HasSuffix(r.URL.Path, ".m3u8") {
					if r.Header.Get("Range") != "" || r.Header.Get("If-None-Match") != "" || r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("playlist request did not require a complete response")
					}
					w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
					w.Header().Set("ETag", "upstream-etag")
					w.Header().Set("Content-Encoding", "gzip")
					gz := gzip.NewWriter(w)
					defer gz.Close()
					if strings.HasSuffix(r.URL.Path, "hls.fmp4.m3u8") {
						_, _ = fmt.Fprint(gz, "#EXTM3U\n#EXT-X-MEDIA:TYPE=AUDIO,URI=\"audio.m3u8?session=audio\"\n#EXT-X-STREAM-INF:BANDWIDTH=100000\nvideo.m3u8?session=video\n")
					} else {
						_, _ = fmt.Fprintf(gz, "#EXTM3U\n#EXT-X-MAP:URI=\"init.mp4?session=init\"\n#EXT-X-KEY:METHOD=AES-128,URI=\"%skey.key\"\n#EXTINF:2,\ndate/hour/seg.m4s?session=segment&token=stale\n#EXTINF:2,\nhttp://%s%sseg.ts\n", prefix, r.Host, prefix)
					}
					return
				}
				if r.Header.Get("Range") == "bytes=0-3" {
					w.Header().Set("Content-Range", "bytes 0-3/4")
					w.WriteHeader(http.StatusPartialContent)
				}
				_, _ = w.Write([]byte("data"))
			})
			token := makePlayTokenForMedia(t, "live", "camera01", server.ID)
			address := driver.GetStreamLiveAddr(t.Context(), server, proxyServer.URL, "example.test", "live", "camera01", token).HLS
			queue := []string{address}
			visited := make(map[string]bool)
			for len(queue) > 0 {
				address, queue = queue[0], queue[1:]
				if visited[address] {
					continue
				}
				visited[address] = true
				u, err := url.Parse(address)
				if err != nil {
					t.Fatal(err)
				}
				if u.Query().Get("token") != token || !strings.HasPrefix(u.Path, "/proxy/sms/"+sms.MediaServerProxyRoute(server.ID)+prefix) {
					t.Fatalf("resource escaped grant: %s", address)
				}
				req, _ := http.NewRequest(http.MethodGet, address, nil)
				req.Header.Set("Origin", "https://third-party.example")
				req.Header.Set("Range", "bytes=0-3")
				req.Header.Set("If-None-Match", "upstream-etag")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(u.Path, ".m3u8") {
					if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != "" || resp.Header.Get("Content-Encoding") != "" || resp.ContentLength != int64(len(body)) {
						t.Fatalf("invalid rewritten playlist: status=%d headers=%v body=%s", resp.StatusCode, resp.Header, body)
					}
					for _, line := range strings.Split(string(body), "\n") {
						if strings.HasPrefix(line, "#") {
							for _, match := range smsHLSURI.FindAllStringSubmatch(line, -1) {
								queue = append(queue, proxyServer.URL+match[1])
							}
						} else if line != "" {
							queue = append(queue, proxyServer.URL+line)
						}
					}
				} else if resp.StatusCode != http.StatusPartialContent || string(body) != "data" || resp.Header.Get("Content-Range") != "bytes 0-3/4" {
					t.Fatalf("segment Range response changed: %d %q", resp.StatusCode, body)
				}
				if resp.Header.Get("Cache-Control") != "private, no-store" {
					t.Fatal("authenticated response permits shared caching")
				}
				// 第三方无需登录，但每个子资源都不能脱离播放凭据访问。
				q := u.Query()
				q.Del("token")
				u.RawQuery = q.Encode()
				anonymous, err := http.Get(u.String())
				if err != nil {
					t.Fatal(err)
				}
				anonymous.Body.Close()
				if anonymous.StatusCode != http.StatusForbidden {
					t.Fatalf("resource accepted without token: %s", u.Path)
				}
			}
			if len(visited) != 7 {
				t.Fatalf("expected master, two children, init, key and two segments; visited %d", len(visited))
			}
		})
	}
}

func TestProxySMSRejectsUnsafePlaylistAndRedirects(t *testing.T) {
	for _, check := range []struct{ name, location, playlist string }{
		{name: "external playlist URI", playlist: "#EXTM3U\nhttps://other.example/live/camera01/seg.ts\n"},
		{name: "external init URI", playlist: "#EXTM3U\n#EXT-X-MAP:URI=\"//other.example/init.mp4\"\n"},
		{name: "cross stream key", playlist: "#EXTM3U\n#EXT-X-KEY:URI=\"/live/camera02/key.key\"\n"},
		{name: "cross stream segment", playlist: "#EXTM3U\n../camera02/seg.ts\n"},
		{name: "management endpoint", playlist: "#EXTM3U\n/index/api/getServerConfig?app=live&stream=camera01&type=play\n"},
		{name: "oversize playlist", playlist: "#EXTM3U\n#" + strings.Repeat("x", smsPlaylistLimit)},
		{name: "malformed playlist", playlist: "not a playlist"},
		{name: "external redirect", location: "https://other.example/live/camera01/hls.fmp4.m3u8"},
		{name: "management redirect", location: "/index/api/close_stream?app=live&stream=camera01&type=play"},
		{name: "cross stream redirect", location: "/live/camera02/hls.fmp4.m3u8"},
	} {
		t.Run(check.name, func(t *testing.T) {
			proxyServer, server := newSMSProxyTestServer(t, sms.ProtocolZLMediaKit, func(w http.ResponseWriter, r *http.Request) {
				if check.location != "" {
					w.Header().Set("Location", check.location)
					w.WriteHeader(http.StatusFound)
					return
				}
				_, _ = io.WriteString(w, check.playlist)
			})
			token := makePlayTokenForMedia(t, "live", "camera01", server.ID)
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Get(proxyServer.URL + "/proxy/sms/" + sms.MediaServerProxyRoute(server.ID) + "/live/camera01/hls.fmp4.m3u8?token=" + token)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Location") != "" {
				t.Fatalf("unsafe upstream reference forwarded: %d %v", resp.StatusCode, resp.Header)
			}
		})
	}
}

func TestProxySMSPreservesAuthorizedRedirect(t *testing.T) {
	proxyServer, server := newSMSProxyTestServer(t, sms.ProtocolZLMediaKit, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "nested/child.m3u8?session=redirect")
		w.WriteHeader(http.StatusFound)
	})
	token := makePlayTokenForMedia(t, "live", "camera01", server.ID)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(proxyServer.URL + "/proxy/sms/" + sms.MediaServerProxyRoute(server.ID) + "/live/camera01/hls.fmp4.m3u8?token=" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusFound || u.Path != "/proxy/sms/"+sms.MediaServerProxyRoute(server.ID)+"/live/camera01/nested/child.m3u8" || u.Query().Get("token") != token || u.Query().Get("session") != "redirect" {
		t.Fatalf("authorized redirect changed: %d %v", resp.StatusCode, resp.Header)
	}
}
