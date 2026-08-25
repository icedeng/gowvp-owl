package sms

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ixugo/goddd/pkg/orm"
)

var (
	_ Storer            = (*TestStorer)(nil)
	_ MediaServerStorer = (*TestMediaServerStorer)(nil)
)

type (
	TestStorer            struct{}
	TestMediaServerStorer struct{}
)

// Create implements [MediaServerStorer].
func (t *TestMediaServerStorer) Create(context.Context, *MediaServer) error {
	panic("unimplemented")
}

// Delete implements [MediaServerStorer].
func (t *TestMediaServerStorer) Delete(context.Context, *MediaServer, ...orm.QueryOption) error {
	panic("unimplemented")
}

// List implements [MediaServerStorer].
func (t *TestMediaServerStorer) List(context.Context, *[]*MediaServer, orm.Pager, ...orm.QueryOption) (int64, error) {
	panic("unimplemented")
}

// Update implements [MediaServerStorer].
func (t *TestMediaServerStorer) Update(context.Context, *MediaServer, func(*MediaServer), ...orm.QueryOption) error {
	panic("unimplemented")
}

// Add implements MediaServerStorer.
func (t *TestMediaServerStorer) Add(context.Context, *MediaServer) error {
	panic("unimplemented")
}

// Del implements MediaServerStorer.
func (t *TestMediaServerStorer) Del(context.Context, *MediaServer, ...orm.QueryOption) error {
	panic("unimplemented")
}

// Edit implements MediaServerStorer.
func (t *TestMediaServerStorer) Edit(ctx context.Context, in *MediaServer, fn func(*MediaServer), args ...orm.QueryOption) error {
	fn(in)
	fmt.Println("edit status:", in.Status)
	return nil
}

// Find implements MediaServerStorer.
func (t *TestMediaServerStorer) Find(context.Context, *[]*MediaServer, orm.Pager, ...orm.QueryOption) (int64, error) {
	panic("unimplemented")
}

// Get implements MediaServerStorer.
func (t *TestMediaServerStorer) Get(context.Context, *MediaServer, ...orm.QueryOption) error {
	panic("unimplemented")
}

// MediaServer implements Storer.
func (t *TestStorer) MediaServer() MediaServerStorer {
	return &TestMediaServerStorer{}
}

func TestKeepalvie(t *testing.T) {
	var storer TestStorer
	nm := NewNodeManager(&storer)
	defer nm.Close()
	nm.cacheServers.Store("local", &WarpMediaServer{
		LastUpdatedAt: time.Now(),
	})
	time.Sleep(time.Second)
	nm.Keepalive("local")
	time.Sleep(25 * time.Second)
	nm.Keepalive("local")
	time.Sleep(5 * time.Second)
	// edit status: true
	// edit status: false
	// edit status: true
}

type nodeManagerTestStore struct {
	updateErr error
}

func (s *nodeManagerTestStore) MediaServer() MediaServerStorer {
	return nodeManagerTestMediaStore{updateErr: s.updateErr}
}

type nodeManagerTestMediaStore struct {
	updateErr error
}

func (s nodeManagerTestMediaStore) List(context.Context, *[]*MediaServer, orm.Pager, ...orm.QueryOption) (int64, error) {
	return 0, nil
}

func (s nodeManagerTestMediaStore) Get(context.Context, *MediaServer, ...orm.QueryOption) error {
	return nil
}

func (s nodeManagerTestMediaStore) Create(context.Context, *MediaServer) error {
	return nil
}

func (s nodeManagerTestMediaStore) Update(_ context.Context, model *MediaServer, changeFn func(*MediaServer), _ ...orm.QueryOption) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	changeFn(model)
	return nil
}

func (s nodeManagerTestMediaStore) Delete(context.Context, *MediaServer, ...orm.QueryOption) error {
	return nil
}

type nodeManagerTestDriver struct {
	Driver
	connectErr error
	setupErr   error
	pingErr    error
	connects   int
	setups     int
	pings      int
}

func (d *nodeManagerTestDriver) Protocol() string { return "test" }

func (d *nodeManagerTestDriver) Connect(context.Context, *MediaServer) error {
	d.connects++
	return d.connectErr
}

func (d *nodeManagerTestDriver) Setup(context.Context, *MediaServer, string) error {
	d.setups++
	return d.setupErr
}

func (d *nodeManagerTestDriver) Ping(context.Context, *MediaServer) error {
	d.pings++
	return d.pingErr
}

func TestNodeManagerConnectionPublishesOnlineOnlyAfterFullSetup(t *testing.T) {
	nm := NewNodeManager(&nodeManagerTestStore{})
	defer nm.Close()
	driver := &nodeManagerTestDriver{}
	nm.RegisterDriver("test", driver)
	server := &MediaServer{ID: "test", Type: "test", HookIP: "127.0.0.1"}

	if err := nm.connection(server, 8080); err != nil {
		t.Fatal(err)
	}
	if !nm.IsOnline(server.ID) || driver.connects != 1 || driver.setups != 1 {
		t.Fatalf("connection state: online=%v connects=%d setups=%d", nm.IsOnline(server.ID), driver.connects, driver.setups)
	}
	nm.Close()
}

func TestNodeManagerConnectionFailureStaysOffline(t *testing.T) {
	tests := []struct {
		name       string
		connectErr error
		setupErr   error
		updateErr  error
	}{
		{name: "connect", connectErr: errors.New("connect failed")},
		{name: "setup", setupErr: errors.New("setup failed")},
		{name: "persist", updateErr: errors.New("db unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nm := NewNodeManager(&nodeManagerTestStore{updateErr: test.updateErr})
			defer nm.Close()
			driver := &nodeManagerTestDriver{connectErr: test.connectErr, setupErr: test.setupErr}
			nm.RegisterDriver("test", driver)
			server := &MediaServer{ID: "test", Type: "test", HookIP: "127.0.0.1"}

			if err := nm.connection(server, 8080); err == nil {
				t.Fatal("connection error = nil")
			}
			if nm.IsOnline(server.ID) {
				t.Fatal("failed connection reported online")
			}
		})
	}
}

func TestNodeManagerReconnectReappliesSetup(t *testing.T) {
	nm := NewNodeManager(&nodeManagerTestStore{})
	defer nm.Close()
	nm.serverPort.Store(8080)
	driver := &nodeManagerTestDriver{}
	nm.RegisterDriver("test", driver)
	server := &MediaServer{ID: "test", Type: "test", HookIP: "127.0.0.1"}
	state := &WarpMediaServer{Config: server}
	nm.cacheServers.Store(server.ID, state)

	nm.checkMediaServer(state)
	if !nm.IsOnline(server.ID) || driver.pings != 1 || driver.connects != 1 || driver.setups != 1 {
		t.Fatalf("reconnect state: online=%v pings=%d connects=%d setups=%d", nm.IsOnline(server.ID), driver.pings, driver.connects, driver.setups)
	}
}

var _ Driver = (*nodeManagerTestDriver)(nil)
