package sip

import "testing"

func TestParseSipURIRejectsMalformedInputWithoutPanic(t *testing.T) {
	tests := []string{
		"",
		"s",
		"sip",
		"sips",
		"sipx:example.com",
		"sip:",
		"sips:",
		"sip:@example.com",
		"sip:user@",
		"sip::5060",
		"sip:example.com:",
		"sip:[2001:db8::1",
		"sip:[]:5060",
		"sip:[example.com]:5060",
		"sip:[fe80::1%en0]:5060",
		"sip:device@example.com\r\nVia: injected",
		"sip:device@example.com\x7f",
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if _, err := ParseSipURI(input); err == nil {
				t.Fatalf("ParseSipURI(%q) succeeded, want error", input)
			}
		})
	}
}

func TestParseSipURIAcceptsValidForms(t *testing.T) {
	tests := []struct {
		input     string
		host      string
		encrypted bool
	}{
		{input: "sip:34020000001320000001@3402000000:5060", host: "3402000000"},
		{input: "sips:34020000001320000001@example.com", host: "example.com", encrypted: true},
		{input: "sip:[2001:db8::1]:5060", host: "2001:db8::1"},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			uri, err := ParseSipURI(test.input)
			if err != nil {
				t.Fatalf("ParseSipURI(%q): %v", test.input, err)
			}
			if uri.Host() != test.host || uri.FIsEncrypted != test.encrypted {
				t.Fatalf("ParseSipURI(%q) = host %q encrypted %v", test.input, uri.Host(), uri.FIsEncrypted)
			}
			if got := uri.String(); got != test.input {
				t.Fatalf("ParseSipURI(%q).String() = %q", test.input, got)
			}
		})
	}
}

func TestParseAddressValueRejectsMalformedInputWithoutPanic(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"<",
		"<>",
		"<sip:device@example.com",
		"camera <sip:device@example.com",
		`"camera <sip:device@example.com>`,
	}

	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			if _, _, _, err := ParseAddressValue(input); err == nil {
				t.Fatalf("ParseAddressValue(%q) succeeded, want error", input)
			}
		})
	}
}

func TestParseAddressValueAcceptsBareAndBracketedAddresses(t *testing.T) {
	tests := []struct {
		input       string
		displayName string
		host        string
		param       string
	}{
		{input: "sip:device@example.com;expires=3600", host: "example.com", param: "expires"},
		{input: `"camera" <sip:device@example.com>;tag=abc`, displayName: "camera", host: "example.com", param: "tag"},
	}

	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			displayName, uri, params, err := ParseAddressValue(test.input)
			if err != nil {
				t.Fatalf("ParseAddressValue(%q): %v", test.input, err)
			}
			if uri == nil || uri.Host() != test.host {
				t.Fatalf("ParseAddressValue(%q) host = %v", test.input, uri)
			}
			gotDisplayName := ""
			if displayName != nil {
				gotDisplayName = displayName.String()
			}
			if gotDisplayName != test.displayName {
				t.Fatalf("ParseAddressValue(%q) display name = %q", test.input, gotDisplayName)
			}
			if !params.Has(test.param) {
				t.Fatalf("ParseAddressValue(%q) params missing %q", test.input, test.param)
			}
		})
	}
}

func TestParseAddressValuesRejectsUnclosedDelimiters(t *testing.T) {
	for _, input := range []string{
		"<sip:first@example.com",
		`"camera <sip:first@example.com>`,
	} {
		t.Run(input, func(t *testing.T) {
			if _, _, _, err := ParseAddressValues(input); err == nil {
				t.Fatalf("ParseAddressValues(%q) succeeded, want error", input)
			}
		})
	}
}

func TestParseSIPCompactHeadersNormalizeToCanonicalNames(t *testing.T) {
	tests := map[string]string{
		"i": "Call-ID",
		"m": "Contact",
		"e": "Content-Encoding",
		"l": "Content-Length",
		"c": "Content-Type",
		"f": "From",
		"s": "Subject",
		"k": "Supported",
		"t": "To",
		"v": "Via",
		"o": "Event",
		"u": "Allow-Events",
	}
	for compact, canonical := range tests {
		t.Run(compact, func(t *testing.T) {
			values := map[string]string{
				"i": "compact-call-id", "m": "<sip:device@example.com>",
				"e": "identity", "l": "0", "c": "text/plain",
				"f": "<sip:device@example.com>;tag=t", "s": "compact-subject",
				"k": "replaces", "t": "<sip:server@example.com>",
				"v": "SIP/2.0/UDP 192.0.2.10:5060;branch=z9hG4bK-test",
				"o": "presence", "u": "presence",
			}
			headerText := compact + ": " + values[compact]
			header, err := ParseHeader(headerText)
			if err != nil {
				t.Fatalf("ParseHeader(%q): %v", headerText, err)
			}
			if len(header) != 1 || header[0].Name() != canonical {
				t.Fatalf("ParseHeader(%q) = %#v, want canonical %q", headerText, header, canonical)
			}
		})
	}
}

func TestSIPCompactHeaderMessageUsesCanonicalGetters(t *testing.T) {
	message := parseFixtureMessage(t, []byte("MESSAGE sip:34020000001320000001@3402000000 SIP/2.0\r\n"+
		"v: SIP/2.0/UDP 192.0.2.10:5060;branch=z9hG4bK-compact\r\n"+
		"f: <sip:34020000001320000001@3402000000>;tag=compact\r\n"+
		"t: <sip:34020000002000000001@3402000000>\r\n"+
		"i: compact-call-id\r\n"+
		"CSeq: 1 MESSAGE\r\n"+
		"m: <sip:34020000001320000001@192.0.2.10:5060>\r\n"+
		"k: replaces\r\n"+
		"o: presence\r\n"+
		"u: presence\r\n"+
		"s: compact-subject\r\n"+
		"e: identity\r\n"+
		"c: Application/MANSCDP+xml\r\n"+
		"l: 0\r\n\r\n"))
	if message == nil {
		t.Fatal("compact SIP message was not parsed")
	}
	if callID, ok := message.CallID(); !ok || callID == nil || string(*callID) != "compact-call-id" {
		t.Fatalf("compact Call-ID = %#v, ok=%v", callID, ok)
	}
	if _, ok := message.From(); !ok {
		t.Fatal("compact From was not available through canonical getter")
	}
	if _, ok := message.To(); !ok {
		t.Fatal("compact To was not available through canonical getter")
	}
	if _, ok := message.Via(); !ok {
		t.Fatal("compact Via was not available through canonical getter")
	}
	if _, ok := message.Contact(); !ok {
		t.Fatal("compact Contact was not available through canonical getter")
	}
	if _, ok := message.ContentType(); !ok {
		t.Fatal("compact Content-Type was not available through canonical getter")
	}
	for name := range map[string]struct{}{"Subject": {}, "Event": {}, "Content-Encoding": {}, "Supported": {}, "Allow-Events": {}} {
		if got := message.GetHeaders(name); len(got) != 1 {
			t.Fatalf("canonical %s headers = %d, want 1", name, len(got))
		}
	}
}

func FuzzParseSIPAddressesDoNotPanic(f *testing.F) {
	for _, seed := range []string{"", "sip", "sip:", "<", "   ", "sip:device@example.com"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ParseSipURI(input)
		_, _, _, _ = ParseAddressValue(input)
		_, _, _, _ = ParseAddressValues(input)
	})
}

func FuzzParseSIPHeaderDoesNotPanic(f *testing.F) {
	for _, seed := range []string{
		"",
		"Via:",
		"Via: SIP/2.0/UDP 192.0.2.10:5060;branch=z9hG4bK-test",
		"From: <sip:34020000001320000001@3402000000>;tag=test",
		"To: <",
		"CSeq: 1 MESSAGE",
		"Route: <sip:proxy.example;lr>",
		"Content-Length: 0",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ParseHeader(input)
	})
}
