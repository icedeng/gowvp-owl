package sip

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	rotatelogs "github.com/lestrrat-go/file-rotatelogs"
)

type TrafficLogConfig struct {
	Enabled      bool
	Dir          string
	MaxAge       time.Duration
	RotationTime time.Duration
	RotationSize int64
}

type TrafficLogger struct {
	mu  sync.Mutex
	out *rotatelogs.RotateLogs
}

var (
	trafficLoggerMu sync.RWMutex
	trafficLogger   *TrafficLogger
)

func NewTrafficLogger(cfg TrafficLogConfig) (*TrafficLogger, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	dir := strings.TrimSpace(cfg.Dir)
	if dir == "" {
		dir = "./logs/sip"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = 7 * 24 * time.Hour
	}
	if cfg.RotationTime <= 0 {
		cfg.RotationTime = 12 * time.Hour
	}
	if cfg.RotationSize <= 0 {
		cfg.RotationSize = 50 * 1024 * 1024
	}

	out, err := rotatelogs.New(
		filepath.Join(dir, "sip_%Y%m%d_%H_%M_%S.log"),
		rotatelogs.WithMaxAge(cfg.MaxAge),
		rotatelogs.WithRotationTime(cfg.RotationTime),
		rotatelogs.WithRotationSize(cfg.RotationSize),
	)
	if err != nil {
		return nil, err
	}
	return &TrafficLogger{out: out}, nil
}

func SetTrafficLogger(logger *TrafficLogger) (previous *TrafficLogger) {
	trafficLoggerMu.Lock()
	defer trafficLoggerMu.Unlock()
	previous = trafficLogger
	trafficLogger = logger
	return previous
}

func (l *TrafficLogger) Close() error {
	if l == nil || l.out == nil {
		return nil
	}
	return l.out.Close()
}

func (l *TrafficLogger) Log(direction, network string, src, dst net.Addr, payload []byte) {
	if l == nil || l.out == nil || len(payload) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	_, _ = fmt.Fprintf(
		l.out,
		"===== %s direction=%s network=%s src=%s dst=%s bytes=%d =====\n%s\n\n",
		time.Now().Format("2006-01-02 15:04:05.000"),
		strings.ToUpper(strings.TrimSpace(direction)),
		strings.TrimSpace(network),
		addrString(src),
		addrString(dst),
		len(payload),
		string(payload),
	)
}

func logTraffic(direction, network string, src, dst net.Addr, payload []byte) {
	trafficLoggerMu.RLock()
	logger := trafficLogger
	trafficLoggerMu.RUnlock()
	if logger == nil {
		return
	}
	logger.Log(direction, network, src, dst, payload)
}

func addrString(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	return addr.String()
}
