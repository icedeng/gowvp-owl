package gbs

import (
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type notificationElementRule struct {
	name               string
	minOccurs          int
	maxOccurs          int
	maxRunes           int
	requireText        bool
	attributes         []notificationAttributeRule
	children           []notificationElementRule
	knownChildren      []string
	allowNested        bool
	allowSimpleText    bool
	allowUnknownTail   bool
	allowKnownAnyOrder bool
}

type notificationAttributeRule struct {
	name      string
	minOccurs int
	maxOccurs int
}

const maxNotificationOccurs = int(^uint(0) >> 1)

var (
	videoUploadNotifyRules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "Time", minOccurs: 1, maxOccurs: 1},
		// A.2.5.8 的字段说明明确将经度、纬度标为可选；发布文本中的
		// Schema 行遗漏了对应的 minOccurs="0"。按字段语义接受二者独立省略，
		// 出现时仍由业务校验限制为有限的 WGS-84 坐标。
		{name: "Longitude", minOccurs: 0, maxOccurs: 1},
		{name: "Latitude", minOccurs: 0, maxOccurs: 1},
	}
	deviceUpgradeResultRules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "SessionID", minOccurs: 1, maxOccurs: 1},
		{name: "UpgradeResult", minOccurs: 1, maxOccurs: 1},
		{name: "Firmware", minOccurs: 1, maxOccurs: 1},
		{name: "UpgradeFailedReason", minOccurs: 0, maxOccurs: 1},
	}
	snapshotFinishedRules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "SessionID", minOccurs: 1, maxOccurs: 1},
		{
			name: "SnapShotList", minOccurs: 1, maxOccurs: 1,
			children: []notificationElementRule{{name: "SnapShotFileID", minOccurs: 0, maxOccurs: 10}},
		},
	}
	mediaStatusRules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "NotifyType", minOccurs: 1, maxOccurs: 1},
	}
	keepaliveRules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		// 兼容存量设备省略 Status；标准值和厂商兼容值仍由业务校验限制。
		{name: "Status", minOccurs: 0, maxOccurs: 1},
		{
			name: "Info", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{{name: "DeviceID", minOccurs: 0, maxOccurs: maxNotificationOccurs}},
		},
	}
	alarmTypedInfoRules = []notificationElementRule{
		{name: "AlarmType", minOccurs: 0, maxOccurs: 1, requireText: true},
		{
			name: "AlarmTypeParam", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{{name: "EventType", minOccurs: 0, maxOccurs: 1, requireText: true}},
		},
	}
	broadcastResponseRules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "Result", minOccurs: 1, maxOccurs: 1},
	}
	broadcastNotifyRules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "SourceID", minOccurs: 1, maxOccurs: 1},
		{name: "TargetID", minOccurs: 1, maxOccurs: 1},
	}
	deviceInfo2011Rules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "Result", minOccurs: 1, maxOccurs: 1},
		// DeviceType、MaxCamera、MaxAlarm 来自四版规范示例，作为兼容字段保留。
		{name: "DeviceType", minOccurs: 0, maxOccurs: 1},
		{name: "Manufacturer", minOccurs: 0, maxOccurs: 1},
		{name: "Model", minOccurs: 0, maxOccurs: 1},
		{name: "Firmware", minOccurs: 0, maxOccurs: 1},
		{name: "MaxCamera", minOccurs: 0, maxOccurs: 1},
		{name: "MaxAlarm", minOccurs: 0, maxOccurs: 1},
		{name: "Channel", minOccurs: 0, maxOccurs: 1},
		{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs},
	}
	deviceInfo2014Rules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceName", minOccurs: 0, maxOccurs: 1},
		{name: "Result", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceType", minOccurs: 0, maxOccurs: 1},
		{name: "Manufacturer", minOccurs: 0, maxOccurs: 1},
		{name: "Model", minOccurs: 0, maxOccurs: 1},
		{name: "Firmware", minOccurs: 0, maxOccurs: 1},
		{name: "MaxCamera", minOccurs: 0, maxOccurs: 1},
		{name: "MaxAlarm", minOccurs: 0, maxOccurs: 1},
		{name: "Channel", minOccurs: 0, maxOccurs: 1},
		{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs},
	}
	deviceInfo2022Rules = []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceName", minOccurs: 0, maxOccurs: 1},
		{name: "Result", minOccurs: 1, maxOccurs: 1},
		{name: "Manufacturer", minOccurs: 0, maxOccurs: 1},
		{name: "Model", minOccurs: 0, maxOccurs: 1},
		{name: "Firmware", minOccurs: 0, maxOccurs: 1},
		{name: "Channel", minOccurs: 0, maxOccurs: 1},
		// 2022 的基本扩展改为 ExtraInfo；结构化 Info 仅承载已另行校验的附录 A.4 对象。
		{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs, allowNested: true},
		{name: "ExtraInfo", minOccurs: 0, maxOccurs: maxNotificationOccurs},
	}
)

func deviceStatusResponseRules(version GBProtocolVersion) []notificationElementRule {
	alarmAttribute := "Num"
	alarmItemRules := []notificationElementRule{
		{name: "DeviceID", minOccurs: 0, maxOccurs: 1},
		{name: "DutyStatus", minOccurs: 0, maxOccurs: 1},
	}
	switch version {
	case GBVersion10:
		// 2011 的 Schema 使用 Status，J.11 示例使用 DutyStatus，两种标准内写法均兼容。
		alarmItemRules = []notificationElementRule{
			{name: "DeviceID", minOccurs: 0, maxOccurs: 1},
			{name: "Status", minOccurs: 0, maxOccurs: 1},
			{name: "DutyStatus", minOccurs: 0, maxOccurs: 1},
		}
	case GBVersion11:
		// 2014 修改补充文件将报警状态字段修订为 StatusDutyStatus；
		// DutyStatus 作为存量设备兼容字段由后续语义校验决定是否接受。
		alarmItemRules = []notificationElementRule{
			{name: "DeviceID", minOccurs: 0, maxOccurs: 1},
			{name: "StatusDutyStatus", minOccurs: 0, maxOccurs: 1},
			{name: "DutyStatus", minOccurs: 0, maxOccurs: 1},
		}
	case GBVersion20:
		alarmAttribute = "num"
	}

	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "Result", minOccurs: 1, maxOccurs: 1},
		{name: "Online", minOccurs: 1, maxOccurs: 1},
		{name: "Status", minOccurs: 1, maxOccurs: 1},
		{name: "Reason", minOccurs: 0, maxOccurs: 1},
		{name: "Encode", minOccurs: 0, maxOccurs: 1},
		{name: "Record", minOccurs: 0, maxOccurs: 1},
		{name: "DeviceTime", minOccurs: 0, maxOccurs: 1},
		{
			name: "Alarmstatus", minOccurs: 0, maxOccurs: 1,
			attributes: []notificationAttributeRule{{name: alarmAttribute, minOccurs: 1, maxOccurs: 1}},
			children: []notificationElementRule{{
				name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs, children: alarmItemRules,
			}},
		},
	}
	if version == GBVersion30 {
		// 结构化 Info 仅承载另行校验的附录 A.4 对象；基本扩展使用 ExtraInfo。
		return append(rules,
			notificationElementRule{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs, allowNested: true},
			notificationElementRule{name: "ExtraInfo", minOccurs: 0, maxOccurs: maxNotificationOccurs},
		)
	}
	return append(rules, notificationElementRule{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs})
}

func presetQueryResponseRules(version GBProtocolVersion) []notificationElementRule {
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
	}
	if version == GBVersion30 {
		rules = append(rules, notificationElementRule{name: "SumNum", minOccurs: 1, maxOccurs: 1})
	} else {
		// 兼容存量 2014/2016 设备在标准 PresetList 前附带 Result、SumNum。
		rules = append(rules,
			notificationElementRule{name: "Result", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SumNum", minOccurs: 0, maxOccurs: 1},
		)
	}
	return append(rules, notificationElementRule{
		name: "PresetList", minOccurs: 1, maxOccurs: 1,
		attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
		children: []notificationElementRule{{
			name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs,
			children: []notificationElementRule{
				{name: "PresetID", minOccurs: 1, maxOccurs: 1},
				{name: "PresetName", minOccurs: 1, maxOccurs: 1},
			},
		}},
	})
}

func recordInfoResponseRules(version GBProtocolVersion) []notificationElementRule {
	recordListMinOccurs := 0
	if version == GBVersion10 {
		// 2011 A.2.6 未声明 minOccurs；2014 起附录 M 明确零结果不携带记录列表。
		recordListMinOccurs = 1
	}
	itemRules := []notificationElementRule{
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "Name", minOccurs: 1, maxOccurs: 1},
		{name: "FilePath", minOccurs: 0, maxOccurs: 1},
		{name: "Address", minOccurs: 0, maxOccurs: 1},
		{name: "StartTime", minOccurs: 0, maxOccurs: 1},
		{name: "EndTime", minOccurs: 0, maxOccurs: 1},
		{name: "Secrecy", minOccurs: 1, maxOccurs: 1},
		{name: "Type", minOccurs: 0, maxOccurs: 1},
		{name: "RecorderID", minOccurs: 0, maxOccurs: 1},
	}
	if version.AtLeast(GBVersion20) {
		itemRules = append(itemRules, notificationElementRule{name: "FileSize", minOccurs: 0, maxOccurs: 1})
	}
	if version == GBVersion30 {
		itemRules = append(itemRules,
			notificationElementRule{name: "RecordLocation", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "StreamNumber", minOccurs: 0, maxOccurs: 1},
		)
	}

	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "Name", minOccurs: 1, maxOccurs: 1},
		{name: "SumNum", minOccurs: 1, maxOccurs: 1},
		{
			name: "RecordList", minOccurs: recordListMinOccurs, maxOccurs: 1,
			attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
			children: []notificationElementRule{{
				name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs, children: itemRules,
			}},
		},
	}
	if version == GBVersion30 {
		return append(rules, notificationElementRule{name: "ExtraInfo", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024})
	}
	return append(rules, notificationElementRule{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024})
}

var (
	catalogItemKnownChildren = []string{
		"DeviceID", "Event", "Name", "Manufacturer", "Model", "Owner", "CivilCode", "Block", "Address",
		"Parental", "ParentID", "SafetyWay", "RegisterWay", "CertNum", "Certifiable", "ErrCode", "EndTime",
		"SecurityLevelCode", "Secrecy", "IPAddress", "Port", "Password", "Status", "Longitude", "Latitude",
		"BusinessGroupID", "Info",
	}
	catalogInfoKnownChildren = []string{
		"PTZType", "PhotoelectricImagingType", "CapturePositionType", "PositionType", "RoomType", "UseType",
		"SupplyLightType", "DirectionType", "Resolution", "StreamNumberList", "BusinessGroupID", "DownloadSpeed",
		"SVCSpaceSupportMode", "SVCTimeSupportMode", "SSVCRatioSupportList", "MobileDeviceType",
		"HorizontalFieldAngle", "VerticalFieldAngle", "MaxViewDistance", "GrassrootsCode", "PointType",
		"PointCommonName", "MAC", "FunctionType", "EncodeType", "InstallTime", "ManagementUnit", "ContactInfo",
		"RecordSaveDays", "IndustrialClassification",
	}
)

func catalogInfoRules(version GBProtocolVersion) []notificationElementRule {
	rules := []notificationElementRule{
		{name: "PTZType", minOccurs: 0, maxOccurs: 1},
	}
	switch version {
	case GBVersion11:
		rules = append(rules,
			notificationElementRule{name: "PositionType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "RoomType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "UseType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SupplyLightType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "DirectionType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "Resolution", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "BusinessGroupID", minOccurs: 0, maxOccurs: 1},
		)
	case GBVersion20:
		rules = append(rules,
			notificationElementRule{name: "PositionType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "RoomType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "UseType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SupplyLightType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "DirectionType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "Resolution", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "BusinessGroupID", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "DownloadSpeed", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SVCSpaceSupportMode", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SVCTimeSupportMode", minOccurs: 0, maxOccurs: 1},
		)
	case GBVersion30:
		rules = append(rules,
			notificationElementRule{name: "PhotoelectricImagingType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "CapturePositionType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "RoomType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SupplyLightType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "DirectionType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "Resolution", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "StreamNumberList", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "DownloadSpeed", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SVCSpaceSupportMode", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SVCTimeSupportMode", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "SSVCRatioSupportList", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "MobileDeviceType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "HorizontalFieldAngle", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "VerticalFieldAngle", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "MaxViewDistance", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "GrassrootsCode", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "PointType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "PointCommonName", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "MAC", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "FunctionType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "EncodeType", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "InstallTime", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "ManagementUnit", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "ContactInfo", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "RecordSaveDays", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "IndustrialClassification", minOccurs: 0, maxOccurs: 1},
		)
	}
	return rules
}

func catalogItemRules(version GBProtocolVersion, notification bool) []notificationElementRule {
	rules := []notificationElementRule{{name: "DeviceID", minOccurs: 1, maxOccurs: 1}}
	if notification && version.AtLeast(GBVersion11) {
		rules = append(rules, notificationElementRule{name: "Event", minOccurs: 1, maxOccurs: 1})
	}
	rules = append(rules,
		notificationElementRule{name: "Name", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Manufacturer", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Model", minOccurs: 0, maxOccurs: 1},
	)
	if version != GBVersion30 {
		rules = append(rules, notificationElementRule{name: "Owner", minOccurs: 0, maxOccurs: 1})
	}
	rules = append(rules,
		notificationElementRule{name: "CivilCode", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Block", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Address", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Parental", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "ParentID", minOccurs: 0, maxOccurs: 1},
	)
	if version != GBVersion30 {
		rules = append(rules, notificationElementRule{name: "SafetyWay", minOccurs: 0, maxOccurs: 1})
	}
	rules = append(rules, notificationElementRule{name: "RegisterWay", minOccurs: 0, maxOccurs: 1})
	if version != GBVersion30 {
		rules = append(rules,
			notificationElementRule{name: "CertNum", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "Certifiable", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "ErrCode", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "EndTime", minOccurs: 0, maxOccurs: 1},
		)
	} else {
		rules = append(rules, notificationElementRule{name: "SecurityLevelCode", minOccurs: 0, maxOccurs: 1})
	}
	rules = append(rules,
		notificationElementRule{name: "Secrecy", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "IPAddress", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Port", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Password", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Status", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Longitude", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Latitude", minOccurs: 0, maxOccurs: 1},
	)
	if version == GBVersion30 {
		rules = append(rules, notificationElementRule{name: "BusinessGroupID", minOccurs: 0, maxOccurs: 1})
	}
	if version.AtLeast(GBVersion11) {
		rules = append(rules, notificationElementRule{
			name: "Info", minOccurs: 0, maxOccurs: 1, children: catalogInfoRules(version),
			knownChildren: catalogInfoKnownChildren, allowUnknownTail: true,
		})
	}
	return rules
}

func catalogEnvelopeRules(version GBProtocolVersion, notification bool) []notificationElementRule {
	itemRules := catalogItemRules(version, notification)
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
	}
	if notification && version.AtLeast(GBVersion11) {
		rules = append(rules, notificationElementRule{name: "Status", minOccurs: 0, maxOccurs: 1})
	}
	rules = append(rules,
		notificationElementRule{name: "SumNum", minOccurs: 1, maxOccurs: 1},
		notificationElementRule{
			name: "DeviceList", minOccurs: 0, maxOccurs: 1,
			attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
			children: []notificationElementRule{{
				name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs, children: itemRules,
				knownChildren: catalogItemKnownChildren, allowUnknownTail: true, allowKnownAnyOrder: true,
			}},
		},
	)
	if version == GBVersion30 {
		return append(rules, notificationElementRule{name: "ExtraInfo", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024})
	}
	return append(rules, notificationElementRule{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024})
}

var (
	homePositionQueryResponseRules = append(genericQueryResponseRules(), notificationElementRule{
		name: "HomePosition", minOccurs: 0, maxOccurs: 1,
		children: []notificationElementRule{
			{name: "Enabled", minOccurs: 1, maxOccurs: 1},
			{name: "ResetTime", minOccurs: 0, maxOccurs: 1},
			{name: "PresetIndex", minOccurs: 0, maxOccurs: 1},
		},
	})
	cruiseTrackListQueryResponseRules = append(genericQueryResponseRules(),
		notificationElementRule{name: "SumNum", minOccurs: 1, maxOccurs: 1},
		notificationElementRule{
			name: "CruiseTrackList", minOccurs: 0, maxOccurs: 1,
			attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
			children: []notificationElementRule{{
				name: "CruiseTrack", minOccurs: 0, maxOccurs: maxNotificationOccurs,
				children: []notificationElementRule{
					{name: "Number", minOccurs: 1, maxOccurs: 1},
					{name: "Name", minOccurs: 0, maxOccurs: 1},
				},
			}},
		},
	)
	cruiseTrackQueryResponseRules = append(genericQueryResponseRules(),
		notificationElementRule{name: "Number", minOccurs: 1, maxOccurs: 1},
		notificationElementRule{name: "Name", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "SumNum", minOccurs: 1, maxOccurs: 1},
		notificationElementRule{
			name: "CruisePointList", minOccurs: 0, maxOccurs: 1,
			attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
			children: []notificationElementRule{{
				name: "CruisePoint", minOccurs: 0, maxOccurs: maxNotificationOccurs,
				children: []notificationElementRule{
					{name: "PresetIndex", minOccurs: 1, maxOccurs: 1},
					{name: "StayTime", minOccurs: 1, maxOccurs: 1},
					{name: "Speed", minOccurs: 1, maxOccurs: 1},
				},
			}},
		},
	)
	ptzPositionQueryResponseRules = append(genericQueryResponseRules(),
		notificationElementRule{name: "Pan", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Tilt", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "Zoom", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "HorizontalFieldAngle", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "VerticalFieldAngle", minOccurs: 0, maxOccurs: 1},
		notificationElementRule{name: "MaxViewDistance", minOccurs: 0, maxOccurs: 1},
	)
	sdCardStatusQueryResponseRules = append(genericQueryResponseRules(),
		notificationElementRule{name: "SumNum", minOccurs: 1, maxOccurs: 1},
		notificationElementRule{
			name: "SDCardStatusInfo", minOccurs: 0, maxOccurs: 1,
			attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
			children: []notificationElementRule{{
				name: "Item", minOccurs: 0, maxOccurs: 8,
				children: []notificationElementRule{
					{name: "ID", minOccurs: 1, maxOccurs: 1},
					{name: "HddName", minOccurs: 1, maxOccurs: 1},
					{name: "Status", minOccurs: 1, maxOccurs: 1},
					{name: "FormatProgress", minOccurs: 0, maxOccurs: 1},
					{name: "Capacity", minOccurs: 1, maxOccurs: 1},
					{name: "FreeSpace", minOccurs: 1, maxOccurs: 1},
				},
			}},
		},
	)
)

func genericQueryResponseRules() []notificationElementRule {
	return []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
	}
}

func validateVideoUploadNotifyStructure(body []byte) error {
	return validateIndependentNotificationStructure(body, "VideoUploadNotify", videoUploadNotifyRules)
}

func validateDeviceUpgradeResultStructure(body []byte) error {
	return validateIndependentNotificationStructure(body, "DeviceUpgradeResult", deviceUpgradeResultRules)
}

func validateSnapshotFinishedStructure(body []byte) error {
	return validateIndependentNotificationStructure(body, "UploadSnapShotFinished", snapshotFinishedRules)
}

func validateMediaStatusStructure(body []byte) error {
	return validateIndependentNotificationStructure(body, "MediaStatus", mediaStatusRules)
}

func validateKeepaliveStructure(body []byte) error {
	return validateIndependentNotificationStructure(body, "Keepalive", keepaliveRules)
}

func validateMobilePositionNotifyStructure(body []byte, version GBProtocolVersion) error {
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
	}
	if version == GBVersion30 {
		itemRules := []notificationElementRule{
			{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
			{name: "CaptureTime", minOccurs: 1, maxOccurs: 1},
			{name: "Longitude", minOccurs: 1, maxOccurs: 1},
			{name: "Latitude", minOccurs: 1, maxOccurs: 1},
			{name: "Speed", minOccurs: 0, maxOccurs: 1},
			{name: "Direction", minOccurs: 0, maxOccurs: 1},
			{name: "Altitude", minOccurs: 0, maxOccurs: 1},
			{name: "Height", minOccurs: 0, maxOccurs: 1},
		}
		rules = append(rules,
			notificationElementRule{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
			notificationElementRule{name: "Time", minOccurs: 1, maxOccurs: 1},
			notificationElementRule{name: "SumNum", minOccurs: 1, maxOccurs: 1},
			notificationElementRule{
				name: "DeviceList", minOccurs: 0, maxOccurs: 1,
				attributes: []notificationAttributeRule{{name: "Num", minOccurs: 0, maxOccurs: 1}},
				children: []notificationElementRule{{
					name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs, children: itemRules,
				}},
			},
		)
	} else {
		// 2016 Schema 不带 DeviceID；兼容已部署设备在 SN 后附带目标编码，目标仍由已认证会话校验。
		rules = append(rules,
			notificationElementRule{name: "DeviceID", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "Time", minOccurs: 1, maxOccurs: 1},
			notificationElementRule{name: "Longitude", minOccurs: 1, maxOccurs: 1},
			notificationElementRule{name: "Latitude", minOccurs: 1, maxOccurs: 1},
			notificationElementRule{name: "Speed", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "Direction", minOccurs: 0, maxOccurs: 1},
			notificationElementRule{name: "Altitude", minOccurs: 0, maxOccurs: 1},
		)
	}
	return validateIndependentNotificationStructure(body, "MobilePosition", rules)
}

func validateAlarmStructure(body []byte, version GBProtocolVersion) error {
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "AlarmPriority", minOccurs: 1, maxOccurs: 1},
		{name: "AlarmMethod", minOccurs: 1, maxOccurs: 1},
		{name: "AlarmTime", minOccurs: 1, maxOccurs: 1},
		// 四版均将报警描述标为可选；2011 Schema 漏写 minOccurs 时继续按注释和既有设备兼容。
		{name: "AlarmDescription", minOccurs: 0, maxOccurs: 1},
		{name: "Longitude", minOccurs: 0, maxOccurs: 1},
		{name: "Latitude", minOccurs: 0, maxOccurs: 1},
	}
	switch version {
	case GBVersion20:
		alarm2016InfoRules := []notificationElementRule{
			{name: "AlarmType", minOccurs: 1, maxOccurs: 1, requireText: true},
			{
				name: "AlarmTypeParam", minOccurs: 0, maxOccurs: 1,
				children: []notificationElementRule{{name: "EventType", minOccurs: 0, maxOccurs: 1, requireText: true}},
			},
		}
		return validateIndependentNotificationStructure(body, "Alarm", append(rules,
			notificationElementRule{
				// 2016 A.2.5 允许一个结构化 Info 后继续携带多项纯文本 Info。
				// 每个元素只能选择结构化子元素或简单文本，二者不能混用。
				name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs,
				children: alarm2016InfoRules, allowSimpleText: true, maxRunes: 1024,
			},
		))
	case GBVersion30:
		return validateIndependentNotificationStructure(body, "Alarm", append(rules,
			notificationElementRule{
				name: "Info", minOccurs: 0, maxOccurs: 1,
				children: alarmTypedInfoRules, knownChildren: []string{"AlarmType", "AlarmTypeParam"},
				allowUnknownTail: true,
			},
			notificationElementRule{name: "ExtraInfo", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024},
		))
	default:
		return validateIndependentNotificationStructure(body, "Alarm", append(rules,
			notificationElementRule{name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024},
		))
	}
}

func validateAlarmBusinessResponseStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Response", "Alarm", broadcastResponseRules)
}

func validateBroadcastResponseStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Response", "Broadcast", broadcastResponseRules)
}

func validateBroadcastNotifyStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Notify", "Broadcast", broadcastNotifyRules)
}

func validateCascadeQueryRequestStructure(body []byte, cmdType string) error {
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
	}
	optional := func(name string) notificationElementRule {
		return notificationElementRule{name: name, minOccurs: 0, maxOccurs: 1}
	}
	switch canonicalGBQueryCmdType(cmdType) {
	case "Alarm":
		rules = append(rules,
			optional("StartAlarmPriority"), optional("EndAlarmPriority"), optional("AlarmMethod"),
			optional("AlarmType"), optional("StartAlarmTime"), optional("EndAlarmTime"),
		)
	case "Catalog":
		rules = append(rules, optional("StartTime"), optional("EndTime"))
	case "RecordInfo":
		rules = append(rules,
			optional("StartTime"), optional("EndTime"), optional("FilePath"), optional("Address"),
			optional("Secrecy"), optional("Type"), optional("RecorderID"), optional("IndistinctQuery"),
			optional("DistinctQuery"), optional("StreamNumber"), optional("AlarmMethod"), optional("AlarmType"),
		)
	case "ConfigDownload":
		rules = append(rules, notificationElementRule{name: "ConfigType", minOccurs: 1, maxOccurs: 1})
	case "MobilePosition":
		rules = append(rules, optional("Interval"))
	case "CruiseTrackQuery":
		rules = append(rules, notificationElementRule{name: "Number", minOccurs: 1, maxOccurs: 1})
	case "DeviceInfo", "DeviceStatus", "PresetQuery", "HomePositionQuery", "CruiseTrackListQuery", "PTZPosition", "SDCardStatus":
	default:
		return fmt.Errorf("unsupported cascade query command: %s", cmdType)
	}
	return validateMANSCDPStructure(body, "Query", canonicalGBQueryCmdType(cmdType), rules)
}

func validateSubscribeEventRequestStructure(body []byte, cmdType string) error {
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "StartAlarmPriority", minOccurs: 0, maxOccurs: 1},
		{name: "EndAlarmPriority", minOccurs: 0, maxOccurs: 1},
		{name: "AlarmMethod", minOccurs: 0, maxOccurs: 1},
		{name: "AlarmType", minOccurs: 0, maxOccurs: 1},
		{name: "StartAlarmTime", minOccurs: 0, maxOccurs: 1},
		{name: "EndAlarmTime", minOccurs: 0, maxOccurs: 1},
		{name: "StartTime", minOccurs: 0, maxOccurs: 1},
		{name: "EndTime", minOccurs: 0, maxOccurs: 1},
		{name: "Interval", minOccurs: 0, maxOccurs: 1},
	}
	return validateMANSCDPStructure(body, "Query", strings.TrimSpace(cmdType), rules)
}

func validateDeviceControlResponseStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Response", "DeviceControl", broadcastResponseRules)
}

func validateDeviceControlRequestStructure(body []byte, version GBProtocolVersion) error {
	dragZoomRules := []notificationElementRule{
		{name: "Length", minOccurs: 1, maxOccurs: 1},
		{name: "Width", minOccurs: 1, maxOccurs: 1},
		{name: "MidPointX", minOccurs: 1, maxOccurs: 1},
		{name: "MidPointY", minOccurs: 1, maxOccurs: 1},
		{name: "LengthX", minOccurs: 1, maxOccurs: 1},
		{name: "LengthY", minOccurs: 1, maxOccurs: 1},
	}
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		{name: "PTZCmd", minOccurs: 0, maxOccurs: 1},
		{
			name: "PTZCmdParams", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "PresetName", minOccurs: 0, maxOccurs: 1},
				{name: "CruiseTrackName", minOccurs: 0, maxOccurs: 1},
			},
		},
		{name: "TeleBoot", minOccurs: 0, maxOccurs: 1},
		{name: "RecordCmd", minOccurs: 0, maxOccurs: 1},
		{name: "StreamNumber", minOccurs: 0, maxOccurs: 1},
		{name: "GuardCmd", minOccurs: 0, maxOccurs: 1},
		{name: "AlarmCmd", minOccurs: 0, maxOccurs: 1},
		{
			name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024, allowSimpleText: true,
			children: []notificationElementRule{
				{name: "ControlPriority", minOccurs: 0, maxOccurs: 1},
				{name: "AlarmMethod", minOccurs: 0, maxOccurs: 1},
				{name: "AlarmType", minOccurs: 0, maxOccurs: 1},
			},
		},
		{name: "IFameCmd", minOccurs: 0, maxOccurs: 1},
		{name: "IFrameCmd", minOccurs: 0, maxOccurs: 1},
		{name: "DragZoomIn", minOccurs: 0, maxOccurs: 1, children: dragZoomRules},
		{name: "DragZoomOut", minOccurs: 0, maxOccurs: 1, children: dragZoomRules},
		{
			name: "HomePosition", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "Enabled", minOccurs: 1, maxOccurs: 1},
				{name: "ResetTime", minOccurs: 0, maxOccurs: 1},
				{name: "PresetIndex", minOccurs: 0, maxOccurs: 1},
			},
		},
		{
			name: "PTZPreciseCtrl", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "Pan", minOccurs: 0, maxOccurs: 1},
				{name: "Tilt", minOccurs: 0, maxOccurs: 1},
				{name: "Zoom", minOccurs: 0, maxOccurs: 1},
			},
		},
		{name: "FormatSDCard", minOccurs: 0, maxOccurs: 1},
		{name: "TargetTrack", minOccurs: 0, maxOccurs: 1},
		{name: "DeviceID2", minOccurs: 0, maxOccurs: 1},
		{name: "TargetArea", minOccurs: 0, maxOccurs: 1, children: dragZoomRules},
		{
			name: "DeviceUpgrade", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "Firmware", minOccurs: 1, maxOccurs: 1},
				{name: "FileURL", minOccurs: 1, maxOccurs: 1},
				{name: "Manufacturer", minOccurs: 1, maxOccurs: 1},
				{name: "SessionID", minOccurs: 1, maxOccurs: 1},
			},
		},
	}
	infoIndex := -1
	for index := range rules {
		if rules[index].name == "Info" {
			infoIndex = index
			break
		}
	}
	if infoIndex >= 0 && version == GBVersion30 {
		rules[infoIndex].maxOccurs = 1
		rules[infoIndex].allowSimpleText = false
	}
	if infoIndex >= 0 && version == GBVersion20 {
		infoRule := rules[infoIndex]
		rules = append(rules[:infoIndex], rules[infoIndex+1:]...)
		homeIndex := -1
		for index := range rules {
			if rules[index].name == "HomePosition" {
				homeIndex = index
				break
			}
		}
		if homeIndex >= 0 {
			rules = append(rules, notificationElementRule{})
			copy(rules[homeIndex+2:], rules[homeIndex+1:])
			rules[homeIndex+1] = infoRule
		}
	}
	if version == GBVersion30 {
		rules = append(rules, notificationElementRule{name: "ExtraInfo", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024})
	}
	if err := validateMANSCDPStructure(body, "Control", "DeviceControl", rules); err != nil {
		return err
	}
	var request deviceControlA23Request
	if err := decodeDeviceControlRequest(body, &request); err != nil {
		return err
	}
	if version == GBVersion30 && len(request.LegacyInfo) > 0 {
		return fmt.Errorf("DeviceControl plain Info is not supported by protocol 3.0")
	}
	return validateDeviceControlExtraInfo(deviceControlTextInfo(&request))
}

func validateDeviceConfigRequestStructure(body []byte, version GBProtocolVersion) error {
	basicParam := notificationElementRule{
		name: "BasicParam", minOccurs: 0, maxOccurs: 1,
		children: []notificationElementRule{
			{name: "Name", minOccurs: 0, maxOccurs: 1},
			{name: "DeviceID", minOccurs: 0, maxOccurs: 1},
			{name: "SIPServerID", minOccurs: 0, maxOccurs: 1},
			{name: "SIPServerIP", minOccurs: 0, maxOccurs: 1},
			{name: "SIPServerPort", minOccurs: 0, maxOccurs: 1},
			{name: "DomainName", minOccurs: 0, maxOccurs: 1},
			{name: "Expiration", minOccurs: 0, maxOccurs: 1},
			{name: "Password", minOccurs: 0, maxOccurs: 1},
			{name: "HeartBeatInterval", minOccurs: 0, maxOccurs: 1},
			{name: "HeartBeatCount", minOccurs: 0, maxOccurs: 1},
		},
	}
	if version == GBVersion11 {
		// 2014 补充文件规定 BasicParam 一旦出现，其全部子字段均为必选。
		for index := range basicParam.children {
			basicParam.children[index].minOccurs = 1
		}
	}
	svacEncode := notificationElementRule{name: "SVACEncodeConfig", minOccurs: 0, maxOccurs: 1, allowNested: true}
	svacDecode := notificationElementRule{name: "SVACDecodeConfig", minOccurs: 0, maxOccurs: 1, allowNested: true}
	rules := []notificationElementRule{
		{name: "CmdType", minOccurs: 1, maxOccurs: 1},
		{name: "SN", minOccurs: 1, maxOccurs: 1},
		{name: "DeviceID", minOccurs: 1, maxOccurs: 1},
		basicParam,
	}
	switch version {
	case GBVersion10, GBVersion11:
		videoItem := notificationElementRule{
			name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs,
			children: []notificationElementRule{
				{name: "StreamName", minOccurs: 1, maxOccurs: 1},
				{name: "VideoFormat", minOccurs: 1, maxOccurs: 1},
				{name: "Resolution", minOccurs: 1, maxOccurs: 1},
				{name: "FrameRate", minOccurs: 1, maxOccurs: 1},
				{name: "BitRateType", minOccurs: 1, maxOccurs: 1},
				{name: "VideoBitRate", minOccurs: 1, maxOccurs: 1},
			},
		}
		audioItem := notificationElementRule{
			name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs,
			children: []notificationElementRule{
				{name: "StreamName", minOccurs: 1, maxOccurs: 1},
				{name: "AudioFormat", minOccurs: 1, maxOccurs: 1},
				{name: "AudioBitRate", minOccurs: 1, maxOccurs: 1},
				{name: "SamplingRate", minOccurs: 1, maxOccurs: 1},
			},
		}
		rules = append(rules,
			notificationElementRule{
				name: "VideoParamConfig", minOccurs: 0, maxOccurs: 1,
				attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
				children:   []notificationElementRule{videoItem},
			},
			notificationElementRule{
				name: "AudioParamConfig", minOccurs: 0, maxOccurs: 1,
				attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
				children:   []notificationElementRule{audioItem},
			},
			svacEncode,
			svacDecode,
		)
	case GBVersion20:
		rules = append(rules, svacEncode, svacDecode)
	case GBVersion30:
		rules = append(rules, deviceConfig2022StructureRules(svacEncode, svacDecode)...)
	default:
		return fmt.Errorf("DeviceConfig has an unsupported protocol version")
	}
	return validateMANSCDPStructure(body, "Control", "DeviceConfig", rules)
}

func deviceConfig2022StructureRules(svacEncode, svacDecode notificationElementRule) []notificationElementRule {
	videoAttributeItem := notificationElementRule{
		name: "Item", minOccurs: 0, maxOccurs: maxNotificationOccurs,
		children: []notificationElementRule{
			{name: "StreamNumber", minOccurs: 1, maxOccurs: 1},
			{name: "VideoFormat", minOccurs: 1, maxOccurs: 1},
			{name: "Resolution", minOccurs: 1, maxOccurs: 1},
			{name: "FrameRate", minOccurs: 1, maxOccurs: 1},
			{name: "BitRateType", minOccurs: 1, maxOccurs: 1},
		},
	}
	timeSegment := notificationElementRule{
		name: "TimeSegment", minOccurs: 0, maxOccurs: 8,
		children: []notificationElementRule{
			{name: "StartHour", minOccurs: 1, maxOccurs: 1},
			{name: "StartMin", minOccurs: 1, maxOccurs: 1},
			{name: "StartSec", minOccurs: 1, maxOccurs: 1},
			{name: "StopHour", minOccurs: 1, maxOccurs: 1},
			{name: "StopMin", minOccurs: 1, maxOccurs: 1},
			{name: "StopSec", minOccurs: 1, maxOccurs: 1},
		},
	}
	recordSchedule := notificationElementRule{
		name: "RecordSchedule", minOccurs: 0, maxOccurs: 7,
		children: []notificationElementRule{
			{name: "WeekDayNum", minOccurs: 1, maxOccurs: 1},
			{name: "TimeSegmentSumNum", minOccurs: 1, maxOccurs: 1},
			timeSegment,
		},
	}
	pictureMaskItem := notificationElementRule{
		name: "Item", minOccurs: 0, maxOccurs: 4,
		children: []notificationElementRule{
			{name: "Seq", minOccurs: 1, maxOccurs: 1},
			{name: "Point", minOccurs: 1, maxOccurs: 1},
		},
	}
	osdItem := notificationElementRule{
		name: "Item", minOccurs: 0, maxOccurs: 8,
		children: []notificationElementRule{
			{name: "Text", minOccurs: 1, maxOccurs: 1},
			{name: "X", minOccurs: 1, maxOccurs: 1},
			{name: "Y", minOccurs: 1, maxOccurs: 1},
		},
	}
	return []notificationElementRule{
		svacEncode,
		svacDecode,
		{
			name: "VideoParamAttribute", minOccurs: 0, maxOccurs: 1,
			attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
			children:   []notificationElementRule{videoAttributeItem},
		},
		{
			name: "VideoRecordPlan", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "RecordEnable", minOccurs: 1, maxOccurs: 1},
				{name: "RecordScheduleSumNum", minOccurs: 1, maxOccurs: 1},
				recordSchedule,
				{name: "StreamNumber", minOccurs: 1, maxOccurs: 1},
			},
		},
		{
			name: "VideoAlarmRecord", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "RecordEnable", minOccurs: 1, maxOccurs: 1},
				{name: "RecordTime", minOccurs: 0, maxOccurs: 1},
				{name: "PreRecordTime", minOccurs: 0, maxOccurs: 1},
				{name: "StreamNumber", minOccurs: 1, maxOccurs: 1},
			},
		},
		{
			name: "PictureMask", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "On", minOccurs: 1, maxOccurs: 1},
				{name: "SumNum", minOccurs: 1, maxOccurs: 1},
				{
					name: "RegionList", minOccurs: 0, maxOccurs: 1,
					attributes: []notificationAttributeRule{{name: "Num", minOccurs: 1, maxOccurs: 1}},
					children:   []notificationElementRule{pictureMaskItem},
				},
			},
		},
		{name: "FrameMirror", minOccurs: 0, maxOccurs: 1},
		{
			name: "AlarmReport", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "MotionDetection", minOccurs: 1, maxOccurs: 1},
				{name: "FieldDetection", minOccurs: 1, maxOccurs: 1},
			},
		},
		{
			name: "OSDConfig", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "Length", minOccurs: 1, maxOccurs: 1},
				{name: "Width", minOccurs: 1, maxOccurs: 1},
				{name: "TimeX", minOccurs: 1, maxOccurs: 1},
				{name: "TimeY", minOccurs: 1, maxOccurs: 1},
				{name: "TimeEnable", minOccurs: 0, maxOccurs: 1},
				{name: "TimeType", minOccurs: 0, maxOccurs: 1},
				{name: "TextEnable", minOccurs: 0, maxOccurs: 1},
				{name: "SumNum", minOccurs: 1, maxOccurs: 1},
				osdItem,
			},
		},
		{
			name: "SnapShotConfig", minOccurs: 0, maxOccurs: 1,
			children: []notificationElementRule{
				{name: "SnapNum", minOccurs: 1, maxOccurs: 1},
				{name: "Interval", minOccurs: 1, maxOccurs: 1},
				{name: "UploadURL", minOccurs: 1, maxOccurs: 1},
				{name: "SessionID", minOccurs: 1, maxOccurs: 1},
			},
		},
		{name: "ExtraInfo", minOccurs: 0, maxOccurs: maxNotificationOccurs, maxRunes: 1024},
	}
}

func validateDeviceConfigResponseStructure(body []byte, version GBProtocolVersion) error {
	rules := append([]notificationElementRule(nil), broadcastResponseRules...)
	if version.AtLeast(GBVersion11) {
		// 保留已发布的厂商应答标记兼容契约，但仍限制为有序单例简单文本。
		rules = append(rules, notificationElementRule{name: "VendorResult", minOccurs: 0, maxOccurs: 1})
	}
	if version == GBVersion30 {
		// 2022 附录 A.4 扩展对象由后续专用校验器验证具体结构。
		rules = append(rules, notificationElementRule{
			name: "Info", minOccurs: 0, maxOccurs: maxNotificationOccurs, allowNested: true,
		})
	}
	return validateMANSCDPStructure(body, "Response", "DeviceConfig", rules)
}

func validateDeviceInfoResponseStructure(body []byte, version GBProtocolVersion) error {
	rules := deviceInfo2011Rules
	if version == GBVersion30 {
		rules = deviceInfo2022Rules
	} else if version.AtLeast(GBVersion11) {
		rules = deviceInfo2014Rules
	}
	return validateMANSCDPStructure(body, "Response", "DeviceInfo", rules)
}

func validateDeviceStatusResponseStructure(body []byte, version GBProtocolVersion) error {
	return validateMANSCDPStructure(body, "Response", "DeviceStatus", deviceStatusResponseRules(version))
}

func validatePresetQueryResponseStructure(body []byte, version GBProtocolVersion) error {
	return validateMANSCDPStructure(body, "Response", "PresetQuery", presetQueryResponseRules(version))
}

func validateRecordInfoResponseStructure(body []byte, version GBProtocolVersion) error {
	return validateMANSCDPStructure(body, "Response", "RecordInfo", recordInfoResponseRules(version))
}

func validateCatalogStructure(body []byte, version GBProtocolVersion, notification bool) error {
	rootName := "Response"
	if notification && version.AtLeast(GBVersion11) {
		rootName = "Notify"
	}
	return validateMANSCDPStructure(body, rootName, "Catalog", catalogEnvelopeRules(version, notification))
}

func validateHomePositionQueryResponseStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Response", "HomePositionQuery", homePositionQueryResponseRules)
}

func validateCruiseTrackListQueryResponseStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Response", "CruiseTrackListQuery", cruiseTrackListQueryResponseRules)
}

func validateCruiseTrackQueryResponseStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Response", "CruiseTrackQuery", cruiseTrackQueryResponseRules)
}

func validatePTZPositionQueryStructure(body []byte) error {
	if err := validateMANSCDPStructure(body, "Response", "PTZPosition", ptzPositionQueryResponseRules); err == nil {
		return nil
	}
	return validateMANSCDPStructure(body, "Notify", "PTZPosition", ptzPositionQueryResponseRules)
}

func validateSDCardStatusQueryResponseStructure(body []byte) error {
	return validateMANSCDPStructure(body, "Response", "SDCardStatus", sdCardStatusQueryResponseRules)
}

func validateIndependentNotificationStructure(body []byte, cmdType string, children []notificationElementRule) error {
	return validateMANSCDPStructure(body, "Notify", cmdType, children)
}

func validateMANSCDPStructure(body []byte, rootName, cmdType string, children []notificationElementRule) error {
	decoder := sip.NewGBXMLDecoder(body)
	seenRoot := false
	seenXMLDeclaration := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			if !seenRoot {
				return fmt.Errorf("%s XML has no %s root", cmdType, rootName)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s XML is invalid: %w", cmdType, err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			if seenRoot {
				return fmt.Errorf("%s XML must contain exactly one root element", cmdType)
			}
			seenRoot = true
			if current.Name.Local != rootName {
				return fmt.Errorf("%s root must be %s", cmdType, rootName)
			}
			if err := validateNotificationElement(decoder, current, notificationElementRule{name: rootName, minOccurs: 1, maxOccurs: 1, children: children}, cmdType); err != nil {
				return err
			}
		case xml.CharData:
			if strings.TrimSpace(string(current)) != "" {
				return fmt.Errorf("%s XML contains text outside the root element", cmdType)
			}
		case xml.ProcInst:
			if seenRoot || seenXMLDeclaration || !strings.EqualFold(current.Target, "xml") {
				return fmt.Errorf("%s XML contains an unsupported instruction", cmdType)
			}
			seenXMLDeclaration = true
		case xml.Directive:
			return fmt.Errorf("%s XML contains an unsupported directive", cmdType)
		}
	}
}

func validateNotificationElement(decoder *xml.Decoder, start xml.StartElement, rule notificationElementRule, cmdType string) error {
	if start.Name.Space != "" {
		return fmt.Errorf("%s element %s must not contain namespaces", cmdType, start.Name.Local)
	}
	if err := validateNotificationAttributes(start, rule, cmdType); err != nil {
		return err
	}
	counts := make([]int, len(rule.children))
	ruleIndex := 0
	textRunes := 0
	hasChild := false
	hasText := false
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("%s element %s is not closed", cmdType, start.Name.Local)
			}
			return fmt.Errorf("%s XML is invalid: %w", cmdType, err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			if rule.allowSimpleText && hasText {
				return fmt.Errorf("%s element %s must not mix text and child elements", cmdType, start.Name.Local)
			}
			hasChild = true
			if len(rule.children) == 0 {
				if !rule.allowNested {
					return fmt.Errorf("%s element %s must contain simple text", cmdType, start.Name.Local)
				}
				if err := validateNotificationArbitraryElement(decoder, current, cmdType); err != nil {
					return err
				}
				continue
			}
			if rule.allowKnownAnyOrder {
				matched := false
				for index, childRule := range rule.children {
					if current.Name.Local != childRule.name {
						continue
					}
					matched = true
					counts[index]++
					if counts[index] > childRule.maxOccurs {
						return fmt.Errorf("%s contains too many %s elements", cmdType, current.Name.Local)
					}
					if err := validateNotificationElement(decoder, current, childRule, cmdType); err != nil {
						return err
					}
					break
				}
				if matched {
					continue
				}
				if rule.allowUnknownTail && !stringInSlice(current.Name.Local, rule.knownChildren) {
					if err := validateNotificationArbitraryElement(decoder, current, cmdType); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("%s element %s is not allowed", cmdType, current.Name.Local)
			}
			for ruleIndex < len(rule.children) && current.Name.Local != rule.children[ruleIndex].name {
				if counts[ruleIndex] < rule.children[ruleIndex].minOccurs {
					return fmt.Errorf("%s requires %s before %s", cmdType, rule.children[ruleIndex].name, current.Name.Local)
				}
				ruleIndex++
			}
			if ruleIndex == len(rule.children) {
				if rule.allowUnknownTail && !stringInSlice(current.Name.Local, rule.knownChildren) {
					if err := validateNotificationArbitraryElement(decoder, current, cmdType); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("%s element %s is not allowed or is out of order", cmdType, current.Name.Local)
			}
			childRule := rule.children[ruleIndex]
			counts[ruleIndex]++
			if counts[ruleIndex] > childRule.maxOccurs {
				return fmt.Errorf("%s contains too many %s elements", cmdType, current.Name.Local)
			}
			if err := validateNotificationElement(decoder, current, childRule, cmdType); err != nil {
				return err
			}
		case xml.EndElement:
			if current.Name != start.Name {
				return fmt.Errorf("%s element %s has an unexpected closing element", cmdType, start.Name.Local)
			}
			// allowSimpleText 表示该元素是“简单文本”或“结构化子元素”的二选一。
			// 选择简单文本（包括合法空字符串）时不再要求结构化分支的必选子元素。
			if !(rule.allowSimpleText && !hasChild) {
				for index, childRule := range rule.children {
					if counts[index] < childRule.minOccurs {
						return fmt.Errorf("%s requires exactly one %s element", cmdType, childRule.name)
					}
				}
			}
			if rule.requireText && !hasText {
				return fmt.Errorf("%s element %s requires non-empty text", cmdType, start.Name.Local)
			}
			return nil
		case xml.CharData:
			if strings.TrimSpace(string(current)) != "" {
				hasText = true
			}
			if len(rule.children) != 0 && hasText {
				if !rule.allowSimpleText {
					return fmt.Errorf("%s element %s must not contain text", cmdType, start.Name.Local)
				}
				if hasChild {
					return fmt.Errorf("%s element %s must not mix text and child elements", cmdType, start.Name.Local)
				}
			}
			if len(rule.children) == 0 && rule.maxRunes > 0 {
				textRunes += utf8.RuneCount(current)
				if textRunes > rule.maxRunes {
					return fmt.Errorf("%s element %s exceeds %d characters", cmdType, start.Name.Local, rule.maxRunes)
				}
			}
		case xml.ProcInst, xml.Directive:
			return fmt.Errorf("%s element %s contains an unsupported XML instruction", cmdType, start.Name.Local)
		}
	}
}

func stringInSlice(value string, values []string) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func validateNotificationAttributes(start xml.StartElement, rule notificationElementRule, cmdType string) error {
	counts := make([]int, len(rule.attributes))
	for _, attribute := range start.Attr {
		if attribute.Name.Space != "" {
			return fmt.Errorf("%s element %s must not contain namespaced attributes", cmdType, start.Name.Local)
		}
		matched := false
		for index, attributeRule := range rule.attributes {
			if attribute.Name.Local != attributeRule.name {
				continue
			}
			matched = true
			counts[index]++
			if counts[index] > attributeRule.maxOccurs {
				return fmt.Errorf("%s element %s contains too many %s attributes", cmdType, start.Name.Local, attribute.Name.Local)
			}
			break
		}
		if !matched {
			return fmt.Errorf("%s element %s contains unsupported attribute %s", cmdType, start.Name.Local, attribute.Name.Local)
		}
	}
	for index, attributeRule := range rule.attributes {
		if counts[index] < attributeRule.minOccurs {
			return fmt.Errorf("%s element %s requires attribute %s", cmdType, start.Name.Local, attributeRule.name)
		}
	}
	return nil
}

func validateNotificationArbitraryElement(decoder *xml.Decoder, start xml.StartElement, cmdType string) error {
	if len(start.Attr) != 0 || start.Name.Space != "" {
		return fmt.Errorf("%s element %s must not contain attributes or namespaces", cmdType, start.Name.Local)
	}
	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("%s element %s is not closed", cmdType, start.Name.Local)
			}
			return fmt.Errorf("%s XML is invalid: %w", cmdType, err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			if err := validateNotificationArbitraryElement(decoder, current, cmdType); err != nil {
				return err
			}
		case xml.EndElement:
			if current.Name != start.Name {
				return fmt.Errorf("%s element %s has an unexpected closing element", cmdType, start.Name.Local)
			}
			return nil
		case xml.ProcInst, xml.Directive:
			return fmt.Errorf("%s element %s contains an unsupported XML instruction", cmdType, start.Name.Local)
		}
	}
}
