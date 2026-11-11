//go:build linux

package controller

import (
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"os"
	"strconv"
	"strings"
)

func currentRSS() *protocol.Decimal {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return nil
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return nil
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || pages < 0 || pages > int64(^uint64(0)>>1)/int64(os.Getpagesize()) {
		return nil
	}
	n := protocol.Number(pages * int64(os.Getpagesize()))
	return &n
}
