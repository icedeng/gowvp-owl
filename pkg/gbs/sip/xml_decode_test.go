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

func TestNewGBXMLDecoderAcceptsGB2312WithoutDeclaration(t *testing.T) {
	source := []byte(`<Response><Name>中文名称</Name></Response>`)
	encoded, err := Utf8ToGbk(source)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Name string `xml:"Name"`
	}
	if err := NewGBXMLDecoder(encoded).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "中文名称" {
		t.Fatalf("decoded name = %q", decoded.Name)
	}
}

func TestEncodeGBXMLDocumentProducesDeclaredGB2312Bytes(t *testing.T) {
	encoded, err := EncodeGBXMLDocument([]byte(`<?xml version="1.0" encoding="UTF-8"?><Response><Name>中文名称</Name></Response>`))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Name string `xml:"Name"`
	}
	if err := XMLDecode(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "中文名称" {
		t.Fatalf("decoded encoded document name = %q", decoded.Name)
	}
	utf8Body, err := GbkToUtf8(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(utf8Body); got != `<?xml version="1.0" encoding="GB2312"?><Response><Name>中文名称</Name></Response>` {
		t.Fatalf("encoded document = %q", got)
	}
}
