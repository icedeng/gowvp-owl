package gbs

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const registerCertificateSampleCapability = "A: RSA/ECB/PKCS1, RSA/CBC/PKCS1; H: SHA1, MD5,SHA256; S: DES/ECB/PKCS5,3DES/ECB/PKCS5,SCB2"

type registerCertificateTestPKI struct {
	platformCertificatePath string
	platformKeyPath         string
	deviceCertificatePath   string
	platformCertificate     *x509.Certificate
	platformKey             *rsa.PrivateKey
	deviceCertificate       *x509.Certificate
	deviceKey               *rsa.PrivateKey
}

func TestRegisterCertificateCapabilityAndAsymmetricRoundTrip(t *testing.T) {
	pki := newRegisterCertificateTestPKI(t, nil, nil)
	authenticator, err := newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Enabled:      true,
		PlatformCert: pki.platformCertificatePath,
		PlatformKey:  pki.platformKeyPath,
		DeviceCertificates: map[string]string{
			gb10DeviceID: pki.deviceCertificatePath,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	nonce, algorithm, err := authenticator.issue(gb10DeviceID, "192.0.2.10", registerCertificateSampleCapability)
	if err != nil {
		t.Fatal(err)
	}
	if algorithm != "SHA256" {
		t.Fatalf("negotiated digest = %s, want SHA256", algorithm)
	}
	secret := decryptAndVerifyRegisterCertificateNonce(t, pki, nonce, algorithm)
	response, err := registerCertificateDigest(algorithm, secret, []byte(nonce))
	if err != nil {
		t.Fatal(err)
	}
	authorization := sip.AuthFromValue(fmt.Sprintf(`nonce="%s",response="%s",algorithm=%s`, nonce, hex.EncodeToString(response), algorithm))
	if err := authenticator.validate(gb10DeviceID, "192.0.2.10", authorization, "request-1"); err != nil {
		t.Fatalf("valid Asymmetric response rejected: %v", err)
	}
	if err := authenticator.validate(gb10DeviceID, "192.0.2.10", authorization, "request-1"); err != nil {
		t.Fatalf("exact Asymmetric retransmission rejected: %v", err)
	}
	if err := authenticator.validate(gb10DeviceID, "192.0.2.10", authorization, "request-2"); err == nil || !strings.Contains(err.Error(), "replay") {
		t.Fatalf("Asymmetric replay result = %v", err)
	}
	if err := authenticator.validate(gb10DeviceID, "192.0.2.11", authorization, "request-1"); err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("cross-source Asymmetric nonce result = %v", err)
	}
}

func TestRegisterCertificateHandlerCompletesRequiredAuthentication(t *testing.T) {
	pki := newRegisterCertificateTestPKI(t, nil, nil)
	authenticator, err := newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Required:     true,
		PlatformCert: pki.platformCertificatePath,
		PlatformKey:  pki.platformKeyPath,
		DeviceCertificates: map[string]string{
			gb10DeviceID: pki.deviceCertificatePath,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	api.registerCertificateAuth = authenticator
	api.cfg.Password = "digest-must-not-be-used"

	// 使用注销闭环验证同一套 REGISTER 鉴权，避免成功上线后自动发起的设备信息查询
	// 混入本测试；标准 9.1.2.3 明确注销同样必须先完成认证。
	first := newRegisterHandlerTestContext(t, connection, "register-certificate-capability", 0)
	first.XGBVer = string(GBVersion20)
	first.XGBVerRaw = string(GBVersion20)
	first.Request.AppendHeader(&sip.GenericHeader{
		HeaderName: "Authorization",
		Contents:   fmt.Sprintf(`Capability algorithm="%s"`, registerCertificateSampleCapability),
	})
	api.handlerRegister(first)
	challengePayload := assertRegisterHandlerResponsePayload(t, connection, "SIP/2.0 401 Unauthorized")
	challengeValue := responseHeaderValue(challengePayload, "WWW-Authenticate")
	if challengeValue == "" {
		t.Fatalf("Asymmetric challenge missing: %s", challengePayload)
	}
	scheme, challenge, err := parseRegisterAuthorizationHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: challengeValue})
	if err != nil || scheme != "asymmetric" {
		t.Fatalf("parse Asymmetric challenge = scheme:%s auth:%v err:%v", scheme, challenge, err)
	}
	nonce := challenge.Get("nonce")
	algorithm, _, err := canonicalRegisterCertificateDigest(challenge.Get("algorithm"))
	if err != nil {
		t.Fatal(err)
	}
	secret := decryptAndVerifyRegisterCertificateNonce(t, pki, nonce, algorithm)
	response, err := registerCertificateDigest(algorithm, secret, []byte(nonce))
	if err != nil {
		t.Fatal(err)
	}

	second := newRegisterHandlerTestContext(t, connection, "register-certificate-response", 0)
	second.XGBVer = string(GBVersion20)
	second.XGBVerRaw = string(GBVersion20)
	second.Request.AppendHeader(&sip.GenericHeader{
		HeaderName: "Authorization",
		Contents: fmt.Sprintf(`Asymmetric nonce="%s",response="%s",algorithm=%s`,
			nonce, hex.EncodeToString(response), algorithm),
	})
	api.handlerRegister(second)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 200 OK")
	if memory.changeCalls != 1 {
		t.Fatalf("authenticated REGISTER state changes = %d, want 1", memory.changeCalls)
	}
}

func TestRegisterCertificateRequiredRejectsDigestDowngrade(t *testing.T) {
	pki := newRegisterCertificateTestPKI(t, nil, nil)
	authenticator, err := newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Required:     true,
		PlatformCert: pki.platformCertificatePath,
		PlatformKey:  pki.platformKeyPath,
		DeviceCertificates: map[string]string{
			gb10DeviceID: pki.deviceCertificatePath,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	api, memory, connection := newRegisterHandlerTestAPI(t, true)
	api.registerCertificateAuth = authenticator
	ctx := newRegisterHandlerTestContext(t, connection, "register-certificate-downgrade", 3600)
	ctx.Request.AppendHeader(&sip.GenericHeader{
		HeaderName: "Authorization",
		Contents:   `Digest realm="3402000000",nonce="attacker",response="attacker"`,
	})
	api.handlerRegister(ctx)
	assertRegisterHandlerResponse(t, connection, "SIP/2.0 403 certificate REGISTER authentication is required")
	if memory.loadOrStoreCalls != 0 || memory.changeCalls != 0 {
		t.Fatalf("downgraded REGISTER mutated state: load_or_store=%d change=%d", memory.loadOrStoreCalls, memory.changeCalls)
	}
}

func TestRegisterCertificateCRLRejectsRevokedDevice(t *testing.T) {
	now := time.Now()
	caKey := generateRegisterCertificateRSAKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(100),
		Subject:               pkix.Name{CommonName: "GB28181 test device CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	caCertificate := createRegisterCertificate(t, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	pki := newRegisterCertificateTestPKI(t, caCertificate, caKey)
	directory := t.TempDir()
	caPath := filepath.Join(directory, "device-ca.pem")
	writeCertificatePEM(t, caPath, caCertificate)
	crlDER, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		SignatureAlgorithm: x509.SHA256WithRSA,
		RevokedCertificateEntries: []x509.RevocationListEntry{{
			SerialNumber:   new(big.Int).Set(pki.deviceCertificate.SerialNumber),
			RevocationTime: now.Add(-time.Minute),
		}},
		Number:     big.NewInt(1),
		ThisUpdate: now.Add(-time.Minute),
		NextUpdate: now.Add(time.Hour),
	}, caCertificate, caKey)
	if err != nil {
		t.Fatal(err)
	}
	crlPath := filepath.Join(directory, "device.crl")
	writePEM(t, crlPath, "X509 CRL", crlDER)

	_, err = newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Enabled:      true,
		PlatformCert: pki.platformCertificatePath,
		PlatformKey:  pki.platformKeyPath,
		DeviceCA:     caPath,
		CRL:          crlPath,
		DeviceCertificates: map[string]string{
			gb10DeviceID: pki.deviceCertificatePath,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked device certificate result = %v", err)
	}
}

func TestRegisterCertificateCRLRequiresApplicableIssuer(t *testing.T) {
	now := time.Now()
	deviceCAKey := generateRegisterCertificateRSAKey(t)
	deviceCATemplate := &x509.Certificate{
		SerialNumber: big.NewInt(200), Subject: pkix.Name{CommonName: "device CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, IsCA: true, BasicConstraintsValid: true,
	}
	deviceCA := createRegisterCertificate(t, deviceCATemplate, deviceCATemplate, &deviceCAKey.PublicKey, deviceCAKey)
	otherCAKey := generateRegisterCertificateRSAKey(t)
	otherCATemplate := &x509.Certificate{
		SerialNumber: big.NewInt(201), Subject: pkix.Name{CommonName: "other CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, IsCA: true, BasicConstraintsValid: true,
	}
	otherCA := createRegisterCertificate(t, otherCATemplate, otherCATemplate, &otherCAKey.PublicKey, otherCAKey)
	pki := newRegisterCertificateTestPKI(t, deviceCA, deviceCAKey)
	directory := t.TempDir()
	caPath := filepath.Join(directory, "ca-bundle.pem")
	caBundle := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: deviceCA.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherCA.Raw})...)
	if err := os.WriteFile(caPath, caBundle, 0o600); err != nil {
		t.Fatal(err)
	}
	crlDER, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		SignatureAlgorithm: x509.SHA256WithRSA,
		Number:             big.NewInt(1),
		ThisUpdate:         now.Add(-time.Minute),
		NextUpdate:         now.Add(time.Hour),
	}, otherCA, otherCAKey)
	if err != nil {
		t.Fatal(err)
	}
	crlPath := filepath.Join(directory, "other-ca.crl")
	writePEM(t, crlPath, "X509 CRL", crlDER)
	_, err = newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Enabled: true, PlatformCert: pki.platformCertificatePath, PlatformKey: pki.platformKeyPath,
		DeviceCA: caPath, CRL: crlPath,
		DeviceCertificates: map[string]string{gb10DeviceID: pki.deviceCertificatePath},
	})
	if err == nil || !strings.Contains(err.Error(), "applies") {
		t.Fatalf("unrelated CRL result = %v", err)
	}
}

func TestCascadeRegisterCertificateAuthenticationRoundTrip(t *testing.T) {
	pki := newRegisterCertificateTestPKI(t, nil, nil)
	serverAuthenticator, err := newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Enabled:      true,
		PlatformCert: pki.platformCertificatePath,
		PlatformKey:  pki.platformKeyPath,
		DeviceCertificates: map[string]string{
			gb10DeviceID: pki.deviceCertificatePath,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clientAuthenticator, err := newCascadeRegisterCertificateAuthenticator(conf.SIPUpstreamRegisterCertificateAuth{
		Required:   true,
		LocalCert:  pki.deviceCertificatePath,
		LocalKey:   writeRegisterCertificatePrivateKey(t, pki.deviceKey),
		ServerCert: pki.platformCertificatePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	nonce, algorithm, err := serverAuthenticator.issue(gb10DeviceID, "", cascadeRegisterCertificateCapability)
	if err != nil {
		t.Fatal(err)
	}
	challenge := sip.AuthFromValue(fmt.Sprintf(`nonce="%s",algorithm="A:RSA/ECB/PKCS1&%s"`, nonce, algorithm))
	authorizationValue, err := clientAuthenticator.asymmetricAuthorization(challenge)
	if err != nil {
		t.Fatal(err)
	}
	scheme, authorization, err := parseRegisterAuthorizationHeader(&sip.GenericHeader{
		HeaderName: "Authorization", Contents: authorizationValue,
	})
	if err != nil || scheme != "asymmetric" {
		t.Fatalf("cascade Asymmetric Authorization parse = scheme:%s auth:%v err:%v", scheme, authorization, err)
	}
	if err := serverAuthenticator.validate(gb10DeviceID, "", authorization, "cascade-register"); err != nil {
		t.Fatalf("cascade Asymmetric Authorization rejected by server: %v", err)
	}
}

func TestCascadeRegisterCertificateRejectsUntrustedServerProof(t *testing.T) {
	pki := newRegisterCertificateTestPKI(t, nil, nil)
	untrusted := newRegisterCertificateTestPKI(t, nil, nil)
	serverAuthenticator, err := newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Enabled:      true,
		PlatformCert: pki.platformCertificatePath,
		PlatformKey:  pki.platformKeyPath,
		DeviceCertificates: map[string]string{
			gb10DeviceID: pki.deviceCertificatePath,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clientAuthenticator, err := newCascadeRegisterCertificateAuthenticator(conf.SIPUpstreamRegisterCertificateAuth{
		Required:   true,
		LocalCert:  pki.deviceCertificatePath,
		LocalKey:   writeRegisterCertificatePrivateKey(t, pki.deviceKey),
		ServerCert: untrusted.platformCertificatePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	nonce, algorithm, err := serverAuthenticator.issue(gb10DeviceID, "", cascadeRegisterCertificateCapability)
	if err != nil {
		t.Fatal(err)
	}
	challenge := sip.AuthFromValue(fmt.Sprintf(`nonce="%s",algorithm="A:RSA/ECB/PKCS1&%s"`, nonce, algorithm))
	_, err = clientAuthenticator.asymmetricAuthorization(challenge)
	if err == nil || !strings.Contains(err.Error(), "server identity") {
		t.Fatalf("untrusted cascade server proof result = %v", err)
	}
}

func TestCascadeWorkerCompletesCertificateRegisterFlow(t *testing.T) {
	pki := newRegisterCertificateTestPKI(t, nil, nil)
	deviceKeyPath := writeRegisterCertificatePrivateKey(t, pki.deviceKey)
	serverAuthenticator, err := newRegisterCertificateAuthenticator(conf.SIPRegisterCertificateAuth{
		Enabled:      true,
		PlatformCert: pki.platformCertificatePath,
		PlatformKey:  pki.platformKeyPath,
		DeviceCertificates: map[string]string{
			gb10DeviceID: pki.deviceCertificatePath,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	platform, err := normalizeCascadePlatform(conf.SIPUpstream{
		Name:              "certificate-upstream",
		Enabled:           true,
		ServerID:          gb10PlatformID,
		Host:              "192.0.2.30",
		Port:              5060,
		LocalID:           gb10DeviceID,
		LocalHost:         "192.0.2.20",
		Version:           string(GBVersion20),
		Expires:           3600,
		KeepaliveInterval: conf.Duration(60 * time.Second),
		RegisterCertificateAuth: conf.SIPUpstreamRegisterCertificateAuth{
			Required:   true,
			LocalCert:  pki.deviceCertificatePath,
			LocalKey:   deviceKeyPath,
			ServerCert: pki.platformCertificatePath,
		},
	}, conf.SIP{ID: gb10DeviceID, Domain: "3402000000", Host: "192.0.2.20", Port: 15060}, "")
	if err != nil {
		t.Fatal(err)
	}
	worker := newCascadeWorker(nil, platform)
	requests := 0
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		requests++
		headers := request.GetHeaders("Authorization")
		if len(headers) != 1 {
			t.Fatalf("cascade REGISTER Authorization headers = %d, want 1", len(headers))
		}
		scheme, authorization, err := parseRegisterAuthorizationHeader(headers[0])
		if err != nil {
			t.Fatal(err)
		}
		switch requests {
		case 1:
			if scheme != "capability" {
				t.Fatalf("initial cascade REGISTER scheme = %s, want capability", scheme)
			}
			nonce, algorithm, err := serverAuthenticator.issue(gb10DeviceID, "", authorization.Get("algorithm"))
			if err != nil {
				t.Fatal(err)
			}
			response := sip.NewResponseFromRequest("", request, 401, "Unauthorized", nil)
			response.AppendHeader(&sip.GenericHeader{
				HeaderName: "WWW-Authenticate",
				Contents:   fmt.Sprintf(`Asymmetric nonce="%s",algorithm="A:RSA/ECB/PKCS1&%s"`, nonce, algorithm),
			})
			return response, nil
		case 2:
			if scheme != "asymmetric" {
				t.Fatalf("second cascade REGISTER scheme = %s, want asymmetric", scheme)
			}
			fingerprint := registerRequestFingerprint(request, strings.ToLower(authorization.Get("response")))
			if err := serverAuthenticator.validate(gb10DeviceID, "", authorization, fingerprint); err != nil {
				t.Fatalf("server rejected cascade certificate REGISTER: %v", err)
			}
			response := sip.NewResponseFromRequest("", request, 200, "OK", nil)
			response.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
			return response, nil
		default:
			t.Fatalf("unexpected cascade REGISTER request %d", requests)
			return nil, nil
		}
	}
	if err := worker.register(context.Background(), 3600); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || !worker.snapshot().Registered {
		t.Fatalf("cascade certificate REGISTER requests=%d status=%+v", requests, worker.snapshot())
	}
}

func TestCascadeCertificateRegisterRejectsDigestDowngrade(t *testing.T) {
	pki := newRegisterCertificateTestPKI(t, nil, nil)
	authenticator, err := newCascadeRegisterCertificateAuthenticator(conf.SIPUpstreamRegisterCertificateAuth{
		Required:   true,
		LocalCert:  pki.deviceCertificatePath,
		LocalKey:   writeRegisterCertificatePrivateKey(t, pki.deviceKey),
		ServerCert: pki.platformCertificatePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	platform := cascadePlatform{
		name: "required-certificate", serverID: gb10PlatformID, remoteDomain: "3402000000",
		remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060}, transport: "udp",
		localID: gb10DeviceID, localDomain: "3402000000", localHost: "192.0.2.20", localPort: 15060,
		version: GBVersion20, expires: 3600, registerCertificateAuth: authenticator,
	}
	worker := newCascadeWorker(nil, platform)
	worker.exchange = func(_ context.Context, request *sip.Request) (*sip.Response, error) {
		response := sip.NewResponseFromRequest("", request, 401, "Unauthorized", nil)
		response.AppendHeader(&sip.GenericHeader{
			HeaderName: "WWW-Authenticate",
			Contents:   `Digest realm="3402000000",nonce="downgrade",algorithm=MD5`,
		})
		return response, nil
	}
	err = worker.register(context.Background(), 3600)
	if err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("certificate-to-Digest downgrade result = %v", err)
	}
}

func TestParseRegisterCertificateStandardHeaders(t *testing.T) {
	scheme, capability, err := parseRegisterAuthorizationHeader(&sip.GenericHeader{
		HeaderName: "Authorization",
		Contents:   fmt.Sprintf(`Capability algorithm="%s"`, registerCertificateSampleCapability),
	})
	if err != nil || scheme != "capability" || capability.Get("algorithm") != registerCertificateSampleCapability {
		t.Fatalf("Capability parse = scheme:%s algorithm:%q err:%v", scheme, capability.Get("algorithm"), err)
	}
	if algorithm, err := selectRegisterCertificateDigest(capability.Get("algorithm")); err != nil || algorithm != "SHA256" {
		t.Fatalf("Capability negotiation = %s, %v", algorithm, err)
	}
	scheme, asymmetric, err := parseRegisterAuthorizationHeader(&sip.GenericHeader{
		HeaderName: "Authorization",
		Contents:   `Asymmetric nonce="a&b",response="9625d92d1bddea7a911926e0db054968",algorithm=SHA1`,
	})
	if err != nil || scheme != "asymmetric" || asymmetric.Get("nonce") != "a&b" || asymmetric.Get("algorithm") != "SHA1" {
		t.Fatalf("Asymmetric parse = scheme:%s auth:%v err:%v", scheme, asymmetric, err)
	}
}

func TestRegisterCertificateCapabilityUsesHashSectionOnly(t *testing.T) {
	algorithm, err := selectRegisterCertificateDigest("A:RSA/ECB/PKCS1;H:MD5;S:FAKE-SHA256")
	if err != nil || algorithm != "MD5" {
		t.Fatalf("hash-section negotiation = %s, %v", algorithm, err)
	}
}

func TestCascadeRegisterChallengePrefersConfiguredCertificateScheme(t *testing.T) {
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodRegister, "multi-auth-challenge", nil)
	response := sip.NewResponseFromRequest("", request, 401, "Unauthorized", nil)
	response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3402000000",nonce="digest"`})
	response.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Asymmetric nonce="a&b",algorithm="A:RSA/ECB/PKCS1&SHA256"`})
	scheme, _, err := cascadeRegisterChallenge(response, true)
	if err != nil || scheme != "asymmetric" {
		t.Fatalf("certificate-preferred challenge = %s, %v", scheme, err)
	}
	scheme, _, err = cascadeRegisterChallenge(response, false)
	if err != nil || scheme != "digest" {
		t.Fatalf("Digest-preferred challenge = %s, %v", scheme, err)
	}
}

func decryptAndVerifyRegisterCertificateNonce(t *testing.T, pki registerCertificateTestPKI, nonce, algorithm string) []byte {
	t.Helper()
	parts := strings.Split(nonce, "&")
	if len(parts) != 2 {
		t.Fatalf("Asymmetric nonce parts = %d, want 2", len(parts))
	}
	serverProof, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	deviceSecret, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	secret, err := rsa.DecryptPKCS1v15(nil, pki.deviceKey, deviceSecret)
	if err != nil {
		t.Fatalf("device private-key decrypt b: %v", err)
	}
	digest, err := registerCertificateDigest(algorithm, secret)
	if err != nil {
		t.Fatal(err)
	}
	platformPublicKey := pki.platformCertificate.PublicKey.(*rsa.PublicKey)
	if err := rsa.VerifyPKCS1v15(platformPublicKey, crypto.Hash(0), digest, serverProof); err != nil {
		t.Fatalf("platform public-key verify a: %v", err)
	}
	return secret
}

func newRegisterCertificateTestPKI(t *testing.T, deviceIssuer *x509.Certificate, deviceIssuerKey *rsa.PrivateKey) registerCertificateTestPKI {
	t.Helper()
	now := time.Now()
	platformKey := generateRegisterCertificateRSAKey(t)
	platformTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "GB28181 test platform"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	platformCertificate := createRegisterCertificate(t, platformTemplate, platformTemplate, &platformKey.PublicKey, platformKey)
	deviceKey := generateRegisterCertificateRSAKey(t)
	deviceTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "GB28181 test device"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
	}
	if deviceIssuer == nil {
		deviceIssuer = deviceTemplate
		deviceIssuerKey = deviceKey
	}
	deviceCertificate := createRegisterCertificate(t, deviceTemplate, deviceIssuer, &deviceKey.PublicKey, deviceIssuerKey)
	directory := t.TempDir()
	pki := registerCertificateTestPKI{
		platformCertificatePath: filepath.Join(directory, "platform.pem"),
		platformKeyPath:         filepath.Join(directory, "platform-key.pem"),
		deviceCertificatePath:   filepath.Join(directory, "device.pem"),
		platformCertificate:     platformCertificate,
		platformKey:             platformKey,
		deviceCertificate:       deviceCertificate,
		deviceKey:               deviceKey,
	}
	writeCertificatePEM(t, pki.platformCertificatePath, platformCertificate)
	writePEM(t, pki.platformKeyPath, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(platformKey))
	writeCertificatePEM(t, pki.deviceCertificatePath, deviceCertificate)
	return pki
}

func generateRegisterCertificateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func createRegisterCertificate(t *testing.T, template, parent *x509.Certificate, publicKey *rsa.PublicKey, signer *rsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func writeCertificatePEM(t *testing.T, path string, certificate *x509.Certificate) {
	t.Helper()
	writePEM(t, path, "CERTIFICATE", certificate.Raw)
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeRegisterCertificatePrivateKey(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "register-private-key.pem")
	writePEM(t, path, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key))
	return path
}

func responseHeaderValue(payload, name string) string {
	prefix := strings.ToLower(name) + ":"
	for _, line := range strings.Split(payload, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}
