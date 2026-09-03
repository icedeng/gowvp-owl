package gbs

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
)

const cascadeCatalogChunkSize = 20

type cascadeQueryEnvelope struct {
	XMLName         xml.Name
	CmdType         string `xml:"CmdType"`
	SN              int    `xml:"SN"`
	DeviceID        string `xml:"DeviceID"`
	SourceID        string `xml:"SourceID"`
	TargetID        string `xml:"TargetID"`
	StartTime       string `xml:"StartTime"`
	EndTime         string `xml:"EndTime"`
	FilePath        string `xml:"FilePath"`
	Address         string `xml:"Address"`
	Secrecy         *int   `xml:"Secrecy"`
	Type            string `xml:"Type"`
	RecorderID      string `xml:"RecorderID"`
	IndistinctQuery *int   `xml:"IndistinctQuery"`
	// DistinctQuery 兼容此前错误发布的 2022 字段名；标准字段始终为 IndistinctQuery。
	DistinctQuery      *int   `xml:"DistinctQuery"`
	StreamNumber       *int   `xml:"StreamNumber"`
	AlarmMethod        string `xml:"AlarmMethod"`
	AlarmType          string `xml:"AlarmType"`
	StartAlarmPriority string `xml:"StartAlarmPriority"`
	EndAlarmPriority   string `xml:"EndAlarmPriority"`
	StartAlarmTime     string `xml:"StartAlarmTime"`
	EndAlarmTime       string `xml:"EndAlarmTime"`
	Interval           int    `xml:"Interval"`
	Number             *int   `xml:"Number"`
	ConfigType         string `xml:"ConfigType"`
	// recordQueryLocationID 保存 SIP To URI 的用户编码，仅用于 2022 RecordInfo 查询源选择。
	recordQueryLocationID string `xml:"-"`
}

type cascadeCatalogResponse struct {
	XMLName    xml.Name                  `xml:"Response"`
	CmdType    string                    `xml:"CmdType"`
	SN         int                       `xml:"SN"`
	DeviceID   string                    `xml:"DeviceID"`
	SumNum     int                       `xml:"SumNum"`
	DeviceList *cascadeCatalogDeviceList `xml:"DeviceList,omitempty"`
}

type cascadeCatalogNotify struct {
	XMLName    xml.Name
	CmdType    string                   `xml:"CmdType"`
	SN         int                      `xml:"SN"`
	DeviceID   string                   `xml:"DeviceID"`
	Status     string                   `xml:"Status,omitempty"`
	SumNum     int                      `xml:"SumNum"`
	DeviceList cascadeCatalogDeviceList `xml:"DeviceList"`
}

type cascadeCatalogDeviceList struct {
	Num   int                  `xml:"Num,attr"`
	Items []cascadeCatalogItem `xml:"Item"`
}

type cascadeCatalogItem struct {
	protocolVersion   GBProtocolVersion   `xml:"-"`
	DeviceID          string              `xml:"DeviceID"`
	Name              string              `xml:"Name"`
	Manufacturer      string              `xml:"Manufacturer"`
	Model             string              `xml:"Model"`
	Owner             *string             `xml:"Owner,omitempty"`
	CivilCode         string              `xml:"CivilCode"`
	Block             string              `xml:"Block"`
	Address           string              `xml:"Address"`
	Parental          int                 `xml:"Parental"`
	ParentID          string              `xml:"ParentID,omitempty"`
	SafetyWay         *int                `xml:"SafetyWay,omitempty"`
	RegisterWay       int                 `xml:"RegisterWay"`
	CertNum           *string             `xml:"CertNum,omitempty"`
	Certifiable       *int                `xml:"Certifiable,omitempty"`
	ErrCode           *int                `xml:"ErrCode,omitempty"`
	EndTime           *string             `xml:"EndTime,omitempty"`
	SecurityLevelCode string              `xml:"SecurityLevelCode,omitempty"`
	Secrecy           int                 `xml:"Secrecy"`
	IPAddress         string              `xml:"IPAddress"`
	Port              int                 `xml:"Port"`
	Status            string              `xml:"Status"`
	Event             string              `xml:"Event,omitempty"`
	Longitude         float64             `xml:"Longitude"`
	Latitude          float64             `xml:"Latitude"`
	BusinessGroupID   string              `xml:"BusinessGroupID,omitempty"`
	Info              *cascadeCatalogInfo `xml:"Info,omitempty"`
	ExtraInfo         []string            `xml:"ExtraInfo,omitempty"`
}

// MarshalXML 按 2022 附录 J 的目录项类别裁剪条件字段。2011/2014/2016
// 保持既有固定字段序列，兼容仍按旧版 XML 模板解析的设备和平台。
func (item cascadeCatalogItem) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	type legacyCatalogItem cascadeCatalogItem
	if item.protocolVersion != GBVersion30 {
		return encoder.EncodeElement(legacyCatalogItem(item), start)
	}

	type catalog30Common struct {
		DeviceID string `xml:"DeviceID"`
		Name     string `xml:"Name"`
	}
	type catalog30Administrative struct {
		catalog30Common
		Event string `xml:"Event,omitempty"`
	}
	type catalog30System struct {
		catalog30Common
		Manufacturer string `xml:"Manufacturer"`
		Model        string `xml:"Model"`
		CivilCode    string `xml:"CivilCode"`
		Address      string `xml:"Address"`
		ParentID     string `xml:"ParentID,omitempty"`
		RegisterWay  int    `xml:"RegisterWay"`
		Secrecy      int    `xml:"Secrecy"`
		IPAddress    string `xml:"IPAddress,omitempty"`
		Port         int    `xml:"Port,omitempty"`
		Status       string `xml:"Status"`
		Event        string `xml:"Event,omitempty"`
		// ExtraInfo 是普通 string；去掉 omitempty 以保留标准允许的空节点。
		ExtraInfo []string `xml:"ExtraInfo"`
	}
	type catalog30BusinessGroup struct {
		catalog30Common
		CivilCode string `xml:"CivilCode"`
		ParentID  string `xml:"ParentID"`
		Event     string `xml:"Event,omitempty"`
	}
	type catalog30VirtualOrganization struct {
		catalog30Common
		ParentID        string `xml:"ParentID,omitempty"`
		BusinessGroupID string `xml:"BusinessGroupID"`
		Event           string `xml:"Event,omitempty"`
	}
	type catalog30Device struct {
		catalog30Common
		Manufacturer      string              `xml:"Manufacturer"`
		Model             string              `xml:"Model"`
		CivilCode         string              `xml:"CivilCode"`
		Block             string              `xml:"Block,omitempty"`
		Address           string              `xml:"Address"`
		Parental          int                 `xml:"Parental"`
		ParentID          string              `xml:"ParentID"`
		RegisterWay       int                 `xml:"RegisterWay"`
		SecurityLevelCode string              `xml:"SecurityLevelCode,omitempty"`
		Secrecy           int                 `xml:"Secrecy"`
		IPAddress         string              `xml:"IPAddress,omitempty"`
		Port              int                 `xml:"Port,omitempty"`
		Status            string              `xml:"Status"`
		Longitude         *float64            `xml:"Longitude,omitempty"`
		Latitude          *float64            `xml:"Latitude,omitempty"`
		BusinessGroupID   string              `xml:"BusinessGroupID,omitempty"`
		Info              *cascadeCatalogInfo `xml:"Info,omitempty"`
		Event             string              `xml:"Event,omitempty"`
		// ExtraInfo 是普通 string；去掉 omitempty 以保留标准允许的空节点。
		ExtraInfo []string `xml:"ExtraInfo"`
	}

	common := catalog30Common{DeviceID: item.DeviceID, Name: item.Name}
	switch classifyGBCatalogItem(item.DeviceID) {
	case GBCatalogItemAdministrative:
		return encoder.EncodeElement(catalog30Administrative{catalog30Common: common, Event: item.Event}, start)
	case GBCatalogItemSystem:
		return encoder.EncodeElement(catalog30System{
			catalog30Common: common, Manufacturer: item.Manufacturer, Model: item.Model,
			CivilCode: item.CivilCode, Address: item.Address, ParentID: item.ParentID,
			RegisterWay: item.RegisterWay, Secrecy: item.Secrecy, IPAddress: item.IPAddress,
			Port: item.Port, Status: item.Status, Event: item.Event, ExtraInfo: item.ExtraInfo,
		}, start)
	case GBCatalogItemBusinessGroup:
		return encoder.EncodeElement(catalog30BusinessGroup{
			catalog30Common: common, CivilCode: item.CivilCode, ParentID: item.ParentID, Event: item.Event,
		}, start)
	case GBCatalogItemVirtualOrganization:
		return encoder.EncodeElement(catalog30VirtualOrganization{
			catalog30Common: common, ParentID: item.ParentID, BusinessGroupID: item.BusinessGroupID, Event: item.Event,
		}, start)
	default:
		var longitude, latitude *float64
		if item.Info != nil && (item.Info.PointType == 1 || item.Info.PointType == 2) {
			longitude, latitude = &item.Longitude, &item.Latitude
		}
		return encoder.EncodeElement(catalog30Device{
			catalog30Common: common, Manufacturer: item.Manufacturer, Model: item.Model,
			CivilCode: item.CivilCode, Block: item.Block, Address: item.Address,
			Parental: item.Parental, ParentID: item.ParentID, RegisterWay: item.RegisterWay,
			SecurityLevelCode: item.SecurityLevelCode, Secrecy: item.Secrecy,
			IPAddress: item.IPAddress, Port: item.Port, Status: item.Status,
			Longitude: longitude, Latitude: latitude, BusinessGroupID: item.BusinessGroupID,
			Info: item.Info, Event: item.Event, ExtraInfo: item.ExtraInfo,
		}, start)
	}
}

type cascadeCatalogInfo struct {
	PTZType                  string  `xml:"PTZType,omitempty"`
	PhotoelectricImagingType string  `xml:"PhotoelectricImagingType,omitempty"`
	CapturePositionType      string  `xml:"CapturePositionType,omitempty"`
	PositionType             int     `xml:"PositionType,omitempty"`
	RoomType                 int     `xml:"RoomType,omitempty"`
	UseType                  int     `xml:"UseType,omitempty"`
	SupplyLightType          int     `xml:"SupplyLightType,omitempty"`
	DirectionType            int     `xml:"DirectionType,omitempty"`
	Resolution               string  `xml:"Resolution,omitempty"`
	StreamNumberList         string  `xml:"StreamNumberList,omitempty"`
	DownloadSpeed            string  `xml:"DownloadSpeed,omitempty"`
	SVCSpaceSupportMode      int     `xml:"SVCSpaceSupportMode,omitempty"`
	SVCTimeSupportMode       int     `xml:"SVCTimeSupportMode,omitempty"`
	SSVCRatioSupportList     string  `xml:"SSVCRatioSupportList,omitempty"`
	MobileDeviceType         int     `xml:"MobileDeviceType,omitempty"`
	HorizontalFieldAngle     float64 `xml:"HorizontalFieldAngle,omitempty"`
	VerticalFieldAngle       float64 `xml:"VerticalFieldAngle,omitempty"`
	MaxViewDistance          float64 `xml:"MaxViewDistance,omitempty"`
	GrassrootsCode           string  `xml:"GrassrootsCode,omitempty"`
	PointType                int     `xml:"PointType,omitempty"`
	PointCommonName          string  `xml:"PointCommonName,omitempty"`
	MAC                      string  `xml:"MAC,omitempty"`
	FunctionType             string  `xml:"FunctionType,omitempty"`
	EncodeType               string  `xml:"EncodeType,omitempty"`
	InstallTime              string  `xml:"InstallTime,omitempty"`
	ManagementUnit           string  `xml:"ManagementUnit,omitempty"`
	ContactInfo              *string `xml:"ContactInfo,omitempty"`
	RecordSaveDays           *int    `xml:"RecordSaveDays,omitempty"`
	IndustrialClassification string  `xml:"IndustrialClassification,omitempty"`
	BusinessGroupID          string  `xml:"BusinessGroupID,omitempty"`
}

type cascadeDeviceInfoResponse struct {
	XMLName      xml.Name `xml:"Response"`
	CmdType      string   `xml:"CmdType"`
	SN           int      `xml:"SN"`
	DeviceID     string   `xml:"DeviceID"`
	DeviceName   string   `xml:"DeviceName,omitempty"`
	Result       string   `xml:"Result"`
	DeviceType   *string  `xml:"DeviceType,omitempty"`
	Manufacturer string   `xml:"Manufacturer"`
	Model        string   `xml:"Model"`
	Firmware     string   `xml:"Firmware"`
	MaxCamera    *int     `xml:"MaxCamera,omitempty"`
	MaxAlarm     *int     `xml:"MaxAlarm,omitempty"`
	Channel      int      `xml:"Channel"`
}

func cascadeDeviceInfoName(version GBProtocolVersion, name string) string {
	if !version.AtLeast(GBVersion11) {
		return ""
	}
	return name
}

func applyCascadeDeviceInfoCompatibility(response *cascadeDeviceInfoResponse, version GBProtocolVersion, deviceType string, maxCamera, maxAlarm int) {
	if response == nil || version == GBVersion30 {
		return
	}
	response.DeviceType = &deviceType
	response.MaxCamera = &maxCamera
	response.MaxAlarm = &maxAlarm
}

type cascadeDeviceStatusResponse struct {
	XMLName    xml.Name           `xml:"Response"`
	CmdType    string             `xml:"CmdType"`
	SN         int                `xml:"SN"`
	DeviceID   string             `xml:"DeviceID"`
	Result     string             `xml:"Result"`
	Online     string             `xml:"Online"`
	Status     string             `xml:"Status"`
	Encode     string             `xml:"Encode"`
	Record     string             `xml:"Record"`
	DeviceTime string             `xml:"DeviceTime"`
	Alarm      cascadeAlarmStatus `xml:"Alarmstatus"`
}

type cascadeAlarmStatus struct {
	Num      *int `xml:"Num,attr,omitempty"`
	LowerNum *int `xml:"num,attr,omitempty"`
}

func cascadeAlarmStatusForVersion(version GBProtocolVersion, count int) cascadeAlarmStatus {
	if version == GBVersion20 {
		return cascadeAlarmStatus{LowerNum: &count}
	}
	return cascadeAlarmStatus{Num: &count}
}

type cascadeQueryErrorResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Result   string   `xml:"Result"`
}

type cascadeQueryBaseResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
}

type cascadeQueryCountResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Number   *int     `xml:"Number,omitempty"`
	SumNum   int      `xml:"SumNum"`
}

type cascadePresetQueryErrorResponse struct {
	XMLName    xml.Name                    `xml:"Response"`
	CmdType    string                      `xml:"CmdType"`
	SN         int                         `xml:"SN"`
	DeviceID   string                      `xml:"DeviceID"`
	SumNum     *int                        `xml:"SumNum,omitempty"`
	PresetList cascadePresetQueryErrorList `xml:"PresetList"`
}

type cascadePresetQueryErrorList struct {
	Num int `xml:"Num,attr"`
}

type cascadeDeviceStatusErrorResponse struct {
	XMLName  xml.Name `xml:"Response"`
	CmdType  string   `xml:"CmdType"`
	SN       int      `xml:"SN"`
	DeviceID string   `xml:"DeviceID"`
	Result   string   `xml:"Result"`
	Online   string   `xml:"Online"`
	Status   string   `xml:"Status"`
}

type cascadeRecordInfoResponse struct {
	XMLName    xml.Name               `xml:"Response"`
	CmdType    string                 `xml:"CmdType"`
	SN         int                    `xml:"SN"`
	DeviceID   string                 `xml:"DeviceID"`
	Name       string                 `xml:"Name"`
	SumNum     int                    `xml:"SumNum"`
	RecordList *cascadeRecordInfoList `xml:"RecordList,omitempty"`
	ExtraInfo  []string               `xml:"ExtraInfo,omitempty"`
}

type cascadeRecordInfoList struct {
	Num   int          `xml:"Num,attr"`
	Items []RecordItem `xml:"Item"`
}

func (g *GB28181API) sipCascadeMessageMiddleware(ctx *sip.Context) {
	value, ok := ctx.Get(cascadeWorkerContextKey)
	if !ok {
		ctx.Next()
		return
	}
	worker, ok := value.(*cascadeWorker)
	if !ok || worker == nil {
		ctx.AbortString(403, "invalid cascade peer")
		return
	}
	var query cascadeQueryEnvelope
	if err := sip.XMLDecode(ctx.Request.Body(), &query); err != nil {
		ctx.AbortString(400, ErrXMLDecode.Error())
		return
	}
	if query.SN <= 0 {
		ctx.AbortString(400, "invalid cascade query")
		return
	}
	query.CmdType = canonicalGBQueryCmdType(query.CmdType)
	if query.XMLName.Local == "Response" && strings.EqualFold(query.CmdType, "Alarm") {
		g.handleCascadeAlarmBusinessResponse(ctx, worker)
		return
	}
	if query.XMLName.Local == "Notify" && query.CmdType == "Broadcast" {
		if err := validateBroadcastNotifyStructure(ctx.Request.Body()); err != nil {
			ctx.AbortString(400, err.Error())
			return
		}
		if filterUnknowDevices(strings.TrimSpace(query.SourceID)) != nil || !cascadeBroadcastTargetAllowed(worker.platform, query.TargetID) {
			ctx.AbortString(404, "cascade Broadcast target not found")
			return
		}
		if recipient := ctx.Request.Recipient(); recipient != nil && recipient.User() != nil {
			requestedID := strings.TrimSpace(recipient.User().String())
			if requestedID != "" && requestedID != strings.TrimSpace(query.TargetID) && requestedID != worker.platform.localID {
				ctx.AbortString(400, "cascade Broadcast target mismatch")
				return
			}
		}
		if !acknowledgeCascadeRequest(ctx, query.CmdType, query.SN) {
			return
		}
		body := query
		identityCtx := monitorUserIdentityContext(ctx)
		g.startCascadeLifecycleTask(identityCtx, worker, func(taskCtx context.Context) {
			if err := g.forwardCascadeBroadcast(taskCtx, worker, body); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("forward cascade Broadcast failed", "upstream", worker.platform.name, "sn", body.SN, "err", err)
			}
		})
		return
	}
	if strings.TrimSpace(query.DeviceID) == "" {
		ctx.AbortString(400, "invalid cascade query")
		return
	}
	if query.XMLName.Local == "Control" {
		if query.CmdType == "DeviceConfig" {
			if err := validateDeviceConfigRequestStructure(ctx.Request.Body(), worker.protocolVersion()); err != nil {
				ctx.AbortString(400, err.Error())
				return
			}
			var request DeviceConfigRequest
			if err := sip.XMLDecode(ctx.Request.Body(), &request); err != nil {
				ctx.AbortString(400, ErrXMLDecode.Error())
				return
			}
			request.CmdType = strings.TrimSpace(request.CmdType)
			request.DeviceID = strings.TrimSpace(request.DeviceID)
			if err := validateCascadeDeviceConfigPayload(&request, worker.protocolVersion()); err != nil {
				ctx.AbortString(400, err.Error())
				return
			}
			if worker.platform.exposedChannelMap[request.DeviceID] == "" {
				ctx.AbortString(404, "cascade config target not found")
				return
			}
			if !acknowledgeCascadeRequest(ctx, query.CmdType, query.SN) {
				return
			}
			body := append([]byte(nil), ctx.Request.Body()...)
			identityCtx := monitorUserIdentityContext(ctx)
			g.startCascadeLifecycleTask(identityCtx, worker, func(taskCtx context.Context) {
				g.forwardCascadeDeviceConfig(worker, body, taskCtx)
			})
			return
		}
		if query.CmdType != ptzCmdTypeDeviceControl {
			ctx.AbortString(404, "cascade control target not found")
			return
		}
		if err := validateDeviceControlRequestStructure(ctx.Request.Body(), worker.protocolVersion()); err != nil {
			ctx.AbortString(400, err.Error())
			return
		}
		if worker.platform.exposedChannelMap[query.DeviceID] == "" {
			ctx.AbortString(404, "cascade control target not found")
			return
		}
		var request deviceControlA23Request
		if err := decodeDeviceControlRequest(ctx.Request.Body(), &request); err != nil {
			ctx.AbortString(400, ErrXMLDecode.Error())
			return
		}
		validationParent := monitorUserIdentityContextWithParent(g.initializedServiceContext(), ctx)
		validationCtx, cancel := context.WithTimeout(validationParent, defaultCascadeRequestTimeout)
		channel, err := g.loadCascadeExposedChannel(validationCtx, worker.platform, request.DeviceID)
		if err != nil {
			cancel()
			ctx.AbortString(404, "cascade control target not found")
			return
		}
		downstreamVersion := GBVersion10
		if g.svr != nil && g.svr.memoryStorer != nil {
			downstreamVersion = g.getDeviceGBProtocolVersion(channel.DeviceID)
		}
		if err = validateCascadeDeviceControl(&request, worker.protocolVersion(), downstreamVersion); err != nil {
			cancel()
			ctx.AbortString(400, err.Error())
			return
		}
		if err = g.validateCascadeDeviceControlOverrides(channel.DeviceID, &request); err != nil {
			cancel()
			ctx.AbortString(400, err.Error())
			return
		}
		if _, err = g.resolveCascadeTargetTrackDeviceID2(validationCtx, worker.platform, channel, &request); err != nil {
			cancel()
			ctx.AbortString(400, err.Error())
			return
		}
		cancel()
		if !acknowledgeCascadeRequest(ctx, query.CmdType, query.SN) {
			return
		}
		body := append([]byte(nil), ctx.Request.Body()...)
		identityCtx := monitorUserIdentityContext(ctx)
		g.startCascadeLifecycleTask(identityCtx, worker, func(taskCtx context.Context) {
			g.forwardCascadeDeviceControl(worker, body, taskCtx)
		})
		return
	}
	if query.XMLName.Local != "Query" {
		ctx.AbortString(400, "invalid cascade query")
		return
	}
	if err := validateCascadeQueryRequestStructure(ctx.Request.Body(), query.CmdType); err != nil {
		ctx.AbortString(400, err.Error())
		return
	}
	if err := validateCascadeQueryRequest(query); err != nil {
		ctx.AbortString(400, err.Error())
		return
	}
	if err := validateCascadeQueryVersion(query, worker.protocolVersion()); err != nil {
		ctx.AbortString(400, err.Error())
		return
	}
	if to, ok := ctx.Request.To(); ok && to != nil && to.Address != nil && to.Address.User() != nil {
		query.recordQueryLocationID = strings.TrimSpace(to.Address.User().String())
	}
	allowed := cascadeQueryTargetAllowed(worker.platform, query.CmdType, query.DeviceID, worker.protocolVersion())
	if !allowed && query.CmdType == "Catalog" && worker.protocolVersion().AtLeast(GBVersion11) {
		lookupParent := monitorUserIdentityContextWithParent(g.initializedServiceContext(), ctx)
		lookupCtx, cancel := context.WithTimeout(lookupParent, 5*time.Second)
		visible, lookupErr := g.cascadeCatalogTargetVisible(lookupCtx, worker.platform, worker.protocolVersion(), query.DeviceID)
		cancel()
		if lookupErr != nil {
			ctx.AbortString(500, "load cascade Catalog target failed")
			return
		}
		allowed = visible
	}
	if !allowed {
		ctx.AbortString(404, "cascade target not found")
		return
	}

	if !acknowledgeCascadeRequest(ctx, query.CmdType, query.SN) {
		return
	}
	identityCtx := monitorUserIdentityContext(ctx)
	g.startCascadeLifecycleTask(identityCtx, worker, func(taskCtx context.Context) {
		g.respondCascadeQuery(worker, query, taskCtx)
	})
}

func acknowledgeCascadeRequest(ctx *sip.Context, cmdType string, sn int) bool {
	if ctx == nil {
		return false
	}
	ctx.Abort()
	if err := ctx.RespondString(200, "OK"); err != nil {
		logger := slog.Default()
		if ctx.Log != nil {
			logger = ctx.Log
		}
		logger.Error("respond cascade request", "err", err, "cmd_type", cmdType, "sn", sn)
		return false
	}
	return true
}

func validateCascadeQueryRequest(query cascadeQueryEnvelope) error {
	deviceID := strings.TrimSpace(query.DeviceID)
	if query.XMLName.Local != "Query" || query.SN <= 0 {
		return fmt.Errorf("invalid cascade query envelope")
	}
	if query.CmdType == "Catalog" {
		if deviceID != "*" && classifyGBCatalogItem(deviceID) == GBCatalogItemUnknown {
			return fmt.Errorf("invalid cascade query envelope")
		}
	} else if !isGBDeviceIdentifier(deviceID) {
		return fmt.Errorf("invalid cascade query envelope")
	}
	switch query.CmdType {
	case "Catalog":
		startAt, endAt, err := cascadeCatalogQueryTimes(query)
		if err != nil {
			return err
		}
		if !startAt.IsZero() && !endAt.IsZero() && !endAt.After(startAt) {
			return fmt.Errorf("Catalog EndTime must be after StartTime")
		}
		return nil
	case "Alarm":
		return nil
	case "DeviceInfo", "DeviceStatus", "PresetQuery", "HomePositionQuery", "CruiseTrackListQuery", "PTZPosition", "SDCardStatus":
		return nil
	case "ConfigDownload":
		if _, ok := normalizeConfigTypes(query.ConfigType); !ok {
			return fmt.Errorf("ConfigDownload requires valid ConfigType")
		}
		return nil
	case "MobilePosition":
		if query.Interval < 0 {
			return fmt.Errorf("MobilePosition Interval must not be negative")
		}
		return nil
	case "CruiseTrackQuery":
		if query.Number == nil || (*query.Number != 0 && *query.Number != 1) {
			return fmt.Errorf("CruiseTrackQuery Number must be 0 or 1")
		}
		return nil
	case "RecordInfo":
		startAt, endAt, err := cascadeRecordQueryTimes(query)
		if err != nil {
			return err
		}
		if !startAt.IsZero() && !endAt.IsZero() && !endAt.After(startAt) {
			return fmt.Errorf("RecordInfo EndTime must be after StartTime")
		}
		if query.Secrecy != nil && *query.Secrecy != 0 && *query.Secrecy != 1 {
			return fmt.Errorf("RecordInfo Secrecy must be 0 or 1")
		}
		if _, err = cascadeRecordIndistinctQuery(query); err != nil {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unsupported cascade query command: %s", query.CmdType)
	}
}

func validateCascadeQueryVersion(query cascadeQueryEnvelope, version GBProtocolVersion) error {
	switch query.CmdType {
	case "Catalog":
		deviceID := strings.TrimSpace(query.DeviceID)
		if deviceID != "*" && !validCatalogTargetID(version, deviceID) {
			return fmt.Errorf("Catalog target is not supported by %s", version.StandardName())
		}
		return nil
	case "Alarm":
		return validateAlarmQueryFilters(query, version)
	case "DeviceInfo", "DeviceStatus":
		return nil
	case "RecordInfo":
		if version.AtLeast(GBVersion20) && (strings.TrimSpace(query.StartTime) == "" || strings.TrimSpace(query.EndTime) == "") {
			return fmt.Errorf("RecordInfo requires StartTime and EndTime in GB/T 28181-2016 and later")
		}
		if query.IndistinctQuery != nil && !version.AtLeast(GBVersion11) {
			return fmt.Errorf("RecordInfo IndistinctQuery requires GB/T 28181-2014 or later")
		}
		if query.DistinctQuery != nil && !version.AtLeast(GBVersion30) {
			return fmt.Errorf("RecordInfo DistinctQuery compatibility alias requires GB/T 28181-2022")
		}
		return validateRecordQueryFilters(version, &RecordQueryInput{
			Type: query.Type, IndistinctQuery: query.IndistinctQuery, StreamNumber: query.StreamNumber, AlarmMethod: query.AlarmMethod, AlarmType: query.AlarmType,
		})
	default:
		action, allowed := cascadeExtendedQueryAction(query.CmdType, version)
		if !allowed {
			return fmt.Errorf("%s is not supported by %s", query.CmdType, version.StandardName())
		}
		if action == deviceQueryActionConfigDownload {
			if _, allowed = cascadeConfigDownloadType(query.ConfigType, version); !allowed {
				return fmt.Errorf("ConfigDownload type is not supported by %s", version.StandardName())
			}
		}
		return nil
	}
}

func validateAlarmQueryFilters(query cascadeQueryEnvelope, version GBProtocolVersion) error {
	return applyAlarmQueryFilters(version, &genericDeviceQueryRequest{}, &DeviceQueryInput{
		StartAlarmPriority: query.StartAlarmPriority,
		EndAlarmPriority:   query.EndAlarmPriority,
		AlarmMethod:        query.AlarmMethod,
		AlarmType:          query.AlarmType,
		StartAlarmTime:     query.StartAlarmTime,
		EndAlarmTime:       query.EndAlarmTime,
	})
}

func cascadeRecordIndistinctQuery(query cascadeQueryEnvelope) (*int, error) {
	if query.IndistinctQuery != nil && query.DistinctQuery != nil {
		return nil, fmt.Errorf("RecordInfo must not contain both IndistinctQuery and DistinctQuery")
	}
	value := query.IndistinctQuery
	if value == nil {
		value = query.DistinctQuery
	}
	if value != nil && *value != 0 && *value != 1 {
		return nil, fmt.Errorf("RecordInfo IndistinctQuery must be 0 or 1")
	}
	return value, nil
}

func cascadeRecordQueryTimes(query cascadeQueryEnvelope) (time.Time, time.Time, error) {
	var startAt, endAt time.Time
	var err error
	if value := strings.TrimSpace(query.StartTime); value != "" {
		startAt, err = parseGBDateTime(value)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("RecordInfo StartTime is invalid")
		}
	}
	if value := strings.TrimSpace(query.EndTime); value != "" {
		endAt, err = parseGBDateTime(value)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("RecordInfo EndTime is invalid")
		}
	}
	return startAt, endAt, nil
}

func cascadeCatalogQueryTimes(query cascadeQueryEnvelope) (time.Time, time.Time, error) {
	var startAt, endAt time.Time
	var err error
	if value := strings.TrimSpace(query.StartTime); value != "" {
		startAt, err = parseGBDateTime(value)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("Catalog StartTime is invalid")
		}
	}
	if value := strings.TrimSpace(query.EndTime); value != "" {
		endAt, err = parseGBDateTime(value)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("Catalog EndTime is invalid")
		}
	}
	return startAt, endAt, nil
}

func (g *GB28181API) respondCascadeQuery(worker *cascadeWorker, query cascadeQueryEnvelope, parents ...context.Context) {
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	if err := validateCascadeQueryVersion(query, worker.protocolVersion()); err != nil {
		slog.Warn("reject unsupported cascade query response", "upstream", worker.platform.name, "cmd_type", query.CmdType, "sn", query.SN, "err", err)
		return
	}
	var err error
	switch query.CmdType {
	case "Catalog":
		err = g.respondCascadeCatalog(ctx, worker, query)
	case "DeviceInfo":
		err = g.respondCascadeDeviceInfo(ctx, worker, query)
	case "DeviceStatus":
		err = g.respondCascadeDeviceStatus(ctx, worker, query)
	case "RecordInfo":
		err = g.respondCascadeRecordInfo(ctx, worker, query)
	case "Alarm", "PresetQuery", "HomePositionQuery", "CruiseTrackListQuery", "CruiseTrackQuery", "MobilePosition", "PTZPosition", "SDCardStatus", "ConfigDownload":
		err = g.respondCascadeExtendedQuery(ctx, worker, query)
	default:
		err = sendCascadeQueryError(ctx, worker, query)
	}
	if err != nil {
		slog.Error("respond cascade query failed", "upstream", worker.platform.name, "cmd_type", query.CmdType, "sn", query.SN, "err", err)
	}
}

func cascadeQueryTargetAllowed(platform cascadePlatform, cmdType, deviceID string, versions ...GBProtocolVersion) bool {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == platform.localID {
		switch strings.TrimSpace(cmdType) {
		case "Catalog", "DeviceInfo", "DeviceStatus", "RecordInfo", "Alarm":
			return true
		case "MobilePosition":
			version := platform.version
			if len(versions) > 0 {
				version = versions[0]
			}
			return version.AtLeast(GBVersion30)
		default:
			return false
		}
	}
	if platform.exposedChannelMap[deviceID] == "" {
		return false
	}
	switch strings.TrimSpace(cmdType) {
	case "Catalog", "DeviceInfo", "DeviceStatus", "RecordInfo", "Alarm", "PresetQuery", "HomePositionQuery",
		"CruiseTrackListQuery", "CruiseTrackQuery", "MobilePosition", "PTZPosition", "SDCardStatus", "ConfigDownload":
		return true
	default:
		return false
	}
}

func (g *GB28181API) cascadeCatalogTargetVisible(ctx context.Context, platform cascadePlatform, version GBProtocolVersion, deviceID string) (bool, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" || deviceID == "*" || deviceID == strings.TrimSpace(platform.localID) {
		return true, nil
	}
	channels, err := g.loadCascadeCatalogChannels(ctx, platform, version)
	if err != nil {
		return false, err
	}
	for _, item := range buildCascadeCatalogItems(channels, platform, version) {
		if strings.TrimSpace(item.DeviceID) == deviceID {
			return true, nil
		}
	}
	return false, nil
}

func cascadeExtendedQueryAction(cmdType string, version GBProtocolVersion) (string, bool) {
	switch canonicalGBQueryCmdType(cmdType) {
	case "Alarm":
		return deviceQueryActionAlarm, true
	case "PresetQuery":
		return deviceQueryActionPresetQuery, version.AtLeast(GBVersion11)
	case "HomePositionQuery":
		return deviceQueryActionHomePositionQuery, version.AtLeast(GBVersion30)
	case "MobilePosition":
		return deviceQueryActionMobilePosition, version.AtLeast(GBVersion20)
	case "CruiseTrackListQuery":
		return deviceQueryActionCruiseTrackList, version.AtLeast(GBVersion30)
	case "CruiseTrackQuery":
		return deviceQueryActionCruiseTrack, version.AtLeast(GBVersion30)
	case "PTZPosition":
		return deviceQueryActionPTZPosition, version.AtLeast(GBVersion30)
	case "SDCardStatus":
		return deviceQueryActionSDCardStatus, version.AtLeast(GBVersion30)
	case "ConfigDownload":
		return deviceQueryActionConfigDownload, version.AtLeast(GBVersion11)
	default:
		return "", false
	}
}

func (g *GB28181API) respondCascadeExtendedQuery(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	action, allowed := cascadeExtendedQueryAction(query.CmdType, worker.protocolVersion())
	if allowed && action == deviceQueryActionConfigDownload {
		query.ConfigType, allowed = cascadeConfigDownloadType(query.ConfigType, worker.protocolVersion())
	}
	if !allowed {
		return fmt.Errorf("%s is not supported by %s", query.CmdType, worker.protocolVersion().StandardName())
	}
	if query.CmdType == "MobilePosition" && strings.TrimSpace(query.DeviceID) == strings.TrimSpace(worker.platform.localID) {
		return g.respondCascadeSystemMobilePositionQuery(ctx, worker, query)
	}
	return g.respondCascadeForwardedQuery(ctx, worker, query, action)
}

func cascadeConfigDownloadType(value string, version GBProtocolVersion) (string, bool) {
	canonical, ok := normalizeConfigTypes(value)
	if !ok {
		return "", false
	}
	for _, name := range strings.Split(canonical, "/") {
		if !configTypeSupported(version, name) {
			return "", false
		}
	}
	return canonical, true
}

func (g *GB28181API) respondCascadeForwardedQuery(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope, action string) error {
	channel, err := g.loadCascadeExposedChannel(ctx, worker.platform, query.DeviceID)
	if err != nil {
		slog.Warn("load shared channel for cascade query failed", "upstream", worker.platform.name, "cmd_type", query.CmdType, "channel", query.DeviceID, "err", err)
		return sendCascadeQueryError(ctx, worker, query)
	}
	if err := g.validateCascadeDeviceQueryTarget(channel.DeviceID, action, query.ConfigType); err != nil {
		slog.Warn("reject unsupported downstream cascade query", "upstream", worker.platform.name, "cmd_type", query.CmdType, "channel", query.DeviceID, "err", err)
		return sendCascadeQueryError(ctx, worker, query)
	}
	queryDevice := g.DeviceQuery
	if g.cascadeDeviceQuery != nil {
		queryDevice = g.cascadeDeviceQuery
	}
	var mobileRoute *cascadeMobilePositionQueryRoute
	if query.CmdType == "MobilePosition" {
		mobileRoute = g.storeCascadeMobilePositionQuery(worker, query.DeviceID, channel.DeviceID, channel.ChannelID)
	}
	out, err := queryDevice(ctx, &DeviceQueryInput{
		DeviceID: channel.DeviceID, TargetID: channel.ChannelID, Action: action,
		Timeout: 25 * time.Second, ConfigType: query.ConfigType, Interval: query.Interval, Number: cascadeQueryNumber(query),
		StartAlarmPriority: query.StartAlarmPriority, EndAlarmPriority: query.EndAlarmPriority,
		AlarmMethod: query.AlarmMethod, AlarmType: query.AlarmType,
		StartAlarmTime: query.StartAlarmTime, EndAlarmTime: query.EndAlarmTime,
	})
	if err != nil || out == nil || !strings.EqualFold(canonicalGBQueryCmdType(out.CmdType), query.CmdType) {
		g.deleteCascadeMobilePositionQuery(mobileRoute)
		if err != nil {
			slog.Warn("forward cascade query failed", "upstream", worker.platform.name, "cmd_type", query.CmdType, "channel", query.DeviceID, "err", err)
		}
		return sendCascadeQueryError(ctx, worker, query)
	}
	if query.CmdType == "MobilePosition" {
		return nil
	}
	if strings.TrimSpace(out.XML) == "" {
		return sendCascadeQueryError(ctx, worker, query)
	}
	responses := out.responseXML
	if len(responses) == 0 {
		responses = []string{out.XML}
	}
	for _, response := range responses {
		body, rewriteErr := rewriteCascadeQueryResponse([]byte(response), query, worker.platform, worker.protocolVersion(), channel)
		if rewriteErr != nil {
			slog.Warn("rewrite cascade query response failed", "upstream", worker.platform.name, "cmd_type", query.CmdType, "channel", query.DeviceID, "err", rewriteErr)
			return sendCascadeQueryError(ctx, worker, query)
		}
		if err := worker.sendMessage(ctx, body); err != nil {
			return err
		}
	}
	return nil
}

// validateCascadeDeviceQueryTarget 在自定义级联查询钩子和通知路由产生副作用前，
// 复用直连 DeviceQuery 的设备在线、下级版本和设备级能力门禁。
func (g *GB28181API) validateCascadeDeviceQueryTarget(deviceID, action, configType string) error {
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		// 保留未装配运行态存储的独立协议测试和外部注入兼容；生产 Server 始终装配该存储。
		return nil
	}
	device, ok := g.svr.memoryStorer.Load(strings.TrimSpace(deviceID))
	if !ok || device == nil || !device.IsOnlineNow() {
		return ErrDeviceOffline
	}
	_, err := g.resolveDeviceQueryCmdType(deviceID, normalizeDeviceQueryAction(action), strings.TrimSpace(configType))
	return err
}

type cascadeMobilePositionQueryRoute struct {
	key            string
	worker         *cascadeWorker
	targetID       string
	sourceDeviceID string
	sourceTargetID string
	system         bool

	notifyMu        sync.Mutex
	mu              sync.Mutex
	ready           bool
	pending         map[string]string
	sources         map[string]string
	positions       map[string]mobilePositionItemXML
	positionSources map[string]string
	hasNotification bool
	lastSN          int
	lastTime        string
}

func cascadeMobilePositionQueryKey(worker *cascadeWorker, targetID string) string {
	if worker == nil {
		return ""
	}
	return strings.TrimSpace(worker.platform.name) + "\x00" + strings.TrimSpace(worker.platform.serverID) + "\x00" + strings.TrimSpace(targetID)
}

func (g *GB28181API) storeCascadeMobilePositionQuery(worker *cascadeWorker, targetID, sourceDeviceID, sourceTargetID string) *cascadeMobilePositionQueryRoute {
	if g == nil || worker == nil || strings.TrimSpace(targetID) == "" || !g.cascadeWorkerAvailable(worker) {
		return nil
	}
	route := &cascadeMobilePositionQueryRoute{
		key: cascadeMobilePositionQueryKey(worker, targetID), worker: worker, targetID: strings.TrimSpace(targetID),
		sourceDeviceID: strings.TrimSpace(sourceDeviceID), sourceTargetID: strings.TrimSpace(sourceTargetID),
	}
	g.cascadeMobilePositionQueries.Store(route.key, route)
	return route
}

func (g *GB28181API) deleteCascadeMobilePositionQuery(route *cascadeMobilePositionQueryRoute) {
	if g == nil || route == nil || route.key == "" {
		return
	}
	g.cascadeMobilePositionQueries.CompareAndDelete(route.key, route)
}

func (g *GB28181API) removeCascadeMobilePositionQueries(worker *cascadeWorker) {
	if g == nil || worker == nil {
		return
	}
	g.cascadeMobilePositionQueries.Range(func(key, value any) bool {
		route, ok := value.(*cascadeMobilePositionQueryRoute)
		if !ok || route == nil {
			g.cascadeMobilePositionQueries.CompareAndDelete(key, value)
			return true
		}
		if route.worker == worker {
			g.cascadeMobilePositionQueries.CompareAndDelete(key, value)
		}
		return true
	})
}

func (g *GB28181API) removeCascadeMobilePositionQueriesForDevice(deviceID string) {
	g.removeCascadeMobilePositionQueriesForTargets(deviceID, nil)
}

func (g *GB28181API) removeCascadeMobilePositionQueriesForChannels(deviceID string, channelIDs map[string]struct{}) {
	if len(channelIDs) == 0 {
		return
	}
	g.removeCascadeMobilePositionQueriesForTargets(deviceID, channelIDs)
}

func (g *GB28181API) removeCascadeMobilePositionQueriesForTargets(deviceID string, channelIDs map[string]struct{}) {
	if g == nil {
		return
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return
	}
	g.cascadeMobilePositionQueries.Range(func(key, value any) bool {
		route, ok := value.(*cascadeMobilePositionQueryRoute)
		if !ok || route == nil {
			g.cascadeMobilePositionQueries.CompareAndDelete(key, value)
			return true
		}
		route.notifyMu.Lock()
		route.mu.Lock()
		targetRemoved := channelIDs == nil
		if !targetRemoved {
			_, targetRemoved = channelIDs[route.sourceTargetID]
		}
		removeRoute := !route.system && route.sourceDeviceID == deviceID && targetRemoved
		if route.system {
			removedChannels := make(map[string]struct{})
			for channelID, sourceID := range route.pending {
				_, channelRemoved := channelIDs[channelID]
				if sourceID == deviceID && (channelIDs == nil || channelRemoved) {
					delete(route.pending, channelID)
					removedChannels[channelID] = struct{}{}
				}
			}
			for channelID, sourceID := range route.sources {
				_, channelRemoved := channelIDs[channelID]
				if sourceID == deviceID && (channelIDs == nil || channelRemoved) {
					delete(route.sources, channelID)
					removedChannels[channelID] = struct{}{}
				}
			}
			for exposedID, channelID := range route.positionSources {
				if _, removed := removedChannels[channelID]; removed {
					delete(route.positionSources, exposedID)
					delete(route.positions, exposedID)
				}
			}
			removeRoute = len(route.pending) == 0 && len(route.sources) == 0
		}
		route.mu.Unlock()
		route.notifyMu.Unlock()
		if removeRoute {
			g.cascadeMobilePositionQueries.CompareAndDelete(key, route)
		}
		return true
	})
}

func (g *GB28181API) respondCascadeSystemMobilePositionQuery(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	if !worker.protocolVersion().AtLeast(GBVersion30) {
		return fmt.Errorf("system MobilePosition query requires GB/T 28181-2022")
	}
	channels, err := g.loadCascadeChannels(ctx, worker.platform)
	if err != nil {
		return err
	}
	mobileChannels := make([]*ipc.Channel, 0, len(channels))
	for _, channel := range channels {
		if !cascadeMobilePositionChannelEligible(channel) {
			continue
		}
		if queryErr := g.validateCascadeDeviceQueryTarget(channel.DeviceID, deviceQueryActionMobilePosition, ""); queryErr != nil {
			slog.Warn("exclude unsupported downstream system MobilePosition target", "upstream", worker.platform.name, "channel", channel.ChannelID, "err", queryErr)
			continue
		}
		mobileChannels = append(mobileChannels, channel)
	}
	if len(mobileChannels) == 0 {
		return fmt.Errorf("no shared online mobile channels")
	}
	route := g.storeCascadeSystemMobilePositionQuery(worker, query.DeviceID, mobileChannels)
	if route == nil {
		return fmt.Errorf("store system MobilePosition route failed")
	}

	queryDevice := g.DeviceQuery
	if g.cascadeDeviceQuery != nil {
		queryDevice = g.cascadeDeviceQuery
	}
	type result struct {
		channel *ipc.Channel
		ok      bool
	}
	results := make(chan result, len(mobileChannels))
	semaphore := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, channel := range mobileChannels {
		channel := channel
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results <- result{channel: channel}
				return
			}
			out, queryErr := queryDevice(ctx, &DeviceQueryInput{
				DeviceID: channel.DeviceID, TargetID: channel.ChannelID, Action: deviceQueryActionMobilePosition,
				Timeout: 25 * time.Second, Interval: query.Interval,
			})
			ok := queryErr == nil && out != nil && strings.EqualFold(canonicalGBQueryCmdType(out.CmdType), query.CmdType)
			if queryErr != nil {
				slog.Warn("forward system MobilePosition query failed", "upstream", worker.platform.name, "channel", channel.ChannelID, "err", queryErr)
			}
			results <- result{channel: channel, ok: ok}
		}()
	}
	wg.Wait()
	close(results)
	succeeded := make(map[string]struct{}, len(mobileChannels))
	for item := range results {
		if item.ok {
			succeeded[item.channel.ChannelID] = struct{}{}
		}
	}
	current, active := g.cascadeMobilePositionQueries.Load(route.key)
	if !active || current != route {
		return nil
	}
	select {
	case <-worker.operationContext().Done():
		g.deleteCascadeMobilePositionQuery(route)
		return worker.operationContext().Err()
	default:
	}
	_, sendErr := route.finishSystemQuery(succeeded)
	if len(succeeded) == 0 {
		g.deleteCascadeMobilePositionQuery(route)
		return fmt.Errorf("all shared mobile channels rejected MobilePosition query")
	}
	if sendErr != nil {
		return sendErr
	}
	return nil
}

func cascadeMobilePositionChannelEligible(channel *ipc.Channel) bool {
	if channel == nil || !channel.IsOnline {
		return false
	}
	if channel.Ext.GBCatalog != nil && channel.Ext.GBCatalog.MobileDeviceType != 0 {
		return true
	}
	channelID := strings.TrimSpace(channel.ChannelID)
	return len(channelID) == 20 && channelID[10:13] == "138"
}

func (g *GB28181API) storeCascadeSystemMobilePositionQuery(worker *cascadeWorker, targetID string, channels []*ipc.Channel) *cascadeMobilePositionQueryRoute {
	if g == nil || worker == nil || strings.TrimSpace(targetID) == "" || !g.cascadeWorkerAvailable(worker) {
		return nil
	}
	route := &cascadeMobilePositionQueryRoute{
		key: cascadeMobilePositionQueryKey(worker, targetID), worker: worker, targetID: strings.TrimSpace(targetID), system: true,
		pending: make(map[string]string, len(channels)), sources: make(map[string]string, len(channels)),
		positions: make(map[string]mobilePositionItemXML), positionSources: make(map[string]string),
	}
	for _, channel := range channels {
		if channel != nil {
			route.pending[strings.TrimSpace(channel.ChannelID)] = strings.TrimSpace(channel.DeviceID)
		}
	}
	g.cascadeMobilePositionQueries.Store(route.key, route)
	return route
}

func (route *cascadeMobilePositionQueryRoute) finishSystemQuery(succeeded map[string]struct{}) (bool, error) {
	route.notifyMu.Lock()
	defer route.notifyMu.Unlock()
	route.mu.Lock()
	for channelID, sourceID := range route.pending {
		if _, ok := succeeded[channelID]; ok {
			route.sources[channelID] = sourceID
			continue
		}
		for exposedID, localID := range route.positionSources {
			if localID == channelID {
				delete(route.positions, exposedID)
				delete(route.positionSources, exposedID)
			}
		}
	}
	route.pending = nil
	route.ready = true
	body, err := route.systemNotifyBodyLocked()
	route.mu.Unlock()
	if err != nil || len(body) == 0 {
		return false, err
	}
	return true, route.sendSystemNotify(body)
}

func (g *GB28181API) forwardCascadeMobilePositionQueryNotify(sourceDeviceID string, body []byte) {
	if g == nil || len(body) == 0 {
		return
	}
	sourceDeviceID = strings.TrimSpace(sourceDeviceID)
	g.cascadeMobilePositionQueries.Range(func(key, value any) bool {
		route, ok := value.(*cascadeMobilePositionQueryRoute)
		if !ok || route == nil || route.worker == nil {
			g.cascadeMobilePositionQueries.CompareAndDelete(key, value)
			return true
		}
		select {
		case <-route.worker.operationContext().Done():
			g.cascadeMobilePositionQueries.CompareAndDelete(key, route)
			return true
		default:
		}
		if !route.system && route.sourceDeviceID != sourceDeviceID {
			return true
		}
		if route.system {
			if err := route.forwardSystemNotify(sourceDeviceID, body); err != nil {
				slog.Warn("forward system MobilePosition query notify failed", "upstream", route.worker.platform.name, "err", err)
			}
			return true
		}
		outputs, err := rewriteCascadeMobilePositionForVersion(route.worker.platform, body, sourceDeviceID, route.worker.protocolVersion(), route.targetID)
		if err != nil {
			slog.Warn("rewrite cascade MobilePosition query notify failed", "upstream", route.worker.platform.name, "err", err)
			return true
		}
		for _, output := range outputs {
			ctx, cancel := context.WithTimeout(route.worker.operationContext(), defaultCascadeRequestTimeout)
			err = route.worker.sendMessage(ctx, output.body)
			cancel()
			if err != nil {
				slog.Warn("send cascade MobilePosition query notify failed", "upstream", route.worker.platform.name, "device_id", output.exposedID, "err", err)
			}
		}
		return true
	})
}

type cascadeSystemMobilePositionNotify struct {
	XMLName    xml.Name `xml:"Notify"`
	CmdType    string   `xml:"CmdType"`
	SN         int      `xml:"SN"`
	DeviceID   string   `xml:"DeviceID"`
	Time       string   `xml:"Time"`
	SumNum     int      `xml:"SumNum"`
	DeviceList struct {
		Num  int                     `xml:"Num,attr"`
		Item []mobilePositionItemXML `xml:"Item"`
	} `xml:"DeviceList"`
}

func (route *cascadeMobilePositionQueryRoute) forwardSystemNotify(sourceDeviceID string, body []byte) error {
	var msg mobilePositionNotify
	if err := sip.XMLDecode(body, &msg); err != nil {
		return err
	}
	if msg.XMLName.Local != "Notify" || !strings.EqualFold(strings.TrimSpace(msg.CmdType), "MobilePosition") {
		return nil
	}
	if len(msg.Info) > 0 || len(msg.ExtraInfo) > 0 || len(msg.ExtralInfo) > 0 {
		return fmt.Errorf("MobilePosition does not define Info or ExtraInfo")
	}
	extended, err := inspectAppendixA4Payload(body)
	if err != nil {
		return err
	}
	if extended {
		return fmt.Errorf("MobilePosition does not support Appendix A.4 extensions")
	}
	route.notifyMu.Lock()
	defer route.notifyMu.Unlock()
	route.mu.Lock()
	accepted := route.collectSystemPositionsLocked(strings.TrimSpace(sourceDeviceID), &msg)
	if !accepted || !route.ready {
		route.mu.Unlock()
		return nil
	}
	aggregated, err := route.systemNotifyBodyLocked()
	route.mu.Unlock()
	if err != nil || len(aggregated) == 0 {
		return err
	}
	return route.sendSystemNotify(aggregated)
}

func (route *cascadeMobilePositionQueryRoute) collectSystemPositionsLocked(sourceDeviceID string, msg *mobilePositionNotify) bool {
	version := GBVersion30
	if msg.SumNum == nil && msg.DeviceList.XMLName.Local == "" {
		version = GBVersion20
	}
	items := msg.DeviceList.Item
	if version == GBVersion30 && msg.SumNum != nil && *msg.SumNum == 0 && len(items) == 0 {
		if msg.SN <= 0 || !validGBDateTime(msg.Time) || msg.Longitude != nil || msg.Latitude != nil || msg.Speed != nil ||
			msg.Direction != nil || msg.Altitude != nil || msg.Height != nil ||
			msg.DeviceList.Num != nil && *msg.DeviceList.Num != 0 {
			return false
		}
		localID := route.uniqueSystemPositionTargetLocked(sourceDeviceID, strings.TrimSpace(msg.DeviceID))
		if localID == "" {
			return false
		}
		exposedID := route.worker.platform.channelIDMap[localID]
		if exposedID == "" {
			return false
		}
		delete(route.positions, exposedID)
		delete(route.positionSources, exposedID)
		route.hasNotification = true
		route.lastSN = msg.SN
		route.lastTime = strings.TrimSpace(msg.Time)
		return true
	}
	if len(items) == 0 && msg.Longitude != nil && msg.Latitude != nil {
		localID := route.uniqueSystemPositionTargetLocked(sourceDeviceID, strings.TrimSpace(msg.DeviceID))
		if localID == "" {
			slog.Warn("skip unresolved 2016 system MobilePosition notify", "upstream", route.worker.platform.name, "source_device_id", sourceDeviceID)
			return false
		}
		items = []mobilePositionItemXML{{
			DeviceID: localID, CaptureTime: strings.TrimSpace(msg.Time), Longitude: msg.Longitude, Latitude: msg.Latitude,
			Speed: msg.Speed, Direction: msg.Direction, Altitude: msg.Altitude,
		}}
	}
	accepted := false
	for _, item := range items {
		localID := strings.TrimSpace(item.DeviceID)
		expectedSource, ok := route.sources[localID]
		if !ok {
			expectedSource, ok = route.pending[localID]
		}
		if !ok || expectedSource != sourceDeviceID {
			continue
		}
		exposedID := route.worker.platform.channelIDMap[localID]
		if exposedID == "" {
			continue
		}
		position := &MobilePositionData{
			DeviceID: localID, Time: strings.TrimSpace(item.CaptureTime), CaptureTime: strings.TrimSpace(item.CaptureTime),
			Longitude: item.Longitude, Latitude: item.Latitude, Speed: item.Speed, Direction: item.Direction, Altitude: item.Altitude, Height: item.Height,
		}
		captureTime, err := parseGBDateTime(position.CaptureTime)
		if err != nil || validateMobilePositionData(position, version) != nil {
			continue
		}
		if version == GBVersion20 && item.Direction != nil && *item.Direction == 360 {
			zero := float64(0)
			item.Direction = &zero
		}
		item.DeviceID = exposedID
		item.CaptureTime = position.CaptureTime
		if current, ok := route.positions[exposedID]; ok {
			currentCaptureTime, currentErr := parseGBDateTime(current.CaptureTime)
			if currentErr == nil && captureTime.Before(currentCaptureTime) {
				continue
			}
		}
		route.positions[exposedID] = item
		route.positionSources[exposedID] = localID
		accepted = true
	}
	if !accepted {
		return false
	}
	route.hasNotification = true
	route.lastSN = msg.SN
	route.lastTime = strings.TrimSpace(msg.Time)
	return true
}

func (route *cascadeMobilePositionQueryRoute) uniqueSystemPositionTargetLocked(sourceDeviceID, explicitID string) string {
	sourceDeviceID = strings.TrimSpace(sourceDeviceID)
	explicitID = strings.TrimSpace(explicitID)
	if explicitID != "" {
		if expected, ok := route.sources[explicitID]; ok && expected == sourceDeviceID {
			return explicitID
		}
		if expected, ok := route.pending[explicitID]; ok && expected == sourceDeviceID {
			return explicitID
		}
		// 报文明确给出了共享通道，但该通道不属于当前成功路由时，不得改归到同一父设备的其他通道。
		if route.worker != nil && route.worker.platform.channelIDMap[explicitID] != "" {
			return ""
		}
	}
	candidates := make(map[string]struct{})
	for localID, expected := range route.sources {
		if expected == sourceDeviceID {
			candidates[localID] = struct{}{}
		}
	}
	for localID, expected := range route.pending {
		if expected == sourceDeviceID {
			candidates[localID] = struct{}{}
		}
	}
	if len(candidates) != 1 {
		return ""
	}
	for localID := range candidates {
		return localID
	}
	return ""
}

func (route *cascadeMobilePositionQueryRoute) systemNotifyBodyLocked() ([]byte, error) {
	if !route.hasNotification {
		return nil, nil
	}
	ids := make([]string, 0, len(route.positions))
	for deviceID := range route.positions {
		ids = append(ids, deviceID)
	}
	sort.Strings(ids)
	notify := cascadeSystemMobilePositionNotify{
		CmdType: "MobilePosition", SN: route.lastSN, DeviceID: route.targetID, Time: route.lastTime, SumNum: len(ids),
	}
	notify.DeviceList.Num = len(ids)
	for _, deviceID := range ids {
		notify.DeviceList.Item = append(notify.DeviceList.Item, route.positions[deviceID])
	}
	return sip.XMLEncode(notify)
}

func (route *cascadeMobilePositionQueryRoute) sendSystemNotify(body []byte) error {
	if route == nil || route.worker == nil || len(body) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(route.worker.operationContext(), defaultCascadeRequestTimeout)
	err := route.worker.sendMessage(ctx, body)
	cancel()
	return err
}

func cascadeQueryNumber(query cascadeQueryEnvelope) int {
	if query.Number == nil {
		return 0
	}
	return *query.Number
}

func sendCascadeQueryError(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	deviceID := strings.TrimSpace(query.DeviceID)
	if deviceID == "" {
		deviceID = worker.platform.localID
	}
	version := worker.protocolVersion()
	cmdType := gbQueryCmdTypeForVersion(query.CmdType, version)
	switch query.CmdType {
	case "Catalog":
		response := cascadeCatalogResponse{CmdType: cmdType, SN: query.SN, DeviceID: deviceID}
		if version == GBVersion10 {
			response.DeviceList = &cascadeCatalogDeviceList{}
		}
		return sendCascadeXML(ctx, worker, response)
	case "RecordInfo":
		response := cascadeRecordInfoResponse{
			CmdType: cmdType, SN: query.SN, DeviceID: deviceID, Name: deviceID,
		}
		if version == GBVersion10 {
			response.RecordList = &cascadeRecordInfoList{}
		}
		return sendCascadeXML(ctx, worker, response)
	case "PresetQuery":
		response := cascadePresetQueryErrorResponse{
			CmdType: cmdType, SN: query.SN, DeviceID: deviceID, PresetList: cascadePresetQueryErrorList{},
		}
		if version.AtLeast(GBVersion30) {
			zero := 0
			response.SumNum = &zero
		}
		return sendCascadeXML(ctx, worker, response)
	case "DeviceStatus":
		return sendCascadeXML(ctx, worker, cascadeDeviceStatusErrorResponse{
			CmdType: cmdType, SN: query.SN, DeviceID: deviceID, Result: "ERROR", Online: "OFFLINE", Status: "ERROR",
		})
	case "HomePositionQuery", "PTZPosition":
		return sendCascadeXML(ctx, worker, cascadeQueryBaseResponse{
			CmdType: cmdType, SN: query.SN, DeviceID: deviceID,
		})
	case "CruiseTrackListQuery", "SDCardStatus":
		return sendCascadeXML(ctx, worker, cascadeQueryCountResponse{
			CmdType: cmdType, SN: query.SN, DeviceID: deviceID,
		})
	case "CruiseTrackQuery":
		return sendCascadeXML(ctx, worker, cascadeQueryCountResponse{
			CmdType: cmdType, SN: query.SN, DeviceID: deviceID, Number: query.Number,
		})
	case "MobilePosition":
		return fmt.Errorf("MobilePosition query failure has no business response schema")
	default:
		return sendCascadeXML(ctx, worker, cascadeQueryErrorResponse{
			CmdType: cmdType, SN: query.SN, DeviceID: deviceID, Result: "ERROR",
		})
	}
}

func rewriteCascadeQueryResponse(body []byte, query cascadeQueryEnvelope, platform cascadePlatform, version GBProtocolVersion, channel *ipc.Channel) ([]byte, error) {
	if len(body) == 0 || channel == nil {
		return nil, fmt.Errorf("empty cascade query response")
	}
	versionedInfoNested := make([]bool, 0)
	if query.CmdType == "DeviceInfo" || query.CmdType == "DeviceStatus" || query.CmdType == "ConfigDownload" {
		var envelope struct {
			Info []versionedInfoXML `xml:"Info"`
		}
		if err := sip.XMLDecode(body, &envelope); err != nil {
			return nil, fmt.Errorf("decode downstream %s extension info: %w", query.CmdType, err)
		}
		for _, info := range envelope.Info {
			versionedInfoNested = append(versionedInfoNested, len(info.Children) > 0)
		}
	}
	decoder := sip.NewGBXMLDecoder(body)

	var output bytes.Buffer
	output.WriteString("<?xml version=\"1.0\" encoding=\"GB2312\"?>\n")
	encoder := xml.NewEncoder(&output)
	depth := 0
	snapshotAliasDepth := 0
	basicParamDepth := 0
	alarmStatusDepth := 0
	alarmDutyStatusDepth := 0
	alarmDutyStatusSourceName := ""
	alarmDutyStatusTargetName := ""
	presetSumSeen := false
	infoIndex := 0
	rootSeen := false
	mappingPlatform := withCascadeIdentifierMapping(platform, channel.DeviceID, platform.localID)
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode downstream response: %w", err)
		}
		switch value := token.(type) {
		case xml.ProcInst:
			if strings.EqualFold(value.Target, "xml") {
				continue
			}
		case xml.StartElement:
			depth++
			if depth == 1 {
				if value.Name.Local != "Response" {
					return nil, fmt.Errorf("unexpected downstream root element: %s", value.Name.Local)
				}
				rootSeen = true
			}
			for index := range value.Attr {
				rewritten, rewriteErr := rewriteCascadeIdentifierValue(value.Attr[index].Value, value.Attr[index].Name.Local, mappingPlatform, channel.ChannelID, query.DeviceID)
				if rewriteErr != nil {
					return nil, rewriteErr
				}
				value.Attr[index].Value = rewritten
			}
			name := value.Name.Local
			if depth == 2 && query.CmdType == "PresetQuery" && version == GBVersion30 && name == "Result" {
				// 2014/2016 的 PresetQuery 示例允许携带 Result；2022 的应答
				// Schema 删除该节点，不能把旧版兼容字段原样转发。
				var discarded string
				if err := decoder.DecodeElement(&discarded, &value); err != nil {
					return nil, fmt.Errorf("decode downstream PresetQuery Result: %w", err)
				}
				depth--
				continue
			}
			if depth == 2 && query.CmdType == "PresetQuery" && version == GBVersion30 && name == "SumNum" {
				presetSumSeen = true
			}
			if depth == 2 && query.CmdType == "PresetQuery" && version == GBVersion30 && name == "PresetList" && !presetSumSeen {
				count := ""
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "Num" {
						count = strings.TrimSpace(attribute.Value)
						break
					}
				}
				parsed, parseErr := strconv.Atoi(count)
				if parseErr != nil || parsed < 0 {
					return nil, fmt.Errorf("downstream PresetList has invalid Num: %q", count)
				}
				sumStart := xml.StartElement{Name: xml.Name{Local: "SumNum"}}
				if err := encoder.EncodeToken(sumStart); err != nil {
					return nil, err
				}
				if err := encoder.EncodeToken(xml.CharData(strconv.Itoa(parsed))); err != nil {
					return nil, err
				}
				if err := encoder.EncodeToken(sumStart.End()); err != nil {
					return nil, err
				}
				presetSumSeen = true
			}
			if depth == 2 && query.CmdType == "DeviceInfo" && version == GBVersion30 &&
				(name == "DeviceType" || name == "MaxCamera" || name == "MaxAlarm") {
				// 2011/2014/2016 的规范示例允许这些兼容字段，2022 已删除。
				// 跨版本级联时只裁剪无法表达的字段，保留同一应答中的其余设备信息。
				var discarded string
				if err := decoder.DecodeElement(&discarded, &value); err != nil {
					return nil, fmt.Errorf("decode downstream %s: %w", name, err)
				}
				depth--
				continue
			}
			if depth == 2 && query.CmdType == "ConfigDownload" && name == "BasicParam" {
				basicParamDepth = depth
			}
			if basicParamDepth > 0 && depth == basicParamDepth+1 {
				if _, known := basicParamResponseFields[name]; known && !basicParamFieldSupported(version, name) {
					var discarded string
					if err := decoder.DecodeElement(&discarded, &value); err != nil {
						return nil, fmt.Errorf("decode downstream BasicParam %s: %w", name, err)
					}
					depth--
					continue
				}
			}
			if depth == 2 && query.CmdType == "DeviceInfo" && version == GBVersion10 && name == "DeviceName" {
				var discarded string
				if err := decoder.DecodeElement(&discarded, &value); err != nil {
					return nil, fmt.Errorf("decode downstream DeviceName: %w", err)
				}
				depth--
				continue
			}
			if depth == 2 && query.CmdType == "ConfigDownload" && version == GBVersion30 && name == "SnapShotConfig" {
				// 兼容下级设备沿用配置命令节点名，但对 2022 上级始终输出标准 SnapShot。
				value.Name.Local = "SnapShot"
				token = value
				snapshotAliasDepth = depth
			}
			infoNested := false
			if depth == 2 && name == "Info" && infoIndex < len(versionedInfoNested) {
				infoNested = versionedInfoNested[infoIndex]
				infoIndex++
			}
			if depth == 2 && name == "Info" && infoNested && version != GBVersion30 {
				var discarded versionedInfoXML
				if err := decoder.DecodeElement(&discarded, &value); err != nil {
					return nil, fmt.Errorf("decode downstream structured Info: %w", err)
				}
				depth--
				continue
			}
			if depth == 2 && query.CmdType == "DeviceStatus" && name == "Alarmstatus" {
				alarmStatusDepth = depth
				attributeName := "Num"
				if version == GBVersion20 {
					attributeName = "num"
				}
				for index := range value.Attr {
					if strings.EqualFold(value.Attr[index].Name.Local, "Num") {
						value.Attr[index].Name.Local = attributeName
					}
				}
			}
			if alarmStatusDepth > 0 && depth == alarmStatusDepth+2 &&
				equalFoldAny(name, "Status", "StatusDutyStatus", "DutyStatus") {
				alarmDutyStatusSourceName = name
				alarmDutyStatusTargetName = "DutyStatus"
				if version == GBVersion11 {
					alarmDutyStatusTargetName = "StatusDutyStatus"
				}
				value.Name.Local = alarmDutyStatusTargetName
				name = value.Name.Local
				token = value
				alarmDutyStatusDepth = depth
			}
			identifierField := strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "id")
			convertLegacyInfo := depth == 2 && name == "Info" && !infoNested && version == GBVersion30
			convertModernExtraInfo := depth == 2 && (name == "ExtraInfo" || name == "ExtralInfo") && version != GBVersion30
			normalizeModernExtraInfo := depth == 2 && name == "ExtralInfo" && version == GBVersion30
			if identifierField || strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") || convertLegacyInfo || convertModernExtraInfo || normalizeModernExtraInfo || (depth == 2 && (name == "CmdType" || name == "SN")) {
				var original string
				if err := decoder.DecodeElement(&original, &value); err != nil {
					return nil, fmt.Errorf("decode downstream %s: %w", name, err)
				}
				rewritten := original
				switch name {
				case "CmdType":
					rewritten = gbQueryCmdTypeForVersion(query.CmdType, version)
				case "SN":
					rewritten = strconv.Itoa(query.SN)
				case "ParentID":
					rewritten = platform.localID
				default:
					if strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") || convertLegacyInfo {
						rewritten, err = rewriteCascadeOpaqueIdentifiers(original, name, mappingPlatform, channel.ChannelID, query.DeviceID)
					} else if basicParamDepth > 0 && depth == basicParamDepth+1 && name == "DeviceID" {
						rewritten = query.DeviceID
					} else if basicParamDepth > 0 && depth == basicParamDepth+1 && name == "SIPServerID" {
						rewritten = platform.localID
					} else if depth == 2 && strings.EqualFold(name, "DeviceID") {
						rewritten = query.DeviceID
					} else {
						rewritten, err = rewriteCascadeIdentifierValue(original, name, mappingPlatform, channel.ChannelID, query.DeviceID)
					}
					if err != nil {
						return nil, err
					}
				}
				if convertLegacyInfo {
					// 旧版设备信息或状态的纯文本 Info 在 2022 中更名为 ExtraInfo。
					value.Name.Local = "ExtraInfo"
				} else if convertModernExtraInfo {
					// 2022 普通扩展在旧版中使用 Info；结构化 A.4 对象已在上方丢弃。
					value.Name.Local = "Info"
				} else if normalizeModernExtraInfo {
					value.Name.Local = "ExtraInfo"
				}
				if err := encoder.EncodeToken(value); err != nil {
					return nil, err
				}
				if err := encoder.EncodeToken(xml.CharData(rewritten)); err != nil {
					return nil, err
				}
				if err := encoder.EncodeToken(value.End()); err != nil {
					return nil, err
				}
				depth--
				continue
			}
		case xml.EndElement:
			if depth == alarmDutyStatusDepth && value.Name.Local == alarmDutyStatusSourceName {
				value.Name.Local = alarmDutyStatusTargetName
				token = value
				alarmDutyStatusDepth = 0
				alarmDutyStatusSourceName = ""
				alarmDutyStatusTargetName = ""
			}
			if depth == snapshotAliasDepth && value.Name.Local == "SnapShotConfig" {
				value.Name.Local = "SnapShot"
				token = value
				snapshotAliasDepth = 0
			}
			if depth == basicParamDepth && value.Name.Local == "BasicParam" {
				basicParamDepth = 0
			}
			if depth == alarmStatusDepth && value.Name.Local == "Alarmstatus" {
				alarmStatusDepth = 0
			}
			depth--
		}
		if err := encoder.EncodeToken(token); err != nil {
			return nil, err
		}
	}
	if !rootSeen || depth != 0 {
		return nil, fmt.Errorf("invalid downstream response document")
	}
	if err := encoder.Flush(); err != nil {
		return nil, err
	}
	encoded, err := sip.Utf8ToGbk(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("encode cascade response: %w", err)
	}
	if err := validateCascadeRewrittenQueryResponse(encoded, query.CmdType, version); err != nil {
		return nil, fmt.Errorf("rewritten cascade %s response is invalid for %s: %w", query.CmdType, version.StandardName(), err)
	}
	return encoded, nil
}

func validateCascadeRewrittenQueryResponse(body []byte, cmdType string, version GBProtocolVersion) error {
	switch cmdType {
	case "DeviceInfo":
		if err := validateDeviceInfoResponseStructure(body, version); err != nil {
			return err
		}
		var msg MessageDeviceInfoResponse
		if err := sip.XMLDecode(body, &msg); err != nil {
			return ErrXMLDecode
		}
		if msg.SN <= 0 || !isGBDeviceIdentifier(strings.TrimSpace(msg.DeviceID)) || !isGBResultValue(msg.Result) {
			return fmt.Errorf("invalid DeviceInfo response envelope")
		}
		if err := validateVersionedInfo(version, "DeviceInfo", msg.Info, msg.ExtraInfo); err != nil {
			return err
		}
		for _, count := range []*int{msg.Channel, msg.MaxCamera, msg.MaxAlarm} {
			if count != nil && *count < 0 {
				return fmt.Errorf("DeviceInfo channel counts must not be negative")
			}
		}
		return nil
	case "ConfigDownload":
		return validateConfigDownloadResponseForVersion(body, version)
	case "MobilePosition":
		// MobilePosition 不产生查询响应。
		return nil
	default:
		return validateGenericQueryPayload(version, cmdType, body)
	}
}

func (g *GB28181API) respondCascadeCatalog(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	startAt, endAt, err := cascadeCatalogQueryTimes(query)
	if err != nil {
		return err
	}
	channels, err := g.loadCascadeCatalogChannelsInRange(ctx, worker.platform, worker.protocolVersion(), startAt, endAt)
	if err != nil {
		return sendCascadeQueryError(ctx, worker, query)
	}
	items := buildCascadeCatalogItems(channels, worker.platform, worker.protocolVersion())
	responseDeviceID := strings.TrimSpace(query.DeviceID)
	if responseDeviceID == "" || responseDeviceID == "*" {
		responseDeviceID = worker.platform.localID
	}
	items = filterCascadeCatalogNotifyItems(items, responseDeviceID, worker.platform.localID)
	if err := validateCascadeCatalogItemsForVersion(items, worker.protocolVersion()); err != nil {
		slog.Warn("refuse invalid cascade Catalog response", "upstream", worker.platform.name, "err", err)
		return sendCascadeQueryError(ctx, worker, query)
	}
	if len(items) == 0 {
		response := cascadeCatalogResponse{
			CmdType: "Catalog", SN: query.SN, DeviceID: responseDeviceID,
		}
		if worker.protocolVersion() == GBVersion10 {
			response.DeviceList = &cascadeCatalogDeviceList{}
		}
		return sendCascadeXML(ctx, worker, response)
	}
	for start := 0; start < len(items); start += cascadeCatalogChunkSize {
		end := min(start+cascadeCatalogChunkSize, len(items))
		if err := sendCascadeXML(ctx, worker, cascadeCatalogResponse{
			CmdType: "Catalog", SN: query.SN, DeviceID: responseDeviceID, SumNum: len(items),
			DeviceList: &cascadeCatalogDeviceList{Num: end - start, Items: items[start:end]},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (g *GB28181API) notifyCascadeCatalog(ctx context.Context) {
	if g == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	reconcileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	g.reconcileCascadeDownstreamSubscriptions(reconcileCtx)
	cancel()
	now := time.Now()
	g.eventSubscribers.Range(func(key, value any) bool {
		sub, ok := value.(*eventSubscription)
		if !ok || sub == nil {
			g.eventSubscribers.CompareAndDelete(key, value)
			return true
		}
		sub.mu.Lock()
		expiresAt := sub.ExpiresAt
		cascade := sub.Cascade
		cmdType := sub.CmdType
		sub.mu.Unlock()
		if subscriptionExpiredAt(now, expiresAt) {
			// 统一由订阅清理器删除并释放下级引用，避免和续订并发互相覆盖。
			return true
		}
		if cascade == nil || !strings.EqualFold(cmdType, "Catalog") {
			return true
		}
		subscription := sub
		upstream := cascade.platform.name
		g.startCascadeLifecycleTask(ctx, cascade, func(taskCtx context.Context) {
			notifyCtx, cancel := context.WithTimeout(taskCtx, 30*time.Second)
			defer cancel()
			if err := g.sendCascadeCatalogNotify(notifyCtx, subscription); err != nil && !errors.Is(err, context.Canceled) {
				slog.Warn("send cascade Catalog NOTIFY failed", "upstream", upstream, "err", err)
			}
		})
		return true
	})
}

func (g *GB28181API) sendCascadeCatalogNotify(ctx context.Context, sub *eventSubscription) error {
	return g.sendCascadeCatalogNotifyMode(ctx, sub, false)
}

func (g *GB28181API) sendCascadeInitialCatalogNotify(ctx context.Context, sub *eventSubscription) error {
	return g.sendCascadeCatalogNotifyMode(ctx, sub, true)
}

func (g *GB28181API) sendCascadeCatalogNotifyMode(ctx context.Context, sub *eventSubscription, initial bool) error {
	if sub == nil {
		return fmt.Errorf("cascade subscription is unavailable")
	}
	// 同一订阅的快照比较、分包发送和快照提交必须串行，避免并发目录变化重复发送同一批增量。
	sub.catalogMu.Lock()
	defer sub.catalogMu.Unlock()
	sub.mu.Lock()
	worker := sub.Cascade
	dialogCSeq := sub.RemoteCSeq
	expiresAt := sub.ExpiresAt
	subscriptionDeviceID := sub.DeviceID
	filter := sub.Filter
	sub.mu.Unlock()
	if worker == nil || subscriptionExpiredAt(time.Now(), expiresAt) {
		return fmt.Errorf("cascade subscription is unavailable")
	}
	startAt, _, err := parseSubscriptionTime(filter.CatalogStartTime)
	if err != nil {
		return err
	}
	endAt, _, err := parseSubscriptionTime(filter.CatalogEndTime)
	if err != nil {
		return err
	}
	channels, err := g.loadCascadeCatalogChannelsInRange(ctx, worker.platform, worker.protocolVersion(), startAt, endAt)
	if err != nil {
		return err
	}
	visibleItems := buildCascadeCatalogItems(channels, worker.platform, worker.protocolVersion())
	visibleItems = filterCascadeCatalogNotifyItems(visibleItems, subscriptionDeviceID, worker.platform.localID)
	if err := validateCascadeCatalogItemsForVersion(visibleItems, worker.protocolVersion()); err != nil {
		return err
	}
	nextSnapshot := catalogSnapshot(visibleItems)
	sub.mu.Lock()
	previous := sub.CatalogSnapshot
	sub.mu.Unlock()
	items, changed := prepareCascadeCatalogNotifyItemsForVersion(worker.protocolVersion(), previous, visibleItems, initial)
	if !changed {
		return nil
	}
	notifyDeviceID := strings.TrimSpace(subscriptionDeviceID)
	if notifyDeviceID == "" || notifyDeviceID == "*" {
		notifyDeviceID = worker.platform.localID
	}
	status := ""
	if initial {
		status = "OK"
	}
	sn := g.nextQuerySN()
	if len(items) == 0 {
		body, err := encodeCascadeCatalogNotify(worker.protocolVersion(), newCascadeCatalogNotify(notifyDeviceID, sn, status, 0, nil))
		if err != nil {
			return err
		}
		sent, err := g.sendCascadeCatalogNotifyPayload(ctx, sub, worker, dialogCSeq, body)
		if err != nil {
			return err
		}
		if !sent {
			return nil
		}
		sub.mu.Lock()
		if sub.Cascade != worker || sub.RemoteCSeq != dialogCSeq {
			sub.mu.Unlock()
			return nil
		}
		sub.CatalogSnapshot = nextSnapshot
		sub.mu.Unlock()
		return nil
	}
	for start := 0; start < len(items); start += cascadeCatalogChunkSize {
		end := min(start+cascadeCatalogChunkSize, len(items))
		body, err := encodeCascadeCatalogNotify(worker.protocolVersion(), newCascadeCatalogNotify(notifyDeviceID, sn, status, len(items), items[start:end]))
		if err != nil {
			return err
		}
		sent, err := g.sendCascadeCatalogNotifyPayload(ctx, sub, worker, dialogCSeq, body)
		if err != nil {
			return err
		}
		if !sent {
			return nil
		}
	}
	sub.mu.Lock()
	if sub.Cascade != worker || sub.RemoteCSeq != dialogCSeq {
		sub.mu.Unlock()
		return nil
	}
	sub.CatalogSnapshot = nextSnapshot
	sub.mu.Unlock()
	return nil
}

func (g *GB28181API) sendCascadeCatalogNotifyPayload(ctx context.Context, sub *eventSubscription, worker *cascadeWorker, dialogCSeq uint32, body []byte) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	batch := eventNotifyBatch{
		key: sub.Key, cascade: worker, dialogCSeq: dialogCSeq,
		cmdType: "Catalog", deviceID: sub.DeviceID, payloads: [][]byte{body}, byteCount: len(body),
	}
	for {
		sent, response, err := g.sendEventNotifyForDispatchAttemptContext(ctx, sub, worker, dialogCSeq, "Catalog", body)
		if !sent || err == nil || !retryableEventNotifyFailure(response, err) {
			return sent, err
		}
		batch.attempts++
		if batch.attempts >= eventNotifyMaxAttempts {
			g.detachEventSubscriptionAfterDeliveryExhausted(ctx, sub, worker, dialogCSeq)
			return true, fmt.Errorf("Catalog NOTIFY delivery exhausted after %d attempts: %w", batch.attempts, err)
		}
		if !g.waitEventNotifyDispatchRetry(ctx, sub, worker, dialogCSeq, batch.attempts, response) {
			return false, ctx.Err()
		}
	}
}

func newCascadeCatalogNotify(deviceID string, sn int, status string, total int, items []cascadeCatalogItem) cascadeCatalogNotify {
	return cascadeCatalogNotify{
		CmdType: "Catalog", SN: sn, DeviceID: deviceID, Status: status, SumNum: total,
		DeviceList: cascadeCatalogDeviceList{Num: len(items), Items: items},
	}
}

func (g *GB28181API) seedCascadeCatalogSnapshot(ctx context.Context, sub *eventSubscription) error {
	if sub == nil {
		return nil
	}
	sub.catalogMu.Lock()
	defer sub.catalogMu.Unlock()
	sub.mu.Lock()
	worker := sub.Cascade
	deviceID := sub.DeviceID
	filter := sub.Filter
	alreadySeeded := sub.CatalogSnapshot != nil
	sub.mu.Unlock()
	if worker == nil || alreadySeeded {
		return nil
	}
	startAt, _, err := parseSubscriptionTime(filter.CatalogStartTime)
	if err != nil {
		return err
	}
	endAt, _, err := parseSubscriptionTime(filter.CatalogEndTime)
	if err != nil {
		return err
	}
	channels, err := g.loadCascadeCatalogChannelsInRange(ctx, worker.platform, worker.protocolVersion(), startAt, endAt)
	if err != nil {
		return err
	}
	items := buildCascadeCatalogItems(channels, worker.platform, worker.protocolVersion())
	items = filterCascadeCatalogNotifyItems(items, deviceID, worker.platform.localID)
	if err := validateCascadeCatalogItemsForVersion(items, worker.protocolVersion()); err != nil {
		return err
	}
	snapshot := catalogSnapshot(items)
	sub.mu.Lock()
	if sub.CatalogSnapshot == nil {
		sub.CatalogSnapshot = snapshot
	}
	sub.mu.Unlock()
	return nil
}

func encodeCascadeCatalogNotify(version GBProtocolVersion, notify cascadeCatalogNotify) ([]byte, error) {
	root := "Notify"
	// 测试构造和删除增量可能复用旧快照，编码前统一绑定协商版本。
	for index := range notify.DeviceList.Items {
		notify.DeviceList.Items[index].protocolVersion = version
	}
	if version == GBVersion10 {
		root = "Response"
		// 2011 的目录通知复用 Catalog 查询应答结构，不包含 2014 新增的
		// 顶层 Status 和目录项 Event。复制切片，避免编码动作改写调用方快照。
		notify.Status = ""
		notify.DeviceList.Items = append([]cascadeCatalogItem(nil), notify.DeviceList.Items...)
		for index := range notify.DeviceList.Items {
			notify.DeviceList.Items[index].Event = ""
		}
	}
	notify.XMLName = xml.Name{Local: root}
	return sip.XMLEncode(notify)
}

func filterCascadeCatalogNotifyItems(items []cascadeCatalogItem, subscriptionDeviceID, localID string) []cascadeCatalogItem {
	targetID := strings.TrimSpace(subscriptionDeviceID)
	if targetID == "" || targetID == "*" || targetID == strings.TrimSpace(localID) {
		return items
	}
	return filterCascadeCatalogDirectorySubtree(items, targetID, classifyGBCatalogItem(targetID))
}

func filterCascadeCatalogDirectorySubtree(items []cascadeCatalogItem, targetID string, kind GBCatalogItemKind) []cascadeCatalogItem {
	included := map[string]struct{}{targetID: {}}
	for _, item := range items {
		itemID := strings.TrimSpace(item.DeviceID)
		itemKind := classifyGBCatalogItem(itemID)
		if itemID == targetID {
			included[itemID] = struct{}{}
			continue
		}
		if kind != GBCatalogItemAdministrative {
			continue
		}
		if itemKind == GBCatalogItemAdministrative && strings.HasPrefix(itemID, targetID) ||
			itemKind == GBCatalogItemDevice && strings.HasPrefix(strings.TrimSpace(item.CivilCode), targetID) {
			included[itemID] = struct{}{}
		}
	}
	for {
		changed := false
		for _, item := range items {
			deviceID := strings.TrimSpace(item.DeviceID)
			itemKind := classifyGBCatalogItem(deviceID)
			if deviceID == "" {
				continue
			}
			if _, exists := included[deviceID]; exists {
				continue
			}
			if !cascadeCatalogSubtreeKindAllowed(kind, itemKind) {
				continue
			}
			matches := false
			for _, parentID := range splitCascadeCatalogParentIDs(item.ParentID) {
				if _, exists := included[parentID]; exists {
					matches = true
					break
				}
			}
			if !matches && (kind == GBCatalogItemSystem || kind == GBCatalogItemBusinessGroup) {
				businessGroupID := strings.TrimSpace(item.BusinessGroupID)
				if businessGroupID == "" && item.Info != nil {
					businessGroupID = strings.TrimSpace(item.Info.BusinessGroupID)
				}
				_, matches = included[businessGroupID]
			}
			if matches {
				included[deviceID] = struct{}{}
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	filtered := make([]cascadeCatalogItem, 0, len(included))
	for _, item := range items {
		if _, exists := included[strings.TrimSpace(item.DeviceID)]; exists {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func cascadeCatalogSubtreeKindAllowed(rootKind, itemKind GBCatalogItemKind) bool {
	switch rootKind {
	case GBCatalogItemAdministrative:
		return itemKind == GBCatalogItemAdministrative || itemKind == GBCatalogItemDevice
	case GBCatalogItemSystem:
		return itemKind != GBCatalogItemUnknown
	case GBCatalogItemBusinessGroup:
		return itemKind == GBCatalogItemVirtualOrganization || itemKind == GBCatalogItemDevice
	case GBCatalogItemVirtualOrganization:
		return itemKind == GBCatalogItemVirtualOrganization || itemKind == GBCatalogItemDevice
	case GBCatalogItemDevice:
		return itemKind == GBCatalogItemDevice
	default:
		return false
	}
}

func prepareCascadeCatalogNotifyItems(items []cascadeCatalogItem, initial bool) []cascadeCatalogItem {
	out := make([]cascadeCatalogItem, 0, len(items))
	for _, item := range items {
		if initial {
			if strings.EqualFold(strings.TrimSpace(item.Status), "ON") {
				continue
			}
			item.Event = "OFF"
		} else {
			item.Event = "UPDATE"
		}
		out = append(out, item)
	}
	return out
}

func prepareCascadeCatalogNotifyItemsForVersion(
	version GBProtocolVersion,
	previous map[string]cascadeCatalogItem,
	current []cascadeCatalogItem,
	initial bool,
) ([]cascadeCatalogItem, bool) {
	if version == GBVersion10 {
		next := catalogSnapshot(current)
		if !initial && reflect.DeepEqual(previous, next) {
			return nil, false
		}
		// 2011 没有目录项 Event，删除和更新只能通过新的完整目录快照表达。
		items := append([]cascadeCatalogItem(nil), current...)
		for index := range items {
			items[index].Event = ""
		}
		return items, true
	}
	if initial {
		return prepareCascadeCatalogNotifyItems(current, true), true
	}
	items := diffCascadeCatalogNotifyItems(previous, current)
	return items, len(items) > 0
}

func catalogSnapshot(items []cascadeCatalogItem) map[string]cascadeCatalogItem {
	snapshot := make(map[string]cascadeCatalogItem, len(items))
	for _, item := range items {
		deviceID := strings.TrimSpace(item.DeviceID)
		if deviceID == "" {
			continue
		}
		item.Event = ""
		item.ExtraInfo = append([]string(nil), item.ExtraInfo...)
		if item.Info != nil {
			info := *item.Info
			item.Info = &info
		}
		snapshot[deviceID] = item
	}
	return snapshot
}

func diffCascadeCatalogNotifyItems(previous map[string]cascadeCatalogItem, current []cascadeCatalogItem) []cascadeCatalogItem {
	currentSnapshot := catalogSnapshot(current)
	changes := make([]cascadeCatalogItem, 0, len(previous)+len(currentSnapshot))
	for deviceID, item := range currentSnapshot {
		old, existed := previous[deviceID]
		if !existed {
			item.Event = "ADD"
			changes = append(changes, item)
			continue
		}
		if reflect.DeepEqual(old, item) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(old.Status), strings.TrimSpace(item.Status)) {
			switch strings.ToUpper(strings.TrimSpace(item.Status)) {
			case "ON", "ONLINE":
				item.Event = "ON"
			case "OFF", "OFFLINE":
				item.Event = "OFF"
			default:
				item.Event = "UPDATE"
			}
		} else {
			item.Event = "UPDATE"
		}
		changes = append(changes, item)
	}
	for deviceID, item := range previous {
		if _, exists := currentSnapshot[deviceID]; exists {
			continue
		}
		item.Event = "DEL"
		changes = append(changes, item)
	}
	sort.Slice(changes, func(i, j int) bool {
		return strings.TrimSpace(changes[i].DeviceID) < strings.TrimSpace(changes[j].DeviceID)
	})
	return changes
}

func (g *GB28181API) loadCascadeChannels(ctx context.Context, platform cascadePlatform) ([]*ipc.Channel, error) {
	if len(platform.sharedChannels) == 0 {
		return []*ipc.Channel{}, nil
	}
	if g == nil || g.core.Store() == nil {
		return nil, fmt.Errorf("channel store is unavailable")
	}
	var rows []*ipc.Channel
	if _, err := g.core.Store().Channel().List(ctx, &rows, web.NewPagerFilterMaxSize(), orm.Where("channel_id IN ?", platform.sharedChannels)); err != nil {
		return nil, fmt.Errorf("list shared cascade channels: %w", err)
	}
	byID := make(map[string]*ipc.Channel, len(rows))
	for _, channel := range rows {
		if channel != nil {
			byID[channel.ChannelID] = channel
		}
	}
	ordered := make([]*ipc.Channel, 0, len(platform.sharedChannels))
	for _, channelID := range platform.sharedChannels {
		if channel := byID[channelID]; channel != nil {
			ordered = append(ordered, channel)
		}
	}
	return ordered, nil
}

// loadCascadeCatalogChannels 为 2014 及后续版本构造可见目录拓扑。
// 显式共享项仍由 shared_channels 控制；目录节点只能从共享项已确认的 ParentID、
// BusinessGroupID、CivilCode 和来源平台关系补齐，避免越过共享白名单枚举其他设备。
func (g *GB28181API) loadCascadeCatalogChannels(ctx context.Context, platform cascadePlatform, version GBProtocolVersion) ([]*ipc.Channel, error) {
	return g.loadCascadeCatalogChannelsInRange(ctx, platform, version, time.Time{}, time.Time{})
}

func (g *GB28181API) loadCascadeCatalogChannelsInRange(ctx context.Context, platform cascadePlatform, version GBProtocolVersion, startAt, endAt time.Time) ([]*ipc.Channel, error) {
	if !version.AtLeast(GBVersion11) {
		channels, err := g.loadCascadeChannels(ctx, platform)
		if err != nil {
			return nil, err
		}
		return filterCascadeChannelsByCreatedAt(channels, startAt, endAt), nil
	}
	if len(platform.sharedChannels) == 0 {
		return []*ipc.Channel{}, nil
	}
	if g == nil || g.core.Store() == nil {
		return nil, fmt.Errorf("channel store is unavailable")
	}

	rows, err := g.listCascadeCatalogChannels(ctx, platform.sharedChannels)
	if err != nil {
		return nil, err
	}
	rows = filterCascadeChannelsByCreatedAt(rows, startAt, endAt)
	sharedChannels := platform.sharedChannels
	if !startAt.IsZero() || !endAt.IsZero() {
		available := make(map[string]struct{}, len(rows))
		for _, channel := range rows {
			if channel != nil {
				available[strings.TrimSpace(channel.ChannelID)] = struct{}{}
			}
		}
		sharedChannels = make([]string, 0, len(platform.sharedChannels))
		for _, channelID := range platform.sharedChannels {
			channelID = strings.TrimSpace(channelID)
			if _, ok := available[channelID]; ok {
				sharedChannels = append(sharedChannels, channelID)
			}
		}
		if len(sharedChannels) == 0 {
			return []*ipc.Channel{}, nil
		}
	}
	allRows := append([]*ipc.Channel(nil), rows...)
	loaded := make(map[string]struct{}, len(sharedChannels))
	for _, channelID := range sharedChannels {
		loaded[strings.TrimSpace(channelID)] = struct{}{}
	}
	syntheticByRelation := make(map[string]*ipc.Channel)

	for {
		for _, channel := range allRows {
			for _, synthetic := range syntheticCascadeCatalogRelations(channel, platform.localID) {
				key := cascadeCatalogSyntheticRelationKey(synthetic)
				if _, exists := syntheticByRelation[key]; !exists {
					syntheticByRelation[key] = synthetic
				}
			}
		}

		pendingSet := make(map[string]struct{})
		for _, synthetic := range syntheticByRelation {
			channelID := strings.TrimSpace(synthetic.ChannelID)
			if channelID == "" {
				continue
			}
			if _, exists := loaded[channelID]; !exists {
				pendingSet[channelID] = struct{}{}
			}
		}
		if len(pendingSet) == 0 {
			break
		}
		pending := make([]string, 0, len(pendingSet))
		for channelID := range pendingSet {
			loaded[channelID] = struct{}{}
			pending = append(pending, channelID)
		}
		sort.Strings(pending)
		parentRows, listErr := g.listCascadeCatalogChannels(ctx, pending)
		if listErr != nil {
			return nil, listErr
		}
		allRows = append(allRows, parentRows...)
	}
	syntheticKeys := make([]string, 0, len(syntheticByRelation))
	for key := range syntheticByRelation {
		syntheticKeys = append(syntheticKeys, key)
	}
	sort.Strings(syntheticKeys)
	for _, key := range syntheticKeys {
		allRows = append(allRows, syntheticByRelation[key])
	}

	merged := mergeCascadeCatalogChannels(allRows, platform.localID)
	shared := make(map[string]struct{}, len(sharedChannels))
	for _, channelID := range sharedChannels {
		shared[strings.TrimSpace(channelID)] = struct{}{}
	}
	directories := make([]string, 0, len(merged))
	for channelID := range merged {
		if _, exists := shared[channelID]; !exists {
			directories = append(directories, channelID)
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		leftRank := cascadeCatalogDirectoryKindRank(classifyGBCatalogItem(directories[i]))
		rightRank := cascadeCatalogDirectoryKindRank(classifyGBCatalogItem(directories[j]))
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return directories[i] < directories[j]
	})
	ordered := make([]*ipc.Channel, 0, len(merged))
	seen := make(map[string]struct{}, len(merged))
	for _, channelID := range directories {
		ordered = append(ordered, merged[channelID])
		seen[channelID] = struct{}{}
	}
	for _, channelID := range sharedChannels {
		channelID = strings.TrimSpace(channelID)
		if _, exists := seen[channelID]; exists {
			continue
		}
		if channel := merged[channelID]; channel != nil {
			ordered = append(ordered, channel)
			seen[channelID] = struct{}{}
		}
	}
	return ordered, nil
}

func filterCascadeChannelsByCreatedAt(channels []*ipc.Channel, startAt, endAt time.Time) []*ipc.Channel {
	if startAt.IsZero() && endAt.IsZero() {
		return channels
	}
	filtered := make([]*ipc.Channel, 0, len(channels))
	for _, channel := range channels {
		if channel == nil || channel.CreatedAt.Time.IsZero() {
			continue
		}
		createdAt := channel.CreatedAt.Time
		if !startAt.IsZero() && createdAt.Before(startAt) || !endAt.IsZero() && createdAt.After(endAt) {
			continue
		}
		filtered = append(filtered, channel)
	}
	return filtered
}

func cascadeCatalogDirectoryKindRank(kind GBCatalogItemKind) int {
	switch kind {
	case GBCatalogItemAdministrative:
		return 0
	case GBCatalogItemSystem:
		return 1
	case GBCatalogItemBusinessGroup:
		return 2
	case GBCatalogItemVirtualOrganization:
		return 3
	case GBCatalogItemDevice:
		return 4
	default:
		return 5
	}
}

func cascadeCatalogSourceSystem(channel *ipc.Channel, localID string) string {
	if channel != nil {
		sourceID := strings.TrimSpace(channel.DeviceID)
		if sourceID != strings.TrimSpace(channel.ChannelID) && classifyGBCatalogItem(sourceID) == GBCatalogItemSystem {
			return sourceID
		}
	}
	return strings.TrimSpace(localID)
}

func syntheticCascadeCatalogRelations(channel *ipc.Channel, localID string) []*ipc.Channel {
	if channel == nil {
		return nil
	}
	sourceID := cascadeCatalogSourceSystem(channel, localID)
	channelID := strings.TrimSpace(channel.ChannelID)
	ext := channel.Ext.GBCatalog
	relations := make([]*ipc.Channel, 0, 8)
	appendRelation := func(id, parentID, businessGroupID, civilCode string) {
		id = strings.TrimSpace(id)
		if id == "" || id == strings.TrimSpace(localID) || classifyGBCatalogItem(id) == GBCatalogItemUnknown {
			return
		}
		relations = append(relations, newSyntheticCascadeCatalogChannel(id, sourceID, parentID, businessGroupID, civilCode))
	}

	if sourceID != "" && sourceID != strings.TrimSpace(localID) && sourceID != channelID {
		appendRelation(sourceID, localID, "", "")
	}

	businessGroupID := ""
	civilCode := ""
	if ext != nil {
		businessGroupID = strings.TrimSpace(ext.BusinessGroupID)
		civilCode = strings.TrimSpace(ext.CivilCode)
		for _, parentID := range splitCascadeCatalogParentIDs(ext.ParentID) {
			parentKind := classifyGBCatalogItem(parentID)
			parentParentID := sourceID
			parentBusinessGroupID := ""
			parentCivilCode := ""
			switch parentKind {
			case GBCatalogItemSystem:
				if parentID == sourceID {
					parentParentID = localID
				}
			case GBCatalogItemAdministrative:
				parentCivilCode = parentID
				parentParentID = cascadeAdministrativeParentID(parentID, sourceID)
			case GBCatalogItemBusinessGroup:
			case GBCatalogItemVirtualOrganization:
				parentParentID = ""
				parentBusinessGroupID = businessGroupID
			case GBCatalogItemDevice:
			default:
				continue
			}
			appendRelation(parentID, parentParentID, parentBusinessGroupID, parentCivilCode)
		}
	}
	if businessGroupID != "" {
		appendRelation(businessGroupID, sourceID, "", civilCode)
	}
	for _, administrativeID := range cascadeAdministrativePrefixes(civilCode) {
		appendRelation(administrativeID, cascadeAdministrativeParentID(administrativeID, sourceID), "", administrativeID)
	}
	return relations
}

func cascadeAdministrativePrefixes(civilCode string) []string {
	civilCode = strings.TrimSpace(civilCode)
	if !allDecimalDigits(civilCode) {
		return nil
	}
	prefixes := make([]string, 0, 4)
	for _, length := range []int{2, 4, 6, 8} {
		if len(civilCode) < length {
			break
		}
		prefixes = append(prefixes, civilCode[:length])
	}
	return prefixes
}

func cascadeAdministrativeParentID(administrativeID, systemID string) string {
	switch len(strings.TrimSpace(administrativeID)) {
	case 4:
		return administrativeID[:2]
	case 6:
		return administrativeID[:4]
	case 8:
		return administrativeID[:6]
	default:
		return strings.TrimSpace(systemID)
	}
}

func newSyntheticCascadeCatalogChannel(channelID, sourceID, parentID, businessGroupID, civilCode string) *ipc.Channel {
	return &ipc.Channel{
		DeviceID:  strings.TrimSpace(sourceID),
		ChannelID: strings.TrimSpace(channelID),
		Name:      strings.TrimSpace(channelID),
		// 派生节点只有目录关系，没有自身状态证据。域间初始订阅要求先默认在线，不能用子项状态推导父目录离线。
		IsOnline: true,
		Type:     ipc.TypeGB28181,
		Ext: ipc.DeviceExt{GBCatalog: &ipc.GBCatalogExt{
			Kind:            classifyGBCatalogItem(channelID),
			CivilCode:       strings.TrimSpace(civilCode),
			ParentID:        strings.TrimSpace(parentID),
			BusinessGroupID: strings.TrimSpace(businessGroupID),
		}},
	}
}

func cascadeCatalogSyntheticRelationKey(channel *ipc.Channel) string {
	if channel == nil || channel.Ext.GBCatalog == nil {
		return ""
	}
	ext := channel.Ext.GBCatalog
	return strings.Join([]string{channel.ChannelID, ext.ParentID, ext.BusinessGroupID, ext.CivilCode}, "\x00")
}

func (g *GB28181API) listCascadeCatalogChannels(ctx context.Context, channelIDs []string) ([]*ipc.Channel, error) {
	if len(channelIDs) == 0 {
		return nil, nil
	}
	var rows []*ipc.Channel
	if _, err := g.core.Store().Channel().List(ctx, &rows, web.NewPagerFilterMaxSize(), orm.Where("channel_id IN ?", channelIDs)); err != nil {
		return nil, fmt.Errorf("list cascade Catalog channels: %w", err)
	}
	return rows, nil
}

func splitCascadeCatalogParentIDs(value string) []string {
	parts := strings.Split(value, "/")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, exists := seen[part]; exists {
			continue
		}
		seen[part] = struct{}{}
		out = append(out, part)
	}
	return out
}

func cascadeCatalogRowParentIDs(row *ipc.Channel, localID string) []string {
	if row == nil {
		return nil
	}
	rawParent := ""
	if row.Ext.GBCatalog != nil {
		rawParent = row.Ext.GBCatalog.ParentID
	}
	parents := splitCascadeCatalogParentIDs(rawParent)
	if len(parents) > 0 {
		return parents
	}
	channelID := strings.TrimSpace(row.ChannelID)
	kind := classifyGBCatalogItem(channelID)
	sourceID := cascadeCatalogSourceSystem(row, localID)
	if kind == GBCatalogItemVirtualOrganization {
		return nil
	}
	fallback := sourceID
	if kind == GBCatalogItemAdministrative {
		fallback = cascadeAdministrativeParentID(channelID, sourceID)
	}
	if fallback == "" || fallback == channelID {
		return nil
	}
	return []string{fallback}
}

func mergeCascadeCatalogChannels(rows []*ipc.Channel, localID string) map[string]*ipc.Channel {
	merged := make(map[string]*ipc.Channel)
	parents := make(map[string]map[string]struct{})
	for _, row := range rows {
		if row == nil {
			continue
		}
		channelID := strings.TrimSpace(row.ChannelID)
		if channelID == "" {
			continue
		}
		current := merged[channelID]
		if current == nil {
			clone := *row
			if row.Ext.GBCatalog != nil {
				ext := *row.Ext.GBCatalog
				clone.Ext.GBCatalog = &ext
			}
			current = &clone
			merged[channelID] = current
		} else if row.IsOnline && strings.TrimSpace(row.ID) != "" {
			// 派生关系不能覆盖持久化目录节点的真实离线状态。
			current.IsOnline = true
		}
		if current.Ext.GBCatalog == nil && row.Ext.GBCatalog != nil {
			ext := *row.Ext.GBCatalog
			current.Ext.GBCatalog = &ext
		}

		parentSet := parents[channelID]
		if parentSet == nil {
			parentSet = make(map[string]struct{})
			parents[channelID] = parentSet
		}
		for _, parentID := range cascadeCatalogRowParentIDs(row, localID) {
			if parentID != channelID {
				parentSet[parentID] = struct{}{}
			}
		}
	}
	for channelID, channel := range merged {
		if channel.Ext.GBCatalog == nil {
			channel.Ext.GBCatalog = &ipc.GBCatalogExt{Kind: classifyGBCatalogItem(channelID)}
		}
		parentIDs := make([]string, 0, len(parents[channelID]))
		for parentID := range parents[channelID] {
			parentIDs = append(parentIDs, parentID)
		}
		sort.Strings(parentIDs)
		channel.Ext.GBCatalog.ParentID = strings.Join(parentIDs, "/")
	}
	return merged
}

func buildCascadeCatalogItems(channels []*ipc.Channel, platform cascadePlatform, version GBProtocolVersion) []cascadeCatalogItem {
	capacity := len(channels)
	includePlatformRoot := version == GBVersion30 && validateCascadePlatformIdentifier(strings.TrimSpace(platform.localID)) == nil
	if includePlatformRoot {
		capacity++
	}
	items := make([]cascadeCatalogItem, 0, capacity)
	if includePlatformRoot {
		items = append(items, cascadeLocalPlatformCatalogItem(platform))
	}
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		exposedID := platform.channelIDMap[channel.ChannelID]
		if exposedID == "" && version.AtLeast(GBVersion11) && classifyGBCatalogItem(channel.ChannelID) != GBCatalogItemUnknown {
			exposedID = strings.TrimSpace(channel.ChannelID)
		}
		if exposedID == "" {
			continue
		}
		ext := channel.Ext.GBCatalog
		item := cascadeCatalogItem{
			protocolVersion: version,
			DeviceID:        exposedID, Name: firstPresentString(channel.Name, exposedID),
			Manufacturer: channel.Ext.Manufacturer, Model: channel.Ext.Model,
			CivilCode: cascadeCatalogCivilCode(exposedID, platform.localDomain), ParentID: platform.localID,
			RegisterWay: 1, Status: "OFF",
		}
		if channel.IsOnline {
			item.Status = "ON"
		}
		if ext != nil {
			item.CivilCode = cascadeCatalogCivilCode(exposedID, ext.CivilCode, item.CivilCode)
			item.Parental = ext.Parental
			if version.AtLeast(GBVersion11) && (strings.TrimSpace(ext.ParentID) != "" || classifyGBCatalogItem(channel.ChannelID) == GBCatalogItemVirtualOrganization) {
				item.ParentID = mapCascadeCatalogParentIDs(ext.ParentID, platform)
			}
			item.Block = ext.Block
			item.Address = ext.Address
			if version == GBVersion30 {
				item.SecurityLevelCode = ext.SecurityLevelCode
			}
			if ext.RegisterWay > 0 {
				item.RegisterWay = ext.RegisterWay
			}
			item.Secrecy = ext.Secrecy
			item.IPAddress = ext.IPAddress
			item.Port = ext.Port
			item.Longitude = ext.Longitude
			item.Latitude = ext.Latitude
		}
		if version != GBVersion30 {
			owner, safetyWay, certNum, certifiable, errCode, endTime := "", 0, "", 0, 0, ""
			if ext != nil {
				owner, safetyWay, certNum = ext.Owner, ext.SafetyWay, ext.CertNum
				certifiable, errCode, endTime = ext.Certifiable, ext.ErrCode, ext.EndTime
			}
			item.Owner, item.SafetyWay, item.CertNum = &owner, &safetyWay, &certNum
			item.Certifiable, item.ErrCode, item.EndTime = &certifiable, &errCode, &endTime
		}
		if version.AtLeast(GBVersion11) {
			item.Info = &cascadeCatalogInfo{}
			if channel.PTZType > 0 {
				item.Info.PTZType = strconv.Itoa(channel.PTZType)
			}
			if ext != nil {
				if version == GBVersion30 && strings.TrimSpace(ext.PTZTypeList) != "" {
					item.Info.PTZType = strings.TrimSpace(ext.PTZTypeList)
				}
				if version != GBVersion30 {
					item.Info.PositionType = ext.PositionType
					item.Info.UseType = ext.UseType
				}
				item.Info.RoomType = ext.RoomType
				item.Info.SupplyLightType = ext.SupplyLightType
				item.Info.DirectionType = ext.DirectionType
				item.Info.Resolution = ext.Resolution
				if version == GBVersion30 {
					item.BusinessGroupID = mapCascadeCatalogIdentifier(ext.BusinessGroupID, platform)
				} else {
					item.Info.BusinessGroupID = ext.BusinessGroupID
				}
				if version == GBVersion30 {
					item.Info.PhotoelectricImagingType = ext.PhotoelectricImagingType
					item.Info.CapturePositionType = ext.CapturePositionType
					item.Info.StreamNumberList = ext.StreamNumberList
					item.Info.DownloadSpeed = ext.DownloadSpeed
					item.Info.SVCSpaceSupportMode = ext.SVCSpaceSupportMode
					item.Info.SVCTimeSupportMode = ext.SVCTimeSupportMode
					item.Info.SSVCRatioSupportList = ext.SSVCRatioSupportList
					item.Info.MobileDeviceType = ext.MobileDeviceType
					item.Info.HorizontalFieldAngle = ext.HorizontalFieldAngle
					item.Info.VerticalFieldAngle = ext.VerticalFieldAngle
					item.Info.MaxViewDistance = ext.MaxViewDistance
					item.Info.GrassrootsCode = ext.GrassrootsCode
					item.Info.PointType = ext.PointType
					item.Info.PointCommonName = ext.PointCommonName
					item.Info.MAC = ext.MAC
					item.Info.FunctionType = ext.FunctionType
					item.Info.EncodeType = ext.EncodeType
					item.Info.InstallTime = ext.InstallTime
					item.Info.ManagementUnit = ext.ManagementUnit
					if ext.ContactInfo != "" || ext.PointType == 1 {
						contactInfo := ext.ContactInfo
						item.Info.ContactInfo = &contactInfo
					}
					if ext.RecordSaveDays != 0 || ext.PointType == 1 {
						recordSaveDays := ext.RecordSaveDays
						item.Info.RecordSaveDays = &recordSaveDays
					}
					item.Info.IndustrialClassification = ext.IndustrialClassification
				}
			}
			if version == GBVersion30 && cascadeCatalogInfoEmpty(item.Info) {
				item.Info = nil
			}
		}
		if version.AtLeast(GBVersion30) && ext != nil {
			mappingPlatform := withCascadeIdentifierMapping(platform, channel.DeviceID, platform.localID)
			item.ExtraInfo = extractCascadeCatalogExtraInfo(ext, mappingPlatform, channel.ChannelID, exposedID)
		}
		items = append(items, item)
	}
	return items
}

func cascadeLocalPlatformCatalogItem(platform cascadePlatform) cascadeCatalogItem {
	civilCode := ""
	localID := strings.TrimSpace(platform.localID)
	if len(localID) >= 8 && allDecimalDigits(localID[:8]) {
		civilCode = localID[:8]
	}
	return cascadeCatalogItem{
		protocolVersion: GBVersion30,
		DeviceID:        localID,
		Name:            firstPresentString(platform.name, localID),
		Manufacturer:    "GoWVP",
		Model:           "OWL",
		CivilCode:       cascadeCatalogCivilCode(localID, civilCode),
		Address:         strings.TrimSpace(platform.localHost),
		Parental:        1,
		RegisterWay:     1,
		Status:          "ON",
		IPAddress:       strings.TrimSpace(platform.localHost),
		Port:            platform.localPort,
	}
}

func cascadeCatalogCivilCode(deviceID string, candidates ...string) string {
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if validCatalogAdministrativeCode(candidate) {
			return candidate
		}
	}
	deviceID = strings.TrimSpace(deviceID)
	if len(deviceID) >= 8 && allDecimalDigits(deviceID[:8]) {
		return deviceID[:8]
	}
	return ""
}

func cascadeCatalogInfoEmpty(info *cascadeCatalogInfo) bool {
	if info == nil {
		return true
	}
	return *info == (cascadeCatalogInfo{})
}

func validateCascadeCatalogItemsForVersion(items []cascadeCatalogItem, version GBProtocolVersion) error {
	if version != GBVersion30 {
		return nil
	}
	for _, item := range items {
		decoded := Channels{
			ChannelID: item.DeviceID, Name: item.Name, Manufacturer: item.Manufacturer,
			Model: item.Model, CivilCode: item.CivilCode, Block: item.Block, Address: item.Address,
			Parental: item.Parental, ParentID: item.ParentID, RegisterWay: item.RegisterWay,
			SecurityLevelCode: item.SecurityLevelCode, Secrecy: item.Secrecy,
			IPAddress: item.IPAddress, Port: item.Port, Status: item.Status,
			Longitude: item.Longitude, Latitude: item.Latitude, BusinessGroupID: item.BusinessGroupID,
			hasName: true,
		}
		kind := classifyGBCatalogItem(item.DeviceID)
		if kind == GBCatalogItemSystem || kind == GBCatalogItemDevice {
			decoded.hasManufacturer = true
			decoded.hasModel = true
			decoded.hasAddress = true
			decoded.hasRegisterWay = true
			decoded.hasSecrecy = true
		}
		if kind == GBCatalogItemDevice {
			decoded.hasParental = true
		}
		if item.Info != nil {
			decoded.Info = CatalogItemInfo{
				XMLName: xml.Name{Local: "Info"}, PTZTypeList: item.Info.PTZType,
				PhotoelectricImagingType: item.Info.PhotoelectricImagingType,
				CapturePositionType:      item.Info.CapturePositionType, RoomType: item.Info.RoomType,
				SupplyLightType: item.Info.SupplyLightType, DirectionType: item.Info.DirectionType,
				Resolution: item.Info.Resolution, StreamNumberList: item.Info.StreamNumberList,
				DownloadSpeed: item.Info.DownloadSpeed, SVCSpaceSupportMode: item.Info.SVCSpaceSupportMode,
				SVCTimeSupportMode: item.Info.SVCTimeSupportMode, SSVCRatioSupportList: item.Info.SSVCRatioSupportList,
				MobileDeviceType: item.Info.MobileDeviceType, HorizontalFieldAngle: item.Info.HorizontalFieldAngle,
				VerticalFieldAngle: item.Info.VerticalFieldAngle, MaxViewDistance: item.Info.MaxViewDistance,
				GrassrootsCode: item.Info.GrassrootsCode, PointType: item.Info.PointType,
				PointCommonName: item.Info.PointCommonName, MAC: item.Info.MAC,
				FunctionType: item.Info.FunctionType, EncodeType: item.Info.EncodeType,
				InstallTime: item.Info.InstallTime, ManagementUnit: item.Info.ManagementUnit,
				IndustrialClassification: item.Info.IndustrialClassification,
			}
			decoded.Info.hasPTZType = strings.TrimSpace(item.Info.PTZType) != ""
			decoded.Info.hasPointType = item.Info.PointType != 0
			decoded.Info.hasMobileDeviceType = item.Info.MobileDeviceType != 0
			decoded.Info.hasHorizontalFieldAngle = item.Info.HorizontalFieldAngle != 0
			decoded.Info.hasVerticalFieldAngle = item.Info.VerticalFieldAngle != 0
			decoded.Info.hasInstallTime = strings.TrimSpace(item.Info.InstallTime) != ""
			if item.Info.ContactInfo != nil {
				decoded.Info.ContactInfo, decoded.Info.hasContactInfo = *item.Info.ContactInfo, true
			}
			if item.Info.RecordSaveDays != nil {
				decoded.Info.RecordSaveDays, decoded.Info.hasRecordSaveDays = *item.Info.RecordSaveDays, true
			}
			if item.Info.PointType == 1 || item.Info.PointType == 2 {
				decoded.hasLongitude, decoded.hasLatitude = true, true
			}
		}
		if err := validateCatalogItemValues(decoded, version); err != nil {
			return fmt.Errorf("Catalog item %s: %w", item.DeviceID, err)
		}
	}
	return nil
}

func mapCascadeCatalogParentIDs(value string, platform cascadePlatform) string {
	parents := splitCascadeCatalogParentIDs(value)
	mapped := make([]string, 0, len(parents))
	seen := make(map[string]struct{}, len(parents))
	for _, parentID := range parents {
		if exposedID := platform.channelIDMap[parentID]; exposedID != "" {
			parentID = exposedID
		}
		if _, exists := seen[parentID]; exists {
			continue
		}
		seen[parentID] = struct{}{}
		mapped = append(mapped, parentID)
	}
	return strings.Join(mapped, "/")
}

func mapCascadeCatalogIdentifier(value string, platform cascadePlatform) string {
	value = strings.TrimSpace(value)
	if exposedID := platform.channelIDMap[value]; exposedID != "" {
		return exposedID
	}
	return value
}

func (g *GB28181API) respondCascadeDeviceInfo(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	if query.DeviceID != worker.platform.localID {
		channel, err := g.loadCascadeExposedChannel(ctx, worker.platform, query.DeviceID)
		if err != nil {
			return sendCascadeQueryError(ctx, worker, query)
		}
		response := cascadeDeviceInfoResponse{
			CmdType: "DeviceInfo", SN: query.SN, DeviceID: query.DeviceID, Result: "OK",
			Manufacturer: channel.Ext.Manufacturer, Model: channel.Ext.Model, Firmware: channel.Ext.Firmware, Channel: 1,
		}
		version := worker.protocolVersion()
		response.DeviceName = cascadeDeviceInfoName(version, firstPresentString(channel.Name, channel.Ext.Name, query.DeviceID))
		applyCascadeDeviceInfoCompatibility(&response, version, "IPC", 1, 0)
		return sendCascadeXML(ctx, worker, response)
	}
	channels, err := g.loadCascadeChannels(ctx, worker.platform)
	if err != nil {
		return sendCascadeQueryError(ctx, worker, query)
	}
	count := len(channels)
	firmware := ""
	if g.boot != nil {
		firmware = strings.TrimSpace(g.boot.BuildVersion)
	}
	response := cascadeDeviceInfoResponse{
		CmdType: "DeviceInfo", SN: query.SN, DeviceID: worker.platform.localID, Result: "OK",
		Manufacturer: "GoWVP", Model: "OWL", Firmware: firmware, Channel: count,
	}
	version := worker.protocolVersion()
	response.DeviceName = cascadeDeviceInfoName(version, "GoWVP OWL")
	applyCascadeDeviceInfoCompatibility(&response, version, "NVR", count, 0)
	return sendCascadeXML(ctx, worker, response)
}

func (g *GB28181API) respondCascadeDeviceStatus(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	if query.DeviceID != worker.platform.localID {
		if worker.protocolVersion().AtLeast(GBVersion30) {
			return g.respondCascadeForwardedQuery(ctx, worker, query, deviceQueryActionDeviceStatus)
		}
		channel, err := g.loadCascadeExposedChannel(ctx, worker.platform, query.DeviceID)
		if err != nil {
			return sendCascadeQueryError(ctx, worker, query)
		}
		online := "OFFLINE"
		encode := "OFF"
		status := "ERROR"
		if channel.IsOnline {
			online = "ONLINE"
			encode = "ON"
			status = "OK"
		}
		return sendCascadeXML(ctx, worker, cascadeDeviceStatusResponse{
			CmdType: "DeviceStatus", SN: query.SN, DeviceID: query.DeviceID,
			Result: "OK", Online: online, Status: status, Encode: encode, Record: "OFF",
			DeviceTime: sip.FormatGBTime(time.Now(), "2006-01-02T15:04:05"), Alarm: cascadeAlarmStatusForVersion(worker.protocolVersion(), 0),
		})
	}
	return sendCascadeXML(ctx, worker, cascadeDeviceStatusResponse{
		CmdType: "DeviceStatus", SN: query.SN, DeviceID: worker.platform.localID,
		Result: "OK", Online: "ONLINE", Status: "OK", Encode: "ON", Record: "OFF",
		DeviceTime: sip.FormatGBTime(time.Now(), "2006-01-02T15:04:05"), Alarm: cascadeAlarmStatusForVersion(worker.protocolVersion(), 0),
	})
}

func (g *GB28181API) respondCascadeRecordInfo(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	startAt, endAt, err := cascadeRecordQueryTimes(query)
	if err != nil {
		return sendCascadeQueryError(ctx, worker, query)
	}
	if endAt.IsZero() {
		endAt = time.Now().In(sip.GBTimeLocation())
	}
	channels, err := g.cascadeRecordQueryChannels(ctx, worker.platform, query)
	if err != nil {
		return sendCascadeQueryError(ctx, worker, query)
	}
	queryCenter, queryFrontend := cascadeRecordQuerySources(worker.protocolVersion(), worker.platform, query)
	items := make([]RecordItem, 0)
	extraInfo := make([]string, 0)
	if queryCenter {
		centerItems, centerErr := g.queryCascadeCenterRecordItems(ctx, worker.platform, channels, query, startAt, endAt)
		if centerErr != nil {
			slog.Warn("query cascade center RecordInfo failed", "upstream", worker.platform.name, "err", centerErr)
		} else {
			items = append(items, centerItems...)
		}
	}
	if queryFrontend {
		frontendChannels := channels
		indistinct, _ := cascadeRecordIndistinctQuery(query)
		if worker.protocolVersion().AtLeast(GBVersion11) && (indistinct == nil || *indistinct == 0) {
			frontendChannels = cascadeRecordLocationChannels(channels, worker.platform, query.recordQueryLocationID)
		}
		frontendItems, frontendExtra, frontendErr := g.queryCascadeFrontendRecordItems(ctx, worker, frontendChannels, query, startAt, endAt)
		if frontendErr != nil {
			return sendCascadeQueryError(ctx, worker, query)
		}
		items = append(items, frontendItems...)
		extraInfo = append(extraInfo, frontendExtra...)
	}
	items = normalizeCascadeRecordItems(items)
	extraInfo = dedupeCascadeRecordExtraInfo(extraInfo)
	name := firstPresentString(worker.platform.name, query.DeviceID)
	if len(channels) == 1 && query.DeviceID != worker.platform.localID {
		name = firstPresentString(channels[0].Name, query.DeviceID)
	}
	return g.sendCascadeRecordItems(ctx, worker, query, items, name, extraInfo)
}

func cascadeRecordQuerySources(version GBProtocolVersion, platform cascadePlatform, query cascadeQueryEnvelope) (center, frontend bool) {
	if !version.AtLeast(GBVersion11) {
		return query.DeviceID == platform.localID, query.DeviceID != platform.localID
	}
	indistinct, _ := cascadeRecordIndistinctQuery(query)
	if indistinct != nil && *indistinct == 1 {
		return true, true
	}
	locationID := strings.TrimSpace(query.recordQueryLocationID)
	if locationID == "" {
		locationID = platform.localID
	}
	return locationID == platform.localID, locationID != platform.localID
}

func (g *GB28181API) cascadeRecordQueryChannels(ctx context.Context, platform cascadePlatform, query cascadeQueryEnvelope) ([]*ipc.Channel, error) {
	if query.DeviceID == platform.localID {
		return g.loadCascadeChannels(ctx, platform)
	}
	channel, err := g.loadCascadeExposedChannel(ctx, platform, query.DeviceID)
	if err != nil {
		return nil, err
	}
	return []*ipc.Channel{channel}, nil
}

func cascadeRecordLocationChannels(channels []*ipc.Channel, platform cascadePlatform, locationID string) []*ipc.Channel {
	localChannelID := platform.exposedChannelMap[strings.TrimSpace(locationID)]
	if localChannelID == "" {
		return nil
	}
	for _, channel := range channels {
		if channel != nil && channel.ChannelID == localChannelID {
			return []*ipc.Channel{channel}
		}
	}
	return nil
}

func (g *GB28181API) queryCascadeFrontendRecordItems(
	ctx context.Context,
	worker *cascadeWorker,
	channels []*ipc.Channel,
	query cascadeQueryEnvelope,
	startAt, endAt time.Time,
) ([]RecordItem, []string, error) {
	indistinct, err := cascadeRecordIndistinctQuery(query)
	if err != nil {
		return nil, nil, err
	}
	requireRecordLocation := worker.protocolVersion() == GBVersion30 && indistinct != nil && *indistinct == 1
	type channelResult struct {
		items     []RecordItem
		extraInfo []string
		err       error
	}
	results := make([]channelResult, len(channels))
	semaphore := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for index, channel := range channels {
		if channel == nil {
			continue
		}
		wg.Add(1)
		go func(index int, channel *ipc.Channel) {
			defer wg.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			localChannelID := channel.ChannelID
			exposedID := worker.platform.channelIDMap[localChannelID]
			if exposedID == "" {
				return
			}
			recordType, _ := normalizeRecordQueryType(query.Type)
			recordQuery := &RecordQueryInput{
				DeviceID: channel.DeviceID, ChannelID: localChannelID,
				Start: startAt.Unix(), End: endAt.Unix(), OmitStartTime: strings.TrimSpace(query.StartTime) == "", OmitEndTime: strings.TrimSpace(query.EndTime) == "",
				Timeout: 25 * time.Second, FilePath: query.FilePath, Address: query.Address, Secrecy: query.Secrecy,
				Type: recordType, RecorderID: query.RecorderID, IndistinctQuery: indistinct,
				StreamNumber: query.StreamNumber, AlarmMethod: query.AlarmMethod, AlarmType: query.AlarmType,
			}
			var result recordQueryResult
			var queryErr error
			if queryErr = g.validateCascadeRecordQueryTarget(recordQuery); queryErr != nil {
				// 允许平台级 RecordInfo 返回其他可用来源，但不能把高版本过滤器交给低版本设备钩子。
			} else if g.cascadeRecordResult != nil {
				result, queryErr = g.cascadeRecordResult(ctx, recordQuery)
			} else if g.cascadeQueryRecords != nil {
				result.Items, queryErr = g.cascadeQueryRecords(ctx, recordQuery)
			} else {
				result, queryErr = g.queryRecordResult(ctx, recordQuery)
			}
			if queryErr != nil {
				slog.Warn("query cascade frontend RecordInfo failed", "upstream", worker.platform.name, "channel", localChannelID, "err", queryErr)
				return
			}
			result.Items = append([]RecordItem(nil), result.Items...)
			for itemIndex := range result.Items {
				result.Items[itemIndex].DeviceID = exposedID
				result.Items[itemIndex].RecorderID = cascadeRecordRecorderID(worker.platform, result.Items[itemIndex].RecorderID, localChannelID, channel.DeviceID, exposedID)
				result.Items[itemIndex].RecordLocation = cascadeRecordDeviceID(worker.platform, result.Items[itemIndex].RecordLocation, localChannelID, channel.DeviceID, exposedID)
				if requireRecordLocation && !isGBDeviceIdentifier(result.Items[itemIndex].RecordLocation) {
					results[index].err = fmt.Errorf("RecordInfo storage location is required for indistinct query")
					return
				}
			}
			rewritten, rewriteErr := rewriteCascadeRecordExtraInfo(result.ExtraInfo, worker.protocolVersion(), worker.platform, localChannelID, channel.DeviceID, exposedID)
			if rewriteErr != nil {
				slog.Warn("rewrite cascade RecordInfo ExtraInfo failed", "upstream", worker.platform.name, "channel", localChannelID, "err", rewriteErr)
				results[index].err = rewriteErr
				return
			}
			results[index] = channelResult{items: result.Items, extraInfo: rewritten}
		}(index, channel)
	}
	wg.Wait()
	items := make([]RecordItem, 0)
	extraInfo := make([]string, 0)
	for _, result := range results {
		if result.err != nil {
			return nil, nil, result.err
		}
		items = append(items, result.items...)
		extraInfo = append(extraInfo, result.extraInfo...)
	}
	return items, extraInfo, nil
}

func (g *GB28181API) validateCascadeRecordQueryTarget(input *RecordQueryInput) error {
	if input == nil || strings.TrimSpace(input.DeviceID) == "" || strings.TrimSpace(input.ChannelID) == "" {
		return fmt.Errorf("invalid record query input")
	}
	if g == nil || g.svr == nil || g.svr.memoryStorer == nil {
		return nil
	}
	device, ok := g.svr.memoryStorer.Load(input.DeviceID)
	if !ok || device == nil || !device.IsOnlineNow() {
		return ErrDeviceOffline
	}
	if input.ChannelID != input.DeviceID {
		if _, exists := g.svr.memoryStorer.GetChannel(input.DeviceID, input.ChannelID); !exists {
			return ErrChannelNotExist
		}
	}
	version := g.getDeviceGBProtocolVersion(input.DeviceID)
	if version.AtLeast(GBVersion20) && (input.OmitStartTime || input.OmitEndTime) {
		return fmt.Errorf("record query start/end are required by GB/T 28181-2016 and later")
	}
	if (!input.OmitStartTime && input.Start <= 0) || (!input.OmitEndTime && input.End <= 0) ||
		(!input.OmitStartTime && !input.OmitEndTime && input.End <= input.Start) {
		return fmt.Errorf("invalid record query time range")
	}
	return validateRecordQueryFilters(version, input)
}

type cascadeRecordPager struct {
	offset int
	limit  int
}

func (p cascadeRecordPager) Offset() int { return p.offset }
func (p cascadeRecordPager) Limit() int  { return p.limit }

func (g *GB28181API) queryCascadeCenterRecordItems(
	ctx context.Context,
	platform cascadePlatform,
	channels []*ipc.Channel,
	query cascadeQueryEnvelope,
	startAt, endAt time.Time,
) ([]RecordItem, error) {
	if g == nil || g.recordingStore == nil || !cascadeCenterRecordFiltersMatch(platform, query) || len(channels) == 0 {
		return nil, nil
	}
	channelByCID := make(map[string]*ipc.Channel, len(channels))
	cids := make([]string, 0, len(channels))
	for _, channel := range channels {
		if channel == nil || strings.TrimSpace(channel.ID) == "" || platform.channelIDMap[channel.ChannelID] == "" {
			continue
		}
		channelByCID[channel.ID] = channel
		cids = append(cids, channel.ID)
	}
	if len(cids) == 0 {
		return nil, nil
	}
	queryBuilder := orm.NewQuery(8).OrderBy("started_at ASC, ended_at ASC, path ASC, id ASC")
	queryBuilder.Where("cid IN ?", cids)
	queryBuilder.Where("delete_flag = ?", false)
	if !endAt.IsZero() {
		queryBuilder.Where("started_at < ?", orm.Time{Time: endAt})
	}
	if !startAt.IsZero() {
		queryBuilder.Where("ended_at > ?", orm.Time{Time: startAt})
	}
	if query.FilePath != "" {
		queryBuilder.Where("path = ?", query.FilePath)
	}

	const pageSize = 1000
	rows := make([]*recording.Recording, 0)
	for offset, total := 0, int64(-1); total < 0 || int64(offset) < total; offset += pageSize {
		page := make([]*recording.Recording, 0, pageSize)
		count, err := g.recordingStore.List(ctx, &page, cascadeRecordPager{offset: offset, limit: pageSize}, queryBuilder.Encode()...)
		if err != nil {
			return nil, err
		}
		total = count
		rows = append(rows, page...)
		if len(page) < pageSize {
			break
		}
	}
	items := make([]RecordItem, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		channel := channelByCID[row.CID]
		if channel == nil {
			continue
		}
		exposedID := platform.channelIDMap[channel.ChannelID]
		items = append(items, RecordItem{
			DeviceID: exposedID, Name: firstPresentString(channel.Name, exposedID), HasName: true,
			FilePath: row.Path, StartTime: sip.FormatGBTime(row.StartedAt.Time, "2006-01-02T15:04:05"), EndTime: sip.FormatGBTime(row.EndedAt.Time, "2006-01-02T15:04:05"),
			Secrecy: 0, HasSecrecy: true, Type: "time", RecorderID: platform.localID,
			FileSize: strconv.FormatInt(row.Size, 10), HasFileSize: true, RecordLocation: platform.localID, HasRecordLocation: true,
		})
	}
	return items, nil
}

func cascadeCenterRecordFiltersMatch(platform cascadePlatform, query cascadeQueryEnvelope) bool {
	recordType, err := normalizeRecordQueryType(query.Type)
	if err != nil || recordType != "time" && recordType != "all" {
		return false
	}
	if query.Address != "" || strings.TrimSpace(query.AlarmMethod) != "" || strings.TrimSpace(query.AlarmType) != "" {
		return false
	}
	if query.Secrecy != nil && *query.Secrecy != 0 {
		return false
	}
	if query.StreamNumber != nil && *query.StreamNumber != 0 {
		return false
	}
	recorderID := query.RecorderID
	return recorderID == "" || recorderID == platform.localID
}

func normalizeCascadeRecordItems(items []RecordItem) []RecordItem {
	seen := make(map[string]struct{}, len(items))
	out := make([]RecordItem, 0, len(items))
	for _, item := range items {
		key := strings.Join([]string{item.DeviceID, item.FilePath, item.StartTime, item.EndTime}, "\x00")
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	sort.SliceStable(out, func(i, j int) bool {
		left := []string{out[i].StartTime, out[i].EndTime, out[i].DeviceID, out[i].FilePath}
		right := []string{out[j].StartTime, out[j].EndTime, out[j].DeviceID, out[j].FilePath}
		for index := range left {
			if left[index] != right[index] {
				return left[index] < right[index]
			}
		}
		return false
	})
	return out
}

func dedupeCascadeRecordExtraInfo(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func cascadeRecordDeviceID(platform cascadePlatform, value, localChannelID, localDeviceID, exposedID string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if mapped := platform.channelIDMap[value]; mapped != "" {
		return mapped
	}
	if value == localChannelID || value == localDeviceID {
		return exposedID
	}
	return ""
}

func cascadeRecordRecorderID(platform cascadePlatform, value, localChannelID, localDeviceID, exposedID string) string {
	if value == "" {
		return ""
	}
	if mapped := platform.channelIDMap[value]; mapped != "" {
		return mapped
	}
	if value == localChannelID || value == localDeviceID {
		return exposedID
	}
	// RecorderID 在四版 Schema 中均为普通 string。未知的标准设备编码仍不透传，
	// 避免泄露未共享标识；其他合法字符串按原值保留。
	if isGBDeviceIdentifier(value) {
		return ""
	}
	return value
}

func rewriteCascadeRecordExtraInfo(values []string, version GBProtocolVersion, platform cascadePlatform, localChannelID, localDeviceID, exposedID string) ([]string, error) {
	if !version.AtLeast(GBVersion30) || len(values) == 0 {
		return nil, nil
	}
	mappingPlatform := withCascadeIdentifierMapping(platform, localDeviceID, platform.localID)
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		rewritten, err := rewriteCascadeOpaqueIdentifiers(value, "ExtraInfo", mappingPlatform, localChannelID, exposedID)
		if err != nil {
			return nil, err
		}
		// 2022 ExtraInfo 是普通 string，仅约束 maxLength=1024；保留空字符串和首尾空白。
		if utf8.RuneCountInString(rewritten) > 1024 {
			return nil, fmt.Errorf("invalid RecordInfo ExtraInfo")
		}
		if _, ok := seen[rewritten]; ok {
			continue
		}
		seen[rewritten] = struct{}{}
		out = append(out, rewritten)
	}
	return out, nil
}

func (g *GB28181API) sendCascadeRecordItems(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope, items []RecordItem, name string, extraInfo []string) error {
	if name == "" {
		name = query.DeviceID
	}
	version := worker.protocolVersion()
	if !version.AtLeast(GBVersion30) {
		extraInfo = nil
	}
	if len(items) == 0 {
		response := cascadeRecordInfoResponse{
			CmdType: "RecordInfo", SN: query.SN, DeviceID: query.DeviceID, Name: name,
			ExtraInfo: append([]string(nil), extraInfo...),
		}
		if version == GBVersion10 {
			response.RecordList = &cascadeRecordInfoList{}
		}
		return sendCascadeXML(ctx, worker, response)
	}
	items = recordItemsForVersion(items, version)
	for start := 0; start < len(items); start += cascadeCatalogChunkSize {
		end := min(start+cascadeCatalogChunkSize, len(items))
		var responseExtraInfo []string
		if start == 0 {
			responseExtraInfo = append([]string(nil), extraInfo...)
		}
		if err := sendCascadeXML(ctx, worker, cascadeRecordInfoResponse{
			CmdType: "RecordInfo", SN: query.SN, DeviceID: query.DeviceID, Name: name, SumNum: len(items),
			RecordList: &cascadeRecordInfoList{Num: end - start, Items: items[start:end]}, ExtraInfo: responseExtraInfo,
		}); err != nil {
			return err
		}
	}
	return nil
}

func recordItemsForVersion(items []RecordItem, version GBProtocolVersion) []RecordItem {
	out := make([]RecordItem, len(items))
	copy(out, items)
	for index := range out {
		if !version.AtLeast(GBVersion20) {
			out[index].FileSize = ""
			out[index].HasFileSize = false
		}
		if !version.AtLeast(GBVersion30) {
			out[index].RecordLocation = ""
			out[index].HasRecordLocation = false
			out[index].StreamNumber = nil
		}
		if version.AtLeast(GBVersion11) && strings.EqualFold(strings.TrimSpace(out[index].Type), "all") {
			out[index].Type = ""
			out[index].hasType = false
		}
	}
	return out
}

func (g *GB28181API) loadCascadeExposedChannel(ctx context.Context, platform cascadePlatform, exposedID string) (*ipc.Channel, error) {
	localID := platform.exposedChannelMap[strings.TrimSpace(exposedID)]
	if localID == "" {
		return nil, fmt.Errorf("cascade channel not shared")
	}
	channels, err := g.loadCascadeChannels(ctx, platform)
	if err != nil {
		return nil, err
	}
	for _, channel := range channels {
		if channel != nil && channel.ChannelID == localID {
			return channel, nil
		}
	}
	return nil, fmt.Errorf("shared cascade channel not found")
}

func sendCascadeXML(ctx context.Context, worker *cascadeWorker, value any) error {
	body, err := sip.XMLEncode(value)
	if err != nil {
		return err
	}
	return worker.sendMessage(ctx, body)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// firstPresentString 为 XML Schema 普通 string 选择回退值。普通 string 的
// whiteSpace 规则是 preserve，因此只把真正的空串视为缺失，不能吞掉空白内容。
func firstPresentString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
