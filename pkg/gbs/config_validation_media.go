package gbs

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

var configDownloadResponseSingletons = map[string]struct{}{
	"CmdType": {}, "SN": {}, "DeviceID": {}, "Result": {}, "BasicParam": {},
	"VideoParamOpt": {}, "VideoParamConfig": {}, "AudioParamOpt": {}, "AudioParamConfig": {},
	"SVACEncodeConfig": {}, "SVACDecodeConfig": {}, "VideoParamAttribute": {},
	"VideoRecordPlan": {}, "VideoAlarmRecord": {}, "PictureMask": {}, "FrameMirror": {},
	"AlarmReport": {}, "OSDConfig": {}, "SnapShot": {}, "SnapShotConfig": {},
}

var configDownloadResponseOrder = map[string]int{
	"CmdType": 0, "SN": 1, "DeviceID": 2, "Result": 3, "BasicParam": 4,
	"VideoParamOpt": 5, "VideoParamConfig": 6, "AudioParamOpt": 7, "AudioParamConfig": 8,
	"SVACEncodeConfig": 9, "SVACDecodeConfig": 10, "VideoParamAttribute": 11,
	"VideoRecordPlan": 12, "VideoAlarmRecord": 13, "PictureMask": 14, "FrameMirror": 15,
	"AlarmReport": 16, "OSDConfig": 17, "SnapShot": 18, "SnapShotConfig": 18,
}

var configDownloadSimpleResponseElements = map[string]struct{}{
	"CmdType": {}, "SN": {}, "DeviceID": {}, "Result": {}, "ExtraInfo": {}, "ExtralInfo": {},
}

var basicParamResponseFields = map[string]struct{}{
	"Name": {}, "DeviceID": {}, "SIPServerID": {}, "SIPServerIP": {}, "SIPServerPort": {},
	"DomainName": {}, "Expiration": {}, "Password": {}, "HeartBeatInterval": {}, "HeartBeatCount": {},
	"PositionCapability": {}, "Longitude": {}, "Latitude": {},
}

type videoParamOptResponse struct {
	VideoFormatOpt   *string `xml:"VideoFormatOpt"`
	ResolutionOpt    *string `xml:"ResolutionOpt"`
	FrameRateOpt     *string `xml:"FrameRateOpt"`
	BitRateTypeOpt   *string `xml:"BitRateTypeOpt"`
	VideoBitRateOpt  *string `xml:"VideoBitRateOpt"`
	DownloadSpeedOpt *string `xml:"DownloadSpeedOpt"`
	DownloadSpeed    *string `xml:"DownloadSpeed"`
	Resolution       *string `xml:"Resolution"`
}

type videoParamConfigResponse struct {
	Items []videoParamConfigItem `xml:"Item"`
}

type videoParamConfigItem struct {
	StreamName   *string `xml:"StreamName"`
	VideoFormat  *string `xml:"VideoFormat"`
	Resolution   *string `xml:"Resolution"`
	FrameRate    *string `xml:"FrameRate"`
	BitRateType  *string `xml:"BitRateType"`
	VideoBitRate *string `xml:"VideoBitRate"`
}

type audioParamOptResponse struct {
	AudioFormatOpt  *string `xml:"AudioFormatOpt"`
	AudioBitRateOpt *string `xml:"AudioBitRateOpt"`
	SamplingRateOpt *string `xml:"SamplingRateOpt"`
}

type audioParamConfigResponse struct {
	Items []audioParamConfigItem `xml:"Item"`
}

type audioParamConfigItem struct {
	StreamName   *string `xml:"StreamName"`
	AudioFormat  *string `xml:"AudioFormat"`
	AudioBitRate *string `xml:"AudioBitRate"`
	SamplingRate *string `xml:"SamplingRate"`
}

func validateConfigDownloadResponseSingletons(body []byte) error {
	decoder := sip.NewGBXMLDecoder(body)
	depth := 0
	seenRoot := false
	seenXMLDeclaration := false
	lastOrder := -1
	simpleElementDepth := 0
	counts := make(map[string]int, len(configDownloadResponseSingletons))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("ConfigDownload response XML is invalid: %w", err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				if seenRoot {
					return fmt.Errorf("ConfigDownload response XML must contain exactly one root element")
				}
				seenRoot = true
				if current.Name.Local != "Response" {
					return fmt.Errorf("ConfigDownload response root must be Response")
				}
				if current.Name.Space != "" || len(current.Attr) != 0 {
					return fmt.Errorf("ConfigDownload response root must not contain attributes or namespaces")
				}
				continue
			}
			if simpleElementDepth != 0 {
				return fmt.Errorf("ConfigDownload response element %s must contain simple text", current.Name.Local)
			}
			if depth != 2 {
				continue
			}
			name := current.Name.Local
			if err := validateConfigDownloadTopLevelAttributes(current, name); err != nil {
				return err
			}
			if _, singleton := configDownloadResponseSingletons[name]; !singleton {
				if name == "Info" || name == "ExtraInfo" || name == "ExtralInfo" || isAppendixA4Type(name) {
					lastOrder = 19
					if _, simple := configDownloadSimpleResponseElements[name]; simple {
						simpleElementDepth = depth
					}
					continue
				}
				return fmt.Errorf("ConfigDownload response contains unsupported %s element", name)
			}
			order := configDownloadResponseOrder[name]
			if order < lastOrder {
				return fmt.Errorf("ConfigDownload response contains out-of-order %s element", name)
			}
			lastOrder = order
			counts[name]++
			if counts[name] > 1 {
				return fmt.Errorf("ConfigDownload response contains duplicate %s elements", name)
			}
			if _, simple := configDownloadSimpleResponseElements[name]; simple {
				simpleElementDepth = depth
			}
		case xml.EndElement:
			if depth == simpleElementDepth {
				simpleElementDepth = 0
			}
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(current)) != "" {
				return fmt.Errorf("ConfigDownload response XML contains text outside the root element")
			}
		case xml.ProcInst:
			if seenRoot || seenXMLDeclaration || !strings.EqualFold(current.Target, "xml") {
				return fmt.Errorf("ConfigDownload response contains an unsupported XML instruction")
			}
			seenXMLDeclaration = true
		case xml.Directive:
			return fmt.Errorf("ConfigDownload response contains an unsupported XML instruction")
		}
	}
	if !seenRoot || depth != 0 {
		return fmt.Errorf("ConfigDownload response XML is incomplete")
	}
	if counts["SnapShot"] > 0 && counts["SnapShotConfig"] > 0 {
		return fmt.Errorf("ConfigDownload response must not contain both SnapShot and SnapShotConfig")
	}
	return nil
}

func validateConfigDownloadResponseForVersion(body []byte, version GBProtocolVersion) error {
	if version == GBVersion10 || !version.Valid() {
		return fmt.Errorf("ConfigDownload is not supported by %s", version.StandardName())
	}
	if err := validateConfigDownloadResponseSingletons(body); err != nil {
		return err
	}
	if err := validateBasicParamResponseStructure(body, version); err != nil {
		return err
	}
	if err := validateConfigDownloadResponseExtensions(body, version); err != nil {
		return err
	}

	var response ConfigDownloadResponse
	if err := sip.XMLDecode(body, &response); err != nil {
		return ErrXMLDecode
	}
	response.CmdType = strings.TrimSpace(response.CmdType)
	response.DeviceID = strings.TrimSpace(response.DeviceID)
	response.Result = strings.TrimSpace(response.Result)
	if response.XMLName.Local != "Response" || !strings.EqualFold(response.CmdType, CMDTypeConfigDownload) ||
		response.SN <= 0 || !isGBDeviceIdentifier(response.DeviceID) || !isGBResultValue(response.Result) {
		return fmt.Errorf("invalid ConfigDownload response envelope")
	}

	sections := []struct {
		name    string
		present bool
	}{
		{"BasicParam", response.BasicParam != nil},
		{"VideoParamOpt", response.VideoParamOpt != nil},
		{"VideoParamConfig", response.VideoParamConfig != nil},
		{"AudioParamOpt", response.AudioParamOpt != nil},
		{"AudioParamConfig", response.AudioParamConfig != nil},
		{"SVACEncodeConfig", response.SVACEncodeConfig != nil},
		{"SVACDecodeConfig", response.SVACDecodeConfig != nil},
		{"VideoParamAttribute", response.VideoParamAttribute != nil},
		{"VideoRecordPlan", response.VideoRecordPlan != nil},
		{"VideoAlarmRecord", response.VideoAlarmRecord != nil},
		{"PictureMask", response.PictureMask != nil},
		{"FrameMirror", response.FrameMirror != nil},
		{"AlarmReport", response.AlarmReport != nil},
		{"OSDConfig", response.OSDConfig != nil},
		{"SnapShotConfig", response.SnapShot != nil || response.SnapShotConfig != nil},
	}
	sectionCount := 0
	for _, section := range sections {
		if !section.present {
			continue
		}
		sectionCount++
		if !configTypeSupported(version, section.name) {
			return fmt.Errorf("ConfigDownload %s is not supported by %s", section.name, version.StandardName())
		}
	}
	resultOK := strings.EqualFold(response.Result, "OK")
	if sectionCount > 1 || resultOK && sectionCount != 1 {
		return fmt.Errorf("ConfigDownload response must contain exactly one configuration section")
	}
	if !resultOK {
		return nil
	}
	if version == GBVersion30 && response.SnapShotConfig != nil {
		return fmt.Errorf("ConfigDownload snapshot response must use SnapShot for %s", version.StandardName())
	}
	if err := validateBasicParamResponse(response.BasicParam, version); err != nil {
		return err
	}
	if err := validateConfigDownloadMediaResponse(version, &response); err != nil {
		return err
	}
	for _, section := range []struct {
		name    string
		value   string
		present bool
	}{
		{"SVACEncodeConfig", svacEncodeXML(response.SVACEncodeConfig), response.SVACEncodeConfig != nil},
		{"SVACDecodeConfig", svacDecodeXML(response.SVACDecodeConfig), response.SVACDecodeConfig != nil},
	} {
		if !section.present {
			continue
		}
		if err := validateSVACConfigResponse(version, section.name, section.value); err != nil {
			return err
		}
	}
	for _, section := range []struct {
		name    string
		value   string
		present bool
	}{
		{"VideoParamAttribute", innerXML(response.VideoParamAttribute), response.VideoParamAttribute != nil},
		{"VideoRecordPlan", innerXML(response.VideoRecordPlan), response.VideoRecordPlan != nil},
		{"VideoAlarmRecord", innerXML(response.VideoAlarmRecord), response.VideoAlarmRecord != nil},
		{"PictureMask", innerXML(response.PictureMask), response.PictureMask != nil},
		{"FrameMirror", innerXML(response.FrameMirror), response.FrameMirror != nil},
		{"AlarmReport", innerXML(response.AlarmReport), response.AlarmReport != nil},
		{"OSDConfig", innerXML(response.OSDConfig), response.OSDConfig != nil},
	} {
		if !section.present {
			continue
		}
		if version != GBVersion30 {
			return fmt.Errorf("ConfigDownload %s is not supported by %s", section.name, version.StandardName())
		}
		if err := validateDeviceConfigXMLFragment(section.name, section.value); err != nil {
			return err
		}
		if section.name == "VideoParamAttribute" {
			if err := validateVideoParamAttribute30(response.VideoParamAttribute, true); err != nil {
				return err
			}
			continue
		}
		if err := validateDeviceConfig30Fragment(section.name, section.value); err != nil {
			return err
		}
	}
	if response.SnapShot != nil {
		return validateSnapshotConfig(response.SnapShot)
	}
	return nil
}

func basicParamResponseFieldOrder(version GBProtocolVersion, name string) (int, bool) {
	var fields []string
	switch version {
	case GBVersion11:
		fields = []string{"Name", "DeviceID", "SIPServerID", "SIPServerIP", "SIPServerPort", "DomainName", "Expiration", "Password", "HeartBeatInterval", "HeartBeatCount"}
	case GBVersion20:
		fields = []string{"Name", "Expiration", "HeartBeatInterval", "HeartBeatCount", "PositionCapability", "Longitude", "Latitude"}
	case GBVersion30:
		fields = []string{"Name", "Expiration", "HeartBeatInterval", "HeartBeatCount"}
	default:
		return 0, false
	}
	for index, field := range fields {
		if field == name {
			return index, true
		}
	}
	return 0, false
}

func basicParamResponseRequiredFields(version GBProtocolVersion) []string {
	switch version {
	case GBVersion11:
		return []string{"Name", "DeviceID", "SIPServerID", "SIPServerIP", "SIPServerPort", "DomainName", "Expiration", "Password", "HeartBeatInterval", "HeartBeatCount"}
	case GBVersion20:
		return []string{"Name", "Expiration", "HeartBeatInterval", "HeartBeatCount"}
	default:
		return nil
	}
}

func basicParamFieldSupported(version GBProtocolVersion, name string) bool {
	_, ok := basicParamResponseFieldOrder(version, name)
	return ok
}

func validateBasicParamResponseStructure(body []byte, version GBProtocolVersion) error {
	decoder := sip.NewGBXMLDecoder(body)
	depth := 0
	basicDepth := 0
	childDepth := 0
	basicSeen := false
	lastOrder := -1
	seen := make(map[string]int, len(basicParamResponseFields))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("ConfigDownload BasicParam XML is invalid: %w", err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && current.Name.Local == "BasicParam" {
				if current.Name.Space != "" || len(current.Attr) != 0 {
					return fmt.Errorf("ConfigDownload BasicParam must not contain attributes or namespaces")
				}
				basicDepth = depth
				basicSeen = true
				continue
			}
			if basicDepth == 0 {
				continue
			}
			if depth != basicDepth+1 || childDepth != 0 {
				return fmt.Errorf("ConfigDownload BasicParam field %s must contain simple text", current.Name.Local)
			}
			name := current.Name.Local
			if current.Name.Space != "" || len(current.Attr) != 0 {
				return fmt.Errorf("ConfigDownload BasicParam field %s must not contain attributes or namespaces", name)
			}
			order, supported := basicParamResponseFieldOrder(version, name)
			if !supported {
				return fmt.Errorf("ConfigDownload BasicParam field %s is not supported by %s", name, version.StandardName())
			}
			if order < lastOrder {
				return fmt.Errorf("ConfigDownload BasicParam contains out-of-order %s field", name)
			}
			lastOrder = order
			seen[name]++
			if seen[name] > 1 {
				return fmt.Errorf("ConfigDownload BasicParam contains duplicate %s fields", name)
			}
			childDepth = depth
		case xml.EndElement:
			if depth == childDepth {
				childDepth = 0
			}
			if depth == basicDepth {
				basicDepth = 0
			}
			depth--
		case xml.CharData:
			if basicDepth > 0 && depth == basicDepth && strings.TrimSpace(string(current)) != "" {
				return fmt.Errorf("ConfigDownload BasicParam contains text outside a field")
			}
		case xml.Directive, xml.ProcInst:
			if basicDepth > 0 {
				return fmt.Errorf("ConfigDownload BasicParam contains an unsupported XML instruction")
			}
		}
	}
	if !basicSeen {
		return nil
	}
	for _, field := range basicParamResponseRequiredFields(version) {
		if seen[field] == 0 {
			return fmt.Errorf("ConfigDownload BasicParam is incomplete for %s", version.StandardName())
		}
	}
	return nil
}

func validateConfigDownloadResponseExtensions(body []byte, version GBProtocolVersion) error {
	decoder := sip.NewGBXMLDecoder(body)
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("ConfigDownload extension XML is invalid: %w", err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			depth++
			name := current.Name.Local
			if depth != 2 || (name != "Info" && name != "ExtraInfo" && name != "ExtralInfo" && !isAppendixA4Type(name)) {
				continue
			}
			var node a4XMLNode
			if err := decoder.DecodeElement(&node, &current); err != nil {
				return fmt.Errorf("decode ConfigDownload extension %s: %w", name, err)
			}
			depth--
			if err := validateConfigDownloadResponseExtensionNode(node, version); err != nil {
				return err
			}
		case xml.EndElement:
			depth--
		}
	}
}

func validateConfigDownloadResponseExtensionNode(node a4XMLNode, version GBProtocolVersion) error {
	name := node.XMLName.Local
	if name == "Info" {
		if len(node.Attrs) != 0 {
			return fmt.Errorf("ConfigDownload Info must not contain attributes")
		}
		if version == GBVersion30 {
			if len(node.Children) == 0 {
				return fmt.Errorf("ConfigDownload legacy Info is not supported by %s", version.StandardName())
			}
			for _, child := range node.Children {
				if !isAppendixA4Type(child.XMLName.Local) && child.XMLName.Local != "ExtraInfo" {
					return fmt.Errorf("ConfigDownload Info contains unsupported Appendix A.4 object %s", child.XMLName.Local)
				}
			}
			return nil
		}
		if len(node.Children) != 0 || utf8.RuneCountInString(node.Content) > 1024 {
			return fmt.Errorf("ConfigDownload Info must contain at most 1024 characters of simple text")
		}
		return nil
	}
	if name == "ExtraInfo" || name == "ExtralInfo" {
		if version != GBVersion30 {
			return fmt.Errorf("ConfigDownload %s is not supported by %s", name, version.StandardName())
		}
		if name != "ExtraInfo" {
			return fmt.Errorf("ConfigDownload extension must use ExtraInfo for %s", version.StandardName())
		}
		if len(node.Attrs) != 0 || len(node.Children) != 0 || utf8.RuneCountInString(node.Content) > 1024 {
			return fmt.Errorf("ConfigDownload ExtraInfo must contain at most 1024 characters of simple text")
		}
		return nil
	}
	if isAppendixA4Type(name) && version != GBVersion30 {
		return fmt.Errorf("ConfigDownload Appendix A.4 object %s is not supported by %s", name, version.StandardName())
	}
	return nil
}

func validateConfigDownloadTopLevelAttributes(element xml.StartElement, name string) error {
	if element.Name.Space != "" {
		return fmt.Errorf("ConfigDownload response element %s must not contain namespaces", name)
	}
	if name == "Info" || isAppendixA4Type(name) {
		return nil
	}
	allowNum := name == "VideoParamConfig" || name == "AudioParamConfig" || name == "VideoParamAttribute"
	seenNum := false
	for _, attribute := range element.Attr {
		if attribute.Name.Space != "" || !allowNum || attribute.Name.Local != "Num" {
			return fmt.Errorf("ConfigDownload response element %s contains unsupported attribute %s", name, attribute.Name.Local)
		}
		if seenNum {
			return fmt.Errorf("ConfigDownload response element %s contains duplicate Num attribute", name)
		}
		seenNum = true
	}
	return nil
}

func validateConfigDownloadMediaResponse(version GBProtocolVersion, response *ConfigDownloadResponse) error {
	if response == nil {
		return fmt.Errorf("ConfigDownload response is required")
	}
	if response.VideoParamOpt != nil {
		if err := validateVideoParamOptResponse(version, response.VideoParamOpt); err != nil {
			return err
		}
	}
	if response.VideoParamConfig != nil {
		if version != GBVersion11 {
			return fmt.Errorf("VideoParamConfig response is not supported by %s", version.StandardName())
		}
		if err := validateVideoParamConfigResponse(response.VideoParamConfig); err != nil {
			return err
		}
	}
	if response.AudioParamOpt != nil {
		if version != GBVersion11 {
			return fmt.Errorf("AudioParamOpt response is not supported by %s", version.StandardName())
		}
		if err := validateAudioParamOptResponse(response.AudioParamOpt); err != nil {
			return err
		}
	}
	if response.AudioParamConfig != nil {
		if version != GBVersion11 {
			return fmt.Errorf("AudioParamConfig response is not supported by %s", version.StandardName())
		}
		if err := validateAudioParamConfigResponse(response.AudioParamConfig); err != nil {
			return err
		}
	}
	return nil
}

func validateVideoParamOptResponse(version GBProtocolVersion, section *VideoParamOpt) error {
	if section == nil {
		return fmt.Errorf("VideoParamOpt response is required")
	}
	if len(section.Attributes) != 0 {
		return fmt.Errorf("VideoParamOpt response contains unsupported attributes")
	}
	var schema svacFragmentSchema
	switch version {
	case GBVersion11:
		schema = svacFragmentSchema{
			"VideoParamOpt": {
				"VideoFormatOpt": 1, "ResolutionOpt": 1, "FrameRateOpt": 1,
				"BitRateTypeOpt": 1, "VideoBitRateOpt": 1, "DownloadSpeedOpt": 1,
			},
		}
	case GBVersion20, GBVersion30:
		schema = svacFragmentSchema{"VideoParamOpt": {"DownloadSpeed": 1, "Resolution": 1}}
	default:
		return fmt.Errorf("VideoParamOpt response is not supported by %s", version.StandardName())
	}
	if err := validateConfigResponseFragmentStructure("VideoParamOpt", section.InnerXML, schema); err != nil {
		return err
	}
	var values videoParamOptResponse
	if err := decodeConfigResponseFragment("VideoParamOpt", section.InnerXML, &values); err != nil {
		return err
	}
	if version == GBVersion11 {
		if !requiredStringList(values.VideoFormatOpt) || !requiredStringList(values.ResolutionOpt) ||
			!requiredIntegerList(values.FrameRateOpt) || !requiredIntegerList(values.BitRateTypeOpt) ||
			!requiredIntegerList(values.VideoBitRateOpt) || !requiredStringList(values.DownloadSpeedOpt) {
			return fmt.Errorf("VideoParamOpt response requires all 2014 option lists with valid integer fields")
		}
		return nil
	}
	if !optionalStringList(values.DownloadSpeed) || !optionalStringList(values.Resolution) {
		return fmt.Errorf("VideoParamOpt response contains an empty option list")
	}
	return nil
}

func validateVideoParamConfigResponse(section *VideoParamConfig) error {
	if section == nil {
		return fmt.Errorf("VideoParamConfig response is required")
	}
	if len(section.Attributes) != 0 {
		return fmt.Errorf("VideoParamConfig response contains unsupported attributes")
	}
	schema := svacFragmentSchema{
		"VideoParamConfig": {"Item": int(^uint(0) >> 1)},
		"VideoParamConfig/Item": {
			"StreamName": 1, "VideoFormat": 1, "Resolution": 1,
			"FrameRate": 1, "BitRateType": 1, "VideoBitRate": 1,
		},
	}
	if err := validateConfigResponseFragmentStructure("VideoParamConfig", section.InnerXML, schema); err != nil {
		return err
	}
	var values videoParamConfigResponse
	if err := decodeConfigResponseFragment("VideoParamConfig", section.InnerXML, &values); err != nil {
		return err
	}
	if err := validateConfigResponseCount("VideoParamConfig", section.Num, len(values.Items)); err != nil {
		return err
	}
	for index, item := range values.Items {
		if !requiredText(item.StreamName) || !requiredText(item.VideoFormat) || !requiredText(item.Resolution) ||
			!requiredText(item.FrameRate) || !requiredText(item.BitRateType) || !requiredText(item.VideoBitRate) {
			return fmt.Errorf("VideoParamConfig Item %d requires all standard fields", index+1)
		}
	}
	return nil
}

func validateAudioParamOptResponse(section *AudioParamOpt) error {
	if section == nil {
		return fmt.Errorf("AudioParamOpt response is required")
	}
	if len(section.Attributes) != 0 {
		return fmt.Errorf("AudioParamOpt response contains unsupported attributes")
	}
	schema := svacFragmentSchema{
		"AudioParamOpt": {"AudioFormatOpt": 1, "AudioBitRateOpt": 1, "SamplingRateOpt": 1},
	}
	if err := validateConfigResponseFragmentStructure("AudioParamOpt", section.InnerXML, schema); err != nil {
		return err
	}
	var values audioParamOptResponse
	if err := decodeConfigResponseFragment("AudioParamOpt", section.InnerXML, &values); err != nil {
		return err
	}
	if !requiredStringList(values.AudioFormatOpt) || !requiredIntegerList(values.AudioBitRateOpt) || !requiredStringList(values.SamplingRateOpt) {
		return fmt.Errorf("AudioParamOpt response requires all 2014 option lists with a valid AudioBitRateOpt")
	}
	return nil
}

func validateAudioParamConfigResponse(section *AudioParamConfig) error {
	if section == nil {
		return fmt.Errorf("AudioParamConfig response is required")
	}
	if len(section.Attributes) != 0 {
		return fmt.Errorf("AudioParamConfig response contains unsupported attributes")
	}
	schema := svacFragmentSchema{
		"AudioParamConfig": {"Item": int(^uint(0) >> 1)},
		"AudioParamConfig/Item": {
			"StreamName": 1, "AudioFormat": 1, "AudioBitRate": 1, "SamplingRate": 1,
		},
	}
	if err := validateConfigResponseFragmentStructure("AudioParamConfig", section.InnerXML, schema); err != nil {
		return err
	}
	var values audioParamConfigResponse
	if err := decodeConfigResponseFragment("AudioParamConfig", section.InnerXML, &values); err != nil {
		return err
	}
	if err := validateConfigResponseCount("AudioParamConfig", section.Num, len(values.Items)); err != nil {
		return err
	}
	for index, item := range values.Items {
		if !requiredText(item.StreamName) || !requiredText(item.AudioFormat) || !requiredText(item.AudioBitRate) || !requiredText(item.SamplingRate) {
			return fmt.Errorf("AudioParamConfig Item %d requires all standard fields", index+1)
		}
	}
	return nil
}

func validateConfigResponseFragmentStructure(name, value string, schema svacFragmentSchema) error {
	type frame struct {
		name     string
		path     string
		children map[string]int
		text     strings.Builder
	}
	decoder := xml.NewDecoder(strings.NewReader("<" + name + ">" + strings.TrimSpace(value) + "</" + name + ">"))
	stack := make([]frame, 0, 3)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s response does not match the GB/T 28181 schema: %w", name, err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			if len(current.Attr) != 0 || current.Name.Space != "" {
				return fmt.Errorf("%s response element %s must not contain attributes or namespaces", name, current.Name.Local)
			}
			path := current.Name.Local
			if len(stack) == 0 {
				if current.Name.Local != name {
					return fmt.Errorf("%s response has invalid root element %s", name, current.Name.Local)
				}
			} else {
				parent := &stack[len(stack)-1]
				limits, ok := schema[parent.path]
				if !ok {
					return fmt.Errorf("%s response element %s must not contain child elements", name, parent.name)
				}
				maximum, ok := limits[current.Name.Local]
				if !ok {
					return fmt.Errorf("%s response element %s is not allowed in %s", name, current.Name.Local, parent.name)
				}
				parent.children[current.Name.Local]++
				if parent.children[current.Name.Local] > maximum {
					return fmt.Errorf("%s response contains too many %s elements in %s", name, current.Name.Local, parent.name)
				}
				path = parent.path + "/" + current.Name.Local
			}
			stack = append(stack, frame{name: current.Name.Local, path: path, children: make(map[string]int)})
		case xml.CharData:
			if len(stack) != 0 {
				stack[len(stack)-1].text.Write(current)
			}
		case xml.EndElement:
			if len(stack) == 0 {
				return fmt.Errorf("%s response contains an unexpected closing element", name)
			}
			currentFrame := stack[len(stack)-1]
			if currentFrame.name != current.Name.Local {
				return fmt.Errorf("%s response contains mismatched element %s", name, current.Name.Local)
			}
			if _, container := schema[currentFrame.path]; container && strings.TrimSpace(currentFrame.text.String()) != "" {
				return fmt.Errorf("%s response element %s must not contain text", name, currentFrame.name)
			}
			stack = stack[:len(stack)-1]
		case xml.Directive, xml.ProcInst:
			return fmt.Errorf("%s response contains an unsupported XML instruction", name)
		}
	}
	if len(stack) != 0 {
		return fmt.Errorf("%s response contains unclosed elements", name)
	}
	return nil
}

func decodeConfigResponseFragment(name, value string, output any) error {
	if err := xml.Unmarshal([]byte("<"+name+">"+strings.TrimSpace(value)+"</"+name+">"), output); err != nil {
		return fmt.Errorf("%s response has invalid values: %w", name, err)
	}
	return nil
}

func validateConfigResponseCount(name string, count *int, items int) error {
	if count != nil && (*count < 0 || *count != items) {
		return fmt.Errorf("%s response Num must match Item count", name)
	}
	return nil
}

func requiredText(value *string) bool {
	return value != nil && strings.TrimSpace(*value) != ""
}

func requiredStringList(value *string) bool {
	return value != nil && validStringList(*value)
}

func optionalStringList(value *string) bool {
	return value == nil || validStringList(*value)
}

func validStringList(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if strings.TrimSpace(part) == "" {
			return false
		}
	}
	return true
}

func requiredIntegerList(value *string) bool {
	if value == nil || !validStringList(*value) {
		return false
	}
	for _, part := range strings.Split(*value, "/") {
		if _, err := strconv.Atoi(strings.TrimSpace(part)); err != nil {
			return false
		}
	}
	return true
}
