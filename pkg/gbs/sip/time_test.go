package sip

import (
	"strings"
	"testing"
	"time"
)

func TestGBProtocolTimesUseBeijingIndependentOfProcessTimezone(t *testing.T) {
	instant := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	if got := FormatGBTime(instant, "2006-01-02T15:04:05"); got != "2026-08-25T08:00:00" {
		t.Fatalf("formatted GB time = %s", got)
	}
	parsed, err := ParseGBTime("2006-01-02T15:04:05", "2026-08-25T08:00:00")
	if err != nil || !parsed.Equal(instant) {
		t.Fatalf("parsed GB time = %v, %v", parsed, err)
	}
	body := string(GetRecordInfoXML("34020000001320000001", 9, instant.Unix(), instant.Add(time.Hour).Unix()))
	if !strings.Contains(body, "<StartTime>2026-08-25T08:00:00</StartTime>") ||
		!strings.Contains(body, "<EndTime>2026-08-25T09:00:00</EndTime>") {
		t.Fatalf("RecordInfo query time is not Beijing time: %s", body)
	}
}
