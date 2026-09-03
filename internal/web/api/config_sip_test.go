package api

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
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
		AnnexG: conf.SIPAnnexG{Enabled: true, MaxSendRecords: 50, InboundRate: 25, InboundBurst: 40, Systems: []conf.SIPAnnexGSystem{{
			ID: "34020000002000000002", Role: "emergency_command_system", Version: "1.0",
			Password: "secret", SignalDigestSeed: "annex-g-note-seed", Address: "192.0.2.40:5061", Transport: "tls", SourceCIDRs: []string{"192.0.2.40"},
		}}},
		Upstreams: []conf.SIPUpstream{{Name: "upstream-a", ServerID: "34020000002000000001"}},
		AlarmReceivers: []conf.SIPAlarmReceiver{{
			Name: "receiver-a", Enabled: true, DeviceID: "34020000002000000011",
			SourceIDs: []string{"34020000001320000001"},
		}},
		Log: conf.SIPLog{
			Enabled: true, Dir: "./logs/sip", MaxAge: conf.Duration(7 * 24 * time.Hour), RotationSize: 50,
		},
	}
	current.DirectTCPDownload = conf.DefaultConfig().Sip.DirectTCPDownload
	current.DirectTCPDownload.Enabled = true
	current.DirectTCPDownload.DeviceAllowlist = []string{"34020000001320000001"}
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
	if len(next.AlarmReceivers) != 1 || next.AlarmReceivers[0].DeviceID != "34020000002000000011" ||
		len(next.AlarmReceivers[0].SourceIDs) != 1 || next.AlarmReceivers[0].SourceIDs[0] != "34020000001320000001" {
		t.Fatalf("omitted Alarm receiver config was cleared: %+v", next.AlarmReceivers)
	}
	current.AlarmReceivers[0].SourceIDs[0] = "mutated"
	if next.AlarmReceivers[0].SourceIDs[0] != "34020000001320000001" {
		t.Fatal("omitted Alarm receiver config was not deep-cloned")
	}
	if next.SignalDigest != current.SignalDigest {
		t.Fatalf("omitted signal Digest was cleared: got %+v want %+v", next.SignalDigest, current.SignalDigest)
	}
	if !next.RegisterCertificateAuth.Enabled || next.RegisterCertificateAuth.PlatformCert != "platform.crt" ||
		next.RegisterCertificateAuth.DeviceCertificates["34020000001320000001"] != "device.crt" {
		t.Fatalf("omitted certificate REGISTER config was cleared: %+v", next.RegisterCertificateAuth)
	}
	if !next.AnnexG.Enabled || next.AnnexG.InboundRate != 25 || next.AnnexG.InboundBurst != 40 ||
		len(next.AnnexG.Systems) != 1 || next.AnnexG.Systems[0].Address != "192.0.2.40:5061" ||
		next.AnnexG.Systems[0].SignalDigestSeed != "annex-g-note-seed" {
		t.Fatalf("omitted Annex G config was cleared: %+v", next.AnnexG)
	}
	if next.TLSClientCA != current.TLSClientCA || next.TLSRequireClientCert != current.TLSRequireClientCert {
		t.Fatalf("omitted TLS client verification config was cleared: %+v", next)
	}
	if !reflect.DeepEqual(next.DirectTCPDownload, current.DirectTCPDownload) {
		t.Fatalf("omitted direct TCP download config was cleared: got %+v want %+v", next.DirectTCPDownload, current.DirectTCPDownload)
	}
	current.DirectTCPDownload.DeviceAllowlist[0] = "mutated"
	if next.DirectTCPDownload.DeviceAllowlist[0] != "34020000001320000001" {
		t.Fatal("omitted direct TCP download config was not deep-cloned")
	}
}

func TestGetConfigInfoRedactsSIPPeerSecrets(t *testing.T) {
	config := conf.DefaultConfig()
	config.Sip.Password = "device-registration-password"
	config.Sip.SignalDigest.Seed = "global-note-seed"
	config.Sip.Upstreams = []conf.SIPUpstream{{
		Name: "upstream-a", Password: "upstream-password", SignalDigestSeed: "upstream-note-seed",
	}}
	config.Sip.AnnexG.Systems = []conf.SIPAnnexGSystem{{
		ID: "34020000002000000002", Password: "annex-g-password", SignalDigestSeed: "annex-g-note-seed",
	}}
	runtimeConfig := config.Sip
	api := ConfigAPI{conf: &config, sipMu: new(sync.RWMutex), sipConfig: &runtimeConfig}

	output, err := api.getConfigInfo(nil, &struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(payload)
	for _, secret := range []string{
		"global-note-seed", "upstream-password", "upstream-note-seed", "annex-g-password", "annex-g-note-seed",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("GET /configs/info exposed SIP peer secret %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, `"password":"device-registration-password"`) {
		t.Fatalf("device access password was unexpectedly removed: %s", text)
	}
	for _, marker := range []string{
		`"signal_digest_seed_configured":true`, `"password_configured":true`,
		`"upstream-a"`, `"34020000002000000002"`,
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("GET /configs/info is missing secret status %s: %s", marker, text)
		}
	}
}

func TestMergeSIPUpdatePreservesRedactedSecretsAndSupportsExplicitClear(t *testing.T) {
	current := conf.SIP{
		SignalDigest: conf.SIPSignalDigest{
			Enabled: true, Seed: " global seed ", Algorithm: "SHA-256", Encoding: "base64",
		},
		Upstreams: []conf.SIPUpstream{{
			Name: "upstream-a", ServerID: "34020000002000000001", Host: "192.0.2.30", Port: 5060,
			Password: " upstream password ", SignalDigestSeed: " upstream seed ", Version: "1.0",
		}},
		AnnexG: conf.SIPAnnexG{Systems: []conf.SIPAnnexGSystem{{
			ID: "34020000002000000002", Password: " annex password ", SignalDigestSeed: " annex seed ",
		}}},
	}

	decode := func(t *testing.T, body string) updateSIPInput {
		t.Helper()
		var input updateSIPInput
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			t.Fatal(err)
		}
		return input
	}

	preserved := mergeSIPUpdate(current, func() *updateSIPInput {
		input := decode(t, `{
			"signal_digest":{"enabled":true,"seed":"","algorithm":"SHA-256","encoding":"base64"},
			"upstreams":[{"name":"upstream-a","server_id":"34020000002000000001","host":"192.0.2.30","port":5060,"password":"","signal_digest_seed":"","version":"1.0"}],
			"annex_g":{"systems":[{"id":"34020000002000000002","password":"","signal_digest_seed":""}]}
		}`)
		return &input
	}())
	if preserved.SignalDigest.Seed != current.SignalDigest.Seed ||
		preserved.Upstreams[0].Password != current.Upstreams[0].Password ||
		preserved.Upstreams[0].SignalDigestSeed != current.Upstreams[0].SignalDigestSeed ||
		preserved.AnnexG.Systems[0].Password != current.AnnexG.Systems[0].Password ||
		preserved.AnnexG.Systems[0].SignalDigestSeed != current.AnnexG.Systems[0].SignalDigestSeed {
		t.Fatalf("redacted round trip cleared configured secrets: %+v", preserved)
	}

	renamed := mergeSIPUpdate(current, func() *updateSIPInput {
		input := decode(t, `{
			"upstreams":[{"name":"upstream-renamed","server_id":"34020000002000000001","host":"192.0.2.30","port":5060,"password":"","signal_digest_seed":"","version":"1.0"}]
		}`)
		return &input
	}())
	if renamed.Upstreams[0].Password != current.Upstreams[0].Password ||
		renamed.Upstreams[0].SignalDigestSeed != current.Upstreams[0].SignalDigestSeed {
		t.Fatalf("renaming an otherwise identical upstream cleared configured secrets: %+v", renamed.Upstreams[0])
	}

	cleared := mergeSIPUpdate(current, func() *updateSIPInput {
		input := decode(t, `{
			"signal_digest":{"enabled":true,"seed":"","algorithm":"SHA-256","encoding":"base64"},
			"upstreams":[{"name":"upstream-a","server_id":"34020000002000000001","host":"192.0.2.30","port":5060,"password":"","signal_digest_seed":"","version":"1.0"}],
			"annex_g":{"systems":[{"id":"34020000002000000002","password":"","signal_digest_seed":""}]},
			"secret_clears":{"signal_digest_seed":true,"upstream_passwords":["upstream-a"],"upstream_signal_digest_seeds":["upstream-a"],"annex_g_passwords":["34020000002000000002"],"annex_g_signal_digest_seeds":["34020000002000000002"]}
		}`)
		return &input
	}())
	if cleared.SignalDigest.Seed != "" || cleared.Upstreams[0].Password != "" ||
		cleared.Upstreams[0].SignalDigestSeed != "" || cleared.AnnexG.Systems[0].Password != "" ||
		cleared.AnnexG.Systems[0].SignalDigestSeed != "" {
		t.Fatalf("explicit secret clear was ignored: %+v", cleared)
	}

	replaced := mergeSIPUpdate(current, func() *updateSIPInput {
		input := decode(t, `{
			"signal_digest":{"enabled":true,"seed":" replacement global ","algorithm":"SHA-256","encoding":"base64"},
			"upstreams":[{"name":"upstream-a","server_id":"34020000002000000001","host":"192.0.2.30","port":5060,"password":" replacement password ","signal_digest_seed":" replacement upstream ","version":"1.0"}],
			"annex_g":{"systems":[{"id":"34020000002000000002","password":" replacement annex password ","signal_digest_seed":" replacement annex seed "}]}
		}`)
		return &input
	}())
	if replaced.SignalDigest.Seed != " replacement global " ||
		replaced.Upstreams[0].Password != " replacement password " ||
		replaced.Upstreams[0].SignalDigestSeed != " replacement upstream " ||
		replaced.AnnexG.Systems[0].Password != " replacement annex password " ||
		replaced.AnnexG.Systems[0].SignalDigestSeed != " replacement annex seed " {
		t.Fatalf("secret replacement did not preserve raw bytes: %+v", replaced)
	}
}

func TestMergeSIPUpdateAppliesExplicitAlarmReceivers(t *testing.T) {
	current := conf.SIP{AlarmReceivers: []conf.SIPAlarmReceiver{{
		Name: "old", Enabled: true, DeviceID: "34020000002000000011", SourceIDs: []string{"3402000000"},
	}}}
	replacement := []conf.SIPAlarmReceiver{{
		Name: "new", Enabled: true, DeviceID: "34020000002000000012",
		SourceIDs: []string{"34020000001320000001"},
	}}
	next := mergeSIPUpdate(current, &updateSIPInput{SIP: current, AlarmReceivers: &replacement})
	if len(next.AlarmReceivers) != 1 || next.AlarmReceivers[0].Name != "new" ||
		next.AlarmReceivers[0].SourceIDs[0] != "34020000001320000001" {
		t.Fatalf("explicit Alarm receiver config = %+v", next.AlarmReceivers)
	}
	replacement[0].SourceIDs[0] = "mutated"
	if next.AlarmReceivers[0].SourceIDs[0] != "34020000001320000001" {
		t.Fatal("explicit Alarm receiver config was not deep-cloned")
	}
}

func TestApplyHotSIPConfigClonesAlarmReceivers(t *testing.T) {
	target := conf.SIP{}
	next := conf.SIP{AlarmReceivers: []conf.SIPAlarmReceiver{{
		Name: "receiver", Enabled: true, DeviceID: "34020000002000000011",
		SourceIDs: []string{"34020000001320000001"},
	}}}
	applyHotSIPConfig(&target, next)
	next.AlarmReceivers[0].SourceIDs[0] = "mutated"
	if len(target.AlarmReceivers) != 1 || target.AlarmReceivers[0].SourceIDs[0] != "34020000001320000001" {
		t.Fatalf("hot Alarm receiver config = %+v", target.AlarmReceivers)
	}
}

func TestMergeSIPUpdateAppliesExplicitAnnexGConfiguration(t *testing.T) {
	current := conf.SIP{AnnexG: conf.SIPAnnexG{Enabled: true, MaxSendRecords: 10}}
	replacement := conf.SIPAnnexG{Enabled: true, MaxSendRecords: 100, InboundRate: 20, InboundBurst: 30, Systems: []conf.SIPAnnexGSystem{{
		ID: "34020000002000000002", SourceCIDRs: []string{"192.0.2.10"},
	}}}
	next := mergeSIPUpdate(current, &updateSIPInput{SIP: current, AnnexG: &replacement})
	if !next.AnnexG.Enabled || next.AnnexG.MaxSendRecords != 100 || next.AnnexG.InboundRate != 20 || next.AnnexG.InboundBurst != 30 || len(next.AnnexG.Systems) != 1 {
		t.Fatalf("explicit Annex G config = %+v", next.AnnexG)
	}
	replacement.Systems[0].SourceCIDRs[0] = "198.51.100.10"
	if next.AnnexG.Systems[0].SourceCIDRs[0] != "192.0.2.10" {
		t.Fatal("explicit Annex G config was not deep-cloned")
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
	body := []byte(`{"upstreams":[{"name":"upstream","enabled":true,"server_id":"34020000002000000001","host":"192.0.2.30","transport":"tcp","keepalive_interval":"60s","alarm_dispatch_enabled":true}],"direct_tcp_download":{"dial_timeout":"5s"}}`)
	if err := json.Unmarshal(body, &input); err != nil {
		t.Fatal(err)
	}
	if input.Upstreams == nil || len(*input.Upstreams) != 1 || (*input.Upstreams)[0].Transport != "tcp" || (*input.Upstreams)[0].KeepaliveInterval != conf.Duration(time.Minute) || !(*input.Upstreams)[0].AlarmDispatchEnabled || input.DirectTCPDownload == nil || input.DirectTCPDownload.DialTimeout != conf.Duration(5*time.Second) {
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

func TestApplySIPRuntimeFailureRollsBackPersistedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	config := conf.DefaultConfig()
	config.ConfigPath = path
	current := config.Sip
	next := current
	next.Password = "replacement-password"
	candidate := config
	candidate.Sip = next
	if err := conf.WriteConfig(&candidate, path); err != nil {
		t.Fatal(err)
	}

	simulated := errors.New("simulated runtime apply failure")
	if err := applySIPRuntimeWithRollback(&config, current, next, func(conf.SIP) error { return simulated }); !errors.Is(err, simulated) {
		t.Fatalf("runtime apply error = %v, want %v", err, simulated)
	}
	var persisted conf.Bootstrap
	if err := conf.SetupConfig(&persisted, path); err != nil {
		t.Fatal(err)
	}
	if persisted.Sip.Password != current.Password {
		t.Fatalf("persisted password after rollback = %q, want %q", persisted.Sip.Password, current.Password)
	}
	if config.Sip.Password != current.Password {
		t.Fatalf("shared config changed after runtime failure: %q", config.Sip.Password)
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

func TestUpdateSIPRejectsInvalidRegisterRedirectBeforePersisting(t *testing.T) {
	config := conf.DefaultConfig()
	config.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	if err := conf.WriteConfig(&config, config.ConfigPath); err != nil {
		t.Fatal(err)
	}
	current := config.Sip
	next := current
	next.RegisterRedirect = "sip:" + current.ID + ":secret@192.0.2.31"
	runtimeConfig := current
	api := ConfigAPI{conf: &config, sipMu: new(sync.RWMutex), sipConfig: &runtimeConfig}

	_, err := api.updateSIP(nil, &updateSIPInput{SIP: next})
	if err == nil || !strings.Contains(err.Error(), "不能包含密码") {
		t.Fatalf("invalid REGISTER redirect update error = %v", err)
	}
	if runtimeConfig.RegisterRedirect != current.RegisterRedirect || config.Sip.RegisterRedirect != current.RegisterRedirect {
		t.Fatalf("invalid REGISTER redirect changed runtime/shared config: runtime=%q shared=%q", runtimeConfig.RegisterRedirect, config.Sip.RegisterRedirect)
	}
	var persisted conf.Bootstrap
	if err := conf.SetupConfig(&persisted, config.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if persisted.Sip.RegisterRedirect != current.RegisterRedirect {
		t.Fatalf("invalid REGISTER redirect was persisted: %q", persisted.Sip.RegisterRedirect)
	}
}

func TestUpdateSIPRejectsInvalidDirectTCPDownloadBeforePersisting(t *testing.T) {
	config := conf.DefaultConfig()
	config.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	if err := conf.WriteConfig(&config, config.ConfigPath); err != nil {
		t.Fatal(err)
	}
	current := config.Sip
	direct := current.DirectTCPDownload
	direct.Enabled = true
	direct.DeviceAllowlist = nil
	runtimeConfig := current
	api := ConfigAPI{conf: &config, sipMu: new(sync.RWMutex), sipConfig: &runtimeConfig}

	_, err := api.updateSIP(nil, &updateSIPInput{SIP: current, DirectTCPDownload: &direct})
	if err == nil || !strings.Contains(err.Error(), "设备白名单") {
		t.Fatalf("invalid direct TCP download update error = %v", err)
	}
	if !reflect.DeepEqual(runtimeConfig.DirectTCPDownload, current.DirectTCPDownload) ||
		!reflect.DeepEqual(config.Sip.DirectTCPDownload, current.DirectTCPDownload) {
		t.Fatalf("invalid direct TCP download changed runtime/shared config: runtime=%+v shared=%+v", runtimeConfig.DirectTCPDownload, config.Sip.DirectTCPDownload)
	}
	var persisted conf.Bootstrap
	if err := conf.SetupConfig(&persisted, config.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if persisted.Sip.DirectTCPDownload.Enabled != current.DirectTCPDownload.Enabled ||
		persisted.Sip.DirectTCPDownload.StorageDir != current.DirectTCPDownload.StorageDir ||
		persisted.Sip.DirectTCPDownload.GlobalConcurrency != current.DirectTCPDownload.GlobalConcurrency {
		t.Fatalf("invalid direct TCP download was persisted: %+v", persisted.Sip.DirectTCPDownload)
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
		{name: "Annex G", change: func(config *conf.SIP) { config.AnnexG.Enabled = true }},
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
	hot.AlarmReceivers = []conf.SIPAlarmReceiver{{
		Name: "receiver", Enabled: true, DeviceID: "34020000002000000011", SourceIDs: []string{"3402000000"},
	}}
	if fields := sipRestartRequiredFields(current, hot); len(fields) != 0 {
		t.Fatalf("hot-reloadable changes require restart: %v", fields)
	}
}
