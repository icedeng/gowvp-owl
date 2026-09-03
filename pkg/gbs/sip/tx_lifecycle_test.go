package sip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

func TestTransactionIdleTimeoutDependsOnRole(t *testing.T) {
	tx := NewTransaction("role-timeout", nil)
	t.Cleanup(tx.Close)

	if got := tx.idleTimeout(); got != transactionIdleTimeout {
		t.Fatalf("client transaction idle timeout = %s, want %s", got, transactionIdleTimeout)
	}
	if first := tx.beginServerRequest(); !first {
		t.Fatal("first server request was reported as a retransmission")
	}
	if got := tx.idleTimeout(); got != serverTransactionIdleTimeout {
		t.Fatalf("server transaction idle timeout = %s, want %s", got, serverTransactionIdleTimeout)
	}
}

func TestTransactionActivityCannotExtendLifetimePastHardLimit(t *testing.T) {
	const (
		idleTimeout = 20 * time.Millisecond
		maxLifetime = 80 * time.Millisecond
	)
	tx := &Transaction{
		active: make(chan int, 1),
		done:   make(chan struct{}),
	}
	finished := make(chan struct{})
	go func() {
		tx.watchLoop(func() time.Duration { return idleTimeout }, maxLifetime)
		close(finished)
	}()

	ticker := time.NewTicker(idleTimeout / 4)
	defer ticker.Stop()
	timeout := time.NewTimer(maxLifetime * 4)
	defer timeout.Stop()
	startedAt := time.Now()
	for {
		select {
		case <-finished:
			if elapsed := time.Since(startedAt); elapsed < maxLifetime/2 || elapsed > maxLifetime*3 {
				t.Fatalf("transaction lifetime = %s, want hard limit near %s", elapsed, maxLifetime)
			}
			return
		case <-ticker.C:
			tx.markActive(1)
		case <-timeout.C:
			tx.Close()
			<-finished
			t.Fatal("continuous activity extended transaction past hard lifetime")
		}
	}
}

func TestClientTransactionIdleTimeoutCoversRFC3261TimerBF(t *testing.T) {
	const timerBF = 64 * 500 * time.Millisecond
	if transactionIdleTimeout < timerBF {
		t.Fatalf("client transaction idle timeout = %s, shorter than RFC 3261 Timer B/F %s", transactionIdleTimeout, timerBF)
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
	select {
	case <-first.watchDone:
	default:
		t.Fatal("first transaction watcher survived store close")
	}
	select {
	case <-second.watchDone:
	default:
		t.Fatal("second transaction watcher survived store close")
	}
}

func TestTransactionStoreRejectsNewTransactionsAfterClose(t *testing.T) {
	store := newTestTransactions()
	store.close()
	if tx := store.newTXIfOpen("after-close", nil); tx != nil {
		tx.Close()
		t.Fatal("closed transaction store accepted a new transaction")
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

func TestClientTransactionKeySeparatesViaBranches(t *testing.T) {
	first := newSignalDigestTestRequest(t, MethodInvite, nil)
	callID := CallID("forked-dialog@example")
	first.RemoveHeader("Call-ID")
	first.AppendHeader(&callID)
	firstVia, ok := first.ViaHop()
	if !ok || firstVia == nil {
		t.Fatal("first request is missing Via")
	}
	firstVia.Params = NewParams().Add("branch", String{Str: "z9hG4bK-first"})

	second := first.Clone().(*Request)
	secondVia, ok := second.ViaHop()
	if !ok || secondVia == nil {
		t.Fatal("second request is missing Via")
	}
	secondVia.Params = NewParams().Add("branch", String{Str: "z9hG4bK-second"})

	firstKey := getTXKey(first)
	secondKey := getTXKey(second)
	if firstKey == secondKey {
		t.Fatalf("client transactions with different Via branches collided: %q", firstKey)
	}
	if got := getTXKey(NewResponseFromRequest("", first, 200, "OK", nil)); got != firstKey {
		t.Fatalf("first response key = %q, want %q", got, firstKey)
	}
	if got := getTXKey(NewResponseFromRequest("", second, 200, "OK", nil)); got != secondKey {
		t.Fatalf("second response key = %q, want %q", got, secondKey)
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

func TestServerTransactionReplaysResponseWithoutRepeatingHandler(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection
	security, err := NewSignalDigestSecurity(SignalDigestOptions{
		Seed: "shared-seed", Required: false,
		Now: func() time.Time { return time.Date(2026, 8, 27, 5, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	server.SetRequestSecurityResolver(func(*Request) (MessageSecurity, error) { return security, nil })

	var calls atomic.Int32
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	server.Handle(MethodOptions, func(ctx *Context) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		ctx.String(200, "OK")
	})

	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request handler did not start")
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls before first response = %d, want 1", got)
	}
	if got := connection.payloadCount(); got != 0 {
		t.Fatalf("response count before handler completion = %d, want 0", got)
	}

	close(release)
	first := connection.waitPayload(t)
	server.handlerRequest(retransmission)
	second := connection.waitPayload(t)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after retransmission = %d, want 1", got)
	}
	if string(first) != string(second) {
		t.Fatalf("retransmitted response changed:\nfirst=%s\nsecond=%s", first, second)
	}
	if !strings.Contains(string(first), "Date: 2026-08-27T13:00:00") || !strings.Contains(string(first), "Note: Digest nonce=") {
		t.Fatalf("cached response is missing signal Digest: %s", first)
	}

	newTransaction := request.Clone().(*Request)
	via, ok := newTransaction.ViaHop()
	if !ok || via == nil {
		t.Fatal("test request is missing Via")
	}
	via.Params.Add("branch", String{Str: GenerateBranch()})
	newTransaction.SetConnection(connection)
	newTransaction.SetSource(connection.remote)
	newTransaction.SetDestination(connection.local)
	server.handlerRequest(newTransaction)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("new Via transaction handler did not start")
	}
	connection.waitPayload(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls for distinct Via transaction = %d, want 2", got)
	}
}

func TestServerTransactionReplaysResponseOnReconnectedTCPPeer(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	first := newServerTransactionCaptureTCPConnection("192.0.2.30", 41000)
	second := newServerTransactionCaptureTCPConnection("192.0.2.30", 41001)
	otherPeer := newServerTransactionCaptureTCPConnection("192.0.2.31", 41002)
	otherTransport := newServerTransactionCaptureTLSConnection("192.0.2.30", 41003)

	var calls atomic.Int32
	server.Handle(MethodOptions, func(ctx *Context) {
		calls.Add(1)
		if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
			t.Errorf("respond OPTIONS: %v", err)
		}
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	via, ok := request.ViaHop()
	if !ok || via == nil {
		t.Fatal("test request is missing Via")
	}
	via.Transport = "TCP"
	request.SetConnection(first)
	request.SetSource(first.remote)
	request.SetDestination(first.local)
	server.handlerRequest(request)
	initial := first.waitPayload(t)
	if !strings.Contains(string(initial), "SIP/2.0 200 OK") {
		t.Fatalf("initial response = %s", initial)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(second)
	retransmission.SetSource(second.remote)
	retransmission.SetDestination(second.local)
	server.handlerRequest(retransmission)
	replayed := second.waitPayload(t)
	if string(replayed) != string(initial) {
		t.Fatalf("TCP reconnect replay changed:\ninitial=%s\nreplayed=%s", initial, replayed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after TCP reconnect = %d, want 1", got)
	}
	if got := first.payloadCount(); got != 1 {
		t.Fatalf("stale TCP connection response count = %d, want 1", got)
	}

	untrusted := request.Clone().(*Request)
	untrusted.SetConnection(otherPeer)
	untrusted.SetSource(otherPeer.remote)
	untrusted.SetDestination(otherPeer.local)
	server.handlerRequest(untrusted)
	if got := otherPeer.payloadCount(); got != 0 {
		t.Fatalf("cached response replayed to a different peer IP: count = %d", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("different peer re-entered cached transaction handler: calls = %d", got)
	}

	downgrade := request.Clone().(*Request)
	downgrade.SetConnection(otherTransport)
	downgrade.SetSource(otherTransport.remote)
	downgrade.SetDestination(otherTransport.local)
	server.handlerRequest(downgrade)
	rejected := otherTransport.waitPayload(t)
	if string(rejected) == string(initial) || !strings.Contains(string(rejected), "SIP/2.0 400") {
		t.Fatalf("cross-transport request was not rejected without cached response disclosure: %s", rejected)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("transport mismatch re-entered cached transaction handler: calls = %d", got)
	}
}

func TestServerTransactionReplaysResponseOnReconnectedTLSPeer(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5061")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	first := newServerTransactionCaptureTLSConnection("2001:db8::30", 41000)
	second := newServerTransactionCaptureTLSConnection("2001:db8::30", 41001)

	var calls atomic.Int32
	server.Handle(MethodOptions, func(ctx *Context) {
		calls.Add(1)
		if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
			t.Errorf("respond OPTIONS: %v", err)
		}
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	via, ok := request.ViaHop()
	if !ok || via == nil {
		t.Fatal("test request is missing Via")
	}
	via.Transport = "TLS"
	request.SetConnection(first)
	request.SetSource(first.remote)
	request.SetDestination(first.local)
	server.handlerRequest(request)
	initial := first.waitPayload(t)

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(second)
	retransmission.SetSource(second.remote)
	retransmission.SetDestination(second.local)
	server.handlerRequest(retransmission)
	replayed := second.waitPayload(t)
	if string(replayed) != string(initial) {
		t.Fatalf("TLS reconnect replay changed:\ninitial=%s\nreplayed=%s", initial, replayed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after TLS reconnect = %d, want 1", got)
	}
	if got := first.payloadCount(); got != 1 {
		t.Fatalf("stale TLS connection response count = %d, want 1", got)
	}
}

func TestServerTransactionRebindsInFlightResponseToReconnectedStreamPeer(t *testing.T) {
	tests := []struct {
		name      string
		transport string
		serverURI string
		newConn   func(string, int) *serverTransactionCaptureConnection
	}{
		{name: "TCP", transport: "TCP", serverURI: "sip:34020000002000000001@192.0.2.20:5060", newConn: newServerTransactionCaptureTCPConnection},
		{name: "TLS", transport: "TLS", serverURI: "sip:34020000002000000001@192.0.2.20:5061", newConn: newServerTransactionCaptureTLSConnection},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			serverURI, err := ParseSipURI(test.serverURI)
			if err != nil {
				t.Fatal(err)
			}
			server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
			defer server.Close()
			first := test.newConn("192.0.2.30", 41100)
			second := test.newConn("192.0.2.30", 41101)

			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			server.Handle(MethodOptions, func(ctx *Context) {
				calls.Add(1)
				close(started)
				<-release
				if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
					t.Errorf("respond OPTIONS: %v", err)
				}
			})
			request := newSignalDigestTestRequest(t, MethodOptions, nil)
			via, ok := request.ViaHop()
			if !ok || via == nil {
				t.Fatal("test request is missing Via")
			}
			via.Transport = test.transport
			request.SetConnection(first)
			request.SetSource(first.remote)
			request.SetDestination(first.local)
			go server.handlerRequest(request)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("first OPTIONS handler did not start")
			}

			retransmission := request.Clone().(*Request)
			retransmission.SetConnection(second)
			retransmission.SetSource(second.remote)
			retransmission.SetDestination(second.local)
			server.handlerRequest(retransmission)
			close(release)
			response := second.waitPayload(t)
			if !strings.Contains(string(response), "SIP/2.0 200 OK") {
				t.Fatalf("reconnected %s response = %s", test.transport, response)
			}
			if got := first.payloadCount(); got != 0 {
				t.Fatalf("in-flight response wrote stale %s connection: count = %d", test.transport, got)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("handler calls after in-flight %s reconnect = %d, want 1", test.transport, got)
			}
		})
	}
}

type serverTransactionCaptureConnection struct {
	local, remote net.Addr
	network       string
	mu            sync.Mutex
	payloads      [][]byte
	writes        chan []byte
}

type failFirstServerResponseConnection struct {
	*serverTransactionCaptureConnection
	failed atomic.Bool
}

type failNthServerResponseConnection struct {
	*serverTransactionCaptureConnection
	failAt int32
	writes atomic.Int32
}

type failFirstResponseSignSecurity struct {
	calls atomic.Int32
}

func (s *failFirstResponseSignSecurity) Sign(Message) error {
	if s.calls.Add(1) == 1 {
		return errors.New("test SIP response signing failed")
	}
	return nil
}

func (*failFirstResponseSignSecurity) Verify(Message) error { return nil }

type pendingOwnedWriteConnection struct {
	local, remote net.Addr
	writeStarted  chan struct{}
	allowWrite    chan struct{}
	closed        chan struct{}
	startOnce     sync.Once
	closeOnce     sync.Once
}

type cancelDeadlineConnection struct {
	local, remote     net.Addr
	cancelHadDeadline atomic.Bool
	cancelDeadline    atomic.Int64
}

func newCancelDeadlineConnection() *cancelDeadlineConnection {
	return &cancelDeadlineConnection{
		local:  &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	}
}

func (c *cancelDeadlineConnection) Read([]byte) (int, error)          { return 0, io.EOF }
func (c *cancelDeadlineConnection) Write(payload []byte) (int, error) { return len(payload), nil }
func (c *cancelDeadlineConnection) Close() error                      { return nil }
func (c *cancelDeadlineConnection) LocalAddr() net.Addr               { return c.local }
func (c *cancelDeadlineConnection) RemoteAddr() net.Addr              { return c.remote }
func (c *cancelDeadlineConnection) SetDeadline(time.Time) error       { return nil }
func (c *cancelDeadlineConnection) SetReadDeadline(time.Time) error   { return nil }
func (c *cancelDeadlineConnection) SetWriteDeadline(time.Time) error  { return nil }
func (c *cancelDeadlineConnection) Network() string                   { return "tcp" }
func (c *cancelDeadlineConnection) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, c.remote, io.EOF
}
func (c *cancelDeadlineConnection) WriteTo(payload []byte, _ net.Addr) (int, error) {
	return len(payload), nil
}
func (c *cancelDeadlineConnection) writeToContext(ctx context.Context, payload []byte, _ net.Addr) (int, error) {
	if strings.HasPrefix(string(payload), MethodCancel+" ") {
		deadline, hasDeadline := ctx.Deadline()
		c.cancelHadDeadline.Store(hasDeadline)
		if !hasDeadline {
			return 0, errors.New("CANCEL write context has no deadline")
		}
		c.cancelDeadline.Store(deadline.UnixNano())
	}
	return len(payload), nil
}

func newPendingOwnedWriteConnection() *pendingOwnedWriteConnection {
	return &pendingOwnedWriteConnection{
		local:        &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 42001},
		remote:       &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		writeStarted: make(chan struct{}),
		allowWrite:   make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

func (c *pendingOwnedWriteConnection) Read([]byte) (int, error) { return 0, io.EOF }
func (c *pendingOwnedWriteConnection) Write(payload []byte) (int, error) {
	return c.WriteTo(payload, c.remote)
}
func (c *pendingOwnedWriteConnection) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}
func (c *pendingOwnedWriteConnection) LocalAddr() net.Addr  { return c.local }
func (c *pendingOwnedWriteConnection) RemoteAddr() net.Addr { return c.remote }
func (c *pendingOwnedWriteConnection) SetDeadline(time.Time) error {
	return nil
}
func (c *pendingOwnedWriteConnection) SetReadDeadline(time.Time) error {
	return nil
}
func (c *pendingOwnedWriteConnection) SetWriteDeadline(time.Time) error {
	return nil
}
func (c *pendingOwnedWriteConnection) Network() string { return "tcp" }
func (c *pendingOwnedWriteConnection) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, c.remote, io.EOF
}
func (c *pendingOwnedWriteConnection) WriteTo(payload []byte, _ net.Addr) (int, error) {
	c.startOnce.Do(func() { close(c.writeStarted) })
	select {
	case <-c.allowWrite:
		return len(payload), nil
	case <-c.closed:
		return 0, net.ErrClosed
	}
}

func TestOwnedConnectionFinalResponseWaitsForRequestWriteCompletion(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	t.Cleanup(server.Close)
	connection := newPendingOwnedWriteConnection()
	request := newSignalDigestTestRequest(t, MethodMessage, []byte("oversized request"))
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())

	type requestResult struct {
		tx  *Transaction
		err error
	}
	result := make(chan requestResult, 1)
	go func() {
		tx, requestErr := server.RequestWithSecurityContextOwnedConnection(t.Context(), request, nil)
		result <- requestResult{tx: tx, err: requestErr}
	}()
	select {
	case <-connection.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("owned request write did not start")
	}
	tx := server.getTX(getTXKey(request))
	if tx == nil {
		t.Fatal("owned request transaction was not registered before write")
	}
	tx.receiveResponse(NewResponseFromRequest("", request, 200, "OK", nil))
	select {
	case <-connection.closed:
		t.Fatal("final response closed owned connection before request write completed")
	default:
	}
	close(connection.allowWrite)
	select {
	case actual := <-result:
		if actual.err != nil || actual.tx != tx {
			t.Fatalf("owned request result = tx %p, err %v; want tx %p", actual.tx, actual.err, tx)
		}
	case <-time.After(time.Second):
		t.Fatal("owned request write did not complete")
	}
	select {
	case <-connection.closed:
	case <-time.After(time.Second):
		t.Fatal("owned connection was not released after final response and request write")
	}
}

func (c *failFirstServerResponseConnection) WriteTo(payload []byte, destination net.Addr) (int, error) {
	if c.failed.CompareAndSwap(false, true) {
		return 0, errors.New("test SIP response write failed")
	}
	return c.serverTransactionCaptureConnection.WriteTo(payload, destination)
}

func (c *failNthServerResponseConnection) WriteTo(payload []byte, destination net.Addr) (int, error) {
	if c.writes.Add(1) == c.failAt {
		return 0, errors.New("test SIP response write failed")
	}
	return c.serverTransactionCaptureConnection.WriteTo(payload, destination)
}

func TestServerTransactionRetriesHandlerAfterResponseWriteFailure(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := &failFirstServerResponseConnection{serverTransactionCaptureConnection: newServerTransactionCaptureConnection()}
	server.udpConn = connection

	var calls atomic.Int32
	handled := make(chan struct{}, 2)
	server.Handle(MethodOptions, func(ctx *Context) {
		calls.Add(1)
		_ = ctx.RespondString(200, "OK")
		handled <- struct{}{}
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("first request handler did not finish")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after failed response = %d, want 1", got)
	}
	if got := connection.payloadCount(); got != 0 {
		t.Fatalf("captured responses after failed write = %d, want 0", got)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("retried request handler did not finish")
	}
	first := connection.waitPayload(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after successful retry = %d, want 2", got)
	}

	server.handlerRequest(retransmission)
	second := connection.waitPayload(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after cached replay = %d, want 2", got)
	}
	if string(first) != string(second) {
		t.Fatalf("retried cached response changed:\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestServerTransactionDefersRetryUntilFailedHandlerReturns(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := &failFirstServerResponseConnection{serverTransactionCaptureConnection: newServerTransactionCaptureConnection()}
	server.udpConn = connection

	var calls atomic.Int32
	responseFailed := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondHandled := make(chan struct{})
	server.Handle(MethodOptions, func(ctx *Context) {
		call := calls.Add(1)
		if err := ctx.RespondString(http.StatusOK, "OK"); call == 1 {
			if err == nil {
				t.Fatal("first response write unexpectedly succeeded")
			}
			close(responseFailed)
			<-releaseFirst
			return
		} else if err != nil {
			t.Errorf("retried response write failed: %v", err)
		}
		close(secondHandled)
	})

	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	select {
	case <-responseFailed:
	case <-time.After(time.Second):
		t.Fatal("first response failure was not observed")
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	if got := calls.Load(); got != 1 {
		t.Fatalf("retransmission entered while failed handler was active: calls = %d", got)
	}
	if got := connection.payloadCount(); got != 0 {
		t.Fatalf("response emitted while failed handler was active: count = %d", got)
	}

	close(releaseFirst)
	server.requestWG.Wait()
	server.handlerRequest(retransmission)
	select {
	case <-secondHandled:
	case <-time.After(time.Second):
		t.Fatal("retransmission did not enter after failed handler returned")
	}
	response := connection.waitPayload(t)
	if !strings.Contains(string(response), "SIP/2.0 200 OK") {
		t.Fatalf("retried response = %s", response)
	}
	if strings.Contains(string(response), "500 Internal Server Error") {
		t.Fatalf("failed handler return emitted a contradictory response: %s", response)
	}
}

func TestServerTransactionCachesHandlerPanicResponse(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection

	var calls atomic.Int32
	server.Handle(MethodOptions, func(*Context) {
		calls.Add(1)
		panic("test handler panic")
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	first := connection.waitPayload(t)
	if !strings.Contains(string(first), "SIP/2.0 500 Internal Server Error") {
		t.Fatalf("panic response = %s", first)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	second := connection.waitPayload(t)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after cached panic response = %d, want 1", got)
	}
	if string(first) != string(second) {
		t.Fatalf("cached panic response changed:\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestServerTransactionRejectsResponseAfterFinalCommit(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection

	secondResult := make(chan error, 1)
	server.Handle(MethodOptions, func(ctx *Context) {
		if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
			secondResult <- fmt.Errorf("first final response failed: %w", err)
			return
		}
		secondResult <- ctx.RespondString(http.StatusInternalServerError, "Internal Server Error")
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)

	response := connection.waitPayload(t)
	if !strings.Contains(string(response), "SIP/2.0 200 OK") {
		t.Fatalf("committed response = %s", response)
	}
	select {
	case err := <-secondResult:
		if err == nil || !strings.Contains(err.Error(), "already committed") {
			t.Fatalf("second response error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not attempt the second response")
	}
	if got := connection.payloadCount(); got != 1 {
		t.Fatalf("responses after final commit = %d, want 1", got)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	replayed := connection.waitPayload(t)
	if string(replayed) != string(response) {
		t.Fatalf("replayed final changed:\nfirst=%s\nreplayed=%s", response, replayed)
	}
}

func TestServerHandlerPanicAfterProvisionalResponseCommitsFinalError(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection

	var calls atomic.Int32
	server.Handle(MethodInvite, func(ctx *Context) {
		calls.Add(1)
		if err := ctx.RespondString(100, "Trying"); err != nil {
			t.Fatal(err)
		}
		panic("panic after provisional response")
	})
	request := newSignalDigestTestRequest(t, MethodInvite, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	provisional := connection.waitPayload(t)
	final := connection.waitPayload(t)
	if !strings.Contains(string(provisional), "SIP/2.0 100 Trying") ||
		!strings.Contains(string(final), "SIP/2.0 500 Internal Server Error") {
		t.Fatalf("panic responses = provisional:%s final:%s", provisional, final)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	replayed := connection.waitPayload(t)
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls after final panic response = %d, want 1", got)
	}
	if string(final) != string(replayed) {
		t.Fatalf("replayed panic final changed:\nfinal=%s\nreplayed=%s", final, replayed)
	}
}

func TestServerHandlerWithoutFinalResponseCommitsStableError(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection

	var calls atomic.Int32
	server.Handle(MethodOptions, func(*Context) { calls.Add(1) })
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	first := connection.waitPayload(t)
	if !strings.Contains(string(first), "SIP/2.0 500 Internal Server Error") {
		t.Fatalf("incomplete handler response = %s", first)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	second := connection.waitPayload(t)
	if got := calls.Load(); got != 1 {
		t.Fatalf("incomplete handler calls after replay = %d, want 1", got)
	}
	if string(first) != string(second) {
		t.Fatalf("incomplete handler final changed:\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestServerHandlerReturningAfterProvisionalCommitsFinalError(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection

	server.Handle(MethodInvite, func(ctx *Context) {
		if err := ctx.RespondString(100, "Trying"); err != nil {
			t.Fatal(err)
		}
	})
	request := newSignalDigestTestRequest(t, MethodInvite, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	provisional := connection.waitPayload(t)
	final := connection.waitPayload(t)
	if !strings.Contains(string(provisional), "SIP/2.0 100 Trying") ||
		!strings.Contains(string(final), "SIP/2.0 500 Internal Server Error") {
		t.Fatalf("incomplete INVITE responses = provisional:%s final:%s", provisional, final)
	}
}

func TestServerTransactionRetriesAfterFinalResponseFailsFollowingProvisional(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := &failNthServerResponseConnection{
		serverTransactionCaptureConnection: newServerTransactionCaptureConnection(),
		failAt:                             2,
	}
	server.udpConn = connection

	var calls atomic.Int32
	handled := make(chan struct{}, 2)
	server.Handle(MethodInvite, func(ctx *Context) {
		call := calls.Add(1)
		if call == 1 {
			if err := ctx.RespondString(100, "Trying"); err != nil {
				t.Fatal(err)
			}
		}
		_ = ctx.RespondString(http.StatusOK, "OK")
		handled <- struct{}{}
	})
	request := newSignalDigestTestRequest(t, MethodInvite, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	provisional := connection.waitPayload(t)
	if !strings.Contains(string(provisional), "SIP/2.0 100 Trying") {
		t.Fatalf("provisional response = %s", provisional)
	}
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("first handler did not finish")
	}
	server.requestWG.Wait()

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	final := connection.waitPayload(t)
	if !strings.Contains(string(final), "SIP/2.0 200 OK") {
		t.Fatalf("retried final response = %s", final)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after failed final response = %d, want 2", got)
	}

	server.handlerRequest(retransmission)
	replayed := connection.waitPayload(t)
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after final replay = %d, want 2", got)
	}
	if string(final) != string(replayed) {
		t.Fatalf("cached final response changed:\nfinal=%s\nreplayed=%s", final, replayed)
	}
}

func TestServerTransactionRetriesHandlerPanicAfterResponseWriteFailure(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := &failFirstServerResponseConnection{serverTransactionCaptureConnection: newServerTransactionCaptureConnection()}
	server.udpConn = connection

	var calls atomic.Int32
	handled := make(chan struct{}, 2)
	server.Handle(MethodOptions, func(*Context) {
		calls.Add(1)
		handled <- struct{}{}
		panic("test handler panic")
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("first panicking handler did not run")
	}
	server.requestWG.Wait()
	if got := connection.payloadCount(); got != 0 {
		t.Fatalf("captured panic responses after failed write = %d, want 0", got)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	response := connection.waitPayload(t)
	if !strings.Contains(string(response), "SIP/2.0 500 Internal Server Error") {
		t.Fatalf("retried panic response = %s", response)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after panic response retry = %d, want 2", got)
	}
}

func TestServerTransactionRetriesHandlerPanicAfterResponseSigningFailure(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection
	security := &failFirstResponseSignSecurity{}

	var calls atomic.Int32
	handled := make(chan struct{}, 2)
	server.Handle(MethodOptions, func(ctx *Context) {
		calls.Add(1)
		ctx.Tx.SetMessageSecurity(security)
		handled <- struct{}{}
		panic("test handler panic")
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("first panicking handler did not run")
	}
	server.requestWG.Wait()
	if got := connection.payloadCount(); got != 0 {
		t.Fatalf("captured panic responses after signing failure = %d, want 0", got)
	}

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	response := connection.waitPayload(t)
	if !strings.Contains(string(response), "SIP/2.0 500 Internal Server Error") {
		t.Fatalf("retried signed panic response = %s", response)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls after panic signing retry = %d, want 2", got)
	}
	if got := security.calls.Load(); got != 2 {
		t.Fatalf("response signing calls = %d, want 2", got)
	}
}

func TestServerHandlerPanicDoesNotOverrideWrittenResponse(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection

	server.Handle(MethodOptions, func(ctx *Context) {
		if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
			t.Fatal(err)
		}
		panic("panic after response")
	})
	request := newSignalDigestTestRequest(t, MethodOptions, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	response := connection.waitPayload(t)
	if !strings.Contains(string(response), "SIP/2.0 200 OK") {
		t.Fatalf("written response = %s", response)
	}
	server.requestWG.Wait()
	if got := connection.payloadCount(); got != 1 {
		t.Fatalf("responses after post-response panic = %d, want 1", got)
	}
}

func TestServerACKHandlerPanicAllowsRetransmissionWithoutResponse(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection

	var calls atomic.Int32
	handled := make(chan struct{}, 2)
	server.Handle(MethodACK, func(*Context) {
		calls.Add(1)
		handled <- struct{}{}
		panic("test ACK handler panic")
	})
	request := newSignalDigestTestRequest(t, MethodACK, nil)
	request.SetConnection(connection)
	request.SetSource(connection.remote)
	request.SetDestination(connection.local)
	server.handlerRequest(request)
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("first ACK handler did not run")
	}
	server.requestWG.Wait()

	retransmission := request.Clone().(*Request)
	retransmission.SetConnection(connection)
	retransmission.SetSource(connection.remote)
	retransmission.SetDestination(connection.local)
	server.handlerRequest(retransmission)
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("retransmitted ACK handler did not run")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("ACK handler calls = %d, want 2", got)
	}
	if got := connection.payloadCount(); got != 0 {
		t.Fatalf("ACK panic emitted %d SIP responses, want 0", got)
	}
}

func newServerTransactionCaptureConnection() *serverTransactionCaptureConnection {
	return &serverTransactionCaptureConnection{
		local:  &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote: &net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
		writes: make(chan []byte, 8),
	}
}

func newServerTransactionCaptureTCPConnection(remoteIP string, remotePort int) *serverTransactionCaptureConnection {
	return &serverTransactionCaptureConnection{
		local:   &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060},
		remote:  &net.TCPAddr{IP: net.ParseIP(remoteIP), Port: remotePort},
		network: "tcp",
		writes:  make(chan []byte, 8),
	}
}

func newServerTransactionCaptureTLSConnection(remoteIP string, remotePort int) *serverTransactionCaptureConnection {
	connection := newServerTransactionCaptureTCPConnection(remoteIP, remotePort)
	connection.network = "tls"
	return connection
}

func (c *serverTransactionCaptureConnection) Read([]byte) (int, error) { return 0, io.EOF }
func (c *serverTransactionCaptureConnection) Write(payload []byte) (int, error) {
	return c.WriteTo(payload, c.remote)
}
func (c *serverTransactionCaptureConnection) Close() error                     { return nil }
func (c *serverTransactionCaptureConnection) LocalAddr() net.Addr              { return c.local }
func (c *serverTransactionCaptureConnection) RemoteAddr() net.Addr             { return c.remote }
func (c *serverTransactionCaptureConnection) SetDeadline(time.Time) error      { return nil }
func (c *serverTransactionCaptureConnection) SetReadDeadline(time.Time) error  { return nil }
func (c *serverTransactionCaptureConnection) SetWriteDeadline(time.Time) error { return nil }
func (c *serverTransactionCaptureConnection) Network() string {
	if c.network != "" {
		return c.network
	}
	return "udp"
}
func (c *serverTransactionCaptureConnection) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, c.remote, io.EOF
}
func (c *serverTransactionCaptureConnection) WriteTo(payload []byte, _ net.Addr) (int, error) {
	copyPayload := append([]byte(nil), payload...)
	c.mu.Lock()
	c.payloads = append(c.payloads, copyPayload)
	c.mu.Unlock()
	c.writes <- copyPayload
	return len(payload), nil
}
func (c *serverTransactionCaptureConnection) payloadCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.payloads)
}
func (c *serverTransactionCaptureConnection) waitPayload(t *testing.T) []byte {
	t.Helper()
	select {
	case payload := <-c.writes:
		return payload
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SIP response")
		return nil
	}
}

func TestHandlerResponseValidatesHeadersAndTransactionBinding(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	request := newSignalDigestTestRequest(t, MethodMessage, nil)
	request.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	tx := server.txs.newTX(getTXKey(request), nil)
	tx.bindResponse(request)

	newResponse := func() *Response {
		response := NewResponseFromRequest("", request, 200, "OK", nil)
		response.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
		return response
	}
	assertDiscarded := func(name string, response *Response) {
		t.Helper()
		server.handlerResponse(response)
		select {
		case got := <-tx.resp:
			t.Fatalf("%s response was delivered: %s", name, got)
		default:
		}
	}
	for _, test := range []struct {
		name   string
		mutate func(*Response)
	}{
		{name: "duplicate From", mutate: func(response *Response) {
			from, _ := response.From()
			response.AppendHeader(from.Clone())
		}},
		{name: "duplicate To", mutate: func(response *Response) {
			to, _ := response.To()
			response.AppendHeader(to.Clone())
		}},
		{name: "duplicate Call-ID", mutate: func(response *Response) {
			callID, _ := response.CallID()
			response.AppendHeader(callID.Clone())
		}},
		{name: "duplicate CSeq", mutate: func(response *Response) {
			cseq, _ := response.CSeq()
			response.AppendHeader(cseq.Clone())
		}},
		{name: "wrong SIP version", mutate: func(response *Response) {
			response.SetSipVersion("SIP/1.0")
		}},
		{name: "invalid status code", mutate: func(response *Response) {
			response.SetStatusCode(700)
		}},
		{name: "wrong From tag", mutate: func(response *Response) {
			from, _ := response.From()
			from.Params = NewParams().Add("tag", String{Str: "forged-from-tag"})
		}},
		{name: "missing From tag", mutate: func(response *Response) {
			from, _ := response.From()
			from.Params = NewParams()
		}},
		{name: "duplicate From tag", mutate: func(response *Response) {
			from, _ := response.From()
			from.Params.Add("Tag", String{Str: "forged-from-tag"})
		}},
		{name: "wrong Via protocol", mutate: func(response *Response) {
			via, _ := response.ViaHop()
			via.ProtocolName = "HTTP"
		}},
		{name: "wrong Via version", mutate: func(response *Response) {
			via, _ := response.ViaHop()
			via.ProtocolVersion = "1.0"
		}},
		{name: "duplicate Via branch", mutate: func(response *Response) {
			via, _ := response.ViaHop()
			via.Params.Add("Branch", String{Str: sipViaBranchValue(via)})
		}},
	} {
		response := newResponse()
		test.mutate(response)
		assertDiscarded(test.name, response)
	}

	wrongBranch := newResponse()
	via, ok := wrongBranch.ViaHop()
	if !ok || via == nil {
		t.Fatal("response Via is unavailable")
	}
	via.Params.Add("branch", String{Str: "z9hG4bK-forged"})
	assertDiscarded("wrong Via branch", wrongBranch)

	missingBranch := newResponse()
	via, ok = missingBranch.ViaHop()
	if !ok || via == nil {
		t.Fatal("response Via is unavailable")
	}
	via.Params = NewParams()
	assertDiscarded("missing Via branch", missingBranch)

	wrongSource := newResponse()
	wrongSource.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.31"), Port: 5060})
	assertDiscarded("wrong source", wrongSource)

	valid := newResponse()
	server.handlerResponse(valid)
	select {
	case got := <-tx.resp:
		if got != valid {
			t.Fatalf("delivered response = %p, want %p", got, valid)
		}
	case <-time.After(time.Second):
		t.Fatal("valid bound response was not delivered")
	}
}

func TestRequestGeneratesMissingViaBranchAndBindsResponse(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn: client, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	})
	request := newSignalDigestTestRequest(t, MethodMessage, nil)
	via, ok := request.ViaHop()
	if !ok || via == nil {
		t.Fatal("request Via is unavailable")
	}
	via.Params = NewParams()
	request.SetConnection(connection)
	request.SetSource(connection.LocalAddr())
	request.SetDestination(connection.RemoteAddr())

	result := make(chan struct {
		tx  *Transaction
		err error
	}, 1)
	go func() {
		tx, requestErr := server.Request(request)
		result <- struct {
			tx  *Transaction
			err error
		}{tx: tx, err: requestErr}
	}()
	buffer := make([]byte, 8192)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Read(buffer); err != nil {
		t.Fatal(err)
	}
	sent := <-result
	if sent.err != nil {
		t.Fatal(sent.err)
	}
	branch := sipViaBranchValue(via)
	if branch == "" {
		t.Fatal("outbound request Via branch was not generated")
	}
	response := NewResponseFromRequest("", request, 200, "OK", nil)
	response.SetSource(connection.RemoteAddr())
	if !sent.tx.acceptsResponse(response) {
		t.Fatal("generated Via branch was not retained in response binding")
	}
}

func TestRequestUsesCaseInsensitiveViaParameters(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn: client, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	})
	request := newSignalDigestTestRequest(t, MethodMessage, nil)
	via, _ := request.ViaHop()
	via.Params = NewParams().Add("Branch", String{Str: "z9hG4bK-mixed-case"}).Add("RPort", nil)
	request.SetConnection(connection)
	request.SetDestination(connection.RemoteAddr())

	result := make(chan error, 1)
	go func() {
		_, requestErr := server.Request(request)
		result <- requestErr
	}()
	buffer := make([]byte, 8192)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	payload := string(buffer[:n])
	if strings.Count(strings.ToLower(payload), ";branch=") != 1 || strings.Count(strings.ToLower(payload), ";rport") != 1 {
		t.Fatalf("case-insensitive Via parameters were duplicated: %s", payload)
	}
}

func TestRequestRejectsDuplicateViaBranchParameters(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	request := newSignalDigestTestRequest(t, MethodMessage, nil)
	via, _ := request.ViaHop()
	via.Params.Add("Branch", String{Str: "z9hG4bK-duplicate"})
	request.SetConnection(NewTCPConnection(client))
	request.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	if _, err := server.Request(request); err == nil || !strings.Contains(err.Error(), "multiple branch") {
		t.Fatalf("duplicate Via branch error = %v", err)
	}
}

func TestTransactionCancelInvitePreservesOriginalTransaction(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn: client, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	})
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())

	sentInvite := make(chan struct {
		tx  *Transaction
		err error
	}, 1)
	go func() {
		tx, requestErr := server.Request(invite)
		sentInvite <- struct {
			tx  *Transaction
			err error
		}{tx: tx, err: requestErr}
	}()
	buffer := make([]byte, 8192)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	invitePayload := string(buffer[:n])
	sent := <-sentInvite
	if sent.err != nil {
		t.Fatal(sent.err)
	}

	cancelResult := make(chan error, 1)
	go func() {
		_, cancelErr := sent.tx.CancelInvite()
		cancelResult <- cancelErr
	}()
	n, err = peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-cancelResult; err != nil {
		t.Fatal(err)
	}
	cancelPayload := string(buffer[:n])
	if !strings.HasPrefix(cancelPayload, "CANCEL ") {
		t.Fatalf("CANCEL payload = %s", cancelPayload)
	}
	if headerValue(cancelPayload, "Content-Length") != "0" {
		t.Fatalf("CANCEL Content-Length = %q", headerValue(cancelPayload, "Content-Length"))
	}
	inviteVia := headerValue(invitePayload, "Via")
	cancelVia := headerValue(cancelPayload, "Via")
	if inviteVia == "" || cancelVia != inviteVia {
		t.Fatalf("CANCEL Via = %q, INVITE Via = %q", cancelVia, inviteVia)
	}
	inviteCSeq := headerValue(invitePayload, "CSeq")
	cancelCSeq := headerValue(cancelPayload, "CSeq")
	inviteNumber := strings.Fields(inviteCSeq)[0]
	if cancelCSeq != inviteNumber+" CANCEL" {
		t.Fatalf("CANCEL CSeq = %q, INVITE CSeq = %q", cancelCSeq, inviteCSeq)
	}
}

func TestTransactionCancelInviteUsesBoundedWriteContext(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	t.Cleanup(server.Close)
	connection := newCancelDeadlineConnection()
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())

	tx, err := server.Request(invite)
	if err != nil {
		t.Fatal(err)
	}
	cancelTX, err := tx.CancelInvite()
	if err != nil {
		t.Fatalf("CancelInvite() error = %v", err)
	}
	if cancelTX == nil {
		t.Fatal("CancelInvite() returned nil transaction")
	}
	t.Cleanup(cancelTX.Close)
	if !connection.cancelHadDeadline.Load() {
		t.Fatal("CANCEL write did not receive a bounded context")
	}
}

func TestTransactionDetachedCancelClosesAfterFinalResponse(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	t.Cleanup(server.Close)
	connection := newCancelDeadlineConnection()
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())

	inviteTX, err := server.Request(invite)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := inviteTX.CancelInviteDetached()
	if err != nil {
		t.Fatalf("CancelInviteDetached() error = %v", err)
	}
	if !sent {
		t.Fatal("CancelInviteDetached() did not send CANCEL")
	}
	cancelRequest, err := NewCancelRequestFromInviteChecked(invite)
	if err != nil {
		t.Fatal(err)
	}
	cancelTX := server.getTX(getTXKey(cancelRequest))
	if cancelTX == nil {
		t.Fatal("detached CANCEL transaction was not registered")
	}
	response := NewResponseFromRequest("", cancelRequest, 200, "OK", nil)
	response.SetSource(connection.RemoteAddr())
	response.SetConnection(connection)
	server.handlerResponse(response)
	select {
	case <-cancelTX.watchDone:
	case <-time.After(time.Second):
		t.Fatal("detached CANCEL transaction did not close after final response")
	}
	if got := server.getTX(getTXKey(cancelRequest)); got != nil {
		t.Fatalf("detached CANCEL transaction remains registered: %p", got)
	}
}

func TestDetachedInviteDiscardsProvisionalResponses(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	t.Cleanup(server.Close)
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())

	inviteTX, err := server.Request(invite)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.waitPayload(t)
	if sent, err := inviteTX.CancelInviteDetached(); err != nil || !sent {
		t.Fatalf("CancelInviteDetached() = %v, %v; want true, nil", sent, err)
	}
	_ = connection.waitPayload(t)

	response := NewResponseFromRequest("", invite, 180, "Ringing", nil)
	response.SetSource(connection.RemoteAddr())
	response.SetConnection(connection)
	for i := 0; i < cap(inviteTX.resp)+2; i++ {
		delivered := make(chan struct{})
		go func() {
			server.handlerResponse(response)
			close(delivered)
		}()
		select {
		case <-delivered:
		case <-time.After(time.Second):
			t.Fatalf("detached provisional response %d blocked the SIP handler", i)
		}
	}
	select {
	case provisional := <-inviteTX.resp:
		t.Fatalf("detached provisional response reached business queue: %s", provisional.StartLine())
	default:
	}
}

func TestInviteDiscardsProvisionalFloodBeforeResponseWaiter(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	t.Cleanup(server.Close)
	connection := newServerTransactionCaptureConnection()
	server.udpConn = connection
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())

	inviteTX, err := server.Request(invite)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.waitPayload(t)
	provisional := NewResponseFromRequest("", invite, 180, "Ringing", nil)
	provisional.SetSource(connection.RemoteAddr())
	provisional.SetConnection(connection)
	for i := 0; i < cap(inviteTX.resp)+2; i++ {
		delivered := make(chan struct{})
		go func() {
			server.handlerResponse(provisional)
			close(delivered)
		}()
		select {
		case <-delivered:
		case <-time.After(time.Second):
			t.Fatalf("provisional response %d blocked before waiter started", i)
		}
	}
	select {
	case response := <-inviteTX.resp:
		t.Fatalf("provisional response reached final-response queue: %s", response.StartLine())
	default:
	}

	final := NewResponseFromRequest("", invite, 200, "OK", []byte("answer"))
	final.SetSource(connection.RemoteAddr())
	final.SetConnection(connection)
	server.handlerResponse(final)
	got, err := inviteTX.GetResponseContext(t.Context())
	if err != nil || got != final {
		t.Fatalf("final INVITE response = %p, %v; want %p", got, err, final)
	}
}

func TestTransactionDetachedCancelContextPreservesCallerDeadline(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	t.Cleanup(server.Close)
	connection := newCancelDeadlineConnection()
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())

	inviteTX, err := server.Request(invite)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	expectedDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("test context has no deadline")
	}
	sent, err := inviteTX.CancelInviteDetachedContext(ctx)
	if err != nil {
		t.Fatalf("CancelInviteDetachedContext() error = %v", err)
	}
	if !sent {
		t.Fatal("CancelInviteDetachedContext() did not send CANCEL")
	}
	deadline := time.Unix(0, connection.cancelDeadline.Load())
	if !deadline.Equal(expectedDeadline) {
		t.Fatalf("CANCEL deadline = %s; want caller deadline %s", deadline, expectedDeadline)
	}
}

func TestHandlerResponseAutomaticallyAcknowledgesNon2xxInvite(t *testing.T) {
	serverURI, err := ParseSipURI("sip:34020000002000000001@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(&Address{URI: &serverURI, Params: NewParams()})
	defer server.Close()
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	connection := NewTCPConnection(&sipTestTCPConn{
		Conn: client, local: &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 41000},
		remote: &net.TCPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060},
	})
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetDestination(connection.RemoteAddr())

	sentInvite := make(chan *Transaction, 1)
	go func() {
		tx, requestErr := server.Request(invite)
		if requestErr != nil {
			t.Errorf("send INVITE: %v", requestErr)
		}
		sentInvite <- tx
	}()
	buffer := make([]byte, 8192)
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	invitePayload := string(buffer[:n])
	tx := <-sentInvite
	response := NewResponseFromRequest("", invite, 486, "Busy Here", nil)
	response.SetSource(connection.RemoteAddr())
	delivered := make(chan struct{})
	go func() {
		server.handlerResponse(response)
		close(delivered)
	}()
	n, err = peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	ackPayload := string(buffer[:n])
	<-delivered
	if !strings.HasPrefix(ackPayload, "ACK ") || headerValue(ackPayload, "Via") != headerValue(invitePayload, "Via") {
		t.Fatalf("automatic non-2xx ACK = %s", ackPayload)
	}
	select {
	case got := <-tx.resp:
		if got != response {
			t.Fatalf("delivered response = %p, want %p", got, response)
		}
	default:
		t.Fatal("non-2xx response was not delivered after ACK")
	}

	for i := 0; i < cap(tx.resp)+2; i++ {
		delivered = make(chan struct{})
		go func() {
			server.handlerResponse(response)
			close(delivered)
		}()
		if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		n, err = peer.Read(buffer)
		if err != nil {
			t.Fatal(err)
		}
		ackPayload = string(buffer[:n])
		if !strings.HasPrefix(ackPayload, "ACK ") || headerValue(ackPayload, "Content-Length") != "0" {
			t.Fatalf("retransmitted non-2xx ACK %d = %s", i, ackPayload)
		}
		select {
		case <-delivered:
		case <-time.After(time.Second):
			t.Fatalf("retransmitted non-2xx response %d blocked the SIP handler", i)
		}
	}
	select {
	case duplicate := <-tx.resp:
		t.Fatalf("retransmitted non-2xx response reached business queue: %s", duplicate.StartLine())
	default:
	}
}

func TestHandlerResponseRetransmitsCached2xxInviteACK(t *testing.T) {
	connection := newServerTransactionCaptureConnection()
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())

	tx := NewTransaction("retransmit-2xx-ack", connection)
	defer tx.Close()
	if err := tx.Request(invite); err != nil {
		t.Fatal(err)
	}
	_ = connection.waitPayload(t)

	response := NewResponseFromRequest("", invite, 202, "Accepted", []byte("answer"))
	response.SetSource(connection.RemoteAddr())
	response.SetConnection(connection)
	tx.receiveResponse(response)
	got, err := tx.GetResponseContext(t.Context())
	if err != nil || got != response {
		t.Fatalf("INVITE response = %p, %v; want %p", got, err, response)
	}
	ack, err := NewRequestFromResponseChecked(MethodACK, response)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Request(ack); err != nil {
		t.Fatal(err)
	}
	firstACK := connection.waitPayload(t)
	if !strings.HasPrefix(string(firstACK), "ACK ") {
		t.Fatalf("initial ACK = %s", firstACK)
	}

	retransmitted, _ := response.Clone().(*Response)
	tx.receiveResponse(retransmitted)
	secondACK := connection.waitPayload(t)
	if string(secondACK) != string(firstACK) {
		t.Fatalf("retransmitted ACK differs:\nfirst: %s\nsecond: %s", firstACK, secondACK)
	}
	select {
	case duplicate := <-tx.resp:
		t.Fatalf("retransmitted 2xx reached business response queue: %s", duplicate.StartLine())
	default:
	}

	forked, _ := response.Clone().(*Response)
	forkedTo, ok := forked.To()
	if !ok || forkedTo == nil {
		t.Fatal("forked 2xx response is missing To")
	}
	forkedTo.Params = NewParams().Add("tag", String{Str: "another-dialog"})
	tx.receiveResponse(forked)
	forkedACK := connection.waitPayload(t)
	if !strings.HasPrefix(string(forkedACK), "ACK ") || string(forkedACK) == string(firstACK) {
		t.Fatalf("forked-dialog ACK = %s", forkedACK)
	}
	forkedBYE := connection.waitPayload(t)
	if !strings.HasPrefix(string(forkedBYE), "BYE ") {
		t.Fatalf("forked-dialog cleanup = %s", forkedBYE)
	}

	forkedRetransmission, _ := forked.Clone().(*Response)
	tx.receiveResponse(forkedRetransmission)
	retransmittedForkedACK := connection.waitPayload(t)
	if string(retransmittedForkedACK) != string(forkedACK) {
		t.Fatalf("retransmitted forked ACK differs:\nfirst: %s\nsecond: %s", forkedACK, retransmittedForkedACK)
	}
	select {
	case payload := <-connection.writes:
		t.Fatalf("forked 2xx retransmission sent duplicate cleanup request: %s", payload)
	default:
	}
	select {
	case got := <-tx.resp:
		t.Fatalf("forked 2xx reached business response queue: %s", got.StartLine())
	default:
	}
}

func TestHandlerResponseCleansUpForked2xxArrivingBeforeSelectedACK(t *testing.T) {
	connection := newServerTransactionCaptureConnection()
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())
	tx := NewTransaction("forked-2xx-before-ack", connection)
	defer tx.Close()
	if err := tx.Request(invite); err != nil {
		t.Fatal(err)
	}
	_ = connection.waitPayload(t)

	selected := NewResponseFromRequest("", invite, 200, "OK", []byte("selected answer"))
	selected.SetSource(connection.RemoteAddr())
	selected.SetConnection(connection)
	forked, _ := selected.Clone().(*Response)
	forked.SetBody([]byte("forked answer"), true)
	forkedTo, ok := forked.To()
	if !ok || forkedTo == nil {
		t.Fatal("forked 2xx response is missing To")
	}
	forkedTo.Params = NewParams().Add("tag", String{Str: "early-forked-dialog"})

	tx.receiveResponse(selected)
	got, err := tx.GetResponseContext(t.Context())
	if err != nil || got != selected {
		t.Fatalf("selected INVITE response = %p, %v; want %p", got, err, selected)
	}
	tx.receiveResponse(forked)
	select {
	case duplicate := <-tx.resp:
		t.Fatalf("early forked 2xx reached business queue: %s", duplicate.StartLine())
	default:
	}

	ack, err := NewRequestFromResponseChecked(MethodACK, selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Request(ack); err != nil {
		t.Fatal(err)
	}
	selectedACK := connection.waitPayload(t)
	if !strings.HasPrefix(string(selectedACK), "ACK ") {
		t.Fatalf("selected ACK = %s", selectedACK)
	}
	forkedACK := connection.waitPayload(t)
	if !strings.HasPrefix(string(forkedACK), "ACK ") || string(forkedACK) == string(selectedACK) {
		t.Fatalf("early forked ACK = %s", forkedACK)
	}
	forkedBYE := connection.waitPayload(t)
	if !strings.HasPrefix(string(forkedBYE), "BYE ") {
		t.Fatalf("early forked cleanup = %s", forkedBYE)
	}
}

func TestInviteForked2xxTrackingIsBoundedBeforeSelectedACK(t *testing.T) {
	connection := newServerTransactionCaptureConnection()
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())
	tx := NewTransaction("bounded-forked-2xx", connection)
	defer tx.Close()
	if err := tx.Request(invite); err != nil {
		t.Fatal(err)
	}
	_ = connection.waitPayload(t)

	selected := NewResponseFromRequest("", invite, 200, "OK", []byte("selected answer"))
	selected.SetSource(connection.RemoteAddr())
	selected.SetConnection(connection)
	tx.receiveResponse(selected)
	if got, err := tx.GetResponseContext(t.Context()); err != nil || got != selected {
		t.Fatalf("selected INVITE response = %p, %v; want %p", got, err, selected)
	}

	const expectedDialogLimit = maxInvite2xxDialogsPerTransaction
	for index := 0; index < expectedDialogLimit*2; index++ {
		forked, _ := selected.Clone().(*Response)
		to, ok := forked.To()
		if !ok || to == nil {
			t.Fatal("forked 2xx response is missing To")
		}
		to.Params = NewParams().Add("tag", String{Str: fmt.Sprintf("fork-%d", index)})
		tx.receiveResponse(forked)
	}

	tx.inviteACKMu.RLock()
	pending := len(tx.invitePending2xx)
	tracked := len(tx.invite2xxDialogs)
	tx.inviteACKMu.RUnlock()
	if tracked != expectedDialogLimit {
		t.Fatalf("tracked forked dialogs = %d, want %d", tracked, expectedDialogLimit)
	}
	if pending != expectedDialogLimit-1 {
		t.Fatalf("tracked pending forked dialogs = %d, want %d", pending, expectedDialogLimit-1)
	}
}

func TestTransactionDoesNotCacheNon2xxInviteACK(t *testing.T) {
	connection := newServerTransactionCaptureConnection()
	invite := newSignalDigestTestRequest(t, MethodInvite, []byte("offer"))
	invite.SetConnection(connection)
	invite.SetSource(connection.LocalAddr())
	invite.SetDestination(connection.RemoteAddr())
	tx := NewTransaction("do-not-cache-non-2xx-ack", connection)
	defer tx.Close()
	if err := tx.Request(invite); err != nil {
		t.Fatal(err)
	}
	_ = connection.waitPayload(t)

	response := NewResponseFromRequest("", invite, 486, "Busy Here", nil)
	response.SetConnection(connection)
	response.SetSource(connection.RemoteAddr())
	ack, err := NewAckRequestForNon2xxResponseChecked(invite, response)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Request(ack); err != nil {
		t.Fatal(err)
	}
	_ = connection.waitPayload(t)
	tx.inviteACKMu.RLock()
	cached := tx.inviteACKs
	tx.inviteACKMu.RUnlock()
	if len(cached) != 0 {
		t.Fatal("non-2xx transaction ACK was cached as a 2xx dialog ACK")
	}
}

func headerValue(payload, name string) string {
	for line := range strings.SplitSeq(payload, "\r\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(key), name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
