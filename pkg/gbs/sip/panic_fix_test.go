package sip

import "testing"

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
