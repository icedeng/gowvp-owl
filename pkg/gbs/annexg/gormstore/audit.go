package gormstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/annexg"
	"gorm.io/gorm"
)

const (
	defaultAuditPageSize = 50
	maximumAuditPageSize = 500
)

// AlarmAuditQuery 是管理端查询附录 G 报警记录使用的过滤和分页条件。
// Kind 只能为 mp、ecs 或 tgs；其余过滤字段继续遵循对应标准命令的字段范围。
type AlarmAuditQuery struct {
	Kind              string
	BeginTime         *time.Time
	EndTime           *time.Time
	AlarmClass        string
	AlarmDeviceRange  string
	AlarmPriority     string
	AlarmMethod       string
	AlarmAddressRange string
	TollgateID        string
	CarPlate          string
	PlateType         string
	Page              int
	PageSize          int
}

// AlarmAuditItem 是一条附录 G 报警审计记录。
// Payload 保留协议层完整报警记录，公共字段同时展开以便管理端筛选和列表展示。
type AlarmAuditItem struct {
	ID            uint64          `json:"id"`
	Kind          string          `json:"kind"`
	AlarmNO       string          `json:"alarm_no,omitempty"`
	AlarmTime     time.Time       `json:"alarm_time"`
	DeviceID      string          `json:"device_id,omitempty"`
	AlarmClass    string          `json:"alarm_class,omitempty"`
	AlarmPriority string          `json:"alarm_priority,omitempty"`
	AlarmMethod   string          `json:"alarm_method,omitempty"`
	AlarmAddress  string          `json:"alarm_address,omitempty"`
	TollgateID    string          `json:"tollgate_id,omitempty"`
	CarPlate      string          `json:"car_plate,omitempty"`
	PlateType     string          `json:"plate_type,omitempty"`
	Payload       json.RawMessage `json:"payload" swaggertype:"object"`
	CreatedAt     time.Time       `json:"created_at"`
}

// AlarmAuditPage 是管理端报警审计分页结果。
type AlarmAuditPage struct {
	Items    []AlarmAuditItem `json:"items"`
	Total    int64            `json:"total"`
	Page     int              `json:"page"`
	PageSize int              `json:"page_size"`
}

// DefenceAuditQuery 是布控当前状态和历史审计共用的过滤与分页条件。
// 时间范围查询状态时作用于 UpdatedAt，查询历史时作用于 CreatedAt。
type DefenceAuditQuery struct {
	TollgateID  string
	CarPlate    string
	PlateType   string
	DefenceType string
	Active      *bool
	BeginTime   *time.Time
	EndTime     *time.Time
	Page        int
	PageSize    int
}

// DefenceStateItem 是卡口布控当前状态。
type DefenceStateItem struct {
	ID          uint64    `json:"id"`
	TollgateID  string    `json:"tollgate_id"`
	CarPlate    string    `json:"car_plate"`
	PlateType   string    `json:"plate_type"`
	DefenceType string    `json:"defence_type"`
	Active      bool      `json:"active"`
	DefenceTime time.Time `json:"defence_time"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// DefenceStatePage 是卡口布控当前状态分页结果。
type DefenceStatePage struct {
	Items    []DefenceStateItem `json:"items"`
	Total    int64              `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

// DefenceAuditItem 是一条不可变的布控或撤控审计记录。
type DefenceAuditItem struct {
	ID          uint64    `json:"id"`
	TollgateID  string    `json:"tollgate_id"`
	CarPlate    string    `json:"car_plate"`
	PlateType   string    `json:"plate_type"`
	DefenceType string    `json:"defence_type"`
	Active      bool      `json:"active"`
	DefenceTime time.Time `json:"defence_time"`
	CreatedAt   time.Time `json:"created_at"`
}

// DefenceAuditPage 是布控历史审计分页结果。
type DefenceAuditPage struct {
	Items    []DefenceAuditItem `json:"items"`
	Total    int64              `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

// ListAlarmAudits 查询附录 G 三类报警的业务审计记录。
func (store *Store) ListAlarmAudits(ctx context.Context, query AlarmAuditQuery) (AlarmAuditPage, error) {
	if ctx == nil {
		return AlarmAuditPage{}, errors.New("nil Annex G audit context")
	}
	db, err := store.database()
	if err != nil {
		return AlarmAuditPage{}, err
	}
	page, pageSize, err := normalizeAuditPage(query.Page, query.PageSize)
	if err != nil {
		return AlarmAuditPage{}, err
	}
	protocolQuery, kind, err := alarmAuditProtocolQuery(query)
	if err != nil {
		return AlarmAuditPage{}, err
	}
	statement := db.WithContext(ctx).Model(new(alarmRecordRow)).Where("kind = ?", kind)
	if query.BeginTime != nil {
		statement = statement.Where("alarm_time >= ?", *query.BeginTime)
	}
	if query.EndTime != nil {
		statement = statement.Where("alarm_time <= ?", *query.EndTime)
	}
	statement = applyQueryFilters(statement, protocolQuery)
	var total int64
	if err := statement.Count(&total).Error; err != nil {
		return AlarmAuditPage{}, err
	}
	rows := make([]alarmRecordRow, 0, min(pageSize, int(total)))
	if total > 0 {
		if err := statement.Order("alarm_time DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
			return AlarmAuditPage{}, err
		}
	}
	items := make([]AlarmAuditItem, 0, len(rows))
	for _, row := range rows {
		if !json.Valid([]byte(row.Payload)) {
			return AlarmAuditPage{}, fmt.Errorf("decode Annex G alarm audit %d: invalid stored JSON", row.ID)
		}
		items = append(items, AlarmAuditItem{
			ID: row.ID, Kind: row.Kind, AlarmNO: row.AlarmNO, AlarmTime: row.AlarmTime,
			DeviceID: row.DeviceID, AlarmClass: row.AlarmClass, AlarmPriority: row.AlarmPriority,
			AlarmMethod: row.AlarmMethod, AlarmAddress: row.AlarmAddress, TollgateID: row.TollgateID,
			CarPlate: row.CarPlate, PlateType: row.PlateType, Payload: json.RawMessage(row.Payload), CreatedAt: row.CreatedAt,
		})
	}
	return AlarmAuditPage{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// ListDefenceStates 查询卡口布控当前状态。
func (store *Store) ListDefenceStates(ctx context.Context, query DefenceAuditQuery) (DefenceStatePage, error) {
	if ctx == nil {
		return DefenceStatePage{}, errors.New("nil Annex G audit context")
	}
	db, err := store.database()
	if err != nil {
		return DefenceStatePage{}, err
	}
	page, pageSize, err := normalizeDefenceAuditQuery(query)
	if err != nil {
		return DefenceStatePage{}, err
	}
	statement := applyDefenceAuditFilters(db.WithContext(ctx).Model(new(defenceStateRow)), query, "updated_at")
	var total int64
	if err := statement.Count(&total).Error; err != nil {
		return DefenceStatePage{}, err
	}
	rows := make([]defenceStateRow, 0, min(pageSize, int(total)))
	if total > 0 {
		if err := statement.Order("updated_at DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
			return DefenceStatePage{}, err
		}
	}
	items := make([]DefenceStateItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, DefenceStateItem{
			ID: row.ID, TollgateID: row.TollgateID, CarPlate: row.CarPlate, PlateType: row.PlateType,
			DefenceType: row.DefenceType, Active: row.Active, DefenceTime: row.DefenceTime, UpdatedAt: row.UpdatedAt,
		})
	}
	return DefenceStatePage{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

// ListDefenceAudits 查询不可变的布控和撤控历史。
func (store *Store) ListDefenceAudits(ctx context.Context, query DefenceAuditQuery) (DefenceAuditPage, error) {
	if ctx == nil {
		return DefenceAuditPage{}, errors.New("nil Annex G audit context")
	}
	db, err := store.database()
	if err != nil {
		return DefenceAuditPage{}, err
	}
	page, pageSize, err := normalizeDefenceAuditQuery(query)
	if err != nil {
		return DefenceAuditPage{}, err
	}
	statement := applyDefenceAuditFilters(db.WithContext(ctx).Model(new(defenceAuditRow)), query, "created_at")
	var total int64
	if err := statement.Count(&total).Error; err != nil {
		return DefenceAuditPage{}, err
	}
	rows := make([]defenceAuditRow, 0, min(pageSize, int(total)))
	if total > 0 {
		if err := statement.Order("created_at DESC, id DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&rows).Error; err != nil {
			return DefenceAuditPage{}, err
		}
	}
	items := make([]DefenceAuditItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, DefenceAuditItem{
			ID: row.ID, TollgateID: row.TollgateID, CarPlate: row.CarPlate, PlateType: row.PlateType,
			DefenceType: row.DefenceType, Active: row.Active, DefenceTime: row.DefenceTime, CreatedAt: row.CreatedAt,
		})
	}
	return DefenceAuditPage{Items: items, Total: total, Page: page, PageSize: pageSize}, nil
}

func alarmAuditProtocolQuery(query AlarmAuditQuery) (annexg.AlarmRecordQuery, string, error) {
	kind := strings.ToLower(strings.TrimSpace(query.Kind))
	command := annexg.Command("")
	switch kind {
	case "mp":
		command = annexg.CommandMPAlarmRecordList
	case "ecs":
		command = annexg.CommandECSAlarmRecordList
	case "tgs":
		command = annexg.CommandTGSAlarmRecordList
	default:
		return annexg.AlarmRecordQuery{}, "", errors.New("Annex G alarm kind must be mp, ecs, or tgs")
	}
	protocolQuery := annexg.AlarmRecordQuery{CmdType: command, SN: 1}
	protocolQuery.AlarmClass = optionalText(query.AlarmClass)
	protocolQuery.AlarmDeviceRange = optionalText(query.AlarmDeviceRange)
	protocolQuery.AlarmPriority = optionalText(query.AlarmPriority)
	protocolQuery.AlarmMethod = optionalText(query.AlarmMethod)
	protocolQuery.AlarmAddressRange = optionalText(query.AlarmAddressRange)
	protocolQuery.TollgateID = optionalText(query.TollgateID)
	protocolQuery.CarPlate = optionalText(query.CarPlate)
	protocolQuery.PlateType = optionalText(query.PlateType)
	if err := protocolQuery.Validate(annexg.Version2011); err != nil {
		return annexg.AlarmRecordQuery{}, "", err
	}
	if err := validateAuditTimeRange(query.BeginTime, query.EndTime); err != nil {
		return annexg.AlarmRecordQuery{}, "", err
	}
	return protocolQuery, kind, nil
}

func normalizeDefenceAuditQuery(query DefenceAuditQuery) (int, int, error) {
	if err := validateAuditTimeRange(query.BeginTime, query.EndTime); err != nil {
		return 0, 0, err
	}
	return normalizeAuditPage(query.Page, query.PageSize)
}

func normalizeAuditPage(page, pageSize int) (int, int, error) {
	if page < 0 || pageSize < 0 {
		return 0, 0, errors.New("Annex G audit page and page_size must not be negative")
	}
	if page == 0 {
		page = 1
	}
	if pageSize == 0 {
		pageSize = defaultAuditPageSize
	}
	if pageSize > maximumAuditPageSize {
		return 0, 0, fmt.Errorf("Annex G audit page_size must be at most %d", maximumAuditPageSize)
	}
	if page > math.MaxInt/pageSize {
		return 0, 0, errors.New("Annex G audit page offset is too large")
	}
	return page, pageSize, nil
}

func validateAuditTimeRange(begin, end *time.Time) error {
	if begin != nil && end != nil && begin.After(*end) {
		return errors.New("Annex G audit begin_time must not be after end_time")
	}
	return nil
}

func applyDefenceAuditFilters(statement *gorm.DB, query DefenceAuditQuery, timeColumn string) *gorm.DB {
	if value := strings.TrimSpace(query.TollgateID); value != "" {
		statement = statement.Where("tollgate_id = ?", value)
	}
	if value := strings.TrimSpace(query.CarPlate); value != "" {
		statement = statement.Where("car_plate = ?", value)
	}
	if value := strings.TrimSpace(query.PlateType); value != "" {
		statement = statement.Where("plate_type = ?", value)
	}
	if value := strings.TrimSpace(query.DefenceType); value != "" {
		statement = statement.Where("defence_type = ?", value)
	}
	if query.Active != nil {
		statement = statement.Where("active = ?", *query.Active)
	}
	if query.BeginTime != nil {
		statement = statement.Where(timeColumn+" >= ?", *query.BeginTime)
	}
	if query.EndTime != nil {
		statement = statement.Where(timeColumn+" <= ?", *query.EndTime)
	}
	return statement
}

func optionalText(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
