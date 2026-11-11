package media

import (
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

type allocation struct {
	port     int
	rtcpPort int
	udp      *net.UDPConn
	rtcp     *net.UDPConn
	listener net.Listener
}
type Ports struct {
	mu       sync.Mutex
	used     map[int]bool
	cooling  map[int]time.Time
	store    *state.Store
	bindIP   string
	cooldown time.Time
}

func NewPorts(bindIP string, store *state.Store) *Ports {
	data := store.Snapshot()
	p := &Ports{used: map[int]bool{}, cooling: map[int]time.Time{}, store: store, bindIP: bindIP}
	for key, until := range data.Ports {
		port, e := strconv.Atoi(key)
		if e == nil {
			p.cooling[port] = until
		}
	}
	if data.HadResources {
		p.cooldown = time.Now().Add(time.Duration(max(data.MaxLeaseTTLMS, 60000))*time.Millisecond + 30*time.Second)
	}
	return p
}
func (p *Ports) CooldownMS() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(0, int(time.Until(p.cooldown)/time.Millisecond))
}
func (p *Ports) Used() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.used) }
func (p *Ports) Allocate(c protocol.EffectiveConfig, mode, rtcp string) (*allocation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Now().Before(p.cooldown) {
		return nil, protocol.Failure(protocol.Exhausted, "gateway ports are in restart cooldown")
	}
	for port := c.MediaPortStart; port <= c.MediaPortEnd; port++ {
		if p.used[port] || time.Now().Before(p.cooling[port]) {
			continue
		}
		a := &allocation{port: port}
		var e error
		if mode == "udp" {
			a.udp, e = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(p.bindIP), Port: port})
		} else {
			a.listener, e = net.Listen("tcp", net.JoinHostPort(p.bindIP, strconv.Itoa(port)))
		}
		if e != nil {
			continue
		}
		if mode == "udp" && rtcp == "udp_separate" {
			for second := c.MediaPortStart; second <= c.MediaPortEnd; second++ {
				if second == port || p.used[second] || time.Now().Before(p.cooling[second]) {
					continue
				}
				a.rtcp, e = net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(p.bindIP), Port: second})
				if e == nil {
					a.rtcpPort = second
					break
				}
			}
			if a.rtcp == nil {
				a.closeSockets()
				continue
			}
		}
		// 先写入冷却记录，预留关闭时的日志容量，再发布已分配端口。
		if err := p.store.Update(func(d *state.Data) error {
			until := time.Now().Add(time.Duration(c.LeaseTTLMS)*time.Millisecond + 30*time.Second)
			d.Ports[strconv.Itoa(port)] = until
			if a.rtcpPort != 0 {
				d.Ports[strconv.Itoa(a.rtcpPort)] = until
			}
			return nil
		}); err != nil {
			a.closeSockets()
			return nil, err
		}
		p.used[port] = true
		if a.rtcpPort != 0 {
			p.used[a.rtcpPort] = true
		}
		return a, nil
	}
	return nil, protocol.Failure(protocol.PortBindFailed, "no available media ports")
}
func (a *allocation) closeSockets() {
	if a.udp != nil {
		a.udp.Close()
	}
	if a.rtcp != nil {
		a.rtcp.Close()
	}
	if a.listener != nil {
		a.listener.Close()
	}
}
func (p *Ports) Release(a *allocation, ttl time.Duration) error {
	a.closeSockets()
	p.mu.Lock()
	defer p.mu.Unlock()
	until := time.Now().Add(ttl + 30*time.Second)
	ports := []int{a.port}
	if a.rtcpPort != 0 {
		ports = append(ports, a.rtcpPort)
	}
	if e := p.store.Update(func(d *state.Data) error {
		for key, t := range d.Ports {
			port, _ := strconv.Atoi(key)
			if time.Now().After(t) && !p.used[port] {
				delete(d.Ports, key)
			}
		}
		for _, port := range ports {
			d.Ports[strconv.Itoa(port)] = until
		}
		return nil
	}); e != nil {
		return fmt.Errorf("persist port quarantine: %w", e)
	}
	for _, port := range ports {
		delete(p.used, port)
		p.cooling[port] = until
	}
	return nil
}

// ReserveTCP 复用唯一端口池，任务端口与媒体端口不能相互占用。
func (p *Ports) ReserveTCP(c protocol.EffectiveConfig) (net.Listener, int, func() error, error) {
	a, e := p.Allocate(c, "tcp_passive", "disabled")
	if e != nil {
		return nil, 0, nil, e
	}
	return a.listener, a.port, func() error { return p.Release(a, time.Duration(c.LeaseTTLMS)*time.Millisecond) }, nil
}
