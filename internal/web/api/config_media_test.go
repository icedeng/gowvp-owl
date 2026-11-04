package api

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/sms"
)

func TestMediaConfigPersistsWithoutChangingSIPOrRecording(t *testing.T) {
	cfg := conf.DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	core := sms.NewCore(nil)
	defer core.Close()
	api := ConfigAPI{conf: &cfg, sipMu: new(sync.RWMutex), uc: &Usecase{SMSAPI: SmsAPI{smsCore: core}}}
	if _, err := api.updateMedia(nil, &mediaPlaybackConfig{StreamNoneReaderDelaySeconds: 240}); err != nil {
		t.Fatal(err)
	}
	var restored conf.Bootstrap
	if err := conf.SetupConfig(&restored, cfg.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if restored.Media.StreamNoneReaderDelaySeconds != 240 || restored.Sip.ID != cfg.Sip.ID || restored.Server.Recording.DefaultMode != "none" {
		t.Fatal("超时未持久化，或覆盖了 SIP / 默认录像模式")
	}
	if _, err := api.updateMedia(nil, &mediaPlaybackConfig{StreamNoneReaderDelaySeconds: 0}); err == nil {
		t.Fatal("接受了非法超时值")
	}
	if cfg.Media.StreamNoneReaderDelaySeconds != 240 {
		t.Fatal("非法请求改变了原有配置")
	}
}
