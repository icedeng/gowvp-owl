package service

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/pelletier/go-toml/v2"
)

type DeploymentTemplate struct {
	Format   string   `json:"format"`
	Content  string   `json:"content"`
	Warnings []string `json:"warnings"`
}

func (s *Service) DeploymentTemplate(ctx context.Context, id, format string) (DeploymentTemplate, error) {
	result := DeploymentTemplate{Format: format, Warnings: []string{"请替换镜像版本、B 的服务端证书和 CA 文件；此模板不包含 A 已保存的私钥。", "公网地址和端口映射须与 B 的本地授权边界一致，TLS 由 B 直接终止。"}}
	row, e := s.repo.Get(ctx, id)
	if e != nil {
		return result, normalizeError(e)
	}
	if format == "" {
		format = "toml"
		result.Format = format
	}
	config := row.DesiredConfig
	controlURL, _ := url.Parse(config.URL)
	controlPort := "443"
	if controlURL != nil && controlURL.Port() != "" {
		controlPort = controlURL.Port()
	}
	cn := config.PlatformID
	if config.CredentialID != nil {
		if credential, e := s.repo.Credential(ctx, row.ID, *config.CredentialID); e == nil {
			if cfg, e := s.files.Load(credential.CAFileRef, credential.CertFileRef, credential.KeyFileRef); e == nil {
				cn = cfg.Certificates[0].Leaf.Subject.CommonName
			}
		}
	}
	if format == "toml" {
		data := map[string]any{"gateway_id": row.StableID, "listen": ":443", "public_ip": config.PublicIP, "allowed_public_ips": []string{config.PublicIP}, "sip_listen": ":" + strconv.Itoa(config.SIPPort), "media_bind_ip": "0.0.0.0", "media_port_range": fmt.Sprintf("%d-%d", config.MediaPortStart, config.MediaPortEnd), "tls_cert_file": "/etc/owl-gateway/tls/server.crt", "tls_key_file": "/etc/owl-gateway/tls/server.key", "client_ca_file": "/etc/owl-gateway/tls/client-ca.crt", "state_dir": "/var/lib/owl-gateway", "platform": map[string]any{"id": config.PlatformID, "client_certificate_cn": cn, "max_media_sessions": config.MaxMediaSessions, "max_transfer_sessions": config.MaxTransferSessions}}
		a, _ := config.MaxToABPS.Int64()
		b, _ := config.MaxToDeviceBPS.Int64()
		data["platform"].(map[string]any)["max_to_a_bps"] = a
		data["platform"].(map[string]any)["max_to_device_bps"] = b
		if config.TaskPublicBaseURL != nil {
			data["task_listen"] = ":8443"
			data["task_public_base_url"] = *config.TaskPublicBaseURL
			result.Warnings = append(result.Warnings, "任务入口示例监听 8443，请核对 task_public_base_url 的实际公网映射。")
		}
		content, e := toml.Marshal(data)
		if e != nil {
			return result, e
		}
		result.Content = string(content)
		return result, nil
	}
	if format == "compose" {
		result.Content = fmt.Sprintf("services:\n  owl-gateway:\n    image: ${OWL_GATEWAY_IMAGE:?请设置固定版本镜像}\n    restart: unless-stopped\n    command: [\"-config\", \"/etc/owl-gateway/gateway.toml\"]\n    ports:\n      - \"443:443/tcp\"\n      - \"%d:%d/udp\"\n      - \"%d:%d/tcp\"\n      - \"%d-%d:%d-%d/udp\"\n      - \"%d-%d:%d-%d/tcp\"\n    volumes:\n      - ./gateway.toml:/etc/owl-gateway/gateway.toml:ro\n      - ./tls:/etc/owl-gateway/tls:ro\n      - ./state:/var/lib/owl-gateway\n    read_only: true\n    tmpfs: [/tmp]\n    security_opt: [no-new-privileges:true]\n    cap_drop: [ALL]\n    cap_add: [NET_BIND_SERVICE]\n", config.SIPPort, config.SIPPort, config.SIPPort, config.SIPPort, config.MediaPortStart, config.MediaPortEnd, config.MediaPortStart, config.MediaPortEnd, config.MediaPortStart, config.MediaPortEnd, config.MediaPortStart, config.MediaPortEnd)
		result.Content = strings.Replace(result.Content, "\"443:443/tcp\"", strconv.Quote(controlPort+":443/tcp"), 1)
		result.Content = strings.Replace(result.Content, "    cap_add: [NET_BIND_SERVICE]\n", "    user: \"10001:10001\"\n    sysctls:\n      net.ipv4.ip_unprivileged_port_start: \"0\"\n", 1)
		if config.TaskPublicBaseURL != nil {
			taskURL, _ := url.Parse(*config.TaskPublicBaseURL)
			taskPort := "443"
			if taskURL != nil && taskURL.Port() != "" {
				taskPort = taskURL.Port()
			}
			if taskPort == controlPort {
				result.Warnings = append(result.Warnings, "设备任务公网端口与 WSS 相同，需独立公网地址/已配置的转发入口指向 B:8443；单机直连部署请为任务选择不同公网端口。")
			} else {
				result.Content = strings.Replace(result.Content, "    volumes:\n", "      - "+strconv.Quote(taskPort+":8443/tcp")+"\n    volumes:\n", 1)
			}
		}
		return result, nil
	}
	return result, core.Fail(400, "INVALID_ARGUMENT", "模板格式只支持 toml/compose")
}
