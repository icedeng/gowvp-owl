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
	if _, err = sipResponse(tx); err != nil {
		g.recordResponses.Cancel(recordKey)
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, in.Timeout)
	defer cancel()
	result := g.recordResponses.Wait(waitCtx, recordKey)
	if g.serviceStopped() {
		return nil, ErrServiceStopped
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
		s, _ := time.ParseInLocation("2006-01-02T15:04:05", item.StartTime, time.Local)
		e, _ := time.ParseInLocation("2006-01-02T15:04:05", item.EndTime, time.Local)
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
	if len(data) == 0 {
		return Records{}
	}
	res := Records{}
	list := map[string][]RecordInfo{}
	sort.Slice(data, func(i, j int) bool {
		return data[i][0] < data[j][0]
	})
	newData := [][]int64{}
	newDataIE := []int64{}

	for x, d := range data {
		if x == 0 {
			newDataIE = d
			continue
		}
		if d[0] == newDataIE[1] {
			newDataIE[1] = d[1]
		} else {
			newData = append(newData, newDataIE)
			newDataIE = d
		}
	}
	newData = append(newData, newDataIE)
	var cs, ce time.Time
	dates := []string{}
	for _, d := range newData {
		s := time.Unix(d[0], 0)
		e := time.Unix(d[1], 0)
		cs, _ = time.ParseInLocation("20060102", s.Format("20060102"), time.Local)
		for {
			ce = cs.Add(24 * time.Hour)
			if e.Unix() >= ce.Unix() {
				// 当前时段跨天
				if v, ok := list[cs.Format("2006-01-02")]; ok {
					list[cs.Format("2006-01-02")] = append(v, RecordInfo{
						Start: sip.Max(s.Unix(), cs.Unix()),
						End:   ce.Unix() - 1,
					})
				} else {
					list[cs.Format("2006-01-02")] = []RecordInfo{
						{
							Start: sip.Max(s.Unix(), cs.Unix()),
							End:   ce.Unix() - 1,
						},
					}
					dates = append(dates, cs.Format("2006-01-02"))
					res.DayTotal++
				}
				res.TimeNum++
				cs = ce
			} else {
				if v, ok := list[cs.Format("2006-01-02")]; ok {
					list[cs.Format("2006-01-02")] = append(v, RecordInfo{
						Start: sip.Max(s.Unix(), cs.Unix()),
						End:   e.Unix(),
					})
				} else {
					list[cs.Format("2006-01-02")] = []RecordInfo{
						{
							Start: sip.Max(s.Unix(), cs.Unix()),
							End:   e.Unix(),
						},
					}
					dates = append(dates, cs.Format("2006-01-02"))
					res.DayTotal++
				}
				res.TimeNum++
				break
			}
		}
	}
	resData := []RecordDate{}
	for _, date := range dates {
		resData = append(resData, RecordDate{
			Date:  date,
			Items: list[date],
		})
	}
	res.Data = resData
	return res
}
