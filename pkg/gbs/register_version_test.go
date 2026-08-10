package gbs

import (
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestRegisterResponseIncludesPlatformVersion(t *testing.T) {
	deviceURI, err := sip.ParseSipURI("sip:34020000001320000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	serverURI, err := sip.ParseSipURI("sip:34020000002000000001@3402000000")
	if err != nil {
		t.Fatal(err)
	}
	from := &sip.Address{URI: &deviceURI, Params: sip.NewParams()}
	to := &sip.Address{URI: &serverURI, Params: sip.NewParams()}
	hb := sip.NewHeaderBuilder().SetFrom(from).SetToWithParam(to).SetMethod(sip.MethodRegister).AddVia(&sip.ViaHop{
		Host:      "192.0.2.10",
		Port:      sip.NewPort(5060),
		Transport: "UDP",
		Params:    sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
	})
	req := sip.NewRequest("", sip.MethodRegister, &serverURI, sip.DefaultSipVersion, hb.Build(), nil)
	ctx := &sip.Context{Request: req}

	api := &GB28181API{}
	for _, status := range []int{200, 401, 403} {
		resp := api.newRegisterResponse(ctx, status, "test")
		headers := resp.GetHeaders("X-GB-Ver")
		if len(headers) != 1 || headers[0].String() != "X-GB-Ver: 3.0" {
			t.Fatalf("status %d X-GB-Ver headers = %#v", status, headers)
		}
	}
}
