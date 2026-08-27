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
	"time"
	"unicode/utf8"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const cascadeCatalogChunkSize = 20

type cascadeQueryEnvelope struct {
	XMLName      xml.Name
	CmdType      string `xml:"CmdType"`
	SN           int    `xml:"SN"`
	DeviceID     string `xml:"DeviceID"`
	SourceID     string `xml:"SourceID"`
	TargetID     string `xml:"TargetID"`
	StartTime    string `xml:"StartTime"`
	EndTime      string `xml:"EndTime"`
	Type         string `xml:"Type"`
	StreamNumber *int   `xml:"StreamNumber"`
	AlarmMethod  string `xml:"AlarmMethod"`
	AlarmType    string `xml:"AlarmType"`
	Interval     int    `xml:"Interval"`
	Number       *int   `xml:"Number"`
	ConfigType   string `xml:"ConfigType"`
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
	DeviceID          string              `xml:"DeviceID"`
	Name              string              `xml:"Name"`
	Manufacturer      string              `xml:"Manufacturer"`
	Model             string              `xml:"Model"`
	Owner             *string             `xml:"Owner,omitempty"`
	CivilCode         string              `xml:"CivilCode"`
	Block             string              `xml:"Block"`
	Address           string              `xml:"Address"`
	Parental          int                 `xml:"Parental"`
	ParentID          string              `xml:"ParentID"`
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
	ContactInfo              string  `xml:"ContactInfo,omitempty"`
	RecordSaveDays           int     `xml:"RecordSaveDays,omitempty"`
	IndustrialClassification string  `xml:"IndustrialClassification,omitempty"`
	BusinessGroupID          string  `xml:"BusinessGroupID,omitempty"`
}

type cascadeDeviceInfoResponse struct {
	XMLName      xml.Name `xml:"Response"`
	CmdType      string   `xml:"CmdType"`
	SN           int      `xml:"SN"`
	DeviceID     string   `xml:"DeviceID"`
	Result       string   `xml:"Result"`
	DeviceName   string   `xml:"DeviceName,omitempty"`
	DeviceType   string   `xml:"DeviceType"`
	Manufacturer string   `xml:"Manufacturer"`
	Model        string   `xml:"Model"`
	Firmware     string   `xml:"Firmware"`
	Channel      int      `xml:"Channel"`
	MaxCamera    int      `xml:"MaxCamera"`
	MaxAlarm     int      `xml:"MaxAlarm"`
}

func cascadeDeviceInfoName(version GBProtocolVersion, name string) string {
	if !version.AtLeast(GBVersion11) {
		return ""
	}
	return name
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

type cascadeRecordInfoResponse struct {
	XMLName    xml.Name               `xml:"Response"`
	CmdType    string                 `xml:"CmdType"`
	SN         int                    `xml:"SN"`
	DeviceID   string                 `xml:"DeviceID"`
	Name       string                 `xml:"Name"`
	SumNum     int                    `xml:"SumNum"`
	RecordList *cascadeRecordInfoList `xml:"RecordList,omitempty"`
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
	if query.XMLName.Local == "Notify" && query.CmdType == "Broadcast" {
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
		ctx.String(200, "OK")
		ctx.Abort()
		body := query
		identityCtx := monitorUserIdentityContext(ctx)
		g.startLifecycleTask(identityCtx, func(taskCtx context.Context) {
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
			var request DeviceConfigRequest
			if err := sip.XMLDecode(ctx.Request.Body(), &request); err != nil {
				ctx.AbortString(400, ErrXMLDecode.Error())
				return
			}
			request.CmdType = strings.TrimSpace(request.CmdType)
			request.DeviceID = strings.TrimSpace(request.DeviceID)
			if err := validateCascadeDeviceConfigPayload(&request); err != nil {
				ctx.AbortString(400, err.Error())
				return
			}
			if worker.platform.exposedChannelMap[request.DeviceID] == "" {
				ctx.AbortString(404, "cascade config target not found")
				return
			}
			ctx.String(200, "OK")
			ctx.Abort()
			body := append([]byte(nil), ctx.Request.Body()...)
			identityCtx := monitorUserIdentityContext(ctx)
			g.startLifecycleTask(identityCtx, func(taskCtx context.Context) {
				g.forwardCascadeDeviceConfig(worker, body, taskCtx)
			})
			return
		}
		if query.CmdType != ptzCmdTypeDeviceControl || worker.platform.exposedChannelMap[query.DeviceID] == "" {
			ctx.AbortString(404, "cascade control target not found")
			return
		}
		ctx.String(200, "OK")
		ctx.Abort()
		body := append([]byte(nil), ctx.Request.Body()...)
		identityCtx := monitorUserIdentityContext(ctx)
		g.startLifecycleTask(identityCtx, func(taskCtx context.Context) {
			g.forwardCascadeDeviceControl(worker, body, taskCtx)
		})
		return
	}
	if query.XMLName.Local != "Query" {
		ctx.AbortString(400, "invalid cascade query")
		return
	}
	if err := validateCascadeQueryRequest(query); err != nil {
		ctx.AbortString(400, err.Error())
		return
	}
	if !cascadeQueryTargetAllowed(worker.platform, query.CmdType, query.DeviceID) {
		ctx.AbortString(404, "cascade target not found")
		return
	}

	ctx.String(200, "OK")
	ctx.Abort()
	identityCtx := monitorUserIdentityContext(ctx)
	g.startLifecycleTask(identityCtx, func(taskCtx context.Context) {
		g.respondCascadeQuery(worker, query, taskCtx)
	})
}

func validateCascadeQueryRequest(query cascadeQueryEnvelope) error {
	if query.XMLName.Local != "Query" || query.SN <= 0 || !isGBDeviceIdentifier(strings.TrimSpace(query.DeviceID)) {
		return fmt.Errorf("invalid cascade query envelope")
	}
	switch query.CmdType {
	case "Catalog", "DeviceInfo", "DeviceStatus", "PresetQuery", "HomePositionQuery", "CruiseTrackListQuery", "PTZPosition", "SDCardStatus":
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
		startAt, startErr := sip.ParseGBTime("2006-01-02T15:04:05", strings.TrimSpace(query.StartTime))
		endAt, endErr := sip.ParseGBTime("2006-01-02T15:04:05", strings.TrimSpace(query.EndTime))
		if startErr != nil || endErr != nil || !endAt.After(startAt) {
			return fmt.Errorf("RecordInfo requires valid StartTime and EndTime")
		}
		return nil
	default:
		return fmt.Errorf("unsupported cascade query command: %s", query.CmdType)
	}
}

func (g *GB28181API) respondCascadeQuery(worker *cascadeWorker, query cascadeQueryEnvelope, parents ...context.Context) {
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
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
	case "PresetQuery", "HomePositionQuery", "CruiseTrackListQuery", "CruiseTrackQuery", "MobilePosition", "PTZPosition", "SDCardStatus", "ConfigDownload":
		err = g.respondCascadeExtendedQuery(ctx, worker, query)
	default:
		err = sendCascadeQueryError(ctx, worker, query)
	}
	if err != nil {
		slog.Error("respond cascade query failed", "upstream", worker.platform.name, "cmd_type", query.CmdType, "sn", query.SN, "err", err)
	}
}

func cascadeQueryTargetAllowed(platform cascadePlatform, cmdType, deviceID string) bool {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == platform.localID {
		return true
	}
	if platform.exposedChannelMap[deviceID] == "" {
		return false
	}
	switch strings.TrimSpace(cmdType) {
	case "Catalog", "DeviceInfo", "DeviceStatus", "RecordInfo", "PresetQuery", "HomePositionQuery",
		"CruiseTrackListQuery", "CruiseTrackQuery", "MobilePosition", "PTZPosition", "SDCardStatus", "ConfigDownload":
		return true
	default:
		return false
	}
}

func cascadeExtendedQueryAction(cmdType string, version GBProtocolVersion) (string, bool) {
	switch canonicalGBQueryCmdType(cmdType) {
	case "PresetQuery":
		return deviceQueryActionPresetQuery, version.AtLeast(GBVersion11)
	case "HomePositionQuery":
		return deviceQueryActionHomePositionQuery, version.AtLeast(GBVersion20)
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
		return sendCascadeQueryError(ctx, worker, query)
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
	queryDevice := g.DeviceQuery
	if g.cascadeDeviceQuery != nil {
		queryDevice = g.cascadeDeviceQuery
	}
	out, err := queryDevice(ctx, &DeviceQueryInput{
		DeviceID: channel.DeviceID, TargetID: channel.ChannelID, Action: action,
		Timeout: 25 * time.Second, ConfigType: query.ConfigType, Interval: query.Interval, Number: cascadeQueryNumber(query),
	})
	if err != nil || out == nil || strings.TrimSpace(out.XML) == "" ||
		!strings.EqualFold(canonicalGBQueryCmdType(out.CmdType), query.CmdType) {
		if err != nil {
			slog.Warn("forward cascade query failed", "upstream", worker.platform.name, "cmd_type", query.CmdType, "channel", query.DeviceID, "err", err)
		}
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
	return sendCascadeXML(ctx, worker, cascadeQueryErrorResponse{
		CmdType: gbQueryCmdTypeForVersion(query.CmdType, worker.protocolVersion()), SN: query.SN, DeviceID: deviceID, Result: "ERROR",
	})
}

func rewriteCascadeQueryResponse(body []byte, query cascadeQueryEnvelope, platform cascadePlatform, version GBProtocolVersion, channel *ipc.Channel) ([]byte, error) {
	if len(body) == 0 || channel == nil {
		return nil, fmt.Errorf("empty cascade query response")
	}
	decoder := xml.NewDecoder(bytes.NewReader(body))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
		if utf8.Valid(body) {
			return input, nil
		}
		return simplifiedchinese.GB18030.NewDecoder().Reader(input), nil
	}

	var output bytes.Buffer
	output.WriteString("<?xml version=\"1.0\" encoding=\"GB2312\"?>\n")
	encoder := xml.NewEncoder(&output)
	depth := 0
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
			identifierField := strings.HasSuffix(strings.ToLower(strings.TrimSpace(name)), "id")
			if identifierField || strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") || (depth == 2 && (name == "CmdType" || name == "SN")) {
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
					if strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") {
						rewritten, err = rewriteCascadeOpaqueIdentifiers(original, name, mappingPlatform, channel.ChannelID, query.DeviceID)
					} else if depth == 2 && strings.EqualFold(name, "DeviceID") {
						rewritten = query.DeviceID
					} else {
						rewritten, err = rewriteCascadeIdentifierValue(original, name, mappingPlatform, channel.ChannelID, query.DeviceID)
					}
					if err != nil {
						return nil, err
					}
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
	return encoded, nil
}

func (g *GB28181API) respondCascadeCatalog(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	channels, err := g.loadCascadeChannels(ctx, worker.platform)
	if err != nil {
		return err
	}
	items := buildCascadeCatalogItems(channels, worker.platform, worker.protocolVersion())
	responseDeviceID := strings.TrimSpace(query.DeviceID)
	if responseDeviceID == "" || responseDeviceID == "*" {
		responseDeviceID = worker.platform.localID
	}
	items = filterCascadeCatalogNotifyItems(items, responseDeviceID, worker.platform.localID)
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
			g.eventSubscribers.Delete(key)
			return true
		}
		sub.mu.Lock()
		expiresAt := sub.ExpiresAt
		cascade := sub.Cascade
		cmdType := sub.CmdType
		sub.mu.Unlock()
		if now.After(expiresAt) {
			// 统一由订阅清理器删除并释放下级引用，避免和续订并发互相覆盖。
			return true
		}
		if cascade == nil || !strings.EqualFold(cmdType, "Catalog") {
			return true
		}
		subscription := sub
		upstream := cascade.platform.name
		g.startLifecycleTask(ctx, func(taskCtx context.Context) {
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
	expiresAt := sub.ExpiresAt
	subscriptionDeviceID := sub.DeviceID
	sub.mu.Unlock()
	if worker == nil || !time.Now().Before(expiresAt) {
		return fmt.Errorf("cascade subscription is unavailable")
	}
	channels, err := g.loadCascadeChannels(ctx, worker.platform)
	if err != nil {
		return err
	}
	visibleItems := buildCascadeCatalogItems(channels, worker.platform, worker.protocolVersion())
	visibleItems = filterCascadeCatalogNotifyItems(visibleItems, subscriptionDeviceID, worker.platform.localID)
	nextSnapshot := catalogSnapshot(visibleItems)
	var items []cascadeCatalogItem
	if initial {
		items = prepareCascadeCatalogNotifyItems(visibleItems, true)
	} else {
		sub.mu.Lock()
		previous := sub.CatalogSnapshot
		sub.mu.Unlock()
		items = diffCascadeCatalogNotifyItems(previous, visibleItems)
		if len(items) == 0 {
			return nil
		}
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
		if err := g.sendEventNotifyContext(ctx, sub, "Catalog", body); err != nil {
			return err
		}
		sub.mu.Lock()
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
		if err := g.sendEventNotifyContext(ctx, sub, "Catalog", body); err != nil {
			return err
		}
	}
	sub.mu.Lock()
	sub.CatalogSnapshot = nextSnapshot
	sub.mu.Unlock()
	return nil
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
	alreadySeeded := sub.CatalogSnapshot != nil
	sub.mu.Unlock()
	if worker == nil || alreadySeeded {
		return nil
	}
	channels, err := g.loadCascadeChannels(ctx, worker.platform)
	if err != nil {
		return err
	}
	items := buildCascadeCatalogItems(channels, worker.platform, worker.protocolVersion())
	items = filterCascadeCatalogNotifyItems(items, deviceID, worker.platform.localID)
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
	if version == GBVersion10 {
		root = "Response"
	}
	notify.XMLName = xml.Name{Local: root}
	return sip.XMLEncode(notify)
}

func filterCascadeCatalogNotifyItems(items []cascadeCatalogItem, subscriptionDeviceID, localID string) []cascadeCatalogItem {
	targetID := strings.TrimSpace(subscriptionDeviceID)
	if targetID == "" || targetID == "*" || targetID == strings.TrimSpace(localID) {
		return items
	}
	filtered := make([]cascadeCatalogItem, 0, 1)
	for _, item := range items {
		if strings.TrimSpace(item.DeviceID) == targetID {
			filtered = append(filtered, item)
		}
	}
	return filtered
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

func buildCascadeCatalogItems(channels []*ipc.Channel, platform cascadePlatform, version GBProtocolVersion) []cascadeCatalogItem {
	items := make([]cascadeCatalogItem, 0, len(channels))
	for _, channel := range channels {
		if channel == nil {
			continue
		}
		exposedID := platform.channelIDMap[channel.ChannelID]
		if exposedID == "" {
			continue
		}
		ext := channel.Ext.GBCatalog
		item := cascadeCatalogItem{
			DeviceID: exposedID, Name: strings.TrimSpace(channel.Name),
			Manufacturer: strings.TrimSpace(channel.Ext.Manufacturer), Model: strings.TrimSpace(channel.Ext.Model),
			CivilCode: platform.localDomain, ParentID: platform.localID,
			RegisterWay: 1, Status: "OFF",
		}
		if item.Name == "" {
			item.Name = exposedID
		}
		if channel.IsOnline {
			item.Status = "ON"
		}
		if ext != nil {
			item.CivilCode = firstNonEmpty(ext.CivilCode, item.CivilCode)
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
					item.BusinessGroupID = ext.BusinessGroupID
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
					item.Info.ContactInfo = ext.ContactInfo
					item.Info.RecordSaveDays = ext.RecordSaveDays
					item.Info.IndustrialClassification = ext.IndustrialClassification
				}
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

func (g *GB28181API) respondCascadeDeviceInfo(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	if query.DeviceID != worker.platform.localID {
		channel, err := g.loadCascadeExposedChannel(ctx, worker.platform, query.DeviceID)
		if err != nil {
			return err
		}
		response := cascadeDeviceInfoResponse{
			CmdType: "DeviceInfo", SN: query.SN, DeviceID: query.DeviceID, Result: "OK",
			DeviceType: "IPC", Manufacturer: channel.Ext.Manufacturer, Model: channel.Ext.Model, Firmware: channel.Ext.Firmware,
			Channel: 1, MaxCamera: 1,
		}
		response.DeviceName = cascadeDeviceInfoName(worker.protocolVersion(), firstNonEmpty(channel.Name, channel.Ext.Name, query.DeviceID))
		return sendCascadeXML(ctx, worker, response)
	}
	channels, err := g.loadCascadeChannels(ctx, worker.platform)
	if err != nil {
		return err
	}
	count := len(channels)
	firmware := ""
	if g.boot != nil {
		firmware = strings.TrimSpace(g.boot.BuildVersion)
	}
	response := cascadeDeviceInfoResponse{
		CmdType: "DeviceInfo", SN: query.SN, DeviceID: worker.platform.localID, Result: "OK",
		DeviceType: "NVR", Manufacturer: "GoWVP", Model: "OWL", Firmware: firmware, Channel: count, MaxCamera: count,
	}
	response.DeviceName = cascadeDeviceInfoName(worker.protocolVersion(), "GoWVP OWL")
	return sendCascadeXML(ctx, worker, response)
}

func (g *GB28181API) respondCascadeDeviceStatus(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope) error {
	if query.DeviceID != worker.platform.localID {
		if worker.protocolVersion().AtLeast(GBVersion30) {
			return g.respondCascadeForwardedQuery(ctx, worker, query, deviceQueryActionDeviceStatus)
		}
		channel, err := g.loadCascadeExposedChannel(ctx, worker.platform, query.DeviceID)
		if err != nil {
			return err
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
	localChannelID := worker.platform.exposedChannelMap[query.DeviceID]
	if localChannelID == "" {
		return g.sendCascadeRecordItems(ctx, worker, query, nil, "")
	}
	channel, err := g.loadCascadeExposedChannel(ctx, worker.platform, query.DeviceID)
	if err != nil {
		return err
	}
	startAt, startErr := sip.ParseGBTime("2006-01-02T15:04:05", strings.TrimSpace(query.StartTime))
	endAt, endErr := sip.ParseGBTime("2006-01-02T15:04:05", strings.TrimSpace(query.EndTime))
	if startErr != nil || endErr != nil || !endAt.After(startAt) {
		return g.sendCascadeRecordItems(ctx, worker, query, nil, channel.Name)
	}
	recordQuery := &RecordQueryInput{
		DeviceID: channel.DeviceID, ChannelID: localChannelID,
		Start: startAt.Unix(), End: endAt.Unix(), Timeout: 25 * time.Second,
		Type: query.Type, StreamNumber: query.StreamNumber, AlarmMethod: query.AlarmMethod, AlarmType: query.AlarmType,
	}
	if err := validateRecordQueryFilters(worker.protocolVersion(), recordQuery); err != nil {
		return sendCascadeQueryError(ctx, worker, query)
	}
	recordQuery.Type, _ = normalizeRecordQueryType(recordQuery.Type)
	queryRecords := g.queryRecordItems
	if g.cascadeQueryRecords != nil {
		queryRecords = g.cascadeQueryRecords
	}
	items, err := queryRecords(ctx, recordQuery)
	if err != nil {
		slog.Warn("query cascade RecordInfo failed", "upstream", worker.platform.name, "channel", localChannelID, "err", err)
		items = nil
	}
	for index := range items {
		items[index].DeviceID = query.DeviceID
		items[index].RecorderID = cascadeRecordDeviceID(worker.platform, items[index].RecorderID, localChannelID, channel.DeviceID, query.DeviceID)
		items[index].RecordLocation = cascadeRecordDeviceID(worker.platform, items[index].RecordLocation, localChannelID, channel.DeviceID, query.DeviceID)
	}
	return g.sendCascadeRecordItems(ctx, worker, query, items, firstNonEmpty(channel.Name, query.DeviceID))
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

func (g *GB28181API) sendCascadeRecordItems(ctx context.Context, worker *cascadeWorker, query cascadeQueryEnvelope, items []RecordItem, name string) error {
	name = firstNonEmpty(strings.TrimSpace(name), strings.TrimSpace(query.DeviceID))
	if len(items) == 0 {
		return sendCascadeXML(ctx, worker, cascadeRecordInfoResponse{
			CmdType: "RecordInfo", SN: query.SN, DeviceID: query.DeviceID, Name: name,
		})
	}
	items = recordItemsForVersion(items, worker.protocolVersion())
	for start := 0; start < len(items); start += cascadeCatalogChunkSize {
		end := min(start+cascadeCatalogChunkSize, len(items))
		if err := sendCascadeXML(ctx, worker, cascadeRecordInfoResponse{
			CmdType: "RecordInfo", SN: query.SN, DeviceID: query.DeviceID, Name: name, SumNum: len(items),
			RecordList: &cascadeRecordInfoList{Num: end - start, Items: items[start:end]},
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
