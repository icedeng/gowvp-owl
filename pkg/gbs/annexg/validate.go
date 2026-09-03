package annexg

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrInvalidVersion     = errors.New("invalid GB/T 28181 version")
	ErrUnsupportedVersion = errors.New("Annex G is not supported by this GB/T 28181 version")
)

// ParseVersion 同时接受 X-GB-Ver 和项目历史使用的年份值。
func ParseVersion(value string) (Version, bool) {
	switch strings.TrimSpace(value) {
	case "1.0", "2011":
		return Version2011, true
	case "1.1", "2014", "2011-supplement-2014":
		return Version2014, true
	case "2.0", "2016":
		return Version2016, true
	case "3.0", "2022":
		return Version2022, true
	default:
		return "", false
	}
}

func validateVersion(version Version) error {
	parsed, ok := ParseVersion(string(version))
	if !ok {
		return fmt.Errorf("%w: %q", ErrInvalidVersion, version)
	}
	if parsed == Version2022 {
		return fmt.Errorf("%w: GB/T 28181-2022 removes normative Annex G", ErrUnsupportedVersion)
	}
	return nil
}

func validateEnvelope(version Version, actualRoot, expectedRoot string, sn int, actualCommand, expectedCommand Command) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	if actualRoot != "" && actualRoot != expectedRoot {
		return fmt.Errorf("invalid Annex G root %q, want %q", actualRoot, expectedRoot)
	}
	if actualCommand != expectedCommand {
		return fmt.Errorf("invalid Annex G command %q, want %q", actualCommand, expectedCommand)
	}
	if sn <= 0 {
		return fmt.Errorf("invalid Annex G SN %d", sn)
	}
	return nil
}

func validateResult(result Result) error {
	if result != ResultOK && result != ResultError {
		return fmt.Errorf("invalid Annex G result %q", result)
	}
	return nil
}

func parseDateTime(value string) (time.Time, error) {
	return ParseDateTime(value)
}

// ParseDateTime 按附录 G 的 XML dateTime 规则解析时间；无时区值按北京时间解释。
func ParseDateTime(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("empty dateTime")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	beijing := time.FixedZone("CST", 8*60*60)
	for _, layout := range []string{"2006-01-02T15:04:05.999999999", "2006-01-02T15:04:05"} {
		if parsed, err := time.ParseInLocation(layout, value, beijing); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid dateTime %q", value)
}

func validateCoordinates(longitude, latitude *float64) error {
	if longitude != nil && (math.IsNaN(*longitude) || math.IsInf(*longitude, 0) || *longitude < -180 || *longitude > 180) {
		return fmt.Errorf("invalid longitude %v", *longitude)
	}
	if latitude != nil && (math.IsNaN(*latitude) || math.IsInf(*latitude, 0) || *latitude < -90 || *latitude > 90) {
		return fmt.Errorf("invalid latitude %v", *latitude)
	}
	return nil
}

func validateMPRecord(record *MPAlarmRecord) error {
	if record == nil {
		return errors.New("missing MPAlarm record")
	}
	if _, err := parseDateTime(record.AlarmTime); err != nil {
		return fmt.Errorf("invalid MPAlarm AlarmTime: %w", err)
	}
	if err := validateCoordinates(record.Longitude, record.Latitude); err != nil {
		return fmt.Errorf("invalid MPAlarm coordinates: %w", err)
	}
	if err := requireStrings("MPAlarm",
		stringField{"AlarmNO", record.AlarmNO}, stringField{"DeviceID", record.DeviceID},
		stringField{"AlarmPriority", record.AlarmPriority}, stringField{"AlarmMethod", record.AlarmMethod},
		stringField{"OriginalNO", record.OriginalNO}, stringField{"OriginalInfo", record.OriginalInfo},
		stringField{"Sender", record.Sender}, stringField{"Processor", record.Processor},
		stringField{"AlarmLevel", record.AlarmLevel}, stringField{"Disposal", record.Disposal},
		stringField{"Alarminfo", record.AlarmInfo},
	); err != nil {
		return err
	}
	return nil
}

func validateECSRecord(record *ECSAlarmRecord) error {
	if record == nil {
		return errors.New("missing ECSAlarm record")
	}
	if _, err := parseDateTime(record.AlarmTime); err != nil {
		return fmt.Errorf("invalid ECSAlarm AlarmTime: %w", err)
	}
	if err := validateCoordinates(record.Longitude, record.Latitude); err != nil {
		return fmt.Errorf("invalid ECSAlarm coordinates: %w", err)
	}
	if err := requireStrings("ECSAlarm",
		stringField{"AlarmNO", record.AlarmNO}, stringField{"AlarmPriority", record.AlarmPriority},
		stringField{"AlarmClass", record.AlarmClass}, stringField{"AlarmAddress", record.AlarmAddress},
		stringField{"AlarmMethod", record.AlarmMethod}, stringField{"AlarmTelephone", record.AlarmTelephone},
		stringField{"Processor", record.Processor}, stringField{"SrecipientName", record.SrecipientName},
		stringField{"NsStatus", record.NsStatus}, stringField{"NCallType", record.NCallType},
		stringField{"Alarminfo", record.AlarmInfo},
	); err != nil {
		return err
	}
	return nil
}

func validateTGSRecord(record *TGSAlarmRecord) error {
	if record == nil {
		return errors.New("missing TGSAlarm record")
	}
	if _, err := parseDateTime(record.AlarmTime); err != nil {
		return fmt.Errorf("invalid TGSAlarm AlarmTime: %w", err)
	}
	if record.PassTime != nil {
		if _, err := parseDateTime(*record.PassTime); err != nil {
			return fmt.Errorf("invalid TGSAlarm PassTime: %w", err)
		}
	}
	if err := requireStrings("TGSAlarm",
		stringField{"TollgateID", record.TollgateID}, stringField{"CarPlate", record.CarPlate},
		stringField{"PlateType", record.PlateType}, stringField{"DefenceType", record.DefenceType},
	); err != nil {
		return err
	}
	return nil
}

type stringField struct {
	name  string
	value string
}

func requireStrings(command string, fields ...stringField) error {
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s requires non-empty %s", command, field.name)
		}
	}
	return nil
}

func (*MPAlarmNotify) RootName() string { return "Notify" }
func (message *MPAlarmNotify) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *MPAlarmNotify) Validate(version Version) error {
	if message == nil {
		return errors.New("nil MPAlarm notification")
	}
	if err := validateEnvelope(version, message.XMLName.Local, message.RootName(), message.SN, message.CmdType, CommandMPAlarm); err != nil {
		return err
	}
	return validateMPRecord(&message.AlarmContent)
}

func (*ECSAlarmNotify) RootName() string { return "Notify" }
func (message *ECSAlarmNotify) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *ECSAlarmNotify) Validate(version Version) error {
	if message == nil {
		return errors.New("nil ECSAlarm notification")
	}
	if err := validateEnvelope(version, message.XMLName.Local, message.RootName(), message.SN, message.CmdType, CommandECSAlarm); err != nil {
		return err
	}
	return validateECSRecord(&message.AlarmContent)
}

func (*TGSAlarmNotify) RootName() string { return "Notify" }
func (message *TGSAlarmNotify) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *TGSAlarmNotify) Validate(version Version) error {
	if message == nil {
		return errors.New("nil TGSAlarm notification")
	}
	if err := validateEnvelope(version, message.XMLName.Local, message.RootName(), message.SN, message.CmdType, CommandTGSAlarm); err != nil {
		return err
	}
	return validateTGSRecord(&message.AlarmContent)
}

func (*ConfigDefenceNotify) RootName() string { return "Notify" }
func (message *ConfigDefenceNotify) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *ConfigDefenceNotify) Validate(version Version) error {
	if message == nil {
		return errors.New("nil ConfigDefence notification")
	}
	if err := validateEnvelope(version, message.XMLName.Local, message.RootName(), message.SN, message.CmdType, CommandConfigDefence); err != nil {
		return err
	}
	if message.Type == nil {
		return errors.New("missing ConfigDefence Type")
	}
	if _, err := parseDateTime(message.DefenceTime); err != nil {
		return fmt.Errorf("invalid ConfigDefence DefenceTime: %w", err)
	}
	if err := requireStrings("ConfigDefence",
		stringField{"TollgateID", message.TollgateID}, stringField{"CarPlate", message.CarPlate},
		stringField{"PlateType", message.PlateType}, stringField{"DefenceType", message.DefenceType},
	); err != nil {
		return err
	}
	return nil
}

func (*AlarmRecordQuery) RootName() string { return "Query" }
func (message *AlarmRecordQuery) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *AlarmRecordQuery) Validate(version Version) error {
	if message == nil {
		return errors.New("nil alarm record query")
	}
	if err := validateVersion(version); err != nil {
		return err
	}
	if message.XMLName.Local != "" && message.XMLName.Local != message.RootName() {
		return fmt.Errorf("invalid Annex G root %q, want %q", message.XMLName.Local, message.RootName())
	}
	if message.SN <= 0 {
		return fmt.Errorf("invalid Annex G SN %d", message.SN)
	}
	if err := validateQueryTimes(message.BeginTime, message.EndTime); err != nil {
		return err
	}
	switch message.CmdType {
	case CommandMPAlarmRecordList:
		if anySet(message.AlarmAddressRange, message.TollgateID, message.CarPlate, message.PlateType) {
			return errors.New("MPAlarmRecordList contains filters from another command")
		}
	case CommandECSAlarmRecordList:
		if anySet(message.AlarmDeviceRange, message.TollgateID, message.CarPlate, message.PlateType) {
			return errors.New("ECSAlarmRecordList contains filters from another command")
		}
	case CommandTGSAlarmRecordList:
		if anySet(message.AlarmClass, message.AlarmDeviceRange, message.AlarmPriority, message.AlarmMethod, message.AlarmAddressRange) {
			return errors.New("TGSAlarmRecordList contains filters from another command")
		}
	default:
		return fmt.Errorf("invalid Annex G query command %q", message.CmdType)
	}
	return nil
}

func validateQueryTimes(begin, end *string) error {
	var beginTime, endTime time.Time
	var err error
	if begin != nil {
		beginTime, err = parseDateTime(*begin)
		if err != nil {
			return fmt.Errorf("invalid BeginTime: %w", err)
		}
	}
	if end != nil {
		endTime, err = parseDateTime(*end)
		if err != nil {
			return fmt.Errorf("invalid EndTime: %w", err)
		}
	}
	if begin != nil && end != nil && endTime.Before(beginTime) {
		return errors.New("EndTime precedes BeginTime")
	}
	return nil
}

func anySet(values ...*string) bool {
	for _, value := range values {
		if value != nil {
			return true
		}
	}
	return false
}

func (*NotificationResponse) RootName() string { return "Response" }
func (message *NotificationResponse) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *NotificationResponse) Validate(version Version) error {
	if message == nil {
		return errors.New("nil notification response")
	}
	switch message.CmdType {
	case CommandMPAlarm, CommandECSAlarm, CommandTGSAlarm, CommandConfigDefence:
	default:
		return fmt.Errorf("invalid Annex G notification response command %q", message.CmdType)
	}
	if err := validateEnvelope(version, message.XMLName.Local, message.RootName(), message.SN, message.CmdType, message.CmdType); err != nil {
		return err
	}
	if err := validateResult(message.Result); err != nil {
		return err
	}
	if message.CmdType != CommandConfigDefence && len(message.Info) > 0 {
		return fmt.Errorf("%s response does not allow Info", message.CmdType)
	}
	for _, info := range message.Info {
		if utf8.RuneCountInString(info) > 1024 {
			return errors.New("ConfigDefence response Info exceeds 1024 characters")
		}
	}
	return nil
}

func validateRecordResponse(version Version, actualRoot string, sn int, actualCommand, expectedCommand Command, result Result, real, sent, records int) error {
	if err := validateEnvelope(version, actualRoot, "Response", sn, actualCommand, expectedCommand); err != nil {
		return err
	}
	if err := validateResult(result); err != nil {
		return err
	}
	if real < 0 || sent < 0 {
		return errors.New("Annex G record counts must be non-negative")
	}
	if sent > real {
		return errors.New("SendRecordNum exceeds RealRecordNum")
	}
	if sent != records {
		return fmt.Errorf("SendRecordNum %d does not match %d AlarmRecord elements", sent, records)
	}
	return nil
}

func (*MPAlarmRecordListResponse) RootName() string { return "Response" }
func (message *MPAlarmRecordListResponse) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *MPAlarmRecordListResponse) Validate(version Version) error {
	if message == nil {
		return errors.New("nil MPAlarmRecordList response")
	}
	if err := validateRecordResponse(version, message.XMLName.Local, message.SN, message.CmdType, CommandMPAlarmRecordList, message.Result, message.RealRecordNum, message.SendRecordNum, len(message.RecordList.AlarmRecords)); err != nil {
		return err
	}
	for index := range message.RecordList.AlarmRecords {
		if err := validateMPRecord(&message.RecordList.AlarmRecords[index]); err != nil {
			return fmt.Errorf("invalid MPAlarmRecordList item %d: %w", index, err)
		}
	}
	return nil
}

func (*ECSAlarmRecordListResponse) RootName() string { return "Response" }
func (message *ECSAlarmRecordListResponse) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *ECSAlarmRecordListResponse) Validate(version Version) error {
	if message == nil {
		return errors.New("nil ECSAlarmRecordList response")
	}
	if err := validateRecordResponse(version, message.XMLName.Local, message.SN, message.CmdType, CommandECSAlarmRecordList, message.Result, message.RealRecordNum, message.SendRecordNum, len(message.RecordList.AlarmRecords)); err != nil {
		return err
	}
	for index := range message.RecordList.AlarmRecords {
		if err := validateECSRecord(&message.RecordList.AlarmRecords[index]); err != nil {
			return fmt.Errorf("invalid ECSAlarmRecordList item %d: %w", index, err)
		}
	}
	return nil
}

func (*TGSAlarmRecordListResponse) RootName() string { return "Response" }
func (message *TGSAlarmRecordListResponse) CommandType() Command {
	if message == nil {
		return ""
	}
	return message.CmdType
}
func (message *TGSAlarmRecordListResponse) Validate(version Version) error {
	if message == nil {
		return errors.New("nil TGSAlarmRecordList response")
	}
	if err := validateRecordResponse(version, message.XMLName.Local, message.SN, message.CmdType, CommandTGSAlarmRecordList, message.Result, message.RealRecordNum, message.SendRecordNum, len(message.RecordList.AlarmRecords)); err != nil {
		return err
	}
	for index := range message.RecordList.AlarmRecords {
		if err := validateTGSRecord(&message.RecordList.AlarmRecords[index]); err != nil {
			return fmt.Errorf("invalid TGSAlarmRecordList item %d: %w", index, err)
		}
	}
	return nil
}
