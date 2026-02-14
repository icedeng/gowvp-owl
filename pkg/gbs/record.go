package gbs

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
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
func (g *GB28181API) QueryRecordList(_ context.Context, in *RecordQueryInput) (*Records, error) {
	if in == nil || in.DeviceID == "" || in.ChannelID == "" {
		return nil, errors.New("invalid record query input")
	}
	if in.Start <= 0 || in.End <= in.Start {
		return nil, errors.New("invalid record query time range")
	}

	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnline {
		return nil, ErrDeviceOffline
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.DeviceID, in.ChannelID)
	if !ok {
		return nil, ErrChannelNotExist
	}

	if in.Timeout <= 0 {
		in.Timeout = 10 * time.Second
	}

	sn := sip.RandInt(100000, 999999)
	resp := make(chan Records, 1)
	recordKey := fmt.Sprintf("%s%d", in.ChannelID, sn)
	// 以 channelID+SN 作为聚合键，收集分片返回。
	_recordList.Store(recordKey, recordList{
		channelid: in.ChannelID,
		resp:      resp,
		data:      [][]int64{},
		l:         &sync.Mutex{},
		s:         in.Start,
		e:         in.End,
	})
	defer _recordList.Delete(recordKey)

	// 按 GB28181 A.2.4.9 发送 RecordInfo 查询命令。
	tx, err := g.svr.wrapRequest(ch, sip.MethodMessage, &sip.ContentTypeXML, sip.GetRecordInfoXML(in.ChannelID, sn, in.Start, in.End))
	if err != nil {
		return nil, err
	}
	if _, err = sipResponse(tx); err != nil {
		return nil, err
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()

	select {
	case res := <-resp:
		return &res, nil
	case <-timer.C:
		// 超时返回已收集到的数据（兼容分包不完整场景）
		if list, ok := _recordList.Load(recordKey); ok {
			info := list.(recordList)
			data := transRecordList(info.data)
			return &data, nil
		}
		return nil, errors.New("query record list timeout")
	}
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
	Name      string `xml:"Name" bson:"Name" json:"Name"`
	FilePath  string `xml:"FilePath" bson:"FilePath" json:"FilePath"`
	Address   string `xml:"Address" bson:"Address" json:"Address"`
	StartTime string `xml:"StartTime" bson:"StartTime" json:"StartTime"`
	EndTime   string `xml:"EndTime" bson:"EndTime" json:"EndTime"`
	Secrecy   int    `xml:"Secrecy" bson:"Secrecy" json:"Secrecy"`
	Type      string `xml:"Type" bson:"Type" json:"Type"`
}

type recordList struct {
	channelid string // 通道国标编码
	resp      chan Records
	num       int       // 已累计条数
	data      [][]int64 // 时间段集合 [start,end]
	l         *sync.Mutex
	s, e      int64 // 查询窗口，超出窗口的数据会被截断
}

// 当前获取目录文件设备集合
var _recordList *sync.Map

func (g *GB28181API) sipMessageRecordInfo(ctx *sip.Context) {
	message := &MessageRecordInfoResponse{}
	if err := sip.XMLDecode(ctx.Request.Body(), message); err != nil {
		ctx.String(400, ErrXMLDecode.Error())
		return
	}

	// RecordInfo 响应的 DeviceID 通常是通道ID，兼容部分设备返回设备ID的场景
	recordKey := fmt.Sprintf("%s%d", message.DeviceID, message.SN)
	if !g.consumeRecordInfo(recordKey, message) && ctx.DeviceID != "" && ctx.DeviceID != message.DeviceID {
		// 某些设备会把 DeviceID 回写成设备ID，此时按设备ID再尝试一次。
		recordKey = fmt.Sprintf("%s%d", ctx.DeviceID, message.SN)
		g.consumeRecordInfo(recordKey, message)
	}

	ctx.String(200, "OK")
}

func (g *GB28181API) consumeRecordInfo(recordKey string, message *MessageRecordInfoResponse) bool {
	list, ok := _recordList.Load(recordKey)
	if !ok {
		return false
	}

	info := list.(recordList)
	info.l.Lock()
	defer info.l.Unlock()

	info.num += len(message.Item)
	var sint, eint int64
	for _, item := range message.Item {
		s, _ := time.ParseInLocation("2006-01-02T15:04:05", item.StartTime, time.Local)
		e, _ := time.ParseInLocation("2006-01-02T15:04:05", item.EndTime, time.Local)
		sint = s.Unix()
		eint = e.Unix()
		if sint < info.s {
			sint = info.s
		}
		if eint > info.e {
			eint = info.e
		}
		if sint < eint {
			// 只保留合法时间段，避免异常数据污染结果。
			info.data = append(info.data, []int64{sint, eint})
		}
	}
	if info.num >= message.SumNum && message.SumNum >= 0 {
		// 获取到完整数据，或设备返回条数异常时尽量收敛
		select {
		case info.resp <- transRecordList(info.data):
		default:
		}
	}
	_recordList.Store(recordKey, info)
	return true
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
