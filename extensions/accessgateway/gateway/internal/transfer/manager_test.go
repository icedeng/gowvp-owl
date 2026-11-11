package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/lease"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/media"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

type transferFixture struct {
	m    *Manager
	s    *Session
	a    *transport.Binary
	url  string
	sent chan protocol.TransferSent
	ctx  context.Context
}

func newFixture(t *testing.T, kind string, limit int64, data []byte) transferFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	store, e := state.Open(dir, "b1", "a")
	if e != nil {
		t.Fatal(e)
	}
	registry := lease.New(ctx, store, nil)
	sent := make(chan protocol.TransferSent, 1)
	m := &Manager{ctx: ctx, registry: registry, budget: media.NewBudget(100000000, 100000000), store: store, maxRecords: 100, tasks: map[string]*Session{}, event: func(name string, payload any) {
		if name == "transfer.sent" {
			sent <- payload.(protocol.TransferSent)
		}
	}}
	owner := lease.Owner{PlatformID: "a", Generation: uuid.NewString(), Epoch: "1", OwnerEpoch: "1", Fingerprint: "cert"}
	spec := protocol.TransferSpec{TaskID: uuid.NewString(), SubjectID: "device1", Kind: kind, Direction: "device_to_a", MaxBytes: protocol.Number(limit), TTLMS: 60000, ContentType: "image/jpeg"}
	if kind == "upgrade_download" {
		spec.Direction = "a_to_device"
		spec.ContentType = "application/octet-stream"
		length := protocol.Number(int64(len(data)))
		offset := protocol.Decimal("0")
		sum := sha256.Sum256(data)
		hash := hex.EncodeToString(sum[:])
		spec.ContentLength = &length
		spec.RangeOffset = &offset
		spec.RangeLength = &length
		spec.SHA256 = &hash
	}
	child, stop := context.WithCancel(ctx)
	s := &Session{manager: m, owner: owner, ctx: child, cancel: stop, spec: spec, ready: make(chan struct{}), commitDone: make(chan struct{}), digest: sha256.New(), phase: "allocated", expires: time.Now().Add(time.Minute)}
	token := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	s.publicHash = sha256.Sum256([]byte(token))
	ref, e := registry.Add(lease.Resource{Ref: protocol.ResourceRef{ResourceType: "transfer"}, Owner: owner, OperationID: uuid.NewString(), SubjectType: "device", SubjectID: "device1", BindingRevision: "1", ConfigRevision: "1", TTL: time.Minute, HardDeadline: s.expires, Value: s, Close: s.close})
	if e != nil {
		t.Fatal(e)
	}
	s.ref = ref
	m.tasks["a/"+spec.TaskID] = s
	ticket, e := registry.Issue(owner, "transfer", ref.ID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = registry.Consume(owner, "transfer", ref.ID, ticket); e != nil {
		t.Fatal(e)
	}
	attached := make(chan error, 1)
	wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e == nil {
			e = m.Attach(owner, ref.ID, s, conn)
		}
		attached <- e
	}))
	conn, _, e := websocket.DefaultDialer.DialContext(ctx, strings.Replace(wsServer.URL, "http://", "ws://", 1), nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = <-attached; e != nil {
		t.Fatal(e)
	}
	a := transport.NewBinary(ctx, conn, "", spec.Direction, true, true)
	taskServer := httptest.NewServer(m.TaskHandler())
	t.Cleanup(func() {
		registry.Close()
		a.Close()
		cancel()
		taskServer.Close()
		wsServer.Close()
		store.Close()
	})
	return transferFixture{m: m, s: s, a: a, url: taskServer.URL + "/tasks/" + spec.TaskID + "/" + token, sent: sent, ctx: ctx}
}
func TestUploadWaitsForDurableCommitAndAllowsExactLimit(t *testing.T) {
	for _, multipartBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "raw", true: "multipart"}[multipartBody], func(t *testing.T) {
			data := []byte("jpeg")
			f := newFixture(t, "snapshot_upload", int64(len(data)), nil)
			result := make(chan *http.Response, 1)
			failure := make(chan error, 1)
			go func() {
				body := bytes.NewBuffer(append([]byte{}, data...))
				contentType := "image/jpeg"
				if multipartBody {
					body.Reset()
					writer := multipart.NewWriter(body)
					part, err := writer.CreateFormFile("file", "snapshot.jpg")
					if err != nil {
						failure <- err
						return
					}
					if _, err = part.Write(data); err != nil {
						failure <- err
						return
					}
					if err = writer.Close(); err != nil {
						failure <- err
						return
					}
					contentType = writer.FormDataContentType()
				}
				request, _ := http.NewRequestWithContext(f.ctx, "POST", f.url, body)
				request.Header.Set("Content-Type", contentType)
				response, e := http.DefaultClient.Do(request)
				if e != nil {
					failure <- e
				} else {
					result <- response
				}
			}()
			received := []byte{}
			for {
				message, e := f.a.Read(f.ctx)
				if e != nil {
					t.Fatal(e)
				}
				if message.Kind == protocol.EOF {
					break
				}
				received = append(received, message.Payload...)
			}
			if !bytes.Equal(received, data) {
				t.Fatal("upload content changed")
			}
			select {
			case response := <-result:
				response.Body.Close()
				t.Fatal("device received success before A committed storage")
			case e := <-failure:
				t.Fatal(e)
			default:
			}
			sum := sha256.Sum256(data)
			hash := hex.EncodeToString(sum[:])
			out, e := f.m.Finish(f.s.owner, protocol.TransferFinish{OperationID: uuid.NewString(), TransferID: f.s.ref.ID, ReceivedBytes: protocol.Number(int64(len(data))), SHA256: hash})
			if e != nil || !out.Committed {
				t.Fatal(e)
			}
			select {
			case response := <-result:
				defer response.Body.Close()
				if response.StatusCode != 200 {
					body, _ := io.ReadAll(response.Body)
					t.Fatal("upload rejected", response.StatusCode, string(body))
				}
			case e := <-failure:
				t.Fatal(e)
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			}
			query, e := f.m.Query("a", f.s.ref.ID)
			if e != nil || !query.Committed {
				t.Fatal("commit not queryable", e)
			}
			duplicate, e := http.Post(f.url, "image/jpeg", strings.NewReader("duplicate must not be stored"))
			if e != nil {
				t.Fatal(e)
			}
			duplicate.Body.Close()
			if duplicate.StatusCode != 200 {
				t.Fatal("completed upload retry not idempotent")
			}
			if f.s.bytes != int64(len(data)) {
				t.Fatal("duplicate upload changed committed bytes")
			}
			records := f.m.store.Snapshot().Transfers
			if len(records) != 1 || !records[0].Committed || records[0].SHA256 == nil || *records[0].SHA256 != hash {
				t.Fatal("missing durable receipt")
			}
		})
	}
}
func TestUploadOverLimitAndPrematureFinishRejected(t *testing.T) {
	f := newFixture(t, "snapshot_upload", 4, nil)
	if _, e := f.m.Finish(f.s.owner, protocol.TransferFinish{OperationID: uuid.NewString(), TransferID: f.s.ref.ID, ReceivedBytes: "0", SHA256: strings.Repeat("0", 64)}); e == nil {
		t.Fatal("accepted finish before EOF")
	}
	response, e := http.Post(f.url, "image/jpeg", strings.NewReader("12345"))
	if e != nil {
		t.Fatal(e)
	}
	response.Body.Close()
	if response.StatusCode != 413 {
		t.Fatalf("over limit HTTP %d", response.StatusCode)
	}
}
func TestDownloadSentAndCommitAreSeparate(t *testing.T) {
	data := []byte("verified upgrade package")
	f := newFixture(t, "upgrade_download", int64(len(data)), data)
	received := make(chan []byte, 1)
	failure := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(f.ctx, "GET", f.url, nil)
		response, e := http.DefaultClient.Do(request)
		if e != nil {
			failure <- e
			return
		}
		defer response.Body.Close()
		body, e := io.ReadAll(response.Body)
		if e != nil {
			failure <- e
			return
		}
		received <- body
	}()
	if e := f.a.Send(f.ctx, protocol.FileBytes, data); e != nil {
		t.Fatal(e)
	}
	if e := f.a.Send(f.ctx, protocol.EOF, nil); e != nil {
		t.Fatal(e)
	}
	select {
	case event := <-f.sent:
		if event.Bytes != protocol.Number(int64(len(data))) {
			t.Fatal("wrong sent byte count")
		}
	case e := <-failure:
		t.Fatal(e)
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	query, e := f.m.Query("a", f.s.ref.ID)
	if e != nil || query.Committed {
		t.Fatal("sent was treated as committed", e)
	}
	sum := sha256.Sum256(data)
	_, e = f.m.Finish(f.s.owner, protocol.TransferFinish{OperationID: uuid.NewString(), TransferID: f.s.ref.ID, ReceivedBytes: protocol.Number(int64(len(data))), SHA256: hex.EncodeToString(sum[:])})
	if e != nil {
		t.Fatal(e)
	}
	select {
	case body := <-received:
		if !bytes.Equal(body, data) {
			t.Fatal("download content changed")
		}
	case e := <-failure:
		t.Fatal(e)
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
}
