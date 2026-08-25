package sip

import (
	"sync"
	"testing"
	"time"
)

func TestTrafficLoggerConcurrentLogAndClose(t *testing.T) {
	logger, err := NewTrafficLogger(TrafficLogConfig{
		Enabled: true, Dir: t.TempDir(), MaxAge: time.Hour, RotationTime: time.Hour, RotationSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}

	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := 0; i < 100; i++ {
				logger.Log("in", "udp", nil, nil, []byte("OPTIONS sip:test@example.com SIP/2.0\r\n\r\n"))
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		_ = logger.Close()
		_ = logger.Close()
	}()
	group.Wait()
}
