package sip

import "testing"

func TestHeaderBuilderDoesNotAssumeGBVersion(t *testing.T) {
	headers := NewHeaderBuilder().SetMethod(MethodOptions).Build()
	for _, header := range headers {
		if header.Name() == "X-GB-Ver" {
			t.Fatalf("new builder unexpectedly contains %s", header.String())
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
