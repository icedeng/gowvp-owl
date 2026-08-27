package gbs

import (
	"context"
	"encoding/xml"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

type cancelableMutex struct {
	once  sync.Once
	token chan struct{}
}

type channelMediaLock struct {
	mutex cancelableMutex
	refs  int
}

func (m *cancelableMutex) init() {
	m.once.Do(func() {
		m.token = make(chan struct{}, 1)
		m.token <- struct{}{}
	})
}

func (m *cancelableMutex) Lock() {
	_ = m.LockContext(context.Background())
}

func (m *cancelableMutex) LockContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.init()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.token:
		if err := ctx.Err(); err != nil {
			m.token <- struct{}{}
			return err
		}
		return nil
	}
}

func (m *cancelableMutex) Unlock() {
	m.init()
	select {
	case m.token <- struct{}{}:
	default:
		panic("gbs: unlock of unlocked cancelable mutex")
	}
}

var (
	// sip服务用户信息
	_serverDevices Devices
)

type Device struct {
	Channels conc.Map[string, *Channel]

	registerWithKeepaliveMutex sync.Mutex
	mediaLockMu                sync.Mutex
	mediaLocks                 map[string]*channelMediaLock
	stateMu                    sync.RWMutex

	IsOnline    bool
	Address     string
	Password    string
	gbVersionMu sync.RWMutex
	gbVersion   string
	// gbDisabledCapabilities 保存设备固件级能力关闭项，与版本档案共同决定运行时门禁。
	gbDisabledCapabilities map[string]struct{}

	conn   sip.Connection
	source net.Addr
	to     *sip.Address

	LastKeepaliveAt time.Time
	LastRegisterAt  time.Time
	Expires         int

	keepaliveInterval uint16
	keepaliveTimeout  uint16
}

func (d *Device) lockMediaContext(ctx context.Context, channelID string) (func(), error) {
	if d == nil {
		return nil, fmt.Errorf("GB28181 device is unavailable")
	}
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return nil, fmt.Errorf("GB28181 media channel is unavailable")
	}
	d.mediaLockMu.Lock()
	if d.mediaLocks == nil {
		d.mediaLocks = make(map[string]*channelMediaLock)
	}
	entry := d.mediaLocks[channelID]
	if entry == nil {
		entry = &channelMediaLock{}
		d.mediaLocks[channelID] = entry
	}
	entry.refs++
	d.mediaLockMu.Unlock()
	if err := entry.mutex.LockContext(ctx); err != nil {
		d.releaseMediaLockRef(channelID, entry)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			d.releaseMediaLockRef(channelID, entry)
		})
	}, nil
}

func (d *Device) releaseMediaLockRef(channelID string, entry *channelMediaLock) {
	d.mediaLockMu.Lock()
	if current := d.mediaLocks[channelID]; current == entry {
		entry.refs--
		if entry.refs == 0 {
			delete(d.mediaLocks, channelID)
		}
	}
	d.mediaLockMu.Unlock()
}

type deviceRuntimeState struct {
	IsOnline          bool
	Address           string
	Password          string
	Conn              sip.Connection
	Source            net.Addr
	To                *sip.Address
	LastKeepaliveAt   time.Time
	LastRegisterAt    time.Time
	Expires           int
	KeepaliveInterval uint16
	KeepaliveTimeout  uint16
}

func (d *Device) runtimeSnapshot() deviceRuntimeState {
	if d == nil {
		return deviceRuntimeState{}
	}
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	state := deviceRuntimeState{
		IsOnline:          d.IsOnline,
		Address:           d.Address,
		Password:          d.Password,
		Conn:              d.conn,
		Source:            d.source,
		LastKeepaliveAt:   d.LastKeepaliveAt,
		LastRegisterAt:    d.LastRegisterAt,
		Expires:           d.Expires,
		KeepaliveInterval: d.keepaliveInterval,
		KeepaliveTimeout:  d.keepaliveTimeout,
	}
	if d.to != nil {
		state.To = d.to.Clone()
	}
	return state
}

// UpdateRuntime 串行更新设备的连接和在线运行态。
func (d *Device) UpdateRuntime(update func(*Device)) {
	if d == nil || update == nil {
		return
	}
	d.stateMu.Lock()
	update(d)
	d.stateMu.Unlock()
}

// SerializeRegistrationState 将同一设备的持久化与运行态变更串行化，避免 REGISTER、心跳和离线扫描交错提交。
func (d *Device) SerializeRegistrationState(change func() error) error {
	if change == nil {
		return nil
	}
	if d == nil {
		return change()
	}
	d.registerWithKeepaliveMutex.Lock()
	defer d.registerWithKeepaliveMutex.Unlock()
	return change()
}

// IsOnlineNow 返回并发安全的当前在线状态。
func (d *Device) IsOnlineNow() bool { return d.runtimeSnapshot().IsOnline }

// PasswordValue 返回并发安全的设备注册密码快照。
func (d *Device) PasswordValue() string { return d.runtimeSnapshot().Password }

func NewDevice(conn sip.Connection, d *ipc.Device) *Device {
	uri, err := sip.ParseURI(fmt.Sprintf("sip:%s@%s", d.GetGB28181DeviceID(), d.Address))
	if err != nil {
		slog.Error("parse uri", "err", err, "did", d.ID)
		return nil
	}

	addr, err := net.ResolveUDPAddr("udp", d.Address)
	if err != nil {
		slog.Error("resolve udp addr", "err", err, "did", d.ID)
		return nil
	}

	c := Device{
		conn:   conn,
		source: addr,
		to: &sip.Address{
			URI:    uri,
			Params: sip.NewParams(),
		},
		Address:                d.Address,
		LastKeepaliveAt:        d.KeepaliveAt.Time,
		LastRegisterAt:         d.RegisteredAt.Time,
		Expires:                d.Expires,
		IsOnline:               d.IsOnline,
		Password:               d.Password,
		gbVersion:              string(deviceProtocolVersion(d.Ext)),
		gbDisabledCapabilities: gbDisabledCapabilitySet(d.Ext.GBDisabledCapabilities),
	}

	return &c
}

// GBVersion 返回设备实际使用的附录 I 协议版本号（1.0/1.1/2.0/3.0）。
func (d *Device) GBVersion() string {
	d.gbVersionMu.RLock()
	defer d.gbVersionMu.RUnlock()
	return d.gbVersion
}

func (d *Device) setGBVersion(version GBProtocolVersion) {
	if version.Valid() {
		d.gbVersionMu.Lock()
		defer d.gbVersionMu.Unlock()
		d.gbVersion = string(version)
	}
}

func (d *Device) setGBProfile(version GBProtocolVersion, disabled []string) {
	if !version.Valid() {
		return
	}
	d.gbVersionMu.Lock()
	d.gbVersion = string(version)
	d.gbDisabledCapabilities = gbDisabledCapabilitySet(disabled)
	d.gbVersionMu.Unlock()
}

func (d *Device) isCapabilityDisabled(name string) bool {
	d.gbVersionMu.RLock()
	_, disabled := d.gbDisabledCapabilities[name]
	d.gbVersionMu.RUnlock()
	return disabled
}

func deviceProtocolVersion(ext ipc.DeviceExt) GBProtocolVersion {
	version, _ := resolveGBProtocolVersion(ext, "")
	return version
}

// CheckConnection 检查 udp 设备能否通信
func (d *Device) CheckConnection() error {
	const timeout = 2 * time.Second
	state := d.runtimeSnapshot()
	if state.Source == nil {
		return fmt.Errorf("设备连接地址不可用")
	}

	if state.Source.Network() == "tcp" {
		return nil
	}
	// 创建临时UDP连接进行检查
	tempConn, err := net.DialTimeout("udp", state.Source.String(), timeout)
	if err != nil {
		return fmt.Errorf("UDP连接失败: %w", err)
	}
	defer tempConn.Close()
	return nil
}

func (d *Device) LoadChannels(channels ...*ipc.Channel) {
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		ch := Channel{
			ChannelID: channel.ChannelID,
			device:    d,
		}
		if err := ch.init(d.Address); err != nil {
			slog.Warn("skip invalid persisted GB28181 channel", "channel_id", channel.ChannelID, "err", err)
			continue
		}
		d.Channels.Store(channel.ChannelID, &ch)
	}
}

// Conn implements Targeter.
func (d *Device) Conn() sip.Connection {
	return d.runtimeSnapshot().Conn
}

// Source implements Targeter.
func (d *Device) Source() net.Addr {
	return d.runtimeSnapshot().Source
}

// To implements Targeter.
func (d *Device) To() *sip.Address {
	return d.runtimeSnapshot().To
}

var _ Targeter = &Device{}

type Channel struct {
	ChannelID string

	uriStr string
	to     *sip.Address

	device *Device
}

// GBVersion 返回通道所属设备的国标版本。
func (c *Channel) GBVersion() string {
	if c.device == nil {
		return ""
	}
	return c.device.GBVersion()
}

// Conn implements Targeter.
func (c *Channel) Conn() sip.Connection {
	if c == nil || c.device == nil {
		return nil
	}
	return c.device.Conn()
}

// Source implements Targeter.
func (c *Channel) Source() net.Addr {
	if c == nil || c.device == nil {
		return nil
	}
	return c.device.Source()
}

// To implements Targeter.
func (c *Channel) To() *sip.Address {
	if c == nil {
		return nil
	}
	return c.to
}

var _ Targeter = &Channel{}

func (c *Channel) init(domain string) error {
	if c == nil {
		return fmt.Errorf("GB28181 channel is nil")
	}
	c.ChannelID = strings.TrimSpace(c.ChannelID)
	if c.ChannelID == "" {
		c.to = nil
		return fmt.Errorf("GB28181 channel code is empty")
	}
	c.uriStr = fmt.Sprintf("sip:%s@%s", c.ChannelID, domain)
	uri, err := sip.ParseURI(c.uriStr)
	if err != nil {
		c.to = nil
		return fmt.Errorf("parse GB28181 channel URI %q: %w", c.uriStr, err)
	}
	c.to = &sip.Address{
		URI:    uri,
		Params: sip.NewParams(),
	}
	return nil
}

func newDevice(network, address string, conn sip.Connection) *Device {
	if network == "tcp" {
		return nil
	}

	raddr, err := net.ResolveUDPAddr(network, address)
	if err != nil {
		return nil
	}

	var dev Device
	dev.source = raddr
	dev.conn = conn
	return &dev
}

// func NewClient() *Client {
// 	return &Client{
// 		devices: conc.Map[string, *Device]{},
// 	}
// }

// func (c *Client) Store(deviceID string, in *Device) {
// 	v, ok := c.devices.LoadOrStore(deviceID, in)
// 	if ok {
// 		v.conn = in.conn
// 		v.source = in.source
// 		v.to = in.to
// 		v.lastKeepaliveAt = in.lastKeepaliveAt
// 		v.lastRegisterAt = in.lastRegisterAt
// 	}
// }

// func (c *Client) Load(deviceID string) (*Device, bool) {
// 	return c.devices.Load(deviceID)
// }

func (c *Device) GetChannel(channelID string) (*Channel, bool) {
	return c.Channels.Load(channelID)
}

// func (c *Client) Delete(deviceID string) {
// 	c.devices.Delete(deviceID)
// }

// Devices NVR  设备信息
type Devices struct {
	// db.DBModel
	// Name 设备名称
	Name string `json:"name" gorm:"column:name" `
	// DeviceID 设备id
	DeviceID string `json:"deviceid" gorm:"column:deviceid"`
	// Region 设备域
	Region string `json:"region" gorm:"column:region"`
	// Host Via 地址
	Host string `json:"host" gorm:"column:host"`
	// Port via 端口
	Port string `json:"port" gorm:"column:port"`
	// TransPort via transport
	TransPort string `json:"transport" gorm:"column:transport"`
	// Proto 协议
	Proto string `json:"proto" gorm:"column:proto"`
	// Rport via rport
	Rport string `json:"report" gorm:"column:report"`
	// RAddr via recevied
	RAddr string `json:"raddr"  gorm:"column:raddr"`
	// Manufacturer 制造厂商
	Manufacturer string `xml:"Manufacturer"  json:"manufacturer"  gorm:"column:manufacturer"`
	// 设备类型DVR，NVR
	DeviceType string `xml:"DeviceType"  json:"devicetype"  gorm:"column:devicetype"`
	// Firmware 固件版本
	Firmware string ` json:"firmware"  gorm:"column:firmware"`
	// Model 型号
	Model  string `json:"model"  gorm:"column:model"`
	URIStr string `json:"uri"  gorm:"column:uri"`
	// ActiveAt 最后心跳检测时间
	ActiveAt int64 `json:"active" gorm:"column:active"`
	// Regist 是否注册
	Regist bool `json:"regist"  gorm:"column:regist"`
	// PWD 密码
	PWD string `json:"pwd" gorm:"column:pwd"`
	// Source
	Source string `json:"source"  gorm:"column:source"`

	Sys m.SysInfo `json:"sysinfo" gorm:"-"`

	//----
	addr   *sip.Address `gorm:"-"`
	source net.Addr     `gorm:"-"`

	Expire string `json:"-"`
}

// Channels 摄像头通道信息
type Channels struct {
	// db.DBModel
	// ChannelID 通道编码
	ChannelID string `xml:"DeviceID" json:"channelid" gorm:"column:channelid"`
	// DeviceID 设备编号
	DeviceID string `xml:"-" json:"deviceid"  gorm:"column:deviceid"`
	// Memo 备注（用来标示通道信息）
	MeMo string `json:"memo"  gorm:"column:memo"`
	// Name 通道名称（设备端设置名称）
	Name         string `xml:"Name" json:"name"  gorm:"column:name"`
	Manufacturer string `xml:"Manufacturer" json:"manufacturer"  gorm:"column:manufacturer"`
	Model        string `xml:"Model" json:"model"  gorm:"column:model"`
	Owner        string `xml:"Owner"  json:"owner"  gorm:"column:owner"`
	CivilCode    string `xml:"CivilCode" json:"civilcode"  gorm:"column:civilcode"`
	Block        string `xml:"Block" json:"block"`
	// Address ip地址
	Address           string `xml:"Address"  json:"address"  gorm:"column:address"`
	Parental          int    `xml:"Parental"  json:"parental"  gorm:"column:parental"`
	ParentID          string `xml:"ParentID" json:"parent_id"`
	SafetyWay         int    `xml:"SafetyWay"  json:"safetyway"  gorm:"column:safetyway"`
	RegisterWay       int    `xml:"RegisterWay"  json:"registerway"  gorm:"column:registerway"`
	CertNum           string `xml:"CertNum" json:"cert_num"`
	Certifiable       int    `xml:"Certifiable" json:"certifiable"`
	ErrCode           int    `xml:"ErrCode" json:"err_code"`
	EndTime           string `xml:"EndTime" json:"end_time"`
	SecurityLevelCode string `xml:"SecurityLevelCode" json:"security_level_code,omitempty"`
	Secrecy           int    `xml:"Secrecy" json:"secrecy"  gorm:"column:secrecy"`
	IPAddress         string `xml:"IPAddress" json:"ip_address"`
	Port              int    `xml:"Port" json:"port"`
	Password          string `xml:"Password" json:"-"`
	// Status 状态  on 在线
	Status    string  `xml:"Status"  json:"status"  gorm:"column:status"`
	Event     string  `xml:"Event" json:"event,omitempty" gorm:"-"`
	Longitude float64 `xml:"Longitude" json:"longitude"`
	Latitude  float64 `xml:"Latitude" json:"latitude"`
	// BusinessGroupID 在 2022 版从 Info 移到目录项外层。
	BusinessGroupID string          `xml:"BusinessGroupID" json:"business_group_id,omitempty"`
	Info            CatalogItemInfo `xml:"Info" json:"info"`
	RawXML          string          `xml:",innerxml" json:"-"`
	// Active 最后活跃时间
	Active int64  `json:"active"  gorm:"column:active"`
	URIStr string ` json:"uri"  gorm:"column:uri"`

	hasOwner             bool
	hasSafetyWay         bool
	hasCertNum           bool
	hasCertifiable       bool
	hasErrCode           bool
	hasEndTime           bool
	hasSecurityLevelCode bool
	hasBusinessGroupID   bool

	// 视频编码格式
	VF string ` json:"vf"  gorm:"column:vf"`
	// 视频高
	Height int `json:"height"  gorm:"column:height"`
	// 视频宽
	Width int `json:"width"  gorm:"column:width"`
	// 视频FPS
	FPS int `json:"fps"  gorm:"column:fps"`
	//  pull 媒体服务器主动拉流，push 监控设备主动推流
	StreamType string `json:"streamtype"  gorm:"column:streamtype"`
	// streamtype=pull时，拉流地址
	URL string `json:"url"  gorm:"column:url"`

	addr *sip.Address `gorm:"-"`
}

func (c *Channels) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	type channelsAlias Channels
	var value struct {
		channelsAlias
		Owner             *string `xml:"Owner"`
		SafetyWay         *int    `xml:"SafetyWay"`
		CertNum           *string `xml:"CertNum"`
		Certifiable       *int    `xml:"Certifiable"`
		ErrCode           *int    `xml:"ErrCode"`
		EndTime           *string `xml:"EndTime"`
		SecurityLevelCode *string `xml:"SecurityLevelCode"`
		BusinessGroupID   *string `xml:"BusinessGroupID"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*c = Channels(value.channelsAlias)
	if value.Owner != nil {
		c.Owner, c.hasOwner = *value.Owner, true
	}
	if value.SafetyWay != nil {
		c.SafetyWay, c.hasSafetyWay = *value.SafetyWay, true
	}
	if value.CertNum != nil {
		c.CertNum, c.hasCertNum = *value.CertNum, true
	}
	if value.Certifiable != nil {
		c.Certifiable, c.hasCertifiable = *value.Certifiable, true
	}
	if value.ErrCode != nil {
		c.ErrCode, c.hasErrCode = *value.ErrCode, true
	}
	if value.EndTime != nil {
		c.EndTime, c.hasEndTime = *value.EndTime, true
	}
	if value.SecurityLevelCode != nil {
		c.SecurityLevelCode, c.hasSecurityLevelCode = *value.SecurityLevelCode, true
	}
	if value.BusinessGroupID != nil {
		c.BusinessGroupID, c.hasBusinessGroupID = *value.BusinessGroupID, true
	}
	return nil
}

// CatalogItemInfo 是 2014 修改补充文件新增的目录项摄像机属性。
type CatalogItemInfo struct {
	XMLName                  xml.Name `xml:"Info" json:"-"`
	PTZType                  int      `xml:"PTZType" json:"ptz_type"`
	PTZTypeList              string   `xml:"-" json:"ptz_type_list,omitempty"`
	PhotoelectricImagingType string   `xml:"PhotoelectricImagingType" json:"photoelectric_imaging_type,omitempty"`
	CapturePositionType      string   `xml:"CapturePositionType" json:"capture_position_type,omitempty"`
	PositionType             int      `xml:"PositionType" json:"position_type"`
	RoomType                 int      `xml:"RoomType" json:"room_type"`
	UseType                  int      `xml:"UseType" json:"use_type"`
	SupplyLightType          int      `xml:"SupplyLightType" json:"supply_light_type"`
	DirectionType            int      `xml:"DirectionType" json:"direction_type"`
	Resolution               string   `xml:"Resolution" json:"resolution"`
	StreamNumberList         string   `xml:"StreamNumberList" json:"stream_number_list,omitempty"`
	DownloadSpeed            string   `xml:"DownloadSpeed" json:"download_speed,omitempty"`
	SVCSpaceSupportMode      int      `xml:"SVCSpaceSupportMode" json:"svc_space_support_mode,omitempty"`
	SVCTimeSupportMode       int      `xml:"SVCTimeSupportMode" json:"svc_time_support_mode,omitempty"`
	SSVCRatioSupportList     string   `xml:"SSVCRatioSupportList" json:"ssvc_ratio_support_list,omitempty"`
	MobileDeviceType         int      `xml:"MobileDeviceType" json:"mobile_device_type,omitempty"`
	HorizontalFieldAngle     float64  `xml:"HorizontalFieldAngle" json:"horizontal_field_angle,omitempty"`
	VerticalFieldAngle       float64  `xml:"VerticalFieldAngle" json:"vertical_field_angle,omitempty"`
	MaxViewDistance          float64  `xml:"MaxViewDistance" json:"max_view_distance,omitempty"`
	GrassrootsCode           string   `xml:"GrassrootsCode" json:"grassroots_code,omitempty"`
	PointType                int      `xml:"PointType" json:"point_type,omitempty"`
	PointCommonName          string   `xml:"PointCommonName" json:"point_common_name,omitempty"`
	MAC                      string   `xml:"MAC" json:"mac,omitempty"`
	FunctionType             string   `xml:"FunctionType" json:"function_type,omitempty"`
	EncodeType               string   `xml:"EncodeType" json:"encode_type,omitempty"`
	InstallTime              string   `xml:"InstallTime" json:"install_time,omitempty"`
	ManagementUnit           string   `xml:"ManagementUnit" json:"management_unit,omitempty"`
	ContactInfo              string   `xml:"ContactInfo" json:"contact_info,omitempty"`
	RecordSaveDays           int      `xml:"RecordSaveDays" json:"record_save_days,omitempty"`
	IndustrialClassification string   `xml:"IndustrialClassification" json:"industrial_classification,omitempty"`
	BusinessGroupID          string   `xml:"BusinessGroupID" json:"business_group_id"`
	RawXML                   string   `xml:",innerxml" json:"-"`
	hasPositionType          bool
	hasUseType               bool
	hasBusinessGroupID       bool
}

func (i *CatalogItemInfo) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var value struct {
		PTZType                  string  `xml:"PTZType"`
		PhotoelectricImagingType string  `xml:"PhotoelectricImagingType"`
		CapturePositionType      string  `xml:"CapturePositionType"`
		PositionType             *int    `xml:"PositionType"`
		RoomType                 int     `xml:"RoomType"`
		UseType                  *int    `xml:"UseType"`
		SupplyLightType          int     `xml:"SupplyLightType"`
		DirectionType            int     `xml:"DirectionType"`
		Resolution               string  `xml:"Resolution"`
		StreamNumberList         string  `xml:"StreamNumberList"`
		DownloadSpeed            string  `xml:"DownloadSpeed"`
		SVCSpaceSupportMode      int     `xml:"SVCSpaceSupportMode"`
		SVCTimeSupportMode       int     `xml:"SVCTimeSupportMode"`
		SSVCRatioSupportList     string  `xml:"SSVCRatioSupportList"`
		MobileDeviceType         int     `xml:"MobileDeviceType"`
		HorizontalFieldAngle     float64 `xml:"HorizontalFieldAngle"`
		VerticalFieldAngle       float64 `xml:"VerticalFieldAngle"`
		MaxViewDistance          float64 `xml:"MaxViewDistance"`
		GrassrootsCode           string  `xml:"GrassrootsCode"`
		PointType                int     `xml:"PointType"`
		PointCommonName          string  `xml:"PointCommonName"`
		MAC                      string  `xml:"MAC"`
		FunctionType             string  `xml:"FunctionType"`
		EncodeType               string  `xml:"EncodeType"`
		InstallTime              string  `xml:"InstallTime"`
		ManagementUnit           string  `xml:"ManagementUnit"`
		ContactInfo              string  `xml:"ContactInfo"`
		RecordSaveDays           int     `xml:"RecordSaveDays"`
		IndustrialClassification string  `xml:"IndustrialClassification"`
		BusinessGroupID          *string `xml:"BusinessGroupID"`
		RawXML                   string  `xml:",innerxml"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*i = CatalogItemInfo{
		XMLName:                  start.Name,
		PTZTypeList:              strings.TrimSpace(value.PTZType),
		PhotoelectricImagingType: strings.TrimSpace(value.PhotoelectricImagingType),
		CapturePositionType:      strings.TrimSpace(value.CapturePositionType),
		RoomType:                 value.RoomType,
		SupplyLightType:          value.SupplyLightType,
		DirectionType:            value.DirectionType,
		Resolution:               value.Resolution,
		StreamNumberList:         strings.TrimSpace(value.StreamNumberList),
		DownloadSpeed:            strings.TrimSpace(value.DownloadSpeed),
		SVCSpaceSupportMode:      value.SVCSpaceSupportMode,
		SVCTimeSupportMode:       value.SVCTimeSupportMode,
		SSVCRatioSupportList:     strings.TrimSpace(value.SSVCRatioSupportList),
		MobileDeviceType:         value.MobileDeviceType,
		HorizontalFieldAngle:     value.HorizontalFieldAngle,
		VerticalFieldAngle:       value.VerticalFieldAngle,
		MaxViewDistance:          value.MaxViewDistance,
		GrassrootsCode:           strings.TrimSpace(value.GrassrootsCode),
		PointType:                value.PointType,
		PointCommonName:          strings.TrimSpace(value.PointCommonName),
		MAC:                      strings.TrimSpace(value.MAC),
		FunctionType:             strings.TrimSpace(value.FunctionType),
		EncodeType:               strings.TrimSpace(value.EncodeType),
		InstallTime:              strings.TrimSpace(value.InstallTime),
		ManagementUnit:           strings.TrimSpace(value.ManagementUnit),
		ContactInfo:              strings.TrimSpace(value.ContactInfo),
		RecordSaveDays:           value.RecordSaveDays,
		IndustrialClassification: strings.TrimSpace(value.IndustrialClassification),
		RawXML:                   value.RawXML,
	}
	if value.PositionType != nil {
		i.PositionType, i.hasPositionType = *value.PositionType, true
	}
	if value.UseType != nil {
		i.UseType, i.hasUseType = *value.UseType, true
	}
	if value.BusinessGroupID != nil {
		i.BusinessGroupID, i.hasBusinessGroupID = *value.BusinessGroupID, true
	}
	if i.PTZTypeList == "" {
		return nil
	}
	first, _, _ := strings.Cut(i.PTZTypeList, "/")
	ptzType, err := strconv.Atoi(strings.TrimSpace(first))
	if err != nil {
		return fmt.Errorf("invalid Catalog PTZType: %w", err)
	}
	i.PTZType = ptzType
	return nil
}

// 同步摄像头编码格式
func SyncDevicesCodec(ssrc, deviceid string) {
	resp := zlmGetMediaList(zlmGetMediaListReq{streamID: ssrc})
	if resp.Code != 0 {
		// logrus.Errorln("syncDevicesCodec fail", ssrc, resp)
		return
	}
	if len(resp.Data) == 0 {
		// logrus.Errorln("syncDevicesCodec fail", ssrc, "not found data", resp)
		return
	}
	for _, data := range resp.Data {
		if len(data.Tracks) == 0 {
			// logrus.Errorln("syncDevicesCodec fail", ssrc, "not found tracks", resp)
		}

		for _, track := range data.Tracks {
			if track.Type == 0 {
				// 视频
				// device := Channels{DeviceID: deviceid}
				// if err := db.Get(db.DBClient, &device); err == nil {
				// 	device.VF = transZLMDeviceVF(track.CodecID)
				// 	device.Height = track.Height
				// 	device.Width = track.Width
				// 	device.FPS = track.FPS
				// 	db.Save(db.DBClient, &device)
				// } else {
				// 	// logrus.Errorln("syncDevicesCodec deviceid not found,deviceid:", deviceid)
				// }
			}
		}
	}
}

// 从请求中解析出设备信息
func parserDevicesFromReqeust(req *sip.Request) (Devices, bool) {
	u := Devices{}
	header, ok := req.From()
	if !ok {
		// logrus.Warningln("not found from header from request", req.String())
		return u, false
	}
	if header.Address == nil {
		// logrus.Warningln("not found from user from request", req.String())
		return u, false
	}
	if header.Address.User() == nil {
		// logrus.Warningln("not found from user from request", req.String())
		return u, false
	}
	u.DeviceID = header.Address.User().String()
	u.Region = header.Address.Host()
	via, ok := req.ViaHop()
	if !ok {
		// logrus.Info("not found ViaHop from request", req.String())
		return u, false
	}
	u.Host = via.Host
	u.Port = via.Port.String()
	report, ok := via.Params.Get("rport")
	if ok && report != nil {
		u.Rport = report.String()
	}
	raddr, ok := via.Params.Get("received")
	if ok && raddr != nil {
		u.RAddr = raddr.String()
	}

	u.TransPort = via.Transport
	u.URIStr = header.Address.String()
	u.addr = sip.NewAddressFromFromHeader(header)
	u.Source = req.Source().String()
	u.source = req.Source()

	headers := req.GetHeaders("Expires")
	if len(headers) != 0 {
		header := headers[0]
		splits := strings.Split(header.String(), ":")
		if len(splits) == 2 {
			u.Expire = splits[1][1:]
		}
	}

	return u, true
}

var deviceStatusMap = map[string]string{
	"ON":     m.DeviceStatusON,
	"OK":     m.DeviceStatusON,
	"ONLINE": m.DeviceStatusON,
	"OFFILE": m.DeviceStatusOFF,
	"OFF":    m.DeviceStatusOFF,
}

func transDeviceStatus(status string) string {
	if v, ok := deviceStatusMap[status]; ok {
		return v
	}
	return status
}
