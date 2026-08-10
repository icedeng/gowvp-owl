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
