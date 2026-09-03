package api

import (
	"testing"
	"time"
)

func TestParseAnnexGAuditRange(t *testing.T) {
	begin, end, err := parseAnnexGAuditRange("2026-08-27T10:00:00+08:00", "2026-08-27T11:00:00+08:00")
	if err != nil {
		t.Fatal(err)
	}
	if begin == nil || end == nil || begin.Hour() != 10 || end.Sub(*begin) != time.Hour {
		t.Fatalf("parsed range = %v..%v", begin, end)
	}

	emptyBegin, emptyEnd, err := parseAnnexGAuditRange("", " ")
	if err != nil || emptyBegin != nil || emptyEnd != nil {
		t.Fatalf("empty range = %v..%v, %v", emptyBegin, emptyEnd, err)
	}
	if _, _, err := parseAnnexGAuditRange("2026-08-27 10:00:00", ""); err == nil {
		t.Fatal("non-RFC3339 time was accepted")
	}
}
