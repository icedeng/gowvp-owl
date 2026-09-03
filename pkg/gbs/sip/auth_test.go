package sip

import "testing"

func TestAuthFromValueParsesQuotedQOPList(t *testing.T) {
	auth := AuthFromValue(`WWW-Authenticate: Digest realm="example", nonce="abc", algorithm=SHA-256, qop="auth,auth-int", opaque="a,b"`)
	if auth.Get("realm") != "example" || auth.Get("nonce") != "abc" || auth.Algorithm() != "SHA-256" {
		t.Fatalf("parsed Digest challenge = %#v", auth.Data)
	}
	if auth.Get("qop") != "auth,auth-int" || auth.QOP() != "auth" || auth.Get("opaque") != "a,b" {
		t.Fatalf("parsed Digest qop/opaque = %#v, selected=%q", auth.Data, auth.QOP())
	}
}

func TestAuthFromValueCheckedRejectsAmbiguousDigestParameters(t *testing.T) {
	auth, err := AuthFromValueChecked(`WWW-Authenticate: Digest realm="example", nonce="abc", algorithm=SHA-256, qop="auth,auth-int", opaque="a,b"`)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Get("realm") != "example" || auth.Get("nonce") != "abc" || auth.Algorithm() != "SHA-256" || auth.QOP() != "auth" {
		t.Fatalf("strict Digest challenge = %#v, selected=%q", auth.Data, auth.QOP())
	}

	for _, value := range []string{
		`realm="example",nonce="abc"`,
		`Basic realm="example",nonce="abc"`,
		`Digest realm="first",realm="second",nonce="abc"`,
		`Digest realm="example",nonce="first",nonce="second"`,
		`Digest realm="example",nonce="abc",algorithm=MD5,algorithm=SHA-256`,
		`Digest realm="example",nonce="abc",qop="auth",qop="auth"`,
		`Digest realm="example",nonce="abc",`,
		`Digest realm="example",nonce="abc" trailing`,
		`Digest realm="example,nonce="abc"`,
	} {
		if _, err := AuthFromValueChecked(value); err == nil {
			t.Fatalf("ambiguous Digest challenge accepted: %s", value)
		}
	}
}

func TestCalcResponseMatchesRFC2617MD5Example(t *testing.T) {
	got := CalcResponse(
		"Mufasa", "testrealm@host.com", "Circle Of Life", "GET", "/dir/index.html",
		"dcd98b7102dd2f0e8b11d0f600bfb0c093", "auth", "0a4f113b", "00000001",
	)
	if want := "6629fae49393a05397450978507c4ef1"; got != want {
		t.Fatalf("MD5 response = %q, want %q", got, want)
	}
}

func TestCalcResponseWithAlgorithmSHA256(t *testing.T) {
	got, err := CalcResponseWithAlgorithm(
		"SHA-256", "Mufasa", "http-auth@example.org", "Circle Of Life", "GET", "/dir/index.html",
		"7ypf/xlj9XXwfDPEoM4URrv/xFPKKgqjJ6dS4v3", "auth", "f2/wE4q74M", "00000001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "c8abfecbd4a197ebe531ef9cad5c50e64ef29c249b154b6d16bfa4029137a2d7"; got != want {
		t.Fatalf("SHA-256 response = %q, want %q", got, want)
	}
}

func TestCalcResponseWithAlgorithmSHA1(t *testing.T) {
	got, err := CalcResponseWithAlgorithm(
		"SHA-1", "Mufasa", "http-auth@example.org", "Circle Of Life", "GET", "/dir/index.html",
		"nonce", "auth", "cnonce", "00000001",
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := "663d8f6d20c233b2b95f9b4b41fd0119fe193824"; got != want {
		t.Fatalf("SHA-1 response = %q, want %q", got, want)
	}
}

func TestCalcResponseCheckedRejectsUnsupportedDigestModes(t *testing.T) {
	auth := AuthFromValue(`Digest realm="example",nonce="abc",algorithm=SM3,qop="auth"`).
		SetUsername("device").SetPassword("secret").SetMethod(MethodRegister).SetURI("sip:example")
	auth.SetClientNonce("00000001", "client")
	if _, err := auth.CalcResponseChecked(); err == nil {
		t.Fatal("unsupported algorithm accepted")
	}

	auth = AuthFromValue(`Digest realm="example",nonce="abc",algorithm=MD5,qop="auth-int"`).
		SetUsername("device").SetPassword("secret").SetMethod(MethodRegister).SetURI("sip:example")
	auth.SetClientNonce("00000001", "client")
	if _, err := auth.CalcResponseChecked(); err == nil {
		t.Fatal("unsupported qop accepted")
	}
}
