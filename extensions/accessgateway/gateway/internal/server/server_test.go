package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/config"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/controller"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

func testCertificates(t *testing.T) (*tls.Config, *tls.Config, *tls.Config) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	root, e = x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	issue := func(cn string, usage x509.ExtKeyUsage, serial int64) tls.Certificate {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		data, e := x509.CreateCertificate(rand.Reader, leaf, root, &k.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		keyBytes, e := x509.MarshalPKCS8PrivateKey(k)
		if e != nil {
			t.Fatal(e)
		}
		pair, e := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: data}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}))
		if e != nil {
			t.Fatal(e)
		}
		return pair
	}
	server := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{issue("gateway", x509.ExtKeyUsageServerAuth, 2)}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"}}
	client := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{issue("platform-a", x509.ExtKeyUsageClientAuth, 3)}}
	wrong := client.Clone()
	wrong.Certificates = []tls.Certificate{issue("unauthorized", x509.ExtKeyUsageClientAuth, 4)}
	return server, client, wrong
}
func TestMTLSHelloProbeAndOwnerIsolation(t *testing.T) {
	tlsServer, tlsClient, tlsWrong := testCertificates(t)
	cfg := config.Config{GatewayID: "b1", PublicIP: "203.0.113.1", MediaPortRange: "30000-30199", TLSCertFile: "cert", TLSKeyFile: "key", ClientCAFile: "ca", Platform: config.Platform{ID: "a", ClientCertificateCN: "platform-a"}}
	cfg.Defaults()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	store, e := state.Open(dir, "b1", "a")
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	control, e := controller.New(context.Background(), cfg, store)
	if e != nil {
		t.Fatal(e)
	}
	defer control.Close()
	httpServer := httptest.NewUnstartedServer(New(cfg, control))
	httpServer.TLS = tlsServer
	httpServer.Config.ErrorLog = log.New(io.Discard, "", 0)
	httpServer.StartTLS()
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsClient}, Timeout: time.Second}
	defer client.CloseIdleConnections()
	checkProbe := func(client *http.Client, status int) protocol.Probe {
		t.Helper()
		resp, e := client.Get(httpServer.URL + protocol.ProbePath)
		if e != nil {
			t.Fatal(e)
		}
		defer resp.Body.Close()
		if resp.StatusCode != status {
			t.Fatalf("probe HTTP %d, expected %d", resp.StatusCode, status)
		}
		var v protocol.Probe
		if status == 200 {
			if e = json.NewDecoder(resp.Body).Decode(&v); e != nil {
				t.Fatal(e)
			}
		}
		return v
	}
	wrongClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsWrong}, Timeout: time.Second}
	defer wrongClient.CloseIdleConnections()
	checkProbe(wrongClient, 403)
	if checkProbe(client, 200).OwnerConnected {
		t.Fatal("probe acquired control ownership")
	}
	dialer := websocket.Dialer{TLSClientConfig: tlsClient, Subprotocols: []string{protocol.Subprotocol}, HandshakeTimeout: time.Second}
	target := strings.Replace(httpServer.URL, "https://", "wss://", 1) + protocol.ControlPath
	ws, _, e := dialer.DialContext(ctx, target, nil)
	if e != nil {
		t.Fatal(e)
	}
	peer := transport.NewPeer(ctx, ws, transport.Identity{GatewayID: "b1", PlatformID: "a", OwnerEpoch: "1"})
	defer peer.Close()
	instance := uuid.NewString()
	reply, e := peer.Call(ctx, "hello", protocol.Hello{ClientInstanceID: instance, SupportedVersions: []int{1}, RequiredCapabilities: []string{"sip_udp"}, LastAppliedRevision: "0"})
	if e != nil {
		t.Fatal(e)
	}
	if reply.Payload.(*protocol.HelloAck).ConfigRevision != "0" || peer.Identity().Generation == nil {
		t.Fatal("hello did not bind epochs")
	}
	if !checkProbe(client, 200).OwnerConnected {
		t.Fatal("probe lost active owner")
	}
	if conn, resp, e := dialer.DialContext(ctx, target, nil); e == nil {
		conn.Close()
		t.Fatal("duplicate control owner accepted")
	} else if resp == nil || resp.StatusCode != 409 {
		t.Fatal("duplicate owner did not return HTTP409", e)
	}
	hb, e := peer.Call(ctx, "heartbeat", protocol.Heartbeat{HeartbeatSeq: "1", RenewSessionIDs: []string{}, RenewTransferIDs: []string{}})
	if e != nil || hb.Payload.(*protocol.HeartbeatAck).HeartbeatSeq != "1" {
		t.Fatal("heartbeat failed", e)
	}
	if checkProbe(client, 200).ConfigRevision != "0" {
		t.Fatal("read-only probe changed configuration")
	}
	if store.Snapshot().ClientInstanceID != instance || store.Snapshot().OwnerEpoch != "1" {
		t.Fatal("accepted owner not persisted")
	}
}
