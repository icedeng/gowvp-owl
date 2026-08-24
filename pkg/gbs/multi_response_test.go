package gbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type multiResponseFixture struct {
	ID string
}

func TestMultiResponseCollectorOutOfOrderDuplicateAndTotalConflict(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	collector.Start("device:Catalog:1")
	collector.Add("device:Catalog:1", 2, []multiResponseFixture{{ID: "2"}})
	collector.Add("device:Catalog:1", 3, []multiResponseFixture{{ID: "2"}, {ID: "1"}})
	collector.Add("device:Catalog:1", 2, []multiResponseFixture{{ID: "3"}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := collector.Wait(ctx, "device:Catalog:1")
	if !result.Complete || result.Expected != 3 || len(result.Items) != 3 {
		t.Fatalf("result = %+v", result)
	}
}

func TestMultiResponseCollectorReturnsPartialOnTimeout(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	collector.Start("device:RecordInfo:2")
	collector.Add("device:RecordInfo:2", 2, []multiResponseFixture{{ID: "1"}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	result := collector.Wait(ctx, "device:RecordInfo:2")
	if result.Complete || result.Expected != 2 || len(result.Items) != 1 {
		t.Fatalf("partial result = %+v", result)
	}
}

func TestMultiResponseCollectorConcurrentQueriesSameDevice(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	for sn := 1; sn <= 2; sn++ {
		key := fmt.Sprintf("device:Catalog:%d", sn)
		collector.Start(key)
		collector.Add(key, 1, []multiResponseFixture{{ID: fmt.Sprintf("item-%d", sn)}})
	}
	for sn := 1; sn <= 2; sn++ {
		key := fmt.Sprintf("device:Catalog:%d", sn)
		result := collector.Wait(context.Background(), key)
		if len(result.Items) != 1 || result.Items[0].ID != fmt.Sprintf("item-%d", sn) {
			t.Fatalf("query %d result = %+v", sn, result)
		}
	}
}

func TestRecordInfo11DecodesRecorderAndFileSize(t *testing.T) {
	body := []byte(`<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><FilePath>/record/1.ps</FilePath><StartTime>2026-08-25T10:00:00</StartTime><EndTime>2026-08-25T10:01:00</EndTime><RecorderID>` + gb10DeviceID + `</RecorderID><FileSize>1048576</FileSize></Item></RecordList></Response>`)
	var response MessageRecordInfoResponse
	if err := sip.XMLDecode(body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Item) != 1 || response.Item[0].RecorderID != gb10DeviceID || response.Item[0].FileSize != "1048576" {
		t.Fatalf("RecordInfo response = %+v", response)
	}
}
