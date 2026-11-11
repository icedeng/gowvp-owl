package runtime

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

// CallExact 防止旧业务在重连后把命令送到新代次。
func (m *Manager) CallExact(ctx context.Context, pk string, ref protocol.ResourceRef, name string, payload any) (any, error) {
	m.mu.RLock()
	w := m.workers[pk]
	m.mu.RUnlock()
	if w == nil {
		return nil, protocol.Failure(protocol.StaleEpoch, "网关入口已失效")
	}
	w.mu.RLock()
	p := w.peer
	valid := p != nil && w.snapshot.Generation == ref.Generation && w.snapshot.Epoch == ref.ConnectionEpoch
	w.mu.RUnlock()
	if !valid {
		return nil, protocol.Failure(protocol.StaleEpoch, "网关入口已经换代")
	}
	r, err := p.Call(ctx, name, payload)
	return r.Payload, err
}

// Attach 先登记事件等待器，再消费一次票据；ready 可能早于 HTTP upgrade 返回。
func (m *Manager) Attach(ctx context.Context, pk string, ref protocol.ResourceRef, mode, direction string) (*transport.Binary, error) {
	m.mu.RLock()
	w := m.workers[pk]
	m.mu.RUnlock()
	if w == nil {
		return nil, core.Fail(503, "GATEWAY_UNAVAILABLE", "网关未连接")
	}
	w.mu.Lock()
	if w.ready == nil {
		w.ready = map[string]chan struct{}{}
	}
	if _, exists := w.ready[ref.ID]; exists {
		w.mu.Unlock()
		return nil, protocol.Failure(protocol.BindingConflict, "会话已经附着")
	}
	ready := make(chan struct{})
	w.ready[ref.ID] = ready
	row := w.config
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.ready, ref.ID); w.mu.Unlock() }()
	r, err := m.CallExact(ctx, pk, ref, "attachment.issue", protocol.AttachmentIssue{OperationID: uuid.NewString(), ResourceType: ref.ResourceType, ResourceID: ref.ID})
	if err != nil {
		return nil, err
	}
	if row.Desired.CredentialID == nil {
		return nil, core.Fail(503, "CREDENTIAL_REQUIRED", "网关凭据缺失")
	}
	credential, err := m.repo.RuntimeCredential(ctx, pk, *row.Desired.CredentialID)
	if err != nil {
		return nil, err
	}
	tlsConfig, err := m.credentials.Load(credential.CA, credential.Cert, credential.Key)
	if err != nil {
		return nil, err
	}
	path := protocol.MediaPath
	if ref.ResourceType == "transfer" {
		path = protocol.TransferPath
	}
	headers := http.Header{"X-Owl-Resource-Id": {ref.ID}, "X-Owl-Attach-Ticket": {r.(*protocol.AttachmentResult).Ticket}}
	conn, err := m.dial(ctx, row.Desired.URL, path, tlsConfig, headers)
	if err != nil {
		return nil, err
	}
	b := transport.NewBinary(w.ctx, conn, mode, direction, ref.ResourceType == "transfer", true)
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-ready:
		return b, nil
	case <-ctx.Done():
		err = ctx.Err()
	case <-w.ctx.Done():
		err = w.ctx.Err()
	case <-b.Done():
		err = b.Err()
	case <-timer.C:
		err = protocol.Failure(protocol.UpstreamTimeout, "未收到媒体/任务附着确认")
	}
	_ = b.Close()
	return nil, err
}

func (w *worker) attached(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if ch := w.ready[id]; ch != nil {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
}
