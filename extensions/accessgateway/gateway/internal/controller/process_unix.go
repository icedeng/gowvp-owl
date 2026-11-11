//go:build linux || darwin

package controller

import (
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"syscall"
	"time"
)

type processSampler struct {
	at  time.Time
	cpu float64
}

func (s *processSampler) sample(now time.Time) (*float64, *protocol.Decimal) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return nil, nil
	}
	cpu := float64(usage.Utime.Sec+usage.Stime.Sec) + float64(usage.Utime.Usec+usage.Stime.Usec)/1e6
	var percent *float64
	if !s.at.IsZero() && now.After(s.at) && cpu >= s.cpu {
		value := (cpu - s.cpu) / now.Sub(s.at).Seconds() * 100
		percent = &value
	}
	s.at, s.cpu = now, cpu
	return percent, currentRSS()
}
