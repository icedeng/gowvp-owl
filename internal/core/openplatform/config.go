package openplatform

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

type Config struct {
	Enabled           bool
	AllowHTTP         bool
	Audience          string
	PublicBaseURL     string
	KeyID             string
	Keys              map[string][]byte
	DataDir           string
	TrustedProxies    []*net.IPNet
	CallbackDenyCIDRs []*net.IPNet
}

func ConfigFromEnv(dataDir string) (Config, error) {
	c := Config{Enabled: os.Getenv("OWL_OPEN_PLATFORM_ENABLED") == "true", Audience: os.Getenv("OWL_OPEN_PLATFORM_AUDIENCE"), PublicBaseURL: strings.TrimRight(os.Getenv("OWL_OPEN_PLATFORM_PUBLIC_URL"), "/"), KeyID: os.Getenv("OWL_OPEN_PLATFORM_KEY_ID"), DataDir: dataDir}
	c.AllowHTTP = os.Getenv("OWL_OPEN_PLATFORM_ALLOW_HTTP") == "true"
	if !c.Enabled {
		return c, nil
	}
	u, err := url.Parse(c.PublicBaseURL)
	if err != nil || !c.AllowsScheme(u.Scheme) || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil || u.Fragment != "" {
		return c, fmt.Errorf("开放平台 Public URL 必须为无路径的 HTTPS origin；HTTP须显式启用ALLOW_HTTP")
	}
	if len(c.Audience) < 3 || len(c.Audience) > 128 || strings.ContainsAny(c.Audience, "\r\n") {
		return c, fmt.Errorf("开放平台 Audience 未配置或格式错误")
	}
	raw := []byte(os.Getenv("OWL_OPEN_PLATFORM_KEYS"))
	if f := os.Getenv("OWL_OPEN_PLATFORM_KEYS_FILE"); f != "" {
		raw, err = os.ReadFile(f)
		if err != nil {
			return c, fmt.Errorf("读取开放平台密钥环失败")
		}
	}
	var keys map[string]string
	if err = json.Unmarshal(raw, &keys); err != nil {
		return c, fmt.Errorf("开放平台密钥环必须为 key-id 到 Base64URL 密钥的 JSON 对象")
	}
	c.Keys = map[string][]byte{}
	for id, v := range keys {
		b, e := base64.RawURLEncoding.DecodeString(v)
		if e != nil || len(b) != 32 || id == "" {
			return c, fmt.Errorf("开放平台加密主密钥必须为32字节")
		}
		c.Keys[id] = b
	}
	if len(c.Keys[c.KeyID]) != 32 {
		return c, fmt.Errorf("缺少开放平台当前版本主密钥")
	}
	for _, v := range strings.Split(os.Getenv("OWL_OPEN_PLATFORM_TRUSTED_PROXIES"), ",") {
		if v == "" {
			continue
		}
		_, n, e := net.ParseCIDR(strings.TrimSpace(v))
		if e != nil {
			return c, fmt.Errorf("可信代理CIDR格式错误")
		}
		c.TrustedProxies = append(c.TrustedProxies, n)
	}
	for _, v := range strings.Split(os.Getenv("OWL_OPEN_PLATFORM_CALLBACK_DENY_CIDRS"), ",") {
		if strings.TrimSpace(v) == "" {
			continue
		}
		_, n, e := net.ParseCIDR(strings.TrimSpace(v))
		if e != nil {
			return c, fmt.Errorf("回调禁止目标CIDR格式错误")
		}
		c.CallbackDenyCIDRs = append(c.CallbackDenyCIDRs, n)
	}
	if addrs, e := net.InterfaceAddrs(); e == nil {
		for _, addr := range addrs {
			ip, _, e := net.ParseCIDR(addr.String())
			if e == nil {
				bits := 128
				if ip.To4() != nil {
					bits = 32
					ip = ip.To4()
				}
				c.CallbackDenyCIDRs = append(c.CallbackDenyCIDRs, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			}
		}
	}
	return c, nil
}

func (c Config) AllowsScheme(scheme string) bool {
	return scheme == "https" || (c.AllowHTTP && scheme == "http")
}
