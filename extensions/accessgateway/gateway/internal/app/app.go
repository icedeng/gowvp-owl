// Package app 只装配 B 的组件，不导入 Owl 宿主、数据库或 ZLM。
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/config"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/controller"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/server"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

func Run(ctx context.Context, c config.Config) error {
	if e := c.Validate(); e != nil {
		return e
	}
	tlsConfig, e := transport.ServerTLS(c.TLSCertFile, c.TLSKeyFile, c.ClientCAFile)
	if e != nil {
		return e
	}
	store, e := state.Open(c.StateDir, c.GatewayID, c.Platform.ID)
	if e != nil {
		return e
	}
	defer store.Close()
	control, e := controller.New(ctx, c, store)
	if e != nil {
		return e
	}
	defer control.Close()
	if e = control.Start(); e != nil {
		return fmt.Errorf("start SIP listeners: %w", e)
	}
	listener, e := net.Listen("tcp", c.Listen)
	if e != nil {
		return e
	}
	srv := &http.Server{Handler: server.New(c, control), TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	result := make(chan error, 2)
	var taskServer *http.Server
	if c.TaskListen != "" {
		taskListener, err := net.Listen("tcp", c.TaskListen)
		if err != nil {
			listener.Close()
			return err
		}
		taskTLS := tlsConfig.Clone()
		taskTLS.ClientAuth = tls.NoClientCert
		taskTLS.ClientCAs = nil
		taskServer = &http.Server{Handler: control.Transfers.TaskHandler(), TLSConfig: taskTLS, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10}
		defer taskServer.Close()
		go func() { result <- taskServer.Serve(tls.NewListener(taskListener, taskTLS)) }()
	}
	go func() { result <- srv.Serve(tls.NewListener(listener, tlsConfig)) }()
	defer srv.Close()
	slog.Info("接入网关已启动", "gateway_id", c.GatewayID, "listen", c.Listen, "sip_listen", c.SIPListen)
	select {
	case <-ctx.Done():
	case e = <-result:
		if !errors.Is(e, http.ErrServerClosed) {
			return e
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if taskServer != nil {
		if err := taskServer.Shutdown(shutdownCtx); err != nil {
			taskServer.Close()
			return err
		}
	}
	if e = srv.Shutdown(shutdownCtx); e != nil {
		srv.Close()
		return e
	}
	return nil
}
