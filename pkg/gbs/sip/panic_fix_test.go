package sip

import (
	"errors"
	"net"
	"sync"
	"testing"
)

type nilCloneHeader struct {
	name string
}

func (h *nilCloneHeader) Name() string { return h.name }

func (h *nilCloneHeader) Clone() Header { return nil }

func (h *nilCloneHeader) String() string { return h.name + ": test" }

func (h *nilCloneHeader) Equals(other any) bool { return false }

func TestRecordRouteHeaderCloneEmptyDoesNotPanic(t *testing.T) {
	header := &RecordRouteHeader{}

	cloned := header.Clone()
	recordRoute, ok := cloned.(*RecordRouteHeader)
	if !ok {
		t.Fatalf("unexpected clone type: %T", cloned)
	}
	if recordRoute == nil {
		t.Fatal("clone returned nil")
	}
	if len(recordRoute.Addresses) != 0 {
		t.Fatalf("expected empty addresses, got %d", len(recordRoute.Addresses))
	}
}

func TestCopyHeadersSkipsNilClone(t *testing.T) {
	from := NewRequest("", MethodInvite, &URI{FHost: "example.com"}, DefaultSipVersion, []Header{
		&nilCloneHeader{name: "Record-Route"},
	}, nil)
	to := NewResponse("", DefaultSipVersion, 200, "OK", nil, nil)

	CopyHeaders("Record-Route", from, to)

	if got := len(to.GetHeaders("Record-Route")); got != 0 {
		t.Fatalf("expected no copied headers, got %d", got)
	}
}

func TestNewResponseFromRequestDoesNotMutateRequestToTag(t *testing.T) {
	target, err := ParseURI("sip:34020000001320000001@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams().Add("tag", String{Str: "remote"})}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "192.0.2.20", Port: NewPort(5060), Params: NewParams()}).
		Build(), nil)

	response := NewResponseFromRequest("", request, 200, "OK", nil)
	requestTo, ok := request.To()
	if !ok || requestTo == nil {
		t.Fatal("request To header is unavailable")
	}
	if requestTo.Params != nil {
		if _, exists := requestTo.Params.Get("tag"); exists {
			t.Fatal("response construction added To tag to original request")
		}
	}
	responseTo, ok := response.To()
	if !ok || responseTo == nil {
		t.Fatal("response To header is unavailable")
	}
	if tag, exists := responseTo.Params.Get("tag"); !exists || tag == nil || tag.String() == "" {
		t.Fatal("response To header is missing generated tag")
	}
}

func TestNewRequestFromResponseFallsBackToToWithoutContact(t *testing.T) {
	toURI, err := ParseURI("sip:34020000001320000001@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodInvite, toURI, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).
		SetFrom(&Address{URI: toURI.Clone(), Params: NewParams()}).
		SetTo(&Address{URI: toURI.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "192.0.2.20", Port: NewPort(5060), Params: NewParams()}).
		Build(), nil)
	response := NewResponseFromRequest("", request, 200, "OK", nil)

	ack := NewRequestFromResponse(MethodACK, response)
	if ack.Recipient() == nil || ack.Recipient().String() != toURI.String() {
		t.Fatalf("ACK recipient = %v; want %s", ack.Recipient(), toURI)
	}
}

func TestNewRequestFromResponseCheckedRejectsMalformedResponse(t *testing.T) {
	target, err := ParseURI("sip:34020000001320000001@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	validRequest := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams()}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "192.0.2.20", Port: NewPort(5060), Params: NewParams()}).
		Build(), nil)

	tests := []struct {
		name     string
		response *Response
	}{
		{name: "nil response"},
		{name: "missing target", response: NewResponse("", DefaultSipVersion, 200, "OK", nil, nil)},
		{name: "missing Via", response: func() *Response {
			response := NewResponseFromRequest("", validRequest, 200, "OK", nil)
			response.RemoveHeader("Via")
			return response
		}()},
		{name: "missing CSeq", response: func() *Response {
			response := NewResponseFromRequest("", validRequest, 200, "OK", nil)
			response.RemoveHeader("CSeq")
			return response
		}()},
		{name: "missing Call-ID", response: func() *Response {
			response := NewResponseFromRequest("", validRequest, 200, "OK", nil)
			response.RemoveHeader("Call-ID")
			return response
		}()},
		{name: "missing From tag", response: func() *Response {
			response := NewResponseFromRequest("", validRequest, 200, "OK", nil)
			from, _ := response.From()
			from.Params = NewParams()
			return response
		}()},
		{name: "missing To tag", response: func() *Response {
			response := NewResponseFromRequest("", validRequest, 200, "OK", nil)
			to, _ := response.To()
			to.Params = NewParams()
			return response
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if request, err := NewRequestFromResponseChecked(MethodACK, test.response); err == nil || request != nil {
				t.Fatalf("malformed response produced request %#v, error %v", request, err)
			}
			if request := NewRequestFromResponse(MethodACK, test.response); request != nil {
				t.Fatalf("compatibility constructor returned request %#v", request)
			}
		})
	}
}

func TestNewRequestFromResponseReversesRouteSetAndPreservesResponseCSeq(t *testing.T) {
	target, err := ParseURI("sip:34020000001320000001@192.0.2.10:5060")
	if err != nil {
		t.Fatal(err)
	}
	request := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams()}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "192.0.2.20", Port: NewPort(5060), Params: NewParams()}).
		Build(), nil)
	response := NewResponseFromRequest("", request, 200, "OK", nil)
	proxy1, _ := ParseURI("sip:proxy1.example;lr")
	proxy2, _ := ParseURI("sip:proxy2.example;lr")
	proxy3, _ := ParseURI("sip:proxy3.example;lr")
	response.AppendHeader(&RecordRouteHeader{Addresses: []*URI{proxy1, proxy2}})
	response.AppendHeader(&RecordRouteHeader{Addresses: []*URI{proxy3}})

	ack := NewRequestFromResponse(MethodACK, response)
	routes := ack.GetHeaders("Route")
	if len(routes) != 1 {
		t.Fatalf("ACK Route headers = %d, want 1", len(routes))
	}
	route, ok := routes[0].(*RouteHeader)
	if !ok || len(route.Addresses) != 3 {
		t.Fatalf("ACK route set = %#v", routes[0])
	}
	for index, want := range []string{"proxy3.example", "proxy2.example", "proxy1.example"} {
		if got := route.Addresses[index].Host(); got != want {
			t.Fatalf("ACK route[%d] = %q, want %q", index, got, want)
		}
	}
	if original, _ := response.CSeq(); original.MethodName != MethodInvite || original.SeqNo != 1 {
		t.Fatalf("response CSeq mutated after ACK: %+v", original)
	}

	bye := NewRequestFromResponse(MethodBYE, response)
	if cseq, _ := bye.CSeq(); cseq.MethodName != MethodBYE || cseq.SeqNo != 2 {
		t.Fatalf("BYE CSeq = %+v", cseq)
	}
	if original, _ := response.CSeq(); original.MethodName != MethodInvite || original.SeqNo != 1 {
		t.Fatalf("response CSeq mutated after BYE: %+v", original)
	}
}

func TestNewRequestFromResponseAppliesStrictAndLooseRouting(t *testing.T) {
	target, _ := ParseURI("sip:34020000001320000001@device.example:5060")
	request := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams()}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "client.example", Port: NewPort(5060), Params: NewParams().Add("branch", String{Str: "z9hG4bK-invite"})}).
		Build(), nil)
	response := NewResponseFromRequest("", request, 200, "OK", nil)
	contact, _ := ParseURI("sip:34020000001320000001@contact.example:5070")
	response.AppendHeader(&ContactHeader{Address: contact, Params: NewParams()})
	loose, _ := ParseURI("sip:loose.example;LR")
	strict, _ := ParseURI("sip:strict.example")
	response.AppendHeader(&RecordRouteHeader{Addresses: []*URI{loose, strict}})

	ack, err := NewRequestFromResponseChecked(MethodACK, response)
	if err != nil {
		t.Fatal(err)
	}
	if length, ok := ack.ContentLength(); !ok || length == nil || *length != 0 {
		t.Fatalf("ACK Content-Length = %v, present %v", length, ok)
	}
	if got := ack.Recipient().Host(); got != "strict.example" {
		t.Fatalf("strict-route Request-URI host = %q", got)
	}
	if nextHop := ack.NextHopURI(); nextHop == nil || nextHop.Host() != "strict.example" {
		t.Fatalf("strict-route next hop = %v", nextHop)
	}
	route, ok := ack.GetHeaders("Route")[0].(*RouteHeader)
	if !ok || len(route.Addresses) != 2 || route.Addresses[0].Host() != "loose.example" || route.Addresses[1].Host() != "contact.example" {
		t.Fatalf("strict-route Route = %#v", ack.GetHeaders("Route"))
	}

	response.RemoveHeader("Record-Route")
	first, _ := ParseURI("sip:first.example;lr")
	second, _ := ParseURI("sip:second.example;lr")
	response.AppendHeader(&RecordRouteHeader{Addresses: []*URI{first, second}})
	ack, err = NewRequestFromResponseChecked(MethodACK, response)
	if err != nil {
		t.Fatal(err)
	}
	if got := ack.Recipient().Host(); got != "contact.example" {
		t.Fatalf("loose-route Request-URI host = %q", got)
	}
	if nextHop := ack.NextHopURI(); nextHop == nil || nextHop.Host() != "second.example" {
		t.Fatalf("loose-route next hop = %v", nextHop)
	}
	route, ok = ack.GetHeaders("Route")[0].(*RouteHeader)
	if !ok || len(route.Addresses) != 2 || route.Addresses[0].Host() != "second.example" || route.Addresses[1].Host() != "first.example" {
		t.Fatalf("loose-route Route = %#v", ack.GetHeaders("Route"))
	}
}

func TestNewRequestFromServerDialogAppliesStrictAndLooseRouting(t *testing.T) {
	local, _ := ParseURI("sip:34020000002000000001@local.example")
	remote, _ := ParseURI("sip:34020000002000000002@remote.example")
	contact, _ := ParseURI("sip:34020000002000000002@contact.example:5070")
	callID := CallID("server-dialog-routing")
	inbound := NewRequest("", MethodSubscribe, local.Clone(), DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodSubscribe).SetSeqNo(7).
		SetFrom(&Address{URI: remote.Clone(), Params: NewParams().Add("tag", String{Str: "remote-tag"})}).
		SetTo(&Address{URI: local.Clone(), Params: NewParams()}).
		SetContact(&Address{URI: contact.Clone(), Params: NewParams()}).
		SetCallID(&callID).
		AddVia(&ViaHop{Host: "remote.example", Port: NewPort(5060), Params: NewParams().Add("branch", String{Str: GenerateBranch()})}).
		Build(), nil)
	inbound.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})
	inbound.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	first, _ := ParseURI("sip:first-proxy.example;lr")
	second, _ := ParseURI("sip:second-proxy.example;lr")
	inbound.AppendHeader(&RecordRouteHeader{Addresses: []*URI{first, second}})
	response := NewResponseFromRequest("", inbound, 200, "OK", nil)

	notify, err := NewRequestFromServerDialogChecked(MethodNotify, inbound, response, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := notify.Recipient().Host(); got != "contact.example" {
		t.Fatalf("loose-route Request-URI host = %q", got)
	}
	if nextHop := notify.NextHopURI(); nextHop == nil || nextHop.Host() != "first-proxy.example" {
		t.Fatalf("loose-route next hop = %v", nextHop)
	}
	route, ok := notify.GetHeaders("Route")[0].(*RouteHeader)
	if !ok || len(route.Addresses) != 2 || route.Addresses[0].Host() != "first-proxy.example" || route.Addresses[1].Host() != "second-proxy.example" {
		t.Fatalf("loose-route Route = %#v", notify.GetHeaders("Route"))
	}
	from, _ := notify.From()
	to, _ := notify.To()
	if dialogHeaderParam(from.Params, "tag") == "" || dialogHeaderParam(to.Params, "tag") != "remote-tag" {
		t.Fatalf("NOTIFY dialog tags = From %v, To %v", from, to)
	}
	if actualCallID, _ := notify.CallID(); actualCallID == nil || *actualCallID != callID {
		t.Fatalf("NOTIFY Call-ID = %v", actualCallID)
	}
	if actualCSeq, _ := notify.CSeq(); actualCSeq == nil || actualCSeq.SeqNo != 1 || actualCSeq.MethodName != MethodNotify {
		t.Fatalf("NOTIFY CSeq = %+v", actualCSeq)
	}
	assertDefaultMaxForwards(t, notify)
	if notify.Destination() == nil || notify.Destination().String() != inbound.Source().String() {
		t.Fatalf("NOTIFY destination = %v", notify.Destination())
	}

	inbound.RemoveHeader("Record-Route")
	strict, _ := ParseURI("sip:strict-proxy.example")
	loose, _ := ParseURI("sip:loose-proxy.example;lr")
	inbound.AppendHeader(&RecordRouteHeader{Addresses: []*URI{strict, loose}})
	notify, err = NewRequestFromServerDialogChecked(MethodNotify, inbound, response, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got := notify.Recipient().Host(); got != "strict-proxy.example" {
		t.Fatalf("strict-route Request-URI host = %q", got)
	}
	if nextHop := notify.NextHopURI(); nextHop == nil || nextHop.Host() != "strict-proxy.example" {
		t.Fatalf("strict-route next hop = %v", nextHop)
	}
	clone, ok := notify.Clone().(*Request)
	if !ok || clone.NextHopURI() == nil || clone.NextHopURI().Host() != "strict-proxy.example" {
		t.Fatalf("strict-route cloned next hop = %v", clone)
	}
	route, ok = notify.GetHeaders("Route")[0].(*RouteHeader)
	if !ok || len(route.Addresses) != 2 || route.Addresses[0].Host() != "loose-proxy.example" || route.Addresses[1].Host() != "contact.example" {
		t.Fatalf("strict-route Route = %#v", notify.GetHeaders("Route"))
	}
}

func TestNewRequestFromServerDialogRejectsMalformedDialog(t *testing.T) {
	target, _ := ParseURI("sip:34020000002000000001@example.com")
	callID := CallID("malformed-server-dialog")
	inbound := NewRequest("", MethodSubscribe, target.Clone(), DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodSubscribe).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams().Add("tag", String{Str: "remote-tag"})}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		SetCallID(&callID).
		AddVia(&ViaHop{Host: "example.com", Params: NewParams().Add("branch", String{Str: GenerateBranch()})}).
		Build(), nil)
	response := NewResponseFromRequest("", inbound, 200, "OK", nil)
	responseTo, _ := response.To()
	responseTo.Params = NewParams()
	if request, err := NewRequestFromServerDialogChecked(MethodNotify, inbound, response, 1); err == nil || request != nil {
		t.Fatal("dialog without local tag produced NOTIFY")
	}

	response = NewResponseFromRequest("", inbound, 200, "OK", nil)
	inbound.AppendHeader(&GenericHeader{HeaderName: "Record-Route", Contents: "invalid"})
	if request, err := NewRequestFromServerDialogChecked(MethodNotify, inbound, response, 1); err == nil || request != nil {
		t.Fatal("malformed Record-Route produced NOTIFY")
	}
}

func TestNewRequestFromResponseSanitizesViaAndAllocatesDialogCSeq(t *testing.T) {
	target, _ := ParseURI("sip:34020000001320000001@device.example:5060")
	request := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).
		SetSeqNo(41).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams()}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "client.example", Port: NewPort(5060), Params: NewParams().
			Add("branch", String{Str: "z9hG4bK-invite"}).Add("received", String{Str: "192.0.2.20"}).
			Add("rport", String{Str: "41000"}).Add("keep", String{Str: "yes"})}).
		Build(), nil)
	response := NewResponseFromRequest("", request, 200, "OK", nil)
	connection, peer := net.Pipe()
	defer connection.Close()
	defer peer.Close()
	wrapped := NewTCPConnection(connection)
	response.SetConnection(wrapped)

	ack, err := NewRequestFromResponseChecked(MethodACK, response)
	if err != nil {
		t.Fatal(err)
	}
	via, _ := ack.ViaHop()
	if branch := sipViaBranchValue(via); branch == "" || branch == "z9hG4bK-invite" {
		t.Fatalf("ACK Via branch = %q", branch)
	}
	if _, _, count := sipViaParam(via, "received"); count != 0 {
		t.Fatal("ACK copied response received parameter")
	}
	if _, value, count := sipViaParam(via, "rport"); count != 1 || value != "" {
		t.Fatalf("ACK rport = count:%d value:%q", count, value)
	}
	if value, ok := via.Params.Get("keep"); !ok || value == nil || value.String() != "yes" {
		t.Fatal("ACK dropped unrelated Via parameter")
	}
	if ack.GetConnection() != wrapped {
		t.Fatal("ACK did not retain response transport connection")
	}
	assertDefaultMaxForwards(t, ack)

	info, err := NewRequestFromResponseChecked(MethodInfo, response)
	if err != nil {
		t.Fatal(err)
	}
	assertDefaultMaxForwards(t, info)
	bye, err := NewRequestFromResponseChecked(MethodBYE, response)
	if err != nil {
		t.Fatal(err)
	}
	assertDefaultMaxForwards(t, bye)
	infoCSeq, _ := info.CSeq()
	byeCSeq, _ := bye.CSeq()
	if infoCSeq.SeqNo != 42 || byeCSeq.SeqNo != 43 {
		t.Fatalf("dialog CSeq = INFO %d, BYE %d", infoCSeq.SeqNo, byeCSeq.SeqNo)
	}
	responseCSeq, _ := response.CSeq()
	if responseCSeq.SeqNo != 41 || responseCSeq.MethodName != MethodInvite {
		t.Fatalf("response CSeq mutated: %+v", responseCSeq)
	}
}

func assertDefaultMaxForwards(t *testing.T, request *Request) {
	t.Helper()
	headers := request.GetHeaders("Max-Forwards")
	if len(headers) != 1 {
		t.Fatalf("%s Max-Forwards header count = %d", request.Method(), len(headers))
	}
	value, ok := headers[0].(*MaxForwards)
	if !ok || value == nil || *value != defaultMaxForwards {
		t.Fatalf("%s Max-Forwards = %#v", request.Method(), headers[0])
	}
}

func TestNewRequestFromResponseAllocatesConcurrentDialogCSeq(t *testing.T) {
	target, _ := ParseURI("sip:34020000001320000001@device.example:5060")
	request := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).SetSeqNo(100).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams()}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "client.example", Params: NewParams().Add("branch", String{Str: "z9hG4bK-concurrent"})}).
		Build(), nil)
	response := NewResponseFromRequest("", request, 200, "OK", nil)
	const workers = 32
	results := make(chan uint32, workers)
	var wait sync.WaitGroup
	wait.Add(workers)
	for range workers {
		go func() {
			defer wait.Done()
			info, err := NewRequestFromResponseChecked(MethodInfo, response)
			if err != nil {
				t.Errorf("build concurrent INFO: %v", err)
				return
			}
			cseq, _ := info.CSeq()
			results <- cseq.SeqNo
		}()
	}
	wait.Wait()
	close(results)
	seen := make(map[uint32]struct{}, workers)
	for cseq := range results {
		if cseq < 101 || cseq > 100+workers {
			t.Fatalf("concurrent dialog CSeq = %d", cseq)
		}
		if _, exists := seen[cseq]; exists {
			t.Fatalf("duplicate concurrent dialog CSeq = %d", cseq)
		}
		seen[cseq] = struct{}{}
	}
	if len(seen) != workers {
		t.Fatalf("concurrent dialog CSeq count = %d, want %d", len(seen), workers)
	}
}

func TestNewRequestFromResponsePreparedFailureDoesNotConsumeDialogCSeq(t *testing.T) {
	target, _ := ParseURI("sip:34020000001320000001@device.example:5060")
	request := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).SetSeqNo(17).
		SetFrom(&Address{URI: target.Clone(), Params: NewParams()}).
		SetTo(&Address{URI: target.Clone(), Params: NewParams()}).
		AddVia(&ViaHop{Host: "client.example", Params: NewParams().Add("branch", String{Str: "z9hG4bK-prepared"})}).
		Build(), nil)
	response := NewResponseFromRequest("", request, 200, "OK", nil)
	prepareErr := errors.New("local preparation failed")
	prepared, err := NewRequestFromResponsePreparedChecked(MethodInfo, response, func(*Request) error {
		return prepareErr
	})
	if prepared != nil || !errors.Is(err, prepareErr) {
		t.Fatalf("prepared failure = request %v, err %v", prepared, err)
	}
	info, err := NewRequestFromResponseChecked(MethodInfo, response)
	if err != nil {
		t.Fatal(err)
	}
	cseq, _ := info.CSeq()
	if cseq == nil || cseq.SeqNo != 18 {
		t.Fatalf("dialog CSeq after prepared failure = %+v, want 18", cseq)
	}
}

func TestNewCancelRequestFromInvitePreservesTransaction(t *testing.T) {
	target, _ := ParseURI("sip:34020000001320000001@device.example:5060")
	from := &Address{URI: target.Clone(), Params: NewParams().Add("tag", String{Str: "from-tag"})}
	to := &Address{URI: target.Clone(), Params: NewParams()}
	callID := CallID("cancel-transaction")
	invite := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).SetSeqNo(17).SetFrom(from).SetTo(to).SetCallID(&callID).
		AddVia(&ViaHop{Host: "client.example", Port: NewPort(5060), Params: NewParams().Add("branch", String{Str: "z9hG4bK-cancel"})}).
		Build(), nil)
	route, _ := ParseURI("sip:proxy.example;lr")
	invite.AppendHeader(&RouteHeader{Addresses: []*URI{route}})
	invite.SetSource(&net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 5060})
	invite.SetDestination(&net.UDPAddr{IP: net.ParseIP("192.0.2.30"), Port: 5060})

	cancel, err := NewCancelRequestFromInviteChecked(invite)
	if err != nil {
		t.Fatal(err)
	}
	if cancel.Recipient().String() != invite.Recipient().String() || cancel.Destination().String() != invite.Destination().String() {
		t.Fatalf("CANCEL target = %s / %v", cancel.Recipient(), cancel.Destination())
	}
	inviteVia, _ := invite.ViaHop()
	cancelVia, _ := cancel.ViaHop()
	if sipViaBranchValue(cancelVia) != sipViaBranchValue(inviteVia) || cancelVia.SentBy() != inviteVia.SentBy() {
		t.Fatalf("CANCEL Via = %s, INVITE Via = %s", cancelVia, inviteVia)
	}
	cseq, _ := cancel.CSeq()
	if cseq.SeqNo != 17 || cseq.MethodName != MethodCancel || len(cancel.GetHeaders("Route")) != 1 {
		t.Fatalf("CANCEL transaction headers = CSeq:%+v Route:%v", cseq, cancel.GetHeaders("Route"))
	}
	if request, err := NewRequestFromResponseChecked(MethodCancel, NewResponseFromRequest("", invite, 200, "OK", nil)); err == nil || request != nil {
		t.Fatal("CANCEL was incorrectly constructed from a response")
	}
}

func TestNewRequestFromServerDialogRejectsOutOfRangeCSeq(t *testing.T) {
	target, _ := ParseURI("sip:34020000001320000001@device.example:5060")
	from := &Address{URI: target.Clone(), Params: NewParams().Add("tag", String{Str: "remote-tag"})}
	to := &Address{URI: target.Clone(), Params: NewParams()}
	callID := CallID("server-dialog-cseq-limit")
	request := NewRequest("", MethodSubscribe, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodSubscribe).SetSeqNo(19).SetFrom(from).SetTo(to).SetCallID(&callID).
		AddVia(&ViaHop{Host: "device.example", Params: NewParams().Add("branch", String{Str: "z9hG4bK-server-dialog-limit"})}).
		Build(), nil)
	response := NewResponseFromRequest("", request, 200, "OK", nil)

	if built, err := NewRequestFromServerDialogChecked(MethodNotify, request, response, maxCseq+1); err == nil || built != nil {
		t.Fatalf("out-of-range server-dialog CSeq accepted: request=%v err=%v", built, err)
	}
}

func TestNewAckRequestForNon2xxResponsePreservesInviteTransaction(t *testing.T) {
	target, _ := ParseURI("sip:34020000001320000001@device.example:5060")
	from := &Address{URI: target.Clone(), Params: NewParams().Add("tag", String{Str: "from-tag"})}
	to := &Address{URI: target.Clone(), Params: NewParams()}
	callID := CallID("non-2xx-ack")
	invite := NewRequest("", MethodInvite, target, DefaultSipVersion, NewHeaderBuilder().
		SetMethod(MethodInvite).SetSeqNo(19).SetFrom(from).SetTo(to).SetCallID(&callID).
		AddVia(&ViaHop{Host: "client.example", Params: NewParams().Add("branch", String{Str: "z9hG4bK-error"})}).
		Build(), nil)
	response := NewResponseFromRequest("", invite, 486, "Busy Here", nil)
	ack, err := NewAckRequestForNon2xxResponseChecked(invite, response)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Recipient().String() != invite.Recipient().String() {
		t.Fatalf("non-2xx ACK Request-URI = %s", ack.Recipient())
	}
	inviteVia, _ := invite.ViaHop()
	ackVia, _ := ack.ViaHop()
	if sipViaBranchValue(ackVia) != sipViaBranchValue(inviteVia) {
		t.Fatalf("non-2xx ACK Via = %s, INVITE Via = %s", ackVia, inviteVia)
	}
	cseq, _ := ack.CSeq()
	if cseq.SeqNo != 19 || cseq.MethodName != MethodACK {
		t.Fatalf("non-2xx ACK CSeq = %+v", cseq)
	}
	if request, err := NewRequestFromResponseChecked(MethodACK, response); err == nil || request != nil {
		t.Fatal("dialog ACK constructor accepted non-2xx response")
	}
}

func TestServerRunContextSafelyRecoversPanic(t *testing.T) {
	srv := NewServer(&Address{})
	ctx := &Context{
		Request: NewRequest("", MethodInvite, &URI{FHost: "example.com"}, DefaultSipVersion, nil, nil),
		handlers: []HandlerFunc{
			func(c *Context) {
				panic("boom")
			},
		},
		index: -1,
	}

	srv.runContextSafely(ctx)
}

func TestZeroContextSetInitializesCache(t *testing.T) {
	ctx := &Context{}
	ctx.Set("worker", "cascade")
	value, ok := ctx.Get("worker")
	if !ok || value != "cascade" {
		t.Fatalf("zero Context cache value = %v, %v", value, ok)
	}
}
