package gbs

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestCascadeCatalogRequestValidationStopsWithServiceLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		method string
		body   []byte
		invoke func(*GB28181API, *sip.Context)
	}{
		{
			name: "MESSAGE query", method: sip.MethodMessage,
			body: []byte(`<Query><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>110101</DeviceID></Query>`),
			invoke: func(api *GB28181API, ctx *sip.Context) {
				api.sipCascadeMessageMiddleware(ctx)
			},
		},
		{
			name: "SUBSCRIBE", method: sip.MethodSubscribe,
			body: []byte(`<Query><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>110101</DeviceID></Query>`),
			invoke: func(api *GB28181API, ctx *sip.Context) {
				api.sipSubscribeEvent(ctx)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entered := make(chan struct{})
			channelStore := &blockingCascadeCatalogChannelStore{entered: entered}
			store := &cascadeContextStore{channel: channelStore}
			lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
			api := &GB28181API{
				core:            ipc.NewAdapter(store, uniqueid.Core{}),
				lifecycleDone:   make(chan struct{}),
				lifecycleCtx:    lifecycleCtx,
				lifecycleCancel: lifecycleCancel,
			}
			platform := testSharedCascadePlatform(t)
			worker := newCascadeWorker(nil, platform)
			connection := newFlowConnection()
			request := newFlowRequest(t, connection, test.method, "cascade-catalog-validation-context", test.body)
			ctx := &sip.Context{
				Request: request, Tx: sip.NewTransaction("cascade-catalog-validation-context", connection),
				DeviceID: platform.serverID, Source: connection.remote,
			}
			ctx.Set(cascadeWorkerContextKey, worker)
			done := make(chan struct{})
			go func() {
				test.invoke(api, ctx)
				close(done)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("cascade Catalog validation did not reach the channel store")
			}
			api.beginClose()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cascade Catalog validation did not stop with the service lifecycle")
			}
		})
	}
}

func TestCascadeCatalogQueryStopsWithWorkerReplacement(t *testing.T) {
	entered := make(chan struct{})
	channelStore := &blockingCascadeCatalogChannelStore{entered: entered}
	store := &cascadeContextStore{channel: channelStore}
	server := &Server{}
	api := &GB28181API{core: ipc.NewAdapter(store, uniqueid.Core{}), svr: server}
	server.gb = api
	manager := NewCascadeManager(server)
	server.cascade = manager
	platform := testSharedCascadePlatform(t)
	worker := newCascadeWorker(server, platform)
	close(worker.done)
	manager.items[worker.platform.name] = worker
	connection := newFlowConnection()
	body := []byte(`<Query><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>` + platform.localID + `</DeviceID></Query>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "cascade-catalog-worker-context", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("cascade-catalog-worker-context", connection),
		DeviceID: platform.serverID, Source: connection.remote,
	}
	ctx.Set(cascadeWorkerContextKey, worker)
	api.sipCascadeMessageMiddleware(ctx)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("cascade Catalog query did not reach the channel store")
	}

	local := conf.SIP{ID: gb10DeviceID, Domain: "local.example", Host: "192.0.2.20", Port: 5060}
	applyDone := make(chan error, 1)
	go func() { applyDone <- manager.Apply(local, nil) }()
	select {
	case err := <-applyDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("CascadeManager.Apply waited for the downstream query timeout")
	}
	tasksDone := make(chan struct{})
	go func() {
		api.lifecycleWG.Wait()
		close(tasksDone)
	}()
	select {
	case <-tasksDone:
	case <-time.After(time.Second):
		t.Fatal("old worker Catalog query survived replacement")
	}
}

type blockingCascadeCatalogChannelStore struct {
	ipc.ChannelStorer
	entered chan struct{}
	once    sync.Once
}

func (s *blockingCascadeCatalogChannelStore) List(ctx context.Context, _ *[]*ipc.Channel, _ orm.Pager, _ ...orm.QueryOption) (int64, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	return 0, ctx.Err()
}
