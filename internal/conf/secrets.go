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
	if cfg.Server.PasswordHash != "" {
		if _, _, err := decodePasswordHash(cfg.Server.PasswordHash); err != nil {
			return err
		}
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
	// 主配置中的旧口令只用于迁移；协议凭据另行生成，不能复制管理员口令。
	if cfg.Server.Password != "" {
		if cfg.Server.PasswordHash == "" && !isRevokedSecret(cfg.Server.Password) {
			var err error
			cfg.Server.PasswordHash, err = HashAdminPassword(cfg.Server.Password)
			if err != nil {
				return err
			}
		}
		cfg.Server.Password = ""
		changed = true
	}
	if cfg.Server.ONVIF.Username == "" {
		cfg.Server.ONVIF.Username = "owl-onvif"
		changed = true
	}
	for _, item := range []struct {
		value *string
		bytes int
	}{
		{&cfg.Server.ONVIF.Password, 24},
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
	applyEnv(envJWTSecret, &cfg.Server.HTTP.JwtSecret)
	credential := cfg.Server.PasswordHash
	var environmentPassword, environmentHash string
	if password, ok := os.LookupEnv(envAdminPassword); ok && strings.TrimSpace(password) != "" {
		var err error
		cfg.Server.PasswordHash, err = HashAdminPassword(password)
		if err != nil {
			return err
		}
		credential = password
		environmentPassword, environmentHash = password, cfg.Server.PasswordHash
	}
	cfg.AdminSessionVersion = adminSessionVersion(cfg.Server.HTTP.JwtSecret, cfg.Server.Username, credential)
	applyEnv("OWL_ONVIF_USERNAME", &cfg.Server.ONVIF.Username)
	applyEnv("OWL_ONVIF_PASSWORD", &cfg.Server.ONVIF.Password)
	applyEnv(envRTMPSecret, &cfg.Server.RTMPSecret)
	applyEnv(envSIPPassword, &cfg.Sip.Password)
	applyEnv(envMediaSecret, &cfg.Media.Secret)
	applyEnv(envWebhookSecret, &cfg.Server.Webhook.RecvSecret)
	// 环境变量用于初始化；网页显式保存的管理员凭据在重启后仍然生效。
	if err := loadAdminCredentials(cfg); err != nil {
		return fmt.Errorf("load admin credentials: %w", err)
	}
	// 已保存凭据优先；只有环境口令实际生效时才校验它，避免旧部署变量阻止启动。
	if environmentPassword != "" && cfg.Server.PasswordHash == environmentHash {
		if err := validateAdminCredentials(cfg.Server.Username, environmentPassword); err != nil {
			return err
		}
	}
	if cfg.Server.PasswordHash == "" {
		return fmt.Errorf("管理员口令未配置，请设置 OWL_ADMIN_PASSWORD（至少 8 个字符且不可使用公开示例口令）")
	}
	if _, _, err := decodePasswordHash(cfg.Server.PasswordHash); err != nil {
		return err
	}
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
		"OWL_ONVIF_PASSWORD": cfg.Server.ONVIF.Password,
		envJWTSecret:         cfg.Server.HTTP.JwtSecret,
		envRTMPSecret:        cfg.Server.RTMPSecret,
		envMediaSecret:       cfg.Media.Secret,
		envWebhookSecret:     cfg.Server.Webhook.RecvSecret,
		envSIPPassword:       cfg.Sip.Password,
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
