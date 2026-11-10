package stat

import (
	"sync"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/net"
)

func TestNetworkSamplerUsesElapsedTimeAndResetsInvalidBaseline(t *testing.T) {
	var sampler networkSampler
	now := time.Now()
	sample := func(counters []net.IOCountersStat, elapsed time.Duration, wantUp, wantDown float64, wantReady bool) {
		t.Helper()
		now = now.Add(elapsed)
		up, down, ready := sampler.sample(counters, now)
		if up != wantUp || down != wantDown || ready != wantReady {
			t.Fatalf("sample = %g/%g/%t, want %g/%g/%t", up, down, ready, wantUp, wantDown, wantReady)
		}
	}
	counter := func(name string, sent, received uint64) []net.IOCountersStat {
		return []net.IOCountersStat{{Name: name, BytesSent: sent, BytesRecv: received}}
	}
	sample(counter("eth0", 100, 200), 0, 0, 0, false)
	sample(counter("eth0", 300, 600), 2*time.Second, 800, 1600, true)
	sample(counter("eth0", 350, 700), 500*time.Millisecond, 800, 1600, true)
	sample(counter("eth0", 10, 20), time.Second, 0, 0, false)
	sample(counter("eth0", 110, 220), time.Second, 800, 1600, true)
	sample(counter("eth1", 10000, 20000), time.Second, 0, 0, false)
	sample(counter("eth1", 10100, 20200), time.Second, 800, 1600, true)
	sample(nil, time.Second, 0, 0, false)
	sample(counter("eth1", 10200, 20400), time.Second, 0, 0, false)
	sample(counter("eth1", 10300, 20600), 0, 0, 0, false)
	sample([]net.IOCountersStat{{Name: "eth1", BytesSent: 10400, BytesRecv: 20800}, {Name: "eth2", BytesSent: 100, BytesRecv: 200}}, time.Second, 0, 0, false)
	sample([]net.IOCountersStat{{Name: "eth2", BytesSent: 200, BytesRecv: 400}, {Name: "eth1", BytesSent: 10500, BytesRecv: 21000}}, time.Second, 1600, 3200, true)
}

func TestCollectTopConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	var network networkSampler
	var wg sync.WaitGroup
	wg.Go(func() {
		for range 3 {
			collectTop(dir, &network)
		}
	})
	for range 8 {
		wg.Go(func() {
			for range 50 {
				GetCurrentMem()
				GetCurrentCPU()
				GetCurrentMainDisk()
				GetTotalMainDisk()
				GetCurrentKernelDisk()
				GetTotalKernelDisk()
				GetMemData()
				GetCPUData()
				GetNetData()
			}
		})
	}
	wg.Wait()
}
