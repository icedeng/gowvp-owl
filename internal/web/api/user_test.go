package api

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestLoginKeyOAEPSeed(t *testing.T) {
	api := NewUserAPI(nil)
	out, err := api.getPublicKey(nil, nil)
	if err != nil {
		t.Fatalf("getPublicKey() error = %v", err)
	}

	seed, err := base64.StdEncoding.DecodeString(out["oaep_seed"].(string))
	if err != nil {
		t.Fatalf("oaep_seed is not base64: %v", err)
	}
	if len(seed) != sha256.Size {
		t.Fatalf("oaep_seed length = %d, want %d", len(seed), sha256.Size)
	}
	if out["key"] == "" {
		t.Fatal("public key is empty")
	}
}
