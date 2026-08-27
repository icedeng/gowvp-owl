package gbs

import (
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type osdConfig30 struct {
	Length     *int              `xml:"Length"`
	Width      *int              `xml:"Width"`
	TimeX      *int              `xml:"TimeX"`
	TimeY      *int              `xml:"TimeY"`
	TimeEnable *int              `xml:"TimeEnable"`
	TimeType   *int              `xml:"TimeType"`
	TextEnable *int              `xml:"TextEnable"`
	SumNum     *int              `xml:"SumNum"`
	Items      []osdConfigItem30 `xml:"Item"`
}

type osdConfigItem30 struct {
	Text *string `xml:"Text"`
	X    *int    `xml:"X"`
	Y    *int    `xml:"Y"`
}

type videoParamAttribute30 struct {
	Items []videoParamAttributeItem30 `xml:"Item"`
}

type videoParamAttributeItem30 struct {
	StreamNumber *int    `xml:"StreamNumber"`
	VideoFormat  *string `xml:"VideoFormat"`
	Resolution   *string `xml:"Resolution"`
	FrameRate    *string `xml:"FrameRate"`
	BitRateType  *string `xml:"BitRateType"`
}

type videoRecordPlan30 struct {
	RecordEnable         *int                    `xml:"RecordEnable"`
	RecordScheduleSumNum *int                    `xml:"RecordScheduleSumNum"`
	Schedules            []videoRecordSchedule30 `xml:"RecordSchedule"`
	StreamNumber         *int                    `xml:"StreamNumber"`
}

type videoRecordSchedule30 struct {
	WeekDayNum        *int                 `xml:"WeekDayNum"`
	TimeSegmentSumNum *int                 `xml:"TimeSegmentSumNum"`
	Segments          []videoTimeSegment30 `xml:"TimeSegment"`
}

type videoTimeSegment30 struct {
	StartHour *int `xml:"StartHour"`
	StartMin  *int `xml:"StartMin"`
	StartSec  *int `xml:"StartSec"`
	StopHour  *int `xml:"StopHour"`
	StopMin   *int `xml:"StopMin"`
	StopSec   *int `xml:"StopSec"`
}

type videoAlarmRecord30 struct {
	RecordEnable  *int `xml:"RecordEnable"`
	RecordTime    *int `xml:"RecordTime"`
	PreRecordTime *int `xml:"PreRecordTime"`
	StreamNumber  *int `xml:"StreamNumber"`
}

type pictureMask30 struct {
	On         *int                     `xml:"On"`
	SumNum     *int                     `xml:"SumNum"`
	RegionList *pictureMaskRegionList30 `xml:"RegionList"`
}

type pictureMaskRegionList30 struct {
	Num   *int                      `xml:"Num,attr"`
	Items []pictureMaskRegionItem30 `xml:"Item"`
}

type pictureMaskRegionItem30 struct {
	Seq   *int    `xml:"Seq"`
	Point *string `xml:"Point"`
}

type alarmReport30 struct {
	MotionDetection *int `xml:"MotionDetection"`
	FieldDetection  *int `xml:"FieldDetection"`
}

func validateDeviceConfig30Fragment(name, value string) error {
	switch name {
	case "OSDConfig":
		return validateOSDConfig30(value)
	case "VideoRecordPlan":
		return validateVideoRecordPlan30(value)
	case "VideoAlarmRecord":
		return validateVideoAlarmRecord30(value)
	case "PictureMask":
		return validatePictureMask30(value)
	case "FrameMirror":
		return validateFrameMirror30(value)
	case "AlarmReport":
		return validateAlarmReport30(value)
	default:
		return nil
	}
}

func decodeDeviceConfig30Fragment(name, value string, out any) error {
	if err := xml.Unmarshal([]byte("<"+name+">"+strings.TrimSpace(value)+"</"+name+">"), out); err != nil {
		return fmt.Errorf("%s does not match the 2022 schema: %w", name, err)
	}
	return nil
}

func validateOSDConfig30(value string) error {
	var config osdConfig30
	if err := decodeDeviceConfig30Fragment("OSDConfig", value, &config); err != nil {
		return err
	}
	if config.Length == nil || config.Width == nil || config.TimeX == nil || config.TimeY == nil || config.SumNum == nil {
		return fmt.Errorf("OSDConfig requires Length, Width, TimeX, TimeY and SumNum")
	}
	if !optionalBinaryValue(config.TimeEnable) || !optionalBinaryValue(config.TextEnable) || !optionalBinaryValue(config.TimeType) {
		return fmt.Errorf("OSDConfig enable and time type values must be 0 or 1")
	}
	if *config.SumNum != len(config.Items) || len(config.Items) > 8 {
		return fmt.Errorf("OSDConfig SumNum must match 0~8 Item elements")
	}
	for index, item := range config.Items {
		if item.Text == nil || item.X == nil || item.Y == nil {
			return fmt.Errorf("OSDConfig item %d requires Text, X and Y", index+1)
		}
		if utf8.RuneCountInString(*item.Text) > 32 {
			return fmt.Errorf("OSDConfig item %d Text exceeds 32 characters", index+1)
		}
	}
	return nil
}

func validateVideoParamAttribute30(section *VideoParamAttribute, requireNum bool) error {
	if section == nil {
		return fmt.Errorf("VideoParamAttribute is required")
	}
	var config videoParamAttribute30
	if err := decodeDeviceConfig30Fragment("VideoParamAttribute", section.InnerXML, &config); err != nil {
		return err
	}
	if requireNum && !section.numPresent {
		return fmt.Errorf("VideoParamAttribute requires Num")
	}
	if requireNum && (section.Num < 0 || section.Num != len(config.Items)) {
		return fmt.Errorf("VideoParamAttribute Num must match Item elements")
	}
	section.Num = len(config.Items)
	for index, item := range config.Items {
		if item.StreamNumber == nil || item.VideoFormat == nil || item.Resolution == nil || item.FrameRate == nil || item.BitRateType == nil {
			return fmt.Errorf("VideoParamAttribute item %d is missing a required field", index+1)
		}
		if *item.StreamNumber < 0 || *item.StreamNumber > 2 {
			return fmt.Errorf("VideoParamAttribute item %d StreamNumber must be 0~2", index+1)
		}
	}
	return nil
}

func (config *VideoParamAttribute) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var decoded struct {
		Num      *int   `xml:"Num,attr"`
		InnerXML string `xml:",innerxml"`
	}
	if err := decoder.DecodeElement(&decoded, &start); err != nil {
		return err
	}
	config.InnerXML = decoded.InnerXML
	config.numPresent = decoded.Num != nil
	if decoded.Num != nil {
		config.Num = *decoded.Num
	}
	return nil
}

func validateVideoRecordPlan30(value string) error {
	var config videoRecordPlan30
	if err := decodeDeviceConfig30Fragment("VideoRecordPlan", value, &config); err != nil {
		return err
	}
	if !requiredBinaryValue(config.RecordEnable) || config.RecordScheduleSumNum == nil || config.StreamNumber == nil {
		return fmt.Errorf("VideoRecordPlan requires valid RecordEnable, RecordScheduleSumNum and StreamNumber")
	}
	if *config.StreamNumber < 0 || *config.StreamNumber > 2 {
		return fmt.Errorf("VideoRecordPlan StreamNumber must be 0~2")
	}
	if *config.RecordScheduleSumNum != len(config.Schedules) || len(config.Schedules) > 7 {
		return fmt.Errorf("VideoRecordPlan RecordScheduleSumNum must match 0~7 RecordSchedule elements")
	}
	for scheduleIndex, schedule := range config.Schedules {
		if schedule.WeekDayNum == nil || *schedule.WeekDayNum < 1 || *schedule.WeekDayNum > 7 || schedule.TimeSegmentSumNum == nil {
			return fmt.Errorf("VideoRecordPlan schedule %d requires WeekDayNum 1~7 and TimeSegmentSumNum", scheduleIndex+1)
		}
		if *schedule.TimeSegmentSumNum != len(schedule.Segments) || len(schedule.Segments) < 1 || len(schedule.Segments) > 8 {
			return fmt.Errorf("VideoRecordPlan schedule %d TimeSegmentSumNum must match 1~8 TimeSegment elements", scheduleIndex+1)
		}
		for segmentIndex, segment := range schedule.Segments {
			if !validVideoTimeSegment30(segment) {
				return fmt.Errorf("VideoRecordPlan schedule %d segment %d has an invalid time", scheduleIndex+1, segmentIndex+1)
			}
		}
	}
	return nil
}

func validateVideoAlarmRecord30(value string) error {
	var config videoAlarmRecord30
	if err := decodeDeviceConfig30Fragment("VideoAlarmRecord", value, &config); err != nil {
		return err
	}
	if !requiredBinaryValue(config.RecordEnable) || config.StreamNumber == nil || *config.StreamNumber < 0 || *config.StreamNumber > 2 {
		return fmt.Errorf("VideoAlarmRecord requires RecordEnable 0~1 and StreamNumber 0~2")
	}
	if (config.RecordTime != nil && *config.RecordTime < 0) || (config.PreRecordTime != nil && *config.PreRecordTime < 0) {
		return fmt.Errorf("VideoAlarmRecord time values must not be negative")
	}
	return nil
}

func validatePictureMask30(value string) error {
	var config pictureMask30
	if err := decodeDeviceConfig30Fragment("PictureMask", value, &config); err != nil {
		return err
	}
	if !requiredBinaryValue(config.On) || config.SumNum == nil || *config.SumNum < 0 || *config.SumNum > 4 {
		return fmt.Errorf("PictureMask requires On 0~1 and SumNum 0~4")
	}
	if config.RegionList == nil {
		if *config.SumNum != 0 {
			return fmt.Errorf("PictureMask SumNum must be 0 when RegionList is absent")
		}
		return nil
	}
	regions := config.RegionList
	if regions.Num == nil || *regions.Num != len(regions.Items) || *config.SumNum != len(regions.Items) || len(regions.Items) > 4 {
		return fmt.Errorf("PictureMask SumNum and RegionList Num must match 0~4 Item elements")
	}
	seen := make(map[int]struct{}, len(regions.Items))
	for index, item := range regions.Items {
		if item.Seq == nil || *item.Seq < 1 || *item.Seq > 4 || item.Point == nil || !validPictureMaskPoint(*item.Point) {
			return fmt.Errorf("PictureMask item %d requires Seq 1~4 and four Point coordinates", index+1)
		}
		if _, ok := seen[*item.Seq]; ok {
			return fmt.Errorf("PictureMask item %d has duplicate Seq", index+1)
		}
		seen[*item.Seq] = struct{}{}
	}
	return nil
}

func validPictureMaskPoint(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), ",")
	if len(parts) != 4 {
		return false
	}
	coordinates := make([]int, 4)
	for index, part := range parts {
		coordinate, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || coordinate < 0 {
			return false
		}
		coordinates[index] = coordinate
	}
	return coordinates[0] < coordinates[2] && coordinates[1] < coordinates[3]
}

func validateFrameMirror30(value string) error {
	mode, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || mode < 0 || mode > 3 {
		return fmt.Errorf("FrameMirror must be 0~3")
	}
	return nil
}

func validateAlarmReport30(value string) error {
	var config alarmReport30
	if err := decodeDeviceConfig30Fragment("AlarmReport", value, &config); err != nil {
		return err
	}
	if !requiredBinaryValue(config.MotionDetection) || !requiredBinaryValue(config.FieldDetection) {
		return fmt.Errorf("AlarmReport requires MotionDetection and FieldDetection values 0~1")
	}
	return nil
}

func requiredBinaryValue(value *int) bool {
	return value != nil && (*value == 0 || *value == 1)
}

func optionalBinaryValue(value *int) bool {
	return value == nil || requiredBinaryValue(value)
}

func validVideoTimeSegment30(segment videoTimeSegment30) bool {
	return valueInRange(segment.StartHour, 0, 23) && valueInRange(segment.StopHour, 0, 23) &&
		valueInRange(segment.StartMin, 0, 59) && valueInRange(segment.StartSec, 0, 59) &&
		valueInRange(segment.StopMin, 0, 59) && valueInRange(segment.StopSec, 0, 59)
}

func valueInRange(value *int, minValue, maxValue int) bool {
	return value != nil && *value >= minValue && *value <= maxValue
}
