package conf

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdminCredentialMigrationAndONVIFSeparation(t *testing.T) {
	for _, legacyFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "main config", true: "credentials file"}[legacyFile], func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
			cfg.Server.Password = "legacy-main-password"
			password := cfg.Server.Password
			if legacyFile {
				password = "legacy-saved-password"
				if err := os.WriteFile(filepath.Join(filepath.Dir(cfg.ConfigPath), AdminCredentialsFile), []byte("Username = 'operator'\nPassword = '"+password+"'\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := SecureRuntimeSecrets(&cfg); err != nil {
				t.Fatal(err)
			}
			if cfg.Server.Password != "" || !VerifyAdminPassword(cfg.Server.PasswordHash, password) || cfg.AdminSessionVersion == "" {
				t.Fatal("migration lost credentials or session version")
			}
			if cfg.Server.ONVIF.Password == "" || cfg.Server.ONVIF.Password == password || cfg.Server.ONVIF.Password == "legacy-main-password" {
				t.Fatal("ONVIF credentials were not separated")
			}
			for _, name := range []string{"config.toml", AdminCredentialsFile} {
				data, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.ConfigPath), name))
				if os.IsNotExist(err) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte(password)) || bytes.Contains(data, []byte("legacy-main-password")) {
					t.Fatalf("plaintext remains in %s", name)
				}
			}
			restarted := DefaultConfig()
			restarted.ConfigPath = cfg.ConfigPath
			if err := SetupConfig(&restarted, cfg.ConfigPath); err != nil {
				t.Fatal(err)
			}
			if err := SecureRuntimeSecrets(&restarted); err != nil {
				t.Fatal(err)
			}
			if restarted.AdminSessionVersion != cfg.AdminSessionVersion || restarted.Server.ONVIF != cfg.Server.ONVIF || !VerifyAdminPassword(restarted.Server.PasswordHash, password) {
				t.Fatal("restart changed migrated identity")
			}
		})
	}
}

func TestAdminPasswordHashAndMissingCredentials(t *testing.T) {
	password := strings.Repeat("long-password", 10)
	one, err := HashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	two, err := HashAdminPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	if one == two || !VerifyAdminPassword(one, password) || VerifyAdminPassword(one, password+"x") || VerifyAdminPassword("broken", password) {
		t.Fatal("invalid password hashing behavior")
	}
	cfg := DefaultConfig()
	if err := SecureRuntimeSecrets(&cfg); err == nil {
		t.Fatal("fresh install without explicit credentials should fail")
	}
}

func TestAdminCredentialMigrationFailureDoesNotFallback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	cfg.Server.Password = "legacy-password"
	path := filepath.Join(filepath.Dir(cfg.ConfigPath), AdminCredentialsFile)
	if err := os.WriteFile(path, []byte("Username = 'admin'\nPasswordHash = 'broken'\nPassword = 'legacy-password'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SecureRuntimeSecrets(&cfg); err == nil {
		t.Fatal("corrupt saved hash fell back to old password")
	}
}

func TestSavedAdminHashTakesPriorityOverOldEnvironment(t *testing.T) {
	t.Setenv(envAdminPassword, "admin")
	cfg := DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	_, version, err := SaveAdminCredentials(&cfg, "operator", "saved-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := SecureRuntimeSecrets(&cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Username != "operator" || cfg.AdminSessionVersion != version || !VerifyAdminPassword(cfg.Server.PasswordHash, "saved-password") {
		t.Fatal("saved credentials lost priority")
	}
}
