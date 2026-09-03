package gbs

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func (g *GB28181API) validateAndDecodeAppendixA4(deviceID, cmdType string, body []byte) ([]AppendixA4Object, error) {
	present, err := inspectAppendixA4Payload(body)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	objects := g.decodeAppendixA4Objects(cmdType, body)
	version := g.getDeviceGBProtocolVersion(deviceID)
	if !version.AtLeast(GBVersion30) {
		return nil, fmt.Errorf("Appendix A.4 requires %s", GBVersion30.StandardName())
	}
	return objects, nil
}

func inspectAppendixA4Payload(body []byte) (bool, error) {
	if len(body) == 0 {
		return false, nil
	}
	var root a4XMLNode
	if err := xml.Unmarshal(body, &root); err != nil {
		return false, nil
	}
	return inspectAppendixA4Node(root)
}

func inspectAppendixA4Node(node a4XMLNode) (bool, error) {
	name := strings.TrimSpace(node.XMLName.Local)
	present := isAppendixA4Type(name)
	if strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") {
		present = true
		if len(node.Children) > 0 {
			return true, fmt.Errorf("Appendix A.4 %s must contain simple text", name)
		}
		if utf8.RuneCountInString(node.Content) > 1024 {
			return true, fmt.Errorf("Appendix A.4 %s exceeds 1024 characters", name)
		}
	}
	for _, child := range node.Children {
		childPresent, err := inspectAppendixA4Node(child)
		if err != nil {
			return true, err
		}
		present = present || childPresent
	}
	return present, nil
}

func appendixA4ExtraInfoValues(objects []AppendixA4Object) []string {
	values := make([]string, 0, len(objects))
	seen := make(map[string]struct{}, len(objects))
	for _, object := range objects {
		path := strings.TrimSpace(object.Path)
		if !strings.HasSuffix(strings.ToLower(path), "/extrainfo") && !strings.HasSuffix(strings.ToLower(path), "/extralinfo") {
			continue
		}
		value := object.Fields["value"]
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	return values
}

// AppendixA4Object 是附录 A.4 扩展对象的协议层模型。
// 说明：
// 1. Type 对应附录 A.4 扩展对象名称；
// 2. Fields 为首层结构化字段，RawXML 保留厂商私有字段。
type AppendixA4Object struct {
	Type      string            `json:"type"`
	CmdType   string            `json:"cmd_type,omitempty"`
	Path      string            `json:"path,omitempty"`
	Fields    map[string]string `json:"fields,omitempty"`
	RawXML    string            `json:"raw_xml,omitempty"`
	UpdatedAt int64             `json:"updated_at,omitempty"`
}

type a4XMLNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr  `xml:",any,attr"`
	Content  string      `xml:",chardata"`
	Children []a4XMLNode `xml:",any"`
}

var appendixA4TypeSet = map[string]struct{}{
	"detectorType":                  {},
	"pmsHostType":                   {},
	"capCameraType":                 {},
	"barrierType":                   {},
	"pmsVehInOutInfoType":           {},
	"dmsHostType":                   {},
	"doorType":                      {},
	"readerType":                    {},
	"doorEventType":                 {},
	"remoteControlDoorEventType":    {},
	"doorOpenType":                  {},
	"alarmType":                     {},
	"doorControlType":               {},
	"personType":                    {},
	"verifyModeType":                {},
	"credentialType":                {},
	"securityDetectDeviceType":      {},
	"dangerousGoodsValueType":       {},
	"rectType":                      {},
	"dangerousInfoType":             {},
	"metalDetectionInfoType":        {},
	"holographicDetectionInfoType":  {},
	"holographicDetectionEventType": {},
	"visiblePackageEventType":       {},
	"xrayPackageEventType":          {},
	"behavioralEventType":           {},
	// 兼容早期厂商使用的非标准别名。
	"behaviorAlertEventType":               {},
	"openCheckEventType":                   {},
	"metalDetectionEventType":              {},
	"liquidDetectionEventType":             {},
	"explosivesAndDrugsDetectionEventType": {},
}

func (g *GB28181API) decodeAppendixA4Objects(cmdType string, body []byte) []AppendixA4Object {
	if len(body) == 0 {
		return nil
	}
	var root a4XMLNode
	if err := xml.Unmarshal(body, &root); err != nil {
		return nil
	}
	cmdType = strings.TrimSpace(cmdType)
	out := make([]AppendixA4Object, 0, 4)
	g.walkAppendixA4Node(&out, root, "", cmdType)
	if len(out) == 0 {
		return nil
	}
	return dedupeAppendixA4Objects(out)
}

func (g *GB28181API) walkAppendixA4Node(out *[]AppendixA4Object, node a4XMLNode, parentPath, cmdType string) {
	name := strings.TrimSpace(node.XMLName.Local)
	if name == "" {
		return
	}
	path := "/" + name
	if parentPath != "" {
		path = parentPath + "/" + name
	}
	if isAppendixA4Type(name) {
		obj := g.buildAppendixA4Object(name, cmdType, path, node)
		*out = append(*out, obj)
	}
	// 兼容扩展信息字段。
	if strings.EqualFold(name, "ExtraInfo") || strings.EqualFold(name, "ExtralInfo") {
		if obj, ok := g.buildAppendixA4FromExtraInfo(cmdType, path, node); ok {
			*out = append(*out, obj)
		}
	}
	for _, child := range node.Children {
		g.walkAppendixA4Node(out, child, path, cmdType)
	}
}

func isAppendixA4Type(name string) bool {
	_, ok := appendixA4TypeSet[strings.TrimSpace(name)]
	return ok
}

func (g *GB28181API) buildAppendixA4Object(name, cmdType, path string, node a4XMLNode) AppendixA4Object {
	fields := extractNodeFields(node)
	raw := marshalNodeXML(node)
	return AppendixA4Object{
		Type:      strings.TrimSpace(name),
		CmdType:   strings.TrimSpace(cmdType),
		Path:      strings.TrimSpace(path),
		Fields:    fields,
		RawXML:    raw,
		UpdatedAt: time.Now().Unix(),
	}
}

func (g *GB28181API) buildAppendixA4FromExtraInfo(cmdType, path string, node a4XMLNode) (AppendixA4Object, bool) {
	text := node.Content
	// ExtraInfo 是普通 string，Schema 只限制最大长度；即使值为空或全空白，
	// 元素本身也携带有效的多值位置信息，必须进入统一快照和 RecordInfo 元数据。
	fields := map[string]string{"value": text}
	for k, v := range parseExtraInfoJSON(text) {
		fields[k] = v
	}
	name := inferAppendixA4Type(text, fields)
	if name == "" {
		name = "ExtraInfo"
	}
	return AppendixA4Object{
		Type:      name,
		CmdType:   strings.TrimSpace(cmdType),
		Path:      strings.TrimSpace(path),
		Fields:    fields,
		RawXML:    marshalNodeXML(node),
		UpdatedAt: time.Now().Unix(),
	}, true
}

func parseExtraInfoJSON(text string) map[string]string {
	text = strings.TrimSpace(text)
	if text == "" || (!strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[")) {
		return nil
	}
	value, ok := decodeExtraInfoJSON(text)
	if !ok {
		return nil
	}
	out := map[string]string{}
	flattenExtraInfoJSON(out, "", value)
	if len(out) == 0 {
		return nil
	}
	return out
}

func decodeExtraInfoJSON(text string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}
	// 只接受一个完整 JSON 值，避免把尾随内容静默吞掉。
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, false
	}
	return value, true
}

func flattenExtraInfoJSON(out map[string]string, path string, value any) {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			childPath := key
			if path != "" {
				childPath = path + "." + key
			}
			flattenExtraInfoJSON(out, childPath, child)
		}
	case []any:
		for index, child := range item {
			childPath := path + "[" + strconv.Itoa(index) + "]"
			flattenExtraInfoJSON(out, childPath, child)
		}
	case string:
		if trimmed := strings.TrimSpace(item); path != "" && trimmed != "" {
			out[path] = trimmed
		}
	case json.Number:
		if path != "" {
			out[path] = item.String()
		}
	case bool:
		if path != "" {
			if item {
				out[path] = "true"
			} else {
				out[path] = "false"
			}
		}
	}
}

func inferAppendixA4Type(text string, fields map[string]string) string {
	// 1. 先尝试从结构化字段中识别。
	for _, key := range []string{"type", "object_type", "event_type"} {
		if v := strings.TrimSpace(fields[key]); isAppendixA4Type(v) {
			return v
		}
	}
	// 2. 再从纯文本中匹配已知类型。
	for name := range appendixA4TypeSet {
		if strings.Contains(text, name) {
			return name
		}
	}
	return ""
}

func extractNodeFields(node a4XMLNode) map[string]string {
	fields := map[string]string{}
	for _, attr := range node.Attrs {
		k := strings.TrimSpace(attr.Name.Local)
		if k != "" {
			fields["@"+k] = attr.Value
		}
	}
	for _, child := range node.Children {
		key := strings.TrimSpace(child.XMLName.Local)
		if key == "" {
			continue
		}
		val := collectNodeText(child)
		if old, ok := fields[key]; ok && old != val {
			fields[key] = old + "," + val
			continue
		}
		fields[key] = val
	}
	if len(fields) == 0 {
		fields["value"] = collectNodeText(node)
	}
	return fields
}

func collectNodeText(node a4XMLNode) string {
	if len(node.Children) == 0 {
		// XML Schema string 默认 whiteSpace=preserve；简单字段应区分缺省、空值、
		// 全空白和仅首尾空白不同的内容。
		return node.Content
	}
	parts := make([]string, 0, len(node.Children)+1)
	if v := strings.TrimSpace(node.Content); v != "" {
		parts = append(parts, v)
	}
	for _, child := range node.Children {
		if v := strings.TrimSpace(collectNodeText(child)); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func marshalNodeXML(node a4XMLNode) string {
	b, err := xml.Marshal(node)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func dedupeAppendixA4Objects(in []AppendixA4Object) []AppendixA4Object {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]AppendixA4Object, 0, len(in))
	for _, obj := range in {
		key := appendixA4ObjectKey(obj)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, obj)
	}
	return out
}

func appendixA4ObjectKey(obj AppendixA4Object) string {
	path := strings.TrimSpace(obj.Path)
	if strings.HasSuffix(strings.ToLower(path), "/extrainfo") || strings.HasSuffix(strings.ToLower(path), "/extralinfo") {
		// ExtraInfo 是有序多值普通 string。去重只能比较原始值，不能把空字符串、
		// 全空白或仅首尾空白不同的值折叠为同一对象。
		return strings.Join([]string{strings.TrimSpace(obj.Type), strings.TrimSpace(obj.CmdType), path, obj.Fields["value"]}, "\x00")
	}
	raw := obj.RawXML
	if raw == "" {
		raw = canonicalFields(obj.Fields)
	}
	return strings.Join([]string{
		strings.TrimSpace(obj.Type),
		strings.TrimSpace(obj.CmdType),
		path,
		raw,
	}, "\x00")
}

func canonicalFields(fields map[string]string) string {
	if len(fields) == 0 {
		return ""
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"\x00"+fields[k])
	}
	return strings.Join(parts, "\x00")
}
