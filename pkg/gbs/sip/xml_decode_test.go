package sip

import "testing"

func TestXMLDecodeFallbackDoesNotRetainPartialFirstPass(t *testing.T) {
	type item struct {
		Name string `xml:"Name"`
	}
	type response struct {
		Items []item `xml:"Item"`
	}

	source := []byte(`<?xml version="1.0" encoding="UTF-8"?><Response><Item><Name>first</Name></Item><Item><Name>中文</Name></Item></Response>`)
	encoded, err := Utf8ToGbk(source)
	if err != nil {
		t.Fatal(err)
	}
	var decoded response
	if err := XMLDecode(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 2 || decoded.Items[0].Name != "first" || decoded.Items[1].Name != "中文" {
		t.Fatalf("fallback XML items = %#v", decoded.Items)
	}
}

func TestXMLDecodeFailureDoesNotMutateDestination(t *testing.T) {
	type response struct {
		Items []string `xml:"Item"`
	}
	destination := response{Items: []string{"existing"}}
	if err := XMLDecode([]byte(`<Response><Item>partial</Item><Item>`), &destination); err == nil {
		t.Fatal("malformed XML was accepted")
	}
	if len(destination.Items) != 1 || destination.Items[0] != "existing" {
		t.Fatalf("failed decode mutated destination: %#v", destination.Items)
	}
}
