// Package core 定义 A 端独立领域模型与宿主端口，不依赖 Gin/GORM/Owl 类型。
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"golang.org/x/net/idna"
)

type Config struct {
	Enabled             bool
	PlatformID          string
	CredentialDir       string
	MaxMediaSessions    int
	MaxTransferSessions int
	MaxConnections      int
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.PlatformID == "" || len(c.PlatformID) > 64 || c.CredentialDir == "" || c.MaxMediaSessions < 1 || c.MaxTransferSessions < 0 || c.MaxConnections < 1 {
		return fmt.Errorf("网关模块启动配置不完整")
	}
	return nil
}

type DesiredConfig struct {
	protocol.EffectiveConfig
	Name                 string   `json:"name"`
	Group                string   `json:"group"`
	Description          string   `json:"description"`
	URL                  string   `json:"url"`
	PlatformID           string   `json:"platform_id"`
	CredentialID         *string  `json:"credential_id"`
	DefaultMediaServerID *string  `json:"default_media_server_id"`
	RequiredCapabilities []string `json:"required_capabilities"`
	ProxyRef             *string  `json:"proxy_ref"`
}

func (c DesiredConfig) Validate(platform string) error {
	if err := protocol.ValidateConfig(c.EffectiveConfig); err != nil {
		return err
	}
	if utf8.RuneCountInString(c.Name) < 1 || utf8.RuneCountInString(c.Name) > 80 || strings.TrimSpace(c.Name) == "" || utf8.RuneCountInString(c.Group) > 64 || utf8.RuneCountInString(c.Description) > 500 {
		return fmt.Errorf("名称/分组/备注长度不符合要求")
	}
	if c.PlatformID != platform {
		return fmt.Errorf("平台身份必须与当前 Owl 配置一致")
	}
	if c.CredentialID != nil && !protocol.ValidID(*c.CredentialID) {
		return fmt.Errorf("凭据 ID 无效")
	}
	if len(c.URL) > 512 {
		return fmt.Errorf("WSS 地址过长")
	}
	if _, err := NormalizeURL(c.URL); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, capability := range c.RequiredCapabilities {
		if !protocol.ValidCapability(capability) || seen[capability] {
			return fmt.Errorf("能力列表无效或重复")
		}
		seen[capability] = true
	}
	if c.ProxyRef != nil && len(*c.ProxyRef) > 128 {
		return fmt.Errorf("代理引用过长")
	}
	return nil
}
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.Path != protocol.ControlPath || u.RawPath != "" {
		return "", fmt.Errorf("WSS 地址必须为 wss://host[:port]%s 且不含账户、查询或片段", protocol.ControlPath)
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		host, err = idna.Lookup.ToASCII(strings.TrimSuffix(host, "."))
		if err != nil {
			return "", fmt.Errorf("WSS 域名无效")
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("WSS 端口无效")
	}
	u.Host = net.JoinHostPort(host, strconv.Itoa(n))
	return u.String(), nil
}
func EndpointKey(raw string) (string, error) {
	canonical, err := NormalizeURL(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:]), nil
}
