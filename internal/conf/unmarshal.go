package conf

import (
	"os"

	"github.com/pelletier/go-toml/v2"
)

// SetupConfig 从文件读取内容初始化配置
func SetupConfig(v any, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	if err := toml.Unmarshal(b, v); err != nil {
		return err
	}
	// 兼容升级前不存在 DeviceHistory 配置段的文件；只有显式配置 0 才表示不限制。
	if bootstrap, ok := v.(*Bootstrap); ok {
		var sections struct {
			SIP struct {
				DeviceHistory *DeviceHistoryConfig
				SignalDigest  *SIPSignalDigest
			}
		}
		if err := toml.Unmarshal(b, &sections); err != nil {
			return err
		}
		if sections.SIP.DeviceHistory == nil {
			bootstrap.Sip.DeviceHistory = DeviceHistoryConfig{MaxRecords: 1000, MaxDays: 30}
		}
		if sections.SIP.SignalDigest == nil {
			bootstrap.Sip.SignalDigest = DefaultConfig().Sip.SignalDigest
		}
		if err := ValidateSignalDigestConfig(bootstrap.Sip.SignalDigest); err != nil {
			return err
		}
	}
	return nil
}

// WriteConfig 将配置写回文件
func WriteConfig(v any, path string) error {
	tmp := path + ".tmp"
	_ = os.RemoveAll(tmp)

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := toml.NewEncoder(f).SetIndentTables(true).Encode(v); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
