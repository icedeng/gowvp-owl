package gbs

import (
	"math"
	"sync/atomic"
)

// nextControlSN 生成设备控制统一序列号。
//
// 说明：
// 1. PTZ 与 DeviceControl 共用 DeviceControl 响应通道。
// 2. 使用单调递增 SN 避免并发随机碰撞引起的串包。
func (g *GB28181API) nextControlSN() int {
	return nextPositiveSN(&g.controlSN)
}

// nextQuerySN 生成设备查询统一序列号，避免随机碰撞。
func (g *GB28181API) nextQuerySN() int {
	return nextPositiveSN(&g.querySN)
}

func nextPositiveSN(counter *atomic.Uint32) int {
	for {
		current := counter.Load()
		next := current + 1
		if next == 0 || next > math.MaxInt32 {
			next = 1
		}
		if counter.CompareAndSwap(current, next) {
			return int(next)
		}
	}
}
