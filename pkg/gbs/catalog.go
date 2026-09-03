package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

type GBCatalogItemKind = string

type catalogRefreshState struct {
	dirty   bool
	running bool
	nextAt  time.Time
	wake    chan struct{}
}

const (
	legacyDeviceSIPWriteTimeout  = 5 * time.Second
	catalogRefreshFailureBackoff = time.Second
)

const (
	GBCatalogItemUnknown             GBCatalogItemKind = "unknown"
	GBCatalogItemAdministrative      GBCatalogItemKind = "administrative"
	GBCatalogItemSystem              GBCatalogItemKind = "system"
	GBCatalogItemBusinessGroup       GBCatalogItemKind = "business_group"
	GBCatalogItemVirtualOrganization GBCatalogItemKind = "virtual_organization"
	GBCatalogItemDevice              GBCatalogItemKind = "device"
)

// MessageDeviceListResponse 设备明细列表返回结构
type MessageDeviceListResponse struct {
	XMLName   xml.Name
	CmdType   string     `xml:"CmdType"`
	SN        int        `xml:"SN"`
	DeviceID  string     `xml:"DeviceID"`
	SumNum    int        `xml:"SumNum"`
	HasSumNum bool       `xml:"-"`
	Item      []Channels `xml:"-"`
	ListNum   *int       `xml:"-"`
	HasList   bool       `xml:"-"`
}

func (m *MessageDeviceListResponse) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var value struct {
		CmdType  string `xml:"CmdType"`
		SN       int    `xml:"SN"`
		DeviceID string `xml:"DeviceID"`
		SumNum   *int   `xml:"SumNum"`
		List     *struct {
			Num  *int       `xml:"Num,attr"`
			Item []Channels `xml:"Item"`
		} `xml:"DeviceList"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*m = MessageDeviceListResponse{
		XMLName: start.Name, CmdType: value.CmdType, SN: value.SN, DeviceID: value.DeviceID,
	}
	if value.SumNum != nil {
		m.SumNum, m.HasSumNum = *value.SumNum, true
	}
	if value.List != nil {
		m.HasList, m.ListNum, m.Item = true, value.List.Num, value.List.Item
	}
	return nil
}

func classifyGBCatalogItem(deviceID string) GBCatalogItemKind {
	deviceID = strings.TrimSpace(deviceID)
	if !allDecimalDigits(deviceID) {
		return GBCatalogItemUnknown
	}
	switch len(deviceID) {
	case 2, 4, 6, 8:
		return GBCatalogItemAdministrative
	case 20:
		typeCode := deviceID[10:13]
		switch typeCode {
		case "200":
			return GBCatalogItemSystem
		case "215":
			return GBCatalogItemBusinessGroup
		case "216":
			return GBCatalogItemVirtualOrganization
		default:
			return GBCatalogItemDevice
		}
	default:
		return GBCatalogItemUnknown
	}
}

func allDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func catalogChannelExt(item Channels) ipc.DeviceExt {
	return ipc.DeviceExt{
		Manufacturer: item.Manufacturer,
		Model:        item.Model,
		GBCatalog: &ipc.GBCatalogExt{
			Kind:                     classifyGBCatalogItem(item.ChannelID),
			Owner:                    item.Owner,
			CivilCode:                item.CivilCode,
			Block:                    item.Block,
			Address:                  item.Address,
			Parental:                 item.Parental,
			ParentID:                 item.ParentID,
			SafetyWay:                item.SafetyWay,
			RegisterWay:              item.RegisterWay,
			CertNum:                  item.CertNum,
			Certifiable:              item.Certifiable,
			ErrCode:                  item.ErrCode,
			EndTime:                  item.EndTime,
			SecurityLevelCode:        item.SecurityLevelCode,
			Secrecy:                  item.Secrecy,
			IPAddress:                item.IPAddress,
			Port:                     item.Port,
			Password:                 item.Password,
			Status:                   item.Status,
			Longitude:                item.Longitude,
			Latitude:                 item.Latitude,
			PTZType:                  item.Info.PTZType,
			PTZTypeList:              item.Info.PTZTypeList,
			PhotoelectricImagingType: item.Info.PhotoelectricImagingType,
			CapturePositionType:      item.Info.CapturePositionType,
			PositionType:             item.Info.PositionType,
			RoomType:                 item.Info.RoomType,
			UseType:                  item.Info.UseType,
			SupplyLightType:          item.Info.SupplyLightType,
			DirectionType:            item.Info.DirectionType,
			Resolution:               item.Info.Resolution,
			StreamNumberList:         item.Info.StreamNumberList,
			DownloadSpeed:            item.Info.DownloadSpeed,
			SVCSpaceSupportMode:      item.Info.SVCSpaceSupportMode,
			SVCTimeSupportMode:       item.Info.SVCTimeSupportMode,
			SSVCRatioSupportList:     item.Info.SSVCRatioSupportList,
			MobileDeviceType:         item.Info.MobileDeviceType,
			HorizontalFieldAngle:     item.Info.HorizontalFieldAngle,
			VerticalFieldAngle:       item.Info.VerticalFieldAngle,
			MaxViewDistance:          item.Info.MaxViewDistance,
			GrassrootsCode:           item.Info.GrassrootsCode,
			PointType:                item.Info.PointType,
			PointCommonName:          item.Info.PointCommonName,
			MAC:                      item.Info.MAC,
			FunctionType:             item.Info.FunctionType,
			EncodeType:               item.Info.EncodeType,
			InstallTime:              item.Info.InstallTime,
			ManagementUnit:           item.Info.ManagementUnit,
			ContactInfo:              item.Info.ContactInfo,
			RecordSaveDays:           item.Info.RecordSaveDays,
			IndustrialClassification: item.Info.IndustrialClassification,
			BusinessGroupID:          firstNonEmpty(strings.TrimSpace(item.BusinessGroupID), strings.TrimSpace(item.Info.BusinessGroupID)),
			RawXML:                   "<Item>" + item.RawXML + "</Item>",
			InfoRawXML:               item.Info.RawXML,
		},
	}
}

// sipMessageCatalog 设备目录信息查询应答
// GB/T28181 90 页 A.2.6.4
func (g *GB28181API) sipMessageCatalog(ctx *sip.Context) {
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	if err := validateCatalogStructure(ctx.Request.Body(), version, false); err != nil {
		ctx.String(400, err.Error())
		return
	}
	var msg MessageDeviceListResponse
	if err := sip.XMLDecode(ctx.Request.Body(), &msg); err != nil {
		slog.Error("Message Unmarshal xml", "err", err)
		ctx.String(400, "xml err")
		return
	}
	msg.CmdType = strings.TrimSpace(msg.CmdType)
	msg.DeviceID = strings.TrimSpace(msg.DeviceID)
	if err := g.validateCatalogEnvelope(ctx, msg, false); err != nil {
		ctx.String(400, err.Error())
		return
	}
	extended, err := g.validateAndDecodeAppendixA4(ctx.DeviceID, msg.CmdType, ctx.Request.Body())
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	catalogKey := buildMultiResponseKey(ctx.DeviceID, "Catalog", msg.SN)
	collectorActive := g.catalogResponses != nil && g.catalogResponses.Has(catalogKey)
	_, queryActive := g.pendingDeviceQueryExpectedTarget(ctx.DeviceID, msg.CmdType, msg.SN)
	if !collectorActive && !queryActive {
		ctx.Log.Warn("ignore unassociated Catalog response", "sn", msg.SN, "target_id", msg.DeviceID)
		ctx.String(200, "OK")
		return
	}

	for index := range msg.Item {
		msg.Item[index].DeviceID = msg.DeviceID
		msg.Item[index].ChannelID = strings.TrimSpace(msg.Item[index].ChannelID)
	}
	// 命中通用查询等待队列（A.2.4 Catalog 查询等待）。
	stateDeviceID := firstNonEmpty(msg.DeviceID, strings.TrimSpace(ctx.DeviceID))
	decoded := g.decodeDeviceQueryResult(stateDeviceID, msg.CmdType, ctx.Request.Body(), extended)
	decoded.data = cloneCatalogChannels(msg.Item)
	decoded.catalogExpected = cloneValue(&msg.SumNum)
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond Catalog", "err", err, "sn", msg.SN, "target_id", msg.DeviceID)
		return
	}
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlockCommit()
	if g.catalogResponses != nil {
		g.catalogResponses.Add(catalogKey, msg.SumNum, msg.Item)
	}
	g.commitDecodedQueryStateForOwnerLocked(ctx.DeviceID, stateDeviceID, msg.CmdType, decoded)
	g.resolvePendingDeviceQueryResult(ctx.DeviceID, msg.CmdType, msg.SN, "", ctx.Request.Body(), msg.DeviceID, decoded)

	// Catalog 可以查询通道、业务分组或行政区域；这些目标没有独立设备记录。
	// 运行态仍使用 stateDeviceID，持久化扩展对象必须落到已鉴权父设备。
	g.persistDecodedQuery(ctx.DeviceID, msg.CmdType, decoded)
}

func (g *GB28181API) validateCatalogEnvelope(ctx *sip.Context, msg MessageDeviceListResponse, notification bool) error {
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	expectedRoot := "Response"
	if notification && version.AtLeast(GBVersion11) {
		expectedRoot = "Notify"
	}
	if msg.XMLName.Local != expectedRoot {
		return fmt.Errorf("Catalog root must be %s", expectedRoot)
	}
	if !strings.EqualFold(strings.TrimSpace(msg.CmdType), "Catalog") {
		return fmt.Errorf("invalid Catalog command")
	}
	if msg.SN <= 0 || !validCatalogTargetID(version, msg.DeviceID) {
		return fmt.Errorf("Catalog requires positive SN and a valid DeviceID")
	}
	if !msg.HasSumNum || msg.SumNum < 0 {
		return fmt.Errorf("Catalog SumNum must not be negative")
	}
	if !msg.HasList {
		if msg.SumNum != 0 {
			return fmt.Errorf("Catalog DeviceList is required for non-empty results")
		}
		if notification || version == GBVersion10 {
			return fmt.Errorf("Catalog DeviceList is required by the protocol profile")
		}
	} else if msg.ListNum == nil || *msg.ListNum < 0 || *msg.ListNum != len(msg.Item) || len(msg.Item) > msg.SumNum || g.multiResponseChunkExceedsLimit(ctx, len(msg.Item)) {
		return fmt.Errorf("invalid Catalog DeviceList count")
	}
	for _, item := range msg.Item {
		if !validCatalogTargetID(version, item.ChannelID) {
			return fmt.Errorf("invalid Catalog item DeviceID")
		}
		if err := validateCatalogItemValues(item, version); err != nil {
			return err
		}
		if notification && version.AtLeast(GBVersion11) && !validCatalogNotifyEvent(item.Event) {
			return fmt.Errorf("invalid Catalog item Event")
		}
	}
	if err := g.validateCatalogResponseTarget(ctx, msg.DeviceID, msg.SN); err != nil {
		return err
	}
	if !notification && g.pendingDeviceQueryTargetMismatch(ctx.DeviceID, msg.CmdType, msg.SN, msg.DeviceID) {
		return fmt.Errorf("Catalog response target mismatch")
	}
	if !notification && g.catalogResponses != nil && g.catalogResponses.Has(buildMultiResponseKey(ctx.DeviceID, "Catalog", msg.SN)) && msg.DeviceID != strings.TrimSpace(ctx.DeviceID) {
		return fmt.Errorf("Catalog aggregate target mismatch")
	}
	return nil
}

func validCatalogNotifyEvent(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "ON", "OFF", "VLOST", "DEFECT", "ADD", "DEL", "UPDATE":
		return true
	default:
		return false
	}
}

func validateCatalogItemValues(item Channels, version GBProtocolVersion) error {
	status := strings.TrimSpace(item.Status)
	if status != "" && !equalFoldAny(status, "ON", "OFF") {
		return fmt.Errorf("Catalog item Status must be ON or OFF")
	}
	if item.Parental < 0 || item.Parental > 1 || item.Certifiable < 0 || item.Certifiable > 1 || item.Secrecy < 0 || item.Secrecy > 1 {
		return fmt.Errorf("Catalog item boolean values must be 0 or 1")
	}
	if item.SafetyWay != 0 && item.SafetyWay != 2 && item.SafetyWay != 3 && item.SafetyWay != 4 {
		return fmt.Errorf("invalid Catalog item SafetyWay")
	}
	registerWayMaximum := 3
	if version == GBVersion30 {
		registerWayMaximum = 4
	}
	if item.RegisterWay < 0 || item.RegisterWay > registerWayMaximum || item.hasRegisterWay && item.RegisterWay == 0 {
		return fmt.Errorf("invalid Catalog item RegisterWay")
	}
	if item.ErrCode < 0 || item.hasErrCode && item.ErrCode == 0 {
		return fmt.Errorf("invalid Catalog item ErrCode")
	}
	if item.Port < 0 || item.Port > 65535 {
		return fmt.Errorf("invalid Catalog item Port")
	}
	if math.IsNaN(item.Longitude) || math.IsInf(item.Longitude, 0) || math.IsNaN(item.Latitude) || math.IsInf(item.Latitude, 0) {
		return fmt.Errorf("Catalog item coordinates must be finite")
	}
	if endTime := strings.TrimSpace(item.EndTime); item.hasEndTime && endTime == "" || endTime != "" && !validGBDateTime(endTime) {
		return fmt.Errorf("Catalog item EndTime must be a non-empty dateTime when present")
	}
	securityLevelCode := strings.ToUpper(strings.TrimSpace(item.SecurityLevelCode))
	if version != GBVersion30 && (item.hasSecurityLevelCode || securityLevelCode != "") {
		return fmt.Errorf("Catalog item SecurityLevelCode requires protocol 3.0")
	}
	if version == GBVersion30 && securityLevelCode != "" && securityLevelCode != "A" && securityLevelCode != "B" && securityLevelCode != "C" {
		return fmt.Errorf("invalid Catalog item SecurityLevelCode")
	}
	info := item.Info
	outerBusinessGroupID := strings.TrimSpace(item.BusinessGroupID)
	innerBusinessGroupID := strings.TrimSpace(info.BusinessGroupID)
	if version == GBVersion10 && (item.hasBusinessGroupID || info.hasBusinessGroupID || outerBusinessGroupID != "" || innerBusinessGroupID != "") {
		return fmt.Errorf("Catalog item BusinessGroupID requires protocol 1.1")
	}
	if (version == GBVersion11 || version == GBVersion20) && (item.hasBusinessGroupID || outerBusinessGroupID != "") {
		return fmt.Errorf("Catalog item outer BusinessGroupID requires protocol 3.0")
	}
	if version == GBVersion30 && (info.hasBusinessGroupID || innerBusinessGroupID != "") {
		return fmt.Errorf("Catalog item Info BusinessGroupID is not supported by protocol 3.0")
	}
	if (version == GBVersion11 || version == GBVersion20) && info.hasBusinessGroupID && innerBusinessGroupID == "" {
		return fmt.Errorf("Catalog item Info BusinessGroupID must be a valid DeviceID when present")
	}
	if version == GBVersion30 && item.hasBusinessGroupID && outerBusinessGroupID == "" {
		return fmt.Errorf("Catalog item BusinessGroupID must be a valid DeviceID when present")
	}
	if !version.AtLeast(GBVersion11) && (info.XMLName.Local != "" || strings.TrimSpace(info.RawXML) != "" || info.PTZType != 0 || strings.TrimSpace(info.PTZTypeList) != "" || info.PositionType != 0 || info.RoomType != 0 || info.UseType != 0 || info.SupplyLightType != 0 || info.DirectionType != 0 || strings.TrimSpace(info.Resolution) != "" || strings.TrimSpace(info.BusinessGroupID) != "") {
		return fmt.Errorf("Catalog item Info requires protocol 1.1")
	}
	if err := validateCatalogPTZType(info, version); err != nil {
		return err
	}
	if version == GBVersion11 && hasCatalog20Info(info) {
		return fmt.Errorf("Catalog item Info extension requires protocol 2.0")
	}
	if version != GBVersion30 && hasCatalog30Info(info) {
		return fmt.Errorf("Catalog item Info extension requires protocol 3.0")
	}
	if version == GBVersion30 {
		if item.hasOwner || item.Owner != "" || item.hasSafetyWay || item.SafetyWay != 0 || item.hasCertNum || item.CertNum != "" || item.hasCertifiable || item.Certifiable != 0 || item.hasErrCode || item.ErrCode != 0 || item.hasEndTime || item.EndTime != "" {
			return fmt.Errorf("Catalog item contains fields removed by protocol 3.0")
		}
		if info.hasPositionType || info.PositionType != 0 || info.hasUseType || info.UseType != 0 {
			return fmt.Errorf("Catalog item Info contains fields removed by protocol 3.0")
		}
	}
	if info.PositionType < 0 || info.PositionType > 10 || info.hasPositionType && info.PositionType == 0 ||
		info.RoomType < 0 || info.RoomType > 2 || info.hasRoomType && info.RoomType == 0 ||
		info.UseType < 0 || info.UseType > 3 || info.hasUseType && info.UseType == 0 ||
		!validCatalogSupplyLightType(info.SupplyLightType, version) || info.hasSupplyLightType && info.SupplyLightType == 0 ||
		info.DirectionType < 0 || info.DirectionType > 8 || info.hasDirectionType && info.DirectionType == 0 {
		return fmt.Errorf("invalid Catalog item Info value")
	}
	if version.AtLeast(GBVersion20) {
		if err := validateCatalog20Info(info); err != nil {
			return err
		}
	}
	if version == GBVersion30 {
		if err := validateCatalog30RequiredFields(item); err != nil {
			return err
		}
		if err := validateCatalog30Info(info); err != nil {
			return err
		}
	}
	businessGroupID := innerBusinessGroupID
	if version == GBVersion30 {
		businessGroupID = outerBusinessGroupID
	}
	if businessGroupID != "" {
		if !version.AtLeast(GBVersion11) || !isGBDeviceIdentifier(businessGroupID) {
			return fmt.Errorf("invalid Catalog item BusinessGroupID")
		}
	}
	return nil
}

func validateCatalog30RequiredFields(item Channels) error {
	deviceID := strings.TrimSpace(item.ChannelID)
	if deviceID == "" {
		// 内部字段级校验调用可不带目录项标识；协议入口在调用本函数前已校验 DeviceID。
		return nil
	}
	if !item.hasName && item.Name == "" {
		return fmt.Errorf("protocol 3.0 Catalog item Name is required")
	}

	kind := classifyGBCatalogItem(deviceID)
	requireCivilCode := func() error {
		civilCode := strings.TrimSpace(item.CivilCode)
		if !validCatalogAdministrativeCode(civilCode) {
			return fmt.Errorf("protocol 3.0 Catalog item CivilCode must contain 2, 4, 6 or 8 ASCII digits")
		}
		return nil
	}
	requireParentID := func() error {
		if !validCatalogParentIDs(item.ParentID) {
			return fmt.Errorf("protocol 3.0 Catalog item ParentID is required and must contain 20-digit IDs")
		}
		return nil
	}

	switch kind {
	case GBCatalogItemAdministrative:
		return nil
	case GBCatalogItemSystem:
		if !requiredCatalogPlainStringPresent(item.Manufacturer, item.hasManufacturer) ||
			!requiredCatalogPlainStringPresent(item.Model, item.hasModel) ||
			!requiredCatalogPlainStringPresent(item.Address, item.hasAddress) {
			return fmt.Errorf("protocol 3.0 system Catalog item requires Manufacturer, Model and Address")
		}
		if err := requireCivilCode(); err != nil {
			return err
		}
		if strings.TrimSpace(item.Status) == "" {
			return fmt.Errorf("protocol 3.0 system Catalog item Status is required")
		}
		if !item.hasRegisterWay || item.RegisterWay < 1 || !item.hasSecrecy {
			return fmt.Errorf("protocol 3.0 system Catalog item requires RegisterWay and Secrecy")
		}
		if strings.TrimSpace(item.ParentID) != "" && !validCatalogParentIDs(item.ParentID) {
			return fmt.Errorf("protocol 3.0 system Catalog item ParentID must contain 20-digit IDs")
		}
		return nil
	case GBCatalogItemBusinessGroup:
		if err := requireCivilCode(); err != nil {
			return err
		}
		return requireParentID()
	case GBCatalogItemVirtualOrganization:
		if !isGBDeviceIdentifier(strings.TrimSpace(item.BusinessGroupID)) {
			return fmt.Errorf("protocol 3.0 virtual organization requires BusinessGroupID")
		}
		if strings.TrimSpace(item.ParentID) != "" && !validCatalogParentIDs(item.ParentID) {
			return fmt.Errorf("protocol 3.0 virtual organization ParentID must contain 20-digit IDs")
		}
		return nil
	case GBCatalogItemDevice:
		if !requiredCatalogPlainStringPresent(item.Manufacturer, item.hasManufacturer) ||
			!requiredCatalogPlainStringPresent(item.Model, item.hasModel) ||
			!requiredCatalogPlainStringPresent(item.Address, item.hasAddress) {
			return fmt.Errorf("protocol 3.0 device Catalog item requires Manufacturer, Model and Address")
		}
		if err := requireCivilCode(); err != nil {
			return err
		}
		if !item.hasParental {
			return fmt.Errorf("protocol 3.0 device Catalog item Parental is required")
		}
		if err := requireParentID(); err != nil {
			return err
		}
		if strings.TrimSpace(item.Status) == "" {
			return fmt.Errorf("protocol 3.0 device Catalog item Status is required")
		}
		if !item.hasRegisterWay || item.RegisterWay < 1 || !item.hasSecrecy {
			return fmt.Errorf("protocol 3.0 device Catalog item requires RegisterWay and Secrecy")
		}
		// 附录 J 的 132 摄像机示例允许整个 Info 缺省；设备携带 Info 时，
		// 再按 A.2.1.9 校验其中的摄像机条件必填字段。
		if isGBCameraIdentifier(deviceID) && item.Info.XMLName.Local != "" {
			return validateCatalog30CameraRequiredFields(item)
		}
		return nil
	default:
		return fmt.Errorf("invalid protocol 3.0 Catalog item DeviceID")
	}
}

func requiredCatalogPlainStringPresent(value string, present bool) bool {
	// 2022 A.2.1.9 将这些字段定义为条件必选的普通 string，未定义 minLength。
	// XML 解码路径以出现标记区分缺失和显式空值；内部构造的非空值保持兼容。
	return present || value != ""
}

func validateCatalog30CameraRequiredFields(item Channels) error {
	info := item.Info
	if !validOptionalDecimalLength(info.GrassrootsCode, 6) || strings.TrimSpace(info.GrassrootsCode) == "" {
		return fmt.Errorf("protocol 3.0 camera Catalog item GrassrootsCode is required")
	}
	if !info.hasPointType || info.PointType != 1 && info.PointType != 2 && info.PointType != 3 && info.PointType != 9 {
		return fmt.Errorf("protocol 3.0 camera Catalog item PointType is required")
	}
	if info.PointType == 1 || info.PointType == 2 {
		if !item.hasLongitude || !item.hasLatitude {
			return fmt.Errorf("protocol 3.0 class I/II camera Catalog item coordinates are required")
		}
	}
	if info.PointType == 1 {
		if strings.TrimSpace(info.InstallTime) == "" ||
			!requiredCatalogPlainStringPresent(info.ContactInfo, info.hasContactInfo) ||
			!info.hasRecordSaveDays {
			return fmt.Errorf("protocol 3.0 class I camera Catalog item requires InstallTime, ContactInfo and RecordSaveDays")
		}
	}
	return nil
}

func validCatalogAdministrativeCode(value string) bool {
	value = strings.TrimSpace(value)
	return (len(value) == 2 || len(value) == 4 || len(value) == 6 || len(value) == 8) && allDecimalDigits(value)
}

func validCatalogParentIDs(value string) bool {
	parts := strings.Split(strings.TrimSpace(value), "/")
	if len(parts) == 0 {
		return false
	}
	for _, part := range parts {
		if !isGBDeviceIdentifier(strings.TrimSpace(part)) {
			return false
		}
	}
	return true
}

func isGBCameraIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if !isGBDeviceIdentifier(value) {
		return false
	}
	switch value[10:13] {
	case "131", "132":
		return true
	default:
		return false
	}
}

func validateCatalog30Info(info CatalogItemInfo) error {
	if info.XMLName.Local != "" && strings.TrimSpace(info.GrassrootsCode) == "" {
		return fmt.Errorf("protocol 3.0 Catalog item Info requires GrassrootsCode")
	}
	if !validSlashSeparatedIntegers(info.PhotoelectricImagingType, 1, map[int]struct{}{1: {}, 2: {}, 3: {}, 4: {}, 5: {}, 9: {}}) {
		return fmt.Errorf("invalid Catalog item PhotoelectricImagingType")
	}
	if !validCapturePositionType(info.CapturePositionType) || !validOptionalDecimalLength(info.GrassrootsCode, 6) {
		return fmt.Errorf("invalid Catalog item position code")
	}
	if !validSlashSeparatedIntegers(info.StreamNumberList, 0, map[int]struct{}{0: {}, 1: {}, 2: {}}) {
		return fmt.Errorf("invalid Catalog item stream list")
	}
	if !validCatalogRatioList(info.SSVCRatioSupportList) {
		return fmt.Errorf("invalid Catalog item SSVCRatioSupportList")
	}
	if !validCatalogMobileDeviceType(info.MobileDeviceType) || info.hasMobileDeviceType && info.MobileDeviceType == 0 ||
		info.PointType != 0 && info.PointType != 1 && info.PointType != 2 && info.PointType != 3 && info.PointType != 9 ||
		info.hasPointType && info.PointType == 0 {
		return fmt.Errorf("invalid Catalog item device or point type")
	}
	if !validOptionalPositiveAngle(info.HorizontalFieldAngle) || info.hasHorizontalFieldAngle && info.HorizontalFieldAngle == 0 ||
		!validOptionalPositiveAngle(info.VerticalFieldAngle) || info.hasVerticalFieldAngle && info.VerticalFieldAngle == 0 ||
		math.IsNaN(info.MaxViewDistance) || math.IsInf(info.MaxViewDistance, 0) {
		return fmt.Errorf("invalid Catalog item field angle or view distance")
	}
	if mac := strings.TrimSpace(info.MAC); mac != "" && !validCatalogMAC(mac) {
		return fmt.Errorf("invalid Catalog item MAC")
	}
	if !validSlashSeparatedStrings(info.FunctionType, map[string]struct{}{"01": {}, "02": {}, "03": {}, "04": {}, "05": {}, "99": {}}) {
		return fmt.Errorf("invalid Catalog item FunctionType")
	}
	if installTime := strings.TrimSpace(info.InstallTime); info.hasInstallTime && installTime == "" || installTime != "" && !validGBDateTime(installTime) {
		return fmt.Errorf("Catalog item InstallTime must be a non-empty dateTime when present")
	}
	return nil
}

func validateCatalog20Info(info CatalogItemInfo) error {
	if !validSlashSeparatedIntegers(info.DownloadSpeed, 1, nil) {
		return fmt.Errorf("invalid Catalog item download list")
	}
	if info.SVCSpaceSupportMode < 0 || info.SVCSpaceSupportMode > 3 || info.SVCTimeSupportMode < 0 || info.SVCTimeSupportMode > 3 {
		return fmt.Errorf("invalid Catalog item SVC support mode")
	}
	return nil
}

// validCapturePositionType 校验 2022 附录 O 的大类和中类代码。
// 最后两位小类顺序码由行业规定，00 为标准默认值，因此这里只限制表 O.1 定义的前五位。
func validCapturePositionType(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if !validOptionalDecimalLength(value, 7) {
		return false
	}
	switch value[:5] {
	case "00101", "00102", "00103", "00104", "00105", "00199",
		"00201", "00202", "00203", "00204", "00297", "00298", "00299",
		"00301", "00302", "00303", "00304", "00305", "00306", "00307", "00308", "00399",
		"00401", "00402", "00499",
		"00501", "00502", "00503", "00599",
		"00601", "00602", "00603", "00604", "00605", "00699",
		"00701", "00799",
		"00801", "00802", "00803", "00804", "00805", "00806", "00807", "00808", "00809", "00896", "00897", "00898", "00899",
		"00901", "01001",
		"01101", "01102", "01199",
		"01201", "01202", "01203", "01204", "01205", "01206", "01207", "01299",
		"01301", "01399",
		"01401", "01402", "01403", "01499",
		"01501", "01502", "01503", "01504", "01505", "01599",
		"01601", "01602", "01603", "01699",
		"01701", "01702", "01703", "01799",
		"01801", "01802", "01803", "01899",
		"01901", "01902", "01903", "01904", "01905", "01999",
		"02001", "02002", "02099",
		"02101", "02102", "02103", "02199",
		"02201", "02299", "02301", "02399", "02401", "02499",
		"02501", "02502", "02599", "02601", "02699", "02701", "02799", "02801", "02899",
		"03101", "03102", "03199", "03201", "03202", "03203", "03299",
		"99899", "99999":
		return true
	default:
		return false
	}
}

func validOptionalDecimalLength(value string, length int) bool {
	value = strings.TrimSpace(value)
	return value == "" || len(value) == length && allDecimalDigits(value)
}

func validCatalogMobileDeviceType(value int) bool {
	return value == 0 || value >= 1 && value <= 5 || value == 9
}

func validSlashSeparatedIntegers(value string, minimum int, allowed map[int]struct{}) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	for _, part := range strings.Split(value, "/") {
		item, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || item < minimum {
			return false
		}
		if allowed != nil {
			if _, ok := allowed[item]; !ok {
				return false
			}
		}
	}
	return true
}

func validSlashSeparatedStrings(value string, allowed map[string]struct{}) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	for _, part := range strings.Split(value, "/") {
		if _, ok := allowed[strings.TrimSpace(part)]; !ok {
			return false
		}
	}
	return true
}

func validCatalogRatioList(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	for _, ratio := range strings.Split(value, "/") {
		parts := strings.Split(strings.TrimSpace(ratio), ":")
		if len(parts) != 2 {
			return false
		}
		for _, part := range parts {
			item, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || item <= 0 {
				return false
			}
		}
	}
	return true
}

func validOptionalPositiveAngle(value float64) bool {
	return value == 0 || !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0 && value <= 360
}

func validCatalogMAC(value string) bool {
	if len(value) != 17 {
		return false
	}
	for index, char := range value {
		if index%3 == 2 {
			if char != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", char) {
			return false
		}
	}
	return true
}

func hasCatalog30Info(info CatalogItemInfo) bool {
	return strings.TrimSpace(info.PhotoelectricImagingType) != "" || strings.TrimSpace(info.CapturePositionType) != "" ||
		strings.TrimSpace(info.StreamNumberList) != "" || strings.TrimSpace(info.SSVCRatioSupportList) != "" ||
		info.hasMobileDeviceType || info.MobileDeviceType != 0 || info.hasHorizontalFieldAngle || info.HorizontalFieldAngle != 0 ||
		info.hasVerticalFieldAngle || info.VerticalFieldAngle != 0 || info.MaxViewDistance != 0 ||
		strings.TrimSpace(info.GrassrootsCode) != "" || info.hasPointType || info.PointType != 0 || strings.TrimSpace(info.PointCommonName) != "" ||
		strings.TrimSpace(info.MAC) != "" || strings.TrimSpace(info.FunctionType) != "" || strings.TrimSpace(info.EncodeType) != "" ||
		info.hasInstallTime || strings.TrimSpace(info.InstallTime) != "" || strings.TrimSpace(info.ManagementUnit) != "" ||
		info.hasContactInfo || strings.TrimSpace(info.ContactInfo) != "" || info.hasRecordSaveDays || info.RecordSaveDays != 0 ||
		strings.TrimSpace(info.IndustrialClassification) != ""
}

func hasCatalog20Info(info CatalogItemInfo) bool {
	return strings.TrimSpace(info.DownloadSpeed) != "" || info.SVCSpaceSupportMode != 0 || info.SVCTimeSupportMode != 0
}

func validCatalogSupplyLightType(value int, version GBProtocolVersion) bool {
	if value == 0 {
		return true
	}
	if version == GBVersion30 {
		return value >= 1 && value <= 4 || value == 9
	}
	return value >= 1 && value <= 3
}

func validateCatalogPTZType(info CatalogItemInfo, version GBProtocolVersion) error {
	value := strings.TrimSpace(info.PTZTypeList)
	if value == "" {
		if info.hasPTZType && version != GBVersion30 {
			return fmt.Errorf("invalid Catalog item PTZType")
		}
		if info.PTZType == 0 {
			return nil
		}
		value = strconv.Itoa(info.PTZType)
	}
	parts := strings.Split(value, "/")
	if version != GBVersion30 && len(parts) != 1 {
		return fmt.Errorf("Catalog PTZType list requires protocol 3.0")
	}
	maximum := 4
	if version == GBVersion30 {
		maximum = 7
	}
	for _, part := range parts {
		item, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || item < 1 || item > maximum {
			return fmt.Errorf("invalid Catalog item PTZType")
		}
	}
	return nil
}

func validCatalogTargetID(version GBProtocolVersion, targetID string) bool {
	targetID = strings.TrimSpace(targetID)
	return isGBDeviceIdentifier(targetID) || version.AtLeast(GBVersion11) && classifyGBCatalogItem(targetID) == GBCatalogItemAdministrative
}

func (g *GB28181API) validateCatalogResponseTarget(ctx *sip.Context, targetID string, sn int) error {
	if err := g.validateAuthenticatedResponseTarget(ctx, targetID); err == nil {
		return nil
	}
	sourceID := ""
	if ctx != nil {
		sourceID = strings.TrimSpace(ctx.DeviceID)
	}
	targetID = strings.TrimSpace(targetID)
	if g.getDeviceGBProtocolVersion(sourceID).AtLeast(GBVersion11) && classifyGBCatalogItem(targetID) == GBCatalogItemAdministrative {
		if expected, ok := g.pendingDeviceQueryExpectedTarget(sourceID, "Catalog", sn); ok && expected == targetID {
			return nil
		}
		return fmt.Errorf("Catalog response target mismatch")
	}
	if len(sourceID) != 20 || len(targetID) != 20 || sourceID[:10] != targetID[:10] {
		return fmt.Errorf("Catalog response target mismatch")
	}
	switch classifyGBCatalogItem(targetID) {
	case GBCatalogItemSystem, GBCatalogItemBusinessGroup, GBCatalogItemVirtualOrganization:
		return nil
	default:
		return fmt.Errorf("Catalog response target mismatch")
	}
}

// QueryCatalog 设备目录查询或订阅请求
// GB/T28181 81 页 A.2.4.3
func (g *GB28181API) QueryCatalog(deviceID string) (err error) {
	return g.QueryCatalogContext(context.Background(), deviceID)
}

// CatalogIncompleteError 表示设备在等待期限内只返回了部分目录项。
// 不完整结果不会替换本地目录快照，调用方可通过 Received/Expected 提示具体完成度。
type CatalogIncompleteError struct {
	Received int
	Expected int
}

func (err *CatalogIncompleteError) Error() string {
	if err == nil {
		return "Catalog response incomplete"
	}
	return fmt.Sprintf("Catalog response incomplete: received %d of %d items", err.Received, err.Expected)
}

func catalogQueryResultError(result multiResponseResult[Channels]) error {
	if result.Complete {
		return nil
	}
	if len(result.Items) == 0 {
		return errors.New("wait Catalog response timeout")
	}
	return &CatalogIncompleteError{Received: len(result.Items), Expected: result.Expected}
}

// QueryCatalogContext 查询目录，并允许调用方取消 SIP 及分包聚合等待。
func (g *GB28181API) QueryCatalogContext(ctx context.Context, deviceID string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.lifecycleMu.Lock()
	if g.lifecycleClosed {
		g.lifecycleMu.Unlock()
		return ErrServiceStopped
	}
	if g.lifecycleCtx == nil {
		g.lifecycleCtx, g.lifecycleCancel = context.WithCancel(context.Background())
	}
	lifecycleCtx := g.lifecycleCtx
	g.lifecycleMu.Unlock()
	queryCtx, cancelQuery := context.WithCancelCause(ctx)
	stopLifecycleCancel := context.AfterFunc(lifecycleCtx, func() { cancelQuery(ErrServiceStopped) })
	defer stopLifecycleCancel()
	defer cancelQuery(nil)

	g.metrics.catalogRequests.Add(1)
	defer func() {
		if err == nil {
			g.metrics.catalogSuccess.Add(1)
		} else if strings.Contains(err.Error(), "Catalog response timeout") {
			g.metrics.catalogTimeouts.Add(1)
		}
	}()
	operation, releaseOperation := g.trackPendingDeviceRequest(queryCtx, deviceID, deviceID)
	defer releaseOperation()
	requestCtx := operation.Context(queryCtx)
	unlockCatalog, err := g.lockCatalogOperation(requestCtx, deviceID)
	if err != nil {
		return operation.ErrorOr(err)
	}
	defer unlockCatalog()
	if g.serviceStopped() {
		return ErrServiceStopped
	}
	slog.Debug("QueryCatalog", "deviceID", deviceID)
	ipc, ok := g.svr.memoryStorer.Load(deviceID)
	if !ok || !ipc.IsOnlineNow() {
		return ErrDeviceOffline
	}

	var (
		sn           int
		key          string
		catalogEntry *multiResponseEntry[Channels]
	)
	for {
		sn = g.nextQuerySN()
		key = buildMultiResponseKey(deviceID, "Catalog", sn)
		entry, started := g.catalogResponses.TryStart(key)
		if entry == nil {
			return ErrServiceStopped
		}
		if !started {
			continue
		}
		catalogEntry = entry
		break
	}
	g.pendingMultiResponse.Store(key, operation)
	defer g.pendingMultiResponse.CompareAndDelete(key, operation)
	body, err := sip.XMLEncode(genericDeviceQueryRequest{
		CmdType:  "Catalog",
		SN:       sn,
		DeviceID: deviceID,
	})
	if err != nil {
		g.catalogResponses.CancelEntry(key, catalogEntry)
		return err
	}
	tx, err := g.svr.wrapRequestContext(requestCtx, ipc, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		g.catalogResponses.CancelEntry(key, catalogEntry)
		return operation.ErrorOr(err)
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		g.catalogResponses.CancelEntry(key, catalogEntry)
		return operation.ErrorOr(err)
	}
	waitCtx, cancel := context.WithTimeout(requestCtx, 10*time.Second)
	defer cancel()
	result := g.catalogResponses.WaitEntry(waitCtx, key, catalogEntry)
	if g.serviceStopped() {
		return ErrServiceStopped
	}
	if result.Err != nil {
		return result.Err
	}
	if !result.Complete && requestCtx.Err() != nil {
		return operation.Cause()
	}
	if resultErr := catalogQueryResultError(result); resultErr != nil {
		if len(result.Items) == 0 {
			return resultErr
		}
		g.metrics.catalogPartial.Add(1)
		slog.Warn("Catalog response incomplete", "device_id", deviceID, "received", len(result.Items), "expected", result.Expected)
		return resultErr
	}
	return g.persistCatalogResultContext(requestCtx, deviceID, result)
}

func (g *GB28181API) lockCatalogOperation(ctx context.Context, deviceID string) (func(), error) {
	if g == nil {
		return nil, fmt.Errorf("GB28181 service is unavailable")
	}
	deviceID = strings.TrimSpace(deviceID)
	g.catalogOperationMu.Lock()
	if g.catalogOperations == nil {
		g.catalogOperations = make(map[string]*keyedOperationLock)
	}
	entry := g.catalogOperations[deviceID]
	if entry == nil {
		entry = &keyedOperationLock{}
		g.catalogOperations[deviceID] = entry
	}
	entry.refs++
	g.catalogOperationMu.Unlock()
	if err := entry.mutex.LockContext(ctx); err != nil {
		g.releaseCatalogOperationRef(deviceID, entry)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mutex.Unlock()
			g.releaseCatalogOperationRef(deviceID, entry)
		})
	}, nil
}

func (g *GB28181API) releaseCatalogOperationRef(deviceID string, entry *keyedOperationLock) {
	g.catalogOperationMu.Lock()
	if current := g.catalogOperations[deviceID]; current == entry {
		entry.refs--
		if entry.refs == 0 {
			delete(g.catalogOperations, deviceID)
		}
	}
	g.catalogOperationMu.Unlock()
}

func (g *GB28181API) scheduleCatalogRefresh(deviceID string) bool {
	return g.scheduleCatalogRefreshAfter(deviceID, 0)
}

func (g *GB28181API) scheduleCatalogRefreshAfter(deviceID string, delay time.Duration) bool {
	if g == nil {
		return false
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false
	}
	if delay < 0 {
		delay = 0
	}
	now := time.Now()
	nextAt := now.Add(delay)
	g.catalogRefreshMu.Lock()
	if g.catalogRefreshes == nil {
		g.catalogRefreshes = make(map[string]*catalogRefreshState)
	}
	if current := g.catalogRefreshes[deviceID]; current != nil {
		wake := false
		switch {
		case current.running:
			current.dirty = true
			if current.nextAt.IsZero() || nextAt.Before(current.nextAt) {
				current.nextAt = nextAt
			}
		case current.nextAt.After(now):
			// 尚未执行的延迟补查已经代表当前刷新需求；只允许更早的触发提前它。
			if nextAt.Before(current.nextAt) {
				current.nextAt = nextAt
				wake = true
			}
		default:
			// 保留原有即时通知语义：已进入可执行窗口的重复触发合并为一次跟进查询。
			current.dirty = true
			if current.nextAt.IsZero() || nextAt.Before(current.nextAt) {
				current.nextAt = nextAt
			}
			wake = true
		}
		wakeCh := current.wake
		g.catalogRefreshMu.Unlock()
		if wake {
			signalCatalogRefresh(wakeCh)
		}
		return true
	}
	state := &catalogRefreshState{nextAt: nextAt, wake: make(chan struct{}, 1)}
	g.catalogRefreshes[deviceID] = state
	g.catalogRefreshMu.Unlock()

	started := g.startLifecycleTask(context.Background(), func(taskCtx context.Context) {
		g.runCatalogRefresh(taskCtx, deviceID, state, g.QueryCatalogContext)
	})
	if started {
		return true
	}
	g.catalogRefreshMu.Lock()
	if g.catalogRefreshes[deviceID] == state {
		delete(g.catalogRefreshes, deviceID)
	}
	g.catalogRefreshMu.Unlock()
	return false
}

// scheduleCatalogRefreshAfterFailure 为设备上线后的首次目录同步提供一次异步补查。
// 离线、停服和调用方取消不是可重试故障，不能在后台重新开启业务。
func (g *GB28181API) scheduleCatalogRefreshAfterFailure(deviceID string, err error) bool {
	return g.scheduleCatalogRefreshAfterFailureWithDelay(deviceID, err, catalogRefreshFailureBackoff)
}

func (g *GB28181API) scheduleCatalogRefreshAfterFailureWithDelay(deviceID string, err error, delay time.Duration) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrServiceStopped) ||
		errors.Is(err, ErrDeviceOffline) || errors.Is(err, ErrDeviceNotExist) {
		return false
	}
	return g.scheduleCatalogRefreshAfter(deviceID, delay)
}

func (g *GB28181API) runCatalogRefresh(ctx context.Context, deviceID string, state *catalogRefreshState, query func(context.Context, string) error) {
	defer g.removeCatalogRefreshState(deviceID, state)
	for {
		if !g.waitCatalogRefresh(ctx, deviceID, state) {
			return
		}
		err := query(ctx, deviceID)
		if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrServiceStopped) {
			slog.Warn("refresh Catalog failed", "device_id", deviceID, "err", err)
		}
		g.catalogRefreshMu.Lock()
		current := g.catalogRefreshes[deviceID]
		if current == state {
			state.running = false
		}
		if current == state && state.dirty && ctx.Err() == nil {
			state.dirty = false
			if state.nextAt.IsZero() {
				state.nextAt = time.Now()
			}
			g.catalogRefreshMu.Unlock()
			continue
		}
		g.catalogRefreshMu.Unlock()
		return
	}
}

func (g *GB28181API) waitCatalogRefresh(ctx context.Context, deviceID string, state *catalogRefreshState) bool {
	for {
		g.catalogRefreshMu.Lock()
		if g.catalogRefreshes[deviceID] != state {
			g.catalogRefreshMu.Unlock()
			return false
		}
		wait := time.Until(state.nextAt)
		if state.nextAt.IsZero() || wait <= 0 {
			state.running = true
			state.nextAt = time.Time{}
			g.catalogRefreshMu.Unlock()
			return true
		}
		wake := state.wake
		g.catalogRefreshMu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			stopCatalogRefreshTimer(timer)
			return false
		case <-wake:
			stopCatalogRefreshTimer(timer)
		case <-timer.C:
		}
	}
}

func (g *GB28181API) removeCatalogRefreshState(deviceID string, state *catalogRefreshState) {
	g.catalogRefreshMu.Lock()
	if g.catalogRefreshes[deviceID] == state {
		delete(g.catalogRefreshes, deviceID)
	}
	g.catalogRefreshMu.Unlock()
}

func signalCatalogRefresh(wake chan struct{}) {
	if wake == nil {
		return
	}
	select {
	case wake <- struct{}{}:
	default:
	}
}

func stopCatalogRefreshTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

// persistCatalogResult 只使用完整目录替换本地快照，避免丢包时把未收到的通道错误标记为离线。
func (g *GB28181API) persistCatalogResult(deviceID string, result multiResponseResult[Channels]) error {
	return g.persistCatalogResultContext(context.Background(), deviceID, result)
}

func (g *GB28181API) persistCatalogResultContext(ctx context.Context, deviceID string, result multiResponseResult[Channels]) error {
	if !result.Complete {
		return nil
	}
	return g.saveCatalogChannelsContext(ctx, deviceID, result.Items)
}

func (g *GB28181API) saveCatalogChannels(deviceID string, items []Channels) error {
	return g.saveCatalogChannelsContext(context.Background(), deviceID, items)
}

func (g *GB28181API) saveCatalogChannelsContext(ctx context.Context, deviceID string, items []Channels) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for index := range items {
		items[index].ChannelID = strings.TrimSpace(items[index].ChannelID)
	}
	offlineChannels := make(map[string]struct{})
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item.Status), "OFF") {
			offlineChannels[item.ChannelID] = struct{}{}
		}
	}
	cfg := g.configSnapshot()
	domain := ""
	if cfg != nil {
		domain = cfg.GetDomain()
	}
	var runtimeDevice *Device
	if g.svr != nil && g.svr.memoryStorer != nil {
		runtimeDevice, _ = g.svr.memoryStorer.Load(deviceID)
	}
	channelDomain := domain
	if runtimeDevice != nil {
		if runtimeDevice.To() != nil && runtimeDevice.To().URI != nil && runtimeDevice.To().URI.Host() != "" {
			channelDomain = runtimeDevice.To().URI.Host()
		}
	}
	runtimeChannels := make([]*Channel, 0, len(items))
	for _, item := range items {
		channel := &Channel{ChannelID: item.ChannelID, device: runtimeDevice}
		if err := channel.init(channelDomain); err != nil {
			return fmt.Errorf("reject invalid GB28181 Catalog snapshot for device %q channel %q: %w", deviceID, item.ChannelID, err)
		}
		runtimeChannels = append(runtimeChannels, channel)
	}

	out := make([]*ipc.Channel, 0, len(items))
	for _, item := range items {
		out = append(out, &ipc.Channel{
			DeviceID:  deviceID,
			ChannelID: item.ChannelID,
			Name:      item.Name,
			PTZType:   item.Info.PTZType,
			IsOnline:  item.Status == "OK" || item.Status == "ON",
			Ext:       catalogChannelExt(item),
			Type:      ipc.TypeGB28181,
		})
	}
	if g.core.Store() != nil {
		if err := g.core.SaveChannelSnapshot(ctx, deviceID, out); err != nil {
			return fmt.Errorf("persist Catalog snapshot for device %q: %w", deviceID, err)
		}
	}

	if runtimeDevice != nil {
		seen := make(map[string]struct{}, len(items))
		for _, channel := range runtimeChannels {
			seen[channel.ChannelID] = struct{}{}
			runtimeDevice.Channels.Store(channel.ChannelID, channel)
		}
		runtimeDevice.Channels.Range(func(channelID string, _ *Channel) bool {
			if _, ok := seen[channelID]; !ok {
				offlineChannels[channelID] = struct{}{}
				runtimeDevice.Channels.Delete(channelID)
			}
			return true
		})
	}
	g.terminateChannelMediaSessions(g.mediaPersistenceContext(), deviceID, offlineChannels, "channel_offline")
	g.startLifecycleTask(context.Background(), g.notifyCascadeCatalog)
	return nil
}

type Targeter interface {
	To() *sip.Address
	Conn() sip.Connection
	Source() net.Addr
}

type gbVersioner interface {
	GBVersion() string
}

type RequestOption func(*sip.Request)

func requestTargetSnapshot(target Targeter) (*sip.Address, sip.Connection, net.Addr) {
	switch value := target.(type) {
	case *Device:
		state := value.runtimeSnapshot()
		return state.To, state.Conn, state.Source
	case *Channel:
		if value == nil || value.device == nil {
			return nil, nil, nil
		}
		state := value.device.runtimeSnapshot()
		var to *sip.Address
		if value.to != nil {
			to = value.to.Clone()
		}
		return to, state.Conn, state.Source
	default:
		return target.To(), target.Conn(), target.Source()
	}
}

// prepareDialogRequestTransport 优先复用建立对话的响应路径，仅在旧响应缺少传输元数据时回退注册链路。
func prepareDialogRequestTransport(request *sip.Request, target Targeter) error {
	if request == nil {
		return fmt.Errorf("SIP dialog request is unavailable")
	}
	var connection sip.Connection
	var destination net.Addr
	if target != nil {
		_, connection, destination = requestTargetSnapshot(target)
	}
	// TCP/TLS 注册可能已重连；发送连接取当前运行态，对话 Request-URI/Route 仍由原响应决定。
	if connection != nil && (request.GetConnection() == nil || !strings.EqualFold(request.Transport(), "UDP")) {
		request.SetConnection(connection)
	}
	if request.GetConnection() == nil {
		return fmt.Errorf("SIP dialog connection is unavailable")
	}
	if request.Destination() == nil {
		if destination == nil {
			return fmt.Errorf("SIP dialog destination is unavailable")
		}
		request.SetDestination(destination)
	}
	return nil
}

// requestDialogContext 统一发送已建立对话内的 INFO/BYE 等请求，复用当前设备连接，
// 并与初始 MESSAGE/INVITE 一样应用跨域身份和 Date+Note 事务级安全策略。
func (s *Server) requestDialogContext(ctx context.Context, target Targeter, request *sip.Request) (*sip.Transaction, error) {
	return s.requestDialogContextInternal(ctx, target, request, false)
}

// requestDialogCleanupContext 仅用于已存在对话的收尾请求，允许在 GB 服务停服窗口内发送 BYE。
func (s *Server) requestDialogCleanupContext(ctx context.Context, target Targeter, request *sip.Request) (*sip.Transaction, error) {
	return s.requestDialogContextInternal(ctx, target, request, true)
}

// requestFromResponseContext 从 UAC 对话响应构造并发送后续请求。
// 连接、生命周期、跨域身份和信令摘要等纯本地准备全部成功后，才提交
// Response 维护的本端 CSeq；真正进入网络发送后则保留已经提交的序号。
func (s *Server) requestFromResponseContext(ctx context.Context, target Targeter, method string, response *sip.Response) (*sip.Transaction, error) {
	return s.requestFromResponseContextInternal(ctx, target, method, response, false, nil, nil)
}

// requestFromResponseCleanupContext 与 requestFromResponseContext 相同，但允许在停服收尾窗口发送 BYE。
func (s *Server) requestFromResponseCleanupContext(ctx context.Context, target Targeter, method string, response *sip.Response) (*sip.Transaction, error) {
	return s.requestFromResponseContextInternal(ctx, target, method, response, true, nil, nil)
}

func (s *Server) requestFromResponsePreparedContext(
	ctx context.Context,
	target Targeter,
	method string,
	response *sip.Response,
	prepare func(*sip.Request) error,
) (*sip.Transaction, error) {
	return s.requestFromResponseContextInternal(ctx, target, method, response, false, prepare, nil)
}

func (s *Server) requestFromResponsePreparedContextWithLocalFailure(
	ctx context.Context,
	target Targeter,
	method string,
	response *sip.Response,
	prepare func(*sip.Request) error,
	onLocalFailure func(),
) (*sip.Transaction, error) {
	return s.requestFromResponseContextInternal(ctx, target, method, response, false, prepare, onLocalFailure)
}

func (s *Server) requestFromResponseContextInternal(
	ctx context.Context,
	target Targeter,
	method string,
	response *sip.Response,
	allowStopped bool,
	prepare func(*sip.Request) error,
	onLocalFailure func(),
) (tx *sip.Transaction, err error) {
	writePhase := false
	defer func() {
		if err != nil && !writePhase && onLocalFailure != nil {
			onLocalFailure()
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	var security sip.MessageSecurity
	var prepared *sip.PreparedRequest
	_, err = sip.NewRequestFromResponsePreparedChecked(method, response, func(request *sip.Request) error {
		// 先由 NewRequestFromResponsePreparedChecked 完成响应与对话头的纯本地结构校验，
		// 再检查服务运行态。这样畸形 2xx 不会被“服务不可用”遮蔽，同时本地失败
		// 仍发生在候选 CSeq 提交之前。
		if s == nil || s.Server == nil || s.gb == nil {
			return fmt.Errorf("SIP server is unavailable")
		}
		if !allowStopped && s.gb.serviceStopped() {
			return ErrServiceStopped
		}
		if prepare != nil {
			if err := prepare(request); err != nil {
				return err
			}
		}
		if err := prepareDialogRequestTransport(request, target); err != nil {
			return err
		}
		if err := applyForwardedMonitorUserIdentity(ctx, request); err != nil {
			return err
		}
		var err error
		security, err = s.gb.newSignalDigestSecurity(targetSignalDigestSeed(target))
		if err != nil {
			return err
		}
		prepared, err = s.Server.PrepareRequestWithSecurityContext(ctx, request, security)
		return err
	})
	if err != nil {
		if prepared != nil {
			prepared.Close()
		}
		return nil, err
	}
	writePhase = true
	return prepared.Send()
}

// consumeDialogResponseAsync 消费无需业务等待的对话收尾请求最终响应，并及时回收客户端事务。
// 调用方仍只同步等待初始网络写；后台等待受 GB 服务生命周期和 SIP 事务超时共同约束。
func (g *GB28181API) consumeDialogResponseAsync(tx *sip.Transaction) {
	if tx == nil {
		return
	}
	if g == nil || !g.startLifecycleTask(context.Background(), func(ctx context.Context) {
		defer tx.Close()
		_, _ = tx.GetResponseContext(ctx)
	}) {
		tx.Close()
	}
}

func (s *Server) requestDialogContextInternal(ctx context.Context, target Targeter, request *sip.Request, allowStopped bool) (*sip.Transaction, error) {
	if s == nil || s.Server == nil || s.gb == nil {
		return nil, fmt.Errorf("SIP server is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !allowStopped && s.gb.serviceStopped() {
		return nil, ErrServiceStopped
	}
	if err := prepareDialogRequestTransport(request, target); err != nil {
		return nil, err
	}
	if err := applyForwardedMonitorUserIdentity(ctx, request); err != nil {
		return nil, err
	}
	security, err := s.gb.newSignalDigestSecurity(targetSignalDigestSeed(target))
	if err != nil {
		return nil, err
	}
	return s.RequestWithSecurityContext(ctx, request, security)
}

func (s *Server) requestDialog(target Targeter, request *sip.Request) (*sip.Transaction, error) {
	ctx, cancel := context.WithTimeout(context.Background(), legacyDeviceSIPWriteTimeout)
	defer cancel()
	return s.requestDialogContext(ctx, target, request)
}

func (s *Server) dialogTarget(deviceID, channelID string) Targeter {
	if s == nil || s.memoryStorer == nil {
		return nil
	}
	if strings.TrimSpace(channelID) != "" {
		if channel, ok := s.memoryStorer.GetChannel(deviceID, channelID); ok {
			return channel
		}
	}
	if device, ok := s.memoryStorer.Load(deviceID); ok {
		return device
	}
	return nil
}

func (s *Server) wrapRequest(t Targeter, method string, contentType *sip.ContentType, body []byte, opts ...RequestOption) (*sip.Transaction, error) {
	ctx, cancel := context.WithTimeout(context.Background(), legacyDeviceSIPWriteTimeout)
	defer cancel()
	return s.wrapRequestContext(ctx, t, method, contentType, body, opts...)
}

func (s *Server) wrapRequestContext(ctx context.Context, t Targeter, method string, contentType *sip.ContentType, body []byte, opts ...RequestOption) (*sip.Transaction, error) {
	return s.wrapRequestContextInternal(ctx, t, method, contentType, body, false, opts...)
}

// wrapRequestCleanupContext 仅用于既存对话的收尾请求，允许在停服收尾窗口发送。
func (s *Server) wrapRequestCleanupContext(ctx context.Context, t Targeter, method string, contentType *sip.ContentType, body []byte, opts ...RequestOption) (*sip.Transaction, error) {
	return s.wrapRequestContextInternal(ctx, t, method, contentType, body, true, opts...)
}

func (s *Server) wrapRequestContextInternal(ctx context.Context, t Targeter, method string, contentType *sip.ContentType, body []byte, allowStopped bool, opts ...RequestOption) (*sip.Transaction, error) {
	if s == nil || s.Server == nil || s.gb == nil {
		return nil, fmt.Errorf("SIP server is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("SIP request target is unavailable")
	}
	if !allowStopped && s.gb.serviceStopped() {
		return nil, ErrServiceStopped
	}
	to, conn, source := requestTargetSnapshot(t)
	if to == nil || to.URI == nil || strings.TrimSpace(to.URI.Host()) == "" {
		return nil, fmt.Errorf("SIP request target URI is unavailable")
	}
	if conn == nil {
		return nil, fmt.Errorf("SIP request target connection is unavailable")
	}
	if source == nil {
		return nil, fmt.Errorf("SIP request target address is unavailable")
	}

	var viaPort *sip.Port
	if s.fromAddress.URI != nil && s.fromAddress.URI.FPort != nil {
		port := *s.fromAddress.URI.FPort
		viaPort = &port
	}
	hb := sip.NewHeaderBuilder().
		SetTo(to).
		SetFrom(&s.fromAddress).
		SetContentType(contentType).
		SetMethod(method).
		SetContact(&s.fromAddress).
		AddVia(&sip.ViaHop{
			Host: strings.TrimSpace(s.fromAddress.URI.Host()), Port: viaPort, Transport: sip.SignalingTransport(conn),
			Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()}).Add("rport", nil),
		})

	if v, ok := t.(gbVersioner); ok {
		version, valid := ParseGBProtocolVersion(v.GBVersion())
		if !valid {
			// 未知设备使用保守的 1.0 档案，避免发送设备无法识别的扩展。
			version = GBVersion10
		}
		hb.SetXGBVerValue(string(version))
	}

	req := sip.NewRequest("", method, to.URI, sip.DefaultSipVersion, hb.Build(), body)
	req.SetConnection(conn)
	req.SetSource(source)
	req.SetDestination(source)

	for _, opt := range opts {
		opt(req)
	}
	if err := applyForwardedMonitorUserIdentity(ctx, req); err != nil {
		return nil, err
	}

	security, err := s.gb.newSignalDigestSecurity(targetSignalDigestSeed(t))
	if err != nil {
		return nil, err
	}
	wireLength, err := signedSIPRequestLength(req, security)
	if err != nil {
		return nil, err
	}
	reliableMethod := strings.EqualFold(method, sip.MethodMessage) || strings.EqualFold(method, sip.MethodNotify)
	if reliableMethod && strings.EqualFold(sip.SignalingTransport(conn), "UDP") && wireLength > cascadeReliableTransportThreshold {
		reliableTarget, err := cascadeTCPDestination(req.Destination())
		if err != nil {
			return nil, fmt.Errorf("resolve oversized device SIP/TCP target: %w", err)
		}
		dial := s.dialDeviceTCP
		if dial == nil {
			dial = func(ctx context.Context, address string) (net.Conn, error) {
				return (&net.Dialer{Timeout: legacyDeviceSIPWriteTimeout}).DialContext(ctx, "tcp", address)
			}
		}
		raw, err := dial(ctx, reliableTarget.String())
		if err != nil {
			return nil, fmt.Errorf("dial oversized device SIP/TCP target %s: %w", reliableTarget, err)
		}
		reliableConnection := sip.NewTCPConnection(raw)
		req.SetConnection(reliableConnection)
		req.SetSource(reliableConnection.LocalAddr())
		req.SetDestination(reliableTarget)
		applyCascadeRequestNextHopTransport(req, "tcp")
		applyCascadeRequestTransport(req, "tcp")
		go s.Server.ProcessTCPConnection(reliableConnection)
		return s.RequestWithSecurityContextOwnedConnection(ctx, req, security)
	}
	return s.RequestWithSecurityContext(ctx, req, security)
}
