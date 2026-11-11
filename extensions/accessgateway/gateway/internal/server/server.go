package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/config"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/controller"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

type Server struct {
	config     config.Config
	controller *controller.Controller
	upgrader   websocket.Upgrader
}

func New(c config.Config, control *controller.Controller) http.Handler {
	s := &Server{config: c, controller: control, upgrader: websocket.Upgrader{Subprotocols: []string{protocol.Subprotocol}, EnableCompression: false, ReadBufferSize: 4096, WriteBufferSize: 4096, CheckOrigin: func(r *http.Request) bool { return r.Header.Get("Origin") == "" }}}
	mux := http.NewServeMux()
	mux.HandleFunc(protocol.ProbePath, s.probe)
	mux.HandleFunc(protocol.ControlPath, s.control)
	mux.HandleFunc(protocol.MediaPath, s.attachment)
	mux.HandleFunc(protocol.TransferPath, s.attachment)
	return mux
}
func fail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(protocol.Failure(code, message))
}
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodGet || r.URL.RawQuery != "" || r.URL.ForceQuery || r.ContentLength > 0 {
		fail(w, 400, protocol.InvalidArgument, "GET without query/body required")
		return "", false
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		fail(w, 401, protocol.IdentityMismatch, "verified client certificate required")
		return "", false
	}
	leaf := r.TLS.PeerCertificates[0]
	if leaf.Subject.CommonName != s.config.Platform.ClientCertificateCN {
		fail(w, 403, protocol.Forbidden, "certificate is not authorized for this platform")
		return "", false
	}
	sum := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(sum[:]), true
}
func (s *Server) websocket(w http.ResponseWriter, r *http.Request) bool {
	if !websocket.IsWebSocketUpgrade(r) || !slices.Contains(websocket.Subprotocols(r), protocol.Subprotocol) || r.Header.Get("Origin") != "" {
		fail(w, 400, protocol.ProtocolUnsupported, "owl-gateway.v1 WebSocket required")
		return false
	}
	return true
}
func (s *Server) probe(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(s.controller.Probe())
}
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	fingerprint, ok := s.authenticate(w, r)
	if !ok || !s.websocket(w, r) {
		return
	}
	release, e := s.controller.Reserve()
	if e != nil {
		fail(w, 409, protocol.OwnerConflict, "control owner already connected")
		return
	}
	defer release()
	conn, e := s.upgrader.Upgrade(w, r, nil)
	if e != nil {
		return
	}
	s.controller.ServeControl(conn, fingerprint)
}
func (s *Server) attachment(w http.ResponseWriter, r *http.Request) {
	fingerprint, ok := s.authenticate(w, r)
	if !ok || !s.websocket(w, r) {
		return
	}
	owner, e := s.controller.CurrentOwner(fingerprint)
	if e != nil {
		fail(w, 403, protocol.Forbidden, "matching control owner required")
		return
	}
	resourceHeaders, ticketHeaders := r.Header.Values("X-Owl-Resource-Id"), r.Header.Values("X-Owl-Attach-Ticket")
	if len(resourceHeaders) != 1 || len(ticketHeaders) != 1 || !protocol.ValidID(resourceHeaders[0]) || strings.TrimSpace(ticketHeaders[0]) != ticketHeaders[0] {
		fail(w, 400, protocol.InvalidArgument, "unique resource and ticket headers required")
		return
	}
	kind := "media"
	if r.URL.Path == protocol.TransferPath {
		kind = "transfer"
	}
	value, e := s.controller.Registry.Consume(owner, kind, resourceHeaders[0], ticketHeaders[0])
	if e != nil {
		fail(w, 403, protocol.TicketInvalid, "invalid attachment ticket")
		return
	}

	conn, e := s.upgrader.Upgrade(w, r, nil)
	if e != nil {
		s.controller.Registry.AttachFailed(resourceHeaders[0])
		return
	}
	current, ownerErr := s.controller.CurrentOwner(fingerprint)
	if ownerErr != nil || current != owner {
		conn.Close()
		s.controller.Registry.AttachFailed(resourceHeaders[0])
		return
	}
	if kind == "media" {
		e = s.controller.Media.Attach(owner, resourceHeaders[0], value, conn)
	} else {
		e = s.controller.Transfers.Attach(owner, resourceHeaders[0], value, conn)
	}
	if e != nil {
		conn.Close()
		s.controller.Registry.AttachFailed(resourceHeaders[0])
	}
}
