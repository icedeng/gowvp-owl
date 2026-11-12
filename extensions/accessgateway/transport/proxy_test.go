package transport

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrivateProxyPolicyAndCONNECT(t *testing.T) {
	observed := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			t.Error("非 CONNECT 请求")
		}
		observed <- r.Host + " " + r.Header.Get("Proxy-Authorization")
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		b := make([]byte, 1)
		_, _ = c.Read(b)
	}))
	defer server.Close()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	cfg := ConnectProxy{URL: server.URL, Username: "test", Password: "secret", AllowedTargets: []string{"wss://8.8.8.8/gateway/v1/control"}}
	data, _ := json.Marshal(cfg)
	path := filepath.Join(dir, "egress.json")
	_ = os.WriteFile(path, data, 0600)
	if _, err := LoadProxy(dir, "egress", "wss://9.9.9.9/gateway/v1/control"); err == nil {
		t.Fatal("代理白名单无效")
	}
	p, err := LoadProxy(dir, "egress", cfg.AllowedTargets[0])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := p.DialContext(ctx, "tcp", "8.8.8.8:8443")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if got := <-observed; got != "8.8.8.8:8443 Basic dGVzdDpzZWNyZXQ=" {
		t.Fatal(got)
	}
	if _, err = p.DialContext(ctx, "tcp", "127.0.0.1:8443"); err == nil {
		t.Fatal("代理允许内网目标")
	}
	_ = os.Chmod(path, 0644)
	if _, err = LoadProxy(dir, "egress", cfg.AllowedTargets[0]); err == nil {
		t.Fatal("非私有配置可读取")
	}
	_ = os.Remove(path)
	_ = os.Symlink("/etc/hosts", path)
	if _, err = LoadProxy(dir, "egress", cfg.AllowedTargets[0]); err == nil {
		t.Fatal("允许符号链接配置")
	}
}
func TestCONNECTRejectionAndTLSStillVerified(t *testing.T) {
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(407) }))
	defer denied.Close()
	proxy := &ConnectProxy{URL: denied.URL, Password: "do-not-leak"}
	_, err := proxy.DialContext(t.Context(), "tcp", "8.8.8.8:443")
	if err == nil || !strings.Contains(err.Error(), "407") || strings.Contains(err.Error(), proxy.Password) {
		t.Fatal(err)
	}
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer tlsServer.Close()
	tunnel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, _, e := w.(http.Hijacker).Hijack()
		if e != nil {
			return
		}
		defer client.Close()
		upstream, e := net.DialTimeout("tcp", tlsServer.Listener.Addr().String(), time.Second)
		if e != nil {
			return
		}
		defer upstream.Close()
		_, _ = io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		go io.Copy(upstream, client)
		_, _ = io.Copy(client, upstream)
	}))
	defer tunnel.Close()
	tlsConfig := tlsServer.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.InsecureSkipVerify = false
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := DialVia(ctx, "wss://8.8.8.8/gateway/v1/control", "/gateway/v1/control", tlsConfig, nil, &ConnectProxy{URL: tunnel.URL})
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("代理连接跳过了网关证书主机名验证")
	}
}
