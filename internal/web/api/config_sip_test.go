package api

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
)

func TestMergeSIPUpdatePreservesOmittedNestedConfiguration(t *testing.T) {
	current := conf.SIP{
		Host: "192.0.2.10", Port: 5060,
		TLSClientCA: "current-client-ca.crt", TLSRequireClientCert: true,
		DeviceHistory: conf.DeviceHistoryConfig{MaxRecords: 1000, MaxDays: 30},
		SignalDigest: conf.SIPSignalDigest{
			Enabled: true, Required: true, Seed: "current-seed", Algorithm: "SHA-256",
			Encoding: "base64", AcceptLegacyHex: true, Window: conf.Duration(5 * time.Minute),
		},
		RegisterCertificateAuth: conf.SIPRegisterCertificateAuth{
			Enabled: true, PlatformCert: "platform.crt", PlatformKey: "platform.key",
			DeviceCertificates: map[string]string{"34020000001320000001": "device.crt"},
		},
		Upstreams: []conf.SIPUpstream{{Name: "upstream-a", ServerID: "34020000002000000001"}},
		Log: conf.SIPLog{
			Enabled: true, Dir: "./logs/sip", MaxAge: conf.Duration(7 * 24 * time.Hour), RotationSize: 50,
		},
	}
	var input updateSIPInput
	if err := json.Unmarshal([]byte(`{"host":"192.0.2.20","port":5061}`), &input); err != nil {
		t.Fatal(err)
	}
	next := mergeSIPUpdate(current, &input)
	if next.Host != "192.0.2.20" || next.Port != 5061 {
		t.Fatalf("updated SIP fields = %+v", next)
	}
	if next.Log != current.Log {
		t.Fatalf("omitted SIP log was cleared: got %+v want %+v", next.Log, current.Log)
	}
	if next.DeviceHistory != current.DeviceHistory || len(next.Upstreams) != 1 || next.Upstreams[0].Name != "upstream-a" {
		t.Fatalf("omitted nested SIP config was cleared: %+v", next)
	}
	if next.SignalDigest != current.SignalDigest {
		t.Fatalf("omitted signal Digest was cleared: got %+v want %+v", next.SignalDigest, current.SignalDigest)
	}
	if !next.RegisterCertificateAuth.Enabled || next.RegisterCertificateAuth.PlatformCert != "platform.crt" ||
		next.RegisterCertificateAuth.DeviceCertificates["34020000001320000001"] != "device.crt" {
		t.Fatalf("omitted certificate REGISTER config was cleared: %+v", next.RegisterCertificateAuth)
	}
	if next.TLSClientCA != current.TLSClientCA || next.TLSRequireClientCert != current.TLSRequireClientCert {
		t.Fatalf("omitted TLS client verification config was cleared: %+v", next)
	}
}

func TestMergeSIPUpdateAppliesExplicitTLSClientVerification(t *testing.T) {
	current := conf.SIP{TLSClientCA: "old-ca.crt", TLSRequireClientCert: true}
	var input updateSIPInput
	if err := json.Unmarshal([]byte(`{"tls_client_ca":"new-ca.crt","tls_require_client_cert":false}`), &input); err != nil {
		t.Fatal(err)
	}
	next := mergeSIPUpdate(current, &input)
	if next.TLSClientCA != "new-ca.crt" || next.TLSRequireClientCert {
		t.Fatalf("explicit TLS client verification config = %+v", next)
	}
}

func TestMergeSIPUpdateAppliesExplicitLogConfiguration(t *testing.T) {
	current := conf.SIP{Log: conf.SIPLog{Enabled: true, Dir: "old"}}
	logConfig := conf.SIPLog{Enabled: false, Dir: "new"}
	next := mergeSIPUpdate(current, &updateSIPInput{SIP: current, Log: &logConfig})
	if next.Log != logConfig {
		t.Fatalf("explicit SIP log = %+v, want %+v", next.Log, logConfig)
	}
}

func TestMergeSIPUpdateAppliesExplicitSignalDigestConfiguration(t *testing.T) {
	current := conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Algorithm: "MD5", Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}
	replacement := conf.SIPSignalDigest{
		Required: true, Seed: "replacement", Algorithm: "SHA-256", Encoding: "hex", Window: conf.Duration(time.Minute),
	}
	next := mergeSIPUpdate(current, &updateSIPInput{SIP: current, SignalDigest: &replacement})
	if next.SignalDigest != replacement {
		t.Fatalf("explicit signal Digest = %+v, want %+v", next.SignalDigest, replacement)
	}
}

func TestUpdateSIPInputDecodesSnakeCaseLogConfiguration(t *testing.T) {
	var input updateSIPInput
	body := []byte(`{"log":{"enabled":true,"dir":"./logs/sip","max_age":604800000000000,"rotation_time":43200000000000,"rotation_size":50}}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Log == nil || !input.Log.Enabled || input.Log.Dir != "./logs/sip" || input.Log.MaxAge != conf.Duration(7*24*time.Hour) || input.Log.RotationTime != conf.Duration(12*time.Hour) || input.Log.RotationSize != 50 {
		t.Fatalf("decoded SIP log = %+v", input.Log)
	}
}

func TestUpdateSIPInputDecodesSignalDigestConfiguration(t *testing.T) {
	var input updateSIPInput
	body := []byte(`{"signal_digest":{"enabled":true,"required":true,"seed":"shared","algorithm":"SHA-256","encoding":"hex","accept_legacy_hex":false,"window":"5m"}}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.SignalDigest == nil || !input.SignalDigest.Enabled || !input.SignalDigest.Required ||
		input.SignalDigest.Seed != "shared" || input.SignalDigest.Algorithm != "SHA-256" ||
		input.SignalDigest.Encoding != "hex" || input.SignalDigest.AcceptLegacyHex ||
		input.SignalDigest.Window != conf.Duration(5*time.Minute) {
		t.Fatalf("decoded signal Digest = %+v", input.SignalDigest)
	}
}

func TestUpdateSIPInputAcceptsReadableDurationStrings(t *testing.T) {
	var input updateSIPInput
	body := []byte(`{"upstreams":[{"name":"upstream","enabled":true,"server_id":"34020000002000000001","host":"192.0.2.30","transport":"tcp","keepalive_interval":"60s"}],"direct_tcp_download":{"dial_timeout":"5s"}}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Upstreams == nil || len(*input.Upstreams) != 1 || (*input.Upstreams)[0].Transport != "tcp" || (*input.Upstreams)[0].KeepaliveInterval != conf.Duration(time.Minute) || input.DirectTCPDownload.DialTimeout != conf.Duration(5*time.Second) {
		t.Fatalf("decoded readable durations = upstreams %+v direct %+v", input.Upstreams, input.DirectTCPDownload)
	}
}

func TestUpdateSIPDoesNotMutateMemoryWhenWriteFails(t *testing.T) {
	config := conf.DefaultConfig()
	config.ConfigPath = filepath.Join(t.TempDir(), "missing", "config.toml")
	current := config.Sip
	next := current
	next.Password = "new-password"
	runtimeConfig := current
	api := ConfigAPI{conf: &config, sipMu: new(sync.RWMutex), sipConfig: &runtimeConfig}

	if _, err := api.updateSIP(nil, &updateSIPInput{SIP: next}); err == nil {
		t.Fatal("configuration write failure was accepted")
	}
	if runtimeConfig.Password != current.Password {
		t.Fatalf("runtime config changed after write failure: got %q want %q", runtimeConfig.Password, current.Password)
	}
	if config.Sip.Password != current.Password {
		t.Fatalf("shared config changed after write failure: got %q want %q", config.Sip.Password, current.Password)
	}
}

func TestUpdateSIPPersistsBeforeCommittingMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	config := conf.DefaultConfig()
	config.ConfigPath = path
	if err := conf.WriteConfig(&config, path); err != nil {
		t.Fatal(err)
	}
	next := config.Sip
	next.Password = "new-password"
	runtimeConfig := config.Sip
	api := ConfigAPI{conf: &config, sipMu: new(sync.RWMutex), sipConfig: &runtimeConfig}

	if _, err := api.updateSIP(nil, &updateSIPInput{SIP: next}); err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.Password != next.Password {
		t.Fatalf("runtime password = %q, want %q", runtimeConfig.Password, next.Password)
	}
	if config.Sip.Password != next.Password {
		t.Fatalf("shared password = %q, want %q", config.Sip.Password, next.Password)
	}
	var persisted conf.Bootstrap
	if err := conf.SetupConfig(&persisted, path); err != nil {
		t.Fatal(err)
	}
	if persisted.Sip.Password != next.Password {
		t.Fatalf("persisted password = %q, want %q", persisted.Sip.Password, next.Password)
	}
}

func TestUpdateSIPRejectsRestartRequiredFields(t *testing.T) {
	config := conf.DefaultConfig()
	config.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	if err := conf.WriteConfig(&config, config.ConfigPath); err != nil {
		t.Fatal(err)
	}
	next := config.Sip
	next.Port++
	runtimeConfig := config.Sip
	api := ConfigAPI{conf: &config, sipMu: new(sync.RWMutex), sipConfig: &runtimeConfig}

	_, err := api.updateSIP(nil, &updateSIPInput{SIP: next})
	if err == nil || !strings.Contains(err.Error(), "需要重启") || !strings.Contains(err.Error(), "Port") {
		t.Fatalf("restart-required update error = %v", err)
	}
	if runtimeConfig.Port != conf.DefaultConfig().Sip.Port {
		t.Fatalf("restart-required update changed runtime port to %d", runtimeConfig.Port)
	}
}

func TestSIPRestartRequiredFields(t *testing.T) {
	current := conf.DefaultConfig().Sip
	tests := []struct {
		name   string
		change func(*conf.SIP)
	}{
		{name: "host", change: func(config *conf.SIP) { config.Host = "192.0.2.20" }},
		{name: "id", change: func(config *conf.SIP) { config.ID = "34020000002000000002" }},
		{name: "domain", change: func(config *conf.SIP) { config.Domain = "3402000000" }},
		{name: "port", change: func(config *conf.SIP) { config.Port++ }},
		{name: "enable TLS", change: func(config *conf.SIP) { config.EnableTLS = true }},
		{name: "TLS port", change: func(config *conf.SIP) { config.TLSPort++ }},
		{name: "TLS certificate", change: func(config *conf.SIP) { config.TLSCert = "server.crt" }},
		{name: "TLS key", change: func(config *conf.SIP) { config.TLSKey = "server.key" }},
		{name: "TLS client CA", change: func(config *conf.SIP) { config.TLSClientCA = "client-ca.crt" }},
		{name: "TLS require client certificate", change: func(config *conf.SIP) { config.TLSRequireClientCert = true }},
		{name: "REGISTER certificate authentication", change: func(config *conf.SIP) {
			config.RegisterCertificateAuth.Enabled = true
			config.RegisterCertificateAuth.PlatformCert = "platform.crt"
		}},
		{name: "log", change: func(config *conf.SIP) { config.Log.Enabled = !config.Log.Enabled }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			next := current
			test.change(&next)
			if fields := sipRestartRequiredFields(current, next); len(fields) == 0 {
				t.Fatal("restart-required change was not detected")
			}
		})
	}

	hot := current
	hot.Password = "updated"
	hot.StrictSourceCheck = !hot.StrictSourceCheck
	hot.Upstreams = []conf.SIPUpstream{{Name: "upstream"}}
	if fields := sipRestartRequiredFields(current, hot); len(fields) != 0 {
		t.Fatalf("hot-reloadable changes require restart: %v", fields)
	}
}
