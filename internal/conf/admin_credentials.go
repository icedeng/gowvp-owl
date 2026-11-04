package conf

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// AdminCredentialsFile 保存网页修改后的凭据，优先于首次部署的环境变量。
const AdminCredentialsFile = "admin-credentials.toml"

type adminCredentials struct {
	Username string
	Password string
}

func validateAdminCredentials(username, password string) error {
	if strings.TrimSpace(username) == "" || len(password) < 8 {
		return fmt.Errorf("账号不能为空，新密码至少需要 8 个字符")
	}
	if IsRevokedSecret(password) {
		return fmt.Errorf("不能使用已公开的示例密码")
	}
	return nil
}

// SaveAdminCredentials 原子保存成功后才由调用方更新内存，不重写主配置及其他密钥。
func SaveAdminCredentials(cfg *Bootstrap, username, password string) error {
	if cfg == nil || cfg.ConfigPath == "" {
		return fmt.Errorf("未配置凭据持久化目录")
	}
	if err := validateAdminCredentials(username, password); err != nil {
		return err
	}
	data, err := toml.Marshal(adminCredentials{Username: username, Password: password})
	if err != nil {
		return err
	}
	dir := filepath.Dir(cfg.ConfigPath)
	f, err := os.CreateTemp(dir, ".admin-credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, AdminCredentialsFile))
}

func loadAdminCredentials(cfg *Bootstrap) error {
	if cfg.ConfigPath == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.ConfigPath), AdminCredentialsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var credentials adminCredentials
	if err := toml.Unmarshal(data, &credentials); err != nil {
		return err
	}
	if err := validateAdminCredentials(credentials.Username, credentials.Password); err != nil {
		return err
	}
	cfg.Server.Username = credentials.Username
	cfg.Server.Password = credentials.Password
	return nil
}
