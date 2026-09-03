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

const (
	// RecordInfo 原始分包仅用于诊断和内部结果转换；单次查询保留上限与 SIP 单包 1 MiB 门禁配合，
	// 允许若干大分包或大量小分包，同时避免重复条目通过变化元数据无界占用内存。
	gbRecordResponseMaxMetadataBytes   = 8 << 20
	gbRecordResponseMaxMetadataObjects = 4096
)

var errRecordResponseMetadataLimit = errors.New("GB28181 RecordInfo metadata limit exceeded")

type RecordQueryInput struct {
	DeviceID        string
	ChannelID       string // RecordInfo 目录目标，可为注册设备/系统编码或其通道/区域编码
	Start           int64  // 查询起始时间（unix 秒）
	End             int64  // 查询结束时间（unix 秒）
	OmitStartTime   bool
	OmitEndTime     bool
	Timeout         time.Duration
	FilePath        string
	Address         string
	Secrecy         *int
	Type            string
	RecorderID      string
	IndistinctQuery *int
	StreamNumber    *int
	AlarmMethod     string
	AlarmType       string
}

// QueryRecordList 查询设备录像目录（RecordInfo）
func (g *GB28181API) QueryRecordList(ctx context.Context, in *RecordQueryInput) (*Records, error) {
	records, _, err := g.queryRecordListResult(ctx, in)
	return records, err
}

func (g *GB28181API) queryRecordListResult(ctx context.Context, in *RecordQueryInput) (*Records, recordQueryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, recordQueryResult{}, err
	}
	if in == nil {
		return nil, recordQueryResult{}, errors.New("invalid record query input")
	}
	query := *in
	start, end := query.Start, query.End
	if !g.getDeviceGBProtocolVersion(query.DeviceID).AtLeast(GBVersion20) {
		query.OmitStartTime = query.Start <= 0
		query.OmitEndTime = query.End <= 0
		if query.OmitStartTime {
			query.Start = 0
			start = 0
		}
		if query.OmitEndTime {
			query.End = time.Now().Unix()
			end = query.End
		}
	}
	result, err := g.queryRecordResult(ctx, &query)
	if err != nil && len(result.Items) == 0 {
		return nil, result, err
	}
	records := transRecordItems(result.Items, start, end)
	return &records, result, err
}

type recordQueryResult struct {
	SN          int
	Items       []RecordItem
	ExtraInfo   []string
	ResponseXML []string
	AppendixA4  []AppendixA4Object
}

// recordResponseGeneration 把别名和附加元数据绑定到一次具体查询，避免 SN 回绕后旧清理误删新代次。
type recordResponseGeneration struct {
	_ byte
}

type recordResponseAlias struct {
	recordKey             string
	generation            *recordResponseGeneration
	requireRecordLocation bool
}

func (g *GB28181API) queryRecordResult(ctx context.Context, in *RecordQueryInput) (recordQueryResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return recordQueryResult{}, err
	}
	if in == nil || in.DeviceID == "" || in.ChannelID == "" {
		return recordQueryResult{}, errors.New("invalid record query input")
	}
	version := g.getDeviceGBProtocolVersion(in.DeviceID)
	if version.AtLeast(GBVersion20) && (in.OmitStartTime || in.OmitEndTime) {
		return recordQueryResult{}, errors.New("record query start/end are required by GB/T 28181-2016 and later")
	}
	if (!in.OmitStartTime && in.Start <= 0) || (!in.OmitEndTime && in.End <= 0) ||
		(!in.OmitStartTime && !in.OmitEndTime && in.End <= in.Start) {
		return recordQueryResult{}, errors.New("invalid record query time range")
	}

	device, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !device.IsOnlineNow() {
		return recordQueryResult{}, ErrDeviceOffline
	}
	var target Targeter = device
	if in.ChannelID != in.DeviceID {
		ch, exists := g.svr.memoryStorer.GetChannel(in.DeviceID, in.ChannelID)
		if !exists {
			return recordQueryResult{}, ErrChannelNotExist
		}
		target = ch
	}
	if err := validateRecordQueryFilters(version, in); err != nil {
		return recordQueryResult{}, err
	}
	recordType, _ := normalizeRecordQueryType(in.Type)
	alarmMethod, _ := formatAlarmMethodFilter(version, in.AlarmMethod)

	if in.Timeout <= 0 {
		in.Timeout = 10 * time.Second
	}
	operation, releaseOperation := g.trackPendingDeviceRequest(ctx, in.DeviceID, in.ChannelID)
	defer releaseOperation()
	requestCtx := operation.Context(ctx)

	var (
		sn          int
		recordKey   string
		aliasKey    string
		queryAlias  *recordResponseAlias
		recordEntry *multiResponseEntry[RecordItem]
	)
	for {
		if err := requestCtx.Err(); err != nil {
			return recordQueryResult{}, operation.Cause()
		}
		sn = g.nextQuerySN()
		recordKey = buildMultiResponseKey(in.ChannelID, "RecordInfo", sn)
		aliasKey = buildMultiResponseKey(in.DeviceID, "RecordInfo", sn)
		entry, started := g.recordResponses.TryStart(recordKey)
		if entry == nil {
			return recordQueryResult{}, ErrServiceStopped
		}
		if !started {
			continue
		}
		generation := &recordResponseGeneration{}
		requireRecordLocation := version == GBVersion30 && in.IndistinctQuery != nil && *in.IndistinctQuery == 1
		alias := &recordResponseAlias{
			recordKey: recordKey, generation: generation, requireRecordLocation: requireRecordLocation,
		}
		if _, loaded := g.recordResponseAliases.LoadOrStore(aliasKey, alias); loaded {
			g.recordResponses.CancelEntry(recordKey, entry)
			continue
		}
		g.startRecordResponseExtraGeneration(recordKey, generation)
		queryAlias = alias
		recordEntry = entry
		break
	}
	defer g.recordResponseAliases.CompareAndDelete(aliasKey, queryAlias)
	g.pendingMultiResponse.Store(recordKey, operation)
	defer g.pendingMultiResponse.CompareAndDelete(recordKey, operation)
	defer g.clearRecordResponseExtraGeneration(recordKey, queryAlias.generation)

	// 按 GB28181 A.2.4.9 发送 RecordInfo 查询命令。
	tx, err := g.svr.wrapRequestContext(requestCtx, target, sip.MethodMessage, &sip.ContentTypeXML, sip.GetRecordInfoXMLWithFilters(in.ChannelID, sn, in.Start, in.End, sip.RecordInfoQueryFilters{
		OmitStartTime:   in.OmitStartTime,
		OmitEndTime:     in.OmitEndTime,
		FilePath:        in.FilePath,
		Address:         in.Address,
		Secrecy:         in.Secrecy,
		Type:            recordType,
		RecorderID:      in.RecorderID,
		IndistinctQuery: in.IndistinctQuery,
		StreamNumber:    in.StreamNumber,
		AlarmMethod:     alarmMethod,
		AlarmType:       in.AlarmType,
	}))
	if err != nil {
		g.recordResponses.CancelEntry(recordKey, recordEntry)
		return recordQueryResult{}, operation.ErrorOr(err)
	}
	if _, err = sipResponseContext(requestCtx, tx); err != nil {
		g.recordResponses.CancelEntry(recordKey, recordEntry)
		return recordQueryResult{}, operation.ErrorOr(err)
	}
	waitCtx, cancel := context.WithTimeout(requestCtx, in.Timeout)
	defer cancel()
	result := g.recordResponses.WaitEntry(waitCtx, recordKey, recordEntry)
	metadata := g.takeRecordResponseMetadata(recordKey, queryAlias.generation)
	if g.serviceStopped() {
		return recordQueryResult{}, ErrServiceStopped
	}
	if result.Err != nil {
		return recordQueryResult{}, result.Err
	}
	if !result.Complete && requestCtx.Err() != nil {
		return recordQueryResult{}, operation.Cause()
	}
	items, err := recordQueryItemsResult(result)
	return recordQueryResult{
		SN: sn, Items: items, ExtraInfo: metadata.ExtraInfo,
		ResponseXML: metadata.ResponseXML, AppendixA4: metadata.AppendixA4,
	}, err
}

func validateRecordQueryFilters(version GBProtocolVersion, in *RecordQueryInput) error {
	if in == nil {
		return errors.New("invalid record query input")
	}
	if _, err := normalizeRecordQueryType(in.Type); err != nil {
		return err
	}
	if in.Secrecy != nil && *in.Secrecy != 0 && *in.Secrecy != 1 {
		return fmt.Errorf("record query secrecy must be 0 or 1")
	}
	if in.IndistinctQuery != nil {
		if !version.AtLeast(GBVersion11) {
			return fmt.Errorf("record query indistinct_query requires GB/T 28181-2014 or later")
		}
		if *in.IndistinctQuery != 0 && *in.IndistinctQuery != 1 {
			return fmt.Errorf("record query indistinct_query must be 0 or 1")
		}
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
	method, err := normalizeAlarmMethodFilter(method)
	if err != nil {
		return fmt.Errorf("invalid record query alarm method: %w", err)
	}
	if alarmType != "" && !alarmTypeMatchesMethods(version, method, alarmType) {
		return fmt.Errorf("invalid record query alarm type for method")
	}
	return nil
}

func normalizeRecordQueryType(value string) (string, error) {
	if value == "" {
		return "time", nil
	}
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "time", "alarm", "manual", "all":
		return value, nil
	default:
		return "", fmt.Errorf("invalid record query type")
	}
}

// RecordInfoIncompleteError 表示设备在等待期限内只返回了部分录像目录项。
type RecordInfoIncompleteError struct {
	Received int
	Expected int
}

func (err *RecordInfoIncompleteError) Error() string {
	if err == nil {
		return "RecordInfo response incomplete"
	}
	return fmt.Sprintf("RecordInfo response incomplete: received %d of %d items", err.Received, err.Expected)
}

func recordQueryItemsResult(result multiResponseResult[RecordItem]) ([]RecordItem, error) {
	if len(result.Items) == 0 && !result.Complete {
		return nil, errors.New("wait RecordInfo response timeout")
	}
	if !result.Complete {
		return result.Items, &RecordInfoIncompleteError{Received: len(result.Items), Expected: result.Expected}
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
	HasName   bool         `xml:"-"`
	SumNum    int          `xml:"SumNum"`
	HasSumNum bool         `xml:"-"`
	Item      []RecordItem `xml:"-"`
	ListNum   *int         `xml:"-"`
	HasList   bool         `xml:"-"`
}

func (m *MessageRecordInfoResponse) UnmarshalXML(decoder *xml.Decoder, start xml.StartElement) error {
	var value struct {
		CmdType  string  `xml:"CmdType"`
		SN       int     `xml:"SN"`
		DeviceID string  `xml:"DeviceID"`
		Name     *string `xml:"Name"`
		SumNum   *int    `xml:"SumNum"`
		List     *struct {
			Num  *int         `xml:"Num,attr"`
			Item []RecordItem `xml:"Item"`
		} `xml:"RecordList"`
	}
	if err := decoder.DecodeElement(&value, &start); err != nil {
		return err
	}
	*m = MessageRecordInfoResponse{
		XMLName: start.Name, CmdType: value.CmdType, SN: value.SN, DeviceID: value.DeviceID,
	}
	if value.Name != nil {
		m.Name, m.HasName = *value.Name, true
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
	if err := validateRecordInfoResponseStructure(ctx.Request.Body(), g.getDeviceGBProtocolVersion(ctx.DeviceID)); err != nil {
		ctx.String(400, err.Error())
		return
	}

	message.CmdType = strings.TrimSpace(message.CmdType)
	message.DeviceID = strings.TrimSpace(message.DeviceID)
	recordKey, expectedTarget := g.recordResponseTarget(ctx, message)
	if err := g.validateRecordInfoEnvelope(ctx, message, expectedTarget); err != nil {
		ctx.String(400, err.Error())
		return
	}
	extended, err := g.validateAndDecodeAppendixA4(ctx.DeviceID, message.CmdType, ctx.Request.Body())
	if err != nil {
		ctx.String(400, err.Error())
		return
	}
	if recordKey == "" {
		ctx.Log.Warn("ignore unassociated RecordInfo response", "sn", message.SN, "target_id", message.DeviceID)
		ctx.String(200, "OK")
		return
	}
	if err := ctx.RespondString(200, "OK"); err != nil {
		ctx.Log.Error("respond RecordInfo", "err", err, "sn", message.SN, "target_id", message.DeviceID)
		return
	}
	unlockCommit, err := g.lockAdmittedInboundDeviceStateCommit(ctx)
	if err != nil {
		return
	}
	defer unlockCommit()
	if len(extended) > 0 {
		// RecordInfo 查询通常面向通道；运行态扩展应归属于实际录像目标，
		// 持久化仍由下方以父设备编码统一聚合。
		g.storeAppendixA4StateForOwnerLocked(ctx.DeviceID, expectedTarget, extended)
	}
	if g.recordResponses != nil && recordKey != "" {
		g.recordResponses.add(recordKey, message.SumNum, message.Item, func() error {
			return g.appendRecordResponseMetadata(recordKey, ctx.Request.Body(), extended)
		})
	}

	if len(extended) > 0 {
		g.persistAppendixA4Objects(ctx.DeviceID, extended)
	}
}

func (g *GB28181API) startRecordResponseExtra(key string) *recordResponseGeneration {
	generation := &recordResponseGeneration{}
	g.startRecordResponseExtraGeneration(key, generation)
	return generation
}

func (g *GB28181API) startRecordResponseExtraGeneration(key string, generation *recordResponseGeneration) {
	if g == nil || strings.TrimSpace(key) == "" {
		return
	}
	g.recordResponseExtraMu.Lock()
	if g.recordResponseExtra == nil {
		g.recordResponseExtra = make(map[string][]string)
	}
	if g.recordResponseXML == nil {
		g.recordResponseXML = make(map[string][]string)
	}
	if g.recordResponseAppendixA4 == nil {
		g.recordResponseAppendixA4 = make(map[string][]AppendixA4Object)
	}
	if g.recordResponseGenerations == nil {
		g.recordResponseGenerations = make(map[string]*recordResponseGeneration)
	}
	if g.recordResponseMetadata == nil {
		g.recordResponseMetadata = make(map[string]*recordResponseMetadataAccumulator)
	}
	g.recordResponseGenerations[key] = generation
	g.recordResponseExtra[key] = nil
	g.recordResponseXML[key] = nil
	g.recordResponseAppendixA4[key] = nil
	g.recordResponseMetadata[key] = newRecordResponseMetadataAccumulator()
	g.recordResponseExtraMu.Unlock()
}

type recordResponseMetadata struct {
	ExtraInfo   []string
	ResponseXML []string
	AppendixA4  []AppendixA4Object
}

type recordResponseMetadataAccumulator struct {
	bytes        int
	objects      int
	extraSeen    map[string]struct{}
	xmlSeen      map[string]struct{}
	appendixSeen map[string]struct{}
}

func newRecordResponseMetadataAccumulator() *recordResponseMetadataAccumulator {
	return &recordResponseMetadataAccumulator{
		extraSeen:    make(map[string]struct{}),
		xmlSeen:      make(map[string]struct{}),
		appendixSeen: make(map[string]struct{}),
	}
}

func recordResponseAliasDetails(value any) (string, *recordResponseGeneration, bool) {
	switch alias := value.(type) {
	case *recordResponseAlias:
		if alias == nil || strings.TrimSpace(alias.recordKey) == "" {
			return "", nil, false
		}
		return alias.recordKey, alias.generation, true
	case string:
		// 兼容旧测试和升级期间进程内构造的内部值；生产查询使用带代次身份的指针。
		if strings.TrimSpace(alias) == "" {
			return "", nil, false
		}
		return alias, nil, true
	default:
		return "", nil, false
	}
}

func recordResponseAppendixA4Identity(value AppendixA4Object) string {
	return value.Type + "\x00" + value.CmdType + "\x00" + value.Path + "\x00" + value.RawXML
}

func recordResponseAppendixA4Bytes(value AppendixA4Object) int {
	size := len(value.Type) + len(value.CmdType) + len(value.Path) + len(value.RawXML)
	for key, field := range value.Fields {
		size += len(key) + len(field)
	}
	return size
}

func buildRecordResponseMetadataAccumulator(extra, responseXML []string, appendixA4 []AppendixA4Object) *recordResponseMetadataAccumulator {
	state := newRecordResponseMetadataAccumulator()
	for _, value := range extra {
		if _, exists := state.extraSeen[value]; exists {
			continue
		}
		state.extraSeen[value] = struct{}{}
		state.bytes += len(value)
		state.objects++
	}
	for _, value := range responseXML {
		if _, exists := state.xmlSeen[value]; exists {
			continue
		}
		state.xmlSeen[value] = struct{}{}
		state.bytes += len(value)
		state.objects++
	}
	for _, value := range appendixA4 {
		identity := recordResponseAppendixA4Identity(value)
		if _, exists := state.appendixSeen[identity]; exists {
			continue
		}
		state.appendixSeen[identity] = struct{}{}
		state.bytes += recordResponseAppendixA4Bytes(value)
		state.objects++
	}
	return state
}

func (g *GB28181API) appendRecordResponseMetadata(key string, body []byte, appendixA4 []AppendixA4Object) error {
	if g == nil || strings.TrimSpace(key) == "" {
		return nil
	}
	g.recordResponseExtraMu.Lock()
	defer g.recordResponseExtraMu.Unlock()
	if g.recordResponseExtra == nil {
		g.recordResponseExtra = make(map[string][]string)
	}
	if g.recordResponseXML == nil {
		g.recordResponseXML = make(map[string][]string)
	}
	if g.recordResponseAppendixA4 == nil {
		g.recordResponseAppendixA4 = make(map[string][]AppendixA4Object)
	}
	if g.recordResponseMetadata == nil {
		g.recordResponseMetadata = make(map[string]*recordResponseMetadataAccumulator)
	}
	state := g.recordResponseMetadata[key]
	if state == nil {
		state = buildRecordResponseMetadataAccumulator(
			g.recordResponseExtra[key], g.recordResponseXML[key], g.recordResponseAppendixA4[key],
		)
		g.recordResponseMetadata[key] = state
	}

	extraValues := make([]string, 0, len(appendixA4))
	localExtraSeen := make(map[string]struct{}, len(appendixA4))
	for _, value := range appendixA4ExtraInfoValues(appendixA4) {
		if _, exists := state.extraSeen[value]; exists {
			continue
		}
		if _, exists := localExtraSeen[value]; exists {
			continue
		}
		localExtraSeen[value] = struct{}{}
		extraValues = append(extraValues, value)
	}

	var responseXML string
	if len(body) > 0 {
		candidate := string(body)
		if _, exists := state.xmlSeen[candidate]; !exists {
			responseXML = candidate
		}
	}

	appendixValues := make([]AppendixA4Object, 0, len(appendixA4))
	appendixIdentities := make([]string, 0, len(appendixA4))
	localAppendixSeen := make(map[string]struct{}, len(appendixA4))
	for _, value := range appendixA4 {
		identity := recordResponseAppendixA4Identity(value)
		if _, exists := state.appendixSeen[identity]; exists {
			continue
		}
		if _, exists := localAppendixSeen[identity]; exists {
			continue
		}
		localAppendixSeen[identity] = struct{}{}
		appendixValues = append(appendixValues, value)
		appendixIdentities = append(appendixIdentities, identity)
	}

	addedBytes := len(responseXML)
	for _, value := range extraValues {
		addedBytes += len(value)
	}
	for _, value := range appendixValues {
		addedBytes += recordResponseAppendixA4Bytes(value)
	}
	addedObjects := len(extraValues) + len(appendixValues)
	if responseXML != "" {
		addedObjects++
	}
	if addedBytes > gbRecordResponseMaxMetadataBytes-state.bytes ||
		addedObjects > gbRecordResponseMaxMetadataObjects-state.objects {
		return fmt.Errorf(
			"%w: retained %d bytes/%d objects, adding %d bytes/%d objects, limits %d bytes/%d objects",
			errRecordResponseMetadataLimit, state.bytes, state.objects, addedBytes, addedObjects,
			gbRecordResponseMaxMetadataBytes, gbRecordResponseMaxMetadataObjects,
		)
	}

	for _, value := range extraValues {
		state.extraSeen[value] = struct{}{}
		g.recordResponseExtra[key] = append(g.recordResponseExtra[key], value)
	}
	if responseXML != "" {
		state.xmlSeen[responseXML] = struct{}{}
		g.recordResponseXML[key] = append(g.recordResponseXML[key], responseXML)
	}
	if len(appendixValues) > 0 {
		cloned := cloneAppendixA4Objects(appendixValues)
		for index, value := range cloned {
			state.appendixSeen[appendixIdentities[index]] = struct{}{}
			g.recordResponseAppendixA4[key] = append(g.recordResponseAppendixA4[key], value)
		}
	}
	state.bytes += addedBytes
	state.objects += addedObjects
	return nil
}

func (g *GB28181API) takeRecordResponseMetadata(key string, generation *recordResponseGeneration) recordResponseMetadata {
	if g == nil || strings.TrimSpace(key) == "" {
		return recordResponseMetadata{}
	}
	g.recordResponseExtraMu.Lock()
	if current := g.recordResponseGenerations[key]; current != generation {
		g.recordResponseExtraMu.Unlock()
		return recordResponseMetadata{}
	}
	metadata := recordResponseMetadata{
		ExtraInfo:   append([]string(nil), g.recordResponseExtra[key]...),
		ResponseXML: append([]string(nil), g.recordResponseXML[key]...),
		AppendixA4:  cloneAppendixA4Objects(g.recordResponseAppendixA4[key]),
	}
	delete(g.recordResponseExtra, key)
	delete(g.recordResponseXML, key)
	delete(g.recordResponseAppendixA4, key)
	delete(g.recordResponseGenerations, key)
	delete(g.recordResponseMetadata, key)
	g.recordResponseExtraMu.Unlock()
	return metadata
}

func (g *GB28181API) clearRecordResponseExtraGeneration(key string, generation *recordResponseGeneration) {
	if g == nil || strings.TrimSpace(key) == "" {
		return
	}
	g.recordResponseExtraMu.Lock()
	if current := g.recordResponseGenerations[key]; current == generation {
		delete(g.recordResponseExtra, key)
		delete(g.recordResponseXML, key)
		delete(g.recordResponseAppendixA4, key)
		delete(g.recordResponseGenerations, key)
		delete(g.recordResponseMetadata, key)
	}
	g.recordResponseExtraMu.Unlock()
}

func (g *GB28181API) clearRecordResponseExtra(key string) {
	if g == nil || strings.TrimSpace(key) == "" {
		return
	}
	g.recordResponseExtraMu.Lock()
	delete(g.recordResponseExtra, key)
	delete(g.recordResponseXML, key)
	delete(g.recordResponseAppendixA4, key)
	delete(g.recordResponseGenerations, key)
	delete(g.recordResponseMetadata, key)
	g.recordResponseExtraMu.Unlock()
}

func (g *GB28181API) clearAllRecordResponseExtra() {
	if g == nil {
		return
	}
	g.recordResponseExtraMu.Lock()
	g.recordResponseExtra = make(map[string][]string)
	g.recordResponseXML = make(map[string][]string)
	g.recordResponseAppendixA4 = make(map[string][]AppendixA4Object)
	g.recordResponseGenerations = make(map[string]*recordResponseGeneration)
	g.recordResponseMetadata = make(map[string]*recordResponseMetadataAccumulator)
	g.recordResponseExtraMu.Unlock()
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
	recordKey, _, ok := recordResponseAliasDetails(value)
	if !ok || !g.recordResponses.Has(recordKey) {
		return "", ""
	}
	separator := strings.IndexByte(recordKey, ':')
	if separator <= 0 {
		return "", ""
	}
	return recordKey, recordKey[:separator]
}

func (g *GB28181API) recordResponseRequiresLocation(ctx *sip.Context, message *MessageRecordInfoResponse, expectedTarget string) bool {
	if g == nil || g.recordResponses == nil || ctx == nil || message == nil || expectedTarget == "" {
		return false
	}
	aliasKey := buildMultiResponseKey(strings.TrimSpace(ctx.DeviceID), "RecordInfo", message.SN)
	value, ok := g.recordResponseAliases.Load(aliasKey)
	if !ok {
		return false
	}
	alias, ok := value.(*recordResponseAlias)
	if !ok || alias == nil || !alias.requireRecordLocation {
		return false
	}
	recordKey := buildMultiResponseKey(expectedTarget, "RecordInfo", message.SN)
	return alias.recordKey == recordKey && g.recordResponses.Has(recordKey)
}

func (g *GB28181API) validateRecordInfoEnvelope(ctx *sip.Context, message *MessageRecordInfoResponse, expectedTarget string) error {
	if message == nil || message.XMLName.Local != "Response" || !strings.EqualFold(message.CmdType, "RecordInfo") || message.SN <= 0 {
		return fmt.Errorf("invalid RecordInfo envelope")
	}
	if !isGBDeviceIdentifier(message.DeviceID) || !message.HasName || !message.HasSumNum || message.SumNum < 0 {
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
	} else if message.DeviceID != expectedTarget && (ctx == nil || message.DeviceID != strings.TrimSpace(ctx.DeviceID)) {
		// 部分 NVR 会在顶层返回父设备编码，但不能让同一 NVR 的兄弟通道
		// 复用查询 SN，提前完成另一个通道的 RecordInfo 等待。
		return fmt.Errorf("RecordInfo response target mismatch")
	}
	version := g.getDeviceGBProtocolVersion(ctx.DeviceID)
	requireRecordLocation := version == GBVersion30 && g.recordResponseRequiresLocation(ctx, message, expectedTarget)
	for index := range message.Item {
		item := &message.Item[index]
		item.DeviceID = strings.TrimSpace(item.DeviceID)
		item.StartTime = strings.TrimSpace(item.StartTime)
		item.EndTime = strings.TrimSpace(item.EndTime)
		item.Type = strings.ToLower(strings.TrimSpace(item.Type))
		item.RecordLocation = strings.TrimSpace(item.RecordLocation)
		if item.DeviceID != expectedTarget {
			return fmt.Errorf("RecordInfo item target mismatch")
		}
		if !item.HasName || !item.HasSecrecy || item.Secrecy < 0 || item.Secrecy > 1 {
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
			if requireRecordLocation && !item.HasRecordLocation {
				return fmt.Errorf("RecordInfo storage location is required for indistinct query")
			}
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
		// 2014 补充文件明确要求文件目录响应项携带具体录像类型，不能返回 all。
		return version == GBVersion10
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
