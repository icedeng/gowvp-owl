package gbs

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

const cascadeRegisterCertificateCapability = "A:RSA/ECB/PKCS1;H:SHA256,SHA1,MD5;S:DES/ECB/PKCS5,3DES/ECB/PKCS5"

type cascadeRegisterCertificateAuthenticator struct {
	required               bool
	localCertificate       *x509.Certificate
	localPrivateKey        *rsa.PrivateKey
	serverCertificate      *x509.Certificate
	serverPublicKey        *rsa.PublicKey
	certificateAuthorities []*x509.Certificate
	revocationLists        []*x509.RevocationList
	now                    func() time.Time
}

func newCascadeRegisterCertificateAuthenticator(config conf.SIPUpstreamRegisterCertificateAuth) (*cascadeRegisterCertificateAuthenticator, error) {
	if !config.Active() {
		return nil, nil
	}
	if err := conf.ValidateUpstreamRegisterCertificateAuthConfig(config); err != nil {
		return nil, err
	}
	localCertificates, err := loadX509Certificates(config.LocalCert)
	if err != nil {
		return nil, fmt.Errorf("load cascade local REGISTER certificate: %w", err)
	}
	localPrivateKey, err := loadRSAPrivateKey(config.LocalKey)
	if err != nil {
		return nil, fmt.Errorf("load cascade local REGISTER private key: %w", err)
	}
	localCertificate := localCertificates[0]
	localPublicKey, ok := localCertificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cascade local REGISTER certificate public key is not RSA")
	}
	if err := validateRSAKeySize(localPublicKey); err != nil {
		return nil, fmt.Errorf("cascade local REGISTER certificate: %w", err)
	}
	if localPublicKey.E != localPrivateKey.PublicKey.E || localPublicKey.N.Cmp(localPrivateKey.PublicKey.N) != 0 {
		return nil, fmt.Errorf("cascade local REGISTER certificate and private key do not match")
	}

	serverCertificates, err := loadX509Certificates(config.ServerCert)
	if err != nil {
		return nil, fmt.Errorf("load cascade server REGISTER certificate: %w", err)
	}
	serverCertificate := serverCertificates[0]
	serverPublicKey, ok := serverCertificate.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("cascade server REGISTER certificate public key is not RSA")
	}
	if err := validateRSAKeySize(serverPublicKey); err != nil {
		return nil, fmt.Errorf("cascade server REGISTER certificate: %w", err)
	}

	authenticator := &cascadeRegisterCertificateAuthenticator{
		required:          config.Required,
		localCertificate:  localCertificate,
		localPrivateKey:   localPrivateKey,
		serverCertificate: serverCertificate,
		serverPublicKey:   serverPublicKey,
		now:               time.Now,
	}
	var roots *x509.CertPool
	if strings.TrimSpace(config.ServerCA) != "" {
		authorities, err := loadX509Certificates(config.ServerCA)
		if err != nil {
			return nil, fmt.Errorf("load cascade server REGISTER CA: %w", err)
		}
		roots = x509.NewCertPool()
		for _, authority := range authorities {
			roots.AddCert(authority)
		}
		authenticator.certificateAuthorities = authorities
	}
	now := authenticator.now()
	if err := verifyPinnedOrChainedCertificate(localCertificate, localCertificates[1:], nil, now); err != nil {
		return nil, fmt.Errorf("verify cascade local REGISTER certificate: %w", err)
	}
	if err := verifyPinnedOrChainedCertificate(serverCertificate, serverCertificates[1:], roots, now); err != nil {
		return nil, fmt.Errorf("verify cascade server REGISTER certificate: %w", err)
	}
	if strings.TrimSpace(config.CRL) != "" {
		authenticator.revocationLists, err = loadX509RevocationLists(config.CRL)
		if err != nil {
			return nil, fmt.Errorf("load cascade server REGISTER CRL: %w", err)
		}
	}
	if err := checkX509CertificateRevocation(serverCertificate, authenticator.revocationLists, authenticator.certificateAuthorities, now); err != nil {
		return nil, fmt.Errorf("verify cascade server REGISTER certificate: %w", err)
	}
	return authenticator, nil
}

func (authenticator *cascadeRegisterCertificateAuthenticator) capabilityAuthorization() string {
	return fmt.Sprintf(`Capability algorithm="%s"`, cascadeRegisterCertificateCapability)
}

func (authenticator *cascadeRegisterCertificateAuthenticator) asymmetricAuthorization(challenge *sip.Authorization) (string, error) {
	if authenticator == nil || challenge == nil {
		return "", fmt.Errorf("cascade REGISTER Asymmetric challenge is unavailable")
	}
	nonce := strings.TrimSpace(challenge.Get("nonce"))
	algorithmValue := strings.TrimSpace(challenge.Get("algorithm"))
	if nonce == "" || algorithmValue == "" {
		return "", fmt.Errorf("cascade REGISTER Asymmetric challenge requires nonce and algorithm")
	}
	algorithm, _, err := canonicalRegisterCertificateDigest(algorithmValue)
	if err != nil {
		return "", err
	}
	now := authenticator.now()
	if err := verifyPinnedOrChainedCertificate(authenticator.localCertificate, nil, nil, now); err != nil {
		return "", fmt.Errorf("cascade local REGISTER certificate: %w", err)
	}
	if err := verifyPinnedOrChainedCertificate(authenticator.serverCertificate, nil, nil, now); err != nil {
		return "", fmt.Errorf("cascade server REGISTER certificate: %w", err)
	}
	if err := checkX509CertificateRevocation(authenticator.serverCertificate, authenticator.revocationLists, authenticator.certificateAuthorities, now); err != nil {
		return "", fmt.Errorf("cascade server REGISTER certificate: %w", err)
	}
	parts := strings.Split(nonce, "&")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("cascade REGISTER Asymmetric nonce must contain two Base64 parts")
	}
	serverProof, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("decode cascade REGISTER server proof: %w", err)
	}
	deviceSecret, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode cascade REGISTER encrypted secret: %w", err)
	}
	secret, err := rsa.DecryptPKCS1v15(rand.Reader, authenticator.localPrivateKey, deviceSecret)
	if err != nil {
		return "", fmt.Errorf("decrypt cascade REGISTER secret: %w", err)
	}
	secretDigest, err := registerCertificateDigest(algorithm, secret)
	if err != nil {
		return "", err
	}
	if err := rsa.VerifyPKCS1v15(authenticator.serverPublicKey, crypto.Hash(0), secretDigest, serverProof); err != nil {
		return "", fmt.Errorf("verify cascade REGISTER server identity: %w", err)
	}
	response, err := registerCertificateDigest(algorithm, secret, []byte(nonce))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(`Asymmetric nonce="%s",response="%s",algorithm=%s`, nonce, hex.EncodeToString(response), algorithm), nil
}

func cascadeRegisterChallenge(response *sip.Response, preferCertificate bool) (string, *sip.Authorization, error) {
	if response == nil {
		return "", nil, fmt.Errorf("cascade REGISTER response is missing")
	}
	headers := response.GetHeaders("WWW-Authenticate")
	if len(headers) == 0 {
		return "", nil, fmt.Errorf("cascade REGISTER 401 missing WWW-Authenticate")
	}
	var asymmetric, digest *sip.Authorization
	var unsupportedScheme string
	var firstError error
	for _, header := range headers {
		scheme, authorization, err := parseRegisterAuthorizationHeader(header)
		if err != nil {
			if firstError == nil {
				firstError = fmt.Errorf("cascade REGISTER invalid %s challenge: %w", scheme, err)
			}
			continue
		}
		switch scheme {
		case "asymmetric":
			if asymmetric == nil {
				asymmetric = authorization
			}
		case "digest":
			if digest == nil {
				digest = authorization
			}
		default:
			if unsupportedScheme == "" {
				unsupportedScheme = scheme
			}
		}
	}
	if preferCertificate && asymmetric != nil {
		return "asymmetric", asymmetric, nil
	}
	if digest != nil {
		return "digest", digest, nil
	}
	if asymmetric != nil {
		return "asymmetric", asymmetric, nil
	}
	if firstError != nil {
		return "", nil, firstError
	}
	if unsupportedScheme != "" {
		return unsupportedScheme, nil, nil
	}
	return "", nil, fmt.Errorf("cascade REGISTER 401 has no supported authentication challenge")
}
