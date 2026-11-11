package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/pelletier/go-toml/v2"
)

type Platform struct {
	ID                  string `toml:"id"`
	ClientCertificateCN string `toml:"client_certificate_cn"`
	MaxMediaSessions    int    `toml:"max_media_sessions"`
	MaxTransferSessions int    `toml:"max_transfer_sessions"`
	MaxToABPS           int64  `toml:"max_to_a_bps"`
	MaxToDeviceBPS      int64  `toml:"max_to_device_bps"`
}
type Config struct {
	GatewayID           string   `toml:"gateway_id"`
	Listen              string   `toml:"listen"`
	PublicIP            string   `toml:"public_ip"`
	AllowedPublicIPs    []string `toml:"allowed_public_ips"`
	SIPListen           string   `toml:"sip_listen"`
	MediaBindIP         string   `toml:"media_bind_ip"`
	MediaPortRange      string   `toml:"media_port_range"`
	TLSCertFile         string   `toml:"tls_cert_file"`
	TLSKeyFile          string   `toml:"tls_key_file"`
	ClientCAFile        string   `toml:"client_ca_file"`
	StateDir            string   `toml:"state_dir"`
	TaskListen          string   `toml:"task_listen"`
	TaskPublicBaseURL   *string  `toml:"task_public_base_url"`
	Platform            Platform `toml:"platform"`
	MaxSIPFlows         int      `toml:"max_sip_flows"`
	SIPPacketsPerSecond int      `toml:"sip_packets_per_second"`
	MaxTransferRecords  int      `toml:"max_transfer_records"`
}

func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if len(b) > 1<<20 {
		return c, fmt.Errorf("configuration exceeds 1 MiB")
	}
	d := toml.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	c.Defaults()
	return c, c.Validate()
}
func (c *Config) Defaults() {
	if c.Listen == "" {
		c.Listen = ":443"
	}
	if c.SIPListen == "" {
		c.SIPListen = ":15060"
	}
	if c.MediaBindIP == "" {
		c.MediaBindIP = "0.0.0.0"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/owl-gateway"
	}
	if c.Platform.MaxMediaSessions == 0 {
		c.Platform.MaxMediaSessions = 64
	}
	if c.Platform.MaxToABPS == 0 {
		c.Platform.MaxToABPS = 100000000
	}
	if c.Platform.MaxToDeviceBPS == 0 {
		c.Platform.MaxToDeviceBPS = 100000000
	}
	if len(c.AllowedPublicIPs) == 0 {
		c.AllowedPublicIPs = []string{c.PublicIP}
	}
	if c.MaxSIPFlows == 0 {
		c.MaxSIPFlows = 4096
	}
	if c.SIPPacketsPerSecond == 0 {
		c.SIPPacketsPerSecond = 100
	}
	if c.MaxTransferRecords == 0 {
		c.MaxTransferRecords = 10000
	}
}
func (c Config) Range() (int, int, error) {
	a, b, ok := strings.Cut(c.MediaPortRange, "-")
	if !ok {
		return 0, 0, fmt.Errorf("invalid media_port_range")
	}
	start, e1 := strconv.Atoi(a)
	end, e2 := strconv.Atoi(b)
	if e1 != nil || e2 != nil || start < 1 || end > 65535 || start >= end {
		return 0, 0, fmt.Errorf("invalid media port bounds")
	}
	return start, end, nil
}
func port(s string) (int, error) {
	_, p, e := net.SplitHostPort(s)
	if e != nil {
		return 0, e
	}
	v, e := strconv.Atoi(p)
	if e != nil || v < 1 || v > 65535 {
		return 0, fmt.Errorf("invalid listener port")
	}
	return v, nil
}
func (c Config) Initial() (protocol.EffectiveConfig, error) {
	start, end, err := c.Range()
	if err != nil {
		return protocol.EffectiveConfig{}, err
	}
	sip, err := port(c.SIPListen)
	if err != nil {
		return protocol.EffectiveConfig{}, err
	}
	v := protocol.DefaultEffectiveConfig()
	v.PublicIP = c.PublicIP
	v.SIPPort = sip
	v.MediaPortStart = start
	v.MediaPortEnd = end
	v.MaxMediaSessions = c.Platform.MaxMediaSessions
	v.MaxTransferSessions = c.Platform.MaxTransferSessions
	v.MaxToABPS = protocol.Number(c.Platform.MaxToABPS)
	v.MaxToDeviceBPS = protocol.Number(c.Platform.MaxToDeviceBPS)
	v.TaskPublicBaseURL = c.TaskPublicBaseURL
	return v, protocol.ValidateConfig(v)
}
func (c Config) Validate() error {
	if !protocol.ValidGatewayID(c.GatewayID) || len(c.Platform.ID) < 1 || len(c.Platform.ID) > 64 || c.Platform.ClientCertificateCN == "" {
		return fmt.Errorf("gateway/platform identity required")
	}
	if c.TLSCertFile == "" || c.TLSKeyFile == "" || c.ClientCAFile == "" || c.StateDir == "" {
		return fmt.Errorf("TLS and state paths required")
	}
	if ip := net.ParseIP(c.MediaBindIP); ip == nil || ip.IsMulticast() {
		return fmt.Errorf("invalid media_bind_ip")
	}
	if c.MaxSIPFlows < 1 || c.MaxSIPFlows > 100000 || c.SIPPacketsPerSecond < 1 || c.SIPPacketsPerSecond > 100000 || c.MaxTransferRecords < 1 || c.MaxTransferRecords > 100000 {
		return fmt.Errorf("invalid resource limits")
	}
	if (c.TaskListen == "") != (c.TaskPublicBaseURL == nil) {
		return fmt.Errorf("task_listen and task_public_base_url must be configured together")
	}
	initial, err := c.Initial()
	if err != nil {
		return err
	}
	return c.Check(initial)
}
func (c Config) Check(v protocol.EffectiveConfig) error {
	fail := func() error {
		return protocol.Failure(protocol.ConfigBoundary, "desired configuration exceeds local gateway boundaries")
	}
	if e := protocol.ValidateConfig(v); e != nil {
		return e
	}
	initial, e := c.Initial()
	if e != nil {
		return e
	}
	allowed := false
	for _, ip := range c.AllowedPublicIPs {
		if ip == v.PublicIP {
			allowed = true
		}
	}
	a, _ := v.MaxToABPS.Int64()
	b, _ := v.MaxToDeviceBPS.Int64()
	if !allowed || v.SIPPort != initial.SIPPort || v.MediaPortStart < initial.MediaPortStart || v.MediaPortEnd > initial.MediaPortEnd || v.MaxMediaSessions > initial.MaxMediaSessions || v.MaxTransferSessions > initial.MaxTransferSessions || a > c.Platform.MaxToABPS || b > c.Platform.MaxToDeviceBPS {
		return fail()
	}
	if (v.TaskPublicBaseURL == nil) != (c.TaskPublicBaseURL == nil) || v.TaskPublicBaseURL != nil && *v.TaskPublicBaseURL != *c.TaskPublicBaseURL {
		return fail()
	}
	for _, listener := range []string{c.Listen, c.TaskListen} {
		if listener == "" {
			continue
		}
		p, e := port(listener)
		if e != nil {
			return e
		}
		if p == v.SIPPort || p >= v.MediaPortStart && p <= v.MediaPortEnd {
			return fail()
		}
	}
	return nil
}
