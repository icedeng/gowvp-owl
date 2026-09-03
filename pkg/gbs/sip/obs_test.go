package sip

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitObserverRegistration(t *testing.T, observer *Observer, key string) *observerRegistration {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if registration, ok := observer.data.Load(key); ok {
			return registration
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("observer registration %q was not installed", key)
	return nil
}

func waitObserverDone(t *testing.T, done <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestObserverNotifyCompletesRegistration(t *testing.T) {
	observer := NewObserver()
	done := make(chan struct{})
	go func() {
		observer.Register("wait", time.Second, func(deviceID string, _ ...string) bool {
			return deviceID == "device"
		})
		close(done)
	}()
	waitObserverRegistration(t, observer, "wait")

	observer.Notify("device")
	waitObserverDone(t, done, "observer registration did not complete")
	if _, ok := observer.data.Load("wait"); ok {
		t.Fatal("completed observer registration was retained")
	}
}

func TestObserverLateNotificationAfterTimeoutDoesNotPanic(t *testing.T) {
	observer := NewObserver()
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	registerDone := make(chan struct{})
	go func() {
		observer.Register("wait", 20*time.Millisecond, func(string, ...string) bool {
			close(handlerStarted)
			<-releaseHandler
			return true
		})
		close(registerDone)
	}()
	waitObserverRegistration(t, observer, "wait")

	notifyDone := make(chan struct{})
	panicValue := make(chan any, 1)
	go func() {
		defer close(notifyDone)
		defer func() { panicValue <- recover() }()
		observer.Notify("device")
	}()
	waitObserverDone(t, handlerStarted, "observer handler did not start")
	waitObserverDone(t, registerDone, "observer timeout remained blocked by an in-flight notification")
	close(releaseHandler)
	waitObserverDone(t, notifyDone, "late observer notification did not return")
	if recovered := <-panicValue; recovered != nil {
		t.Fatalf("late observer notification panicked: %v", recovered)
	}
}

func TestObserverExpiredGenerationCannotDeleteReplacement(t *testing.T) {
	observer := NewObserver()
	firstDone := make(chan struct{})
	go func() {
		observer.Register("shared", 20*time.Millisecond, func(string, ...string) bool { return false })
		close(firstDone)
	}()
	first := waitObserverRegistration(t, observer, "shared")

	secondDone := make(chan struct{})
	go func() {
		observer.Register("shared", time.Second, func(deviceID string, _ ...string) bool {
			return deviceID == "replacement"
		})
		close(secondDone)
	}()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if current, ok := observer.data.Load("shared"); ok && current != first {
			break
		}
		time.Sleep(time.Millisecond)
	}
	current, ok := observer.data.Load("shared")
	if !ok || current == first {
		t.Fatal("replacement observer registration was not installed")
	}
	waitObserverDone(t, firstDone, "replaced observer generation did not stop")

	observer.Notify("replacement")
	waitObserverDone(t, secondDone, "expired observer generation deleted its replacement")
}

func TestObserverConcurrentNotifyCompletesHandlerOnce(t *testing.T) {
	observer := NewObserver()
	registerDone := make(chan struct{})
	var calls atomic.Int32
	go func() {
		observer.Register("wait", time.Second, func(string, ...string) bool {
			calls.Add(1)
			return true
		})
		close(registerDone)
	}()
	waitObserverRegistration(t, observer, "wait")

	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			observer.Notify("device")
		}()
	}
	wait.Wait()
	waitObserverDone(t, registerDone, "concurrent observer notifications did not complete")
	if got := calls.Load(); got != 1 {
		t.Fatalf("observer handler calls = %d, want 1", got)
	}
}

func TestObserverQueuesMatchingNotificationBehindInFlightHandler(t *testing.T) {
	observer := NewObserver()
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	registerDone := make(chan struct{})
	go func() {
		observer.Register("wait", time.Second, func(deviceID string, _ ...string) bool {
			if deviceID == "unrelated" {
				close(handlerStarted)
				<-releaseHandler
			}
			return deviceID == "expected"
		})
		close(registerDone)
	}()
	waitObserverRegistration(t, observer, "wait")

	unrelatedDone := make(chan struct{})
	go func() {
		observer.Notify("unrelated")
		close(unrelatedDone)
	}()
	waitObserverDone(t, handlerStarted, "unrelated observer handler did not start")
	observer.Notify("expected")
	close(releaseHandler)
	waitObserverDone(t, unrelatedDone, "observer did not drain queued matching notification")
	waitObserverDone(t, registerDone, "queued matching notification was lost")
}

func TestObserverHandlerCanNotifyWithoutDeadlock(t *testing.T) {
	observer := NewObserver()
	registerDone := make(chan struct{})
	go func() {
		observer.Register("wait", time.Second, func(string, ...string) bool {
			observer.Notify("nested")
			return true
		})
		close(registerDone)
	}()
	waitObserverRegistration(t, observer, "wait")

	observer.Notify("device")
	waitObserverDone(t, registerDone, "reentrant observer notification deadlocked")
}
