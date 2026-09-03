package conf

import (
	"testing"
	"time"
)

func TestDefaultDirectTCPDownloadConfigIsSafe(t *testing.T) {
	config := DefaultConfig().Sip.DirectTCPDownload
	if config.Enabled || len(config.DeviceAllowlist) != 0 || config.AllowAddressMismatch || len(config.AllowedAddressCIDRs) != 0 {
		t.Fatalf("unsafe direct TCP defaults: %+v", config)
	}
	if config.StorageDir == "" || config.RetainDays != 7 || config.OfferPort != 9 || config.MaxFileSize != 10<<30 {
		t.Fatalf("unexpected direct TCP storage defaults: %+v", config)
	}
	if config.GlobalConcurrency != 4 || config.DeviceConcurrency != 1 ||
		time.Duration(config.DialTimeout) != 5*time.Second ||
		time.Duration(config.FirstByteTimeout) != 15*time.Second ||
		time.Duration(config.IdleTimeout) != 30*time.Second ||
		time.Duration(config.TotalTimeout) != 2*time.Hour {
		t.Fatalf("unexpected direct TCP resource defaults: %+v", config)
	}
}

func TestProjectConfigParsesDirectTCPDownload(t *testing.T) {
	config := DefaultConfig()
	if err := SetupConfig(&config, "../../configs/config.toml"); err != nil {
		t.Fatal(err)
	}
	direct := config.Sip.DirectTCPDownload
	if direct.Enabled || direct.StorageDir != "./configs/downloads/gb28181" || direct.MaxFileSize != 10<<30 || time.Duration(direct.TotalTimeout) != 2*time.Hour {
		t.Fatalf("parsed direct TCP config = %+v", direct)
	}
}

func TestValidateSIPDirectTCPDownloadConfig(t *testing.T) {
	valid := DefaultConfig().Sip.DirectTCPDownload
	valid.Enabled = true
	valid.DeviceAllowlist = []string{"34020000001320000001"}
	if err := ValidateSIPDirectTCPDownloadConfig(valid); err != nil {
		t.Fatalf("valid direct TCP download config was rejected: %v", err)
	}

	tests := []struct {
		name   string
		change func(*SIPDirectTCPDownload)
	}{
		{name: "missing allowlist", change: func(config *SIPDirectTCPDownload) { config.DeviceAllowlist = nil }},
		{name: "invalid device ID", change: func(config *SIPDirectTCPDownload) { config.DeviceAllowlist = []string{"device"} }},
		{name: "duplicate device ID", change: func(config *SIPDirectTCPDownload) {
			config.DeviceAllowlist = []string{"34020000001320000001", "34020000001320000001"}
		}},
		{name: "missing storage directory", change: func(config *SIPDirectTCPDownload) { config.StorageDir = "" }},
		{name: "invalid retain days", change: func(config *SIPDirectTCPDownload) { config.RetainDays = 0 }},
		{name: "invalid offer port", change: func(config *SIPDirectTCPDownload) { config.OfferPort = 70000 }},
		{name: "invalid max file size", change: func(config *SIPDirectTCPDownload) { config.MaxFileSize = -1 }},
		{name: "invalid global concurrency", change: func(config *SIPDirectTCPDownload) { config.GlobalConcurrency = 0 }},
		{name: "invalid device concurrency", change: func(config *SIPDirectTCPDownload) { config.DeviceConcurrency = 0 }},
		{name: "device concurrency exceeds global", change: func(config *SIPDirectTCPDownload) { config.GlobalConcurrency = 1; config.DeviceConcurrency = 2 }},
		{name: "invalid dial timeout", change: func(config *SIPDirectTCPDownload) { config.DialTimeout = 0 }},
		{name: "invalid first byte timeout", change: func(config *SIPDirectTCPDownload) { config.FirstByteTimeout = 0 }},
		{name: "invalid idle timeout", change: func(config *SIPDirectTCPDownload) { config.IdleTimeout = 0 }},
		{name: "invalid total timeout", change: func(config *SIPDirectTCPDownload) { config.TotalTimeout = 0 }},
		{name: "total timeout shorter than phase", change: func(config *SIPDirectTCPDownload) { config.TotalTimeout = Duration(time.Second) }},
		{name: "address mismatch without CIDR", change: func(config *SIPDirectTCPDownload) {
			config.AllowAddressMismatch = true
			config.AllowedAddressCIDRs = nil
		}},
		{name: "invalid allowed CIDR", change: func(config *SIPDirectTCPDownload) {
			config.AllowAddressMismatch = true
			config.AllowedAddressCIDRs = []string{"not-a-cidr"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			config.DeviceAllowlist = append([]string(nil), valid.DeviceAllowlist...)
			config.AllowedAddressCIDRs = append([]string(nil), valid.AllowedAddressCIDRs...)
			test.change(&config)
			if err := ValidateSIPDirectTCPDownloadConfig(config); err == nil {
				t.Fatalf("invalid direct TCP download config was accepted: %+v", config)
			}
		})
	}

	// 默认关闭时保持旧配置兼容；真正启用前必须满足完整安全约束。
	if err := ValidateSIPDirectTCPDownloadConfig(SIPDirectTCPDownload{}); err != nil {
		t.Fatalf("disabled legacy direct TCP download config was rejected: %v", err)
	}
}
