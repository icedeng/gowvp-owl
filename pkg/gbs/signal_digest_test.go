package gbs

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestResolveSignalDigestSecurityUsesDevicePasswordAcrossVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			memory := newFlowMemory(gb10DeviceID)
			memory.runtime.Password = "device-signal-seed"
			memory.runtime.setGBVersion(version)
			api := &GB28181API{
				cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
					Enabled: true, Required: true, Algorithm: "MD5", Encoding: "base64",
					AcceptLegacyHex: true, Window: conf.Duration(10 * time.Minute),
				}},
				svr: &Server{memoryStorer: memory},
			}
			request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "signal-digest-"+string(version), []byte("payload"))
			request.RemoveHeader("X-GB-Ver")
			request.AppendHeader(&sip.GenericHeader{HeaderName: "X-GB-Ver", Contents: string(version)})
			signer, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
				Seed: "device-signal-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := signer.Sign(request); err != nil {
				t.Fatal(err)
			}
			security, err := api.resolveSignalDigestSecurity(request)
			if err != nil {
				t.Fatal(err)
			}
			if security == nil {
				t.Fatal("signal Digest security was not resolved")
			}
			if err := security.Verify(request); err != nil {
				t.Fatalf("version %s signed request rejected: %v", version, err)
			}
		})
	}
}

func TestSignalDigestSeedResolutionAndAlgorithms(t *testing.T) {
	device := &Device{Password: "device-seed"}
	channel := &Channel{device: device}
	if got := targetSignalDigestSeed(device); got != "device-seed" {
		t.Fatalf("device seed = %q", got)
	}
	if got := targetSignalDigestSeed(channel); got != "device-seed" {
		t.Fatalf("channel seed = %q", got)
	}
	platform := cascadePlatform{password: "upstream-password", signalDigestSeed: "upstream-note-seed"}
	if got := cascadeSignalDigestSeed(platform, "fallback"); got != "upstream-note-seed" {
		t.Fatalf("explicit upstream seed = %q", got)
	}
	platform.signalDigestSeed = ""
	if got := cascadeSignalDigestSeed(platform, "fallback"); got != "upstream-password" {
		t.Fatalf("upstream password seed = %q", got)
	}

	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Seed: "seed", Algorithm: "SM3", Encoding: "base64", Window: conf.Duration(time.Minute),
	}}}
	if _, err := api.newSignalDigestSecurity(""); err != nil {
		t.Fatalf("SM3 signal Digest was rejected: %v", err)
	}
	api.cfg.SignalDigest.Algorithm = "SM4"
	if _, err := api.newSignalDigestSecurity(""); err == nil {
		t.Fatal("unsupported SM4 signal Digest was accepted")
	}
}

func TestResolveSignalDigestSecurityUsesRegisteredUpstreamSeed(t *testing.T) {
	platform := testCascadePlatform(t, "1.1")
	platform.signalDigestSeed = "upstream-note-seed"
	worker := newCascadeWorker(nil, platform)
	worker.updateStatus(func(status *CascadePlatformStatus) { status.Registered = true })
	manager := NewCascadeManager(nil)
	manager.items[platform.name] = worker
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Required: true, Seed: "global-note-seed", Algorithm: "MD5",
		Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}}
	api.svr = &Server{cascade: manager}

	request := newFlowRequest(t, newFlowConnection(), sip.MethodMessage, "upstream-signal-digest", []byte("payload"))
	upstream := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	request.RemoveHeader("From")
	request.AppendHeader(&sip.FromHeader{Address: upstream.URI, Params: sip.NewParams()})
	request.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5090})
	signer, err := sip.NewSignalDigestSecurity(sip.SignalDigestOptions{
		Seed: "upstream-note-seed", Algorithm: "MD5", Encoding: "base64", Required: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := signer.Sign(request); err != nil {
		t.Fatal(err)
	}
	security, err := api.resolveSignalDigestSecurity(request)
	if err != nil {
		t.Fatal(err)
	}
	if security == nil {
		t.Fatal("registered upstream signal Digest security was not resolved")
	}
	if err := security.Verify(request); err != nil {
		t.Fatalf("registered upstream signature rejected: %v", err)
	}
}

func TestWrapRequestSignsOutboundDeviceMessage(t *testing.T) {
	flow := newFlowConnection()
	connection := &signalDigestFlowTCPConnection{flowConnection: flow}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	deviceAddress := mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000")
	sipServer := sip.NewServer(platform)
	defer sipServer.Close()
	api := &GB28181API{cfg: &conf.SIP{SignalDigest: conf.SIPSignalDigest{
		Enabled: true, Algorithm: "MD5", Encoding: "base64", Window: conf.Duration(10 * time.Minute),
	}}}
	server := &Server{Server: sipServer, gb: api, fromAddress: *platform}
	api.svr = server
	device := &Device{
		Password: "device-signal-seed", conn: connection, source: flow.remote, to: deviceAddress,
		gbVersion: string(GBVersion10),
	}
	if _, err := server.wrapRequest(device, sip.MethodMessage, &sip.ContentTypeXML, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	select {
	case payload := <-flow.writes:
		text := string(payload)
		if !strings.Contains(text, "Date: ") || !strings.Contains(text, "Note: Digest nonce=") || !strings.Contains(text, "algorithm=MD5") {
			t.Fatalf("outbound secured MESSAGE = %s", text)
		}
	case <-time.After(time.Second):
		t.Fatal("outbound secured MESSAGE timeout")
	}
}

type signalDigestFlowTCPConnection struct {
	*flowConnection
}

func (*signalDigestFlowTCPConnection) Network() string { return "tcp" }
