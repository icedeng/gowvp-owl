package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
)

func TestMergeSIPUpdatePreservesOmittedNestedConfiguration(t *testing.T) {
	current := conf.SIP{
		Host: "192.0.2.10", Port: 5060,
		DeviceHistory: conf.DeviceHistoryConfig{MaxRecords: 1000, MaxDays: 30},
		SignalDigest: conf.SIPSignalDigest{
			Enabled: true, Required: true, Seed: "current-seed", Algorithm: "SHA-256",
			Encoding: "base64", AcceptLegacyHex: true, Window: conf.Duration(5 * time.Minute),
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
