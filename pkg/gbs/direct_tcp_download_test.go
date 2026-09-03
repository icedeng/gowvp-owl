package gbs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/conc"
)

func TestDirectTCPDownloadManagerKnownSizeAndSHA256(t *testing.T) {
	payload := []byte("gb28181-direct-tcp-download")
	address := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write(payload)
	})
	manager := newTestDirectTCPManager(t)
	startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
		SessionID:     "known-size",
		DeviceID:      gb10DeviceID,
		ChannelID:     gb10ChannelID,
		Address:       address,
		RegisteredIP:  net.ParseIP("127.0.0.1"),
		FileSize:      int64(len(payload)),
		FileSizeKnown: true,
	})
	state := waitDirectTCPState(t, manager, "known-size")
	if state.Status != directTCPStatusCompleted || !state.SizeVerified || state.Received != int64(len(payload)) {
		t.Fatalf("download state = %+v", state)
	}
	wantHash := sha256.Sum256(payload)
	if state.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("SHA256 = %s; want %x", state.SHA256, wantHash)
	}
	if filepath.IsAbs(state.Output) {
		t.Fatalf("download state exposed absolute output path: %s", state.Output)
	}
	got, err := os.ReadFile(filepath.Join(manager.opts.StorageDir, state.Output))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("downloaded payload = %q", got)
	}
}

func TestDirectTCPDownloadManagerSkipsMediaStatusWaitWhenCapabilityDisabled(t *testing.T) {
	payload := []byte("gb28181-direct-tcp-without-media-status")
	written := make(chan struct{})
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write(payload)
		close(written)
		<-release
	})
	t.Cleanup(func() { close(release) })

	manager := newTestDirectTCPManager(t)
	manager.opts.IdleTimeout = time.Second
	startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
		SessionID:           "media-status-disabled",
		DeviceID:            gb10DeviceID,
		ChannelID:           gb10ChannelID,
		Address:             address,
		RegisteredIP:        net.ParseIP("127.0.0.1"),
		FileSize:            int64(len(payload)),
		FileSizeKnown:       true,
		MediaStatusDisabled: true,
	})
	<-written

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	state, err := manager.Wait(ctx, "media-status-disabled")
	if err != nil {
		t.Fatalf("download still waited for MediaStatus: %v", err)
	}
	if state.Status != directTCPStatusCompleted || state.EndReason != "size_reached" || !state.SizeVerified {
		t.Fatalf("download state = %+v", state)
	}
}

func TestDirectTCPDownloadManagerIPv6(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	payload := []byte("gb28181-direct-tcp-ipv6")
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write(payload)
	}()

	manager := newTestDirectTCPManager(t)
	startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
		SessionID: "ipv6", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		Address: listener.Addr().String(), RegisteredIP: net.ParseIP("::1"),
		FileSize: int64(len(payload)), FileSizeKnown: true,
	})
	state := waitDirectTCPState(t, manager, "ipv6")
	if state.Status != directTCPStatusCompleted || !state.SizeVerified || state.Received != int64(len(payload)) {
		t.Fatalf("IPv6 download state = %+v", state)
	}
}

func TestDirectTCPDownloadManagerUnknownSizeEOF(t *testing.T) {
	address := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("unknown-size"))
	})
	manager := newTestDirectTCPManager(t)
	startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
		SessionID:    "unknown-size",
		DeviceID:     gb10DeviceID,
		ChannelID:    gb10ChannelID,
		Address:      address,
		RegisteredIP: net.ParseIP("127.0.0.1"),
	})
	state := waitDirectTCPState(t, manager, "unknown-size")
	if state.Status != directTCPStatusCompleted || state.SizeVerified || state.EndReason != "eof" {
		t.Fatalf("unknown-size state = %+v", state)
	}
}

func TestDirectTCPDownloadManagerZeroByte(t *testing.T) {
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(net.Conn) { <-release })
	defer close(release)
	manager := newTestDirectTCPManager(t)
	startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
		SessionID:     "zero-byte",
		DeviceID:      gb10DeviceID,
		ChannelID:     gb10ChannelID,
		Address:       address,
		RegisteredIP:  net.ParseIP("127.0.0.1"),
		FileSizeKnown: true,
	})
	state := waitDirectTCPState(t, manager, "zero-byte")
	if state.Status != directTCPStatusCompleted || !state.SizeVerified || state.Received != 0 {
		t.Fatalf("zero-byte state = %+v", state)
	}
}

func TestDirectTCPDownloadManagerRejectsEarlyEOFAndExcess(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		size    int64
		reason  string
	}{
		{name: "early EOF", payload: []byte("short"), size: 10, reason: "early_eof"},
		{name: "excess", payload: []byte("too-long"), size: 3, reason: "declared_size_exceeded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address := startDirectTCPFixture(t, func(conn net.Conn) { _, _ = conn.Write(tt.payload) })
			manager := newTestDirectTCPManager(t)
			startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
				SessionID:     tt.name,
				DeviceID:      gb10DeviceID,
				ChannelID:     gb10ChannelID,
				Address:       address,
				RegisteredIP:  net.ParseIP("127.0.0.1"),
				FileSize:      tt.size,
				FileSizeKnown: true,
			})
			state := waitDirectTCPState(t, manager, tt.name)
			if state.Status != directTCPStatusFailed || state.EndReason != tt.reason || state.Output != "" {
				t.Fatalf("failed download state = %+v", state)
			}
		})
	}
}

func TestDirectTCPDownloadManagerTimeoutCancelAndMediaStatus(t *testing.T) {
	t.Run("connect failure", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		manager := newTestDirectTCPManager(t)
		startDirectTCPTestDownload(t, manager, directTCPRequest("connect-failure", address))
		state := waitDirectTCPState(t, manager, "connect-failure")
		if state.Status != directTCPStatusFailed || state.EndReason != "connect_failed" {
			t.Fatalf("connect-failure state = %+v", state)
		}
	})

	t.Run("first byte timeout", func(t *testing.T) {
		release := make(chan struct{})
		address := startDirectTCPFixture(t, func(net.Conn) { <-release })
		defer close(release)
		manager := newTestDirectTCPManager(t)
		manager.opts.FirstByteTimeout = 30 * time.Millisecond
		startDirectTCPTestDownload(t, manager, directTCPRequest("first-byte", address))
		state := waitDirectTCPState(t, manager, "first-byte")
		if state.Status != directTCPStatusFailed || state.EndReason != "first_byte_timeout" {
			t.Fatalf("timeout state = %+v", state)
		}
	})

	t.Run("idle timeout", func(t *testing.T) {
		release := make(chan struct{})
		address := startDirectTCPFixture(t, func(conn net.Conn) {
			_, _ = conn.Write([]byte("x"))
			<-release
		})
		defer close(release)
		manager := newTestDirectTCPManager(t)
		manager.opts.IdleTimeout = 30 * time.Millisecond
		startDirectTCPTestDownload(t, manager, directTCPRequest("idle-timeout", address))
		state := waitDirectTCPState(t, manager, "idle-timeout")
		if state.Status != directTCPStatusFailed || state.EndReason != "idle_timeout" {
			t.Fatalf("idle-timeout state = %+v", state)
		}
	})

	t.Run("total timeout", func(t *testing.T) {
		release := make(chan struct{})
		address := startDirectTCPFixture(t, func(net.Conn) { <-release })
		defer close(release)
		manager := newTestDirectTCPManager(t)
		manager.opts.TotalTimeout = 30 * time.Millisecond
		manager.opts.FirstByteTimeout = time.Second
		startDirectTCPTestDownload(t, manager, directTCPRequest("total-timeout", address))
		state := waitDirectTCPState(t, manager, "total-timeout")
		if state.Status != directTCPStatusFailed || state.EndReason != "total_timeout" {
			t.Fatalf("total-timeout state = %+v", state)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		release := make(chan struct{})
		address := startDirectTCPFixture(t, func(net.Conn) { <-release })
		defer close(release)
		manager := newTestDirectTCPManager(t)
		startDirectTCPTestDownload(t, manager, directTCPRequest("cancel", address))
		waitDirectTCPReceiving(t, manager, "cancel")
		if !manager.Cancel("cancel") {
			t.Fatal("Cancel returned false")
		}
		state := waitDirectTCPState(t, manager, "cancel")
		if state.Status != directTCPStatusCancelled || state.EndReason != "cancelled" {
			t.Fatalf("cancel state = %+v", state)
		}
	})

	t.Run("cancel device", func(t *testing.T) {
		targetRelease := make(chan struct{})
		targetAddress := startDirectTCPFixture(t, func(net.Conn) { <-targetRelease })
		defer close(targetRelease)
		otherRelease := make(chan struct{})
		otherAddress := startDirectTCPFixture(t, func(net.Conn) { <-otherRelease })
		defer close(otherRelease)
		manager := newTestDirectTCPManager(t)
		target := directTCPRequest("cancel-device-target", targetAddress)
		other := directTCPRequest("cancel-device-other", otherAddress)
		other.DeviceID = "34020000001320000009"
		startDirectTCPTestDownload(t, manager, target)
		startDirectTCPTestDownload(t, manager, other)
		waitDirectTCPReceiving(t, manager, target.SessionID)
		waitDirectTCPReceiving(t, manager, other.SessionID)

		if count := manager.CancelDevice(target.DeviceID); count != 1 {
			t.Fatalf("cancelled device sessions = %d; want 1", count)
		}
		state := waitDirectTCPState(t, manager, target.SessionID)
		if state.Status != directTCPStatusCancelled {
			t.Fatalf("target device state = %+v", state)
		}
		if state, ok := manager.State(other.SessionID); !ok || state.Status != directTCPStatusReceiving {
			t.Fatalf("other device state = %+v, exists=%v", state, ok)
		}
		manager.CancelDevice(other.DeviceID)
		_ = waitDirectTCPState(t, manager, other.SessionID)
	})

	t.Run("cancel channel", func(t *testing.T) {
		targetRelease := make(chan struct{})
		targetAddress := startDirectTCPFixture(t, func(net.Conn) { <-targetRelease })
		defer close(targetRelease)
		otherRelease := make(chan struct{})
		otherAddress := startDirectTCPFixture(t, func(net.Conn) { <-otherRelease })
		defer close(otherRelease)
		manager := newTestDirectTCPManager(t)
		manager.opts.DeviceConcurrency = 2
		target := directTCPRequest("cancel-channel-target", targetAddress)
		other := directTCPRequest("cancel-channel-other", otherAddress)
		other.ChannelID = "34020000001320000003"
		startDirectTCPTestDownload(t, manager, target)
		startDirectTCPTestDownload(t, manager, other)
		waitDirectTCPReceiving(t, manager, target.SessionID)
		waitDirectTCPReceiving(t, manager, other.SessionID)

		if count := manager.CancelChannel(target.DeviceID, target.ChannelID); count != 1 {
			t.Fatalf("cancelled channel sessions = %d; want 1", count)
		}
		state := waitDirectTCPState(t, manager, target.SessionID)
		if state.Status != directTCPStatusCancelled {
			t.Fatalf("target channel state = %+v", state)
		}
		if state, ok := manager.State(other.SessionID); !ok || state.Status != directTCPStatusReceiving {
			t.Fatalf("other channel state = %+v, exists=%v", state, ok)
		}
		manager.CancelChannel(other.DeviceID, other.ChannelID)
		_ = waitDirectTCPState(t, manager, other.SessionID)
	})

	t.Run("media status", func(t *testing.T) {
		release := make(chan struct{})
		address := startDirectTCPFixture(t, func(conn net.Conn) {
			_, _ = conn.Write([]byte("partial-without-size"))
			<-release
		})
		defer close(release)
		manager := newTestDirectTCPManager(t)
		startDirectTCPTestDownload(t, manager, directTCPRequest("media-status", address))
		waitDirectTCPBytes(t, manager, "media-status")
		if manager.NotifySenderFinishedForDevice("media-status", "34020000001320009999") {
			t.Fatal("another device stopped the direct TCP download")
		}
		if !manager.NotifySenderFinishedForDevice("media-status", gb10DeviceID) {
			t.Fatal("NotifySenderFinished returned false")
		}
		state := waitDirectTCPState(t, manager, "media-status")
		if state.Status != directTCPStatusCompleted || state.EndReason != "media_status" || state.SizeVerified {
			t.Fatalf("media-status state = %+v", state)
		}
	})

	t.Run("known size waits for media status", func(t *testing.T) {
		payload := []byte("complete-before-media-status")
		release := make(chan struct{})
		address := startDirectTCPFixture(t, func(conn net.Conn) {
			_, _ = conn.Write(payload)
			<-release
		})
		defer close(release)
		manager := newTestDirectTCPManager(t)
		startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
			SessionID:     "known-size-media-status",
			DeviceID:      gb10DeviceID,
			ChannelID:     gb10ChannelID,
			Address:       address,
			RegisteredIP:  net.ParseIP("127.0.0.1"),
			FileSize:      int64(len(payload)),
			FileSizeKnown: true,
		})
		waitDirectTCPBytes(t, manager, "known-size-media-status")
		if state, ok := manager.State("known-size-media-status"); !ok || state.Status != directTCPStatusReceiving {
			t.Fatalf("known-size download completed before MediaStatus: state=%+v exists=%v", state, ok)
		}
		if !manager.NotifySenderFinishedForDevice("known-size-media-status", gb10DeviceID) {
			t.Fatal("NotifySenderFinished returned false")
		}
		state := waitDirectTCPState(t, manager, "known-size-media-status")
		if state.Status != directTCPStatusCompleted || state.EndReason != "media_status" || !state.SizeVerified {
			t.Fatalf("known-size media-status state = %+v", state)
		}
	})

	t.Run("known size idle fallback", func(t *testing.T) {
		payload := []byte("complete-without-media-status")
		release := make(chan struct{})
		address := startDirectTCPFixture(t, func(conn net.Conn) {
			_, _ = conn.Write(payload)
			<-release
		})
		defer close(release)
		manager := newTestDirectTCPManager(t)
		manager.opts.IdleTimeout = 30 * time.Millisecond
		startDirectTCPTestDownload(t, manager, DirectTCPDownloadRequest{
			SessionID:     "known-size-idle-fallback",
			DeviceID:      gb10DeviceID,
			ChannelID:     gb10ChannelID,
			Address:       address,
			RegisteredIP:  net.ParseIP("127.0.0.1"),
			FileSize:      int64(len(payload)),
			FileSizeKnown: true,
		})
		state := waitDirectTCPState(t, manager, "known-size-idle-fallback")
		if state.Status != directTCPStatusCompleted || state.EndReason != "size_reached" || !state.SizeVerified {
			t.Fatalf("known-size idle fallback state = %+v", state)
		}
	})
}

func TestDirectTCPDownloadManagerLimitsAndAddressPolicy(t *testing.T) {
	manager := newTestDirectTCPManager(t)
	if err := manager.Start(context.Background(), DirectTCPDownloadRequest{
		SessionID: "too-large", DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
		Address: "127.0.0.1:1", RegisteredIP: net.ParseIP("127.0.0.1"),
		FileSize: manager.opts.MaxFileSize + 1, FileSizeKnown: true,
	}); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("large file error = %v", err)
	}
	unsafeOptions := manager.opts
	unsafeOptions.AllowUnsafeAddresses = false
	if err := validateDirectTCPAddress("127.0.0.1:9000", net.ParseIP("127.0.0.1"), unsafeOptions); err == nil {
		t.Fatal("loopback address must be rejected in production policy")
	}
	if err := validateDirectTCPAddress("192.0.2.20:9000", net.ParseIP("192.0.2.21"), unsafeOptions); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("address mismatch error = %v", err)
	}
	unsafeOptions.AllowAddressMismatch = true
	if err := validateDirectTCPAddress("192.0.2.20:9000", net.ParseIP("192.0.2.21"), unsafeOptions); err == nil || !strings.Contains(err.Error(), "allowed address CIDRs") {
		t.Fatalf("empty CIDR allowlist error = %v", err)
	}
	unsafeOptions.AllowedAddressCIDRs = []string{"192.0.2.0/24"}
	if err := validateDirectTCPAddress("192.0.2.20:9000", net.ParseIP("192.0.2.21"), unsafeOptions); err != nil {
		t.Fatalf("CIDR-allowed mismatch rejected: %v", err)
	}
	if err := validateDirectTCPAddress("[2001:db8::20]:9000", net.ParseIP("2001:db8::20"), unsafeOptions); err != nil {
		t.Fatalf("matching IPv6 address rejected: %v", err)
	}
	if err := validateDirectTCPAddress("[fe80::1]:9000", net.ParseIP("fe80::1"), unsafeOptions); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("link-local IPv6 error = %v", err)
	}
	unsafeOptions.AllowedAddressCIDRs = []string{"2001:db8::/32"}
	if err := validateDirectTCPAddress("[2001:db8::20]:9000", net.ParseIP("2001:db9::21"), unsafeOptions); err != nil {
		t.Fatalf("CIDR-allowed IPv6 mismatch rejected: %v", err)
	}

	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(net.Conn) { <-release })
	defer close(release)
	manager.opts.GlobalConcurrency = 1
	manager.opts.DeviceConcurrency = 1
	startDirectTCPTestDownload(t, manager, directTCPRequest("limit-1", address))
	waitDirectTCPReceiving(t, manager, "limit-1")
	second := directTCPRequest("limit-2", address)
	second.DeviceID = "34020000001320000009"
	if err := manager.Start(context.Background(), second); err == nil || !strings.Contains(err.Error(), "global concurrency") {
		t.Fatalf("global concurrency error = %v", err)
	}
	manager.opts.GlobalConcurrency = 2
	if err := manager.Start(context.Background(), directTCPRequest("limit-3", address)); err == nil || !strings.Contains(err.Error(), "device concurrency") {
		t.Fatalf("device concurrency error = %v", err)
	}
	manager.Cancel("limit-1")
	_ = waitDirectTCPState(t, manager, "limit-1")
}

func TestDirectTCPDownloadManagerTerminalRaceIsIdempotent(t *testing.T) {
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(conn net.Conn) {
		_, _ = conn.Write([]byte("race"))
		<-release
	})
	defer close(release)
	manager := newTestDirectTCPManager(t)
	var callbacks atomic.Int32
	req := directTCPRequest("terminal-race", address)
	req.OnFinish = func(DirectTCPDownloadState) { callbacks.Add(1) }
	startDirectTCPTestDownload(t, manager, req)
	waitDirectTCPBytes(t, manager, req.SessionID)
	var group sync.WaitGroup
	group.Add(3)
	go func() { defer group.Done(); manager.Cancel(req.SessionID) }()
	go func() { defer group.Done(); manager.NotifySenderFinished(req.SessionID) }()
	go func() { defer group.Done(); manager.NotifySenderFinished(req.SessionID) }()
	group.Wait()
	state := waitDirectTCPState(t, manager, req.SessionID)
	if state.Status != directTCPStatusCancelled && state.Status != directTCPStatusCompleted {
		t.Fatalf("terminal race state = %+v", state)
	}
	deadline := time.Now().Add(time.Second)
	for callbacks.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if callbacks.Load() != 1 {
		t.Fatalf("OnFinish callbacks = %d; want 1", callbacks.Load())
	}
}

func TestDirectTCPDownloadManagerWaitAcceptsNilContext(t *testing.T) {
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(net.Conn) { <-release })
	t.Cleanup(func() { close(release) })
	manager := newTestDirectTCPManager(t)
	req := directTCPRequest("wait-nil-context", address)
	startDirectTCPTestDownload(t, manager, req)
	waitDirectTCPReceiving(t, manager, req.SessionID)
	time.AfterFunc(20*time.Millisecond, func() { manager.Cancel(req.SessionID) })
	if _, err := manager.Wait(nil, req.SessionID); err != nil {
		t.Fatalf("Wait(nil) error = %v", err)
	}
}

func TestDirectTCPDownloadManagerShutdownWaitsAndRejectsNewDownloads(t *testing.T) {
	serverRelease := make(chan struct{})
	address := startDirectTCPFixture(t, func(net.Conn) { <-serverRelease })
	t.Cleanup(func() { close(serverRelease) })
	manager := newTestDirectTCPManager(t)
	finishStarted := make(chan struct{})
	finishRelease := make(chan struct{})
	req := directTCPRequest("shutdown", address)
	req.OnFinish = func(DirectTCPDownloadState) {
		close(finishStarted)
		<-finishRelease
	}
	startDirectTCPTestDownload(t, manager, req)
	waitDirectTCPReceiving(t, manager, req.SessionID)

	shutdownDone := make(chan struct{})
	go func() {
		manager.Shutdown()
		close(shutdownDone)
	}()
	select {
	case <-finishStarted:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel the active download")
	}
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before the active download exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(finishRelease)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not return after the active download exited")
	}

	state, ok := manager.State(req.SessionID)
	if !ok || state.Status != directTCPStatusCancelled {
		t.Fatalf("shutdown state = %+v, exists=%v", state, ok)
	}
	if err := manager.Start(context.Background(), directTCPRequest("after-shutdown", address)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("start after shutdown error = %v", err)
	}
	manager.Shutdown()
}

func TestGB28181APICloseWaitsForDirectTCPDownloads(t *testing.T) {
	serverRelease := make(chan struct{})
	address := startDirectTCPFixture(t, func(net.Conn) { <-serverRelease })
	t.Cleanup(func() { close(serverRelease) })
	manager := newTestDirectTCPManager(t)
	finishStarted := make(chan struct{})
	finishRelease := make(chan struct{})
	req := directTCPRequest("api-close", address)
	req.OnFinish = func(DirectTCPDownloadState) {
		close(finishStarted)
		<-finishRelease
	}
	startDirectTCPTestDownload(t, manager, req)
	waitDirectTCPReceiving(t, manager, req.SessionID)

	api := &GB28181API{directDownloads: manager, lifecycleDone: make(chan struct{})}
	closeDone := make(chan struct{})
	go func() {
		api.close()
		close(closeDone)
	}()
	select {
	case <-finishStarted:
	case <-time.After(time.Second):
		t.Fatal("GB28181 close did not cancel the direct TCP download")
	}
	select {
	case <-closeDone:
		t.Fatal("GB28181 close returned before the direct TCP download exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(finishRelease)
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("GB28181 close did not wait for the direct TCP download")
	}
	if err := manager.Start(context.Background(), directTCPRequest("after-api-close", address)); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("start after GB28181 close error = %v", err)
	}
}

func TestDirectTCPDownloadManagerDefersReconfigureUntilIdle(t *testing.T) {
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(net.Conn) { <-release })
	defer close(release)
	manager := newTestDirectTCPManager(t)
	oldLimit := manager.opts.MaxFileSize
	startDirectTCPTestDownload(t, manager, directTCPRequest("reconfigure", address))
	waitDirectTCPReceiving(t, manager, "reconfigure")
	updated := manager.opts
	updated.MaxFileSize = 123
	manager.Reconfigure(updated)
	manager.mu.RLock()
	currentLimit := manager.opts.MaxFileSize
	pending := manager.pendingOpts != nil
	manager.mu.RUnlock()
	if currentLimit != oldLimit || !pending {
		t.Fatalf("active reconfigure applied early: current=%d pending=%v", currentLimit, pending)
	}
	manager.Cancel("reconfigure")
	_ = waitDirectTCPState(t, manager, "reconfigure")
	manager.mu.RLock()
	currentLimit = manager.opts.MaxFileSize
	pending = manager.pendingOpts != nil
	manager.mu.RUnlock()
	if currentLimit != 123 || pending {
		t.Fatalf("idle reconfigure not applied: current=%d pending=%v", currentLimit, pending)
	}
}

func TestDirectTCPDownloadManagerRetentionOnlyRemovesManagedFiles(t *testing.T) {
	manager := newTestDirectTCPManager(t)
	old := time.Now().Add(-48 * time.Hour)
	managed := []string{"old.ps", ".old.ps-123.part"}
	for _, name := range managed {
		path := filepath.Join(manager.opts.StorageDir, name)
		if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	unmanaged := filepath.Join(manager.opts.StorageDir, "keep.txt")
	if err := os.WriteFile(unmanaged, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unmanaged, old, old); err != nil {
		t.Fatal(err)
	}
	manager.cleanupExpiredFiles(time.Now(), manager.opts, nil)
	for _, name := range managed {
		if _, err := os.Stat(filepath.Join(manager.opts.StorageDir, name)); !os.IsNotExist(err) {
			t.Fatalf("managed file %s was not removed: %v", name, err)
		}
	}
	if _, err := os.Stat(unmanaged); err != nil {
		t.Fatalf("unmanaged file was removed: %v", err)
	}
}

func TestDirectTCPDownloadManagerCleanupExpiresOnlyTerminalStates(t *testing.T) {
	manager := newTestDirectTCPManager(t)
	now := time.Now()
	manager.states["expired"] = DirectTCPDownloadState{
		SessionID: "expired", StartedAt: now.Add(-48 * time.Hour),
		CompletedAt: now.Add(-25 * time.Hour), Status: directTCPStatusCompleted,
	}
	manager.states["recent"] = DirectTCPDownloadState{
		SessionID: "recent", StartedAt: now.Add(-time.Hour),
		CompletedAt: now.Add(-time.Minute), Status: directTCPStatusCompleted,
	}
	manager.states["active"] = DirectTCPDownloadState{
		SessionID: "active", StartedAt: now.Add(-48 * time.Hour), Status: directTCPStatusReceiving,
	}
	manager.active["active"] = &directTCPDownloadSession{}

	manager.Cleanup(now)

	if _, ok := manager.State("expired"); ok {
		t.Fatal("expired Direct TCP terminal state was retained")
	}
	if _, ok := manager.State("recent"); !ok {
		t.Fatal("recent Direct TCP terminal state was removed")
	}
	if _, ok := manager.State("active"); !ok {
		t.Fatal("active Direct TCP state was removed")
	}
}

func TestDirectTCPDownloadManagerCleanupBoundsTerminalStates(t *testing.T) {
	manager := newTestDirectTCPManager(t)
	now := time.Now()
	for i := 0; i < directTCPMaxTerminalStates+2; i++ {
		sessionID := fmt.Sprintf("terminal-%04d", i)
		completedAt := now.Add(time.Duration(i) * time.Nanosecond)
		manager.states[sessionID] = DirectTCPDownloadState{
			SessionID: sessionID, Status: directTCPStatusCompleted,
			StartedAt: completedAt.Add(-time.Second), UpdatedAt: completedAt, CompletedAt: completedAt,
		}
	}
	manager.states["active"] = DirectTCPDownloadState{
		SessionID: "active", Status: directTCPStatusReceiving, StartedAt: now,
	}
	manager.active["active"] = &directTCPDownloadSession{}

	manager.Cleanup(now.Add(time.Second))

	manager.mu.RLock()
	terminalCount := len(manager.states) - len(manager.active)
	_, oldestExists := manager.states["terminal-0000"]
	_, activeExists := manager.states["active"]
	manager.mu.RUnlock()
	if terminalCount != directTCPMaxTerminalStates {
		t.Fatalf("Direct TCP terminal states = %d; want %d", terminalCount, directTCPMaxTerminalStates)
	}
	if oldestExists {
		t.Fatal("oldest Direct TCP terminal state was retained")
	}
	if !activeExists {
		t.Fatal("active Direct TCP state was removed by capacity cleanup")
	}
}

func TestDirectTCPDownloadManagerCleanupProtectsActivePartialFile(t *testing.T) {
	manager := newTestDirectTCPManager(t)
	partial := filepath.Join(manager.opts.StorageDir, ".active.ps-123.part")
	if err := os.WriteFile(partial, []byte("active"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(partial, old, old); err != nil {
		t.Fatal(err)
	}
	session := &directTCPDownloadSession{}
	session.setTempPath(partial)
	manager.active["active"] = session
	manager.states["active"] = DirectTCPDownloadState{SessionID: "active", Status: directTCPStatusReceiving}

	manager.Cleanup(time.Now())
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("active partial file was removed: %v", err)
	}

	delete(manager.active, "active")
	manager.Cleanup(time.Now())
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("inactive expired partial file was retained: %v", err)
	}
}

func TestDirectTCPStorageRejectsSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureDirectTCPStorageRoot(link); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink storage error = %v", err)
	}
}

func TestDirectTCPCompletionDoesNotDeleteReplacementSession(t *testing.T) {
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	oldStream := &Streams{DirectTCP: true, DirectSessionID: "old"}
	newStream := &Streams{DirectTCP: true, DirectSessionID: "new"}
	streams := &conc.Map[string, *Streams]{}
	streams.Store(key, newStream)
	api := &GB28181API{streams: streams}
	api.finishDirectTCPHistory(key, nil, oldStream, DirectTCPDownloadState{Status: directTCPStatusCancelled, EndReason: "cancelled"})
	got, ok := streams.Load(key)
	if !ok || got != newStream {
		t.Fatalf("old completion removed replacement session: got=%p ok=%v", got, ok)
	}
	if metric := api.metrics.Snapshot().DirectCancelled; metric != 1 {
		t.Fatalf("direct cancelled metric = %d; want 1", metric)
	}
}

func TestDirectTCPRuntimePolicyConcurrentRefresh(t *testing.T) {
	api := &GB28181API{}
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(2)
		go func(index int) {
			defer group.Done()
			api.applyDirectTCPConfig(conf.SIPDirectTCPDownload{
				Enabled:         index%2 == 0,
				OfferPort:       9000 + index,
				DeviceAllowlist: []string{gb10DeviceID},
			})
		}(i)
		go func() {
			defer group.Done()
			policy := api.directTCPPolicySnapshot()
			if policy.OfferPort != 0 && (policy.OfferPort < 9000 || policy.OfferPort > 9019) {
				t.Errorf("unexpected policy snapshot: %+v", policy)
			}
		}()
	}
	group.Wait()
	policy := api.directTCPPolicySnapshot()
	if _, ok := policy.Allowlist[gb10DeviceID]; !ok {
		t.Fatalf("policy allowlist = %+v", policy)
	}
}

func TestDisablingDirectTCPPolicyCancelsActiveDownloads(t *testing.T) {
	release := make(chan struct{})
	address := startDirectTCPFixture(t, func(net.Conn) { <-release })
	defer close(release)
	manager := newTestDirectTCPManager(t)
	api := &GB28181API{directDownloads: manager}
	api.applyDirectTCPConfig(conf.SIPDirectTCPDownload{Enabled: true, DeviceAllowlist: []string{gb10DeviceID}})
	startDirectTCPTestDownload(t, manager, directTCPRequest("disable-policy", address))
	waitDirectTCPReceiving(t, manager, "disable-policy")
	api.applyDirectTCPConfig(conf.SIPDirectTCPDownload{Enabled: false})
	state := waitDirectTCPState(t, manager, "disable-policy")
	if state.Status != directTCPStatusCancelled {
		t.Fatalf("disabled policy state = %+v", state)
	}
}

func newTestDirectTCPManager(t *testing.T) *DirectTCPDownloadManager {
	t.Helper()
	return NewDirectTCPDownloadManager(DirectTCPDownloadOptions{
		StorageDir:           t.TempDir(),
		RetainDays:           1,
		MaxFileSize:          1 << 20,
		GlobalConcurrency:    4,
		DeviceConcurrency:    1,
		DialTimeout:          time.Second,
		FirstByteTimeout:     time.Second,
		IdleTimeout:          time.Second,
		TotalTimeout:         2 * time.Second,
		AllowUnsafeAddresses: true,
	})
}

func directTCPRequest(sessionID, address string) DirectTCPDownloadRequest {
	return DirectTCPDownloadRequest{
		SessionID:    sessionID,
		DeviceID:     gb10DeviceID,
		ChannelID:    gb10ChannelID,
		Address:      address,
		RegisteredIP: net.ParseIP("127.0.0.1"),
	}
}

func startDirectTCPTestDownload(t *testing.T, manager *DirectTCPDownloadManager, req DirectTCPDownloadRequest) {
	t.Helper()
	if err := manager.Start(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func waitDirectTCPState(t *testing.T, manager *DirectTCPDownloadManager, sessionID string) DirectTCPDownloadState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	state, err := manager.Wait(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func waitDirectTCPReceiving(t *testing.T, manager *DirectTCPDownloadManager, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if state, ok := manager.State(sessionID); ok && state.Status == directTCPStatusReceiving {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s did not start receiving", sessionID)
}

func waitDirectTCPBytes(t *testing.T, manager *DirectTCPDownloadManager, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if state, ok := manager.State(sessionID); ok && state.Received > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("session %s did not receive bytes", sessionID)
}

func startDirectTCPFixture(t *testing.T, handler func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		handler(conn)
	}()
	return listener.Addr().String()
}
