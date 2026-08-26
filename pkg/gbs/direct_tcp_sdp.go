package gbs

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	sdp "github.com/panjjo/gosdp"
)

type directTCPDownloadOffer struct {
	Address       string
	IP            net.IP
	Port          int
	FileSize      int64
	FileSizeKnown bool
	SSRC          string
}

// parseDirectTCPDownloadSDP 解析附录 O 中媒体发送方 200 OK 的 TCP 服务地址和文件大小。
func parseDirectTCPDownloadSDP(body []byte) (directTCPDownloadOffer, error) {
	message, err := sdp.Decode(body)
	if err != nil {
		return directTCPDownloadOffer{}, fmt.Errorf("decode direct TCP download SDP: %w", err)
	}
	for i := range message.Medias {
		media := &message.Medias[i]
		if !strings.EqualFold(media.Description.Type, "video") || !strings.EqualFold(media.Description.Protocol, "tcp") {
			continue
		}
		if media.Description.Port <= 0 || media.Description.Port > 65535 {
			return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP port: %d", media.Description.Port)
		}
		ip := media.Connection.IP
		networkType, addressType := media.Connection.NetworkType, media.Connection.AddressType
		if ip == nil {
			ip = message.Connection.IP
			networkType, addressType = message.Connection.NetworkType, message.Connection.AddressType
		}
		if err := validateSDPConnectionAddress(networkType, addressType, ip); err != nil {
			return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP: SDP does not contain a valid IP connection address")
		}
		if ipv4 := ip.To4(); ipv4 != nil {
			ip = ipv4
		} else {
			ip = ip.To16()
		}
		value := strings.TrimSpace(media.Attribute("filesize"))
		if value == "" {
			// 兼容部分设备沿用标准示例 ACK 中的 fileszie 拼写错误。
			value = strings.TrimSpace(media.Attribute("fileszie"))
		}
		if value == "" {
			value = strings.TrimSpace(message.Attribute("filesize"))
		}
		if value == "" {
			value = directTCPAttributeValue(body, "filesize", "fileszie")
		}
		ssrc := strings.TrimSpace(message.SSRC)
		if ssrc == "" {
			ssrc = directTCPSDPLineValue(body, "y")
		}
		offer := directTCPDownloadOffer{
			IP:      ip,
			Port:    media.Description.Port,
			SSRC:    ssrc,
			Address: net.JoinHostPort(ip.String(), strconv.Itoa(media.Description.Port)),
		}
		if value != "" {
			size, err := strconv.ParseInt(value, 10, 64)
			if err != nil || size < 0 {
				return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP filesize: %q", value)
			}
			offer.FileSize = size
			offer.FileSizeKnown = true
		}
		return offer, nil
	}
	return directTCPDownloadOffer{}, fmt.Errorf("invalid direct TCP download SDP: SDP does not contain a video tcp media description")
}

func directTCPAttributeValue(body []byte, names ...string) string {
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 3 || !strings.EqualFold(line[:2], "a=") {
			continue
		}
		attribute := line[2:]
		for _, name := range names {
			prefix := name + ":"
			if len(attribute) >= len(prefix) && strings.EqualFold(attribute[:len(prefix)], prefix) {
				return strings.TrimSpace(attribute[len(prefix):])
			}
		}
	}
	return ""
}

func directTCPSDPLineValue(body []byte, field string) string {
	prefix := field + "="
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= len(prefix) && strings.EqualFold(line[:len(prefix)], prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	return ""
}
