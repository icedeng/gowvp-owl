package sip

import (
	"net"
	"testing"
)

func TestContextParsesXGBVersion(t *testing.T) {
	versions := []string{"1.0", "1.1", "2.0", "3.0"}
	for _, version := range versions {
		t.Run(version, func(t *testing.T) {
			ctx := contextWithXGBVersion(t, version)
			if ctx.XGBVer != version || ctx.XGBVerRaw != version {
				t.Fatalf("version = %q, raw = %q; want %q", ctx.XGBVer, ctx.XGBVerRaw, version)
			}
		})
	}
}

func TestContextPreservesUnknownXGBVersion(t *testing.T) {
	ctx := contextWithXGBVersion(t, "4.0")
	if ctx.XGBVer != "" || ctx.XGBVerRaw != "4.0" {
		t.Fatalf("version = %q, raw = %q; want empty, 4.0", ctx.XGBVer, ctx.XGBVerRaw)
	}
}

func contextWithXGBVersion(t *testing.T, version string) *Context {
	t.Helper()
	deviceURI, err := ParseSipURI("sip:34020000001320000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	serverURI, err := ParseSipURI("sip:34020000002000000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	from := &Address{URI: &deviceURI, Params: NewParams()}
	to := &Address{URI: &serverURI, Params: NewParams()}
	hb := NewHeaderBuilder().SetFrom(from).SetTo(to).SetMethod(MethodRegister).AddVia(&ViaHop{
		Host:      "192.0.2.10",
		Port:      NewPort(5060),
		Transport: "UDP",
		Params:    NewParams().Add("branch", String{Str: GenerateBranch()}),
	}).SetXGBVerValue(version)
	req := NewRequest("", MethodRegister, &serverURI, DefaultSipVersion, hb.Build(), nil)
	req.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 5060})

	ctx := newContext(req, nil)
	if ctx.DeviceID == "" {
		t.Fatal("context did not parse device ID")
	}
	return ctx
}
