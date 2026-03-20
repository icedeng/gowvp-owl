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
