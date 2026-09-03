package gbs

import (
	"testing"
	"unicode/utf8"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestStrictConfigValidatorAcceptsGB2312(t *testing.T) {
	body, err := sip.EncodeGBXMLDocument([]byte(`<Response><CmdType>ConfigDownload</CmdType><SN>1</SN><DeviceID>34020000001320000001</DeviceID><Result>OK</Result><BasicParam><Name>中文名称</Name></BasicParam></Response>`))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateConfigDownloadResponseSingletons(body); err != nil {
		t.Fatal(err)
	}
}

func TestCascadeAlarmRewritePreservesGB2312Encoding(t *testing.T) {
	body, err := sip.EncodeGBXMLDocument([]byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>34020000001320000001</DeviceID><AlarmDescription>中文报警</AlarmDescription></Notify>`))
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := rewriteAlarmDispatchSN(body, 27)
	if err != nil {
		t.Fatal(err)
	}
	if utf8.Valid(rewritten) {
		t.Fatal("rewritten GB2312 document unexpectedly contains only valid UTF-8 bytes")
	}
	var decoded struct {
		SN               int    `xml:"SN"`
		AlarmDescription string `xml:"AlarmDescription"`
	}
	if err := sip.XMLDecode(rewritten, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.SN != 27 || decoded.AlarmDescription != "中文报警" {
		t.Fatalf("rewritten alarm = %#v", decoded)
	}
}
