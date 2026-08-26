package gbs

import (
	"fmt"
	"net"
	"strings"
)

type sdpAddress struct {
	IP        net.IP
	Type      string
	Canonical string
}

// parseSDPAddress 将字面 IP 规范化为 SDP 的 IP4/IP6 地址。
// SDP 不携带 IPv6 zone，链路本地地址如需 zone 无法被可靠路由，因此显式拒绝。
func parseSDPAddress(value string) (sdpAddress, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	if value == "" || strings.Contains(value, "%") {
		return sdpAddress{}, fmt.Errorf("SDP requires a valid IP address: %s", value)
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return sdpAddress{}, fmt.Errorf("SDP requires a valid IP address: %s", value)
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return sdpAddress{IP: ipv4, Type: "IP4", Canonical: ipv4.String()}, nil
	}
	ip = ip.To16()
	if ip == nil {
		return sdpAddress{}, fmt.Errorf("SDP requires a valid IP address: %s", value)
	}
	return sdpAddress{IP: ip, Type: "IP6", Canonical: ip.String()}, nil
}

func validateSDPConnectionAddress(networkType, addressType string, ip net.IP) error {
	if !strings.EqualFold(strings.TrimSpace(networkType), "IN") || ip == nil || ip.To16() == nil {
		return fmt.Errorf("invalid SDP connection address")
	}
	want := "IP6"
	if ip.To4() != nil {
		want = "IP4"
	}
	if !strings.EqualFold(strings.TrimSpace(addressType), want) {
		return fmt.Errorf("SDP connection address type %s does not match %s", addressType, want)
	}
	return nil
}
