package stat

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
)

type UsageStat struct {
	Name      string `json:"name"`       // 路径
	Unit      string `json:"unit"`       // 单位
	Size      string `json:"size"`       // 大小
	FreeSpace string `json:"free_space"` // 可用空间
	Used      string `json:"used"`       // 已使用
	Percent   string `json:"percent"`    // 比率
	Threshold string `json:"threshold"`  // 阈值
}

const (
	TopQueneCap = 30
)

var (
	currentMu sync.RWMutex
	memData   = NewCircleQueue(TopQueneCap)
	cpuData   = NewCircleQueue(TopQueneCap)
	// netUpData         = NewCircleQueue(TopQueneCap)
	netData           = NewCircleQueue(TopQueneCap)
	currentMem        float64
	currentCPU        float64
	currentMainDisk   uint64
	totalMainDisk     uint64
	currentKernelDisk float64
	totalKernelDisk   uint64
)

func GetCurrentMem() float64 {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return currentMem
}

func GetCurrentCPU() float64 {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return currentCPU
}

func GetCurrentMainDisk() uint64 {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return currentMainDisk
}

func GetTotalMainDisk() uint64 {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return totalMainDisk
}

func GetCurrentKernelDisk() float64 {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return currentKernelDisk
}

func GetTotalKernelDisk() uint64 {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return totalKernelDisk
}

func GetMemData() []PercentData {
	return memData.Range()
}

func GetCPUData() []PercentData {
	return cpuData.Range()
}

func GetNetData() []PercentData {
	return netData.Range()
}

func LoadTop(path string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var network networkSampler
	for {
		<-ticker.C
		collectTop(path, &network)
	}
}

func collectTop(path string, network *networkSampler) {
	now := orm.Now()
	cpuValues, err := cpu.Percent(0, false)
	if err != nil && err.Error() != `not implemented yet` {
		slog.Error("LoadTop cpu", "err", err)
	}
	if len(cpuValues) > 0 {
		cpuData.Push(PercentData{Time: now, Used: cpuValues[0]})
	}
	memory, err := mem.VirtualMemory()
	if err != nil && err.Error() != `not implemented yet` {
		slog.Error("LoadTop VirtualMemory", "err", err)
	}
	if memory != nil {
		memData.Push(PercentData{Time: now, Used: memory.UsedPercent})
	}
	if counters, err := readNetworkCounters(net.IOCounters); err == nil {
		sampledAt := time.Now()
		if up, down, ready := network.sample(counters, sampledAt); ready {
			netData.Push(PercentData{Time: orm.Time{Time: sampledAt}, Up: up, Down: down})
		}
	}
	diskUsage, diskErr := disk.Usage(path)
	currentMu.Lock()
	defer currentMu.Unlock()
	if memory != nil {
		currentMem = memory.UsedPercent
	}
	if len(cpuValues) > 0 {
		currentCPU = cpuValues[0]
	}
	if diskErr == nil {
		currentMainDisk, totalMainDisk = diskUsage.Used, diskUsage.Total
	}
}

// 第三方 netstat 解析在部分 macOS 环境会 panic；指标不可用不能终止整个服务。
func readNetworkCounters(read func(bool) ([]net.IOCountersStat, error)) (counters []net.IOCountersStat, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			counters = nil
			err = fmt.Errorf("read network counters: %v", failure)
		}
	}()
	return read(true)
}

type networkSampler struct {
	previous map[string]net.IOCountersStat
	at       time.Time
}

// 逐网卡检查回退和集合变化；重置基线时不生成伪造的零速率或下溢尖峰。
func (s *networkSampler) sample(counters []net.IOCountersStat, now time.Time) (up, down float64, ready bool) {
	current := make(map[string]net.IOCountersStat, len(counters))
	for _, counter := range counters {
		current[counter.Name] = counter
	}
	defer func() { s.previous, s.at = current, now }()
	elapsed := now.Sub(s.at).Seconds()
	if s.at.IsZero() || elapsed <= 0 || len(current) == 0 || len(current) != len(s.previous) {
		return 0, 0, false
	}
	for name, counter := range current {
		previous, exists := s.previous[name]
		if !exists || counter.BytesSent < previous.BytesSent || counter.BytesRecv < previous.BytesRecv {
			return 0, 0, false
		}
		up += float64(counter.BytesSent-previous.BytesSent) * 8 / elapsed
		down += float64(counter.BytesRecv-previous.BytesRecv) * 8 / elapsed
	}
	return up, down, true
}
