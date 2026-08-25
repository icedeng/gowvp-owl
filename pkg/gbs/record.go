package gbs

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type RecordQueryInput struct {
	DeviceID  string
	ChannelID string
	Start     int64 // 查询起始时间（unix 秒）
	End       int64 // 查询结束时间（unix 秒）
	Timeout   time.Duration
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
	tx, err := g.svr.wrapRequest(ch, sip.MethodMessage, &sip.ContentTypeXML, sip.GetRecordInfoXML(in.ChannelID, sn, in.Start, in.End))
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

func recordQueryItemsResult(result multiResponseResult[RecordItem]) ([]RecordItem, error) {
	if len(result.Items) == 0 && !result.Complete {
		return nil, errors.New("wait RecordInfo response timeout")
	}
	return result.Items, nil
}

// MessageRecordInfoResponse 目录列表
type MessageRecordInfoResponse struct {
	CmdType  string       `xml:"CmdType"`
	SN       int          `xml:"SN"`
	DeviceID string       `xml:"DeviceID"`
	SumNum   int          `xml:"SumNum"`
	Item     []RecordItem `xml:"RecordList>Item"`
}

// RecordItem 目录详情
type RecordItem struct {
	// DeviceID 设备编号
	DeviceID string `xml:"DeviceID" bson:"DeviceID" json:"DeviceID"`
	// Name 设备名称
	Name       string `xml:"Name" bson:"Name" json:"Name"`
	FilePath   string `xml:"FilePath" bson:"FilePath" json:"FilePath"`
	Address    string `xml:"Address" bson:"Address" json:"Address"`
	StartTime  string `xml:"StartTime" bson:"StartTime" json:"StartTime"`
	EndTime    string `xml:"EndTime" bson:"EndTime" json:"EndTime"`
	Secrecy    int    `xml:"Secrecy" bson:"Secrecy" json:"Secrecy"`
	Type       string `xml:"Type" bson:"Type" json:"Type"`
	RecorderID string `xml:"RecorderID" bson:"RecorderID" json:"RecorderID,omitempty"`
	FileSize   string `xml:"FileSize" bson:"FileSize" json:"FileSize,omitempty"`
}

func (g *GB28181API) sipMessageRecordInfo(ctx *sip.Context) {
	message := &MessageRecordInfoResponse{}
	if err := sip.XMLDecode(ctx.Request.Body(), message); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}

	if g.recordResponses != nil {
		recordKey := buildMultiResponseKey(message.DeviceID, "RecordInfo", message.SN)
		if !g.recordResponses.Add(recordKey, message.SumNum, message.Item) {
			aliasKey := buildMultiResponseKey(ctx.DeviceID, "RecordInfo", message.SN)
			if value, ok := g.recordResponseAliases.Load(aliasKey); ok {
				g.recordResponses.Add(value.(string), message.SumNum, message.Item)
			}
		}
	}

	ctx.String(200, "OK")
}

func transRecordItems(items []RecordItem, start, end int64) Records {
	data := make([][]int64, 0, len(items))
	for _, item := range items {
		s, startErr := sip.ParseGBTime("2006-01-02T15:04:05", item.StartTime)
		e, endErr := sip.ParseGBTime("2006-01-02T15:04:05", item.EndTime)
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
