package sip

import "testing"

func TestParamsLookupIsCaseInsensitiveAndPreservesWireName(t *testing.T) {
	params := NewParams().Add("Tag", String{Str: "remote-tag"})
	value, ok := params.Get("tag")
	if !ok || value == nil || value.String() != "remote-tag" {
		t.Fatalf("case-insensitive tag lookup = %#v, %t", value, ok)
	}
	if !params.Has("TAG") {
		t.Fatal("case-insensitive Has did not find Tag")
	}
	if got := params.ToString(';'); got != "Tag=remote-tag" {
		t.Fatalf("serialized params = %q", got)
	}
}

func TestParamsLookupRejectsAmbiguousCaseVariants(t *testing.T) {
	params := NewParams().
		Add("branch", String{Str: "first"}).
		Add("Branch", String{Str: "second"})
	if _, ok := params.Get("BRANCH"); ok {
		t.Fatal("ambiguous case-variant parameters must not produce a value")
	}
	if params.Has("branch") {
		t.Fatal("ambiguous case-variant parameters must not be reported as a single value")
	}
	if got := params.ToString(';'); got != "branch=first;Branch=second" {
		t.Fatalf("ambiguous params serialization = %q", got)
	}
}

func TestParseParamsRejectsDuplicateNamesCaseInsensitively(t *testing.T) {
	for _, source := range []string{
		";branch=first;branch=second",
		";branch=first;Branch=second",
		";branch=first;",
	} {
		t.Run(source, func(t *testing.T) {
			if _, _, err := ParseParams(source, ';', ';', 0, true, true); err == nil {
				t.Fatalf("ParseParams(%q) succeeded", source)
			}
		})
	}
}

func TestParseParamsAcceptsCaseInsensitiveLookup(t *testing.T) {
	params, consumed, err := ParseParams(";Branch=z9hG4bK-test;RPort", ';', ';', 0, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != len(";Branch=z9hG4bK-test;RPort") {
		t.Fatalf("consumed = %d", consumed)
	}
	branch, ok := params.Get("branch")
	if !ok || branch == nil || branch.String() != "z9hG4bK-test" {
		t.Fatalf("branch = %#v, %t", branch, ok)
	}
	if !params.Has("rport") {
		t.Fatal("RPort singleton was not found case-insensitively")
	}
}
