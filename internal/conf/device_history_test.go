package conf

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSetupConfigDefaultsMissingDeviceHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[Sip]\nPort = 5060\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg Bootstrap
	if err := SetupConfig(&cfg, path); err != nil {
		t.Fatal(err)
	}
	if cfg.Sip.DeviceHistory.MaxRecords != 1000 || cfg.Sip.DeviceHistory.MaxDays != 30 {
		t.Fatalf("unexpected defaults: %+v", cfg.Sip.DeviceHistory)
	}
}

func TestSetupConfigKeepsExplicitUnlimitedDeviceHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := "[Sip.DeviceHistory]\nMaxRecords = 0\nMaxDays = 0\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var cfg Bootstrap
	if err := SetupConfig(&cfg, path); err != nil {
		t.Fatal(err)
	}
	if cfg.Sip.DeviceHistory.MaxRecords != 0 || cfg.Sip.DeviceHistory.MaxDays != 0 {
		t.Fatalf("explicit unlimited policy was overwritten: %+v", cfg.Sip.DeviceHistory)
	}
}
