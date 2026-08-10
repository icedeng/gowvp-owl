package ota

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestGetLastVersion(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet || req.URL.String() != "https://api.github.com/repos/gowvp/owl/releases/latest" {
			t.Fatalf("unexpected request: %s %s", req.Method, req.URL.String())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"tag_name":"v1.2.3","body":"test release"}`)),
		}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = previous })

	version, desc, err := GetLastVersion("gowvp/owl")
	if err != nil {
		t.Fatalf("GetLastVersion() error = %v", err)
	}
	if version != "v1.2.3" || desc != "test release" {
		t.Fatalf("unexpected release: version=%q desc=%q", version, desc)
	}
}
