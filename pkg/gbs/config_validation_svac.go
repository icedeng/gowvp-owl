package gbs

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type svacROIConfig struct {
	ROIFlag            *int          `xml:"ROIFlag"`
	ROINumber          *int          `xml:"ROINumber"`
	Items              []svacROIItem `xml:"Item"`
	BackGroundQP       *int          `xml:"BackGroundQP"`
	BackGroundSkipFlag *int          `xml:"BackGroundSkipFlag"`
}

type svacROIItem struct {
	ROISeq      *int `xml:"ROISeq"`
	TopLeft     *int `xml:"TopLeft"`
	BottomRight *int `xml:"BottomRight"`
	ROIQP       *int `xml:"ROIQP"`
}

type svacSVCConfig struct {
	SVCFlag              *int    `xml:"SVCFlag"`
	SVCSTMMode           *int    `xml:"SVCSTMMode"`
	SVCSpaceDomainMode   *int    `xml:"SVCSpaceDomainMode"`
	SVCTimeDomainMode    *int    `xml:"SVCTimeDomainMode"`
	SSVCRatioValue       *string `xml:"SSVCRatioValue"`
	SVCSpaceSupportMode  *int    `xml:"SVCSpaceSupportMode"`
	SVCTimeSupportMode   *int    `xml:"SVCTimeSupportMode"`
	SSVCRatioSupportList *string `xml:"SSVCRatioSupportList"`
}

type svacSurveillanceConfig struct {
	TimeFlag      *int `xml:"TimeFlag"`
	EventFlag     *int `xml:"EventFlag"`
	AlertFlag     *int `xml:"AlertFlag"`
	OSDFlag       *int `xml:"OSDFlag"`
	AIFlag        *int `xml:"AIFlag"`
	GISFlag       *int `xml:"GISFlag"`
	TimeShowFlag  *int `xml:"TimeShowFlag"`
	EventShowFlag *int `xml:"EventShowFlag"`
	AlerShowtFlag *int `xml:"AlerShowtFlag"`
	OSDShowFlag   *int `xml:"OSDShowFlag"`
	AIShowFlag    *int `xml:"AIShowFlag"`
	GISShowFlag   *int `xml:"GISShowFlag"`
}

type svacEncryptConfig struct {
	EncryptionFlag     *int `xml:"EncryptionFlag"`
	AuthenticationFlag *int `xml:"AuthenticationFlag"`
}

type svacAudioConfig struct {
	AudioRecognitionFlag *int `xml:"AudioRecognitionFlag"`
}

type svacEncodeFragment struct {
	ROIParam          *svacROIConfig          `xml:"ROIParam"`
	SVCParam          *svacSVCConfig          `xml:"SVCParam"`
	SurveillanceParam *svacSurveillanceConfig `xml:"SurveillanceParam"`
	EncryptParam      *svacEncryptConfig      `xml:"EncryptParam"`
	AudioParam        *svacAudioConfig        `xml:"AudioParam"`
}

type svacDecodeFragment struct {
	SVCParam          *svacSVCConfig          `xml:"SVCParam"`
	SurveillanceParam *svacSurveillanceConfig `xml:"SurveillanceParam"`
}

type svacChildLimits map[string]int
type svacFragmentSchema map[string]svacChildLimits

func svacEncodeXML(config *SVACEncodeConfig) string {
	if config == nil {
		return ""
	}
	return strings.TrimSpace(config.InnerXML)
}

func svacDecodeXML(config *SVACDecodeConfig) string {
	if config == nil {
		return ""
	}
	return strings.TrimSpace(config.InnerXML)
}

func validateSVACConfig(version GBProtocolVersion, name, value string) error {
	return validateSVACFragment(version, name, value, false)
}

func validateSVACConfigResponse(version GBProtocolVersion, name, value string) error {
	return validateSVACFragment(version, name, value, true)
}

func validateSVACFragment(version GBProtocolVersion, name, value string, queryResponse bool) error {
	if err := validateDeviceConfigXMLFragment(name, value); err != nil {
		return err
	}
	schema, err := svacSchema(version, name, queryResponse)
	if err != nil {
		return err
	}
	if err := validateSVACFragmentStructure(name, value, schema); err != nil {
		return err
	}
	switch name {
	case "SVACEncodeConfig":
		var config svacEncodeFragment
		if err := decodeSVACFragment(name, value, &config); err != nil {
			return err
		}
		return validateSVACEncodeConfig(version, &config, queryResponse)
	case "SVACDecodeConfig":
		var config svacDecodeFragment
		if err := decodeSVACFragment(name, value, &config); err != nil {
			return err
		}
		return validateSVACDecodeConfig(version, &config, queryResponse)
	default:
		return fmt.Errorf("unsupported SVAC configuration section %s", name)
	}
}

func svacSchema(version GBProtocolVersion, name string, queryResponse bool) (svacFragmentSchema, error) {
	switch version {
	case GBVersion11:
		return svacSchema2014(name)
	case GBVersion20:
		if queryResponse {
			return svacResponseSchema2016(name)
		}
		return svacSchema2016(name)
	case GBVersion30:
		return svacSchema2022(name)
	default:
		return nil, fmt.Errorf("%s is not supported by %s", name, version.StandardName())
	}
}

func svacSchema2014(name string) (svacFragmentSchema, error) {
	switch name {
	case "SVACEncodeConfig":
		return svacFragmentSchema{
			name:                        {"ROIParam": 1, "SVCParam": 1, "SurveillanceParam": 1, "EncryptParam": 1, "AudioParam": 1},
			name + "/ROIParam":          {"ROIFlag": 1, "ROINumber": 1, "Item": 16, "BackGroundQP": 1, "BackGroundSkipFlag": 1},
			name + "/ROIParam/Item":     {"ROISeq": 1, "TopLeft": 1, "BottomRight": 1, "ROIQP": 1},
			name + "/SVCParam":          {"SVCFlag": 1, "SVCSTMMode": 1, "SVCSpaceDomainMode": 1, "SVCTimeDomainMode": 1},
			name + "/SurveillanceParam": {"TimeFlag": 1, "EventFlag": 1, "AlertFlag": 1},
			name + "/EncryptParam":      {"EncryptionFlag": 1, "AuthenticationFlag": 1},
			name + "/AudioParam":        {"AudioRecognitionFlag": 1},
		}, nil
	case "SVACDecodeConfig":
		return svacFragmentSchema{
			name:                        {"SVCParam": 1, "SurveillanceParam": 1},
			name + "/SVCParam":          {"SVCSTMMode": 1},
			name + "/SurveillanceParam": {"TimeShowFlag": 1, "EventShowFlag": 1, "AlerShowtFlag": 1},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported SVAC configuration section %s", name)
	}
}

func svacSchema2016(name string) (svacFragmentSchema, error) {
	switch name {
	case "SVACEncodeConfig":
		return svacFragmentSchema{
			name:                    {"ROIParam": 1, "AudioParam": 1},
			name + "/ROIParam":      {"ROIFlag": 1, "ROINumber": 1, "Item": 16, "BackGroundQP": 1, "BackGroundSkipFlag": 1},
			name + "/ROIParam/Item": {"ROISeq": 1, "TopLeft": 1, "BottomRight": 1, "ROIQP": 1},
			name + "/AudioParam":    {"AudioRecognitionFlag": 1},
		}, nil
	case "SVACDecodeConfig":
		return svacFragmentSchema{
			name:                        {"SVCParam": 1, "SurveillanceParam": 1},
			name + "/SVCParam":          {"SVCSTMMode": 1},
			name + "/SurveillanceParam": {"TimeShowFlag": 1, "EventShowFlag": 1, "AlerShowtFlag": 1},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported SVAC configuration section %s", name)
	}
}

func svacResponseSchema2016(name string) (svacFragmentSchema, error) {
	switch name {
	case "SVACEncodeConfig":
		return svacFragmentSchema{
			name:                        {"ROIParam": 1, "SVCParam": 1, "SurveillanceParam": 1, "AudioParam": 1},
			name + "/ROIParam":          {"ROIFlag": 1, "ROINumber": 1, "Item": 16, "BackGroundQP": 1, "BackGroundSkipFlag": 1},
			name + "/ROIParam/Item":     {"ROISeq": 1, "TopLeft": 1, "BottomRight": 1, "ROIQP": 1},
			name + "/SVCParam":          {"SVCSpaceDomainMode": 1, "SVCTimeDomainMode": 1, "SVCSpaceSupportMode": 1, "SVCTimeSupportMode": 1},
			name + "/SurveillanceParam": {"TimeFlag": 1, "EventFlag": 1, "AlertFlag": 1},
			name + "/AudioParam":        {"AudioRecognitionFlag": 1},
		}, nil
	case "SVACDecodeConfig":
		return svacFragmentSchema{
			name:                        {"SVCParam": 1, "SurveillanceParam": 1},
			name + "/SVCParam":          {"SVCSpaceSupportMode": 1, "SVCTimeSupportMode": 1},
			name + "/SurveillanceParam": {"TimeShowFlag": 1, "EventShowFlag": 1, "AlerShowtFlag": 1},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported SVAC configuration section %s", name)
	}
}

func svacSchema2022(name string) (svacFragmentSchema, error) {
	switch name {
	case "SVACEncodeConfig":
		return svacFragmentSchema{
			name:                        {"ROIParam": 1, "SVCParam": 1, "SurveillanceParam": 1, "AudioParam": 1},
			name + "/ROIParam":          {"ROIFlag": 1, "ROINumber": 1, "Item": 16},
			name + "/ROIParam/Item":     {"ROISeq": 1, "TopLeft": 1, "BottomRight": 1, "ROIQP": 1},
			name + "/SVCParam":          {"SVCSpaceDomainMode": 1, "SVCTimeDomainMode": 1, "SSVCRatioValue": 1, "SVCSpaceSupportMode": 1, "SVCTimeSupportMode": 1, "SSVCRatioSupportList": 1},
			name + "/SurveillanceParam": {"TimeFlag": 1, "OSDFlag": 1, "AIFlag": 1, "GISFlag": 1},
			name + "/AudioParam":        {"AudioRecognitionFlag": 1},
		}, nil
	case "SVACDecodeConfig":
		return svacFragmentSchema{
			name:                        {"SVCParam": 1, "SurveillanceParam": 1},
			name + "/SVCParam":          {"SVCSTMMode": 1, "SVCSpaceSupportMode": 1, "SVCTimeSupportMode": 1},
			name + "/SurveillanceParam": {"TimeShowFlag": 1, "OSDShowFlag": 1, "AIShowFlag": 1, "GISShowFlag": 1},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported SVAC configuration section %s", name)
	}
}

func validateSVACFragmentStructure(name, value string, schema svacFragmentSchema) error {
	type frame struct {
		name     string
		path     string
		children map[string]int
		text     strings.Builder
	}
	decoder := xml.NewDecoder(strings.NewReader("<" + name + ">" + strings.TrimSpace(value) + "</" + name + ">"))
	stack := make([]frame, 0, 4)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%s does not match the GB/T 28181 schema: %w", name, err)
		}
		switch current := token.(type) {
		case xml.StartElement:
			if len(current.Attr) != 0 || current.Name.Space != "" {
				return fmt.Errorf("%s element %s must not contain attributes or namespaces", name, current.Name.Local)
			}
			path := current.Name.Local
			if len(stack) == 0 {
				if current.Name.Local != name {
					return fmt.Errorf("%s has invalid root element %s", name, current.Name.Local)
				}
			} else {
				parent := &stack[len(stack)-1]
				limits, ok := schema[parent.path]
				if !ok {
					return fmt.Errorf("%s element %s must not contain child elements", name, parent.name)
				}
				maximum, ok := limits[current.Name.Local]
				if !ok {
					return fmt.Errorf("%s element %s is not allowed in %s", name, current.Name.Local, parent.name)
				}
				parent.children[current.Name.Local]++
				if parent.children[current.Name.Local] > maximum {
					return fmt.Errorf("%s contains too many %s elements in %s", name, current.Name.Local, parent.name)
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
				return fmt.Errorf("%s contains an unexpected closing element", name)
			}
			currentFrame := stack[len(stack)-1]
			if currentFrame.name != current.Name.Local {
				return fmt.Errorf("%s contains mismatched element %s", name, current.Name.Local)
			}
			if _, container := schema[currentFrame.path]; container && strings.TrimSpace(currentFrame.text.String()) != "" {
				return fmt.Errorf("%s element %s must not contain text", name, currentFrame.name)
			}
			if currentFrame.path == name && len(currentFrame.children) == 0 {
				return fmt.Errorf("%s requires at least one configuration parameter", name)
			}
			stack = stack[:len(stack)-1]
		case xml.Directive, xml.ProcInst:
			return fmt.Errorf("%s contains an unsupported XML instruction", name)
		}
	}
	if len(stack) != 0 {
		return fmt.Errorf("%s contains unclosed elements", name)
	}
	return nil
}

func decodeSVACFragment(name, value string, out any) error {
	if err := xml.Unmarshal([]byte("<"+name+">"+strings.TrimSpace(value)+"</"+name+">"), out); err != nil {
		return fmt.Errorf("%s has invalid values: %w", name, err)
	}
	return nil
}

func validateSVACEncodeConfig(version GBProtocolVersion, config *svacEncodeFragment, queryResponse bool) error {
	if config == nil {
		return fmt.Errorf("SVACEncodeConfig is required")
	}
	if config.ROIParam != nil {
		if err := validateSVACROIConfig(version, config.ROIParam, queryResponse); err != nil {
			return err
		}
	}
	switch version {
	case GBVersion11:
		if config.SVCParam != nil {
			if !requiredBinaryValue(config.SVCParam.SVCFlag) || !optionalSVACMode(config.SVCParam.SVCSTMMode) ||
				!optionalSVACMode(config.SVCParam.SVCSpaceDomainMode) || !optionalSVACMode(config.SVCParam.SVCTimeDomainMode) {
				return fmt.Errorf("SVACEncodeConfig SVCParam requires SVCFlag 0~1 and mode values 0~3")
			}
		}
		if config.SurveillanceParam != nil && !validBinarySet(config.SurveillanceParam.TimeFlag, config.SurveillanceParam.EventFlag, config.SurveillanceParam.AlertFlag) {
			return fmt.Errorf("SVACEncodeConfig SurveillanceParam values must be 0 or 1")
		}
		if config.EncryptParam != nil && !validBinarySet(config.EncryptParam.EncryptionFlag, config.EncryptParam.AuthenticationFlag) {
			return fmt.Errorf("SVACEncodeConfig EncryptParam values must be 0 or 1")
		}
	case GBVersion20:
		if config.SVCParam != nil && (!requiredSVACMode(config.SVCParam.SVCSpaceDomainMode) ||
			!requiredSVACMode(config.SVCParam.SVCTimeDomainMode) || !requiredSVACMode(config.SVCParam.SVCSpaceSupportMode) ||
			!requiredSVACMode(config.SVCParam.SVCTimeSupportMode)) {
			return fmt.Errorf("SVACEncodeConfig SVCParam response values must be 0~3")
		}
		if config.SurveillanceParam != nil && !validRequiredBinaryValues(config.SurveillanceParam.TimeFlag, config.SurveillanceParam.EventFlag, config.SurveillanceParam.AlertFlag) {
			return fmt.Errorf("SVACEncodeConfig SurveillanceParam response requires values 0 or 1")
		}
	case GBVersion30:
		if config.SVCParam != nil {
			if !valueInRange(config.SVCParam.SVCSpaceDomainMode, 0, 3) || !valueInRange(config.SVCParam.SVCTimeDomainMode, 0, 3) ||
				!optionalSVACMode(config.SVCParam.SVCSpaceSupportMode) || !optionalSVACMode(config.SVCParam.SVCTimeSupportMode) ||
				!optionalSVACRatioList(config.SVCParam.SSVCRatioValue) || !optionalSVACRatioList(config.SVCParam.SSVCRatioSupportList) {
				return fmt.Errorf("SVACEncodeConfig SVCParam requires encoding modes 0~3 and non-blank ratio values")
			}
			if queryResponse && (!requiredSVACMode(config.SVCParam.SVCSpaceSupportMode) || !requiredSVACMode(config.SVCParam.SVCTimeSupportMode)) {
				return fmt.Errorf("SVACEncodeConfig SVCParam response requires support modes 0~3")
			}
		}
		if config.SurveillanceParam != nil {
			valid := validOptionalBinaryValues(config.SurveillanceParam.TimeFlag, config.SurveillanceParam.OSDFlag, config.SurveillanceParam.AIFlag, config.SurveillanceParam.GISFlag)
			if queryResponse {
				valid = validRequiredBinaryValues(config.SurveillanceParam.TimeFlag, config.SurveillanceParam.OSDFlag, config.SurveillanceParam.AIFlag, config.SurveillanceParam.GISFlag)
			}
			if !valid {
				return fmt.Errorf("SVACEncodeConfig SurveillanceParam values must be 0 or 1")
			}
		}
	}
	if config.AudioParam != nil && !requiredBinaryValue(config.AudioParam.AudioRecognitionFlag) {
		return fmt.Errorf("SVACEncodeConfig AudioRecognitionFlag must be 0 or 1")
	}
	return nil
}

func validateSVACDecodeConfig(version GBProtocolVersion, config *svacDecodeFragment, queryResponse bool) error {
	if config == nil {
		return fmt.Errorf("SVACDecodeConfig is required")
	}
	if config.SVCParam != nil {
		valid := valueInRange(config.SVCParam.SVCSTMMode, 0, 3) &&
			optionalSVACMode(config.SVCParam.SVCSpaceSupportMode) && optionalSVACMode(config.SVCParam.SVCTimeSupportMode)
		if queryResponse && version == GBVersion20 {
			valid = requiredSVACMode(config.SVCParam.SVCSpaceSupportMode) && requiredSVACMode(config.SVCParam.SVCTimeSupportMode)
		}
		if queryResponse && version == GBVersion30 {
			valid = optionalSVACMode(config.SVCParam.SVCSTMMode) && requiredSVACMode(config.SVCParam.SVCSpaceSupportMode) && requiredSVACMode(config.SVCParam.SVCTimeSupportMode)
		}
		if !valid {
			return fmt.Errorf("SVACDecodeConfig SVCParam values must be 0~3")
		}
	}
	if config.SurveillanceParam == nil {
		return nil
	}
	if version == GBVersion30 {
		valid := validOptionalBinaryValues(config.SurveillanceParam.TimeShowFlag, config.SurveillanceParam.OSDShowFlag, config.SurveillanceParam.AIShowFlag, config.SurveillanceParam.GISShowFlag)
		if queryResponse {
			valid = validRequiredBinaryValues(config.SurveillanceParam.TimeShowFlag, config.SurveillanceParam.OSDShowFlag, config.SurveillanceParam.AIShowFlag, config.SurveillanceParam.GISShowFlag)
		}
		if !valid {
			return fmt.Errorf("SVACDecodeConfig SurveillanceParam values must be 0 or 1")
		}
		return nil
	}
	valid := validOptionalBinaryValues(config.SurveillanceParam.TimeShowFlag, config.SurveillanceParam.EventShowFlag, config.SurveillanceParam.AlerShowtFlag)
	if queryResponse && version == GBVersion20 {
		valid = validRequiredBinaryValues(config.SurveillanceParam.TimeShowFlag, config.SurveillanceParam.EventShowFlag, config.SurveillanceParam.AlerShowtFlag)
	}
	if !valid {
		return fmt.Errorf("SVACDecodeConfig SurveillanceParam values must be 0 or 1")
	}
	return nil
}

func validateSVACROIConfig(version GBProtocolVersion, config *svacROIConfig, queryResponse bool) error {
	if config == nil {
		return fmt.Errorf("SVACEncodeConfig ROIParam is required")
	}
	requireValues := version == GBVersion11 || queryResponse
	if (requireValues && !requiredBinaryValue(config.ROIFlag)) || (!requireValues && !optionalBinaryValue(config.ROIFlag)) {
		return fmt.Errorf("SVACEncodeConfig ROIFlag must be 0 or 1")
	}
	if requireValues {
		if !valueInRange(config.ROINumber, 0, 16) || *config.ROINumber != len(config.Items) {
			return fmt.Errorf("SVACEncodeConfig ROINumber must match 0~16 Item elements")
		}
		if version != GBVersion30 && (!valueInRange(config.BackGroundQP, 0, 3) || !requiredBinaryValue(config.BackGroundSkipFlag)) {
			return fmt.Errorf("SVACEncodeConfig requires BackGroundQP 0~3 and BackGroundSkipFlag 0~1")
		}
	} else {
		if config.ROINumber != nil && (!valueInRange(config.ROINumber, 0, 16) || *config.ROINumber != len(config.Items)) {
			return fmt.Errorf("SVACEncodeConfig ROINumber must match 0~16 Item elements")
		}
		if version == GBVersion20 && (!optionalValueInRange(config.BackGroundQP, 0, 3) || !optionalBinaryValue(config.BackGroundSkipFlag)) {
			return fmt.Errorf("SVACEncodeConfig background values must be 0~3 and 0~1")
		}
	}
	seen := make(map[int]struct{}, len(config.Items))
	for index, item := range config.Items {
		if err := validateSVACROIItem(version, item, requireValues); err != nil {
			return fmt.Errorf("SVACEncodeConfig ROI item %d: %w", index+1, err)
		}
		if item.ROISeq != nil {
			if _, ok := seen[*item.ROISeq]; ok {
				return fmt.Errorf("SVACEncodeConfig contains duplicate ROISeq %d", *item.ROISeq)
			}
			seen[*item.ROISeq] = struct{}{}
		}
	}
	return nil
}

func validateSVACROIItem(version GBProtocolVersion, item svacROIItem, required bool) error {
	if (required && !valueInRange(item.ROISeq, 1, 16)) || (!required && !optionalValueInRange(item.ROISeq, 1, 16)) {
		return fmt.Errorf("ROISeq must be 1~16")
	}
	coordinateMaximum := 19683
	if version == GBVersion30 {
		coordinateMaximum = int(^uint(0) >> 1)
	}
	if (required && (!valueInRange(item.TopLeft, 0, coordinateMaximum) || !valueInRange(item.BottomRight, 0, coordinateMaximum))) ||
		(!required && (!optionalValueInRange(item.TopLeft, 0, coordinateMaximum) || !optionalValueInRange(item.BottomRight, 0, coordinateMaximum))) {
		return fmt.Errorf("coordinates are outside the allowed range")
	}
	if item.TopLeft != nil && item.BottomRight != nil && *item.TopLeft > *item.BottomRight {
		return fmt.Errorf("TopLeft must not follow BottomRight")
	}
	if (required && !valueInRange(item.ROIQP, 0, 3)) || (!required && !optionalValueInRange(item.ROIQP, 0, 3)) {
		return fmt.Errorf("ROIQP must be 0~3")
	}
	return nil
}

func optionalValueInRange(value *int, minimum, maximum int) bool {
	return value == nil || valueInRange(value, minimum, maximum)
}

func optionalSVACMode(value *int) bool {
	return optionalValueInRange(value, 0, 3)
}

func requiredSVACMode(value *int) bool {
	return valueInRange(value, 0, 3)
}

func optionalSVACRatioList(value *string) bool {
	if value == nil {
		return true
	}
	parts := strings.Split(*value, "/")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		ratio := strings.Split(strings.TrimSpace(part), ":")
		if len(ratio) != 2 {
			return false
		}
		left, leftErr := strconv.Atoi(strings.TrimSpace(ratio[0]))
		right, rightErr := strconv.Atoi(strings.TrimSpace(ratio[1]))
		if leftErr != nil || rightErr != nil || left <= 0 || right <= 0 {
			return false
		}
	}
	return true
}

func validOptionalBinaryValues(values ...*int) bool {
	for _, value := range values {
		if !optionalBinaryValue(value) {
			return false
		}
	}
	return true
}

func validRequiredBinaryValues(values ...*int) bool {
	for _, value := range values {
		if !requiredBinaryValue(value) {
			return false
		}
	}
	return true
}

func validBinarySet(values ...*int) bool {
	present := false
	for _, value := range values {
		if value != nil {
			present = true
		}
		if !optionalBinaryValue(value) {
			return false
		}
	}
	return present
}
