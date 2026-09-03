package gormstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/annexg"
	"gorm.io/gorm/clause"
)

type pendingExchangeRow struct {
	ID       uint64 `gorm:"primaryKey"`
	SystemID string `gorm:"column:system_id;size:20;not null;uniqueIndex:idx_annex_g_pending_identity,priority:1"`
	Command  string `gorm:"column:command;size:64;not null;uniqueIndex:idx_annex_g_pending_identity,priority:2"`
	SN       int    `gorm:"column:sn;not null;uniqueIndex:idx_annex_g_pending_identity,priority:3"`
	Version  string `gorm:"column:version;size:8;not null"`
	// ProfileFingerprint 将在途请求绑定到完整外部系统档案；空值仅表示旧版本遗留数据。
	ProfileFingerprint string    `gorm:"column:profile_fingerprint;size:64"`
	Request            string    `gorm:"column:request;type:text;not null"`
	Response           string    `gorm:"column:response;type:text"`
	CreatedAt          time.Time `gorm:"column:created_at;not null;index"`
	ExpiresAt          time.Time `gorm:"column:expires_at;not null;index"`
}

func (*pendingExchangeRow) TableName() string { return "gb_annex_g_pending_exchanges" }

// PendingExchange 是进程重启后可恢复的管理平台主动交换。
type PendingExchange struct {
	SystemID           string
	Version            annexg.Version
	ProfileFingerprint string
	Request            annexg.Message
	Response           annexg.Message
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// SavePendingResponse 在返回 SIP 成功确认前保存已校验业务响应，支持崩溃后重放副作用。
func (store *Store) SavePendingResponse(ctx context.Context, systemID string, version annexg.Version, response annexg.Message) error {
	if ctx == nil {
		return errors.New("nil Annex G store context")
	}
	db, err := store.database()
	if err != nil {
		return err
	}
	systemID = strings.TrimSpace(systemID)
	sn, ok := annexg.MessageSequence(response)
	if systemID == "" || response == nil || response.RootName() != "Response" || !ok || sn <= 0 || response.CommandType() == "" {
		return errors.New("invalid Annex G pending response")
	}
	if err := response.Validate(version); err != nil {
		return err
	}
	payload, err := annexg.Encode(version, response)
	if err != nil {
		return err
	}
	store.exchangeMu.Lock()
	defer store.exchangeMu.Unlock()
	statement := db.WithContext(ctx)
	var row pendingExchangeRow
	result := statement.Where("system_id = ? AND command = ? AND sn = ?", systemID, string(response.CommandType()), sn).Limit(1).Find(&row)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("Annex G pending exchange %s/%d for system %s was not found", response.CommandType(), sn, systemID)
	}
	if row.Version != string(version) {
		return errors.New("Annex G pending response version does not match request")
	}
	if row.Response != "" {
		if row.Response == string(payload) {
			return nil
		}
		return errors.New("Annex G pending exchange already has a different response")
	}
	updated := statement.Model(new(pendingExchangeRow)).Where("id = ? AND (response = '' OR response IS NULL)", row.ID).Update("response", string(payload))
	if updated.Error != nil || updated.RowsAffected > 0 {
		return updated.Error
	}
	if err := statement.Select("response").Where("id = ?", row.ID).First(&row).Error; err != nil {
		return err
	}
	if row.Response != string(payload) {
		return errors.New("Annex G pending exchange already has a different response")
	}
	return nil
}

// SavePendingExchange 在发送 SIP 请求前持久化业务关联；相同载荷可幂等重试。
func (store *Store) SavePendingExchange(ctx context.Context, systemID string, version annexg.Version, request annexg.Message, profileFingerprint ...string) error {
	if ctx == nil {
		return errors.New("nil Annex G store context")
	}
	db, err := store.database()
	if err != nil {
		return err
	}
	systemID = strings.TrimSpace(systemID)
	if len(profileFingerprint) > 1 {
		return errors.New("at most one Annex G profile fingerprint is allowed")
	}
	profile := ""
	if len(profileFingerprint) == 1 {
		profile = strings.TrimSpace(profileFingerprint[0])
		if profile != "" && len(profile) != 64 {
			return errors.New("invalid Annex G profile fingerprint")
		}
	}
	sn, ok := annexg.MessageSequence(request)
	if systemID == "" || request == nil || !ok || sn <= 0 || request.CommandType() == "" {
		return errors.New("invalid Annex G pending exchange")
	}
	if err := request.Validate(version); err != nil {
		return err
	}
	payload, err := annexg.Encode(version, request)
	if err != nil {
		return err
	}
	now := time.Now()
	row := pendingExchangeRow{
		SystemID: systemID, Command: string(request.CommandType()), SN: sn,
		Version: string(version), ProfileFingerprint: profile, Request: string(payload), CreatedAt: now, ExpiresAt: now.Add(store.pendingTTL),
	}

	store.exchangeMu.Lock()
	defer store.exchangeMu.Unlock()
	statement := db.WithContext(ctx)
	if err := statement.Where("expires_at <= ?", now).Delete(new(pendingExchangeRow)).Error; err != nil {
		return fmt.Errorf("prune expired Annex G exchanges: %w", err)
	}
	var existing pendingExchangeRow
	existingResult := statement.Where("system_id = ? AND command = ? AND sn = ?", row.SystemID, row.Command, row.SN).Limit(1).Find(&existing)
	if existingResult.Error != nil {
		return existingResult.Error
	}
	if existingResult.RowsAffected > 0 {
		if existing.Version == row.Version && existing.Request == row.Request {
			// 旧调用方可能没有提供指纹；一旦新调用方提供，则补齐绑定信息。
			if row.ProfileFingerprint != "" && existing.ProfileFingerprint == "" {
				if err := statement.Model(new(pendingExchangeRow)).Where("id = ? AND (profile_fingerprint = '' OR profile_fingerprint IS NULL)", existing.ID).Update("profile_fingerprint", row.ProfileFingerprint).Error; err != nil {
					return err
				}
			}
			if row.ProfileFingerprint != "" && existing.ProfileFingerprint != "" && existing.ProfileFingerprint != row.ProfileFingerprint {
				return fmt.Errorf("Annex G pending exchange %s/%d for system %s belongs to a different system profile", row.Command, row.SN, row.SystemID)
			}
			return nil
		}
		return fmt.Errorf("Annex G pending exchange %s/%d for system %s already exists with different content", row.Command, row.SN, row.SystemID)
	}
	var count int64
	if err := statement.Model(new(pendingExchangeRow)).Count(&count).Error; err != nil {
		return err
	}
	if count >= int64(store.maxPending) {
		return fmt.Errorf("Annex G pending exchange limit %d reached", store.maxPending)
	}
	created := statement.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	if created.Error != nil {
		return created.Error
	}
	if created.RowsAffected > 0 {
		return nil
	}
	if err := statement.Where("system_id = ? AND command = ? AND sn = ?", row.SystemID, row.Command, row.SN).First(&existing).Error; err != nil {
		return err
	}
	if existing.Version == row.Version && existing.Request == row.Request {
		if row.ProfileFingerprint != "" && existing.ProfileFingerprint == "" {
			if err := statement.Model(new(pendingExchangeRow)).Where("id = ? AND (profile_fingerprint = '' OR profile_fingerprint IS NULL)", existing.ID).Update("profile_fingerprint", row.ProfileFingerprint).Error; err != nil {
				return err
			}
		}
		if row.ProfileFingerprint != "" && existing.ProfileFingerprint != "" && existing.ProfileFingerprint != row.ProfileFingerprint {
			return fmt.Errorf("Annex G pending exchange %s/%d for system %s belongs to a different system profile", row.Command, row.SN, row.SystemID)
		}
		return nil
	}
	return fmt.Errorf("Annex G pending exchange %s/%d for system %s already exists with different content", row.Command, row.SN, row.SystemID)
}

// LoadPendingExchanges 返回所有未过期关联，并删除已过期记录。
func (store *Store) LoadPendingExchanges(ctx context.Context) ([]PendingExchange, error) {
	if ctx == nil {
		return nil, errors.New("nil Annex G store context")
	}
	db, err := store.database()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	store.exchangeMu.Lock()
	defer store.exchangeMu.Unlock()
	statement := db.WithContext(ctx)
	if err := statement.Where("expires_at <= ?", now).Delete(new(pendingExchangeRow)).Error; err != nil {
		return nil, err
	}
	var count int64
	if err := statement.Model(new(pendingExchangeRow)).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > int64(store.maxPending) {
		return nil, fmt.Errorf("stored Annex G pending exchanges %d exceed limit %d", count, store.maxPending)
	}
	var rows []pendingExchangeRow
	if err := statement.Order("created_at ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]PendingExchange, 0, len(rows))
	for _, row := range rows {
		version, ok := annexg.ParseVersion(row.Version)
		if !ok || version == annexg.Version2022 {
			return nil, fmt.Errorf("stored Annex G exchange %d has unsupported version %q", row.ID, row.Version)
		}
		message, err := annexg.Decode(version, []byte(row.Request))
		if err != nil {
			return nil, fmt.Errorf("decode stored Annex G exchange %d: %w", row.ID, err)
		}
		if message.RootName() == "Response" {
			return nil, fmt.Errorf("stored Annex G exchange %d is not a request", row.ID)
		}
		sn, ok := annexg.MessageSequence(message)
		if !ok || sn != row.SN || string(message.CommandType()) != row.Command {
			return nil, fmt.Errorf("stored Annex G exchange %d correlation mismatch", row.ID)
		}
		var response annexg.Message
		if row.Response != "" {
			response, err = annexg.Decode(version, []byte(row.Response))
			if err != nil {
				return nil, fmt.Errorf("decode stored Annex G response %d: %w", row.ID, err)
			}
			responseSN, responseOK := annexg.MessageSequence(response)
			if response.RootName() != "Response" || !responseOK || responseSN != row.SN || response.CommandType() != message.CommandType() {
				return nil, fmt.Errorf("stored Annex G response %d correlation mismatch", row.ID)
			}
		}
		result = append(result, PendingExchange{
			SystemID: row.SystemID, Version: version, ProfileFingerprint: row.ProfileFingerprint,
			Request: message, Response: response,
			CreatedAt: row.CreatedAt, ExpiresAt: row.ExpiresAt,
		})
	}
	return result, nil
}

// DeletePendingExchange 在请求确认未发送或业务响应已幂等落地后删除关联。
func (store *Store) DeletePendingExchange(ctx context.Context, systemID string, command annexg.Command, sn int) error {
	if ctx == nil {
		return errors.New("nil Annex G store context")
	}
	db, err := store.database()
	if err != nil {
		return err
	}
	if strings.TrimSpace(systemID) == "" || command == "" || sn <= 0 {
		return errors.New("invalid Annex G pending exchange key")
	}
	store.exchangeMu.Lock()
	defer store.exchangeMu.Unlock()
	return db.WithContext(ctx).Where("system_id = ? AND command = ? AND sn = ?", strings.TrimSpace(systemID), string(command), sn).
		Delete(new(pendingExchangeRow)).Error
}

// DeleteExpiredPendingExchanges 删除到期关联并返回删除数量。
func (store *Store) DeleteExpiredPendingExchanges(ctx context.Context, now time.Time) (int64, error) {
	if ctx == nil {
		return 0, errors.New("nil Annex G store context")
	}
	db, err := store.database()
	if err != nil {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	store.exchangeMu.Lock()
	defer store.exchangeMu.Unlock()
	result := db.WithContext(ctx).Where("expires_at <= ?", now).Delete(new(pendingExchangeRow))
	return result.RowsAffected, result.Error
}
