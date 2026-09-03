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

// CompareAndSwapTrafficLogger 仅在全局日志器仍为 old 时替换，避免旧 Server 清理新代次日志器。
func CompareAndSwapTrafficLogger(old, new *TrafficLogger) bool {
	trafficLoggerMu.Lock()
	defer trafficLoggerMu.Unlock()
	if trafficLogger != old {
		return false
	}
	trafficLogger = new
	return true
}

func (l *TrafficLogger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		return nil
	}
	err := l.out.Close()
	l.out = nil
	return err
}

func (l *TrafficLogger) Log(direction, network string, src, dst net.Addr, payload []byte) {
	if l == nil || len(payload) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		return
	}

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
