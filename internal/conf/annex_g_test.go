package conf

import (
	"strings"
	"testing"
	"time"
)

func validAnnexGConfig() SIPAnnexG {
	return SIPAnnexG{
		Enabled: true, MaxSendRecords: 100, InboundRate: 50, InboundBurst: 100,
		PendingTTL: Duration(24 * time.Hour), MaxPending: 4096,
		Systems: []SIPAnnexGSystem{{
			ID: "34020000002000000002", Role: "emergency_command_system", Version: "1.0",
			Password: "secret", Address: "192.0.2.10:5061", Transport: "tls",
			SourceCIDRs: []string{"192.0.2.10", "2001:db8::/64"},
		}},
	}
}

func TestDefaultSIPAnnexGPendingPolicy(t *testing.T) {
	config := DefaultConfig().Sip.AnnexG
	if config.PendingTTL.Duration() != 24*time.Hour {
		t.Fatalf("default pending TTL = %v, want 24h", config.PendingTTL.Duration())
	}
	if config.MaxPending != 4096 {
		t.Fatalf("default max pending = %d, want 4096", config.MaxPending)
	}
	if config.InboundRate != 50 || config.InboundBurst != 100 {
		t.Fatalf("default inbound limit = %d/%d, want 50/100", config.InboundRate, config.InboundBurst)
	}
}

func TestValidateSIPAnnexGConfig(t *testing.T) {
	if err := ValidateSIPAnnexGConfig(SIPAnnexG{}, "34020000002000000001", false); err != nil {
		t.Fatalf("disabled config = %v", err)
	}
	valid := validAnnexGConfig()
	if err := ValidateSIPAnnexGConfig(valid, "34020000002000000001", true); err != nil {
		t.Fatalf("valid config = %v", err)
	}
	valid.Systems[0].AllowInsecureTransport = true
	if err := ValidateSIPAnnexGConfig(valid, "34020000002000000001", false); err != nil {
		t.Fatalf("explicit insecure transport = %v", err)
	}
	for _, password := range []string{" ", " # "} {
		config := validAnnexGConfig()
		config.Systems[0].Password = password
		if err := ValidateSIPAnnexGConfig(config, "34020000002000000001", true); err != nil {
			t.Fatalf("opaque password %q rejected: %v", password, err)
		}
	}

	tests := []struct {
		name       string
		tlsEnabled bool
		mutate     func(*SIPAnnexG)
		want       string
	}{
		{name: "limit", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.MaxSendRecords = 10001 }, want: "0–10000"},
		{name: "inbound rate", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.InboundRate = 10001 }, want: "0–10000"},
		{name: "inbound burst", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.InboundBurst = 10001 }, want: "0–10000"},
		{name: "pending TTL", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.PendingTTL = Duration(time.Second) }, want: "1m–168h"},
		{name: "pending limit", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.MaxPending = 10001 }, want: "0–10000"},
		{name: "empty systems", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems = nil }, want: "至少配置"},
		{name: "invalid id", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].ID = "invalid" }, want: "20 位"},
		{name: "local id", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].ID = "34020000002000000001" }, want: "本平台"},
		{name: "duplicate id", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems = append(config.Systems, config.Systems[0]) }, want: "重复"},
		{name: "role", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Role = "device" }, want: "role"},
		{name: "2022", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Version = "3.0" }, want: "已删除"},
		{name: "version", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Version = "9.9" }, want: "version"},
		{name: "password", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Password = "" }, want: "password"},
		{name: "password bypass", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Password = "#" }, want: "免鉴权"},
		{name: "password raw length", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Password = strings.Repeat("a", 128) + " " }, want: "1–128"},
		{name: "signal Digest seed", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].SignalDigestSeed = "bad\nseed" }, want: "signal_digest_seed"},
		{name: "signal Digest seed raw length", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].SignalDigestSeed = strings.Repeat("a", 128) + " " }, want: "signal_digest_seed"},
		{name: "source empty", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].SourceCIDRs = nil }, want: "来源"},
		{name: "source invalid", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].SourceCIDRs = []string{"bad"} }, want: "有效"},
		{name: "realm", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Realm = "bad" }, want: "realm"},
		{name: "address", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Address = "bad" }, want: "address"},
		{name: "port", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Address = "192.0.2.10:70000" }, want: "端口"},
		{name: "transport", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Transport = "ws" }, want: "transport"},
		{name: "insecure transport", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].Transport = "udp" }, want: "明文"},
		{name: "TLS client pair", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].TLSCert = "client.crt" }, want: "同时配置"},
		{name: "TLS CRL without CA", tlsEnabled: true, mutate: func(config *SIPAnnexG) { config.Systems[0].TLSCRL = "server.crl" }, want: "TLS CA"},
		{name: "TLS", mutate: func(config *SIPAnnexG) {}, want: "SIP-TLS"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validAnnexGConfig()
			test.mutate(&config)
			err := ValidateSIPAnnexGConfig(config, "34020000002000000001", test.tlsEnabled)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}
