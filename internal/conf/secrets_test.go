package conf

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSecureRuntimeSecretsRotatesRevokedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := DefaultConfig()
	cfg.ConfigPath = path
	cfg.Server.Password = "admin"
	cfg.Server.RTMPSecret = "123"
	cfg.Server.HTTP.JwtSecret = "6caOiETMs8SPWNHgEKA1Jhmn9wxpjAj9"
	cfg.Media.Secret = "jvRqCAzEg7AszBi4gm1cfhwXpmnVmJMG"

	if err := SecureRuntimeSecrets(&cfg); err != nil {
		t.Fatalf("SecureRuntimeSecrets() error = %v", err)
	}
	for name, value := range map[string]string{
		"password": cfg.Server.Password,
		"rtmp":     cfg.Server.RTMPSecret,
		"jwt":      cfg.Server.HTTP.JwtSecret,
		"webhook":  cfg.Server.Webhook.RecvSecret,
		"media":    cfg.Media.Secret,
	} {
		if value == "" || isRevokedSecret(value) {
			t.Fatalf("%s secret was not rotated", name)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSecureRuntimeSecretsEnvironmentOverride(t *testing.T) {
	t.Setenv(envAdminPassword, "environment-password")
	t.Setenv(envJWTSecret, "environment-jwt")
	t.Setenv(envMediaIP, "zlm")
	t.Setenv(envMediaHTTPPort, "80")
	t.Setenv(envMediaWebhook, "gowvp")
	t.Setenv(envMediaSDPIP, "192.0.2.10")
	cfg := DefaultConfig()
	if err := SecureRuntimeSecrets(&cfg); err != nil {
		t.Fatalf("SecureRuntimeSecrets() error = %v", err)
	}
	if cfg.Server.Password != "environment-password" || cfg.Server.HTTP.JwtSecret != "environment-jwt" {
		t.Fatalf("environment overrides not applied")
	}
	if cfg.Media.IP != "zlm" || cfg.Media.HTTPPort != 80 || cfg.Media.WebHookIP != "gowvp" || cfg.Media.SDPIP != "192.0.2.10" {
		t.Fatalf("media environment overrides not applied: %+v", cfg.Media)
	}
}

func TestSecureRuntimeSecretsRejectsInvalidMediaHTTPPort(t *testing.T) {
	t.Setenv(envMediaHTTPPort, "70000")
	cfg := DefaultConfig()
	if err := SecureRuntimeSecrets(&cfg); err == nil {
		t.Fatal("expected invalid media HTTP port to fail")
	}
}

func TestSecureRuntimeSecretsDoesNotPersistEnvironmentOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv(envAdminPassword, "environment-password")
	cfg := DefaultConfig()
	cfg.ConfigPath = path
	if err := SecureRuntimeSecrets(&cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || bytes.Contains(data, []byte("environment-password")) {
		t.Fatal("environment secret was persisted")
	}
}
