package sip

import (
	"context"
	"sync"
	"testing"
	"time"
)

func newTestTransactions() *transacionts {
	return &transacionts{txs: make(map[string]*Transaction), rwm: new(sync.RWMutex)}
}

func TestTransactionOwnershipIsIsolatedPerStore(t *testing.T) {
	first := newTestTransactions()
	second := newTestTransactions()
	firstTX := first.newTX("shared-key", nil)
	secondTX := second.newTX("shared-key", nil)
	t.Cleanup(firstTX.Close)
	t.Cleanup(secondTX.Close)

	firstTX.Close()
	if got := first.getTX("shared-key"); got != nil {
		t.Fatalf("closed transaction remains in first store: %p", got)
	}
	if got := second.getTX("shared-key"); got != secondTX {
		t.Fatalf("closing first store removed second transaction: %p", got)
	}
}

func TestTransactionCloseIsIdempotentAndUnblocksWaiter(t *testing.T) {
	tx := NewTransaction("close-waiter", nil)
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		response, err := tx.GetResponseContext(context.Background())
		if response != nil || err != nil {
			t.Errorf("closed transaction wait = response:%v err:%v", response, err)
		}
	}()
	tx.Close()
	tx.Close()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("transaction close did not unblock response waiter")
	}
}

func TestTransactionStoreCloseReleasesAllTransactions(t *testing.T) {
	store := newTestTransactions()
	first := store.newTX("first", nil)
	second := store.newTX("second", nil)
	store.close()
	if store.getTX("first") != nil || store.getTX("second") != nil {
		t.Fatal("transaction store close retained active transactions")
	}
	select {
	case <-first.done:
	default:
		t.Fatal("first transaction was not closed")
	}
	select {
	case <-second.done:
	default:
		t.Fatal("second transaction was not closed")
	}
}

func TestTransactionStoreDeduplicatesConcurrentKey(t *testing.T) {
	store := newTestTransactions()
	const workers = 32
	transactions := make(chan *Transaction, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			transactions <- store.newTX("same-call-id", nil)
		}()
	}
	wait.Wait()
	close(transactions)
	var expected *Transaction
	for tx := range transactions {
		if expected == nil {
			expected = tx
			continue
		}
		if tx != expected {
			t.Fatalf("same key created multiple transactions: %p and %p", expected, tx)
		}
	}
	if expected == nil {
		t.Fatal("transaction was not created")
	}
	expected.Close()
}

func TestTransactionKeySeparatesRequestsWithinSameDialog(t *testing.T) {
	first := newSignalDigestTestRequest(t, MethodMessage, nil)
	callID := CallID("same-dialog@example")
	first.RemoveHeader("Call-ID")
	first.AppendHeader(&callID)

	second := first.Clone().(*Request)
	second.RemoveHeader("CSeq")
	second.AppendHeader(&CSeq{SeqNo: 2, MethodName: MethodMessage})

	cancel := first.Clone().(*Request)
	cancel.SetMethod(MethodCancel)
	cancel.RemoveHeader("CSeq")
	cancel.AppendHeader(&CSeq{SeqNo: 1, MethodName: MethodCancel})

	firstKey := getTXKey(first)
	secondKey := getTXKey(second)
	cancelKey := getTXKey(cancel)
	if firstKey == secondKey || firstKey == cancelKey || secondKey == cancelKey {
		t.Fatalf("dialog transaction keys collided: %q %q %q", firstKey, secondKey, cancelKey)
	}
	if got := getTXKey(NewResponseFromRequest("", first, 200, "OK", nil)); got != firstKey {
		t.Fatalf("first response key = %q, want %q", got, firstKey)
	}
	if got := getTXKey(NewResponseFromRequest("", second, 200, "OK", nil)); got != secondKey {
		t.Fatalf("second response key = %q, want %q", got, secondKey)
	}
	if got := getTXKey(NewResponseFromRequest("", cancel, 200, "OK", nil)); got != cancelKey {
		t.Fatalf("CANCEL response key = %q, want %q", got, cancelKey)
	}
}

func TestTransactionKeyFallsBackToCallIDWithoutCSeq(t *testing.T) {
	request := newSignalDigestTestRequest(t, MethodMessage, nil)
	request.RemoveHeader("CSeq")
	callID, ok := request.CallID()
	if !ok || callID == nil {
		t.Fatal("test request is missing Call-ID")
	}
	if got := getTXKey(request); got != callID.String() {
		t.Fatalf("legacy transaction key = %q, want %q", got, callID.String())
	}
}
