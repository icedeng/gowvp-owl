package data

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// NormalizeMySQLDSN 统一管理服务与维护工具的MySQL输入；错误不回显连接凭据。
func NormalizeMySQLDSN(raw string) (string, error) {
	if strings.HasPrefix(raw, "mysql://") {
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" || u.User == nil || strings.TrimPrefix(u.Path, "/") == "" {
			return "", fmt.Errorf("MySQL URL格式错误")
		}
		cfg := mysqldriver.NewConfig()
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
		cfg.Net = "tcp"
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		cfg.Addr = net.JoinHostPort(u.Hostname(), port)
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		cfg.ParseTime = true
		q := u.Query()
		if mode := q.Get("sslmode"); mode != "" {
			if mode != "disable" {
				return "", fmt.Errorf("MySQL请使用tls参数配置TLS")
			}
			q.Del("sslmode")
			q.Set("tls", "false")
		}
		result := cfg.FormatDSN()
		if len(q) > 0 {
			result += "&" + q.Encode()
		}
		if _, e = mysqldriver.ParseDSN(result); e != nil {
			return "", fmt.Errorf("MySQL连接参数格式错误")
		}
		return result, nil
	}
	raw = strings.TrimPrefix(raw, "mysql:")
	cfg, e := mysqldriver.ParseDSN(raw)
	if e != nil {
		return "", fmt.Errorf("MySQL DSN格式错误")
	}
	cfg.ParseTime = true
	return cfg.FormatDSN(), nil
}
