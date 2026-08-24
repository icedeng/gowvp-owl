package gbs

import (
	"context"
	"strings"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/orm"
)

type GBDiagnostics struct {
	DeviceID               string   `json:"device_id"`
	DeclaredVersion        string   `json:"declared_version,omitempty"`
	EffectiveVersion       string   `json:"effective_version"`
	ManualVersion          string   `json:"manual_version,omitempty"`
	VersionSource          string   `json:"version_source,omitempty"`
	VersionUpdatedAt       int64    `json:"version_updated_at,omitempty"`
	Capabilities           []string `json:"capabilities"`
	DisabledCapabilities   []string `json:"disabled_capabilities,omitempty"`
	LastUnsupportedCommand string   `json:"last_unsupported_command,omitempty"`
	LastUnsupportedVersion string   `json:"last_unsupported_version,omitempty"`
	LastUnsupportedAt      int64    `json:"last_unsupported_at,omitempty"`
}

func (g *GB28181API) GetDiagnostics(ctx context.Context, deviceID string) (*GBDiagnostics, error) {
	deviceID = strings.TrimSpace(deviceID)
	var ext ipc.DeviceExt
	if g.core.Store() != nil {
		var device ipc.Device
		if err := g.core.Store().Device().Get(ctx, &device, orm.Where("device_id=?", deviceID)); err != nil {
			return nil, err
		}
		ext = device.Ext
	}
	version, source := resolveGBProtocolVersion(ext, "")
	if ext.GBEffectiveVersion == "" {
		ext.GBEffectiveVersion = string(version)
	}
	if ext.GBVersionSource == "" {
		ext.GBVersionSource = source
	}
	return &GBDiagnostics{
		DeviceID:               deviceID,
		DeclaredVersion:        ext.GBDeclaredVersion,
		EffectiveVersion:       ext.GBEffectiveVersion,
		ManualVersion:          ext.GBManualVersion,
		VersionSource:          ext.GBVersionSource,
		VersionUpdatedAt:       ext.GBVersionUpdatedAt,
		Capabilities:           effectiveCapabilityNames(version, ext.GBDisabledCapabilities),
		DisabledCapabilities:   append([]string(nil), ext.GBDisabledCapabilities...),
		LastUnsupportedCommand: ext.GBLastUnsupportedCommand,
		LastUnsupportedVersion: ext.GBLastUnsupportedVersion,
		LastUnsupportedAt:      ext.GBLastUnsupportedUpdatedAt,
	}, nil
}

func (s *Server) GetDiagnostics(ctx context.Context, deviceID string) (*GBDiagnostics, error) {
	return s.gb.GetDiagnostics(ctx, deviceID)
}
