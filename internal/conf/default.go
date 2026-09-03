package conf

import (
	"time"
)

func DefaultConfig() Bootstrap {
	return Bootstrap{
		Server: Server{
			Username: "admin",
			HTTP: ServerHTTP{
				Port:    15123,
				Timeout: Duration(60 * time.Second),
				AuthURL: "",
				PProf: ServerPPROF{
					Enabled:   false,
					AccessIps: []string{"::1", "127.0.0.1"},
				},
			},
			Webhook: ServerWebhook{},
			AI: ServerAI{
				Disabled:             false,
				RetainDays:           7,
				AnalysisInterval:     2.0,
				AlertCooldownSeconds: 10,
			},
			Recording: ServerRecording{
				Disabled:           false,
				DefaultMode:        "always",
				StorageDir:         "./configs/recordings",
				RetainDays:         3,
				DiskUsageThreshold: 95.0,
				SegmentSeconds:     300,
			},
		},
		Data: Data{
			Database: Database{
				Dsn:             "./configs/data.db",
				MaxIdleConns:    10,
				MaxOpenConns:    50,
				ConnMaxLifetime: Duration(6 * time.Hour),
				SlowThreshold:   Duration(200 * time.Millisecond),
			},
		},
		Sip: SIP{
			Host:               "",
			Port:               15060,
			ID:                 "34010000002000000001",
			Domain:             "3401000000",
			Password:           "",
			EnableTLS:          false,
			TLSPort:            15061,
			TLSCert:            "",
			TLSKey:             "",
			StrictSourceCheck:  false,
			RequireMessageAuth: false,
			PTZWeakConfirm:     false,
			RegisterRedirect:   "",
			RegisterCertificateAuth: SIPRegisterCertificateAuth{
				Enabled:            false,
				Required:           false,
				DeviceCertificates: map[string]string{},
			},
			SignalDigest: SIPSignalDigest{
				Enabled:         false,
				Required:        false,
				Seed:            "",
				Algorithm:       "MD5",
				Encoding:        "base64",
				AcceptLegacyHex: true,
				Window:          Duration(10 * time.Minute),
			},
			DeviceHistory: DeviceHistoryConfig{MaxRecords: 1000, MaxDays: 30},
			AnnexG: SIPAnnexG{
				Enabled: false, MaxSendRecords: 100,
				InboundRate: 50, InboundBurst: 100,
				PendingTTL: Duration(24 * time.Hour), MaxPending: 4096,
			},
			DirectTCPDownload: SIPDirectTCPDownload{
				Enabled:              false,
				CascadeRelayEnabled:  false,
				DeviceAllowlist:      nil,
				StorageDir:           "./configs/downloads/gb28181",
				RetainDays:           7,
				OfferPort:            9,
				RelayListenIP:        "0.0.0.0",
				RelayAdvertiseIP:     "",
				RelayPortStart:       30200,
				RelayPortEnd:         30300,
				MaxFileSize:          10 << 30,
				GlobalConcurrency:    4,
				DeviceConcurrency:    1,
				DialTimeout:          Duration(5 * time.Second),
				FirstByteTimeout:     Duration(15 * time.Second),
				IdleTimeout:          Duration(30 * time.Second),
				TotalTimeout:         Duration(2 * time.Hour),
				AllowAddressMismatch: false,
				AllowedAddressCIDRs:  nil,
			},
			Log: SIPLog{
				Enabled:      false,
				Dir:          "./logs/sip",
				MaxAge:       Duration(3 * 24 * time.Hour),
				RotationTime: Duration(8 * time.Hour),
				RotationSize: 50,
			},
		},
		Media: Media{
			IP:                          "127.0.0.1",
			HTTPPort:                    80,
			Secret:                      "",
			GBSnapshotBaseURL:           "",
			GBSnapshotFFmpegConcurrency: 2,
			WebHookIP:                   "127.0.0.1",
			SDPIP:                       "127.0.0.1",
			RTPPortRange:                "20000-20100",
			Type:                        "zlm",
		},
		Log: Log{
			Dir:          "./logs",
			Level:        "error",
			MaxAge:       0,
			MaxDays:      7,
			MaxSize:      50,
			RotationTime: Duration(12 * time.Hour),
		},
	}
}
