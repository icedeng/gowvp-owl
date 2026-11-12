package sip

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"golang.org/x/net/html/charset"
)

// ParseAlarm 只识别 MESSAGE/NOTIFY Alarm；不执行国标业务或设备鉴权。
func ParseAlarm(raw []byte) (protocol.AlarmRecord, bool, error) {
	var a protocol.AlarmRecord
	header, body, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		return a, false, nil
	}
	lines := strings.Split(string(header), "\r\n")
	if !strings.HasPrefix(lines[0], "MESSAGE ") && !strings.HasPrefix(lines[0], "NOTIFY ") {
		return a, false, nil
	}
	if len(body) > 64<<10 {
		return a, false, nil
	}
	var doc struct {
		XMLName     xml.Name
		CmdType     string `xml:"CmdType"`
		DeviceID    string `xml:"DeviceID"`
		AlarmTime   string `xml:"AlarmTime"`
		Priority    string `xml:"AlarmPriority"`
		Method      string `xml:"AlarmMethod"`
		Type        string `xml:"AlarmType"`
		Description string `xml:"AlarmDescription"`
		Longitude   string `xml:"Longitude"`
		Latitude    string `xml:"Latitude"`
		Info        []struct {
			Type   string `xml:"AlarmType"`
			Method string `xml:"AlarmMethod"`
			Param  struct {
				EventType *int `xml:"EventType"`
			} `xml:"AlarmTypeParam"`
		} `xml:"Info"`
	}
	d := xml.NewDecoder(bytes.NewReader(body))
	d.CharsetReader = charset.NewReaderLabel
	if err := d.Decode(&doc); err != nil {
		return a, false, nil
	}
	if doc.XMLName.Local != "Notify" || !strings.EqualFold(strings.TrimSpace(doc.CmdType), "Alarm") {
		return a, false, nil
	}
	// 已签名消息必须由 A 验签，B 不截断原事务或伪造签名响应。
	for _, line := range lines[1:] {
		key, _, _ := strings.Cut(line, ":")
		if strings.EqualFold(strings.TrimSpace(key), "Note") {
			return a, false, nil
		}
	}
	from := ""
	for _, line := range lines[1:] {
		k, v, ok := strings.Cut(line, ":")
		if ok && (strings.EqualFold(k, "From") || strings.EqualFold(k, "f")) {
			if from != "" {
				return a, true, fmt.Errorf("duplicate From")
			}
			from = v
		}
	}
	index := strings.Index(strings.ToLower(from), "sip:")
	if index < 0 {
		return a, true, fmt.Errorf("missing alarm identity")
	}
	from = from[index+4:]
	code, _, ok := strings.Cut(from, "@")
	if !ok {
		return a, true, fmt.Errorf("missing alarm identity")
	}
	method, kind := strings.TrimSpace(doc.Method), strings.TrimSpace(doc.Type)
	var eventType *int
	for _, info := range doc.Info {
		infoMethod, infoType := strings.TrimSpace(info.Method), strings.TrimSpace(info.Type)
		if infoMethod != "" {
			if method != "" && method != infoMethod {
				return a, true, fmt.Errorf("conflicting alarm method")
			}
			method = infoMethod
		}
		if infoType != "" {
			if kind != "" && kind != infoType {
				return a, true, fmt.Errorf("conflicting alarm type")
			}
			kind = infoType
		}
		if info.Param.EventType != nil {
			if eventType != nil && *eventType != *info.Param.EventType {
				return a, true, fmt.Errorf("conflicting alarm phase")
			}
			eventType = info.Param.EventType
		}
	}
	a = protocol.AlarmRecord{DeviceCode: strings.TrimSpace(code), ChannelCode: strings.TrimSpace(doc.DeviceID), AlarmTime: strings.TrimSpace(doc.AlarmTime), Priority: strings.TrimSpace(doc.Priority), Method: strings.TrimSpace(method), Type: strings.TrimSpace(kind), EventType: eventType, Description: strings.TrimSpace(doc.Description), Longitude: strings.TrimSpace(doc.Longitude), Latitude: strings.TrimSpace(doc.Latitude)}
	return a, true, nil
}
func AlarmID(subject string, raw []byte) string {
	sum := sha256.Sum256(append([]byte(subject+"\x00"), raw...))
	return hex.EncodeToString(sum[:])
}
func Accepted(raw []byte) []byte {
	head, _, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		return nil
	}
	lines := strings.Split(string(head), "\r\n")
	names := map[string]string{"v": "via", "f": "from", "t": "to", "i": "call-id"}
	count := map[string]int{}
	var out strings.Builder
	out.WriteString("SIP/2.0 200 OK\r\n")
	tag := AlarmID("reply", raw)[:16]
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(name))
		if full := names[key]; full != "" {
			key = full
		}
		switch key {
		case "via", "from", "to", "call-id", "cseq":
		default:
			continue
		}
		count[key]++
		if key == "to" && !strings.Contains(strings.ToLower(value), ";tag=") {
			line += ";tag=" + tag
		}
		out.WriteString(line)
		out.WriteString("\r\n")
	}
	for _, key := range []string{"from", "to", "call-id", "cseq"} {
		if count[key] != 1 {
			return nil
		}
	}
	if count["via"] == 0 {
		return nil
	}
	out.WriteString("Content-Length: 0\r\n\r\n")
	return []byte(out.String())
}

func (h *Hub) AlarmBinding(flow string) (string, protocol.Decimal, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.flows[flow]
	if f == nil || f.SubjectType != "device" || f.SubjectID == "" || time.Now().After(f.Expires) {
		return "", "", false
	}
	return f.SubjectID, f.BindingRevision, true
}
func (h *Hub) ResumeFlows(after string) protocol.ResumeFlows {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := protocol.ResumeFlows{Items: []protocol.ResumeFlow{}}
	ids := make([]string, 0, len(h.flows))
	for id := range h.flows {
		if id > after {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		f := h.flows[id]
		if f.SubjectType != "device" || f.SubjectID == "" || time.Now().After(f.Expires) {
			continue
		}
		out.Items = append(out.Items, protocol.ResumeFlow{SubjectID: f.SubjectID, BindingRevision: f.BindingRevision, Route: f.Route, ExpiresAt: f.Expires.UTC().Format(time.RFC3339Nano), LastSeenAt: f.LastSeen.UTC().Format(time.RFC3339Nano)})
		if len(out.Items) == 64 {
			out.Next = id
			break
		}
	}
	return out
}
