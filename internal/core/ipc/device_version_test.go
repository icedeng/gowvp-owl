package ipc

import "testing"

func TestNormalizeManualGBVersion(t *testing.T) {
	tests := map[string]string{
		"":                     "",
		"2011":                 "1.0",
		"1.1":                  "1.1",
		"2011-supplement-2014": "1.1",
		"2016":                 "2.0",
		"2022":                 "3.0",
	}
	for input, want := range tests {
		got, ok := normalizeManualGBVersion(input)
		if !ok || got != want {
			t.Fatalf("normalizeManualGBVersion(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := normalizeManualGBVersion("9.9"); ok {
		t.Fatal("unknown version must be rejected")
	}
}

func TestApplyManualGBVersion(t *testing.T) {
	ext := DeviceExt{GBDeclaredVersion: "1.1"}
	applyManualGBVersion(&ext, "2.0")
	if ext.GBManualVersion != "2.0" || ext.GBEffectiveVersion != "2.0" || ext.GBVersion != "2016" || ext.GBVersionSource != "manual" {
		t.Fatalf("manual version not applied: %+v", ext)
	}

	applyManualGBVersion(&ext, "")
	if ext.GBManualVersion != "" || ext.GBEffectiveVersion != "1.1" || ext.GBVersion != "2014" || ext.GBVersionSource != "header" {
		t.Fatalf("automatic version not restored: %+v", ext)
	}
}
