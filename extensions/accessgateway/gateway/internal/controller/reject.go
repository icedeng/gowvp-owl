package controller

import (
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

func (c *Controller) rejectHello(conn *websocket.Conn, request protocol.Envelope, code, message string) {
	reply := protocol.Envelope{Version: protocol.Version, Type: "hello.ack", Kind: "response", RequestID: uuid.NewString(), ReplyTo: request.RequestID, GatewayID: request.GatewayID, PlatformID: request.PlatformID, OwnerEpoch: request.OwnerEpoch, Payload: protocol.Payload(struct{}{}), Error: protocol.Failure(code, message)}
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_ = conn.WriteJSON(reply)
}
