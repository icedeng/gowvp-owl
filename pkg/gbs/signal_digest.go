package gbs

import (
	"fmt"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func (g *GB28181API) signalDigestConfigured() bool {
	return g != nil && g.cfg != nil && (g.cfg.SignalDigest.Enabled || g.cfg.SignalDigest.Required)
}

func (g *GB28181API) newSignalDigestSecurity(seed string) (sip.MessageSecurity, error) {
	if !g.signalDigestConfigured() {
		return nil, nil
	}
	seed = strings.TrimSpace(seed)
	if seed == "" || seed == ignorePassword {
		seed = strings.TrimSpace(g.cfg.SignalDigest.Seed)
	}
	if seed == "" {
		password := strings.TrimSpace(g.cfg.Password)
		if password != ignorePassword {
			seed = password
		}
	}
	if seed == "" {
		return nil, fmt.Errorf("signal Digest is enabled but no seed is available")
	}
	return sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed:            seed,
		Algorithm:       g.cfg.SignalDigest.Algorithm,
		Encoding:        g.cfg.SignalDigest.Encoding,
		Window:          g.cfg.SignalDigest.Window.Duration(),
		Required:        g.cfg.SignalDigest.Required,
		AcceptLegacyHex: g.cfg.SignalDigest.AcceptLegacyHex,
	})
}

func (g *GB28181API) resolveSignalDigestSecurity(request *sip.Request) (sip.MessageSecurity, error) {
	if !g.signalDigestConfigured() || request == nil || strings.EqualFold(request.Method(), sip.MethodRegister) {
		return nil, nil
	}
	deviceID := signalDigestRequestDeviceID(request)
	if deviceID == "" {
		if g.cfg.SignalDigest.Required {
			return nil, fmt.Errorf("signal Digest request has no From device ID")
		}
		return nil, nil
	}
	if g.svr != nil && g.svr.cascade != nil {
		if worker, ok := g.svr.cascade.matchRegistered(deviceID, request.Source()); ok && worker != nil {
			return g.newSignalDigestSecurity(cascadeSignalDigestSeed(worker.platform, g.cfg.SignalDigest.Seed))
		}
	}
	credential, err := g.lookupDeviceCredential(deviceID)
	if err != nil {
		if g.cfg.SignalDigest.Required {
			return nil, fmt.Errorf("resolve signal Digest device seed: %w", err)
		}
		return nil, nil
	}
	return g.newSignalDigestSecurity(credential.Password)
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
