package conf

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	envAdminUsername = "OWL_ADMIN_USERNAME"
	envAdminPassword = "OWL_ADMIN_PASSWORD"
	envJWTSecret     = "OWL_JWT_SECRET"
	envRTMPSecret    = "OWL_RTMP_SECRET"
	envSIPPassword   = "OWL_SIP_PASSWORD"
	envMediaSecret   = "OWL_MEDIA_SECRET"
	envMediaIP       = "OWL_MEDIA_IP"
	envMediaHTTPPort = "OWL_MEDIA_HTTP_PORT"
	envMediaWebhook  = "OWL_MEDIA_WEBHOOK_IP"
	envMediaSDPIP    = "OWL_MEDIA_SDP_IP"
	envWebhookSecret = "OWL_WEBHOOK_SECRET"
)

var revokedSecretHashes = map[string]struct{}{
	"8c6976e5b5410415bde908bd4dee15dfb167a9c873fc4bb8a81f6f2ab448a918": {},
	"a665a45920422f9d417e4867efdc4fb8a04a1f3fff1fa07e998e86f7f7a27ae3": {},
	"b87ccb4b178e9133b611ed5efea8ca140046039a2c333cf4744c107a7814826c": {},
	"cbb93635bdf9e4f8a9d94557f6baa752b9a78f51d7ea98b70ada769a2e56bdc9": {},
}

// SecureRuntimeSecrets 轮换空值或已泄露的示例密钥，再应用仅驻留进程内的环境变量覆盖。
func SecureRuntimeSecrets(cfg *Bootstrap) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	changed := false
	rotate := func(value *string, bytes int) error {
		if strings.TrimSpace(*value) != "" && !isRevokedSecret(*value) {
			return nil
		}
		secret, err := randomSecret(bytes)
		if err != nil {
			return err
		}
		*value = secret
		changed = true
		return nil
	}

	if strings.TrimSpace(cfg.Server.Username) == "" {
		cfg.Server.Username = "admin"
		changed = true
	}
	for _, item := range []struct {
		value *string
		bytes int
	}{
		{&cfg.Server.Password, 24},
		{&cfg.Server.HTTP.JwtSecret, 32},
		{&cfg.Server.RTMPSecret, 32},
		{&cfg.Server.Webhook.RecvSecret, 32},
		{&cfg.Media.Secret, 32},
	} {
		if err := rotate(item.value, item.bytes); err != nil {
			return fmt.Errorf("generate runtime secret: %w", err)
		}
	}
	if strings.TrimSpace(cfg.Sip.Password) != "" && isRevokedSecret(cfg.Sip.Password) {
		if err := rotate(&cfg.Sip.Password, 24); err != nil {
			return fmt.Errorf("generate SIP password: %w", err)
		}
	}
	if changed && cfg.ConfigPath != "" {
		if err := WriteConfig(cfg, cfg.ConfigPath); err != nil {
			return fmt.Errorf("persist rotated secrets: %w", err)
		}
	}

	// 环境变量仅覆盖当前进程，避免将编排平台注入的密钥回写到宿主机配置文件。
	applyEnv := func(name string, target *string) {
		if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			*target = value
		}
	}
	applyEnv(envAdminUsername, &cfg.Server.Username)
	applyEnv(envAdminPassword, &cfg.Server.Password)
	applyEnv(envJWTSecret, &cfg.Server.HTTP.JwtSecret)
	applyEnv(envRTMPSecret, &cfg.Server.RTMPSecret)
	applyEnv(envSIPPassword, &cfg.Sip.Password)
	applyEnv(envMediaSecret, &cfg.Media.Secret)
	applyEnv(envWebhookSecret, &cfg.Server.Webhook.RecvSecret)
	applyEnv(envMediaIP, &cfg.Media.IP)
	applyEnv(envMediaWebhook, &cfg.Media.WebHookIP)
	applyEnv(envMediaSDPIP, &cfg.Media.SDPIP)
	if value, ok := os.LookupEnv(envMediaHTTPPort); ok && strings.TrimSpace(value) != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("%s must be a valid TCP port", envMediaHTTPPort)
		}
		cfg.Media.HTTPPort = port
	}
	for name, value := range map[string]string{
		envAdminPassword: cfg.Server.Password,
		envJWTSecret:     cfg.Server.HTTP.JwtSecret,
		envRTMPSecret:    cfg.Server.RTMPSecret,
		envMediaSecret:   cfg.Media.Secret,
		envWebhookSecret: cfg.Server.Webhook.RecvSecret,
		envSIPPassword:   cfg.Sip.Password,
	} {
		if isRevokedSecret(value) {
			return fmt.Errorf("%s contains a revoked secret", name)
		}
	}

	return nil
}

func randomSecret(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func isRevokedSecret(value string) bool {
	if value == "" {
		return false
	}
	sum := sha256.Sum256([]byte(value))
	_, ok := revokedSecretHashes[hex.EncodeToString(sum[:])]
	return ok
}

// IsRevokedSecret 报告值是否命中已公开的历史示例密钥。
func IsRevokedSecret(value string) bool {
	return isRevokedSecret(value)
}
