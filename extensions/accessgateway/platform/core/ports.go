package core

import (
	"context"
	"crypto/tls"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"time"
)

// RuntimeConfig 仅传递运行需要的快照，不泄漏 ORM 对象。
type RuntimeConfig struct {
	GatewayPK       string
	GatewayID       string
	Desired         DesiredConfig
	AdminState      string
	ConfigRevision  int64
	AppliedRevision int64
}
type CredentialRefs struct {
	CA   string
	Cert string
	Key  string
}
type RecoverySession struct {
	ID                string
	GatewayPK         string
	Ref               protocol.ResourceRef
	MediaServerID     *string
	StreamID          *string
	SSRC              *string
	MediaNodeSnapshot string
	RemoteClosed      bool
	LocalClosed       bool
	TransferCommitted bool
}
type RecoveryOutcome struct {
	RemoteClosed      bool
	LocalClosed       bool
	DialogUnavailable bool
	Error             *protocol.Error
}
type RuntimeRepository interface {
	RuntimeLeasesRenewed(context.Context, string, string, int64, []protocol.ResourceRef, time.Time, int) error
	RuntimeConfigs(context.Context) ([]RuntimeConfig, error)
	AcquireOwner(context.Context, string, string) (int64, error)
	RenewOwner(context.Context, string, string, int64) error
	ReleaseOwner(context.Context, string, string, int64) error
	RuntimeCredential(context.Context, string, string) (CredentialRefs, error)
	RecoverySessions(context.Context, string) ([]RecoverySession, error)
	SaveRecovery(context.Context, string, string, int64, RecoverySession, RecoveryOutcome) error
	RuntimeCheckpoint(context.Context, string, string, int64, *time.Time, *protocol.Error) error
}
type CredentialLoader interface {
	Load(string, string, string) (*tls.Config, error)
}
type SignalIngress struct {
	Ingress IngressRoute
	Route   protocol.Route
	Raw     []byte
	Send    func(context.Context, []byte) error
}

// SignalHost 由 integration/owl 实现。注册、鉴权和事务始终留在宿主。
type SignalHost interface {
	ReceiveSignal(context.Context, SignalIngress) error
	ResourceFailed(context.Context, string, protocol.ResourceRef, protocol.Error) error
	FlowClosed(context.Context, string, protocol.ResourceRef) error
	InvalidateIngress(context.Context, string, string, protocol.Decimal) error
	RecoverLocalSession(context.Context, RecoverySession) (bool, error)
	SupportedGatewayCapabilities(context.Context, DesiredConfig) ([]string, error)
}
type RuntimePort interface {
	Snapshot(string) RuntimeSnapshot
	Call(context.Context, string, string, any) (any, error)
	Reconcile(context.Context) error
	OwnerLease(string) (string, int64, bool)
	Reconnect(context.Context, string) error
	Close() error
}
