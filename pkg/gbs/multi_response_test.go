package gbs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type multiResponseFixture struct {
	ID string
}

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	done     chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.done
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

func TestMultiResponseCollectorWaitEntryAcceptsNilContext(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	key := "device:Catalog:nil-context"
	entry := collector.Start(key)
	if entry == nil {
		t.Fatal("failed to start multi-response entry")
	}
	collector.Add(key, 1, []multiResponseFixture{{ID: "ok"}})
	result := collector.WaitEntry(nil, key, entry)
	if !result.Complete || len(result.Items) != 1 || result.Items[0].ID != "ok" {
		t.Fatalf("nil-context wait result = %+v", result)
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

func TestMultiResponseCollectorWaitDoesNotConsumeReplacementGeneration(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:Catalog:wrapped"
	collector.Start(key)
	collector.Add(key, 2, []multiResponseFixture{{ID: "old"}})

	ctx := &observedDoneContext{
		Context:  context.Background(),
		observed: make(chan struct{}),
		done:     make(chan struct{}),
	}
	oldDone := make(chan multiResponseResult[multiResponseFixture], 1)
	go func() { oldDone <- collector.Wait(ctx, key) }()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("old waiter did not begin waiting")
	}

	collector.Start(key)
	collector.Add(key, 1, []multiResponseFixture{{ID: "new"}})
	close(ctx.done)

	oldResult := <-oldDone
	if oldResult.Complete || oldResult.Expected != 2 || len(oldResult.Items) != 1 || oldResult.Items[0].ID != "old" {
		t.Fatalf("old waiter consumed replacement generation: %+v", oldResult)
	}
	newResult := collector.Wait(context.Background(), key)
	if !newResult.Complete || newResult.Expected != 1 || len(newResult.Items) != 1 || newResult.Items[0].ID != "new" {
		t.Fatalf("replacement generation was removed: %+v", newResult)
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

func TestMultiResponseCollectorCancelWakesWaiter(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:Catalog:cancel"
	collector.Start(key)
	waitDone := make(chan multiResponseResult[multiResponseFixture], 1)
	go func() {
		waitDone <- collector.Wait(context.Background(), key)
	}()

	collector.Cancel(key)
	select {
	case result := <-waitDone:
		if result.Complete || result.Expected != -1 || result.Err != nil {
			t.Fatalf("cancelled collector result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("collector cancel did not wake waiter")
	}
}

func TestMultiResponseCollectorAbortPrefixIsIsolatedAndRejectsLateChunks(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const (
		abortedKey = "device-a:Catalog:1"
		keptKey    = "device-b:Catalog:2"
	)
	collector.Start(abortedKey)
	collector.Start(keptKey)
	waitDone := make(chan multiResponseResult[multiResponseFixture], 1)
	go func() {
		waitDone <- collector.Wait(context.Background(), abortedKey)
	}()

	if aborted := collector.AbortPrefix("device-a:Catalog:", ErrDeviceNotExist); aborted != 1 {
		t.Fatalf("aborted entries = %d; want 1", aborted)
	}
	select {
	case result := <-waitDone:
		if !errors.Is(result.Err, ErrDeviceNotExist) || result.Complete {
			t.Fatalf("aborted collector result = %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("collector abort did not wake waiter")
	}
	if collector.Add(abortedKey, 1, []multiResponseFixture{{ID: "late"}}) {
		t.Fatal("aborted collector accepted a late response chunk")
	}
	if !collector.Add(keptKey, 1, []multiResponseFixture{{ID: "kept"}}) {
		t.Fatal("unrelated collector entry was aborted")
	}
	result := collector.Wait(context.Background(), keptKey)
	if !result.Complete || result.Err != nil || len(result.Items) != 1 || result.Items[0].ID != "kept" {
		t.Fatalf("unrelated collector result = %+v", result)
	}
}

func TestMultiResponseCollectorAcceptsDeclaredTotalAbovePerChunkLimit(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:Catalog:declared-total"
	collector.Start(key)
	if !collector.Add(key, gbMultiResponseMaxItems+1, []multiResponseFixture{{ID: "first"}}) {
		t.Fatal("associated response was not consumed")
	}

	waitCtx, cancel := context.WithCancel(context.Background())
	cancel()
	defer cancel()
	result := collector.Wait(waitCtx, key)
	if result.Err != nil || result.Complete || result.Expected != gbMultiResponseMaxItems+1 || len(result.Items) != 1 {
		t.Fatalf("declared total result = %+v", result)
	}
}

func TestMultiResponseCollectorAggregatesAcrossPerChunkLimit(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:RecordInfo:multi-chunk-total"
	collector.Start(key)
	first := make([]multiResponseFixture, gbMultiResponseMaxItems)
	for index := range first {
		first[index].ID = fmt.Sprintf("item-%d", index)
	}
	if !collector.Add(key, gbMultiResponseMaxItems+1, first) {
		t.Fatal("first response chunk was not consumed")
	}
	if !collector.Add(key, gbMultiResponseMaxItems+1, []multiResponseFixture{{ID: "item-last"}}) {
		t.Fatal("second response chunk was not consumed")
	}

	result := collector.Wait(context.Background(), key)
	if result.Err != nil || !result.Complete || result.Expected != gbMultiResponseMaxItems+1 || len(result.Items) != gbMultiResponseMaxItems+1 {
		t.Fatalf("multi-chunk total result = %+v", result)
	}
}

func TestMultiResponseCollectorCountsOnlyUniqueItemsAtLimit(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:Catalog:unique-limit"
	collector.Start(key)
	first := make([]multiResponseFixture, gbMultiResponseMaxItems-1)
	for index := range first {
		first[index].ID = fmt.Sprintf("item-%d", index)
	}
	collector.Add(key, gbMultiResponseMaxItems, first)
	collector.Add(key, gbMultiResponseMaxItems, []multiResponseFixture{{ID: first[0].ID}, {ID: "item-last"}})

	result := collector.Wait(context.Background(), key)
	if result.Err != nil || !result.Complete || len(result.Items) != gbMultiResponseMaxItems {
		t.Fatalf("unique limit result = %+v", result)
	}
}

func TestMultiResponseCollectorKeepsIndependentCumulativeSafetyLimit(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:Catalog:cumulative-safety-limit"
	entry := collector.Start(key)
	if entry == nil {
		t.Fatal("collector entry was not created")
	}
	entry.items = make([]multiResponseFixture, gbMultiResponseMaxCollectedItems)
	if !collector.Add(key, gbMultiResponseMaxCollectedItems+1, []multiResponseFixture{{ID: "overflow"}}) {
		t.Fatal("overflowing response chunk was not consumed")
	}

	result := collector.Wait(context.Background(), key)
	if !errors.Is(result.Err, errMultiResponseItemLimit) || result.Complete || result.Expected != gbMultiResponseMaxCollectedItems+1 || len(result.Items) != gbMultiResponseMaxCollectedItems {
		t.Fatalf("cumulative safety result = %+v", result)
	}
}

func TestMultiResponseCollectorRejectsChunksAfterCompletion(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:Catalog:complete"
	collector.Start(key)
	if !collector.Add(key, 1, []multiResponseFixture{{ID: "first"}}) {
		t.Fatal("completing response chunk was not consumed")
	}
	late := make([]multiResponseFixture, gbMultiResponseMaxItems)
	for index := range late {
		late[index].ID = fmt.Sprintf("late-%d", index)
	}
	if collector.Add(key, gbMultiResponseMaxItems+1, late) {
		t.Fatal("completed collector accepted a late response chunk")
	}

	result := collector.Wait(context.Background(), key)
	if result.Err != nil || !result.Complete || result.Expected != 1 || len(result.Items) != 1 || result.Items[0].ID != "first" {
		t.Fatalf("completed result = %+v", result)
	}
}

func TestMultiResponseCollectorMetadataFailureRejectsChunkAtomically(t *testing.T) {
	collector := newMultiResponseCollector(func(item multiResponseFixture) string { return item.ID })
	const key = "device:RecordInfo:metadata-limit"
	metadataErr := errors.New("metadata rejected")
	collector.Start(key)
	if !collector.add(key, 1, []multiResponseFixture{{ID: "must-not-commit"}}, func() error {
		return metadataErr
	}) {
		t.Fatal("associated response was not consumed")
	}
	if collector.Add(key, 1, []multiResponseFixture{{ID: "late"}}) {
		t.Fatal("failed collector accepted a later response chunk")
	}

	result := collector.Wait(context.Background(), key)
	if !errors.Is(result.Err, metadataErr) || result.Complete || len(result.Items) != 0 {
		t.Fatalf("metadata failure result = %+v", result)
	}
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
