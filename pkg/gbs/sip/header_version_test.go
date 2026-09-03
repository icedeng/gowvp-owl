package sip

import "testing"

func TestHeaderBuilderAdvertisesRequiredGBMediaMethods(t *testing.T) {
	headers := NewHeaderBuilder().SetMethod(MethodInvite).Build()
	var allow *AllowHeader
	for _, header := range headers {
		if value, ok := header.(*AllowHeader); ok {
			allow = value
			break
		}
	}
	if allow == nil {
		t.Fatal("missing Allow header")
	}

	advertised := make(map[string]struct{}, len(*allow))
	for _, method := range *allow {
		advertised[method] = struct{}{}
	}
	for _, method := range []string{
		MethodInvite,
		MethodACK,
		MethodInfo,
		MethodCancel,
		MethodBYE,
		MethodOptions,
		MethodMessage,
	} {
		if _, ok := advertised[method]; !ok {
			t.Errorf("Allow header does not advertise %s", method)
		}
	}
}

func TestHeaderBuilderDoesNotAssumeGBVersion(t *testing.T) {
	headers := NewHeaderBuilder().SetMethod(MethodOptions).Build()
	for _, header := range headers {
		if header.Name() == "X-GB-Ver" {
			t.Fatalf("new builder unexpectedly contains %s", header.String())
		}
	}
}

func TestHeaderBuilderOmitsEmptySupportedHeader(t *testing.T) {
	headers := NewHeaderBuilder().SetMethod(MethodOptions).Build()
	for _, header := range headers {
		if header.Name() == "Supported" {
			t.Fatalf("new builder unexpectedly contains empty extension declaration %q", header.String())
		}
	}
}

func TestHeaderBuilderSetsGBVersionExplicitly(t *testing.T) {
	headers := NewHeaderBuilder().SetMethod(MethodOptions).SetXGBVerValue("1.1").Build()
	found := false
	for _, header := range headers {
		if header.Name() == "X-GB-Ver" {
			found = true
			if header.String() != "X-GB-Ver: 1.1" {
				t.Fatalf("header = %q", header.String())
			}
		}
	}
	if !found {
		t.Fatal("missing explicit X-GB-Ver")
	}
}

func TestExpiresStringPreservesUint32Range(t *testing.T) {
	expires := Expires(^uint32(0))
	if got := expires.String(); got != "Expires: 4294967295" {
		t.Fatalf("maximum Expires header = %q", got)
	}
}
