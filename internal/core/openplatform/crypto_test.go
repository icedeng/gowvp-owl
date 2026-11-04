package openplatform

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSharedSigningVector(t *testing.T) {
	var v struct {
		Audience, Method, Path, Timestamp, Nonce, Body, Signature string
		RawQuery                                                  string `json:"raw_query"`
		CanonicalQuery                                            string `json:"canonical_query"`
		ContentType                                               string `json:"content_type"`
		IdempotencyKey                                            string `json:"idempotency_key"`
		SecretKey                                                 string `json:"secret_key"`
		CanonicalRequest                                          string `json:"canonical_request"`
		BodySHA256                                                string `json:"body_sha256"`
	}
	b, e := os.ReadFile("../../../docs/open-platform/signing-vectors.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	q, _, e := CanonicalQuery(v.RawQuery)
	if e != nil || q != v.CanonicalQuery {
		t.Fatal(q, e)
	}
	if Hash([]byte(v.Body)) != v.BodySHA256 {
		t.Fatal("body hash mismatch")
	}
	text := Canonical(v.Audience, v.Method, v.Path, q, v.ContentType, v.Timestamp, v.Nonce, v.IdempotencyKey, v.BodySHA256)
	if text != v.CanonicalRequest {
		t.Fatal("canonical mismatch")
	}
	key, e := ValidateSecret(v.SecretKey)
	if e != nil || Sign(key, text) != v.Signature {
		t.Fatal("signature mismatch", e)
	}
	for _, raw := range []string{"a=1&%61=2", "x=%FF", "x=%", "=value"} {
		if _, _, e := CanonicalQuery(raw); e == nil {
			t.Fatal("invalid query accepted", raw)
		}
	}
	for _, path := range []string{"/openapi/v1/../devices", "/openapi//devices", "/openapi/v1/%2Fdevices", "/openapi/v1/\\devices"} {
		if ValidPath(path) {
			t.Fatal(path)
		}
	}
}
