package gbs

import (
	"fmt"
	"github.com/panjjo/gosdp"
	"net"
	"strconv"
	"strings"
)

// negotiatedUDPRTCP 优先使用 SDP 明确地址；缺省遵循 RTP/AVP 的相邻端口约定。
func negotiatedUDPRTCP(message *sdp.Message, media *sdp.Media, ip string, port int) (string, int, error) {
	values, err := effectiveSDPAttributeValues(message, media, "rtcp")
	if err != nil {
		return "", 0, err
	}
	if len(values) > 1 {
		return "", 0, fmt.Errorf("重复 RTCP 地址")
	}
	if len(values) == 0 {
		if port >= 65535 {
			return "", 0, fmt.Errorf("RTCP 端口超限")
		}
		return ip, port + 1, nil
	}
	parts := strings.Fields(values[0])
	if len(parts) != 1 && len(parts) != 4 {
		return "", 0, fmt.Errorf("非法 RTCP 属性")
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 1 || n > 65535 {
		return "", 0, fmt.Errorf("非法 RTCP 端口")
	}
	if len(parts) == 4 {
		if parts[1] != "IN" || (parts[2] != "IP4" && parts[2] != "IP6") {
			return "", 0, fmt.Errorf("非法 RTCP 网络")
		}
		address := net.ParseIP(parts[3])
		if address == nil || address.IsUnspecified() || address.IsMulticast() {
			return "", 0, fmt.Errorf("非法 RTCP 地址")
		}
		ip = address.String()
	}
	return ip, n, nil
}

func appendRTCPAttribute(body []byte, ip string, port int) []byte {
	if port < 1 {
		return body
	}
	family := "IP4"
	if net.ParseIP(ip).To4() == nil {
		family = "IP6"
	}
	return append(body, []byte(fmt.Sprintf("a=rtcp:%d IN %s %s\r\n", port, family, ip))...)
}
