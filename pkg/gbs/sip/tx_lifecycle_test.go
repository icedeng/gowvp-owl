package sip

import (
	"context"
	"net"
	"strings"
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

	delivered = make(chan struct{})
	go func() {
		server.handlerResponse(response)
		close(delivered)
	}()
	n, err = peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	ackPayload = string(buffer[:n])
	<-delivered
	if !strings.HasPrefix(ackPayload, "ACK ") || headerValue(ackPayload, "Content-Length") != "0" {
		t.Fatalf("retransmitted non-2xx ACK = %s", ackPayload)
	}
	select {
	case got := <-tx.resp:
		if got != response {
			t.Fatalf("retransmitted response = %p, want %p", got, response)
		}
	default:
		t.Fatal("retransmitted non-2xx response was not delivered after ACK")
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
