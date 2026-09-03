package gbs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

// Streams Streams
type Streams struct {
	// historyControlMu 串行化同一历史会话的 INFO 控制，保证 2022 Range-only
	// 命令读取到的是前一个已成功命令提交的 Scale。
	historyControlMu sync.Mutex
	historyState     historyControlState
	// lifecycleMu 串行化异步媒体完成与启动状态提交，避免终态被迟到的启动写回覆盖。
	lifecycleMu sync.Mutex
	// cleanupMu 串行化 BYE 与 RTP 接收端口清理；成功步骤会被记住，失败步骤允许重试。
	cleanupMu     sync.Mutex
	dialogStopped bool
	// dialogCleanupDeferred 表示级联已转发 MediaStatus，需等待最后一个上级 BYE 后再结束下级对话。
	dialogCleanupDeferred bool
	rtpClosed             bool
	// cleanupRequested 允许活动判断在外部清理阻塞时无锁读取终止态。
	cleanupRequested atomic.Bool
	// 0  直播 1 历史
	T int `json:"t" gorm:"column:t"`
	// 设备ID
	DeviceID string `json:"deviceid" gorm:"column:deviceid"`
	// 通道ID
	ChannelID string `json:"channelid" gorm:"column:channelid"`
	//  pull 媒体服务器主动拉流，push 监控设备主动推流
	StreamType string `json:"streamtype" gorm:"column:streamtype"`
	// 0正常 1关闭 -1 尚未开始
	Status int `json:"status" gorm:"column:status"`
	// header from params
	// Ftag db.M `gorm:"column:ftag" sql:"type:json" json:"-"`
	// header to params
	// Ttag db.M `gorm:"column:ttag" sql:"type:json" json:"-"`
	// header callid
	CallID string `json:"callid" gorm:"column:callid"`
	// 是否停止
	Stop   bool   `json:"stop" gorm:"column:stop"`
	Msg    string `json:"msg" gorm:"column:msg"`
	CseqNo uint32 `json:"cseqno" gorm:"column:cseqno"`
	// 视频流ID gb28181的ssrc
	StreamID string `json:"streamid"  gorm:"column:streamid"`
	// m3u8播放地址
	HTTP string `json:"http" gorm:"column:http"`
	// rtmp 播放地址
	RTMP string `json:"rtmp" gorm:"column:rtmp"`
	// rtsp 播放地址
	RTSP string `json:"rtsp" gorm:"column:rtsp"`
	// flv 播放地址
	WSFLV string `json:"wsflv" gorm:"column:wsflv"`
	// zlm是否收到流
	Stream bool `json:"stream" gorm:"column:stream"`
	// LastMediaAt 是最近一次媒体服务器确认流存在的时间。
	LastMediaAt time.Time `json:"last_media_at" gorm:"-"`
	EndReason   string    `json:"end_reason" gorm:"-"`
	// DirectTCP 标识 2014 附录 O 裸 TCP 下载会话，不能按 RTP 流处理。
	DirectTCP       bool   `json:"direct_tcp" gorm:"-"`
	DirectSessionID string `json:"direct_session_id,omitempty" gorm:"-"`
	FileSize        int64  `json:"file_size,omitempty" gorm:"-"`
	FileSizeKnown   bool   `json:"file_size_known" gorm:"-"`

	// ---
	S, E        time.Time        `json:"-" gorm:"-"`
	ssrc        string           // 国标ssrc 10进制字符串
	ssrcRelease func()           // 仅在 SIP/RTP 资源均清理完成后释放当前 SSRC 代次
	mediaServer *sms.MediaServer `json:"-" gorm:"-"`
	sessionKey  string           // 内部会话键；级联历史会话用于隔离同通道的不同时间范围
	Ext         int64            `json:"-" gorm:"-"` // 流等待过期时间
	Resp        *sip.Response    `json:"-" gorm:"-"`
}

func (s *Streams) bindSSRCReservation(ssrc string, release func()) error {
	if s == nil || release == nil {
		return fmt.Errorf("invalid GB28181 SSRC reservation")
	}
	s.cleanupMu.Lock()
	if s.cleanupRequested.Load() {
		s.cleanupMu.Unlock()
		release()
		return fmt.Errorf("GB28181 media session stopped before SSRC reservation was bound")
	}
	if s.ssrcRelease != nil {
		s.cleanupMu.Unlock()
		release()
		return fmt.Errorf("GB28181 media session already owns an SSRC reservation")
	}
	s.ssrc = ssrc
	s.ssrcRelease = release
	s.cleanupMu.Unlock()
	return nil
}

func (s *Streams) releaseSSRCReservation() {
	if s == nil {
		return
	}
	s.cleanupMu.Lock()
	release := s.ssrcRelease
	s.ssrcRelease = nil
	s.cleanupMu.Unlock()
	if release != nil {
		release()
	}
}

func (s *Streams) nextCSeq() (uint32, error) {
	if s == nil {
		return 0, fmt.Errorf("stream is unavailable")
	}
	for {
		current := atomic.LoadUint32(&s.CseqNo)
		next, err := sip.NextCSeq(current)
		if err != nil {
			return 0, fmt.Errorf("stream CSeq: %w", err)
		}
		if atomic.CompareAndSwapUint32(&s.CseqNo, current, next) {
			return next, nil
		}
	}
}

// commitChannelStreamStart 把启动态提交与同一流的异步终止串行化。
// 若终止路径已经摘除该对象，启动方只承认会话曾成功建立，不能重新写回活动状态。
func (g *GB28181API) commitChannelStreamStart(ctx context.Context, key string, stream *Streams) (bool, error) {
	if g == nil || g.streams == nil || stream == nil {
		return false, nil
	}
	stream.lifecycleMu.Lock()
	defer stream.lifecycleMu.Unlock()
	current, ok := g.streams.Load(key)
	if !ok || current != stream {
		return false, nil
	}
	return true, g.persistChannelActive(ctx, stream.DeviceID, stream.ChannelID)
}

func (g *GB28181API) compareAndDeleteChannelStream(key string, stream *Streams) bool {
	if g == nil || g.streams == nil || stream == nil {
		return false
	}
	stream.lifecycleMu.Lock()
	defer stream.lifecycleMu.Unlock()
	return g.streams.CompareAndDelete(key, stream)
}

func (g *GB28181API) loadAndDeleteChannelStream(key string) (*Streams, bool) {
	if g == nil || g.streams == nil {
		return nil, false
	}
	stream, ok := g.streams.Load(key)
	if !ok || stream == nil {
		return stream, ok && g.streams.CompareAndDelete(key, stream)
	}
	if !g.compareAndDeleteChannelStream(key, stream) {
		return nil, false
	}
	return stream, true
}

// 当前系统中存在的流列表
type streamsList struct {
	// key=ssrc value=PlayParams  播放对应的PlayParams 用来发送bye获取tag，callid等数据
	Response *sync.Map
	// key=channelid value={Play}  当前设备直播信息，防止重复直播
	Succ *sync.Map
}

var StreamList streamsList

const gbSSRCSuffixCount = 10000

type ssrcDomainState struct {
	next  uint16
	inUse map[uint16]uint64
}

type ssrcAllocator struct {
	mu         sync.Mutex
	domains    map[string]*ssrcDomainState
	generation uint64
}

var processSSRCAllocator ssrcAllocator

func validateSSRCDomain(domain string) (string, error) {
	domain = strings.TrimSpace(domain)
	if len(domain) != 10 {
		return "", fmt.Errorf("GB28181 SIP domain must be a 10-digit code, got %q", domain)
	}
	for _, char := range domain {
		if char < '0' || char > '9' {
			return "", fmt.Errorf("GB28181 SIP domain must be a 10-digit code, got %q", domain)
		}
	}
	return domain, nil
}

// reserve 在同一 SIP 监控域内跨实时/历史媒体共享后四位，满足标准的不重复要求。
// release 绑定分配代次且幂等，迟到的旧会话清理不会释放后来复用的同一后四位。
func (a *ssrcAllocator) reserve(domain string, streamType int) (string, func(), error) {
	if a == nil {
		return "", nil, fmt.Errorf("GB28181 SSRC allocator is unavailable")
	}
	if streamType != 0 && streamType != 1 {
		return "", nil, fmt.Errorf("invalid GB28181 SSRC stream type %d", streamType)
	}
	validatedDomain, err := validateSSRCDomain(domain)
	if err != nil {
		return "", nil, err
	}

	a.mu.Lock()
	if a.domains == nil {
		a.domains = make(map[string]*ssrcDomainState)
	}
	state := a.domains[validatedDomain]
	if state == nil {
		state = &ssrcDomainState{inUse: make(map[uint16]uint64)}
		a.domains[validatedDomain] = state
	}
	if len(state.inUse) >= gbSSRCSuffixCount {
		a.mu.Unlock()
		return "", nil, fmt.Errorf("GB28181 SSRC space exhausted for SIP domain %s", validatedDomain)
	}

	var suffix uint16
	found := false
	for offset := 0; offset < gbSSRCSuffixCount; offset++ {
		candidate := uint16((int(state.next) + offset) % gbSSRCSuffixCount)
		if _, exists := state.inUse[candidate]; exists {
			continue
		}
		suffix = candidate
		found = true
		break
	}
	if !found {
		a.mu.Unlock()
		return "", nil, fmt.Errorf("GB28181 SSRC space exhausted for SIP domain %s", validatedDomain)
	}
	a.generation++
	if a.generation == 0 {
		a.generation++
	}
	generation := a.generation
	state.inUse[suffix] = generation
	state.next = uint16((int(suffix) + 1) % gbSSRCSuffixCount)
	a.mu.Unlock()

	ssrc := fmt.Sprintf("%d%s%04d", streamType, validatedDomain[3:8], suffix)
	var once sync.Once
	release := func() {
		once.Do(func() {
			a.mu.Lock()
			if current := a.domains[validatedDomain]; current != nil && current.inUse[suffix] == generation {
				delete(current.inUse, suffix)
			}
			a.mu.Unlock()
		})
	}
	return ssrc, release, nil
}

func (g *GB28181API) reserveSSRC(streamType int) (string, func(), error) {
	cfg := g.configSnapshot()
	if cfg == nil {
		return "", nil, fmt.Errorf("GB28181 SIP configuration is unavailable")
	}
	return processSSRCAllocator.reserve(cfg.GetDomain(), streamType)
}

// 定时检查未关闭的流
// 检查规则：
// 1. 数据库查询当前status=0在推流状态的所有流信息
// 2. 比对当前streamlist中存在的流，如果不在streamlist或者ssrc与channelid不匹配则关闭
func CheckStreams() {
	// logrus.Debugln("checkStreamWithCron")
	var skip int
	for {
		streams := []Streams{}
		// db.FindT(db.DBClient, new(Streams), &streams, db.M{"status=?": 0, "streamtype=?": "push"}, "", skip, 100, false)
		for index := range streams {
			stream := &streams[index]
			// logrus.Debugln("checkStreamStreamID", stream.StreamID, stream.DeviceID)
			if p, ok := StreamList.Response.Load(stream.StreamID); ok {
				streamActive := p.(*Streams)
				if streamActive.ChannelID == stream.ChannelID {
					// 此流在用
					// 查询media流是否仍然存在。不存在的需要关闭。
					rtpInfo := zlmGetMediaInfo(stream.StreamID)
					if rtpInfo.Exist {
						// 流仍然存在
						continue
					}
					if !stream.Stream && time.Now().Unix() < stream.Ext {
						// 推流尚未成功 未超时
						continue
					}
				}
			}
			// logrus.Debugln("checkStreamActiveDevice", stream.StreamID, stream.DeviceID)
			device, ok := _activeDevices.Get(stream.DeviceID)
			if !ok {
				continue
			}
			if device.source == nil {
				// logrus.Warningln("checkStreamDeviceSource is nil", stream.StreamID, stream.DeviceID)
				continue
			}
			// logrus.Debugln("checkStreamClosed", stream.StreamID, stream.DeviceID)
			// 关闭此流
			channel := Channels{ChannelID: stream.ChannelID}
			// if err := db.Get(db.DBClient, &channel); err != nil {
			// 	// logrus.Errorln("checkStreamGetchannelError", stream.StreamID, stream.ChannelID, err)
			// 	stream.Msg = err.Error()
			// 	db.Save(db.DBClient, stream)
			// 	channel = Channels{
			// 		ChannelID: stream.ChannelID,
			// 		DeviceID:  stream.DeviceID,
			// 		URIStr:    fmt.Sprintf("sip:%s@%s", stream.ChannelID, _serverDevices.Region),
			// 	}
			// }
			// channelURI, _ := sip.ParseURI(channel.URIStr)
			// channel.addr = &sip.Address{URI: channelURI, Params: sip.NewParams()}
			// for k, v := range stream.Ttag {
			// 	channel.addr.Params.Add(k, sip.String{Str: v.(string)})
			// }
			// for k, v := range stream.Ftag {
			// 	_serverDevices.addr.Params.Add(k, sip.String{Str: v.(string)})
			// }
			callid := sip.CallID(stream.CallID)
			cseq, err := stream.nextCSeq()
			if err != nil {
				stream.Msg = err.Error()
				continue
			}

			hb := sip.NewHeaderBuilder().SetToWithParam(channel.addr).SetFrom(_serverDevices.addr).AddVia(&sip.ViaHop{
				Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}),
			}).SetContentType(&sip.ContentTypeSDP).SetMethod(sip.MethodBYE).SetContact(_serverDevices.addr).SetCallID(&callid).SetSeqNo(uint(cseq)).SetXGBVerValue("1.0")
			req := sip.NewRequest("", sip.MethodBYE, channel.addr.URI, sip.DefaultSipVersion, hb.Build(), nil)
			req.SetDestination(device.source)
			req.SetRecipient(channel.addr.URI)

			// 不管成功不成功 程序都删除掉，后面开新流，关闭不成功的后面重试
			StreamList.Response.Delete(stream.StreamID)
			StreamList.Succ.Delete(stream.ChannelID)

			server := defaultSIPServer.Load()
			if server == nil {
				stream.Msg = "SIP server is unavailable"
				continue
			}
			tx, err := server.requestDialog(server.dialogTarget(stream.DeviceID, stream.ChannelID), req)
			if err != nil {
				// logrus.Warningln("checkStreamClosedFail", stream.StreamID, err)
				stream.Msg = err.Error()
				// db.Save(db.DBClient, stream)
				continue
			}
			response := tx.GetResponse()
			if response == nil {
				// logrus.Warningln("checkStreamClosedFail response is nil", channel.ChannelID, channel.DeviceID, stream.StreamID)
				continue
			}
			if response.StatusCode() != http.StatusOK {
				if response.StatusCode() == 481 {
					// logrus.Infoln("checkStreamClosedFail1", stream.StreamID, response.StatusCode())
					stream.Msg = response.Reason()
					stream.Status = 1
					stream.Stop = true
				} else {
					// logrus.Warningln("checkStreamClosedFail1", stream.StreamID, response.StatusCode())
					stream.Msg = response.Reason()
				}
			} else {
				stream.Status = 1
				stream.Stop = true
			}
			// db.Save(db.DBClient, stream)

		}
		if len(streams) != 100 {
			break
		}
		skip += 100
	}
}
