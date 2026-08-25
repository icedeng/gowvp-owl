package conf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateSignalDigestConfig(t *testing.T) {
	valid := SIPSignalDigest{Algorithm: "MD5", Encoding: "base64", Window: Duration(10 * time.Minute)}
	for _, algorithm := range []string{"MD5", "sha-1", "SHA1", "sha_256"} {
		config := valid
		config.Algorithm = algorithm
		if err := ValidateSignalDigestConfig(config); err != nil {
			t.Fatalf("algorithm %q rejected: %v", algorithm, err)
		}
	}
	for _, encoding := range []string{"base64", "HEX"} {
		config := valid
		config.Encoding = encoding
		if err := ValidateSignalDigestConfig(config); err != nil {
			t.Fatalf("encoding %q rejected: %v", encoding, err)
		}
	}

	tests := []struct {
		name   string
		change func(*SIPSignalDigest)
	}{
		{name: "unsupported algorithm", change: func(config *SIPSignalDigest) { config.Algorithm = "SM3" }},
		{name: "unsupported encoding", change: func(config *SIPSignalDigest) { config.Encoding = "raw" }},
		{name: "window too small", change: func(config *SIPSignalDigest) { config.Window = Duration(time.Millisecond) }},
		{name: "window too large", change: func(config *SIPSignalDigest) { config.Window = Duration(25 * time.Hour) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.change(&config)
			if err := ValidateSignalDigestConfig(config); err == nil {
				t.Fatalf("invalid config was accepted: %+v", config)
			}
		})
	}
}

func TestSetupConfigDefaultsMissingSignalDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[Sip]\nPort = 5060\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var config Bootstrap
	if err := SetupConfig(&config, path); err != nil {
		t.Fatal(err)
	}
	if config.Sip.SignalDigest != DefaultConfig().Sip.SignalDigest {
		t.Fatalf("missing signal Digest defaults = %+v", config.Sip.SignalDigest)
	}
}

func TestSetupConfigRejectsInvalidSignalDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := strings.Join([]string{
		"[Sip.SignalDigest]",
		"Algorithm = 'SM3'",
		"Encoding = 'base64'",
		"Window = '10m'",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var config Bootstrap
	if err := SetupConfig(&config, path); err == nil {
		t.Fatal("invalid signal Digest config was accepted")
	}
}
