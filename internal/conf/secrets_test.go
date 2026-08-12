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
	cfg := DefaultConfig()
	if err := SecureRuntimeSecrets(&cfg); err != nil {
		t.Fatalf("SecureRuntimeSecrets() error = %v", err)
	}
	if cfg.Server.Password != "environment-password" || cfg.Server.HTTP.JwtSecret != "environment-jwt" {
		t.Fatalf("environment overrides not applied")
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
