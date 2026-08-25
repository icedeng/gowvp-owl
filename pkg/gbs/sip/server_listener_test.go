package sip

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStartUDPServerReturnsBindError(t *testing.T) {
	occupied, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	server := NewServer(&Address{})
	defer server.Close()
	if err := server.StartUDPServer(occupied.LocalAddr().String()); err == nil || !strings.Contains(err.Error(), "net.ListenUDP") {
		t.Fatalf("occupied UDP listener error = %v", err)
	}
}

func TestStartTCPServerReturnsBindError(t *testing.T) {
	occupied, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()

	server := NewServer(&Address{})
	defer server.Close()
	if err := server.StartTCPServer(occupied.Addr().String()); err == nil || !strings.Contains(err.Error(), "net.ListenTCP") {
		t.Fatalf("occupied TCP listener error = %v", err)
	}
}

func TestStartTLSServerReturnsCertificateError(t *testing.T) {
	server := NewServer(&Address{})
	defer server.Close()
	if err := server.StartTLSServer("127.0.0.1:0", "missing.crt", "missing.key"); err == nil || !strings.Contains(err.Error(), "tls.LoadX509KeyPair") {
		t.Fatalf("invalid TLS certificate error = %v", err)
	}
}

func TestTLSListenerConfigRequiresAndVerifiesClientCertificate(t *testing.T) {
	certFile, keyFile := writeTestTLSCertificate(t)
	config, err := newTLSListenerConfig(TLSListenerOptions{
		CertFile: certFile, KeyFile: keyFile, ClientCAFile: certFile, RequireClientCert: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.MinVersion != tls.VersionTLS12 || config.ClientAuth != tls.RequireAndVerifyClientCert || config.ClientCAs == nil {
		t.Fatalf("TLS listener config = %+v", config)
	}
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	contents, err := os.ReadFile(certFile)
	if err != nil || !roots.AppendCertsFromPEM(contents) {
		t.Fatalf("load test root CA: %v", err)
	}
	clientConfig := &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "localhost",
		Certificates: []tls.Certificate{certificate},
	}
	if err := runTestTLSHandshake(config, clientConfig); err != nil {
		t.Fatalf("trusted TLS client certificate rejected: %v", err)
	}
	clientConfig.Certificates = nil
	if err := runTestTLSHandshake(config, clientConfig); err == nil {
		t.Fatal("TLS client without a certificate was accepted")
	}
}

func TestTLSListenerConfigRejectsMissingOrInvalidClientCA(t *testing.T) {
	certFile, keyFile := writeTestTLSCertificate(t)
	if _, err := newTLSListenerConfig(TLSListenerOptions{
		CertFile: certFile, KeyFile: keyFile, RequireClientCert: true,
	}); err == nil || !strings.Contains(err.Error(), "client CA is required") {
		t.Fatalf("missing client CA error = %v", err)
	}
	invalidCA := t.TempDir() + "/invalid-ca.pem"
	if err := os.WriteFile(invalidCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newTLSListenerConfig(TLSListenerOptions{
		CertFile: certFile, KeyFile: keyFile, ClientCAFile: invalidCA,
	}); err == nil || !strings.Contains(err.Error(), "valid certificate") {
		t.Fatalf("invalid client CA error = %v", err)
	}
}

func writeTestTLSCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile := dir+"/certificate.pem", dir+"/private-key.pem"
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func runTestTLSHandshake(serverConfig, clientConfig *tls.Config) error {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()
	deadline := time.Now().Add(2 * time.Second)
	_ = serverConn.SetDeadline(deadline)
	_ = clientConn.SetDeadline(deadline)
	serverTLS := tls.Server(serverConn, serverConfig.Clone())
	clientTLS := tls.Client(clientConn, clientConfig.Clone())
	serverResult := make(chan error, 1)
	go func() { serverResult <- serverTLS.Handshake() }()
	clientErr := clientTLS.Handshake()
	serverErr := <-serverResult
	if serverErr != nil {
		return serverErr
	}
	return clientErr
}

func TestStartedUDPAndTCPServersCloseCleanly(t *testing.T) {
	server := NewServer(&Address{})
	if err := server.StartUDPServer("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if err := server.StartTCPServer("127.0.0.1:0"); err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Close()
}
