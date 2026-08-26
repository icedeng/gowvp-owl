package gbs

import (
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
)

// GBProtocolVersion 是附录 I 定义的 GB/T 28181 协议版本号。
type GBProtocolVersion string

const (
	GBVersion10 GBProtocolVersion = "1.0"
	GBVersion11 GBProtocolVersion = "1.1"
	GBVersion20 GBProtocolVersion = "2.0"
	GBVersion30 GBProtocolVersion = "3.0"

	// 保留现有业务代码使用的年份常量，避免破坏已有调用。
	gbVersion2011 = "2011"
	gbVersion2014 = "2014"
	gbVersion2016 = "2016"
	gbVersion2022 = "2022"

	platformMaxGBVersion = GBVersion30
)

// GBCapabilities 描述一个协议档案可以安全使用的能力。
// 业务层应按能力判断，避免把 2014 修改补充文件简单视作 2016。
type GBCapabilities struct {
	ConfigQuery       bool
	ConfigWrite       bool
	CatalogExtension  bool
	DirectoryNotify   bool
	MultiResponse     bool
	MediaStatus       bool
	VoiceBroadcast    bool
	VoiceIntercom     bool
	RTPOverTCP        bool
	DirectTCPDownload bool
	DownloadSpeed     bool
	IFrameControl     bool
	DragZoomControl   bool
	PresetQuery       bool
	MobilePosition    bool
	PTZPosition       bool
	HomePosition      bool
	CruiseTrackQuery  bool
	SDCard            bool
	H265              bool
	AAC               bool
	Snapshot          bool
	Upgrade           bool
	TargetTrack       bool
}

// ParseGBProtocolVersion 同时兼容附录 I 版本号和项目历史年份值。
func ParseGBProtocolVersion(value string) (GBProtocolVersion, bool) {
	switch strings.TrimSpace(value) {
	case string(GBVersion10), gbVersion2011:
		return GBVersion10, true
	case string(GBVersion11), gbVersion2014, "2011-supplement-2014":
		return GBVersion11, true
	case string(GBVersion20), gbVersion2016:
		return GBVersion20, true
	case string(GBVersion30), gbVersion2022:
		return GBVersion30, true
	default:
		return "", false
	}
}

func (v GBProtocolVersion) Valid() bool {
	_, ok := ParseGBProtocolVersion(string(v))
	return ok
}

func (v GBProtocolVersion) StandardYear() string {
	switch v {
	case GBVersion10:
		return gbVersion2011
	case GBVersion11:
		return gbVersion2014
	case GBVersion20:
		return gbVersion2016
	case GBVersion30:
		return gbVersion2022
	default:
		return ""
	}
}

func (v GBProtocolVersion) StandardName() string {
	switch v {
	case GBVersion10:
		return "GB/T 28181-2011"
	case GBVersion11:
		return "GB/T 28181-2011 修改补充文件"
	case GBVersion20:
		return "GB/T 28181-2016"
	case GBVersion30:
		return "GB/T 28181-2022"
	default:
		return "未知版本"
	}
}

func (v GBProtocolVersion) rank() int {
	switch v {
	case GBVersion10:
		return 1
	case GBVersion11:
		return 2
	case GBVersion20:
		return 3
	case GBVersion30:
		return 4
	default:
		return 0
	}
}

func (v GBProtocolVersion) AtLeast(min GBProtocolVersion) bool {
	return v.rank() >= min.rank() && min.rank() > 0
}

func (v GBProtocolVersion) Capabilities() GBCapabilities {
	switch v {
	case GBVersion10:
		return GBCapabilities{
			DirectoryNotify: true,
			MultiResponse:   true,
			MediaStatus:     true,
		}
	case GBVersion11:
		return GBCapabilities{
			ConfigQuery:       true,
			ConfigWrite:       true,
			CatalogExtension:  true,
			DirectoryNotify:   true,
			MultiResponse:     true,
			MediaStatus:       true,
			VoiceBroadcast:    true,
			DirectTCPDownload: true,
			DownloadSpeed:     true,
			DragZoomControl:   true,
			PresetQuery:       true,
		}
	case GBVersion20:
		return GBCapabilities{
			ConfigQuery:      true,
			ConfigWrite:      true,
			CatalogExtension: true,
			DirectoryNotify:  true,
			MultiResponse:    true,
			MediaStatus:      true,
			VoiceBroadcast:   true,
			VoiceIntercom:    true,
			RTPOverTCP:       true,
			DownloadSpeed:    true,
			IFrameControl:    true,
			DragZoomControl:  true,
			PresetQuery:      true,
			MobilePosition:   true,
			HomePosition:     true,
		}
	case GBVersion30:
		return GBCapabilities{
			ConfigQuery:      true,
			ConfigWrite:      true,
			CatalogExtension: true,
			DirectoryNotify:  true,
			MultiResponse:    true,
			MediaStatus:      true,
			VoiceBroadcast:   true,
			VoiceIntercom:    true,
			RTPOverTCP:       true,
			DownloadSpeed:    true,
			IFrameControl:    true,
			DragZoomControl:  true,
			PresetQuery:      true,
			MobilePosition:   true,
			PTZPosition:      true,
			HomePosition:     true,
			CruiseTrackQuery: true,
			SDCard:           true,
			H265:             true,
			AAC:              true,
			Snapshot:         true,
			Upgrade:          true,
			TargetTrack:      true,
		}
	default:
		return GBCapabilities{}
	}
}

func (v GBProtocolVersion) CapabilityNames() []string {
	c := v.Capabilities()
	checks := []struct {
		name string
		on   bool
	}{
		{"config_query", c.ConfigQuery},
		{"config_write", c.ConfigWrite},
		{"catalog_extension", c.CatalogExtension},
		{"directory_notify", c.DirectoryNotify},
		{"multi_response", c.MultiResponse},
		{"media_status", c.MediaStatus},
		{"voice_broadcast", c.VoiceBroadcast},
		{"voice_intercom", c.VoiceIntercom},
		{"rtp_over_tcp", c.RTPOverTCP},
		{"direct_tcp_download", c.DirectTCPDownload},
		{"download_speed", c.DownloadSpeed},
		{"iframe_control", c.IFrameControl},
		{"drag_zoom_control", c.DragZoomControl},
		{"preset_query", c.PresetQuery},
		{"mobile_position", c.MobilePosition},
		{"ptz_position", c.PTZPosition},
		{"home_position", c.HomePosition},
		{"cruise_track_query", c.CruiseTrackQuery},
		{"sdcard", c.SDCard},
		{"h265", c.H265},
		{"aac", c.AAC},
		{"snapshot", c.Snapshot},
		{"upgrade", c.Upgrade},
		{"target_track", c.TargetTrack},
	}
	out := make([]string, 0, len(checks))
	for _, check := range checks {
		if check.on {
			out = append(out, check.name)
		}
	}
	return out
}

var knownGBCapabilityNames = map[string]struct{}{
	"config_query": {}, "config_write": {}, "catalog_extension": {}, "directory_notify": {}, "multi_response": {}, "media_status": {},
	"voice_broadcast": {}, "voice_intercom": {},
	"rtp_over_tcp": {}, "direct_tcp_download": {}, "download_speed": {}, "iframe_control": {}, "drag_zoom_control": {},
	"preset_query": {}, "mobile_position": {}, "ptz_position": {}, "home_position": {}, "cruise_track_query": {}, "sdcard": {}, "h265": {}, "aac": {}, "snapshot": {}, "upgrade": {}, "target_track": {},
}

// NormalizeGBDisabledCapabilities 校验并规范化设备级能力关闭列表。
// 仅接受平台已知的稳定能力名，避免配置拼写错误被静默忽略。
func NormalizeGBDisabledCapabilities(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		name := strings.ToLower(strings.TrimSpace(value))
		if name == "" {
			continue
		}
		if _, ok := knownGBCapabilityNames[name]; !ok {
			return nil, fmt.Errorf("未知的 GB/T 28181 能力: %s", value)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

func effectiveCapabilityNames(version GBProtocolVersion, disabled []string) []string {
	blocked := gbDisabledCapabilitySet(disabled)
	if len(blocked) == 0 {
		return version.CapabilityNames()
	}
	names := version.CapabilityNames()
	out := names[:0]
	for _, name := range names {
		if _, ok := blocked[name]; !ok {
			out = append(out, name)
		}
	}
	return out
}

func gbDisabledCapabilitySet(values []string) map[string]struct{} {
	blocked := make(map[string]struct{}, len(values))
	for _, value := range values {
		name := strings.ToLower(strings.TrimSpace(value))
		if _, ok := knownGBCapabilityNames[name]; ok {
			blocked[name] = struct{}{}
		}
	}
	return blocked
}

func protocolVersionForMinimum(value string) (GBProtocolVersion, bool) {
	return ParseGBProtocolVersion(value)
}

// resolveGBProtocolVersion 按手动覆盖、当前声明、已协商值、历史值的优先级解析有效版本。
func resolveGBProtocolVersion(ext ipc.DeviceExt, declared string) (GBProtocolVersion, string) {
	if version, ok := ParseGBProtocolVersion(ext.GBManualVersion); ok {
		return version, "manual"
	}
	if version, ok := ParseGBProtocolVersion(declared); ok {
		return version, "header"
	}
	if version, ok := ParseGBProtocolVersion(ext.GBEffectiveVersion); ok {
		return version, ext.GBVersionSource
	}
	if version, ok := ParseGBProtocolVersion(ext.GBDeclaredVersion); ok {
		return version, "header"
	}
	if version, ok := ParseGBProtocolVersion(ext.GBVersion); ok {
		return version, "legacy"
	}
	return GBVersion10, "default"
}

// applyGBProtocolVersion 更新兼容字段和新的协商字段，空声明不会清除已有声明。
func applyGBProtocolVersion(ext *ipc.DeviceExt, declared string) GBProtocolVersion {
	previousDeclared := ext.GBDeclaredVersion
	previousEffective := ext.GBEffectiveVersion
	previousSource := ext.GBVersionSource
	if version, ok := ParseGBProtocolVersion(declared); ok {
		ext.GBDeclaredVersion = string(version)
		declared = string(version)
	} else {
		declared = ""
	}

	version, source := resolveGBProtocolVersion(*ext, declared)
	ext.GBEffectiveVersion = string(version)
	ext.GBVersion = version.StandardYear()
	ext.GBVersionSource = source
	ext.GBVersionCapabilities = effectiveCapabilityNames(version, ext.GBDisabledCapabilities)
	if previousDeclared != ext.GBDeclaredVersion || previousEffective != ext.GBEffectiveVersion || previousSource != ext.GBVersionSource {
		ext.GBVersionUpdatedAt = time.Now().Unix()
	}
	return version
}

// getDeviceGBProtocolVersion 获取设备的有效协议档案；未知设备保守按 1.0 处理。
func (g *GB28181API) getDeviceGBProtocolVersion(deviceID string) GBProtocolVersion {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return GBVersion10
	}
	if d, ok := g.svr.memoryStorer.Load(deviceID); ok {
		if version, valid := ParseGBProtocolVersion(d.GBVersion()); valid {
			return version
		}
	}
	return GBVersion10
}

// getDeviceGBVersion 保留原有年份返回格式，未知时按 2011 处理。
func (g *GB28181API) getDeviceGBVersion(deviceID string) string {
	return g.getDeviceGBProtocolVersion(deviceID).StandardYear()
}

func (g *GB28181API) isDeviceCapabilityDisabled(deviceID, capability string) bool {
	if capability == "" || g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return false
	}
	device, ok := g.svr.memoryStorer.Load(deviceID)
	return ok && device != nil && device.isCapabilityDisabled(capability)
}

func (g *GB28181API) deviceSupportsGBFeature(deviceID, capability string, version GBProtocolVersion, supported func(GBCapabilities) bool) bool {
	return supported != nil && supported(version.Capabilities()) && !g.isDeviceCapabilityDisabled(deviceID, capability)
}

func (g *GB28181API) requireGBFeature(deviceID, capability, feature string, supported func(GBCapabilities) bool) error {
	version := g.getDeviceGBProtocolVersion(deviceID)
	if !g.deviceSupportsGBFeature(deviceID, capability, version, supported) {
		g.recordUnsupportedGBFeature(deviceID, feature, version)
		return fmt.Errorf("%s 不受当前协议档案 %s 支持", feature, version.StandardName())
	}
	return nil
}

func (g *GB28181API) recordUnsupportedGBFeature(deviceID, feature string, version GBProtocolVersion) {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return
	}
	_ = g.svr.memoryStorer.Change(deviceID, func(device *ipc.Device) error {
		device.Ext.GBLastUnsupportedCommand = feature
		device.Ext.GBLastUnsupportedVersion = string(version)
		device.Ext.GBLastUnsupportedUpdatedAt = time.Now().Unix()
		return nil
	}, func(*Device) {})
}

func (g *GB28181API) requireMediaTransport(deviceID string, streamMode int8, feature string) error {
	if streamMode == 0 {
		return nil
	}
	return g.requireGBFeature(deviceID, "rtp_over_tcp", feature+" RTP over TCP", func(c GBCapabilities) bool {
		return c.RTPOverTCP
	})
}

// requireGBVersionAtLeast 检查设备是否满足最小协议版本要求。
func (g *GB28181API) requireGBVersionAtLeast(deviceID string, min string, feature string) error {
	version := g.getDeviceGBProtocolVersion(deviceID)
	minimum, ok := protocolVersionForMinimum(min)
	if !ok {
		return fmt.Errorf("未知的最低 GB/T 28181 版本: %s", min)
	}
	if !version.AtLeast(minimum) {
		return fmt.Errorf("%s 在 %s 中未定义，当前设备版本为 %s", feature, minimum.StandardName(), version.StandardName())
	}
	return nil
}
