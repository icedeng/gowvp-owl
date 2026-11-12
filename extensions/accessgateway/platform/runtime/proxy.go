package runtime

import (
	"context"
	"crypto/tls"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

func (w *worker) dialGateway(ctx context.Context, raw, path string, tlsConfig *tls.Config, headers http.Header) (*websocket.Conn, error) {
	row := w.configuration()
	if row.Desired.ProxyRef == nil {
		return w.manager.dial(ctx, raw, path, tlsConfig, headers)
	}
	p, err := transport.LoadProxy(transport.ProxyDirectory(w.manager.config.CredentialDir), *row.Desired.ProxyRef, raw)
	if err != nil {
		return nil, err
	}
	return transport.DialVia(ctx, raw, path, tlsConfig, headers, p)
}
