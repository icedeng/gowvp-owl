package gbs

import (
	"fmt"
	"strings"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func (g *GB28181API) signalDigestConfigured() bool {
	cfg := g.configSnapshot()
	return cfg != nil && (cfg.SignalDigest.Enabled || cfg.SignalDigest.Required)
}

func (g *GB28181API) newSignalDigestSecurity(seed string) (sip.MessageSecurity, error) {
	return newSignalDigestSecurity(g.configSnapshot(), seed)
}

func newSignalDigestSecurity(cfg *conf.SIP, seed string) (sip.MessageSecurity, error) {
	if cfg == nil || (!cfg.SignalDigest.Enabled && !cfg.SignalDigest.Required) {
		return nil, nil
	}
	seed = strings.TrimSpace(seed)
	if seed == "" || seed == ignorePassword {
		seed = strings.TrimSpace(cfg.SignalDigest.Seed)
	}
	if seed == "" {
		password := strings.TrimSpace(cfg.Password)
		if password != ignorePassword {
			seed = password
		}
	}
	if seed == "" {
		return nil, fmt.Errorf("signal Digest is enabled but no seed is available")
	}
	return sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed:            seed,
		Algorithm:       cfg.SignalDigest.Algorithm,
		Encoding:        cfg.SignalDigest.Encoding,
		Window:          cfg.SignalDigest.Window.Duration(),
		Required:        cfg.SignalDigest.Required,
		AcceptLegacyHex: cfg.SignalDigest.AcceptLegacyHex,
	})
}

func (g *GB28181API) resolveSignalDigestSecurity(request *sip.Request) (sip.MessageSecurity, error) {
	cfg := g.configSnapshot()
	if cfg == nil || (!cfg.SignalDigest.Enabled && !cfg.SignalDigest.Required) || request == nil || strings.EqualFold(request.Method(), sip.MethodRegister) {
		return nil, nil
	}
	deviceID := signalDigestRequestDeviceID(request)
	if deviceID == "" {
		if cfg.SignalDigest.Required {
			return nil, fmt.Errorf("signal Digest request has no From device ID")
		}
		return nil, nil
	}
	if g.svr != nil && g.svr.cascade != nil {
		if worker, ok := g.svr.cascade.matchRegistered(deviceID, request.Source()); ok && worker != nil {
			return newSignalDigestSecurity(cfg, cascadeSignalDigestSeed(worker.platform, cfg.SignalDigest.Seed))
		}
	}
	credential, err := g.lookupDeviceCredential(deviceID)
	if err != nil {
		if cfg.SignalDigest.Required {
			return nil, fmt.Errorf("resolve signal Digest device seed: %w", err)
		}
		return nil, nil
	}
	return newSignalDigestSecurity(cfg, credential.Password)
}

func signalDigestRequestDeviceID(request *sip.Request) string {
	if request == nil {
		return ""
	}
	from, ok := request.From()
	if !ok || from == nil || from.Address == nil || from.Address.User() == nil {
		return ""
	}
	return strings.TrimSpace(from.Address.User().String())
}

func cascadeSignalDigestSeed(platform cascadePlatform, fallback string) string {
	if seed := strings.TrimSpace(platform.signalDigestSeed); seed != "" {
		return seed
	}
	if password := strings.TrimSpace(platform.password); password != "" && password != ignorePassword {
		return password
	}
	return strings.TrimSpace(fallback)
}

func targetSignalDigestSeed(target Targeter) string {
	switch value := target.(type) {
	case *Device:
		if value != nil {
			return value.Password
		}
	case *Channel:
		if value != nil && value.device != nil {
			return value.device.Password
		}
	}
	return ""
}
