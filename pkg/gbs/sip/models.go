package sip

import (
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// DefaultProtocol DefaultProtocol
var DefaultProtocol = "udp"

// DefaultSipVersion DefaultSipVersion
var DefaultSipVersion = "SIP/2.0"

// Port number
type Port uint16

// NewPort NewPort
func NewPort(port int) *Port {
	newPort := Port(port)
	return &newPort
}

// Clone clone
func (port *Port) Clone() *Port {
	if port == nil {
		return nil
	}
	newPort := *port
	return &newPort
}

func (port *Port) String() string {
	if port == nil {
		return ""
	}
	return fmt.Sprintf("%d", *port)
}

// Equals Equals
func (port *Port) Equals(other any) bool {
	if p, ok := other.(*Port); ok {
		return Uint16PtrEq((*uint16)(port), (*uint16)(p))
	}

	return false
}

// MaybeString  wrapper
type MaybeString interface {
	String() string
	Equals(other any) bool
}

// String string
type String struct {
	Str string
}

func (str String) String() string {
	return str.Str
}

// Equals Equals
func (str String) Equals(other any) bool {
	if v, ok := other.(String); ok {
		return str.Str == v.Str
	}

	return false
}

// ContentTypeSDP SDP contenttype
var ContentTypeSDP = ContentType("application/sdp")

// ContentTypeXML XML contenttype
var ContentTypeXML = ContentType("Application/MANSCDP+xml")

var (
	// CatalogXML 获取设备列表xml样式
	CatalogXML = `<?xml version="1.0" encoding="GB2312"?>
<Query>
<CmdType>Catalog</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
</Query>
`
	// RecordInfoXML 获取录像文件列表xml样式
	RecordInfoXML = `<?xml version="1.0" encoding="GB2312"?>
<Query>
<CmdType>RecordInfo</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
<StartTime>%s</StartTime>
<EndTime>%s</EndTime>
<Secrecy>0</Secrecy>
<Type>%s</Type>
</Query>
`
	// DeviceInfoXML 查询设备详情xml样式
	DeviceInfoXML = `<?xml version="1.0" encoding="GB2312"?>
<Query>
<CmdType>DeviceInfo</CmdType>
<SN>%d</SN>
<DeviceID>%s</DeviceID>
</Query>
`
)

// GetDeviceInfoXML 获取设备详情指令
func GetDeviceInfoXML(id string) []byte {
	return fmt.Appendf(nil, DeviceInfoXML, RandInt(100000, 999999), id)
}

// GetCatalogXML 获取NVR下设备列表指令
func GetCatalogXML(id string) []byte {
	return fmt.Appendf(nil, CatalogXML, RandInt(100000, 999999), id)
}

// GetRecordInfoXML 获取录像文件列表指令
func GetRecordInfoXML(id string, sceqNo int, start, end int64) []byte {
	return GetRecordInfoXMLWithFilters(id, sceqNo, start, end, RecordInfoQueryFilters{})
}

// RecordInfoQueryFilters 对应文件目录检索类型及 GB/T 28181-2022 新增过滤条件。
type RecordInfoQueryFilters struct {
	Type         string
	StreamNumber *int
	AlarmMethod  string
	AlarmType    string
}

// GetRecordInfoXMLWithFilters 获取带录像类型及 2022 扩展过滤条件的录像文件列表指令。
func GetRecordInfoXMLWithFilters(id string, sceqNo int, start, end int64, filters RecordInfoQueryFilters) []byte {
	recordType := strings.ToLower(strings.TrimSpace(filters.Type))
	if recordType == "" {
		recordType = "time"
	}
	var escapedType strings.Builder
	_ = xml.EscapeText(&escapedType, []byte(recordType))
	body := fmt.Appendf(nil, RecordInfoXML, sceqNo, id, FormatGBTime(time.Unix(start, 0), "2006-01-02T15:04:05"), FormatGBTime(time.Unix(end, 0), "2006-01-02T15:04:05"), escapedType.String())
	closing := []byte("</Query>")
	index := strings.LastIndex(string(body), string(closing))
	if index < 0 {
		return body
	}

	var extra strings.Builder
	appendFilter := func(name, value string) {
		extra.WriteByte('\t')
		extra.WriteByte('<')
		extra.WriteString(name)
		extra.WriteByte('>')
		_ = xml.EscapeText(&extra, []byte(value))
		extra.WriteString("</")
		extra.WriteString(name)
		extra.WriteString(">\n")
	}
	if filters.StreamNumber != nil {
		appendFilter("StreamNumber", fmt.Sprintf("%d", *filters.StreamNumber))
	}
	if value := strings.TrimSpace(filters.AlarmMethod); value != "" {
		appendFilter("AlarmMethod", value)
	}
	if value := strings.TrimSpace(filters.AlarmType); value != "" {
		appendFilter("AlarmType", value)
	}
	if extra.Len() == 0 {
		return body
	}

	result := make([]byte, 0, len(body)+extra.Len())
	result = append(result, body[:index]...)
	result = append(result, extra.String()...)
	result = append(result, body[index:]...)
	return result
}

// RFC3261BranchMagicCookie RFC3261BranchMagicCookie
const RFC3261BranchMagicCookie = "z9hG4bK"

// GenerateBranch returns random unique branch ID.
func GenerateBranch() string {
	return strings.Join([]string{
		RFC3261BranchMagicCookie,
		RandString(32),
	}, "")
}
