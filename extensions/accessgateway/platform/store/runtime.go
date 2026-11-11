package store

import (
	"context"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
	"time"
)

func (r *Repository) RuntimeConfigs(ctx context.Context) ([]core.RuntimeConfig, error) {
	rows, e := r.Enabled(ctx)
	if e != nil {
		return nil, e
	}
	result := make([]core.RuntimeConfig, 0, len(rows))
	for _, row := range rows {
		result = append(result, core.RuntimeConfig{GatewayPK: row.ID, GatewayID: row.StableID, Desired: row.DesiredConfig, AdminState: row.AdminState, ConfigRevision: row.ConfigRevision, AppliedRevision: row.AppliedRevision})
	}
	return result, nil
}
func (r *Repository) RuntimeCredential(ctx context.Context, gatewayPK, id string) (core.CredentialRefs, error) {
	v, e := r.Credential(ctx, gatewayPK, id)
	if e != nil {
		return core.CredentialRefs{}, e
	}
	if v.State == "retired" {
		return core.CredentialRefs{}, core.Fail(409, "CREDENTIAL_RETIRED", "凭据已退役")
	}
	return core.CredentialRefs{CA: v.CAFileRef, Cert: v.CertFileRef, Key: v.KeyFileRef}, nil
}
func (r *Repository) RecoverySessions(ctx context.Context, gatewayPK string) ([]core.RecoverySession, error) {
	rows, e := r.ActiveSessions(ctx, gatewayPK)
	if e != nil {
		return nil, e
	}
	result := make([]core.RecoverySession, 0, len(rows))
	for _, row := range rows {
		result = append(result, core.RecoverySession{ID: row.ID, GatewayPK: row.GatewayID, Ref: protocol.ResourceRef{ResourceType: row.ResourceType, ID: row.RemoteID, Generation: row.Generation, ConnectionEpoch: protocol.Number(row.ConnectionEpoch)}, MediaServerID: row.MediaServerID, StreamID: row.StreamID, SSRC: row.SSRC, MediaNodeSnapshot: row.MediaNodeSnapshot, RemoteClosed: row.CleanupSteps.RemoteClosed, LocalClosed: row.CleanupSteps.BridgeClosed && row.CleanupSteps.MediaClosed, TransferCommitted: row.CleanupSteps.TransferCommitted})
	}
	return result, nil
}
func (r *Repository) SaveRecovery(ctx context.Context, gatewayPK, ownerID string, ownerEpoch int64, session core.RecoverySession, outcome core.RecoveryOutcome) error {
	return r.Transaction(ctx, func(tx *gorm.DB) error {
		if e := CheckOwner(tx, gatewayPK, ownerID, ownerEpoch); e != nil {
			return e
		}
		var row Session
		if e := tx.Where("id = ? AND gateway_id = ? AND remote_id = ? AND generation = ? AND connection_epoch = ?", session.ID, gatewayPK, session.Ref.ID, session.Ref.Generation, session.Ref.ConnectionEpoch).First(&row).Error; e != nil {
			return e
		}
		row.CleanupSteps.BusinessInvalidated = true
		row.CleanupSteps.RemoteClosed = outcome.RemoteClosed
		row.CleanupSteps.BridgeClosed = outcome.LocalClosed
		row.CleanupSteps.MediaClosed = outcome.LocalClosed
		row.CleanupSteps.DialogUnavailable = outcome.DialogUnavailable
		row.Error = outcome.Error
		row.State = "closing"
		if outcome.RemoteClosed && outcome.LocalClosed {
			now := time.Now().UTC()
			row.ClosedAt = &now
			row.State = "closed"
			row.CleanupSteps.Committed = true
			until := now.Add(24 * time.Hour)
			if e := tx.Model(&SSRCReservation{}).Where("session_id = ?", row.ID).Updates(map[string]any{"state": "quarantined", "quarantine_until": until}).Error; e != nil {
				return e
			}
		}
		return tx.Omit("Traffic").Save(&row).Error
	})
}
func (r *Repository) RuntimeCheckpoint(ctx context.Context, gatewayPK, ownerID string, ownerEpoch int64, seen *time.Time, failure *protocol.Error) error {
	return r.GatewayRuntimeUpdate(ctx, gatewayPK, ownerID, ownerEpoch, map[string]any{"last_seen_at": seen, "last_error": failure})
}

func (r *Repository) RuntimeLeasesRenewed(ctx context.Context, gatewayPK, ownerID string, epoch int64, refs []protocol.ResourceRef, sent time.Time, ttlMS int) error {
	return r.Transaction(ctx, func(tx *gorm.DB) error {
		if e := CheckOwner(tx, gatewayPK, ownerID, epoch); e != nil {
			return e
		}
		for _, ref := range refs {
			if e := tx.Model(&Session{}).Where("gateway_id = ? AND resource_type = ? AND remote_id = ? AND generation = ? AND connection_epoch = ? AND state IN ?", gatewayPK, ref.ResourceType, ref.ID, ref.Generation, ref.ConnectionEpoch, []string{"allocated", "attached", "receiving"}).Updates(map[string]any{"last_renewed_at": sent, "expires_at": sent.Add(time.Duration(ttlMS) * time.Millisecond)}).Error; e != nil {
				return e
			}
		}
		return nil
	})
}
