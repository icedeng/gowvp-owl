package gbs

import (
	"encoding/json"
	"encoding/xml"
	"sort"
	"strings"
	"time"
)

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
	"detectorType":                         {},
	"pmsHostType":                          {},
	"capCameraType":                        {},
	"barrierType":                          {},
	"pmsVehInOutInfoType":                  {},
	"dmsHostType":                          {},
	"doorType":                             {},
	"readerType":                           {},
	"doorEventType":                        {},
	"remoteControlDoorEventType":           {},
	"doorOpenType":                         {},
	"alarmType":                            {},
	"doorControlType":                      {},
	"personType":                           {},
	"verifyModeType":                       {},
	"credentialType":                       {},
	"securityDetectDeviceType":             {},
	"dangerousGoodsValueType":              {},
	"rectType":                             {},
	"dangerousInfoType":                    {},
	"metalDetectionInfoType":               {},
	"holographicDetectionInfoType":         {},
	"holographicDetectionEventType":        {},
	"visiblePackageEventType":              {},
	"xrayPackageEventType":                 {},
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
	text := strings.TrimSpace(collectNodeText(node))
	if text == "" {
		return AppendixA4Object{}, false
	}
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
	out := map[string]string{}
	var kv map[string]any
	if err := json.Unmarshal([]byte(text), &kv); err != nil {
		return nil
	}
	for k, v := range kv {
		switch t := v.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				out[k] = strings.TrimSpace(t)
			}
		case float64:
			out[k] = strings.TrimSpace(strings.TrimRight(strings.TrimRight(formatFloat(t), "0"), "."))
		case bool:
			if t {
				out[k] = "true"
			} else {
				out[k] = "false"
			}
		}
	}
	return out
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
		v := strings.TrimSpace(attr.Value)
		if k != "" && v != "" {
			fields["@"+k] = v
		}
	}
	for _, child := range node.Children {
		key := strings.TrimSpace(child.XMLName.Local)
		val := strings.TrimSpace(collectNodeText(child))
		if key == "" || val == "" {
			continue
		}
		if old, ok := fields[key]; ok && old != "" && old != val {
			fields[key] = old + "," + val
			continue
		}
		fields[key] = val
	}
	if len(fields) == 0 {
		if v := strings.TrimSpace(collectNodeText(node)); v != "" {
			fields["value"] = v
		}
	}
	return fields
}

func collectNodeText(node a4XMLNode) string {
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
	uniq := make(map[string]AppendixA4Object, len(in))
	for _, obj := range in {
		key := appendixA4ObjectKey(obj)
		uniq[key] = obj
	}
	out := make([]AppendixA4Object, 0, len(uniq))
	for _, obj := range uniq {
		out = append(out, obj)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt == out[j].UpdatedAt {
			return out[i].Type < out[j].Type
		}
		return out[i].UpdatedAt > out[j].UpdatedAt
	})
	return out
}

func appendixA4ObjectKey(obj AppendixA4Object) string {
	return strings.Join([]string{
		strings.TrimSpace(obj.Type),
		strings.TrimSpace(obj.CmdType),
		strings.TrimSpace(obj.Path),
		canonicalFields(obj.Fields),
	}, "|")
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
		parts = append(parts, k+"="+strings.TrimSpace(fields[k]))
	}
	return strings.Join(parts, ";")
}

func formatFloat(v float64) string {
	return strings.TrimSpace(strings.TrimRight(strings.TrimRight(jsonNumber(v), "0"), "."))
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
