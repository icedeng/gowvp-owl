package gbs

import (
	"context"
	"fmt"
	"sync"
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

func TestMultiResponseCollectorCloseWakesWaitersAndRejectsNewEntries(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	collector.Start("device:Catalog:close")
	waitDone := make(chan multiResponseResult[multiResponseFixture], 1)
	go func() {
		waitDone <- collector.Wait(context.Background(), "device:Catalog:close")
	}()

	collector.Close()
	collector.Close()
	select {
	case result := <-waitDone:
		if result.Complete || result.Expected != -1 || len(result.Items) != 0 {
			t.Fatalf("closed collector result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("collector close did not wake waiter")
	}

	collector.Start("device:Catalog:after-close")
	if collector.Add("device:Catalog:after-close", 1, []multiResponseFixture{{ID: "1"}}) {
		t.Fatal("closed collector accepted a new entry")
	}
}

func TestMultiResponseCollectorAddAndCloseConcurrent(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	collector.Start("device:Catalog:race")
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for item := 0; item < 100; item++ {
				collector.Add("device:Catalog:race", 1000, []multiResponseFixture{{ID: fmt.Sprintf("%d-%d", worker, item)}})
			}
		}(worker)
	}
	collector.Close()
	workers.Wait()
	collector.Close()
}

func TestMultiResponseChunkLimitStartsWith2014(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	ctx := &sip.Context{DeviceID: gb10DeviceID}

	memory.runtime.setGBVersion(GBVersion10)
	if api.multiResponseChunkExceedsLimit(ctx, gbMultiResponseMaxItems+1) {
		t.Fatal("2011 must not inherit the 2014 Annex M item limit")
	}
	memory.runtime.setGBVersion(GBVersion11)
	if !api.multiResponseChunkExceedsLimit(ctx, gbMultiResponseMaxItems+1) {
		t.Fatal("2014 must enforce the Annex M item limit")
	}
}

func TestRecordInfo30DecodesExtendedFileFields(t *testing.T) {
	body := []byte(`<Response><CmdType>RecordInfo</CmdType><SN>3</SN><DeviceID>` + gb10ChannelID + `</DeviceID><Name>camera</Name><SumNum>1</SumNum><RecordList Num="1"><Item><DeviceID>` + gb10ChannelID + `</DeviceID><Name>record</Name><FilePath>/record/1.ps</FilePath><StartTime>2026-08-25T10:00:00</StartTime><EndTime>2026-08-25T10:01:00</EndTime><Secrecy>0</Secrecy><RecorderID>` + gb10DeviceID + `</RecorderID><FileSize>1048576</FileSize><RecordLocation>` + gb10DeviceID + `</RecordLocation><StreamNumber>2</StreamNumber></Item></RecordList></Response>`)
	var response MessageRecordInfoResponse
	if err := sip.XMLDecode(body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Item) != 1 || response.Item[0].RecorderID != gb10DeviceID || response.Item[0].FileSize != "1048576" ||
		response.Item[0].RecordLocation != gb10DeviceID || response.Item[0].StreamNumber == nil || *response.Item[0].StreamNumber != 2 {
		t.Fatalf("RecordInfo response = %+v", response)
	}
}
