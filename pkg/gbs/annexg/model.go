// Package annexg 实现 GB/T 28181-2011 和 GB/T 28181-2016 规范性附录 G 的 XML 协议。
// 2014 修改补充文件继承 2011 附录 G，GB/T 28181-2022 已删除该附录。
package annexg

import (
	"context"
	"encoding/xml"
)

// Version 是校验附录 G 消息时使用的 X-GB-Ver 协议档案。
type Version string

const (
	Version2011 Version = "1.0"
	Version2014 Version = "1.1"
	Version2016 Version = "2.0"
	Version2022 Version = "3.0"
)

// Command 是附录 G 业务命令。
type Command string

const (
	CommandMPAlarm            Command = "MPAlarm"
	CommandECSAlarm           Command = "ECSAlarm"
	CommandTGSAlarm           Command = "TGSAlarm"
	CommandConfigDefence      Command = "ConfigDefence"
	CommandMPAlarmRecordList  Command = "MPAlarmRecordList"
	CommandECSAlarmRecordList Command = "ECSAlarmRecordList"
	CommandTGSAlarmRecordList Command = "TGSAlarmRecordList"
)

// Result 是附录 G 标准业务结果。
type Result string

const (
	ResultOK    Result = "OK"
	ResultError Result = "ERROR"
)

// Message 是可按版本校验的附录 G XML 消息。
type Message interface {
	RootName() string
	CommandType() Command
	Validate(Version) error
}

// MessageSequence 返回用于关联请求和独立业务响应的命令序列号。
func MessageSequence(message Message) (int, bool) {
	switch value := message.(type) {
	case *MPAlarmNotify:
		return value.SN, true
	case *ECSAlarmNotify:
		return value.SN, true
	case *TGSAlarmNotify:
		return value.SN, true
	case *ConfigDefenceNotify:
		return value.SN, true
	case *AlarmRecordQuery:
		return value.SN, true
	case *NotificationResponse:
		return value.SN, true
	case *MPAlarmRecordListResponse:
		return value.SN, true
	case *ECSAlarmRecordListResponse:
		return value.SN, true
	case *TGSAlarmRecordListResponse:
		return value.SN, true
	default:
		return 0, false
	}
}

// MPAlarmSink 保存管理平台报警记录。
type MPAlarmSink interface {
	SaveMPAlarmRecord(context.Context, MPAlarmRecord) error
}

// MPAlarmQuerier 查询管理平台报警记录。
type MPAlarmQuerier interface {
	QueryMPAlarmRecords(context.Context, AlarmRecordQuery) (MPAlarmRecordListResponse, error)
}

// ECSAlarmSink 保存综合接处警报警记录。
type ECSAlarmSink interface {
	SaveECSAlarmRecord(context.Context, ECSAlarmRecord) error
}

// ECSAlarmQuerier 查询综合接处警报警记录。
type ECSAlarmQuerier interface {
	QueryECSAlarmRecords(context.Context, AlarmRecordQuery) (ECSAlarmRecordListResponse, error)
}

// TGSAlarmSink 保存卡口报警记录。
type TGSAlarmSink interface {
	SaveTGSAlarmRecord(context.Context, TGSAlarmRecord) error
}

// TGSAlarmQuerier 查询卡口报警记录。
type TGSAlarmQuerier interface {
	QueryTGSAlarmRecords(context.Context, AlarmRecordQuery) (TGSAlarmRecordListResponse, error)
}

// DefenceStore 是卡口布控状态消费方所需的持久化边界。
type DefenceStore interface {
	ApplyConfigDefence(context.Context, ConfigDefenceNotify) (NotificationResponse, error)
}

// MPAlarmRecord 是管理平台报警记录。
type MPAlarmRecord struct {
	AlarmNO       string   `xml:"AlarmNO"`
	AlarmTime     string   `xml:"AlarmTime"`
	DeviceID      string   `xml:"DeviceID"`
	AlarmClass    *string  `xml:"AlarmClass,omitempty"`
	AlarmPriority string   `xml:"AlarmPriority"`
	AlarmMethod   string   `xml:"AlarmMethod"`
	Longitude     *float64 `xml:"Longitude,omitempty"`
	Latitude      *float64 `xml:"Latitude,omitempty"`
	AlarmAddress  *string  `xml:"AlarmAddress,omitempty"`
	Address       *string  `xml:"Address,omitempty"`
	Name          *string  `xml:"Name,omitempty"`
	Sex           *string  `xml:"Sex,omitempty"`
	Contact       *string  `xml:"Contact,omitempty"`
	CarPlates     []string `xml:"CarPlate,omitempty"`
	PlateTypes    []string `xml:"PlateType,omitempty"`
	Victims       []string `xml:"Victim,omitempty"`
	OriginalNO    string   `xml:"OriginalNO"`
	OriginalInfo  string   `xml:"OriginalInfo"`
	Sender        string   `xml:"Sender"`
	Processor     string   `xml:"Processor"`
	AlarmLevel    string   `xml:"AlarmLevel"`
	Disposal      string   `xml:"Disposal"`
	AlarmInfo     string   `xml:"Alarminfo"`
	Info          []string `xml:"Info,omitempty"`
}

// ECSAlarmRecord 是综合接处警系统报警记录。SrecipientName 保留标准原拼写。
type ECSAlarmRecord struct {
	AlarmNO        string   `xml:"AlarmNO"`
	AlarmTime      string   `xml:"AlarmTime"`
	AlarmPriority  string   `xml:"AlarmPriority"`
	AlarmClass     string   `xml:"AlarmClass"`
	AlarmAddress   string   `xml:"AlarmAddress"`
	AlarmMethod    string   `xml:"AlarmMethod"`
	AlarmTelephone string   `xml:"AlarmTelephone"`
	Longitude      *float64 `xml:"Longitude,omitempty"`
	Latitude       *float64 `xml:"Latitude,omitempty"`
	Name           *string  `xml:"Name,omitempty"`
	Address        *string  `xml:"Address,omitempty"`
	Contact        *string  `xml:"Contact,omitempty"`
	Sex            *string  `xml:"Sex,omitempty"`
	CarPlates      []string `xml:"CarPlate,omitempty"`
	PlateTypes     []string `xml:"PlateType,omitempty"`
	Victims        []string `xml:"Victim,omitempty"`
	Processor      string   `xml:"Processor"`
	SrecipientName string   `xml:"SrecipientName"`
	NsStatus       string   `xml:"NsStatus"`
	NCallType      string   `xml:"NCallType"`
	AlarmInfo      string   `xml:"Alarminfo"`
	Info           []string `xml:"Info,omitempty"`
}

// TGSAlarmRecord 是卡口系统报警记录。
type TGSAlarmRecord struct {
	AlarmTime    string  `xml:"AlarmTime"`
	TollgateID   string  `xml:"TollgateID"`
	CarPlate     string  `xml:"CarPlate"`
	PlateType    string  `xml:"PlateType"`
	DefenceType  string  `xml:"DefenceType"`
	ImageURL     *string `xml:"ImageURL,omitempty"`
	Direction    *string `xml:"Direction,omitempty"`
	VehicleSpeed *int    `xml:"VehicleSpeed,omitempty"`
	PassTime     *string `xml:"PassTime,omitempty"`
}

// MPAlarmNotify 承载管理平台报警通知。
type MPAlarmNotify struct {
	XMLName      xml.Name      `xml:"Notify"`
	CmdType      Command       `xml:"CmdType"`
	SN           int           `xml:"SN"`
	AlarmContent MPAlarmRecord `xml:"AlarmContent"`
}

// ECSAlarmNotify 承载综合接处警系统报警通知。
type ECSAlarmNotify struct {
	XMLName      xml.Name       `xml:"Notify"`
	CmdType      Command        `xml:"CmdType"`
	SN           int            `xml:"SN"`
	AlarmContent ECSAlarmRecord `xml:"AlarmContent"`
}

// TGSAlarmNotify 承载卡口系统报警通知。
type TGSAlarmNotify struct {
	XMLName      xml.Name       `xml:"Notify"`
	CmdType      Command        `xml:"CmdType"`
	SN           int            `xml:"SN"`
	AlarmContent TGSAlarmRecord `xml:"AlarmContent"`
}

// ConfigDefenceNotify 承载卡口布控或撤控通知。Type=true 表示布控，false 表示撤控。
type ConfigDefenceNotify struct {
	XMLName     xml.Name `xml:"Notify"`
	CmdType     Command  `xml:"CmdType"`
	SN          int      `xml:"SN"`
	Type        *bool    `xml:"Type"`
	TollgateID  string   `xml:"TollgateID"`
	CarPlate    string   `xml:"CarPlate"`
	PlateType   string   `xml:"PlateType"`
	DefenceType string   `xml:"DefenceType"`
	DefenceTime string   `xml:"DefenceTime"`
}

// AlarmRecordQuery 是三类附录 G 记录查询的共用模型，只能设置对应 CmdType 定义的过滤字段。
type AlarmRecordQuery struct {
	XMLName           xml.Name `xml:"Query"`
	CmdType           Command  `xml:"CmdType"`
	SN                int      `xml:"SN"`
	BeginTime         *string  `xml:"BeginTime,omitempty"`
	EndTime           *string  `xml:"EndTime,omitempty"`
	AlarmClass        *string  `xml:"AlarmClass,omitempty"`
	AlarmDeviceRange  *string  `xml:"AlarmDeviceRange,omitempty"`
	AlarmPriority     *string  `xml:"AlarmPriority,omitempty"`
	AlarmMethod       *string  `xml:"AlarmMethod,omitempty"`
	AlarmAddressRange *string  `xml:"AlarmAddressRange,omitempty"`
	TollgateID        *string  `xml:"TollgateID,omitempty"`
	CarPlate          *string  `xml:"CarPlate,omitempty"`
	PlateType         *string  `xml:"PlateType,omitempty"`
}

// NotificationResponse 是附录 G 通知的业务应答。
type NotificationResponse struct {
	XMLName xml.Name `xml:"Response"`
	CmdType Command  `xml:"CmdType"`
	SN      int      `xml:"SN"`
	Result  Result   `xml:"Result"`
	Info    []string `xml:"Info,omitempty"`
}

type MPAlarmRecordList struct {
	AlarmRecords []MPAlarmRecord `xml:"AlarmRecord"`
}

type ECSAlarmRecordList struct {
	AlarmRecords []ECSAlarmRecord `xml:"AlarmRecord"`
}

type TGSAlarmRecordList struct {
	AlarmRecords []TGSAlarmRecord `xml:"AlarmRecord"`
}

// MPAlarmRecordListResponse 返回管理平台报警记录。
type MPAlarmRecordListResponse struct {
	XMLName       xml.Name          `xml:"Response"`
	CmdType       Command           `xml:"CmdType"`
	SN            int               `xml:"SN"`
	Result        Result            `xml:"Result"`
	RealRecordNum int               `xml:"RealRecordNum"`
	SendRecordNum int               `xml:"SendRecordNum"`
	RecordList    MPAlarmRecordList `xml:"RecordList"`
}

// ECSAlarmRecordListResponse 返回综合接处警系统报警记录。
type ECSAlarmRecordListResponse struct {
	XMLName       xml.Name           `xml:"Response"`
	CmdType       Command            `xml:"CmdType"`
	SN            int                `xml:"SN"`
	Result        Result             `xml:"Result"`
	RealRecordNum int                `xml:"RealRecordNum"`
	SendRecordNum int                `xml:"SendRecordNum"`
	RecordList    ECSAlarmRecordList `xml:"RecordList"`
}

// TGSAlarmRecordListResponse 返回卡口系统报警记录。
type TGSAlarmRecordListResponse struct {
	XMLName       xml.Name           `xml:"Response"`
	CmdType       Command            `xml:"CmdType"`
	SN            int                `xml:"SN"`
	Result        Result             `xml:"Result"`
	RealRecordNum int                `xml:"RealRecordNum"`
	SendRecordNum int                `xml:"SendRecordNum"`
	RecordList    TGSAlarmRecordList `xml:"RecordList"`
}
