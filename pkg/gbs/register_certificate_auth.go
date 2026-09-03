package gbs

import (
	"bytes"
	"crypto"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	registerCertificateNonceTTL  = 5 * time.Minute
	maxRegisterCertificateNonce  = 4096
	registerCertificateSecretLen = 32
)

type registerCertificateDevice struct {
	certificate *x509.Certificate
	publicKey   *rsa.PublicKey
}

type registerCertificateNonceState struct {
	DeviceID            string
	SourceIP            string
	Secret              []byte
	Algorithm           string
	IssuedAt            time.Time
	Expires             time.Time
	AcceptedFingerprint string
}

// registerCertificateAuthenticator 实现 GB/T 28181-2011 9.1.2.2/J.2 定义、
// 2014 修改补充文件继续沿用且 2016 保留的 Capability/Asymmetric REGISTER 挑战应答。
// 它与传输层 TLS 证书相互独立。
type registerCertificateAuthenticator struct {
	required               bool
	platformCertificate    *x509.Certificate
	platformPrivateKey     *rsa.PrivateKey
	devices                map[string]registerCertificateDevice
	certificateAuthorities []*x509.Certificate
	revocationLists        []*x509.RevocationList

	now    func() time.Time
	random io.Reader
	mu     sync.Mutex
	nonces map[string]registerCertificateNonceState
}

func newRegisterCertificateAuthenticator(config conf.SIPRegisterCertificateAuth) (*registerCertificateAuthenticator, error) {
	if !config.Active() {
		return nil, nil
	}
	if err := conf.ValidateRegisterCertificateAuthConfig(config); err != nil {
		return nil, err
	}
	platformCertificates, err := loadX509Certificates(config.PlatformCert)
	if err != nil {
		return nil, fmt.Errorf("load platform REGISTER certificate: %w", err)
	}
	platformPrivateKey, err := loadRSAPrivateKey(config.PlatformKey)
	if err != nil {
		return nil, fmt.Errorf("load platform REGISTER private key: %w", err)
	}
	platformCertificate := platformCertificates[0]
	platformPublicKey, ok := platformCertificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("platform REGISTER certificate public key is not RSA")
	}
	if err := validateRSAKeySize(platformPublicKey); err != nil {
		return nil, fmt.Errorf("platform REGISTER certificate: %w", err)
	}
	now := time.Now()
	if now.Before(platformCertificate.NotBefore) || now.After(platformCertificate.NotAfter) {
		return nil, fmt.Errorf("platform REGISTER certificate is outside its validity period")
	}
	if platformPublicKey.E != platformPrivateKey.PublicKey.E || platformPublicKey.N.Cmp(platformPrivateKey.PublicKey.N) != 0 {
		return nil, fmt.Errorf("platform REGISTER certificate and private key do not match")
	}

	authenticator := &registerCertificateAuthenticator{
		required:            config.Required,
		platformCertificate: platformCertificate,
		platformPrivateKey:  platformPrivateKey,
		devices:             make(map[string]registerCertificateDevice, len(config.DeviceCertificates)),
		now:                 time.Now,
		random:              rand.Reader,
		nonces:              make(map[string]registerCertificateNonceState),
	}

	var roots *x509.CertPool
	if strings.TrimSpace(config.DeviceCA) != "" {
		authorities, err := loadX509Certificates(config.DeviceCA)
		if err != nil {
			return nil, fmt.Errorf("load device REGISTER CA: %w", err)
		}
		roots = x509.NewCertPool()
		for _, authority := range authorities {
			roots.AddCert(authority)
		}
		authenticator.certificateAuthorities = authorities
	}
	if strings.TrimSpace(config.CRL) != "" {
		authenticator.revocationLists, err = loadX509RevocationLists(config.CRL)
		if err != nil {
			return nil, fmt.Errorf("load device REGISTER CRL: %w", err)
		}
		if err := authenticator.validateRevocationLists(authenticator.now()); err != nil {
			return nil, err
		}
	}

	for rawDeviceID, path := range config.DeviceCertificates {
		deviceID := strings.TrimSpace(rawDeviceID)
		certificates, err := loadX509Certificates(path)
		if err != nil {
			return nil, fmt.Errorf("load REGISTER certificate for device %s: %w", deviceID, err)
		}
		certificate := certificates[0]
		publicKey, ok := certificate.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("REGISTER certificate for device %s does not contain an RSA public key", deviceID)
		}
		if err := validateRSAKeySize(publicKey); err != nil {
			return nil, fmt.Errorf("REGISTER certificate for device %s: %w", deviceID, err)
		}
		if err := verifyPinnedOrChainedCertificate(certificate, certificates[1:], roots, authenticator.now()); err != nil {
			return nil, fmt.Errorf("verify REGISTER certificate for device %s: %w", deviceID, err)
		}
		if err := authenticator.checkCertificateRevocation(certificate, authenticator.now()); err != nil {
			return nil, fmt.Errorf("verify REGISTER certificate for device %s: %w", deviceID, err)
		}
		authenticator.devices[deviceID] = registerCertificateDevice{certificate: certificate, publicKey: publicKey}
	}
	return authenticator, nil
}

func validateRSAKeySize(publicKey *rsa.PublicKey) error {
	if publicKey == nil || publicKey.N == nil {
		return fmt.Errorf("RSA public key is missing")
	}
	bits := publicKey.N.BitLen()
	if bits < 1024 || bits > 4096 {
		return fmt.Errorf("RSA public key size %d is outside the supported 1024-4096 bit range", bits)
	}
	return nil
}

func loadX509Certificates(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	var certificates []*x509.Certificate
	rest := data
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		certificate, err := x509.ParseCertificate(data)
		if err != nil {
			return nil, fmt.Errorf("no X.509 certificate found: %w", err)
		}
		certificates = append(certificates, certificate)
	}
	return certificates, nil
}

func loadRSAPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block != nil {
		data = block.Bytes
	}
	if privateKey, err := x509.ParsePKCS1PrivateKey(data); err == nil {
		if err := privateKey.Validate(); err != nil {
			return nil, err
		}
		return privateKey, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("private key is neither PKCS#1 nor PKCS#8 RSA: %w", err)
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA")
	}
	if err := privateKey.Validate(); err != nil {
		return nil, err
	}
	return privateKey, nil
}

func loadX509RevocationLists(path string) ([]*x509.RevocationList, error) {
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		return nil, err
	}
	var lists []*x509.RevocationList
	rest := data
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			break
		}
		rest = remaining
		if block.Type != "X509 CRL" && block.Type != "CRL" {
			continue
		}
		list, err := x509.ParseRevocationList(block.Bytes)
		if err != nil {
			return nil, err
		}
		lists = append(lists, list)
	}
	if len(lists) == 0 {
		list, err := x509.ParseRevocationList(data)
		if err != nil {
			return nil, fmt.Errorf("no X.509 CRL found: %w", err)
		}
		lists = append(lists, list)
	}
	return lists, nil
}

func verifyPinnedOrChainedCertificate(certificate *x509.Certificate, chain []*x509.Certificate, roots *x509.CertPool, now time.Time) error {
	if certificate == nil {
		return fmt.Errorf("certificate is missing")
	}
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return fmt.Errorf("certificate is outside its validity period")
	}
	if roots == nil {
		// 未配置 CA 时，设备 ID 到证书文件的映射本身就是固定证书信任锚。
		return nil
	}
	intermediates := x509.NewCertPool()
	for _, intermediate := range chain {
		intermediates.AddCert(intermediate)
	}
	_, err := certificate.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	return err
}

func (authenticator *registerCertificateAuthenticator) validateRevocationLists(now time.Time) error {
	return validateX509RevocationLists(authenticator.revocationLists, authenticator.certificateAuthorities, now)
}

func validateX509RevocationLists(lists []*x509.RevocationList, authorities []*x509.Certificate, now time.Time) error {
	for _, list := range lists {
		if list == nil {
			return fmt.Errorf("device REGISTER CRL is missing")
		}
		if list.ThisUpdate.After(now.Add(5 * time.Minute)) {
			return fmt.Errorf("device REGISTER CRL is not yet valid")
		}
		if list.NextUpdate.IsZero() || !list.NextUpdate.After(now) {
			return fmt.Errorf("device REGISTER CRL is expired or has no next update")
		}
		verified := false
		for _, authority := range authorities {
			if authority == nil || !bytes.Equal(list.RawIssuer, authority.RawSubject) {
				continue
			}
			if err := list.CheckSignatureFrom(authority); err == nil {
				verified = true
				break
			}
		}
		if !verified {
			return fmt.Errorf("device REGISTER CRL signature cannot be verified by the configured CA")
		}
	}
	return nil
}

func (authenticator *registerCertificateAuthenticator) checkCertificateRevocation(certificate *x509.Certificate, now time.Time) error {
	return checkX509CertificateRevocation(certificate, authenticator.revocationLists, authenticator.certificateAuthorities, now)
}

func checkX509CertificateRevocation(certificate *x509.Certificate, lists []*x509.RevocationList, authorities []*x509.Certificate, now time.Time) error {
	if certificate == nil {
		return fmt.Errorf("certificate is missing")
	}
	if err := validateX509RevocationLists(lists, authorities, now); err != nil {
		return err
	}
	applicable := false
	for _, list := range lists {
		if !bytes.Equal(list.RawIssuer, certificate.RawIssuer) {
			continue
		}
		applicable = true
		for _, entry := range list.RevokedCertificateEntries {
			if entry.SerialNumber != nil && entry.SerialNumber.Cmp(certificate.SerialNumber) == 0 {
				return fmt.Errorf("certificate serial %s is revoked", certificate.SerialNumber)
			}
		}
		for _, entry := range list.RevokedCertificates {
			if entry.SerialNumber != nil && entry.SerialNumber.Cmp(certificate.SerialNumber) == 0 {
				return fmt.Errorf("certificate serial %s is revoked", certificate.SerialNumber)
			}
		}
	}
	if len(lists) > 0 && !applicable {
		return fmt.Errorf("no configured REGISTER CRL applies to certificate issuer %s", certificate.Issuer.String())
	}
	return nil
}

func (authenticator *registerCertificateAuthenticator) hasDevice(deviceID string) bool {
	if authenticator == nil {
		return false
	}
	_, ok := authenticator.devices[strings.TrimSpace(deviceID)]
	return ok
}

func parseRegisterAuthorizationHeader(header sip.Header) (string, *sip.Authorization, error) {
	generic, ok := header.(*sip.GenericHeader)
	if !ok || generic == nil {
		return "", nil, fmt.Errorf("invalid Authorization header")
	}
	value := strings.TrimSpace(generic.Contents)
	if value == "" {
		return "", nil, fmt.Errorf("empty Authorization header")
	}
	space := strings.IndexAny(value, " \t")
	if space <= 0 {
		return strings.ToLower(value), nil, fmt.Errorf("Authorization scheme parameters are missing")
	}
	scheme := strings.ToLower(strings.TrimSpace(value[:space]))
	parameters := strings.TrimSpace(value[space+1:])
	if parameters == "" {
		return scheme, nil, fmt.Errorf("Authorization scheme parameters are missing")
	}
	return scheme, sip.AuthFromValue(parameters), nil
}

func selectRegisterCertificateDigest(capability string) (string, error) {
	normalized := strings.ToUpper(strings.NewReplacer(" ", "", "\t", "", "_", "-").Replace(capability))
	if normalized == "" {
		return "", fmt.Errorf("Capability algorithm is missing")
	}
	rsaSupported := false
	if index := strings.Index(normalized, "A:"); index >= 0 {
		asymmetric := normalized[index+2:]
		if end := strings.IndexByte(asymmetric, ';'); end >= 0 {
			asymmetric = asymmetric[:end]
		}
		for token := range strings.SplitSeq(asymmetric, ",") {
			token = strings.TrimSpace(token)
			if token == "RSA" || token == "RSA/ECB/PKCS1" {
				rsaSupported = true
				break
			}
		}
	} else if normalized == "RSA" || strings.Contains(normalized, "RSA,") || strings.Contains(normalized, "RSA/ECB/PKCS1") {
		rsaSupported = true
	}
	if !rsaSupported {
		return "", fmt.Errorf("Capability does not offer RSA/ECB/PKCS1")
	}
	digestCapability := normalized
	if index := strings.Index(normalized, "H:"); index >= 0 {
		digestCapability = normalized[index+2:]
		if end := strings.IndexByte(digestCapability, ';'); end >= 0 {
			digestCapability = digestCapability[:end]
		}
	}
	offeredDigests := make(map[string]struct{})
	for token := range strings.FieldsFuncSeq(digestCapability, func(r rune) bool { return r == ',' || r == '/' }) {
		token = strings.ReplaceAll(strings.TrimSpace(token), "-", "")
		offeredDigests[token] = struct{}{}
	}
	for _, candidate := range []struct {
		wire string
	}{
		{wire: "SHA256"},
		{wire: "SHA1"},
		{wire: "MD5"},
	} {
		if _, ok := offeredDigests[candidate.wire]; ok {
			return candidate.wire, nil
		}
	}
	return "", fmt.Errorf("Capability does not offer MD5, SHA1, or SHA256")
}

func canonicalRegisterCertificateDigest(value string) (string, func() hash.Hash, error) {
	normalized := strings.ToUpper(strings.NewReplacer(" ", "", "_", "", "-", "").Replace(value))
	if ampersand := strings.LastIndexByte(normalized, '&'); ampersand >= 0 {
		normalized = normalized[ampersand+1:]
	}
	switch normalized {
	case "MD5":
		return "MD5", md5.New, nil
	case "SHA1":
		return "SHA1", sha1.New, nil
	case "SHA256":
		return "SHA256", sha256.New, nil
	default:
		return "", nil, fmt.Errorf("unsupported Asymmetric digest algorithm %q", value)
	}
}

func registerCertificateDigest(algorithm string, values ...[]byte) ([]byte, error) {
	_, factory, err := canonicalRegisterCertificateDigest(algorithm)
	if err != nil {
		return nil, err
	}
	digest := factory()
	for _, value := range values {
		_, _ = digest.Write(value)
	}
	return digest.Sum(nil), nil
}

func (authenticator *registerCertificateAuthenticator) issue(deviceID, sourceIP, capability string) (string, string, error) {
	algorithm, err := selectRegisterCertificateDigest(capability)
	if err != nil {
		return "", "", err
	}
	return authenticator.issueWithAlgorithm(deviceID, sourceIP, algorithm)
}

func (authenticator *registerCertificateAuthenticator) issueWithAlgorithm(deviceID, sourceIP, algorithm string) (string, string, error) {
	if authenticator == nil {
		return "", "", fmt.Errorf("certificate REGISTER authentication is unavailable")
	}
	deviceID = strings.TrimSpace(deviceID)
	device, ok := authenticator.devices[deviceID]
	if !ok {
		return "", "", fmt.Errorf("no trusted REGISTER certificate is configured for device %s", deviceID)
	}
	algorithm, _, err := canonicalRegisterCertificateDigest(algorithm)
	if err != nil {
		return "", "", err
	}
	now := authenticator.now()
	if now.Before(authenticator.platformCertificate.NotBefore) || now.After(authenticator.platformCertificate.NotAfter) {
		return "", "", fmt.Errorf("platform REGISTER certificate is outside its validity period")
	}
	if err := verifyPinnedOrChainedCertificate(device.certificate, nil, nil, now); err != nil {
		return "", "", err
	}
	if err := authenticator.checkCertificateRevocation(device.certificate, now); err != nil {
		return "", "", err
	}
	secret := make([]byte, registerCertificateSecretLen)
	if _, err := io.ReadFull(authenticator.random, secret); err != nil {
		return "", "", fmt.Errorf("generate certificate REGISTER secret: %w", err)
	}
	secretDigest, err := registerCertificateDigest(algorithm, secret)
	if err != nil {
		return "", "", err
	}
	serverProof, err := rsa.SignPKCS1v15(nil, authenticator.platformPrivateKey, crypto.Hash(0), secretDigest)
	if err != nil {
		return "", "", fmt.Errorf("sign certificate REGISTER challenge: %w", err)
	}
	deviceSecret, err := rsa.EncryptPKCS1v15(authenticator.random, device.publicKey, secret)
	if err != nil {
		return "", "", fmt.Errorf("encrypt certificate REGISTER challenge: %w", err)
	}
	nonce := base64.StdEncoding.EncodeToString(serverProof) + "&" + base64.StdEncoding.EncodeToString(deviceSecret)

	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	var oldestKey string
	var oldest time.Time
	for key, state := range authenticator.nonces {
		if !state.Expires.After(now) {
			delete(authenticator.nonces, key)
			continue
		}
		if oldestKey == "" || state.IssuedAt.Before(oldest) {
			oldestKey = key
			oldest = state.IssuedAt
		}
	}
	if len(authenticator.nonces) >= maxRegisterCertificateNonce && oldestKey != "" {
		delete(authenticator.nonces, oldestKey)
	}
	authenticator.nonces[nonce] = registerCertificateNonceState{
		DeviceID:  deviceID,
		SourceIP:  strings.TrimSpace(sourceIP),
		Secret:    append([]byte(nil), secret...),
		Algorithm: algorithm,
		IssuedAt:  now,
		Expires:   now.Add(registerCertificateNonceTTL),
	}
	return nonce, algorithm, nil
}

func (authenticator *registerCertificateAuthenticator) validate(deviceID, sourceIP string, authorization *sip.Authorization, fingerprint string) error {
	if authenticator == nil || authorization == nil {
		return fmt.Errorf("invalid Asymmetric Authorization header")
	}
	nonce := strings.TrimSpace(authorization.Get("nonce"))
	response := strings.ToLower(strings.TrimSpace(authorization.Get("response")))
	algorithmValue := strings.TrimSpace(authorization.Get("algorithm"))
	if nonce == "" || response == "" || algorithmValue == "" {
		return fmt.Errorf("Asymmetric Authorization requires nonce, response, and algorithm")
	}
	algorithm, _, err := canonicalRegisterCertificateDigest(algorithmValue)
	if err != nil {
		return err
	}
	provided, err := hex.DecodeString(response)
	if err != nil {
		return fmt.Errorf("Asymmetric response is not hexadecimal")
	}
	now := authenticator.now()
	deviceID = strings.TrimSpace(deviceID)
	device, ok := authenticator.devices[deviceID]
	if !ok {
		return fmt.Errorf("no trusted REGISTER certificate is configured for device %s", deviceID)
	}
	if err := verifyPinnedOrChainedCertificate(device.certificate, nil, nil, now); err != nil {
		return err
	}
	if err := authenticator.checkCertificateRevocation(device.certificate, now); err != nil {
		return err
	}

	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	state, ok := authenticator.nonces[nonce]
	if !ok {
		return fmt.Errorf("Asymmetric nonce was not issued by this server")
	}
	if !state.Expires.After(now) {
		delete(authenticator.nonces, nonce)
		return fmt.Errorf("Asymmetric nonce expired")
	}
	if state.DeviceID != deviceID {
		return fmt.Errorf("Asymmetric nonce device mismatch")
	}
	if state.SourceIP != "" && state.SourceIP != strings.TrimSpace(sourceIP) {
		return fmt.Errorf("Asymmetric nonce source mismatch")
	}
	if state.Algorithm != algorithm {
		return fmt.Errorf("Asymmetric algorithm mismatch")
	}
	expected, err := registerCertificateDigest(algorithm, state.Secret, []byte(nonce))
	if err != nil {
		return err
	}
	if len(provided) != len(expected) || subtle.ConstantTimeCompare(provided, expected) != 1 {
		return fmt.Errorf("Asymmetric response mismatch")
	}
	if state.AcceptedFingerprint != "" && state.AcceptedFingerprint != fingerprint {
		return fmt.Errorf("Asymmetric nonce replay detected")
	}
	state.AcceptedFingerprint = fingerprint
	authenticator.nonces[nonce] = state
	return nil
}

func (g *GB28181API) respondRegisterCertificateChallenge(ctx *sip.Context, capability string) error {
	if g == nil || g.registerCertificateAuth == nil {
		return fmt.Errorf("certificate REGISTER authentication is unavailable")
	}
	nonce, algorithm, err := g.registerCertificateAuth.issue(ctx.DeviceID, registerNonceSourceIP(ctx), capability)
	if err != nil {
		return err
	}
	resp := g.newRegisterResponse(ctx, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
	resp.AppendHeader(&sip.GenericHeader{
		HeaderName: "WWW-Authenticate",
		Contents:   fmt.Sprintf(`Asymmetric nonce="%s",algorithm="A:RSA/ECB/PKCS1&%s"`, nonce, algorithm),
	})
	return ctx.Tx.Respond(resp)
}

// authenticateRegisterCertificate returns authenticated=true after a valid Asymmetric response.
// stop=true means a response has already been sent and the REGISTER handler must stop.
func (g *GB28181API) authenticateRegisterCertificate(ctx *sip.Context, headers []sip.Header) (authenticated, stop bool) {
	authenticator := g.registerCertificateAuth
	if authenticator == nil {
		return false, false
	}
	if !authenticator.hasDevice(ctx.DeviceID) && authenticator.required {
		g.respondRegister(ctx, http.StatusForbidden, "trusted device certificate is required")
		return false, true
	}
	if len(headers) == 0 {
		if authenticator.required {
			g.respondRegister(ctx, http.StatusForbidden, "certificate REGISTER authentication is required")
			return false, true
		}
		return false, false
	}
	if len(headers) != 1 {
		g.respondRegister(ctx, http.StatusBadRequest, "REGISTER requires exactly one Authorization header")
		return false, true
	}
	scheme, authorization, err := parseRegisterAuthorizationHeader(headers[0])
	if err != nil {
		if scheme == "capability" || scheme == "asymmetric" || authenticator.required {
			g.respondRegister(ctx, http.StatusBadRequest, err.Error())
			return false, true
		}
		return false, false
	}
	switch scheme {
	case "capability":
		if !authenticator.hasDevice(ctx.DeviceID) {
			g.respondRegister(ctx, http.StatusForbidden, "no trusted certificate is configured for this device")
			return false, true
		}
		capability := authorization.Get("algorithm")
		if capability == "" {
			g.respondRegister(ctx, http.StatusBadRequest, "Capability algorithm is missing")
			return false, true
		}
		if err := g.respondRegisterCertificateChallenge(ctx, capability); err != nil {
			ctx.Log.Info("设备数字证书注册能力协商失败", "err", err)
			g.respondRegister(ctx, http.StatusForbidden, "certificate REGISTER capability is not supported")
		}
		return false, true
	case "asymmetric":
		if !authenticator.hasDevice(ctx.DeviceID) {
			g.respondRegister(ctx, http.StatusForbidden, "no trusted certificate is configured for this device")
			return false, true
		}
		fingerprint := registerRequestFingerprint(ctx.Request, strings.ToLower(strings.TrimSpace(authorization.Get("response"))))
		if err := authenticator.validate(ctx.DeviceID, registerNonceSourceIP(ctx), authorization, fingerprint); err != nil {
			ctx.Log.Info("设备数字证书注册鉴权失败", "err", err)
			g.respondRegister(ctx, http.StatusForbidden, "certificate REGISTER authentication failed")
			return false, true
		}
		return true, false
	default:
		if authenticator.required {
			g.respondRegister(ctx, http.StatusForbidden, "certificate REGISTER authentication is required")
			return false, true
		}
		return false, false
	}
}
