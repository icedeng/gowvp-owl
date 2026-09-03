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
	if seed == "" {
		seed = cfg.SignalDigest.Seed
	}
	if seed == "" {
		password := cfg.Password
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
	if g.annexG != nil && annexGCommandFromRequest(request) != "" {
		if system := g.annexG.systems[deviceID]; system != nil {
			return newSignalDigestSecurity(cfg, annexGSignalDigestSeed(
				system.signalDigestSeed, system.password, cfg.SignalDigest.Seed,
			))
		}
	}
	if g.svr != nil && g.svr.cascade != nil {
		if worker, ok := g.svr.cascade.matchRegistered(deviceID, request.Source(), request.GetConnection()); ok && worker != nil {
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
	return newSignalDigestSecurity(cfg, annexGSignalDigestSeed("", credential.Password, cfg.SignalDigest.Seed))
}

type combinedMessageSecurity struct {
	items []sip.MessageSecurity
}

func (security combinedMessageSecurity) Verify(message sip.Message) error {
	for _, item := range security.items {
		if item != nil {
			if err := item.Verify(message); err != nil {
				return err
			}
		}
	}
	return nil
}

func (security combinedMessageSecurity) Sign(message sip.Message) error {
	for _, item := range security.items {
		if item != nil {
			if err := item.Sign(message); err != nil {
				return err
			}
		}
	}
	return nil
}

func combineMessageSecurity(items ...sip.MessageSecurity) sip.MessageSecurity {
	active := make([]sip.MessageSecurity, 0, len(items))
	for _, item := range items {
		if item != nil {
			active = append(active, item)
		}
	}
	switch len(active) {
	case 0:
		return nil
	case 1:
		return active[0]
	default:
		return combinedMessageSecurity{items: active}
	}
}

func signedSIPRequestLength(request *sip.Request, security sip.MessageSecurity) (int, error) {
	if request == nil {
		return 0, fmt.Errorf("SIP request is unavailable")
	}
	if security == nil {
		wire, err := safeSIPMessageString(request)
		if err != nil {
			return 0, fmt.Errorf("serialize SIP request for transport selection: %w", err)
		}
		return len(wire), nil
	}
	probe, err := cloneSIPRequestForTransportSelection(request)
	if err != nil {
		return 0, err
	}
	if err := signSIPMessageSafely(security, probe); err != nil {
		return 0, fmt.Errorf("sign SIP request for transport selection: %w", err)
	}
	wire, err := safeSIPMessageString(probe)
	if err != nil {
		return 0, fmt.Errorf("serialize signed SIP request for transport selection: %w", err)
	}
	return len(wire), nil
}

func cloneSIPRequestForTransportSelection(request *sip.Request) (probe *sip.Request, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			probe = nil
			err = fmt.Errorf("clone SIP request for transport selection: %v", recovered)
		}
	}()
	cloned := request.Clone()
	var ok bool
	probe, ok = cloned.(*sip.Request)
	if !ok || probe == nil {
		return nil, fmt.Errorf("clone SIP request for transport selection")
	}
	return probe, nil
}

func signSIPMessageSafely(security sip.MessageSecurity, message sip.Message) (err error) {
	if security == nil {
		return nil
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("SIP message signer panic: %v", recovered)
		}
	}()
	return security.Sign(message)
}

func safeSIPMessageString(message sip.Message) (wire string, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			wire = ""
			err = fmt.Errorf("SIP message serialization panic: %v", recovered)
		}
	}()
	return message.String(), nil
}

func (g *GB28181API) resolveRequestSecurity(request *sip.Request) (sip.MessageSecurity, error) {
	digest, err := g.resolveSignalDigestSecurity(request)
	if err != nil {
		return nil, err
	}
	var identity sip.MessageSecurity
	if request != nil && !strings.EqualFold(request.Method(), sip.MethodRegister) && g != nil && g.svr != nil && g.svr.cascade != nil {
		deviceID := signalDigestRequestDeviceID(request)
		if worker, ok := g.svr.cascade.matchRegistered(deviceID, request.Source(), request.GetConnection()); ok &&
			worker != nil && worker.platform.monitorUserIdentity != nil {
			identity = &monitorUserIdentityMessageSecurity{policy: worker.platform.monitorUserIdentity}
		}
	}
	return combineMessageSecurity(identity, digest), nil
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
	if seed := platform.signalDigestSeed; seed != "" {
		return seed
	}
	if password := platform.password; password != "" && password != ignorePassword {
		return password
	}
	return fallback
}

func annexGSignalDigestSeed(explicit, password, fallback string) string {
	if seed := explicit; seed != "" {
		return seed
	}
	if seed := password; seed != "" && seed != ignorePassword {
		return seed
	}
	return fallback
}

func targetSignalDigestSeed(target Targeter) string {
	switch value := target.(type) {
	case *Device:
		if value != nil {
			if password := value.PasswordValue(); password != ignorePassword {
				return password
			}
		}
	case *Channel:
		if value != nil && value.device != nil {
			if password := value.device.PasswordValue(); password != ignorePassword {
				return password
			}
		}
	}
	return ""
}
