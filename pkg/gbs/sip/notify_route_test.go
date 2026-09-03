package sip

import (
	"strings"
	"testing"
)

func TestRequestRouteKeyAllowsEmptyTerminatedNotify(t *testing.T) {
	target, err := ParseSipURI("sip:34020000002000000001@192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodNotify, &target, DefaultSipVersion, []Header{
		&GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"},
		func() Header { value := ContentLength(0); return &value }(),
	}, nil)
	key, err := requestRouteKey(request)
	if err != nil || key != MethodNotify {
		t.Fatalf("terminated NOTIFY route = %q, %v", key, err)
	}

	request.RemoveHeader("Subscription-State")
	request.AppendHeader(&GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := requestRouteKey(request); err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("active empty NOTIFY error = %v", err)
	}

	message := NewRequest("", MethodMessage, &target, DefaultSipVersion, []Header{
		func() Header { value := ContentLength(0); return &value }(),
	}, nil)
	if _, err := requestRouteKey(message); err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("empty MESSAGE error = %v", err)
	}
}

func TestRequestRouteKeyNormalizesCmdTypeWhitespace(t *testing.T) {
	target, err := ParseSipURI("sip:34020000002000000001@192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{MethodMessage, MethodNotify} {
		t.Run(method, func(t *testing.T) {
			body := []byte(`<Response><CmdType> RecordInfo </CmdType></Response>`)
			request := NewRequest("", method, &target, DefaultSipVersion, []Header{
				func() Header { value := ContentTypeXML; return &value }(),
				func() Header { value := ContentLength(len(body)); return &value }(),
			}, body)
			key, err := requestRouteKey(request)
			if err != nil || key != method+"-RecordInfo" {
				t.Fatalf("route = %q, %v", key, err)
			}
		})
	}
}

func TestRequestRouteKeyNormalizesMethodCaseBeforeMANSCDPRouting(t *testing.T) {
	target, err := ParseSipURI("sip:34020000002000000001@192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType></Notify>`)
	for _, test := range []struct {
		method string
		want   string
	}{
		{method: "message", want: MethodMessage + "-Keepalive"},
		{method: "MeSsAgE", want: MethodMessage + "-Keepalive"},
		{method: "notify", want: MethodNotify + "-Keepalive"},
		{method: "NoTiFy", want: MethodNotify + "-Keepalive"},
	} {
		t.Run(test.method, func(t *testing.T) {
			request := NewRequest("", test.method, &target, DefaultSipVersion, []Header{
				func() Header { value := ContentTypeXML; return &value }(),
				func() Header { value := ContentLength(len(body)); return &value }(),
			}, body)
			key, routeErr := requestRouteKey(request)
			if routeErr != nil || key != test.want {
				t.Fatalf("route = %q, %v, want %q", key, routeErr, test.want)
			}
		})
	}
}

func TestRequestRouteKeyRejectsMissingCmdType(t *testing.T) {
	target, err := ParseSipURI("sip:34020000002000000001@192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Response><CmdType> </CmdType></Response>`)
	request := NewRequest("", MethodMessage, &target, DefaultSipVersion, []Header{
		func() Header { value := ContentTypeXML; return &value }(),
		func() Header { value := ContentLength(len(body)); return &value }(),
	}, body)
	if _, err := requestRouteKey(request); err == nil || !strings.Contains(err.Error(), "missing CmdType") {
		t.Fatalf("missing CmdType error = %v", err)
	}
}

func TestRequestRouteKeyValidatesMANSCDPContentType(t *testing.T) {
	target, err := ParseSipURI("sip:34020000002000000001@192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Notify><CmdType>Keepalive</CmdType></Notify>`)
	contentLength := func() Header { value := ContentLength(len(body)); return &value }
	tests := []struct {
		name        string
		contentType []Header
		wantErr     string
	}{
		{
			name: "canonical",
			contentType: []Header{
				func() Header { value := ContentTypeXML; return &value }(),
			},
		},
		{
			name: "case insensitive with charset",
			contentType: []Header{
				func() Header { value := ContentType("application/manscdp+xml; charset=UTF-8"); return &value }(),
			},
		},
		{name: "missing", wantErr: "requires exactly one Content-Type"},
		{
			name: "wrong media type",
			contentType: []Header{
				func() Header { value := ContentType("application/sdp"); return &value }(),
			},
			wantErr: "requires Application/MANSCDP+xml Content-Type",
		},
		{
			name: "malformed",
			contentType: []Header{
				func() Header { value := ContentType("Application/MANSCDP+xml; charset"); return &value }(),
			},
			wantErr: "requires Application/MANSCDP+xml Content-Type",
		},
		{
			name: "duplicate",
			contentType: []Header{
				func() Header { value := ContentTypeXML; return &value }(),
				func() Header { value := ContentTypeXML; return &value }(),
			},
			wantErr: "requires exactly one Content-Type",
		},
	}
	for _, method := range []string{MethodMessage, MethodNotify} {
		for _, test := range tests {
			t.Run(method+"/"+test.name, func(t *testing.T) {
				headers := append([]Header{}, test.contentType...)
				headers = append(headers, contentLength())
				request := NewRequest("", method, &target, DefaultSipVersion, headers, body)
				key, err := requestRouteKey(request)
				if test.wantErr == "" {
					if err != nil || key != method+"-Keepalive" {
						t.Fatalf("route = %q, %v", key, err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("content type error = %v, want %q", err, test.wantErr)
				}
			})
		}
	}
}

func TestRequestRouteKeyValidatesSubscribeMANSCDPContentType(t *testing.T) {
	target, err := ParseSipURI("sip:34020000002000000001@192.0.2.20")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`<Query><CmdType>Catalog</CmdType></Query>`)
	request := func(contentTypes ...ContentType) *Request {
		headers := make([]Header, 0, len(contentTypes)+1)
		for index := range contentTypes {
			value := contentTypes[index]
			headers = append(headers, &value)
		}
		length := ContentLength(len(body))
		headers = append(headers, &length)
		return NewRequest("", MethodSubscribe, &target, DefaultSipVersion, headers, body)
	}

	if key, err := requestRouteKey(request(ContentType("application/manscdp+xml; charset=UTF-8"))); err != nil || key != MethodSubscribe {
		t.Fatalf("valid SUBSCRIBE route = %q, %v", key, err)
	}
	for _, invalid := range []*Request{
		request(),
		request(ContentTypeSDP),
		request(ContentTypeXML, ContentTypeXML),
	} {
		if _, err := requestRouteKey(invalid); err == nil || !strings.Contains(err.Error(), "Content-Type") {
			t.Fatalf("invalid SUBSCRIBE Content-Type error = %v", err)
		}
	}
}
