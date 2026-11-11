package protocol

import (
	"math"
	"time"
)

// BitRate 用同代次单调计数器差值生成速率；首次采样、计数器重置或缺失保持 null。
func BitRate(previous, current *Decimal, elapsed time.Duration) *Decimal {
	if previous == nil || current == nil || elapsed <= 0 {
		return nil
	}
	before, e1 := previous.Int64()
	after, e2 := current.Int64()
	if e1 != nil || e2 != nil || after < before {
		return nil
	}
	rate := float64(after-before) * 8 / elapsed.Seconds()
	if rate >= float64(math.MaxInt64) {
		return nil
	}
	value := Number(int64(math.Round(rate)))
	return &value
}
