package gbs

import (
	"fmt"
	"math"
)

// 只在设备接收入口兼容字段顺序和扩展位置；不改变必选项、次数、值域及对外标准校验。
func validateDeviceMANSCDPStructure(body []byte, version GBProtocolVersion, root, cmd string, rules []notificationElementRule) error {
	if version != GBVersion20 && version != GBVersion30 {
		return validateMANSCDPStructure(body, root, cmd, rules)
	}
	rule := notificationElementRule{name: root, children: rules}
	return validateMANSCDPStructureRule(body, cmd, deviceReceiveRule(rule, deviceReceiveFieldNames(rule)))
}

func deviceReceiveFieldNames(rule notificationElementRule) []string {
	names := []string{rule.name}
	for _, child := range rule.children {
		names = append(names, deviceReceiveFieldNames(child)...)
	}
	return names
}

func deviceReceiveRule(rule notificationElementRule, known []string) notificationElementRule {
	// 克隆规则树，避免污染全局切片和并发使用的严格发送规则。
	rule.children = append([]notificationElementRule(nil), rule.children...)
	if len(rule.children) > 0 {
		rule.allowKnownAnyOrder = true
	}
	if rule.allowUnknownTail {
		// 标准字段错放到其他层级不能伪装为厂商扩展。
		rule.knownChildren = append(append([]string(nil), rule.knownChildren...), known...)
	}
	for i := range rule.children {
		rule.children[i] = deviceReceiveRule(rule.children[i], known)
	}
	return rule
}

// 声明 2022、但仍上报旧版字段的固件不降级协议版本。
func validateDeviceCatalogStructure(body []byte, version GBProtocolVersion, notification bool) error {
	if version != GBVersion30 && version != GBVersion20 {
		return validateCatalogStructure(body, version, notification)
	}

	rules := catalogEnvelopeRules(version, notification)
	for index := range rules {
		if rules[index].name != "DeviceList" {
			continue
		}
		item := &rules[index].children[0]
		if version == GBVersion30 {
			for _, name := range []string{"Owner", "SafetyWay", "CertNum", "Certifiable", "ErrCode", "EndTime"} {
				item.children = append(item.children, notificationElementRule{name: name, maxOccurs: 1})
			}
		}
		for child := range item.children {
			info := &item.children[child]
			if info.name == "Info" && version == GBVersion30 {
				for _, name := range []string{"PositionType", "UseType"} {
					info.children = append(info.children, notificationElementRule{name: name, maxOccurs: 1})
				}
				info.allowKnownAnyOrder = true
			}
		}
	}
	root := "Response"
	if notification {
		root = "Notify"
	}
	if version == GBVersion30 {
		rules = append(rules, notificationElementRule{name: "Info", maxOccurs: maxNotificationOccurs, maxRunes: 1024})
	}
	return validateDeviceMANSCDPStructure(body, version, root, "Catalog", rules)
}

func validateDeviceCatalogItemValues(item Channels, version GBProtocolVersion) error {
	return validateCatalogItemValuesWithCompatibility(item, version, version == GBVersion30)
}

func validateDeviceInfoStructure(body []byte, version GBProtocolVersion) error {
	if version != GBVersion30 && version != GBVersion20 {
		return validateDeviceInfoResponseStructure(body, version)
	}
	rules := deviceInfo2014Rules
	if version == GBVersion30 {
		rules = append([]notificationElementRule(nil), deviceInfo2022Rules...)
		for _, name := range []string{"DeviceType", "MaxCamera", "MaxAlarm"} {
			rules = append(rules, notificationElementRule{name: name, maxOccurs: 1})
		}
	}
	return validateDeviceMANSCDPStructure(body, version, "Response", "DeviceInfo", rules)
}

func validateDeviceVersionedInfo(version GBProtocolVersion, cmd string, info []versionedInfoXML, extra []string) error {
	if version != GBVersion30 {
		return validateVersionedInfo(version, cmd, info, extra)
	}
	for _, value := range info {
		if len(value.Children) == 0 {
			if err := validateVersionedInfo(GBVersion20, cmd, []versionedInfoXML{value}, nil); err != nil {
				return err
			}
		} else if err := validateVersionedInfo(version, cmd, []versionedInfoXML{value}, nil); err != nil {
			return err
		}
	}
	return validateVersionedInfo(version, cmd, nil, extra)
}

func validateResponseInfo(version GBProtocolVersion, cmd string, info []versionedInfoXML, extra []string, compatible bool) error {
	if compatible {
		return validateDeviceVersionedInfo(version, cmd, info, extra)
	}
	return validateVersionedInfo(version, cmd, info, extra)
}

func deviceStatusReceiveRules(version GBProtocolVersion, compatible bool) []notificationElementRule {
	rules := deviceStatusResponseRules(version)
	if !compatible {
		return rules
	}
	for i := range rules {
		if rules[i].name == "Alarmstatus" {
			// Num/num 在各版示例和厂商固件中混用；业务层仍拒绝同时出现及数量不符。
			rules[i].attributes = []notificationAttributeRule{{name: "Num", maxOccurs: 1}, {name: "num", maxOccurs: 1}}
		}
	}
	return rules
}

func deviceRecordInfoRules(version GBProtocolVersion) []notificationElementRule {
	rules := recordInfoResponseRules(version)
	if version == GBVersion30 {
		rules = append(rules, notificationElementRule{name: "Info", maxOccurs: maxNotificationOccurs, maxRunes: 1024})
	}
	return rules
}

func validateDeviceBasicParamResponse(param *BasicParam, version GBProtocolVersion) error {
	if param == nil || version != GBVersion30 {
		return validateBasicParamResponse(param, version)
	}
	if param.present.PositionCapability && (param.PositionCapability < 0 || param.PositionCapability > 2) {
		return fmt.Errorf("ConfigDownload BasicParam PositionCapability must be 0, 1 or 2")
	}
	if param.present.Longitude && (math.IsNaN(param.Longitude) || math.IsInf(param.Longitude, 0) || param.Longitude < -180 || param.Longitude > 180) ||
		param.present.Latitude && (math.IsNaN(param.Latitude) || math.IsInf(param.Latitude, 0) || param.Latitude < -90 || param.Latitude > 90) {
		return fmt.Errorf("ConfigDownload BasicParam positioning coordinates are invalid")
	}
	// 只放行旧版定位字段；注册身份、口令等其他版本字段继续由标准规则拒绝。
	value := *param
	value.present.PositionCapability, value.present.Longitude, value.present.Latitude = false, false, false
	return validateBasicParamResponse(&value, version)
}
