//go:build !linux && !darwin

package controller

import (
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"time"
)

type processSampler struct{}

func (*processSampler) sample(time.Time) (*float64, *protocol.Decimal) { return nil, nil }
