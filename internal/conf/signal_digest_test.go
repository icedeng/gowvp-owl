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
		{name: "missing TLS client CA", change: func(config *SIP) {
			config.EnableTLS = true
			config.TLSCert = "server.crt"
			config.TLSKey = "server.key"
			config.TLSRequireClientCert = true
		}},
		{name: "history record limit", change: func(config *SIP) { config.DeviceHistory.MaxRecords = 100001 }},
		{name: "history day limit", change: func(config *SIP) { config.DeviceHistory.MaxDays = 3651 }},
		{name: "redirect scheme", change: func(config *SIP) { config.RegisterRedirect = "https://192.0.2.31" }},
		{name: "redirect empty address", change: func(config *SIP) { config.RegisterRedirect = "sip:" }},
		{name: "redirect password", change: func(config *SIP) { config.RegisterRedirect = "sip:" + config.ID + ":secret@192.0.2.31" }},
		{name: "redirect server ID mismatch", change: func(config *SIP) { config.RegisterRedirect = "sip:34020000002000000002@192.0.2.31" }},
		{name: "redirect empty host", change: func(config *SIP) { config.RegisterRedirect = "sip:" + config.ID + "@" }},
		{name: "redirect invalid port", change: func(config *SIP) { config.RegisterRedirect = "sip:" + config.ID + "@192.0.2.31:70000" }},
		{name: "redirect unsupported transport", change: func(config *SIP) { config.RegisterRedirect = "sip:" + config.ID + "@192.0.2.31;transport=ws" }},
		{name: "redirect ambiguous transport", change: func(config *SIP) {
			config.RegisterRedirect = "sip:" + config.ID + "@192.0.2.31;transport=tcp;transport=tls"
		}},
		{name: "redirect SIPS transport conflict", change: func(config *SIP) { config.RegisterRedirect = "sips:" + config.ID + "@192.0.2.31;transport=tcp" }},
		{name: "redirect control character", change: func(config *SIP) {
			config.RegisterRedirect = "sip:" + config.ID + "@192.0.2.31\r\nContact: <sip:attacker.example>"
		}},
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
	tlsOnMainPort.TLSRequireClientCert = true
	tlsOnMainPort.TLSClientCA = "client-ca.crt"
	if err := ValidateSIPConfig(tlsOnMainPort); err != nil {
		t.Fatalf("TLS client certificate config was rejected: %v", err)
	}

	for _, redirect := range []string{
		"",
		"sip:" + valid.ID + "@192.0.2.31:5070",
		"sip:redirect.example;transport=tcp",
		"sips:" + valid.ID + "@[2001:db8::31]:5061;transport=tls",
	} {
		config := valid
		config.RegisterRedirect = redirect
		if err := ValidateSIPConfig(config); err != nil {
			t.Fatalf("valid REGISTER redirect %q was rejected: %v", redirect, err)
		}
	}
}

func TestValidateSIPAlarmReceivers(t *testing.T) {
	valid := []SIPAlarmReceiver{{
		Name: "dispatch-center", Enabled: true, DeviceID: "34020000002000000011",
		SourceIDs: []string{"3402000000", "34020000001320000001"},
	}}
	if err := ValidateSIPAlarmReceivers(valid); err != nil {
		t.Fatalf("valid Alarm receiver was rejected: %v", err)
	}

	tests := []struct {
		name      string
		receivers []SIPAlarmReceiver
	}{
		{name: "missing name", receivers: []SIPAlarmReceiver{{Enabled: true, DeviceID: "34020000002000000011", SourceIDs: []string{"3402000000"}}}},
		{name: "duplicate name", receivers: []SIPAlarmReceiver{
			{Name: "receiver", Enabled: true, DeviceID: "34020000002000000011", SourceIDs: []string{"3402000000"}},
			{Name: "receiver", Enabled: true, DeviceID: "34020000002000000012", SourceIDs: []string{"3402000001"}},
		}},
		{name: "invalid device id", receivers: []SIPAlarmReceiver{{Name: "receiver", Enabled: true, DeviceID: "3402000000", SourceIDs: []string{"3402000000"}}}},
		{name: "duplicate device id", receivers: []SIPAlarmReceiver{
			{Name: "first", Enabled: true, DeviceID: "34020000002000000011", SourceIDs: []string{"3402000000"}},
			{Name: "second", Enabled: true, DeviceID: "34020000002000000011", SourceIDs: []string{"3402000001"}},
		}},
		{name: "missing sources", receivers: []SIPAlarmReceiver{{Name: "receiver", Enabled: true, DeviceID: "34020000002000000011"}}},
		{name: "invalid source", receivers: []SIPAlarmReceiver{{Name: "receiver", Enabled: true, DeviceID: "34020000002000000011", SourceIDs: []string{"source"}}}},
		{name: "duplicate source", receivers: []SIPAlarmReceiver{{
			Name: "receiver", Enabled: true, DeviceID: "34020000002000000011",
			SourceIDs: []string{"3402000000", "3402000000"},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateSIPAlarmReceivers(test.receivers); err == nil {
				t.Fatalf("invalid Alarm receivers were accepted: %+v", test.receivers)
			}
		})
	}

	// 关闭项不参与运行时分发，允许先保存未完成草稿，保持默认关闭兼容性。
	if err := ValidateSIPAlarmReceivers([]SIPAlarmReceiver{{Name: "draft", Enabled: false}}); err != nil {
		t.Fatalf("disabled Alarm receiver draft was rejected: %v", err)
	}
}
