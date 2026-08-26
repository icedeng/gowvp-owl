package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type RecordQueryInput struct {
	DeviceID     string
	ChannelID    string
	Start        int64 // 查询起始时间（unix 秒）
	End          int64 // 查询结束时间（unix 秒）
	Timeout      time.Duration
	StreamNumber *int
	AlarmMethod  string
	AlarmType    string
}

// QueryRecordList 查询设备录像目录（RecordInfo）
func (g *GB28181API) QueryRecordList(ctx context.Context, in *RecordQueryInput) (*Records, error) {
	items, err := g.queryRecordItems(ctx, in)
	if err != nil {
		return nil, err
	}
	records := transRecordItems(items, in.Start, in.End)
	return &records, nil
}

func (g *GB28181API) queryRecordItems(ctx context.Context, in *RecordQueryInput) ([]RecordItem, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in == nil || in.DeviceID == "" || in.ChannelID == "" {
		return nil, errors.New("invalid record query input")
	}
	if in.Start <= 0 || in.End <= in.Start {
		return nil, errors.New("invalid record query time range")
	}

	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnlineNow() {
		return nil, ErrDeviceOffline
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.DeviceID, in.ChannelID)
	if !ok {
		return nil, ErrChannelNotExist
	}
	if err := validateRecordQueryFilters(g.getDeviceGBProtocolVersion(in.DeviceID), in); err != nil {
		return nil, err
	}

	if in.Timeout <= 0 {
		in.Timeout = 10 * time.Second
	}

	sn := g.nextQuerySN()
	recordKey := buildMultiResponseKey(in.ChannelID, "RecordInfo", sn)
	g.recordResponses.Start(recordKey)
	aliasKey := buildMultiResponseKey(in.DeviceID, "RecordInfo", sn)
	g.recordResponseAliases.Store(aliasKey, recordKey)
	defer g.recordResponseAliases.Delete(aliasKey)

	// 按 GB28181 A.2.4.9 发送 RecordInfo 查询命令。
	tx, err := g.svr.wrapRequestContext(ctx, ch, sip.MethodMessage, &sip.ContentTypeXML, sip.GetRecordInfoXMLWithFilters(in.ChannelID, sn, in.Start, in.End, sip.RecordInfoQueryFilters{
		StreamNumber: in.StreamNumber,
		AlarmMethod:  in.AlarmMethod,
		AlarmType:    in.AlarmType,
	}))
	if err != nil {
		g.recordResponses.Cancel(recordKey)
		return nil, err
	}
	if _, err = sipResponseContext(ctx, tx); err != nil {
		g.recordResponses.Cancel(recordKey)
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, in.Timeout)
	defer cancel()
	result := g.recordResponses.Wait(waitCtx, recordKey)
	if g.serviceStopped() {
		return nil, ErrServiceStopped
	}
	if !result.Complete && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return recordQueryItemsResult(result)
}

func validateRecordQueryFilters(version GBProtocolVersion, in *RecordQueryInput) error {
	if in == nil {
		return errors.New("invalid record query input")
	}
	method := strings.TrimSpace(in.AlarmMethod)
	alarmType := strings.TrimSpace(in.AlarmType)
	if in.StreamNumber == nil && method == "" && alarmType == "" {
		return nil
	}
	if !version.AtLeast(GBVersion30) {
		return fmt.Errorf("record query filters require GB/T 28181-2022")
	}
	if in.StreamNumber != nil && *in.StreamNumber < 0 {
		return fmt.Errorf("record query stream_number must not be negative")
	}
	if method == "" && alarmType == "" {
		return nil
	}
	if err := validateAlarmResetInfo(method, alarmType); err != nil {
		return fmt.Errorf("invalid record query alarm filter: %w", err)
	}
	return nil
}

func recordQueryItemsResult(result multiResponseResult[RecordItem]) ([]RecordItem, error) {
	if len(result.Items) == 0 && !result.Complete {
		return nil, errors.New("wait RecordInfo response timeout")
	}
	return result.Items, nil
}

// MessageRecordInfoResponse 目录列表
type MessageRecordInfoResponse struct {
	XMLName   xml.Name
	CmdType   string       `xml:"CmdType"`
	SN        int          `xml:"SN"`
	DeviceID  string       `xml:"DeviceID"`
	Name      string       `xml:"Name"`
	SumNum    int          `xml:"SumNum"`
	HasSumNum bool         `xml:"-"`
	Item      []RecordItem `xml:"-"`
	ListNum   *int         `xml:"-"`
	HasList   bool         `xml:"-"`
}

func (m *MessageRecordInfoResponse) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var value struct {
		CmdType  string `xml:"CmdType"`
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
		Name     string `xml:"Name"`
		SumNum   *int   `xml:"SumNum"`
		List     *struct {
			Num  *int         `xml:"Num,attr"`
			Item []RecordItem `xml:"Item"`
		} `xml:"RecordList"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*m = MessageRecordInfoResponse{
		XMLName: start.Name, CmdType: value.CmdType, SN: value.SN, DeviceID: value.DeviceID, Name: value.Name,
	}
	if value.List != nil {
		m.HasList, m.Item, m.ListNum = true, value.List.Item, value.List.Num
	}
	if value.SumNum != nil {
		m.SumNum, m.HasSumNum = *value.SumNum, true
	}
	return nil
}

// RecordItem 目录详情
type RecordItem struct {
	// DeviceID 设备编号
	DeviceID string `xml:"DeviceID" bson:"DeviceID" json:"DeviceID"`
	// Name 设备名称
	Name        string `xml:"Name" bson:"Name" json:"Name"`
	HasName     bool   `xml:"-" bson:"-" json:"-"`
	FilePath    string `xml:"FilePath,omitempty" bson:"FilePath" json:"FilePath"`
	Address     string `xml:"Address,omitempty" bson:"Address" json:"Address"`
	StartTime   string `xml:"StartTime,omitempty" bson:"StartTime" json:"StartTime"`
	EndTime     string `xml:"EndTime,omitempty" bson:"EndTime" json:"EndTime"`
	Secrecy     int    `xml:"Secrecy" bson:"Secrecy" json:"Secrecy"`
	HasSecrecy  bool   `xml:"-" bson:"-" json:"-"`
	Type        string `xml:"Type,omitempty" bson:"Type" json:"Type"`
	RecorderID  string `xml:"RecorderID,omitempty" bson:"RecorderID" json:"RecorderID,omitempty"`
	FileSize    string `xml:"FileSize,omitempty" bson:"FileSize" json:"FileSize,omitempty"`
	HasFileSize bool   `xml:"-" bson:"-" json:"-"`
	// RecordLocation 和 StreamNumber 由 2022 A.2.1.10 新增。
	RecordLocation    string `xml:"RecordLocation,omitempty" bson:"RecordLocation" json:"RecordLocation,omitempty"`
	HasRecordLocation bool   `xml:"-" bson:"-" json:"-"`
	StreamNumber      *int   `xml:"StreamNumber,omitempty" bson:"StreamNumber" json:"StreamNumber,omitempty"`
	hasStartTime      bool
	hasEndTime        bool
	hasType           bool
}

func (item *RecordItem) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	type recordItemAlias RecordItem
	var value struct {
		recordItemAlias
		Name           *string `xml:"Name"`
		Secrecy        *int    `xml:"Secrecy"`
		StartTime      *string `xml:"StartTime"`
		EndTime        *string `xml:"EndTime"`
		Type           *string `xml:"Type"`
		FileSize       *string `xml:"FileSize"`
		RecordLocation *string `xml:"RecordLocation"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*item = RecordItem(value.recordItemAlias)
	if value.Name != nil {
		item.Name, item.HasName = *value.Name, true
	}
	if value.Secrecy != nil {
		item.Secrecy, item.HasSecrecy = *value.Secrecy, true
	}
	if value.StartTime != nil {
		item.StartTime, item.hasStartTime = *value.StartTime, true
	}
	if value.EndTime != nil {
		item.EndTime, item.hasEndTime = *value.EndTime, true
	}
	if value.Type != nil {
		item.Type, item.hasType = *value.Type, true
	}
	if value.FileSize != nil {
		item.FileSize, item.HasFileSize = *value.FileSize, true
	}
	if value.RecordLocation != nil {
		item.RecordLocation, item.HasRecordLocation = *value.RecordLocation, true
	}
	return nil
}

func (g *GB28181API) sipMessageRecordInfo(ctx *sip.Context) {
	message := &MessageRecordInfoResponse{}
	if err := sip.XMLDecode(ctx.Request.Body(), message); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}

	message.CmdType = strings.TrimSpace(message.CmdType)
	message.DeviceID = strings.TrimSpace(message.DeviceID)
	message.Name = strings.TrimSpace(message.Name)
	recordKey, expectedTarget := g.recordResponseTarget(ctx, message)
	if err := g.validateRecordInfoEnvelope(ctx, message, expectedTarget); err != nil {
		ctx.String(400, err.Error())
		return
	}
	if g.recordResponses != nil && recordKey != "" {
		g.recordResponses.Add(recordKey, message.SumNum, message.Item)
	}

	ctx.String(200, "OK")
}

func (g *GB28181API) recordResponseTarget(ctx *sip.Context, message *MessageRecordInfoResponse) (string, string) {
	if g == nil || g.recordResponses == nil || message == nil {
		return "", ""
	}
	directKey := buildMultiResponseKey(message.DeviceID, "RecordInfo", message.SN)
	if g.recordResponses.Has(directKey) {
		return directKey, message.DeviceID
	}
	if ctx == nil {
		return "", ""
	}
	aliasKey := buildMultiResponseKey(strings.TrimSpace(ctx.DeviceID), "RecordInfo", message.SN)
	value, ok := g.recordResponseAliases.Load(aliasKey)
	if !ok {
		return "", ""
	}
	recordKey, ok := value.(string)
	if !ok {
		return "", ""
	}
	separator := strings.IndexByte(recordKey, ':')
	if separator <= 0 {
		return "", ""
	}
	return recordKey, recordKey[:separator]
}

func (g *GB28181API) validateRecordInfoEnvelope(ctx *sip.Context, message *MessageRecordInfoResponse, expectedTarget string) error {
	if message == nil || message.XMLName.Local != "Response" || !strings.EqualFold(message.CmdType, "RecordInfo") || message.SN <= 0 {
		return fmt.Errorf("invalid RecordInfo envelope")
	}
	if !isGBDeviceIdentifier(message.DeviceID) || message.Name == "" || !message.HasSumNum || message.SumNum < 0 {
		return fmt.Errorf("invalid RecordInfo response")
	}
	if !message.HasList {
		if message.SumNum != 0 || len(message.Item) != 0 {
			return fmt.Errorf("RecordInfo RecordList is required for non-empty results")
		}
	} else if message.ListNum == nil || *message.ListNum < 0 || *message.ListNum != len(message.Item) || len(message.Item) > message.SumNum || g.multiResponseChunkExceedsLimit(ctx, len(message.Item)) {
		return fmt.Errorf("invalid RecordInfo list count")
	}
	if err := g.validateAuthenticatedResponseTarget(ctx, message.DeviceID); err != nil {
		return err
	}
	if expectedTarget == "" {
		expectedTarget = message.DeviceID
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	for index := range message.Item {
		item := &message.Item[index]
		item.DeviceID = strings.TrimSpace(item.DeviceID)
		item.Name = strings.TrimSpace(item.Name)
		item.StartTime = strings.TrimSpace(item.StartTime)
		item.EndTime = strings.TrimSpace(item.EndTime)
		item.Type = strings.ToLower(strings.TrimSpace(item.Type))
		item.RecordLocation = strings.TrimSpace(item.RecordLocation)
		if item.DeviceID != expectedTarget {
			return fmt.Errorf("RecordInfo item target mismatch")
		}
		if !item.HasName || item.Name == "" || !item.HasSecrecy || item.Secrecy < 0 || item.Secrecy > 1 {
			return fmt.Errorf("invalid RecordInfo item")
		}
		var startTime, endTime time.Time
		var startErr, endErr error
		if item.hasStartTime {
			startTime, startErr = parseGBDateTime(item.StartTime)
		}
		if item.hasEndTime {
			endTime, endErr = parseGBDateTime(item.EndTime)
		}
		if startErr != nil || endErr != nil || item.hasStartTime && item.hasEndTime && !endTime.After(startTime) {
			return fmt.Errorf("invalid RecordInfo item time")
		}
		if (item.hasType && item.Type == "") || (item.Type != "" && !validRecordTypeForVersion(item.Type, version)) {
			return fmt.Errorf("invalid RecordInfo item type")
		}
		if item.HasFileSize && !version.AtLeast(GBVersion20) {
			return fmt.Errorf("RecordInfo file size requires protocol 2.0")
		}
		if (item.HasRecordLocation || item.StreamNumber != nil) && !version.AtLeast(GBVersion30) {
			return fmt.Errorf("RecordInfo storage fields require protocol 3.0")
		}
		if version.AtLeast(GBVersion30) {
			if item.HasRecordLocation && !isGBDeviceIdentifier(item.RecordLocation) {
				return fmt.Errorf("invalid RecordInfo storage location")
			}
			if item.StreamNumber != nil && *item.StreamNumber < 0 {
				return fmt.Errorf("invalid RecordInfo stream number")
			}
		}
	}
	return nil
}

func validRecordTypeForVersion(recordType string, version GBProtocolVersion) bool {
	switch recordType {
	case "time", "alarm", "manual":
		return true
	case "all":
		return version == GBVersion10 || version == GBVersion11
	default:
		return false
	}
}

func transRecordItems(items []RecordItem, start, end int64) Records {
	data := make([][]int64, 0, len(items))
	for _, item := range items {
		s, startErr := parseGBDateTime(item.StartTime)
		e, endErr := parseGBDateTime(item.EndTime)
		if startErr != nil || endErr != nil || !e.After(s) {
			continue
		}
		sint := s.Unix()
		eint := e.Unix()
		if sint < start {
			sint = start
		}
		if eint > end {
			eint = end
		}
		if sint < eint {
			data = append(data, []int64{sint, eint})
		}
	}
	return transRecordList(data)
}

// Records Records
type Records struct {
	// 存在录像的天数
	DayTotal int          `json:"daynum"`
	TimeNum  int          `json:"timenum"`
	Data     []RecordDate `json:"list"`
}

type RecordDate struct {
	// 日期
	Date string `json:"date"`
	// 时间段
	Items []RecordInfo `json:"items"`
}

type RecordInfo struct {
	Start int64 `json:"start" bson:"start"`
	End   int64 `json:"end" bson:"end"`
}

// 将返回的多组数据合并，时间连续的进行合并，最后按照天返回数据，返回为某天内时间段列表
func transRecordList(data [][]int64) Records {
	valid := make([][]int64, 0, len(data))
	for _, interval := range data {
		if len(interval) < 2 || interval[0] >= interval[1] {
			continue
		}
		valid = append(valid, []int64{interval[0], interval[1]})
	}
	if len(valid) == 0 {
		return Records{}
	}
	res := Records{}
	list := map[string][]RecordInfo{}
	sort.Slice(valid, func(i, j int) bool {
		return valid[i][0] < valid[j][0]
	})
	merged := make([][]int64, 0, len(valid))
	current := valid[0]
	for _, interval := range valid[1:] {
		if interval[0] <= current[1] {
			if interval[1] > current[1] {
				current[1] = interval[1]
			}
			continue
		}
		merged = append(merged, current)
		current = interval
	}
	merged = append(merged, current)

	dates := make([]string, 0)
	for _, interval := range merged {
		segmentStart, intervalEnd := interval[0], interval[1]
		startTime := time.Unix(segmentStart, 0).In(sip.GBTimeLocation())
		dayStart := time.Date(startTime.Year(), startTime.Month(), startTime.Day(), 0, 0, 0, 0, sip.GBTimeLocation())
		for segmentStart < intervalEnd {
			nextDay := dayStart.AddDate(0, 0, 1)
			segmentEnd := intervalEnd
			if segmentEnd >= nextDay.Unix() {
				segmentEnd = nextDay.Unix() - 1
			}
			date := dayStart.Format("2006-01-02")
			if _, exists := list[date]; !exists {
				dates = append(dates, date)
				res.DayTotal++
			}
			list[date] = append(list[date], RecordInfo{Start: segmentStart, End: segmentEnd})
			res.TimeNum++
			segmentStart = nextDay.Unix()
			dayStart = nextDay
		}
	}
	resData := make([]RecordDate, 0, len(dates))
	for _, date := range dates {
		resData = append(resData, RecordDate{
			Date:  date,
			Items: list[date],
		})
	}
	res.Data = resData
	return res
}
