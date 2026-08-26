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

func TestGetRecordInfoXMLWith2022Filters(t *testing.T) {
	streamNumber := 2
	body := string(GetRecordInfoXMLWithFilters("34020000001320000001", 9, 1, 2, RecordInfoQueryFilters{
		StreamNumber: &streamNumber,
		AlarmMethod:  "2/5",
		AlarmType:    "13",
	}))
	for _, expected := range []string{
		"<StreamNumber>2</StreamNumber>",
		"<AlarmMethod>2/5</AlarmMethod>",
		"<AlarmType>13</AlarmType>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("RecordInfo query missing %q: %s", expected, body)
		}
	}
	if strings.Index(body, "<StreamNumber>") > strings.Index(body, "</Query>") {
		t.Fatalf("RecordInfo filters are outside Query: %s", body)
	}
}
