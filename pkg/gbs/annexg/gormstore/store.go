// Package gormstore 提供附录 G 报警记录和布控状态的可选 GORM 持久化实现。
package gormstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/pkg/gbs/annexg"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultMaxSendRecords = 100
	maximumMaxSendRecords = 10000
	defaultPendingTTL     = 24 * time.Hour
	defaultMaxPending     = 4096
	maximumMaxPending     = 10000
)

// Options 控制查询记录数和主动交换在途关联的持久化上限。
type Options struct {
	MaxSendRecords int
	PendingTTL     time.Duration
	MaxPending     int
}

// Store 实现附录 G 的三类报警写入/查询和布控状态存储接口。
type Store struct {
	db             *gorm.DB
	maxSendRecords int
	pendingTTL     time.Duration
	maxPending     int
	exchangeMu     sync.Mutex
}

// New 创建可选附录 G GORM 存储。调用方必须先执行 Migrate。
func New(db *gorm.DB, options Options) (*Store, error) {
	if db == nil {
		return nil, errors.New("Annex G GORM database is unavailable")
	}
	if options.MaxSendRecords < 0 || options.MaxSendRecords > maximumMaxSendRecords {
		return nil, fmt.Errorf("Annex G max send records must be 0..%d", maximumMaxSendRecords)
	}
	if options.PendingTTL < 0 || options.PendingTTL > 7*24*time.Hour {
		return nil, fmt.Errorf("Annex G pending TTL must be 0..168h")
	}
	if options.MaxPending < 0 || options.MaxPending > maximumMaxPending {
		return nil, fmt.Errorf("Annex G max pending exchanges must be 0..%d", maximumMaxPending)
	}
	limit := options.MaxSendRecords
	if limit <= 0 {
		limit = defaultMaxSendRecords
	}
	pendingTTL := options.PendingTTL
	if pendingTTL <= 0 {
		pendingTTL = defaultPendingTTL
	}
	maxPending := options.MaxPending
	if maxPending <= 0 {
		maxPending = defaultMaxPending
	}
	return &Store{db: db, maxSendRecords: limit, pendingTTL: pendingTTL, maxPending: maxPending}, nil
}

// Migrate 创建或更新附录 G 独立数据表。
func (store *Store) Migrate(ctx context.Context) error {
	if store == nil || store.db == nil {
		return errors.New("Annex G GORM store is unavailable")
	}
	if ctx == nil {
		return errors.New("nil Annex G store context")
	}
	return store.db.WithContext(ctx).AutoMigrate(
		new(alarmRecordRow),
		new(defenceStateRow),
		new(defenceAuditRow),
		new(pendingExchangeRow),
	)
}

type alarmRecordRow struct {
	ID            uint64    `gorm:"primaryKey"`
	Kind          string    `gorm:"column:kind;size:16;not null;index:idx_annex_g_alarm_query,priority:1"`
	Fingerprint   string    `gorm:"column:fingerprint;size:64;not null;uniqueIndex:idx_annex_g_alarm_fingerprint"`
	AlarmNO       string    `gorm:"column:alarm_no;size:255;index"`
	AlarmTime     time.Time `gorm:"column:alarm_time;not null;index:idx_annex_g_alarm_query,priority:2"`
	DeviceID      string    `gorm:"column:device_id;size:255;index"`
	AlarmClass    string    `gorm:"column:alarm_class;size:255;index"`
	AlarmPriority string    `gorm:"column:alarm_priority;size:255;index"`
	AlarmMethod   string    `gorm:"column:alarm_method;size:255;index"`
	AlarmAddress  string    `gorm:"column:alarm_address;size:1024"`
	TollgateID    string    `gorm:"column:tollgate_id;size:255;index"`
	CarPlate      string    `gorm:"column:car_plate;size:255;index"`
	PlateType     string    `gorm:"column:plate_type;size:255;index"`
	Payload       string    `gorm:"column:payload;type:text;not null"`
	CreatedAt     time.Time `gorm:"column:created_at;not null;index"`
}

func (*alarmRecordRow) TableName() string { return "gb_annex_g_alarm_records" }

type defenceStateRow struct {
	ID          uint64    `gorm:"primaryKey"`
	TollgateID  string    `gorm:"column:tollgate_id;size:255;not null;uniqueIndex:idx_annex_g_defence_identity,priority:1"`
	CarPlate    string    `gorm:"column:car_plate;size:255;not null;uniqueIndex:idx_annex_g_defence_identity,priority:2"`
	PlateType   string    `gorm:"column:plate_type;size:255;not null;uniqueIndex:idx_annex_g_defence_identity,priority:3"`
	DefenceType string    `gorm:"column:defence_type;size:255;not null;uniqueIndex:idx_annex_g_defence_identity,priority:4"`
	Active      bool      `gorm:"column:active;not null;index"`
	DefenceTime time.Time `gorm:"column:defence_time;not null"`
	UpdatedAt   time.Time `gorm:"column:updated_at;not null;index"`
}

func (*defenceStateRow) TableName() string { return "gb_annex_g_defence_states" }

type defenceAuditRow struct {
	ID          uint64    `gorm:"primaryKey"`
	Fingerprint string    `gorm:"column:fingerprint;size:64;not null;uniqueIndex:idx_annex_g_defence_audit_fingerprint"`
	TollgateID  string    `gorm:"column:tollgate_id;size:255;not null;index"`
	CarPlate    string    `gorm:"column:car_plate;size:255;not null;index"`
	PlateType   string    `gorm:"column:plate_type;size:255;not null"`
	DefenceType string    `gorm:"column:defence_type;size:255;not null"`
	Active      bool      `gorm:"column:active;not null"`
	DefenceTime time.Time `gorm:"column:defence_time;not null"`
	CreatedAt   time.Time `gorm:"column:created_at;not null;index"`
}

func (*defenceAuditRow) TableName() string { return "gb_annex_g_defence_audits" }

func (store *Store) database() (*gorm.DB, error) {
	if store == nil || store.db == nil {
		return nil, errors.New("Annex G GORM store is unavailable")
	}
	return store.db, nil
}

func fingerprint(kind string, value any) (string, string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(append([]byte(kind+"\x00"), payload...))
	return hex.EncodeToString(digest[:]), string(payload), nil
}

func (store *Store) saveAlarmRecord(ctx context.Context, row *alarmRecordRow) error {
	if ctx == nil {
		return errors.New("nil Annex G store context")
	}
	db, err := store.database()
	if err != nil {
		return err
	}
	if row == nil || row.Kind == "" || row.Fingerprint == "" || row.Payload == "" || row.AlarmTime.IsZero() {
		return errors.New("invalid Annex G alarm record")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now()
	}
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "fingerprint"}},
		DoNothing: true,
	}).Create(row).Error
}

// SaveMPAlarmRecord 幂等保存一条管理平台报警记录。
func (store *Store) SaveMPAlarmRecord(ctx context.Context, record annexg.MPAlarmRecord) error {
	if err := (&annexg.MPAlarmNotify{CmdType: annexg.CommandMPAlarm, SN: 1, AlarmContent: record}).Validate(annexg.Version2011); err != nil {
		return err
	}
	alarmTime, _ := annexg.ParseDateTime(record.AlarmTime)
	fp, payload, err := fingerprint("mp", record)
	if err != nil {
		return err
	}
	return store.saveAlarmRecord(ctx, &alarmRecordRow{
		Kind: "mp", Fingerprint: fp, AlarmNO: strings.TrimSpace(record.AlarmNO), AlarmTime: alarmTime,
		DeviceID: strings.TrimSpace(record.DeviceID), AlarmClass: pointerValue(record.AlarmClass),
		AlarmPriority: strings.TrimSpace(record.AlarmPriority), AlarmMethod: strings.TrimSpace(record.AlarmMethod),
		AlarmAddress: pointerValue(record.AlarmAddress), Payload: payload,
	})
}

// SaveECSAlarmRecord 幂等保存一条综合接处警报警记录。
func (store *Store) SaveECSAlarmRecord(ctx context.Context, record annexg.ECSAlarmRecord) error {
	if err := (&annexg.ECSAlarmNotify{CmdType: annexg.CommandECSAlarm, SN: 1, AlarmContent: record}).Validate(annexg.Version2011); err != nil {
		return err
	}
	alarmTime, _ := annexg.ParseDateTime(record.AlarmTime)
	fp, payload, err := fingerprint("ecs", record)
	if err != nil {
		return err
	}
	return store.saveAlarmRecord(ctx, &alarmRecordRow{
		Kind: "ecs", Fingerprint: fp, AlarmNO: strings.TrimSpace(record.AlarmNO), AlarmTime: alarmTime,
		AlarmClass: strings.TrimSpace(record.AlarmClass), AlarmPriority: strings.TrimSpace(record.AlarmPriority),
		AlarmMethod: strings.TrimSpace(record.AlarmMethod), AlarmAddress: strings.TrimSpace(record.AlarmAddress), Payload: payload,
	})
}

// SaveTGSAlarmRecord 幂等保存一条卡口报警记录。
func (store *Store) SaveTGSAlarmRecord(ctx context.Context, record annexg.TGSAlarmRecord) error {
	if err := (&annexg.TGSAlarmNotify{CmdType: annexg.CommandTGSAlarm, SN: 1, AlarmContent: record}).Validate(annexg.Version2011); err != nil {
		return err
	}
	alarmTime, _ := annexg.ParseDateTime(record.AlarmTime)
	fp, payload, err := fingerprint("tgs", record)
	if err != nil {
		return err
	}
	return store.saveAlarmRecord(ctx, &alarmRecordRow{
		Kind: "tgs", Fingerprint: fp, AlarmTime: alarmTime, TollgateID: strings.TrimSpace(record.TollgateID),
		CarPlate: strings.TrimSpace(record.CarPlate), PlateType: strings.TrimSpace(record.PlateType), Payload: payload,
	})
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

type queryResult struct {
	total int64
	rows  []alarmRecordRow
}

func (store *Store) query(ctx context.Context, kind string, query annexg.AlarmRecordQuery) (queryResult, error) {
	if ctx == nil {
		return queryResult{}, errors.New("nil Annex G store context")
	}
	db, err := store.database()
	if err != nil {
		return queryResult{}, err
	}
	if err := query.Validate(annexg.Version2011); err != nil {
		return queryResult{}, err
	}
	statement := db.WithContext(ctx).Model(new(alarmRecordRow)).Where("kind = ?", kind)
	if query.BeginTime != nil {
		begin, err := annexg.ParseDateTime(*query.BeginTime)
		if err != nil {
			return queryResult{}, err
		}
		statement = statement.Where("alarm_time >= ?", begin)
	}
	if query.EndTime != nil {
		end, err := annexg.ParseDateTime(*query.EndTime)
		if err != nil {
			return queryResult{}, err
		}
		statement = statement.Where("alarm_time <= ?", end)
	}
	statement = applyQueryFilters(statement, query)
	var total int64
	if err := statement.Count(&total).Error; err != nil {
		return queryResult{}, err
	}
	rows := make([]alarmRecordRow, 0, min(int64(store.maxSendRecords), total))
	if total > 0 {
		if err := statement.Order("alarm_time ASC, id ASC").Limit(store.maxSendRecords).Find(&rows).Error; err != nil {
			return queryResult{}, err
		}
	}
	return queryResult{total: total, rows: rows}, nil
}

func applyQueryFilters(statement *gorm.DB, query annexg.AlarmRecordQuery) *gorm.DB {
	if query.AlarmClass != nil {
		statement = statement.Where("alarm_class = ?", strings.TrimSpace(*query.AlarmClass))
	}
	if query.AlarmPriority != nil {
		statement = statement.Where("alarm_priority = ?", strings.TrimSpace(*query.AlarmPriority))
	}
	if query.AlarmMethod != nil {
		statement = statement.Where("alarm_method = ?", strings.TrimSpace(*query.AlarmMethod))
	}
	if query.AlarmDeviceRange != nil {
		statement = statement.Where("device_id LIKE ? ESCAPE '\\'", likePrefix(*query.AlarmDeviceRange))
	}
	if query.AlarmAddressRange != nil {
		statement = statement.Where("alarm_address LIKE ? ESCAPE '\\'", likePrefix(*query.AlarmAddressRange))
	}
	if query.TollgateID != nil {
		statement = statement.Where("tollgate_id = ?", strings.TrimSpace(*query.TollgateID))
	}
	if query.CarPlate != nil {
		statement = statement.Where("car_plate = ?", strings.TrimSpace(*query.CarPlate))
	}
	if query.PlateType != nil {
		statement = statement.Where("plate_type = ?", strings.TrimSpace(*query.PlateType))
	}
	return statement
}

func likePrefix(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	value = strings.ReplaceAll(value, `_`, `\_`)
	return value + "%"
}

// QueryMPAlarmRecords 查询管理平台报警记录。
func (store *Store) QueryMPAlarmRecords(ctx context.Context, query annexg.AlarmRecordQuery) (annexg.MPAlarmRecordListResponse, error) {
	result, err := store.query(ctx, "mp", query)
	if err != nil {
		return annexg.MPAlarmRecordListResponse{}, err
	}
	records := make([]annexg.MPAlarmRecord, 0, len(result.rows))
	for _, row := range result.rows {
		var record annexg.MPAlarmRecord
		if err := json.Unmarshal([]byte(row.Payload), &record); err != nil {
			return annexg.MPAlarmRecordListResponse{}, fmt.Errorf("decode MPAlarm record %d: %w", row.ID, err)
		}
		records = append(records, record)
	}
	return annexg.MPAlarmRecordListResponse{
		CmdType: annexg.CommandMPAlarmRecordList, SN: query.SN, Result: annexg.ResultOK,
		RealRecordNum: boundedInt(result.total), SendRecordNum: len(records),
		RecordList: annexg.MPAlarmRecordList{AlarmRecords: records},
	}, nil
}

// QueryECSAlarmRecords 查询综合接处警报警记录。
func (store *Store) QueryECSAlarmRecords(ctx context.Context, query annexg.AlarmRecordQuery) (annexg.ECSAlarmRecordListResponse, error) {
	result, err := store.query(ctx, "ecs", query)
	if err != nil {
		return annexg.ECSAlarmRecordListResponse{}, err
	}
	records := make([]annexg.ECSAlarmRecord, 0, len(result.rows))
	for _, row := range result.rows {
		var record annexg.ECSAlarmRecord
		if err := json.Unmarshal([]byte(row.Payload), &record); err != nil {
			return annexg.ECSAlarmRecordListResponse{}, fmt.Errorf("decode ECSAlarm record %d: %w", row.ID, err)
		}
		records = append(records, record)
	}
	return annexg.ECSAlarmRecordListResponse{
		CmdType: annexg.CommandECSAlarmRecordList, SN: query.SN, Result: annexg.ResultOK,
		RealRecordNum: boundedInt(result.total), SendRecordNum: len(records),
		RecordList: annexg.ECSAlarmRecordList{AlarmRecords: records},
	}, nil
}

// QueryTGSAlarmRecords 查询卡口报警记录。
func (store *Store) QueryTGSAlarmRecords(ctx context.Context, query annexg.AlarmRecordQuery) (annexg.TGSAlarmRecordListResponse, error) {
	result, err := store.query(ctx, "tgs", query)
	if err != nil {
		return annexg.TGSAlarmRecordListResponse{}, err
	}
	records := make([]annexg.TGSAlarmRecord, 0, len(result.rows))
	for _, row := range result.rows {
		var record annexg.TGSAlarmRecord
		if err := json.Unmarshal([]byte(row.Payload), &record); err != nil {
			return annexg.TGSAlarmRecordListResponse{}, fmt.Errorf("decode TGSAlarm record %d: %w", row.ID, err)
		}
		records = append(records, record)
	}
	return annexg.TGSAlarmRecordListResponse{
		CmdType: annexg.CommandTGSAlarmRecordList, SN: query.SN, Result: annexg.ResultOK,
		RealRecordNum: boundedInt(result.total), SendRecordNum: len(records),
		RecordList: annexg.TGSAlarmRecordList{AlarmRecords: records},
	}, nil
}

func boundedInt(value int64) int {
	maximum := int64(^uint(0) >> 1)
	if value > maximum {
		return int(maximum)
	}
	return int(value)
}

// ApplyConfigDefence 幂等更新布控当前状态，并追加不可变审计记录。
func (store *Store) ApplyConfigDefence(ctx context.Context, notify annexg.ConfigDefenceNotify) (annexg.NotificationResponse, error) {
	response := annexg.NotificationResponse{CmdType: annexg.CommandConfigDefence, SN: notify.SN, Result: annexg.ResultError}
	if ctx == nil {
		return response, errors.New("nil Annex G store context")
	}
	if err := notify.Validate(annexg.Version2011); err != nil {
		return response, err
	}
	db, err := store.database()
	if err != nil {
		return response, err
	}
	defenceTime, _ := annexg.ParseDateTime(notify.DefenceTime)
	fp, _, err := fingerprint("defence", struct {
		Active      bool
		TollgateID  string
		CarPlate    string
		PlateType   string
		DefenceType string
		DefenceTime string
	}{
		Active: *notify.Type, TollgateID: strings.TrimSpace(notify.TollgateID),
		CarPlate: strings.TrimSpace(notify.CarPlate), PlateType: strings.TrimSpace(notify.PlateType),
		DefenceType: strings.TrimSpace(notify.DefenceType), DefenceTime: strings.TrimSpace(notify.DefenceTime),
	})
	if err != nil {
		return response, err
	}
	now := time.Now()
	active := notify.Type != nil && *notify.Type
	state := defenceStateRow{
		TollgateID: strings.TrimSpace(notify.TollgateID), CarPlate: strings.TrimSpace(notify.CarPlate),
		PlateType: strings.TrimSpace(notify.PlateType), DefenceType: strings.TrimSpace(notify.DefenceType),
		Active: active, DefenceTime: defenceTime, UpdatedAt: now,
	}
	audit := defenceAuditRow{
		Fingerprint: fp, TollgateID: state.TollgateID, CarPlate: state.CarPlate, PlateType: state.PlateType,
		DefenceType: state.DefenceType, Active: active, DefenceTime: defenceTime, CreatedAt: now,
	}
	err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "fingerprint"}},
			DoNothing: true,
		}).Create(&audit)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "tollgate_id"}, {Name: "car_plate"}, {Name: "plate_type"}, {Name: "defence_type"}},
			DoUpdates: clause.AssignmentColumns([]string{"active", "defence_time", "updated_at"}),
		}).Create(&state).Error
	})
	if err != nil {
		return response, err
	}
	response.Result = annexg.ResultOK
	return response, nil
}

var (
	_ annexg.MPAlarmSink     = (*Store)(nil)
	_ annexg.MPAlarmQuerier  = (*Store)(nil)
	_ annexg.ECSAlarmSink    = (*Store)(nil)
	_ annexg.ECSAlarmQuerier = (*Store)(nil)
	_ annexg.TGSAlarmSink    = (*Store)(nil)
	_ annexg.TGSAlarmQuerier = (*Store)(nil)
	_ annexg.DefenceStore    = (*Store)(nil)
)
