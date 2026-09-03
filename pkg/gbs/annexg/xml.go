package annexg

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const (
	annexGNamespace = "http://www.w3.org/namespace/"
	maximumXMLBytes = 1 << 20
	maximumXMLDepth = 32
	maximumXMLNodes = 100000
)

type xmlNode struct {
	name     xml.Name
	attrs    []xml.Attr
	text     strings.Builder
	children []*xmlNode
}

type elementSpec struct {
	name     string
	minimum  int
	maximum  int
	children []elementSpec
}

// Decode 严格解析并按协议版本校验一条附录 G 消息。
func Decode(version Version, data []byte) (Message, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	root, command, err := classifyEnvelope(data)
	if err != nil {
		return nil, err
	}
	schema, err := schemaFor(root.name.Local, command)
	if err != nil {
		return nil, err
	}
	if err := validateTree(root, schema, true, root.name.Space); err != nil {
		return nil, err
	}
	if err := validateScalarLexemes(root, command); err != nil {
		return nil, err
	}

	message, err := newMessage(root.name.Local, command)
	if err != nil {
		return nil, err
	}
	if err := sip.NewGBXMLDecoder(data).Decode(message); err != nil {
		return nil, fmt.Errorf("decode Annex G XML: %w", err)
	}
	normalizeEnvelope(message)
	if err := message.Validate(version); err != nil {
		return nil, err
	}
	return message, nil
}

// ClassifyEnvelope 严格识别附录 G 根元素和直接子 CmdType，供传输层在认证前选择身份域。
// 这里只校验无歧义包络；完整字段、顺序和值域仍由 Decode 按协商版本校验。
func ClassifyEnvelope(data []byte) (string, Command, error) {
	root, command, err := classifyEnvelope(data)
	if err != nil {
		return "", "", err
	}
	return root.name.Local, command, nil
}

func classifyEnvelope(data []byte) (*xmlNode, Command, error) {
	if len(data) > maximumXMLBytes {
		return nil, "", fmt.Errorf("Annex G XML exceeds %d byte limit", maximumXMLBytes)
	}
	root, err := parseXMLTree(data)
	if err != nil {
		return nil, "", err
	}
	if err := validateRootNamespace(root); err != nil {
		return nil, "", err
	}
	commandNode, commandText, err := requiredSimpleChildNode(root, "CmdType")
	if err != nil {
		return nil, "", err
	}
	if commandNode.name.Space != root.name.Space {
		return nil, "", errors.New("Annex G CmdType changes XML namespace")
	}
	if len(commandNode.attrs) != 0 {
		return nil, "", errors.New("Annex G CmdType must not contain attributes")
	}
	if commandText != strings.TrimSpace(commandText) {
		return nil, "", errors.New("Annex G CmdType must match its fixed value exactly")
	}
	command := Command(commandText)
	if _, err := schemaFor(root.name.Local, command); err != nil {
		return nil, "", err
	}
	return root, command, nil
}

// Encode 校验消息后生成 UTF-8 编码的附录 G XML。
func Encode(version Version, message Message) ([]byte, error) {
	if message == nil {
		return nil, errors.New("nil Annex G message")
	}
	if err := message.Validate(version); err != nil {
		return nil, err
	}
	body, err := xml.MarshalIndent(message, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode Annex G XML: %w", err)
	}
	encoded := append([]byte(xml.Header), body...)
	if _, err := Decode(version, encoded); err != nil {
		return nil, fmt.Errorf("validate encoded Annex G XML: %w", err)
	}
	return encoded, nil
}

func parseXMLTree(data []byte) (*xmlNode, error) {
	decoder := sip.NewGBXMLDecoder(data)
	decoder.Strict = true
	var root *xmlNode
	stack := make([]*xmlNode, 0, 8)
	xmlDeclarationSeen := false
	nodeCount := 0
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode Annex G XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if len(stack)+1 > maximumXMLDepth {
				return nil, fmt.Errorf("Annex G XML exceeds maximum depth %d", maximumXMLDepth)
			}
			nodeCount++
			if nodeCount > maximumXMLNodes {
				return nil, fmt.Errorf("Annex G XML exceeds maximum node count %d", maximumXMLNodes)
			}
			node := &xmlNode{name: value.Name, attrs: value.Attr}
			if len(stack) == 0 {
				if root != nil {
					return nil, errors.New("Annex G XML contains multiple root elements")
				}
				root = node
			} else {
				parent := stack[len(stack)-1]
				parent.children = append(parent.children, node)
			}
			stack = append(stack, node)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("Annex G XML contains an unmatched end element")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			if len(stack) == 0 {
				if strings.TrimSpace(string(value)) != "" {
					return nil, errors.New("Annex G XML contains text outside the root element")
				}
				continue
			}
			stack[len(stack)-1].text.Write(value)
		case xml.Comment:
			// 注释不参与协议结构校验。
		case xml.ProcInst:
			if root != nil || !strings.EqualFold(value.Target, "xml") || xmlDeclarationSeen {
				return nil, errors.New("Annex G XML contains an unsupported processing instruction")
			}
			xmlDeclarationSeen = true
		case xml.Directive:
			return nil, errors.New("Annex G XML directives are not allowed")
		}
	}
	if root == nil {
		return nil, errors.New("empty Annex G XML")
	}
	if len(stack) != 0 {
		return nil, errors.New("incomplete Annex G XML")
	}
	return root, nil
}

func requiredSimpleChild(root *xmlNode, name string) (string, error) {
	_, value, err := requiredSimpleChildNode(root, name)
	return value, err
}

func requiredSimpleChildNode(root *xmlNode, name string) (*xmlNode, string, error) {
	var matched *xmlNode
	for _, child := range root.children {
		if child.name.Local != name {
			continue
		}
		if matched != nil {
			return nil, "", fmt.Errorf("duplicate Annex G %s", name)
		}
		matched = child
	}
	if matched == nil {
		return nil, "", fmt.Errorf("missing Annex G %s", name)
	}
	if len(matched.children) != 0 {
		return nil, "", fmt.Errorf("Annex G %s must contain simple text", name)
	}
	return matched, matched.text.String(), nil
}

func validateRootNamespace(root *xmlNode) error {
	if root.name.Space != "" && root.name.Space != annexGNamespace {
		return fmt.Errorf("unsupported Annex G XML namespace %q", root.name.Space)
	}
	for _, attr := range root.attrs {
		isNamespaceDeclaration := attr.Name.Local == "xmlns" || attr.Name.Space == "xmlns"
		if !isNamespaceDeclaration || attr.Value != annexGNamespace {
			return fmt.Errorf("unexpected Annex G root attribute %s", attr.Name.Local)
		}
	}
	return nil
}

func validateScalarLexemes(root *xmlNode, command Command) error {
	if root.name.Local != "Notify" || command != CommandConfigDefence {
		return nil
	}
	value, err := requiredSimpleChild(root, "Type")
	if err != nil {
		return err
	}
	value = strings.TrimSpace(value)
	switch value {
	case "true", "false", "1", "0":
		return nil
	default:
		return fmt.Errorf("invalid Annex G ConfigDefence Type %q", value)
	}
}

func validateTree(node *xmlNode, spec elementSpec, root bool, namespace string) error {
	if node.name.Local != spec.name {
		return fmt.Errorf("unexpected Annex G element %s, want %s", node.name.Local, spec.name)
	}
	if root {
		if err := validateRootNamespace(node); err != nil {
			return err
		}
	} else {
		if node.name.Space != namespace {
			return fmt.Errorf("Annex G element %s changes XML namespace", node.name.Local)
		}
		if len(node.attrs) != 0 {
			return fmt.Errorf("unexpected Annex G attribute on %s", node.name.Local)
		}
	}
	if len(spec.children) == 0 {
		if len(node.children) != 0 {
			return fmt.Errorf("Annex G element %s must contain simple text", node.name.Local)
		}
		return nil
	}
	if strings.TrimSpace(node.text.String()) != "" {
		return fmt.Errorf("Annex G element %s contains mixed text", node.name.Local)
	}

	counts := make([]int, len(spec.children))
	position := 0
	for _, child := range node.children {
		matched := -1
		for candidate := position; candidate < len(spec.children); candidate++ {
			field := spec.children[candidate]
			if field.maximum >= 0 && counts[candidate] >= field.maximum {
				continue
			}
			if child.name.Local == field.name {
				matched = candidate
				break
			}
			if counts[candidate] < field.minimum {
				break
			}
		}
		if matched < 0 {
			return fmt.Errorf("unexpected or out-of-order Annex G element %s in %s", child.name.Local, node.name.Local)
		}
		for skipped := position; skipped < matched; skipped++ {
			if counts[skipped] < spec.children[skipped].minimum {
				return fmt.Errorf("missing Annex G element %s in %s", spec.children[skipped].name, node.name.Local)
			}
		}
		counts[matched]++
		position = matched
		if err := validateTree(child, spec.children[matched], false, namespace); err != nil {
			return err
		}
	}
	for index, field := range spec.children {
		if counts[index] < field.minimum {
			return fmt.Errorf("missing Annex G element %s in %s", field.name, node.name.Local)
		}
	}
	return nil
}

func simple(name string, minimum, maximum int) elementSpec {
	return elementSpec{name: name, minimum: minimum, maximum: maximum}
}

func complex(name string, minimum, maximum int, children ...elementSpec) elementSpec {
	return elementSpec{name: name, minimum: minimum, maximum: maximum, children: children}
}

func schemaFor(root string, command Command) (elementSpec, error) {
	switch root {
	case "Notify":
		switch command {
		case CommandMPAlarm:
			return complex("Notify", 1, 1, simple("CmdType", 1, 1), simple("SN", 1, 1), complex("AlarmContent", 1, 1, mpRecordSchema()...)), nil
		case CommandECSAlarm:
			return complex("Notify", 1, 1, simple("CmdType", 1, 1), simple("SN", 1, 1), complex("AlarmContent", 1, 1, ecsRecordSchema()...)), nil
		case CommandTGSAlarm:
			return complex("Notify", 1, 1, simple("CmdType", 1, 1), simple("SN", 1, 1), complex("AlarmContent", 1, 1, tgsRecordSchema()...)), nil
		case CommandConfigDefence:
			return complex("Notify", 1, 1,
				simple("CmdType", 1, 1), simple("SN", 1, 1), simple("Type", 1, 1),
				simple("TollgateID", 1, 1), simple("CarPlate", 1, 1), simple("PlateType", 1, 1),
				simple("DefenceType", 1, 1), simple("DefenceTime", 1, 1),
			), nil
		}
	case "Query":
		base := []elementSpec{simple("CmdType", 1, 1), simple("SN", 1, 1), simple("BeginTime", 0, 1), simple("EndTime", 0, 1)}
		switch command {
		case CommandMPAlarmRecordList:
			return complex("Query", 1, 1, append(base,
				simple("AlarmClass", 0, 1), simple("AlarmDeviceRange", 0, 1), simple("AlarmPriority", 0, 1), simple("AlarmMethod", 0, 1),
			)...), nil
		case CommandECSAlarmRecordList:
			return complex("Query", 1, 1, append(base,
				simple("AlarmAddressRange", 0, 1), simple("AlarmPriority", 0, 1), simple("AlarmMethod", 0, 1), simple("AlarmClass", 0, 1),
			)...), nil
		case CommandTGSAlarmRecordList:
			return complex("Query", 1, 1, append(base,
				simple("TollgateID", 0, 1), simple("CarPlate", 0, 1), simple("PlateType", 0, 1),
			)...), nil
		}
	case "Response":
		switch command {
		case CommandMPAlarm, CommandECSAlarm, CommandTGSAlarm:
			return complex("Response", 1, 1, simple("CmdType", 1, 1), simple("SN", 1, 1), simple("Result", 1, 1)), nil
		case CommandConfigDefence:
			return complex("Response", 1, 1, simple("CmdType", 1, 1), simple("SN", 1, 1), simple("Result", 1, 1), simple("Info", 0, -1)), nil
		case CommandMPAlarmRecordList:
			return recordResponseSchema(command, mpRecordSchema()), nil
		case CommandECSAlarmRecordList:
			return recordResponseSchema(command, ecsRecordSchema()), nil
		case CommandTGSAlarmRecordList:
			return recordResponseSchema(command, tgsRecordSchema()), nil
		}
	}
	return elementSpec{}, fmt.Errorf("unsupported Annex G root/command %s/%s", root, command)
}

func mpRecordSchema() []elementSpec {
	return []elementSpec{
		simple("AlarmNO", 1, 1), simple("AlarmTime", 1, 1), simple("DeviceID", 1, 1), simple("AlarmClass", 0, 1),
		simple("AlarmPriority", 1, 1), simple("AlarmMethod", 1, 1), simple("Longitude", 0, 1), simple("Latitude", 0, 1),
		simple("AlarmAddress", 0, 1), simple("Address", 0, 1), simple("Name", 0, 1), simple("Sex", 0, 1), simple("Contact", 0, 1),
		simple("CarPlate", 0, -1), simple("PlateType", 0, -1), simple("Victim", 0, -1),
		simple("OriginalNO", 1, 1), simple("OriginalInfo", 1, 1), simple("Sender", 1, 1), simple("Processor", 1, 1),
		simple("AlarmLevel", 1, 1), simple("Disposal", 1, 1), simple("Alarminfo", 1, 1), simple("Info", 0, -1),
	}
}

func ecsRecordSchema() []elementSpec {
	return []elementSpec{
		simple("AlarmNO", 1, 1), simple("AlarmTime", 1, 1), simple("AlarmPriority", 1, 1), simple("AlarmClass", 1, 1),
		simple("AlarmAddress", 1, 1), simple("AlarmMethod", 1, 1), simple("AlarmTelephone", 1, 1),
		simple("Longitude", 0, 1), simple("Latitude", 0, 1), simple("Name", 0, 1), simple("Address", 0, 1), simple("Contact", 0, 1), simple("Sex", 0, 1),
		simple("CarPlate", 0, -1), simple("PlateType", 0, -1), simple("Victim", 0, -1),
		simple("Processor", 1, 1), simple("SrecipientName", 1, 1), simple("NsStatus", 1, 1), simple("NCallType", 1, 1),
		simple("Alarminfo", 1, 1), simple("Info", 0, -1),
	}
}

func tgsRecordSchema() []elementSpec {
	return []elementSpec{
		simple("AlarmTime", 1, 1), simple("TollgateID", 1, 1), simple("CarPlate", 1, 1), simple("PlateType", 1, 1),
		simple("DefenceType", 1, 1), simple("ImageURL", 0, 1), simple("Direction", 0, 1), simple("VehicleSpeed", 0, 1), simple("PassTime", 0, 1),
	}
}

func recordResponseSchema(command Command, record []elementSpec) elementSpec {
	return complex("Response", 1, 1,
		simple("CmdType", 1, 1), simple("SN", 1, 1), simple("Result", 1, 1),
		simple("RealRecordNum", 1, 1), simple("SendRecordNum", 1, 1),
		complex("RecordList", 1, 1, complex("AlarmRecord", 0, -1, record...)),
	)
}

func newMessage(root string, command Command) (Message, error) {
	switch root {
	case "Notify":
		switch command {
		case CommandMPAlarm:
			return &MPAlarmNotify{}, nil
		case CommandECSAlarm:
			return &ECSAlarmNotify{}, nil
		case CommandTGSAlarm:
			return &TGSAlarmNotify{}, nil
		case CommandConfigDefence:
			return &ConfigDefenceNotify{}, nil
		}
	case "Query":
		switch command {
		case CommandMPAlarmRecordList, CommandECSAlarmRecordList, CommandTGSAlarmRecordList:
			return &AlarmRecordQuery{}, nil
		}
	case "Response":
		switch command {
		case CommandMPAlarm, CommandECSAlarm, CommandTGSAlarm, CommandConfigDefence:
			return &NotificationResponse{}, nil
		case CommandMPAlarmRecordList:
			return &MPAlarmRecordListResponse{}, nil
		case CommandECSAlarmRecordList:
			return &ECSAlarmRecordListResponse{}, nil
		case CommandTGSAlarmRecordList:
			return &TGSAlarmRecordListResponse{}, nil
		}
	}
	return nil, fmt.Errorf("unsupported Annex G root/command %s/%s", root, command)
}

func normalizeEnvelope(message Message) {
	switch value := message.(type) {
	case *MPAlarmNotify:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
	case *ECSAlarmNotify:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
	case *TGSAlarmNotify:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
	case *ConfigDefenceNotify:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
	case *AlarmRecordQuery:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
	case *NotificationResponse:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
		value.Result = Result(strings.TrimSpace(string(value.Result)))
	case *MPAlarmRecordListResponse:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
		value.Result = Result(strings.TrimSpace(string(value.Result)))
	case *ECSAlarmRecordListResponse:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
		value.Result = Result(strings.TrimSpace(string(value.Result)))
	case *TGSAlarmRecordListResponse:
		value.CmdType = Command(strings.TrimSpace(string(value.CmdType)))
		value.Result = Result(strings.TrimSpace(string(value.Result)))
	}
}

// MarshalXML 按各查询命令在标准中的字段顺序编码共用查询模型。
func (message AlarmRecordQuery) MarshalXML(encoder *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "Query"}
	if err := encoder.EncodeToken(start); err != nil {
		return err
	}
	encode := func(name string, value any) error {
		return encoder.EncodeElement(value, xml.StartElement{Name: xml.Name{Local: name}})
	}
	if err := encode("CmdType", message.CmdType); err != nil {
		return err
	}
	if err := encode("SN", message.SN); err != nil {
		return err
	}
	if err := encodeOptional(encode, "BeginTime", message.BeginTime); err != nil {
		return err
	}
	if err := encodeOptional(encode, "EndTime", message.EndTime); err != nil {
		return err
	}
	var fields []struct {
		name  string
		value *string
	}
	switch message.CmdType {
	case CommandMPAlarmRecordList:
		fields = append(fields,
			struct {
				name  string
				value *string
			}{"AlarmClass", message.AlarmClass},
			struct {
				name  string
				value *string
			}{"AlarmDeviceRange", message.AlarmDeviceRange},
			struct {
				name  string
				value *string
			}{"AlarmPriority", message.AlarmPriority},
			struct {
				name  string
				value *string
			}{"AlarmMethod", message.AlarmMethod},
		)
	case CommandECSAlarmRecordList:
		fields = append(fields,
			struct {
				name  string
				value *string
			}{"AlarmAddressRange", message.AlarmAddressRange},
			struct {
				name  string
				value *string
			}{"AlarmPriority", message.AlarmPriority},
			struct {
				name  string
				value *string
			}{"AlarmMethod", message.AlarmMethod},
			struct {
				name  string
				value *string
			}{"AlarmClass", message.AlarmClass},
		)
	case CommandTGSAlarmRecordList:
		fields = append(fields,
			struct {
				name  string
				value *string
			}{"TollgateID", message.TollgateID},
			struct {
				name  string
				value *string
			}{"CarPlate", message.CarPlate},
			struct {
				name  string
				value *string
			}{"PlateType", message.PlateType},
		)
	}
	for _, field := range fields {
		if err := encodeOptional(encode, field.name, field.value); err != nil {
			return err
		}
	}
	return encoder.EncodeToken(start.End())
}

func encodeOptional(encode func(string, any) error, name string, value *string) error {
	if value == nil {
		return nil
	}
	return encode(name, *value)
}
