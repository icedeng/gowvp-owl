package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
)

func TestConcurrentLoginKeysRemainUsable(t *testing.T) {
	var secret Secret
	var wg sync.WaitGroup
	keys := make([]*rsa.PublicKey, 16)
	start := make(chan struct{})
	for i := range keys {
		wg.Go(func() {
			<-start
			key, err := secret.GetOrCreatePublicKey()
			if err != nil {
				t.Error(err)
				return
			}
			keys[i] = key
		})
	}
	close(start)
	wg.Wait()
	for _, key := range keys {
		if key == nil {
			t.Fatal("missing key")
		}
		ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key, []byte("credentials"), nil)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := secret.Decrypt(base64.StdEncoding.EncodeToString(ciphertext))
		if err != nil || string(plain) != "credentials" {
			t.Fatalf("issued key became unusable: %v", err)
		}
	}
}

func TestUserUpdateRunsAuthenticationOnce(t *testing.T) {
	r := gin.New()
	calls := 0
	RegisterUser(r, NewUserAPI(nil), func(c *gin.Context) { calls++; c.Next() })
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPut, "/users", nil))
	if calls != 1 {
		t.Fatalf("authentication calls = %d, want 1", calls)
	}
}

func TestLoginKeyOAEPSeed(t *testing.T) {
	api := NewUserAPI(nil)
	out, err := api.getPublicKey(nil, nil)
	if err != nil {
		t.Fatalf("getPublicKey() error = %v", err)
	}

	seed, err := base64.StdEncoding.DecodeString(out["oaep_seed"].(string))
	if err != nil {
		t.Fatalf("oaep_seed is not base64: %v", err)
	}
	if len(seed) != sha256.Size {
		t.Fatalf("oaep_seed length = %d, want %d", len(seed), sha256.Size)
	}
	if out["key"] == "" {
		t.Fatal("public key is empty")
	}
}

func encryptUserTestCredentials(t *testing.T, api UserAPI, credentials any) string {
	t.Helper()
	key, err := api.secret.GetOrCreatePublicKey()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(credentials)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, key, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(ciphertext)
}

func TestUpdateCredentialsPersistsAcrossEnvironmentOverride(t *testing.T) {
	t.Setenv("OWL_ADMIN_USERNAME", "admin")
	t.Setenv("OWL_ADMIN_PASSWORD", "initial-password")
	cfg := conf.DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	if err := conf.SecureRuntimeSecrets(&cfg); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(cfg.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	api := NewUserAPI(&cfg)
	input := map[string]string{"old_password": "initial-password", "username": "operator", "password": "updated-password"}
	_, err = api.updateCredentials(nil, &updateCredentialsInput{Data: encryptUserTestCredentials(t, api, input)})
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(cfg.ConfigPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("updating admin credentials changed the main configuration")
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(cfg.ConfigPath), conf.AdminCredentialsFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("saved credentials must have mode 0600")
	}
	restarted := conf.DefaultConfig()
	restarted.ConfigPath = cfg.ConfigPath
	if err := conf.SetupConfig(&restarted, restarted.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if err := conf.SecureRuntimeSecrets(&restarted); err != nil {
		t.Fatal(err)
	}
	restartedAPI := NewUserAPI(&restarted)
	for _, tc := range []struct {
		password string
		accepted bool
	}{{"updated-password", true}, {"initial-password", false}} {
		data := encryptUserTestCredentials(t, restartedAPI, map[string]string{"username": "operator", "password": tc.password})
		_, err := restartedAPI.login(nil, &loginInput{Data: data})
		if (err == nil) != tc.accepted {
			t.Fatalf("login after restart accepted=%v, want %v", err == nil, tc.accepted)
		}
	}
}

func TestUpdateCredentialsFailureLeavesCurrentCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, oldPassword, newPassword string
		missingDirectory               bool
	}{
		{"wrong old password", "wrong-password", "updated-password", false},
		{"short new password", "initial-password", "short", false},
		{"cannot persist", "initial-password", "updated-password", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := conf.DefaultConfig()
			cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
			if tc.missingDirectory {
				cfg.ConfigPath = filepath.Join(filepath.Dir(cfg.ConfigPath), "missing", "config.toml")
			}
			cfg.Server.Username = "admin"
			cfg.Server.PasswordHash, _ = conf.HashAdminPassword("initial-password")
			cfg.AdminSessionVersion = "before-update"
			api := NewUserAPI(&cfg)
			input := map[string]string{"old_password": tc.oldPassword, "username": "operator", "password": tc.newPassword}
			_, err := api.updateCredentials(nil, &updateCredentialsInput{Data: encryptUserTestCredentials(t, api, input)})
			if err == nil {
				t.Fatal("expected credential update to fail")
			}
			if cfg.Server.Username != "admin" || !conf.VerifyAdminPassword(cfg.Server.PasswordHash, "initial-password") || cfg.AdminSessionVersion != "before-update" {
				t.Fatal("failed update changed active credentials")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(cfg.ConfigPath), conf.AdminCredentialsFile)); !os.IsNotExist(err) {
				t.Fatal("failed update saved new credentials")
			}
		})
	}
}
