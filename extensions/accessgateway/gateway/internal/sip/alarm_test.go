package sip

import (
	"fmt"
	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"net"
	"strings"
	"testing"
	"time"
)

func alarmSIP(info string) []byte {
	body := `<?xml version="1.0" encoding="UTF-8"?><Notify><CmdType>Alarm</CmdType><DeviceID>34020000001320000002</DeviceID><AlarmTime>2026-11-12T10:00:00</AlarmTime><AlarmMethod>5</AlarmMethod><AlarmDescription>移动侦测</AlarmDescription>` + info + `</Notify>`
	return []byte("NOTIFY sip:a@8.8.8.8 SIP/2.0\r\nv: SIP/2.0/UDP 8.8.8.8:15060;branch=z9hG4bKa\r\nf: <sip:34020000001320000001@8.8.8.8>;tag=a\r\nt: <sip:a@8.8.8.8>\r\ni: alarm-1\r\nCSeq: 1 NOTIFY\r\nContent-Length: " + fmtInt(len(body)) + "\r\n\r\n" + body)
}
func fmtInt(n int) string { return fmt.Sprint(n) }
func TestAlarmTextCompatibilityAndReply(t *testing.T) {
	for _, info := range []string{`<AlarmType>2</AlarmType>`, `<Info><AlarmType>2</AlarmType><AlarmTypeParam><EventType>1</EventType></AlarmTypeParam></Info>`} {
		raw := alarmSIP(info)
		a, ok, err := ParseAlarm(raw)
		if err != nil || !ok || a.Type != "2" || a.Method != "5" || a.Description != "移动侦测" {
			t.Fatal(a, ok, err)
		}
		reply := string(Accepted(raw))
		if !strings.HasPrefix(reply, "SIP/2.0 200 OK") || !strings.Contains(reply, ";tag=") || strings.Contains(reply, "Retry-After") || !strings.Contains(reply, "CSeq: 1 NOTIFY") {
			t.Fatal(reply)
		}
		if string(Accepted(raw)) != reply {
			t.Fatal("重传应返回同一 To-tag")
		}
	}
	raw := alarmSIP("")
	response := strings.ReplaceAll(strings.ReplaceAll(string(raw), "<Notify>", "<Response>"), "</Notify>", "</Response>")
	if _, ok, _ := ParseAlarm([]byte(response)); ok {
		t.Fatal("查询/业务应答被当成报警")
	}
	signed := strings.Replace(string(raw), "CSeq:", "Note: signed\r\nCSeq:", 1)
	if _, ok, _ := ParseAlarm([]byte(signed)); ok {
		t.Fatal("签名报警绕过 A 验签")
	}
	if AlarmID("one", raw) == AlarmID("two", raw) {
		t.Fatal("报警身份未隔离")
	}
}
func TestResumeOnlyValidAuthenticatedDeviceFlows(t *testing.T) {
	h := New(t.Context(), 10, 100, nil, nil)
	defer h.Close()
	f, err := h.newFlow(&net.UDPAddr{IP: net.ParseIP("8.8.8.8"), Port: 15060}, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 15060}, "udp", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := h.AlarmBinding(f.Route.FlowID); ok {
		t.Fatal("匿名 flow 获得补报权")
	}
	if err = h.Bind(protocol.Binding{FlowID: f.Route.FlowID, SubjectID: "device", SubjectType: "device", BindingRevision: "1", ExpiresInMS: 60000}, "bind"); err != nil {
		t.Fatal(err)
	}
	if len(h.ResumeFlows("").Items) != 1 {
		t.Fatal("有效绑定未被续接")
	}
	h.mu.Lock()
	f.Expires = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if _, _, ok := h.AlarmBinding(f.Route.FlowID); ok || len(h.ResumeFlows("").Items) != 0 {
		t.Fatal("过期绑定仍允许补报或续接")
	}
	if len(h.ResumeFlows(uuid.NewString()).Items) != 0 {
		t.Fatal("过期流仍被返回")
	}
}
