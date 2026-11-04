package openplatform

import (
	"encoding/base64"
	"testing"
)

func TestConfigHTTPRequiresExplicitOptIn(t *testing.T) {
	t.Setenv("OWL_OPEN_PLATFORM_ENABLED", "true")
	t.Setenv("OWL_OPEN_PLATFORM_AUDIENCE", "owl-test")
	t.Setenv("OWL_OPEN_PLATFORM_PUBLIC_URL", "http://10.144.0.20:15123")
	t.Setenv("OWL_OPEN_PLATFORM_KEY_ID", "v1")
	t.Setenv("OWL_OPEN_PLATFORM_KEYS_FILE", "")
	t.Setenv("OWL_OPEN_PLATFORM_KEYS", `{"v1":"`+base64.RawURLEncoding.EncodeToString(make([]byte, 32))+`"}`)
	t.Setenv("OWL_OPEN_PLATFORM_TRUSTED_PROXIES", "")
	t.Setenv("OWL_OPEN_PLATFORM_CALLBACK_DENY_CIDRS", "")
	t.Setenv("OWL_OPEN_PLATFORM_ALLOW_HTTP", "false")
	if _, err := ConfigFromEnv(t.TempDir()); err == nil {
		t.Fatal("HTTP must be rejected by default")
	}
	t.Setenv("OWL_OPEN_PLATFORM_ALLOW_HTTP", "true")
	cfg, err := ConfigFromEnv(t.TempDir())
	if err != nil || !cfg.AllowHTTP || !cfg.AllowsScheme("http") || cfg.AllowsScheme("ftp") {
		t.Fatal("explicit HTTP config failed", err)
	}
}

func TestCallbackHTTPRequiresExplicitOptIn(t *testing.T) {
	s, p, _ := fixture(t)
	if _, err := s.SaveTarget(p.App.ID, "", "http://receiver.example/alarm", nil, "pending", 0, "admin"); err == nil {
		t.Fatal("HTTP callback accepted by default")
	}
	s.Config.AllowHTTP = true
	if _, err := s.SaveTarget(p.App.ID, "", "http://receiver.example/alarm", nil, "pending", 0, "admin"); err != nil {
		t.Fatal("explicit HTTP callback rejected", err)
	}
	if _, err := s.SaveTarget(p.App.ID, "", "http://open.example/admin", nil, "pending", 0, "admin"); err == nil {
		t.Fatal("platform callback target accepted")
	}
}
