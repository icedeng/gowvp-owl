package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

func TestInvalidMediaRequestDoesNotDisconnectPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	generation, epoch := uuid.NewString(), protocol.Number(1)
	identity := Identity{GatewayID: "b1", PlatformID: "a", Generation: &generation, ConnectionEpoch: &epoch, OwnerEpoch: epoch}
	serverPeer := make(chan *Peer, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverPeer <- NewPeer(ctx, conn, identity)
	}))
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	client := NewPeer(ctx, conn, identity)
	defer client.Close()
	remote := <-serverPeer
	defer remote.Close()
	channel, ssrc := "channel1", "0123456789"
	request := protocol.MediaOpen{OperationID: uuid.NewString(), Spec: protocol.MediaSpec{
		SubjectType: "device", SubjectID: "device1", ChannelID: &channel, Purpose: "live",
		Direction: "device_to_a", WireMode: "tcp_passive", SSRC: &ssrc, FlowID: uuid.NewString(), ConfigRevision: epoch, RTCP: "disabled",
	}}
	valid := protocol.Payload(request)
	invalid := strings.Replace(string(valid), `"source_ips":[]`, `"source_ips":null`, 1)
	if _, err = client.Call(ctx, "media.open", protocolRaw(invalid)); err == nil {
		t.Fatal("invalid media request was sent")
	}
	if err = client.Event(ctx, "media.ready", protocol.MediaReady{SessionID: uuid.NewString()}); err != nil {
		t.Fatal("invalid request damaged the connection:", err)
	}
	in, err := remote.Next(ctx)
	if err != nil || in.Envelope.Type != "media.ready" {
		t.Fatalf("remote received invalid traffic or disconnected: %s, %v", in.Envelope.Type, err)
	}
	// 播放入口省略 SourceIPs 的真实构造方式必须通过对端的严格解码。
	if err = client.Send(ctx, protocol.Envelope{Version: protocol.Version, Type: "media.open", Kind: "request", RequestID: uuid.NewString(), GatewayID: "b1", PlatformID: "a", Generation: &generation, ConnectionEpoch: &epoch, OwnerEpoch: epoch, Payload: valid}); err != nil {
		t.Fatal(err)
	}
	in, err = remote.Next(ctx)
	if err != nil || in.Envelope.Type != "media.open" || in.Payload.(*protocol.MediaOpen).Spec.SourceIPs == nil {
		t.Fatalf("media request did not reach strict receiver: %s, %v", in.Envelope.Type, err)
	}
}

type protocolRaw string

func (v protocolRaw) MarshalJSON() ([]byte, error) { return []byte(v), nil }
