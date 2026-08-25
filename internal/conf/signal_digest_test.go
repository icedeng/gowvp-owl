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
	for _, algorithm := range []string{"MD5", "sha-1", "SHA1", "sha_256", "sm3"} {
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
		{name: "unsupported algorithm", change: func(config *SIPSignalDigest) { config.Algorithm = "SM4" }},
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
	if err := os.WriteFile(path, []byte("[Sip]\nPort = 5060\nID = '34020000002000000001'\n"), 0o600); err != nil {
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
		"[Sip]",
		"Port = 5060",
		"ID = '34020000002000000001'",
		"",
		"[Sip.SignalDigest]",
		"Algorithm = 'SM4'",
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

func TestValidateSIPConfig(t *testing.T) {
	valid := DefaultConfig().Sip
	if err := ValidateSIPConfig(valid); err != nil {
		t.Fatalf("default SIP config was rejected: %v", err)
	}

	tests := []struct {
		name   string
		change func(*SIP)
	}{
		{name: "short platform ID", change: func(config *SIP) { config.ID = "3402000000" }},
		{name: "non numeric platform ID", change: func(config *SIP) { config.ID = "3402000000200000000x" }},
		{name: "short domain", change: func(config *SIP) { config.Domain = "340200" }},
		{name: "non numeric domain", change: func(config *SIP) { config.Domain = "340200000x" }},
		{name: "invalid port", change: func(config *SIP) { config.Port = 0 }},
		{name: "invalid TLS port", change: func(config *SIP) {
			config.TLSPort = 70000
			config.TLSCert = "server.crt"
			config.TLSKey = "server.key"
		}},
		{name: "missing TLS certificate", change: func(config *SIP) { config.EnableTLS = true; config.TLSCert = ""; config.TLSKey = "server.key" }},
		{name: "missing TLS key", change: func(config *SIP) { config.EnableTLS = true; config.TLSCert = "server.crt"; config.TLSKey = "" }},
		{name: "history record limit", change: func(config *SIP) { config.DeviceHistory.MaxRecords = 100001 }},
		{name: "history day limit", change: func(config *SIP) { config.DeviceHistory.MaxDays = 3651 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.change(&config)
			if err := ValidateSIPConfig(config); err == nil {
				t.Fatalf("invalid SIP config was accepted: %+v", config)
			}
		})
	}

	derived := valid
	derived.Domain = ""
	if err := ValidateSIPConfig(derived); err != nil {
		t.Fatalf("derived SIP domain was rejected: %v", err)
	}
	tlsOnMainPort := valid
	tlsOnMainPort.EnableTLS = true
	tlsOnMainPort.TLSPort = 0
	tlsOnMainPort.TLSCert = "server.crt"
	tlsOnMainPort.TLSKey = "server.key"
	if err := ValidateSIPConfig(tlsOnMainPort); err != nil {
		t.Fatalf("TLS main-port fallback was rejected: %v", err)
	}
}
