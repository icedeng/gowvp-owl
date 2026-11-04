package openplatform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"unicode/utf8"
)

func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func aad(app, ak, key string) []byte {
	b := []byte("owl-secret-v1")
	for _, s := range []string{app, ak, key} {
		b = binary.BigEndian.AppendUint32(b, uint32(len(s)))
		b = append(b, []byte(s)...)
	}
	return b
}
func (s *Service) Encrypt(app, ak string, raw []byte) (Encrypted, error) {
	e := Encrypted{KeyID: s.Config.KeyID}
	block, err := aes.NewCipher(s.Config.Keys[e.KeyID])
	if err != nil {
		return e, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return e, err
	}
	n := make([]byte, g.NonceSize())
	if _, err = rand.Read(n); err != nil {
		return e, err
	}
	e.Nonce = base64.RawURLEncoding.EncodeToString(n)
	e.Ciphertext = base64.RawURLEncoding.EncodeToString(g.Seal(nil, n, raw, aad(app, ak, e.KeyID)))
	return e, nil
}
func (s *Service) Decrypt(app, ak string, e Encrypted) ([]byte, error) {
	block, err := aes.NewCipher(s.Config.Keys[e.KeyID])
	if err != nil {
		return nil, ErrUnavailable
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrUnavailable
	}
	n, err := base64.RawURLEncoding.DecodeString(e.Nonce)
	if err != nil || len(n) != g.NonceSize() {
		return nil, ErrUnavailable
	}
	b, err := base64.RawURLEncoding.DecodeString(e.Ciphertext)
	if err != nil {
		return nil, ErrUnavailable
	}
	return g.Open(nil, n, b, aad(app, ak, e.KeyID))
}
func Encode(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// CanonicalQuery 独立于表单解析，+ 保持加号语义，并拒绝重复参数。
func CanonicalQuery(raw string) (string, map[string]string, error) {
	values := map[string]string{}
	pairs := []string{}
	if raw == "" {
		return "", values, nil
	}
	for _, pair := range strings.Split(raw, "&") {
		a, b, _ := strings.Cut(pair, "=")
		k, e := url.PathUnescape(a)
		if e != nil || k == "" || !utf8.ValidString(k) {
			return "", nil, ErrAuth
		}
		v, e := url.PathUnescape(b)
		if e != nil || !utf8.ValidString(v) {
			return "", nil, ErrAuth
		}
		if _, ok := values[k]; ok {
			return "", nil, ErrAuth
		}
		values[k] = v
		pairs = append(pairs, Encode(k)+"="+Encode(v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&"), values, nil
}
func ValidPath(path string) bool {
	if !strings.HasPrefix(path, "/openapi/v1/") || strings.Contains(path, "//") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.~", c)) {
				return false
			}
		}
	}
	return true
}
func Canonical(audience, method, path, query, contentType, timestamp, nonce, idempotency, digest string) string {
	return strings.Join([]string{"OWL-HMAC-SHA256", audience, method, path, query, contentType, timestamp, nonce, idempotency, digest}, "\n")
}
func Sign(key []byte, canonical string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(canonical))
	return hex.EncodeToString(m.Sum(nil))
}
func Verify(key []byte, canonical, signature string) bool {
	b, err := hex.DecodeString(signature)
	if err != nil || signature != strings.ToLower(signature) || len(b) != 32 {
		return false
	}
	expected, _ := hex.DecodeString(Sign(key, canonical))
	return hmac.Equal(expected, b)
}
func ParseAuthorization(raw string) (string, string, error) {
	rest, ok := strings.CutPrefix(raw, "OWL-HMAC-SHA256 ")
	if !ok {
		return "", "", ErrAuth
	}
	parts := strings.Split(rest, ",")
	if len(parts) != 2 {
		return "", "", ErrAuth
	}
	ak, ok := strings.CutPrefix(strings.TrimSpace(parts[0]), "Credential=")
	if !ok || !strings.HasPrefix(ak, "owl_ak_") || len(ak) > 80 {
		return "", "", ErrAuth
	}
	signature, ok := strings.CutPrefix(strings.TrimSpace(parts[1]), "Signature=")
	if !ok || len(signature) != 64 {
		return "", "", ErrAuth
	}
	return ak, signature, nil
}
func ValidateSecret(raw string) ([]byte, error) {
	b, e := base64.RawURLEncoding.DecodeString(raw)
	if e != nil || len(b) != 32 {
		return nil, fmt.Errorf("密钥必须为32字节Base64URL")
	}
	return b, nil
}
