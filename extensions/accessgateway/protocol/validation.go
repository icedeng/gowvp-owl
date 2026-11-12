package protocol

import (
	"fmt"
	"net/netip"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

var ssrcPattern = regexp.MustCompile(`^[0-9]{10}$`)
var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidHash(s string) bool { return hashPattern.MatchString(s) }
func IsSSRC(s string) bool {
	if !ssrcPattern.MatchString(s) {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}
func oneOf(s string, allowed ...string) bool { return slices.Contains(allowed, s) }
func invalid(what string) error              { return fmt.Errorf("invalid %s", what) }

var nonPublicPrefixes = func() []netip.Prefix {
	raw := []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001::/32", "2001:2::/48", "2001:db8::/32", "2002::/16"}
	out := make([]netip.Prefix, len(raw))
	for i, s := range raw {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}()

func ValidPublicIP(value string) bool {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" || ip != ip.Unmap() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func ValidateConfig(c EffectiveConfig) error {
	ip, err := netip.ParseAddr(c.PublicIP)
	if err != nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Zone() != "" || ip.String() != c.PublicIP {
		return invalid("public_ip")
	}
	if c.SIPPort < 1 || c.SIPPort > 65535 || c.MediaPortStart < 1 || c.MediaPortEnd > 65535 || c.MediaPortStart >= c.MediaPortEnd || c.SIPPort >= c.MediaPortStart && c.SIPPort <= c.MediaPortEnd {
		return invalid("port range")
	}
	if c.MaxMediaSessions < 1 || c.MaxMediaSessions > 10000 || c.MaxTransferSessions < 0 || c.MaxTransferSessions > 1000 {
		return invalid("session quota")
	}
	for _, d := range []Decimal{c.MaxToABPS, c.MaxToDeviceBPS} {
		n, e := d.Int64()
		if e != nil || n == 0 {
			return invalid("bandwidth quota")
		}
	}
	if c.HeartbeatIntervalMS < 5000 || c.HeartbeatIntervalMS > 60000 || c.HeartbeatTimeoutMS < 3*c.HeartbeatIntervalMS || c.LeaseTTLMS < c.HeartbeatTimeoutMS+c.HeartbeatIntervalMS || c.LeaseTTLMS > 300000 {
		return invalid("lease/heartbeat timeouts")
	}
	if c.MediaConnectTimeoutMS < 1000 || c.MediaConnectTimeoutMS > 60000 || c.MediaFirstPacketTimeoutMS < 1000 || c.MediaFirstPacketTimeoutMS > 60000 {
		return invalid("media timeouts")
	}
	if len(c.SourceCIDRs) > 128 {
		return invalid("source_cidrs")
	}
	seen := map[string]bool{}
	for _, s := range c.SourceCIDRs {
		p, e := netip.ParsePrefix(s)
		if e != nil || p.Addr().Zone() != "" || p.Masked().String() != s || seen[s] {
			return invalid("source_cidrs")
		}
		seen[s] = true
	}
	if c.TaskPublicBaseURL != nil {
		u, e := url.Parse(*c.TaskPublicBaseURL)
		if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
			return invalid("task_public_base_url")
		}
	}
	return nil
}
func Validate(value any) error {
	if err := validateFields(reflect.ValueOf(value), ""); err != nil {
		return err
	}
	switch v := value.(type) {
	case *AlarmPull:
		if v.After != "" && !ValidHash(v.After) {
			return invalid("alarm cursor")
		}
	case *AlarmAck:
		if len(v.IDs) > 64 {
			return invalid("alarm ack batch")
		}
		for _, id := range v.IDs {
			if !ValidHash(id) {
				return invalid("alarm id")
			}
		}
	case *AlarmBatch:
		if v.Next != "" && !ValidHash(v.Next) {
			return invalid("alarm cursor")
		}
		if len(v.Items) > 64 {
			return invalid("alarm batch")
		}
		for _, a := range v.Items {
			if err := ValidateAlarm(a); err != nil {
				return err
			}
		}
	case *ResumeList:
		if v.After != "" && !ValidID(v.After) {
			return invalid("resume cursor")
		}
	case *ResumeFlows:
		if v.Next != "" && !ValidID(v.Next) {
			return invalid("resume cursor")
		}
		for _, f := range v.Items {
			if n, e := f.BindingRevision.Int64(); e != nil || n < 1 {
				return invalid("resume binding")
			}
			expires, e1 := time.Parse(time.RFC3339Nano, f.ExpiresAt)
			seen, e2 := time.Parse(time.RFC3339Nano, f.LastSeenAt)
			if e1 != nil || e2 != nil || !expires.After(seen) {
				return invalid("resume times")
			}
		}
		if len(v.Items) > 64 {
			return invalid("resume batch")
		}
	case *Hello:
		if len(v.SupportedVersions) == 0 || len(v.SupportedVersions) > 16 {
			return invalid("supported_versions")
		}
		return validateCapabilities(v.RequiredCapabilities)
	case *HelloAck:
		if v.SelectedVersion != Version || v.CooldownRemainingMS < 0 {
			return invalid("hello.ack")
		}
		if err := validateCapabilities(v.Capabilities); err != nil {
			return err
		}
		return ValidateConfig(v.Config)
	case *Heartbeat:
		if len(v.RenewSessionIDs) > 500 || len(v.RenewTransferIDs) > 500 || v.PartCount < 0 || v.PartCount > 64 || v.PartIndex < 0 || v.PartCount > 0 && v.PartIndex >= v.PartCount || v.PartCount == 0 && v.PartIndex != 0 {
			return invalid("heartbeat partition")
		}
		if err := uniqueIDs(v.RenewSessionIDs); err != nil {
			return err
		}
		return uniqueIDs(v.RenewTransferIDs)
	case *SIPPacket:
		if err := validateRoute(v.Route); err != nil {
			return err
		}
		_, err := DecodeSIP(*v)
		return err
	case *FlowOpen:
		if v.ConfiguredPlatform && (v.SubjectType != "platform" || !ValidPublicIP(v.Target.IP)) {
			return invalid("configured platform public target")
		}
		if !oneOf(v.SubjectType, "device", "platform") || !oneOf(v.Transport, "udp", "tcp") || v.SubjectType == "device" && v.Transport != "tcp" {
			return invalid("flow target")
		}
		return v.Target.Validate()
	case *FlowOpenResult:
		return validateRoute(v.Route)
	case *BindingUpdate:
		if !oneOf(v.Action, "bind", "refresh", "unbind") || !oneOf(v.Binding.SubjectType, "device", "platform") || v.Binding.ExpiresInMS < 0 || v.Binding.ExpiresInMS > 86400000 {
			return invalid("binding")
		}
	case *MediaOpen:
		return ValidateMedia(v.Spec)
	case *MediaConnect:
		return v.Target.Validate()
	case *MediaOpenResult:
		if !ValidID(v.Lease.SessionID) || !oneOf(v.Lease.State, "allocated", "attached", "receiving", "closing", "closed") || v.Lease.LeaseTTLMS < 1 || v.Lease.AttachTTLMS < 1 {
			return invalid("lease")
		}
		if v.Lease.PublicRTP != nil {
			if err := v.Lease.PublicRTP.Validate(); err != nil {
				return err
			}
		}
		if v.Lease.PublicRTCP != nil {
			return v.Lease.PublicRTCP.Validate()
		}
	case *ResourcesList:
		if !oneOf(v.ResourceType, "all", "media", "transfer", "flow") || v.Size < 1 || v.Size > 200 {
			return invalid("resources.list")
		}
	case *ResourceClose:
		return validateRef(v.Ref)
	case *AttachmentIssue:
		if !oneOf(v.ResourceType, "media", "transfer") {
			return invalid("attachment resource")
		}
	case *ConfigPrepare:
		if !ValidHash(v.ConfigSHA256) {
			return invalid("config_sha256")
		}
		if err := ValidateConfig(v.Config); err != nil {
			return err
		}
		expected, _ := v.ExpectedRevision.Int64()
		desired, _ := v.DesiredRevision.Int64()
		if desired <= expected {
			return invalid("desired_revision")
		}
		hash, err := ConfigHash(v.Config)
		if err != nil || hash != v.ConfigSHA256 {
			return invalid("config hash mismatch")
		}
	case *ConfigApply:
		if !ValidHash(v.ConfigSHA256) {
			return invalid("config_sha256")
		}
	case *Drain:
		if v.DeadlineMS < 0 || v.DeadlineMS > 3600000 {
			return invalid("deadline_ms")
		}
	case *TransferOpen:
		return ValidateTransfer(v.Spec)
	case *TransferConnect:
		return v.Target.Validate()
	case *TransferFinish:
		if !ValidHash(v.SHA256) {
			return invalid("sha256")
		}
	case *TransferProgress:
		if !oneOf(v.Phase, "receiving", "forwarding", "waiting_commit") {
			return invalid("phase")
		}
	case *StatusSnapshot:
		if _, err := time.Parse(time.RFC3339Nano, v.SampledAt); err != nil {
			return invalid("sampled_at")
		}
	case *MetricsSnapshot:
		if _, err := time.Parse(time.RFC3339Nano, v.SampledAt); err != nil {
			return invalid("sampled_at")
		}
	}
	return nil
}
func validateRoute(v Route) error {
	if !ValidID(v.FlowID) || !oneOf(v.Transport, "udp", "tcp") {
		return invalid("route")
	}
	if err := v.Peer.Validate(); err != nil {
		return err
	}
	return v.Listener.Validate()
}
func validateRef(v ResourceRef) error {
	if !oneOf(v.ResourceType, "media", "transfer", "flow") || !ValidID(v.ID) || !ValidID(v.Generation) {
		return invalid("resource ref")
	}
	n, e := v.ConnectionEpoch.Int64()
	if e != nil || n < 1 {
		return invalid("resource epoch")
	}
	return nil
}
func uniqueIDs(ids []string) error {
	seen := map[string]bool{}
	for _, s := range ids {
		if !ValidID(s) || seen[s] {
			return invalid("resource IDs")
		}
		seen[s] = true
	}
	return nil
}
func validateCapabilities(items []string) error {
	seen := map[string]bool{}
	for _, s := range items {
		if !ValidCapability(s) || seen[s] {
			return invalid("capability")
		}
		seen[s] = true
	}
	return nil
}
func ValidateMedia(s MediaSpec) error {
	if !oneOf(s.SubjectType, "device", "platform") || s.SubjectID == "" || !ValidID(s.FlowID) || !oneOf(s.Purpose, "live", "playback", "download", "talk", "broadcast", "cascade") || !oneOf(s.Direction, "device_to_a", "a_to_device", "duplex") || !oneOf(s.WireMode, "udp", "tcp_passive", "tcp_active") || !oneOf(s.RTCP, "disabled", "udp_separate", "tcp_mux") {
		return invalid("media spec")
	}
	if s.WireMode == "udp" && s.RTCP == "tcp_mux" || s.WireMode != "udp" && s.RTCP == "udp_separate" {
		return invalid("rtcp mode")
	}
	if s.Direction != "a_to_device" && (s.SSRC == nil || !IsSSRC(*s.SSRC)) || s.Direction == "a_to_device" && s.SSRC != nil || s.Direction != "device_to_a" && (s.SendSSRC == nil || !IsSSRC(*s.SendSSRC)) || s.Direction == "device_to_a" && s.SendSSRC != nil {
		return invalid("media SSRC")
	}
	if _, err := s.ConfigRevision.Int64(); err != nil {
		return err
	}
	return validateIPs(s.SourceIPs)
}
func validateIPs(items []string) error {
	if len(items) > 128 {
		return invalid("source_ips")
	}
	seen := map[string]bool{}
	for _, s := range items {
		ip, e := netip.ParseAddr(s)
		if e != nil || ip.Zone() != "" || ip.IsMulticast() || ip.IsUnspecified() || ip.String() != s || seen[s] {
			return invalid("source_ips")
		}
		seen[s] = true
	}
	return nil
}
func ValidateTransfer(s TransferSpec) error {
	if s.SubjectType != "" && s.SubjectType != "device" && s.SubjectType != "platform" {
		return invalid("transfer subject type")
	}

	if !ValidID(s.TaskID) || s.SubjectID == "" || !oneOf(s.Kind, "snapshot_upload", "upgrade_download", "raw_download") || !oneOf(s.Direction, "device_to_a", "a_to_device") || s.TTLMS < 1000 || s.TTLMS > 86400000 || len(s.ContentType) == 0 || len(s.ContentType) > 256 || strings.ContainsAny(s.ContentType, "\r\n") {
		return invalid("transfer spec")
	}
	size, err := s.MaxBytes.Int64()
	if err != nil || size == 0 {
		return invalid("max_bytes")
	}
	if s.ExpectedSHA256 != nil && !ValidHash(*s.ExpectedSHA256) {
		return invalid("expected_sha256")
	}
	if s.Kind == "raw_download" {
		if !oneOf(s.WireMode, "tcp_active", "tcp_passive") {
			return invalid("raw download")
		}
		return validateIPs(s.SourceIPs)
	}
	if s.WireMode != "" || len(s.SourceIPs) != 0 {
		return invalid("unexpected raw transfer fields")
	}
	if s.Kind == "snapshot_upload" && s.Direction != "device_to_a" || s.Kind == "upgrade_download" && s.Direction != "a_to_device" {
		return invalid("transfer direction")
	}
	if s.Kind == "upgrade_download" {
		if s.ContentLength == nil || s.SHA256 == nil || s.RangeOffset == nil || s.RangeLength == nil || !ValidHash(*s.SHA256) {
			return invalid("download metadata")
		}
		length, e1 := s.ContentLength.Int64()
		offset, e2 := s.RangeOffset.Int64()
		count, e3 := s.RangeLength.Int64()
		if e1 != nil || e2 != nil || e3 != nil || length <= 0 || offset > length || count <= 0 || count > length-offset || count > size {
			return invalid("download range")
		}
	} else if s.ContentLength != nil || s.SHA256 != nil || s.RangeOffset != nil || s.RangeLength != nil {
		return invalid("unexpected download metadata")
	}
	return nil
}

// 递归核对通用 ID/版本/端点字段，业务枚举再由具体类型验证。
func validateFields(v reflect.Value, name string) error {
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return validateFields(v.Elem(), name)
	}
	if v.Type() == reflect.TypeOf(Decimal("")) {
		_, e := v.Interface().(Decimal).Int64()
		return e
	}
	switch v.Kind() {
	case reflect.Int, reflect.Int64:
		if v.Int() < 0 {
			return invalid(name)
		}
	case reflect.Struct:
		if ref, ok := v.Interface().(ResourceRef); ok {
			return validateRef(ref)
		}
		if route, ok := v.Interface().(Route); ok {
			return validateRoute(route)
		}
		if e, ok := v.Interface().(Endpoint); ok {
			return e.Validate()
		}
		for i := 0; i < v.NumField(); i++ {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			if err := validateFields(v.Field(i), v.Type().Field(i).Name); err != nil {
				return err
			}
		}
	case reflect.String:
		s := v.String()
		if oneOf(name, "OperationID", "ClientInstanceID", "SessionID", "TransferID", "FlowID", "TaskID", "PrepareID", "ResourceID") && !ValidID(s) {
			return invalid(name)
		}
		if oneOf(name, "SubjectID") && (len(s) == 0 || len(s) > 128) {
			return invalid(name)
		}
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := validateFields(v.Index(i), name); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateAlarm(a AlarmRecord) error {
	if !ValidHash(a.ID) || a.SubjectID == "" || len(a.SubjectID) > 64 || len(a.DeviceCode) != 20 || len(a.ChannelCode) > 20 || len(a.Description) > 4096 || len(a.AlarmTime) > 64 || len(a.Priority) > 32 || len(a.Method) > 32 || len(a.Type) > 128 || len(a.Longitude) > 64 || len(a.Latitude) > 64 {
		return invalid("alarm record")
	}
	for _, c := range a.DeviceCode + a.ChannelCode {
		if c < '0' || c > '9' {
			return invalid("alarm code")
		}
	}
	if n, e := a.BindingRevision.Int64(); e != nil || n < 1 {
		return invalid("alarm binding")
	}
	if _, e := time.Parse(time.RFC3339Nano, a.ReceivedAt); e != nil {
		return invalid("alarm receipt time")
	}
	return nil
}
