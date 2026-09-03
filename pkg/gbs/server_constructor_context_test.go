package gbs_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipccache"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs"
	"github.com/ixugo/goddd/domain/uniqueid"
	"gorm.io/gorm"
)

func TestNewServerContextCancellationReleasesListeners(t *testing.T) {
	port := reserveSIPTestPort(t)
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	store := ipccache.NewCache(ipcdb.NewDB(db).AutoMigrate(true))
	adapter := ipc.NewAdapter(store, uniqueid.Core{})
	cfg := conf.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	cfg.Sip.Host = "127.0.0.1"
	cfg.Sip.Port = port
	cfg.Media.SDPIP = "127.0.0.1"

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	server, cleanup, err := gbs.NewServerWithStoresContext(ctx, &cfg, adapter, sms.Core{}, db, nil)
	if server != nil || cleanup != nil {
		t.Fatalf("canceled constructor returned server=%v cleanup=%v", server, cleanup != nil)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NewServerWithStoresContext error = %v, want context.Canceled", err)
	}
	address := fmt.Sprintf("127.0.0.1:%d", port)
	udp, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatalf("UDP listener was not released: %v", err)
	}
	defer udp.Close()
	tcp, err := net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("TCP listener was not released: %v", err)
	}
	defer tcp.Close()
}

func reserveSIPTestPort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 10; attempt++ {
		tcp, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := tcp.Addr().(*net.TCPAddr).Port
		udp, udpErr := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		_ = tcp.Close()
		if udpErr != nil {
			continue
		}
		_ = udp.Close()
		return port
	}
	t.Fatal("failed to reserve a UDP/TCP SIP test port")
	return 0
}
