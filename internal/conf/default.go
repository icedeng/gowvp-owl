package conf

import (
	"time"

	"github.com/ixugo/goddd/pkg/orm"
)

func DefaultConfig() Bootstrap {
	return Bootstrap{
		Server: Server{
			Username:   "admin",
			Password:   "admin",
			RTMPSecret: "123",
			HTTP: ServerHTTP{
				Port:      15123,
				Timeout:   Duration(60 * time.Second),
				JwtSecret: orm.GenerateRandomString(24),
				AuthURL:   "",
				PProf: ServerPPROF{
					Enabled:   true,
					AccessIps: []string{"::1", "127.0.0.1"},
				},
			},
			AI: ServerAI{
				Disabled:   false,
				RetainDays: 7,
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
			DirectTCPDownload: SIPDirectTCPDownload{
				Enabled:              false,
				DeviceAllowlist:      nil,
				StorageDir:           "./configs/downloads/gb28181",
				RetainDays:           7,
				OfferPort:            9,
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
			MaxAge:       Duration(3 * 24 * time.Hour),
			RotationTime: Duration(8 * time.Hour),
			RotationSize: 50,
		},
	}
}
