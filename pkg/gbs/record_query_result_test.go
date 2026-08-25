package gbs

import (
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestRecordQueryItemsResultDistinguishesEmptyResponseFromTimeout(t *testing.T) {
	if items, err := recordQueryItemsResult(multiResponseResult[RecordItem]{Expected: 0, Complete: true}); err != nil || len(items) != 0 {
		t.Fatalf("complete empty RecordInfo result = %v, %v", items, err)
	}
	if _, err := recordQueryItemsResult(multiResponseResult[RecordItem]{Expected: -1}); err == nil {
		t.Fatal("missing RecordInfo response was reported as an empty recording list")
	}
	partial := []RecordItem{{FilePath: "partial.ps"}}
	if items, err := recordQueryItemsResult(multiResponseResult[RecordItem]{Items: partial, Expected: 2}); err != nil || len(items) != 1 {
		t.Fatalf("partial RecordInfo result = %v, %v", items, err)
	}
}

func TestTransRecordListMergesOverlapAndDoesNotCreateMidnightZeroLengthItem(t *testing.T) {
	day := time.Date(2026, 8, 24, 23, 0, 0, 0, sip.GBTimeLocation())
	midnight := time.Date(2026, 8, 25, 0, 0, 0, 0, sip.GBTimeLocation())
	records := transRecordList([][]int64{
		{day.Unix(), day.Add(45 * time.Minute).Unix()},
		{day.Add(30 * time.Minute).Unix(), midnight.Unix()},
		{0},
	})
	if records.DayTotal != 1 || records.TimeNum != 1 || len(records.Data) != 1 || len(records.Data[0].Items) != 1 {
		t.Fatalf("record grouping = %+v", records)
	}
	item := records.Data[0].Items[0]
	if item.Start != day.Unix() || item.End != midnight.Unix()-1 {
		t.Fatalf("merged record = %+v", item)
	}
}

func TestTransRecordItemsSkipsMalformedDeviceTimes(t *testing.T) {
	start := time.Date(2026, 8, 25, 8, 0, 0, 0, sip.GBTimeLocation())
	end := start.Add(time.Hour)
	records := transRecordItems([]RecordItem{
		{StartTime: "invalid", EndTime: end.Format("2006-01-02T15:04:05")},
		{StartTime: start.Format("2006-01-02T15:04:05"), EndTime: end.Format("2006-01-02T15:04:05")},
	}, start.Unix(), end.Unix())
	if records.TimeNum != 1 || len(records.Data) != 1 || len(records.Data[0].Items) != 1 {
		t.Fatalf("record items = %+v", records)
	}
}
