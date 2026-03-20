package api

import (
	"context"
	"os"
	"time"
)

// waitForFreshCover 等待封面文件更新到指定时间之后，避免抓拍接口返回旧图。
func waitForFreshCover(ctx context.Context, path string, after time.Time, timeout time.Duration) bool {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	check := func() bool {
		info, err := os.Stat(path)
		if err != nil {
			return false
		}
		return !info.ModTime().Before(after)
	}

	if check() {
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return check()
		case <-ticker.C:
			if check() {
				return true
			}
		}
	}
}
