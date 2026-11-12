package conf

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/pbkdf2"
)

const passwordHashPrefix = "pbkdf2-sha256$600000$"

// HashAdminPassword 使用随机盐和固定成本，兼容旧口令长度，不截断口令。
func HashAdminPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := pbkdf2.Key([]byte(password), salt, 600000, 32, sha256.New)
	return passwordHashPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func decodePasswordHash(encoded string) ([]byte, []byte, error) {
	if !strings.HasPrefix(encoded, passwordHashPrefix) {
		return nil, nil, fmt.Errorf("不支持的管理员口令哈希格式")
	}
	parts := strings.Split(strings.TrimPrefix(encoded, passwordHashPrefix), "$")
	if len(parts) != 2 {
		return nil, nil, fmt.Errorf("管理员口令哈希格式错误")
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[0])
	hash, e2 := base64.RawStdEncoding.DecodeString(parts[1])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(hash) != 32 {
		return nil, nil, fmt.Errorf("管理员口令哈希格式错误")
	}
	return salt, hash, nil
}

func VerifyAdminPassword(encoded, password string) bool {
	salt, hash, err := decodePasswordHash(encoded)
	if err != nil {
		return false
	}
	actual := pbkdf2.Key([]byte(password), salt, 600000, 32, sha256.New)
	return subtle.ConstantTimeCompare(actual, hash) == 1
}

func adminSessionVersion(secret, username, credential string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("owl-admin-session\x00" + username + "\x00" + credential))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
