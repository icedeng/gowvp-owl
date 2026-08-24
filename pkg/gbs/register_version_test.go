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

func TestParseRegisterExpires(t *testing.T) {
	conn := newFlowConnection()
	request := newFlowRequest(t, conn, sip.MethodRegister, "register-expires", nil)
	ctx := &sip.Context{Request: request}
	if got, err := parseRegisterExpires(ctx); err != nil || got != defaultRegisterExpires {
		t.Fatalf("default expires = %d, %v", got, err)
	}

	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 3600 {
		t.Fatalf("header expires = %d, %v", got, err)
	}
	request.RemoveHeader("Expires")
	contact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	contact.Params.Add("expires", sip.String{Str: "7200"})
	request.AppendHeader(&sip.ContactHeader{Address: contact.URI, Params: contact.Params})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 7200 {
		t.Fatalf("Contact expires = %d, %v", got, err)
	}
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "3600"})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 7200 {
		t.Fatalf("Contact expires did not override Expires header: %d, %v", got, err)
	}
	request.RemoveHeader("Contact")
	unregisterContact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	unregisterContact.Params.Add("expires", sip.String{Str: "0"})
	request.AppendHeader(&sip.ContactHeader{Address: unregisterContact.URI, Params: unregisterContact.Params})
	if got, err := parseRegisterExpires(ctx); err != nil || got != 0 {
		t.Fatalf("Contact expires=0 did not override Expires header: %d, %v", got, err)
	}

	request.RemoveHeader("Contact")
	request.RemoveHeader("Expires")
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "invalid"})
	if _, err := parseRegisterExpires(ctx); err == nil {
		t.Fatal("invalid expires accepted")
	}
}
