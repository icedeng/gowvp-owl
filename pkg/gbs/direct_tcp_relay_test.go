package gbs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDirectTCPRelayForwardsBytesAndReleasesCapacity(t *testing.T) {
	payload := bytes.Repeat([]byte("gb28181-relay-"), 8192)
	deviceResult := make(chan error, 1)
	deviceAddress := startDirectTCPFixture(t, func(conn net.Conn) {
		_, err := conn.Write(payload)
		deviceResult <- err
	})
	relay, relayAddress, released := newDirectTCPRelayTestFixture(t, deviceAddress, func(net.IP) bool { return true }, int64(len(payload)), true)
	result := startDirectTCPRelayTest(t, relay)

	upstream, err := net.DialTimeout("tcp", relayAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	received, readErr := io.ReadAll(upstream)
	_ = upstream.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("relayed payload length = %d, want %d", len(received), len(payload))
	}
	if err := <-deviceResult; err != nil {
		t.Fatal(err)
	}
	terminal := waitDirectTCPRelayTestResult(t, result)
	if terminal.Err != nil || terminal.Reason != "eof" || terminal.Received != int64(len(payload)) {
		t.Fatalf("relay result = %+v", terminal)
	}
	if released.Load() != 1 {
		t.Fatalf("relay capacity releases = %d, want 1", released.Load())
	}
	if relay.Start(nil) {
		t.Fatal("completed relay restarted")
	}
}

func TestDirectTCPRelayRejectsUnauthorizedPeerBeforeForwarding(t *testing.T) {
	payload := []byte("authorized-upstream-only")
	deviceAddress := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write(payload)
	})
	var allowed atomic.Bool
	relay, relayAddress, _ := newDirectTCPRelayTestFixture(t, deviceAddress, func(net.IP) bool {
		return allowed.Load()
	}, int64(len(payload)), true)
	result := startDirectTCPRelayTest(t, relay)

	unauthorized, err := net.DialTimeout("tcp", relayAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := unauthorized.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := unauthorized.Read(one[:]); err == nil {
		t.Fatal("unauthorized upstream connection remained open")
	}
	_ = unauthorized.Close()

	allowed.Store(true)
	upstream, err := net.DialTimeout("tcp", relayAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	received, err := io.ReadAll(upstream)
	_ = upstream.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("authorized relay payload = %q, want %q", received, payload)
	}
	terminal := waitDirectTCPRelayTestResult(t, result)
	if terminal.Err != nil || terminal.Reason != "eof" {
		t.Fatalf("relay result after rejected peer = %+v", terminal)
	}
}

func TestDirectTCPRelayMediaStatusCompletesUnknownSize(t *testing.T) {
	payload := []byte("unknown-size-relay")
	holdDevice := make(chan struct{})
	t.Cleanup(func() { close(holdDevice) })
	deviceAddress := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write(payload)
		<-holdDevice
	})
	relay, relayAddress, _ := newDirectTCPRelayTestFixture(t, deviceAddress, func(net.IP) bool { return true }, 0, false)
	result := startDirectTCPRelayTest(t, relay)

	upstream, err := net.DialTimeout("tcp", relayAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(upstream, received); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatalf("relayed payload = %q, want %q", received, payload)
	}
	relay.NotifySenderFinished()
	relay.NotifySenderFinished()
	terminal := waitDirectTCPRelayTestResult(t, result)
	_ = upstream.Close()
	if terminal.Err != nil || terminal.Reason != "media_status" || terminal.Received != int64(len(payload)) {
		t.Fatalf("MediaStatus relay result = %+v", terminal)
	}
}

func TestDirectTCPRelayTimesOutWaitingForUpstream(t *testing.T) {
	deviceAddress := startDirectTCPFixture(t, func(net.Conn) {})
	relay, _, released := newDirectTCPRelayTestFixture(t, deviceAddress, func(net.IP) bool { return true }, 0, false)
	relay.opts.FirstByteTimeout = 30 * time.Millisecond
	result := startDirectTCPRelayTest(t, relay)

	terminal := waitDirectTCPRelayTestResult(t, result)
	if terminal.Err == nil || terminal.Reason != "accept_failed" || !strings.Contains(terminal.Err.Error(), "timeout") {
		t.Fatalf("upstream timeout result = %+v", terminal)
	}
	if released.Load() != 1 {
		t.Fatalf("timed-out relay capacity releases = %d, want 1", released.Load())
	}
}

func TestPrepareCascadeDirectTCPRelayRejectsDeclaredSizeAboveLimit(t *testing.T) {
	manager := NewDirectTCPDownloadManager(DirectTCPDownloadOptions{
		MaxFileSize:          8,
		GlobalConcurrency:    1,
		DeviceConcurrency:    1,
		DialTimeout:          time.Second,
		FirstByteTimeout:     time.Second,
		IdleTimeout:          time.Second,
		TotalTimeout:         time.Second,
		AllowUnsafeAddresses: true,
	})
	t.Cleanup(manager.Shutdown)
	api := &GB28181API{directDownloads: manager}

	_, _, _, err := api.prepareCascadeDirectTCPRelay(t.Context(), directTCPRuntimePolicy{CascadeRelayEnabled: true}, cascadePlatform{}, gb10DeviceID,
		net.ParseIP("127.0.0.1"), directTCPDownloadOffer{Address: "127.0.0.1:9", FileSize: 9, FileSizeKnown: true})
	if err == nil || !strings.Contains(err.Error(), "exceeds limit 8") {
		t.Fatalf("oversized relay preparation error = %v", err)
	}
	manager.mu.RLock()
	active := manager.activeCount
	manager.mu.RUnlock()
	if active != 0 {
		t.Fatalf("oversized relay retained capacity: %d", active)
	}
}

func TestCascadeDirectTCPDownloadSDPVersionGateAndAnswer(t *testing.T) {
	const start, end = int64(1711929600), int64(1711933200)
	platform := testSharedCascadePlatform(t)
	body := cascadeHistoryOfferSDP(historyModeDownload, "tcp", "192.0.2.30", "", start, end, 4)
	offer, err := parseCascadeVideoOffer(body, GBVersion11, platform)
	if err != nil {
		t.Fatal(err)
	}
	if !offer.DirectTCP || offer.Mode != historyModeDownload || offer.Protocol != "TCP" || offer.Port != 30000 || offer.DownloadSpeed != 4 {
		t.Fatalf("parsed cascade direct TCP offer = %+v", offer)
	}
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion20, GBVersion30} {
		if _, err := parseCascadeVideoOffer(body, version, platform); err == nil || !strings.Contains(err.Error(), "only supported by GB/T 28181-2014") {
			t.Fatalf("%s raw TCP cascade result = %v", version.StandardYear(), err)
		}
	}
	playback := cascadeHistoryOfferSDP(historyModePlayback, "tcp", "192.0.2.30", "", start, end, 0)
	if _, err := parseCascadeVideoOffer(playback, GBVersion11, platform); err == nil || !strings.Contains(err.Error(), "only supported") {
		t.Fatalf("2014 raw TCP Playback result = %v", err)
	}
	withSetup := cascadeHistoryOfferSDP(historyModeDownload, "tcp", "192.0.2.30", "active", start, end, 0)
	if _, err := parseCascadeVideoOffer(withSetup, GBVersion11, platform); err == nil || !strings.Contains(err.Error(), "must not contain RTP TCP") {
		t.Fatalf("raw TCP setup attribute result = %v", err)
	}

	offer.FileSize = 1024
	offer.FileSizeKnown = true
	answer, err := buildCascadeDirectTCPSDPAnswer(gb10DeviceID, offer, "198.51.100.10", 42000, "v/2/5/25/1/0a///")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"s=Download", "m=video 42000 tcp 96", "c=IN IP4 198.51.100.10", "a=sendonly",
		"a=rtpmap:96 PS/90000", "a=downloadspeed:4", "a=filesize:1024", "y=1100000011", "f=v/2/5/25/1/0a///",
	} {
		if !bytes.Contains(answer, []byte(expected)) {
			t.Fatalf("cascade direct TCP answer missing %q:\n%s", expected, answer)
		}
	}
	if bytes.Contains(answer, []byte("a=recvonly")) || bytes.Contains(answer, []byte("a=setup:")) {
		t.Fatalf("cascade direct TCP answer contains receiver/RTP-TCP attributes:\n%s", answer)
	}
}

func newDirectTCPRelayTestFixture(
	t *testing.T,
	deviceAddress string,
	allowed func(net.IP) bool,
	fileSize int64,
	fileSizeKnown bool,
) (*directTCPRelay, string, *atomic.Int32) {
	t.Helper()
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	released := &atomic.Int32{}
	relay := &directTCPRelay{
		ctx:             ctx,
		cancel:          cancel,
		listener:        listener,
		deviceAddress:   deviceAddress,
		upstreamAllowed: allowed,
		opts: DirectTCPDownloadOptions{
			MaxFileSize:      1 << 20,
			DialTimeout:      time.Second,
			FirstByteTimeout: time.Second,
			IdleTimeout:      time.Second,
			TotalTimeout:     3 * time.Second,
		},
		fileSize:      fileSize,
		fileSizeKnown: fileSizeKnown,
		release:       func() { released.Add(1) },
		done:          make(chan struct{}),
	}
	t.Cleanup(relay.Close)
	return relay, listener.Addr().String(), released
}

func startDirectTCPRelayTest(t *testing.T, relay *directTCPRelay) <-chan directTCPRelayResult {
	t.Helper()
	result := make(chan directTCPRelayResult, 1)
	if !relay.Start(func(terminal directTCPRelayResult) { result <- terminal }) {
		t.Fatal("relay did not start")
	}
	return result
}

func waitDirectTCPRelayTestResult(t *testing.T, result <-chan directTCPRelayResult) directTCPRelayResult {
	t.Helper()
	select {
	case terminal := <-result:
		return terminal
	case <-time.After(3 * time.Second):
		t.Fatal("direct TCP relay result timeout")
		return directTCPRelayResult{}
	}
}

func TestDirectTCPRelayReportsContextTimeoutBeforeStart(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	released := &atomic.Int32{}
	relay := &directTCPRelay{
		ctx: ctx, cancel: cancel, listener: listener,
		release: func() { released.Add(1) }, done: make(chan struct{}),
	}
	go relay.finishOnContextDone()
	cancel()
	select {
	case <-relay.done:
	case <-time.After(time.Second):
		t.Fatal("relay did not finish after pre-start context cancellation")
	}

	result := make(chan directTCPRelayResult, 1)
	if relay.Start(func(terminal directTCPRelayResult) { result <- terminal }) {
		t.Fatal("finished relay unexpectedly started")
	}
	terminal := waitDirectTCPRelayTestResult(t, result)
	if terminal.Reason != "cancelled" || !errors.Is(terminal.Err, context.Canceled) {
		t.Fatalf("terminal result = %+v, want cancelled", terminal)
	}
	if released.Load() != 1 {
		t.Fatalf("relay release count = %d, want 1", released.Load())
	}
	if relay.Start(func(directTCPRelayResult) { t.Error("duplicate start delivered terminal callback") }) {
		t.Fatal("duplicate start unexpectedly succeeded")
	}
}
