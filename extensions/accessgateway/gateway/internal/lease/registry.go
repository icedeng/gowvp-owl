package lease

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/state"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

type Owner struct {
	PlatformID  string
	Generation  string
	Epoch       protocol.Decimal
	OwnerEpoch  protocol.Decimal
	Fingerprint string
}
type Resource struct {
	Ref             protocol.ResourceRef
	Owner           Owner
	OperationID     string
	SubjectType     string
	SubjectID       string
	BindingRevision protocol.Decimal
	ConfigRevision  protocol.Decimal
	State           string
	Expires         time.Time
	TTL             time.Duration
	HardDeadline    time.Time
	BytesReceived   int64
	BytesSent       int64
	LocalEOF        bool
	RemoteEOF       bool
	Close           func() error
	Value           any
	ticket          [32]byte
	ticketValid     bool
	ticketExpires   time.Time
	attaching       bool
	attached        bool
	closing         bool
	closedAt        time.Time
}
type Registry struct {
	Counters *transport.Counters
	mu       sync.Mutex
	items    map[string]*Resource
	state    *state.Store
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	failed   func(protocol.ResourceRef, error)
}

func New(parent context.Context, store *state.Store, failed func(protocol.ResourceRef, error)) *Registry {
	ctx, cancel := context.WithCancel(parent)
	r := &Registry{items: map[string]*Resource{}, state: store, ctx: ctx, cancel: cancel, failed: failed}
	r.wg.Add(1)
	go r.sweep()
	return r
}
func (r *Registry) Add(v Resource) (protocol.ResourceRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx.Err() != nil {
		return protocol.ResourceRef{}, r.ctx.Err()
	}
	if len(r.items) >= 100000 {
		return protocol.ResourceRef{}, protocol.Failure(protocol.Exhausted, "resource record budget")
	}
	if v.Ref.ID == "" {
		v.Ref.ID = uuid.NewString()
	}
	v.Ref.Generation = v.Owner.Generation
	v.Ref.ConnectionEpoch = v.Owner.Epoch
	v.State = "allocated"
	v.Expires = time.Now().Add(30 * time.Second)
	if !v.HardDeadline.IsZero() && v.HardDeadline.Before(v.Expires) {
		v.Expires = v.HardDeadline
	}
	persisted := r.state.Snapshot()
	if !persisted.HadResources || int(v.TTL/time.Millisecond) > persisted.MaxLeaseTTLMS {
		if err := r.state.Update(func(d *state.Data) error {
			d.HadResources = true
			if int(v.TTL/time.Millisecond) > d.MaxLeaseTTLMS {
				d.MaxLeaseTTLMS = int(v.TTL / time.Millisecond)
			}
			return nil
		}); err != nil {
			return protocol.ResourceRef{}, err
		}
	}
	if _, ok := r.items[v.Ref.ID]; ok {
		return protocol.ResourceRef{}, fmt.Errorf("duplicate resource ID")
	}
	r.items[v.Ref.ID] = &v
	return v.Ref, nil
}
func (r *Registry) Count(resourceType string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, v := range r.items {
		if v.State != "closed" && (resourceType == "all" || v.Ref.ResourceType == resourceType) {
			n++
		}
	}
	return n
}
func (r *Registry) Renew(owner Owner, ids []string) (renewed, rejected []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	renewed = []string{}
	rejected = []string{}
	now := time.Now()
	for _, id := range ids {
		v := r.items[id]
		if v == nil || v.Owner != owner || v.State == "closing" || v.State == "closed" || now.After(v.Expires) {
			rejected = append(rejected, id)
			continue
		}
		v.Expires = now.Add(v.TTL)
		if !v.HardDeadline.IsZero() && v.HardDeadline.Before(v.Expires) {
			v.Expires = v.HardDeadline
		}
		renewed = append(renewed, id)
	}
	return
}
func (r *Registry) Issue(owner Owner, kind, id string) (string, error) {
	var random [32]byte
	if _, e := rand.Read(random[:]); e != nil {
		return "", e
	}
	token := base64.RawURLEncoding.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.items[id]
	if v == nil || v.Ref.ResourceType != kind || v.Owner != owner || v.State != "allocated" || v.attached || v.attaching || time.Now().After(v.Expires) {
		return "", protocol.Failure(protocol.TicketInvalid, "resource cannot be attached")
	}
	v.ticket = digest
	v.ticketValid = true
	v.ticketExpires = time.Now().Add(30 * time.Second)
	return token, nil
}
func (r *Registry) Consume(owner Owner, kind, id, token string) (any, error) {
	if len(token) != 43 {
		return nil, protocol.Failure(protocol.TicketInvalid, "invalid attachment ticket")
	}
	digest := sha256.Sum256([]byte(token))
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.items[id]
	if v == nil || v.Owner != owner || v.Ref.ResourceType != kind || !v.ticketValid || v.attached || v.attaching || v.State != "allocated" || time.Now().After(v.Expires) || time.Now().After(v.ticketExpires) || subtle.ConstantTimeCompare(digest[:], v.ticket[:]) != 1 {
		return nil, protocol.Failure(protocol.TicketInvalid, "invalid attachment ticket")
	}
	v.ticketValid = false
	v.attaching = true
	return v.Value, nil
}
func (r *Registry) AttachFailed(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.items[id]; v != nil {
		v.attaching = false
	}
}
func (r *Registry) Attached(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.items[id]
	if v == nil || v.State != "allocated" || !v.attaching || time.Now().After(v.Expires) {
		return protocol.Failure(protocol.NotFound, "resource expired before attachment")
	}
	v.attaching = false
	v.attached = true
	v.State = "attached"
	v.Expires = time.Now().Add(v.TTL)
	if !v.HardDeadline.IsZero() && v.HardDeadline.Before(v.Expires) {
		v.Expires = v.HardDeadline
	}
	return nil
}
func (r *Registry) Receiving(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.items[id]; v != nil && v.State == "attached" {
		v.State = "receiving"
	}
}
func (r *Registry) AddBytes(id string, toA bool, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.items[id]; v != nil {
		if toA {
			r.Counters.DeviceReceived(n)
			v.BytesReceived += int64(n)
		} else {
			r.Counters.DeviceSent(n)
			v.BytesSent += int64(n)
		}
	}
}
func (r *Registry) EOF(id string, local bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.items[id]; v != nil {
		if local {
			v.LocalEOF = true
		} else {
			v.RemoteEOF = true
		}
	}
}
func (r *Registry) Get(owner Owner, id string) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.items[id]
	if v == nil || v.Owner != owner || v.State == "closed" || v.State == "closing" || time.Now().After(v.Expires) {
		return nil, protocol.Failure(protocol.NotFound, "resource unavailable")
	}
	return v.Value, nil
}
func (r *Registry) CloseRef(platform string, ref protocol.ResourceRef) (bool, error) {
	r.mu.Lock()
	v := r.items[ref.ID]
	if v == nil {
		r.mu.Unlock()
		return true, nil
	}
	if v.Owner.PlatformID != platform || v.Ref != ref {
		r.mu.Unlock()
		return false, protocol.Failure(protocol.StaleEpoch, "resource reference mismatch")
	}
	if v.State == "closed" {
		r.mu.Unlock()
		return true, nil
	}
	if v.closing {
		r.mu.Unlock()
		return false, nil
	}
	v.State = "closing"
	v.ticketValid = false
	v.closing = true
	closeFn := v.Close
	r.mu.Unlock()
	var err error
	if closeFn != nil {
		err = closeFn()
	}
	r.mu.Lock()
	v.closing = false
	if err == nil {
		v.State = "closed"
		v.closedAt = time.Now()
	}
	r.mu.Unlock()
	return err == nil, err
}
func (r *Registry) List(platform string) []protocol.ResourceRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := []protocol.ResourceRecord{}
	now := time.Now()
	for _, v := range r.items {
		if v.Owner.PlatformID != platform || v.State == "closed" {
			continue
		}
		remaining := max(0, int(v.Expires.Sub(now)/time.Millisecond))
		op := v.OperationID
		items = append(items, protocol.ResourceRecord{Ref: v.Ref, OperationID: &op, SubjectType: v.SubjectType, SubjectID: v.SubjectID, BindingRevision: v.BindingRevision, ConfigRevision: v.ConfigRevision, State: v.State, RemainingTTLMS: remaining, BytesReceived: protocol.Number(v.BytesReceived), BytesSent: protocol.Number(v.BytesSent), LocalEOF: v.LocalEOF, RemoteEOF: v.RemoteEOF})
	}
	return items
}
func (r *Registry) Close() error {
	r.cancel()
	r.wg.Wait()
	r.mu.Lock()
	refs := []struct {
		platform string
		ref      protocol.ResourceRef
	}{}
	for _, v := range r.items {
		if v.State != "closed" {
			refs = append(refs, struct {
				platform string
				ref      protocol.ResourceRef
			}{v.Owner.PlatformID, v.Ref})
		}
	}
	r.mu.Unlock()
	var last error
	for _, v := range refs {
		if _, e := r.CloseRef(v.platform, v.ref); e != nil {
			last = e
		}
	}
	return last
}
func (r *Registry) sweep() {
	defer r.wg.Done()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case now := <-ticker.C:
			r.mu.Lock()
			expired := []struct {
				platform string
				ref      protocol.ResourceRef
			}{}
			for id, v := range r.items {
				if v.State == "closed" {
					if now.Sub(v.closedAt) > 10*time.Minute {
						delete(r.items, id)
					}
					continue
				}
				if now.After(v.Expires) || v.State == "closing" {
					expired = append(expired, struct {
						platform string
						ref      protocol.ResourceRef
					}{v.Owner.PlatformID, v.Ref})
				}
			}
			r.mu.Unlock()
			for _, v := range expired {
				_, e := r.CloseRef(v.platform, v.ref)
				if r.failed != nil {
					r.failed(v.ref, e)
				}
			}
		}
	}
}

func (r *Registry) Active(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.items[id]
	return v != nil && v.State != "closed"
}

// Traffic 只返回本控制所有者本次续租涉及的资源；快照后再读取队列以避免锁反转。
func (r *Registry) Traffic(owner Owner, ids []string) []protocol.ResourceTraffic {
	type entry struct {
		sample protocol.ResourceTraffic
		value  any
	}
	entries := make([]entry, 0, len(ids))
	r.mu.Lock()
	for _, id := range ids {
		if v := r.items[id]; v != nil && v.Owner == owner {
			entries = append(entries, entry{protocol.ResourceTraffic{Ref: v.Ref, RXBytes: protocol.Number(v.BytesReceived), TXBytes: protocol.Number(v.BytesSent), QueueBytes: "0"}, v.Value})
		}
	}
	r.mu.Unlock()
	samples := make([]protocol.ResourceTraffic, 0, len(entries))
	for _, entry := range entries {
		if queue, ok := entry.value.(interface{ QueuedBytes() int64 }); ok {
			entry.sample.QueueBytes = protocol.Number(queue.QueuedBytes())
		}
		samples = append(samples, entry.sample)
	}
	return samples
}
func (r *Registry) QueuedBytes() int64 {
	r.mu.Lock()
	values := make([]any, 0, len(r.items))
	for _, v := range r.items {
		if v.State != "closed" {
			values = append(values, v.Value)
		}
	}
	r.mu.Unlock()
	var total int64
	for _, v := range values {
		if queue, ok := v.(interface{ QueuedBytes() int64 }); ok {
			total += queue.QueuedBytes()
		}
	}
	return total
}
