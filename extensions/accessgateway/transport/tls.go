package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

func ClientTLS(ca, cert, key []byte) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid CA bundle")
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("invalid client certificate/key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, e := x509.ParseCertificate(der)
		if e != nil {
			return nil, e
		}
		intermediates.AddCert(c)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return nil, fmt.Errorf("invalid client certificate chain/use: %w", err)
	}
	pair.Leaf = leaf
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{pair}}, nil
}
func ServerTLS(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid client CA")
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"}}, nil
}

// PublicDialer 在每次拨号前检查所有 DNS 结果并固定实际连接地址，阻止 DNS rebinding。
// 不读取环境代理；需要代理时必须由宿主注入受控实现。
func PublicDialer(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS returned no addresses")
	}
	for _, ip := range ips {
		if !protocol.ValidPublicIP(ip.Unmap().String()) {
			return nil, protocol.Failure(protocol.Forbidden, "gateway resolved to a non-public address")
		}
	}
	var last error
	d := net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	for _, ip := range ips {
		conn, e := d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if e == nil {
			return conn, nil
		}
		last = e
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}
func EndpointURL(raw, path string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != protocol.ControlPath || u.RawPath != "" {
		return "", fmt.Errorf("invalid gateway WSS URL")
	}
	if !strings.HasPrefix(path, "/gateway/v1/") {
		return "", fmt.Errorf("invalid gateway path")
	}
	u.Path = path
	return u.String(), nil
}
func Dial(ctx context.Context, raw, path string, config *tls.Config, headers http.Header) (*websocket.Conn, error) {
	target, err := EndpointURL(raw, path)
	if err != nil {
		return nil, err
	}
	d := websocket.Dialer{TLSClientConfig: config, NetDialContext: PublicDialer, HandshakeTimeout: 10 * time.Second, Subprotocols: []string{protocol.Subprotocol}, EnableCompression: false}
	conn, response, err := d.DialContext(ctx, target, headers)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	if conn.Subprotocol() != protocol.Subprotocol {
		_ = conn.Close()
		return nil, fmt.Errorf("gateway subprotocol mismatch")
	}
	return conn, nil
}
func Probe(ctx context.Context, raw string, config *tls.Config) (protocol.Probe, error) {
	var result protocol.Probe
	target, err := EndpointURL(raw, protocol.ProbePath)
	if err != nil {
		return result, err
	}
	target = strings.Replace(target, "wss://", "https://", 1)
	transport := &http.Transport{TLSClientConfig: config, DialContext: PublicDialer, ForceAttemptHTTP2: false, TLSHandshakeTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return result, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result, fmt.Errorf("gateway probe HTTP %d", resp.StatusCode)
	}
	data, err := readLimited(resp.Body, protocol.MaxControlBytes)
	if err != nil {
		return result, err
	}
	err = protocol.DecodeStrict(data, &result)
	return result, err
}
