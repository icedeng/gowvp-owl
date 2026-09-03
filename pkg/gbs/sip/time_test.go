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
	secrecy := 1
	indistinctQuery := 1
	body := string(GetRecordInfoXMLWithFilters("34020000001320000001", 9, 1, 2, RecordInfoQueryFilters{
		FilePath:        `records/<front>&"gate".mp4`,
		Address:         "north&gate",
		Secrecy:         &secrecy,
		Type:            "ALL",
		RecorderID:      `recorder-<main>&"1"`,
		IndistinctQuery: &indistinctQuery,
		StreamNumber:    &streamNumber,
		AlarmMethod:     "2/5",
		AlarmType:       "13",
	}))
	for _, expected := range []string{
		"<FilePath>records/&lt;front&gt;&amp;&#34;gate&#34;.mp4</FilePath>",
		"<Address>north&amp;gate</Address>",
		"<Secrecy>1</Secrecy>",
		"<Type>all</Type>",
		"<RecorderID>recorder-&lt;main&gt;&amp;&#34;1&#34;</RecorderID>",
		"<IndistinctQuery>1</IndistinctQuery>",
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

func TestGetRecordInfoXMLPreservesOpaqueStringWhitespace(t *testing.T) {
	body := string(GetRecordInfoXMLWithFilters("34020000001320000001", 9, 1, 2, RecordInfoQueryFilters{
		FilePath:   ` records/<front>&gate.ps `,
		Address:    " north&gate ",
		RecorderID: " 34020000001320000001 ",
	}))
	for _, expected := range []string{
		"<FilePath> records/&lt;front&gt;&amp;gate.ps </FilePath>",
		"<Address> north&amp;gate </Address>",
		"<RecorderID> 34020000001320000001 </RecorderID>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("RecordInfo query changed opaque string %q: %s", expected, body)
		}
	}
}

func TestGetRecordInfoXMLOmitsLegacyOptionalTimes(t *testing.T) {
	body := string(GetRecordInfoXMLWithFilters("34020000001320000001", 9, 0, 0, RecordInfoQueryFilters{
		OmitStartTime: true,
		OmitEndTime:   true,
	}))
	if strings.Contains(body, "<StartTime>") || strings.Contains(body, "<EndTime>") {
		t.Fatalf("legacy optional RecordInfo times were emitted: %s", body)
	}
	for _, expected := range []string{"<CmdType>RecordInfo</CmdType>", "<Secrecy>0</Secrecy>", "<Type>time</Type>"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("legacy RecordInfo query missing %q: %s", expected, body)
		}
	}
}

func TestGetRecordInfoXMLDefaultsAndPreservesQueryType(t *testing.T) {
	tests := []struct {
		name    string
		filters RecordInfoQueryFilters
		want    string
	}{
		{name: "default", want: "time"},
		{name: "time", filters: RecordInfoQueryFilters{Type: "time"}, want: "time"},
		{name: "alarm", filters: RecordInfoQueryFilters{Type: "alarm"}, want: "alarm"},
		{name: "manual", filters: RecordInfoQueryFilters{Type: "manual"}, want: "manual"},
		{name: "all", filters: RecordInfoQueryFilters{Type: "all"}, want: "all"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := string(GetRecordInfoXMLWithFilters("34020000001320000001", 9, 1, 2, test.filters))
			if !strings.Contains(body, "<Type>"+test.want+"</Type>") || strings.Count(body, "<Type>") != 1 {
				t.Fatalf("RecordInfo query type = %s", body)
			}
		})
	}
}
