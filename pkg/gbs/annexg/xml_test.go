package annexg

import (
	"errors"
	"strings"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const testTime = "2026-08-27T10:20:30"

func stringPointer(value string) *string  { return &value }
func floatPointer(value float64) *float64 { return &value }
func intPointer(value int) *int           { return &value }
func boolPointer(value bool) *bool        { return &value }

func testMPRecord() MPAlarmRecord {
	return MPAlarmRecord{
		AlarmNO:       "mp-1",
		AlarmTime:     testTime,
		DeviceID:      "34020000001320000001",
		AlarmClass:    stringPointer("1"),
		AlarmPriority: "1",
		AlarmMethod:   "2",
		Longitude:     floatPointer(120.5),
		Latitude:      floatPointer(30.5),
		CarPlates:     []string{"浙A12345"},
		PlateTypes:    []string{"02"},
		Victims:       []string{"person"},
		OriginalNO:    "original-1",
		OriginalInfo:  "original alarm",
		Sender:        "sender",
		Processor:     "processor",
		AlarmLevel:    "level-1",
		Disposal:      "disposal",
		AlarmInfo:     "alarm info",
		Info:          []string{"extension"},
	}
}

func TestDecodeAcceptsGB2312AnnexGMessage(t *testing.T) {
	encoded, err := Encode(Version2014, &ConfigDefenceNotify{
		CmdType: CommandConfigDefence, SN: 21, Type: boolPointer(false), TollgateID: "34020000001990000001",
		CarPlate: "浙A12345", PlateType: "02", DefenceType: "布控", DefenceTime: testTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	gb2312, err := sip.EncodeGBXMLDocument(encoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(Version2014, gb2312)
	if err != nil {
		t.Fatal(err)
	}
	message, ok := decoded.(*ConfigDefenceNotify)
	if !ok || message.DefenceType != "布控" || message.CarPlate != "浙A12345" {
		t.Fatalf("decoded Annex G message = %#v", decoded)
	}
}

func testECSRecord() ECSAlarmRecord {
	return ECSAlarmRecord{
		AlarmNO:        "ecs-1",
		AlarmTime:      testTime,
		AlarmPriority:  "1",
		AlarmClass:     "1",
		AlarmAddress:   "address",
		AlarmMethod:    "2",
		AlarmTelephone: "110",
		Processor:      "processor-1",
		SrecipientName: "operator",
		NsStatus:       "open",
		NCallType:      "alarm",
		AlarmInfo:      "alarm info",
	}
}

func testTGSRecord() TGSAlarmRecord {
	return TGSAlarmRecord{
		AlarmTime:    testTime,
		TollgateID:   "34020000001990000001",
		CarPlate:     "浙A12345",
		PlateType:    "02",
		DefenceType:  "wanted",
		ImageURL:     stringPointer("https://example.invalid/image.jpg"),
		Direction:    stringPointer("north"),
		VehicleSpeed: intPointer(80),
		PassTime:     stringPointer(testTime),
	}
}

func TestEncodeDecodeMessageMatrix(t *testing.T) {
	messages := []Message{
		&MPAlarmNotify{CmdType: CommandMPAlarm, SN: 1, AlarmContent: testMPRecord()},
		&ECSAlarmNotify{CmdType: CommandECSAlarm, SN: 2, AlarmContent: testECSRecord()},
		&TGSAlarmNotify{CmdType: CommandTGSAlarm, SN: 3, AlarmContent: testTGSRecord()},
		&ConfigDefenceNotify{
			CmdType: CommandConfigDefence, SN: 4, Type: boolPointer(false), TollgateID: "34020000001990000001",
			CarPlate: "浙A12345", PlateType: "02", DefenceType: "wanted", DefenceTime: testTime,
		},
		&AlarmRecordQuery{CmdType: CommandMPAlarmRecordList, SN: 5, BeginTime: stringPointer(testTime), AlarmClass: stringPointer("1")},
		&AlarmRecordQuery{CmdType: CommandECSAlarmRecordList, SN: 6, AlarmAddressRange: stringPointer("3402")},
		&AlarmRecordQuery{CmdType: CommandTGSAlarmRecordList, SN: 7, TollgateID: stringPointer("34020000001990000001")},
		&NotificationResponse{CmdType: CommandMPAlarm, SN: 8, Result: ResultOK},
		&NotificationResponse{CmdType: CommandConfigDefence, SN: 9, Result: ResultError, Info: []string{"denied"}},
		&MPAlarmRecordListResponse{
			CmdType: CommandMPAlarmRecordList, SN: 10, Result: ResultOK, RealRecordNum: 1, SendRecordNum: 1,
			RecordList: MPAlarmRecordList{AlarmRecords: []MPAlarmRecord{testMPRecord()}},
		},
		&ECSAlarmRecordListResponse{
			CmdType: CommandECSAlarmRecordList, SN: 11, Result: ResultOK, RealRecordNum: 1, SendRecordNum: 1,
			RecordList: ECSAlarmRecordList{AlarmRecords: []ECSAlarmRecord{testECSRecord()}},
		},
		&TGSAlarmRecordListResponse{
			CmdType: CommandTGSAlarmRecordList, SN: 12, Result: ResultOK, RealRecordNum: 1, SendRecordNum: 1,
			RecordList: TGSAlarmRecordList{AlarmRecords: []TGSAlarmRecord{testTGSRecord()}},
		},
	}

	for _, version := range []Version{Version2011, Version2014, Version2016} {
		for _, message := range messages {
			name := string(version) + "/" + message.RootName() + "/" + string(message.CommandType())
			t.Run(name, func(t *testing.T) {
				encoded, err := Encode(version, message)
				if err != nil {
					t.Fatalf("Encode() error = %v", err)
				}
				decoded, err := Decode(version, encoded)
				if err != nil {
					t.Fatalf("Decode() error = %v\n%s", err, encoded)
				}
				if decoded.RootName() != message.RootName() || decoded.CommandType() != message.CommandType() {
					t.Fatalf("Decode() = %s/%s, want %s/%s", decoded.RootName(), decoded.CommandType(), message.RootName(), message.CommandType())
				}
			})
		}
	}
}

func TestConfigDefenceGolden(t *testing.T) {
	message := &ConfigDefenceNotify{
		CmdType: CommandConfigDefence, SN: 18, Type: boolPointer(false), TollgateID: "34020000001990000001",
		CarPlate: "浙A12345", PlateType: "02", DefenceType: "wanted", DefenceTime: testTime,
	}
	encoded, err := Encode(Version2011, message)
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<Notify>
  <CmdType>ConfigDefence</CmdType>
  <SN>18</SN>
  <Type>false</Type>
  <TollgateID>34020000001990000001</TollgateID>
  <CarPlate>浙A12345</CarPlate>
  <PlateType>02</PlateType>
  <DefenceType>wanted</DefenceType>
  <DefenceTime>2026-08-27T10:20:30</DefenceTime>
</Notify>`
	if string(encoded) != want {
		t.Fatalf("Encode() =\n%s\nwant:\n%s", encoded, want)
	}
}

func TestQueryGoldenFieldOrder(t *testing.T) {
	message := &AlarmRecordQuery{
		CmdType: CommandECSAlarmRecordList, SN: 19,
		BeginTime: stringPointer("2026-08-27T10:00:00"), EndTime: stringPointer("2026-08-27T11:00:00"),
		AlarmAddressRange: stringPointer("3402"), AlarmPriority: stringPointer("1"), AlarmMethod: stringPointer("2"), AlarmClass: stringPointer("3"),
	}
	encoded, err := Encode(Version2016, message)
	if err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"<BeginTime>", "<EndTime>", "<AlarmAddressRange>", "<AlarmPriority>", "<AlarmMethod>", "<AlarmClass>"}
	position := -1
	for _, element := range wantOrder {
		next := strings.Index(string(encoded), element)
		if next <= position {
			t.Fatalf("field %s is out of order:\n%s", element, encoded)
		}
		position = next
	}
}

func TestVersionGate(t *testing.T) {
	message := &NotificationResponse{CmdType: CommandMPAlarm, SN: 1, Result: ResultOK}
	encoded := []byte(`<Response><CmdType>MPAlarm</CmdType><SN>1</SN><Result>OK</Result></Response>`)

	for _, version := range []Version{Version2011, Version2014, Version2016} {
		if _, err := Encode(version, message); err != nil {
			t.Errorf("Encode(%s) error = %v", version, err)
		}
		if _, err := Decode(version, encoded); err != nil {
			t.Errorf("Decode(%s) error = %v", version, err)
		}
	}
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{name: "encode", run: func() error { _, err := Encode(Version2022, message); return err }},
		{name: "decode", run: func() error { _, err := Decode(Version2022, encoded); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, ErrUnsupportedVersion) {
				t.Fatalf("error = %v, want ErrUnsupportedVersion", err)
			}
		})
	}
	for _, message := range []Message{
		&MPAlarmNotify{CmdType: CommandMPAlarm, SN: 1, AlarmContent: testMPRecord()},
		&ECSAlarmNotify{CmdType: CommandECSAlarm, SN: 1, AlarmContent: testECSRecord()},
		&TGSAlarmNotify{CmdType: CommandTGSAlarm, SN: 1, AlarmContent: testTGSRecord()},
		&ConfigDefenceNotify{CmdType: CommandConfigDefence, SN: 1, Type: boolPointer(true), TollgateID: "gate", CarPlate: "plate", PlateType: "02", DefenceType: "wanted", DefenceTime: testTime},
		&AlarmRecordQuery{CmdType: CommandMPAlarmRecordList, SN: 1},
		&AlarmRecordQuery{CmdType: CommandECSAlarmRecordList, SN: 1},
		&AlarmRecordQuery{CmdType: CommandTGSAlarmRecordList, SN: 1},
	} {
		if _, err := Encode(Version2022, message); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("Encode(2022, %s) error = %v, want ErrUnsupportedVersion", message.CommandType(), err)
		}
	}
}

func TestDecodeRejectsMalformedXML(t *testing.T) {
	tests := []struct {
		name string
		xml  string
	}{
		{name: "unknown command", xml: `<Notify><CmdType>Unknown</CmdType><SN>1</SN></Notify>`},
		{name: "wrong root", xml: `<Control><CmdType>ConfigDefence</CmdType><SN>1</SN></Control>`},
		{name: "duplicate SN", xml: `<Response><CmdType>MPAlarm</CmdType><SN>1</SN><SN>2</SN><Result>OK</Result></Response>`},
		{name: "unknown element", xml: `<Response><CmdType>MPAlarm</CmdType><SN>1</SN><Result>OK</Result><Extra>x</Extra></Response>`},
		{name: "out of order", xml: `<Response><SN>1</SN><CmdType>MPAlarm</CmdType><Result>OK</Result></Response>`},
		{name: "nested simple value", xml: `<Response><CmdType><Value>MPAlarm</Value></CmdType><SN>1</SN><Result>OK</Result></Response>`},
		{name: "attribute", xml: `<Response><CmdType mode="x">MPAlarm</CmdType><SN>1</SN><Result>OK</Result></Response>`},
		{name: "foreign namespace", xml: `<Response xmlns="urn:invalid"><CmdType>MPAlarm</CmdType><SN>1</SN><Result>OK</Result></Response>`},
		{name: "duplicate XML declaration", xml: `<?xml version="1.0"?><?xml version="1.0"?><Response><CmdType>MPAlarm</CmdType><SN>1</SN><Result>OK</Result></Response>`},
		{name: "invalid result", xml: `<Response><CmdType>MPAlarm</CmdType><SN>1</SN><Result>SUCCESS</Result></Response>`},
		{name: "non-positive SN", xml: `<Response><CmdType>MPAlarm</CmdType><SN>0</SN><Result>OK</Result></Response>`},
		{name: "invalid bool", xml: `<Notify><CmdType>ConfigDefence</CmdType><SN>1</SN><Type>yes</Type><TollgateID>x</TollgateID><CarPlate>x</CarPlate><PlateType>x</PlateType><DefenceType>x</DefenceType><DefenceTime>${testTime}</DefenceTime></Notify>`},
		{name: "non-schema bool", xml: `<Notify><CmdType>ConfigDefence</CmdType><SN>1</SN><Type>TRUE</Type><TollgateID>x</TollgateID><CarPlate>x</CarPlate><PlateType>x</PlateType><DefenceType>x</DefenceType><DefenceTime>${testTime}</DefenceTime></Notify>`},
		{name: "missing bool", xml: `<Notify><CmdType>ConfigDefence</CmdType><SN>1</SN><TollgateID>x</TollgateID><CarPlate>x</CarPlate><PlateType>x</PlateType><DefenceType>x</DefenceType><DefenceTime>${testTime}</DefenceTime></Notify>`},
		{name: "bad timestamp", xml: `<Notify><CmdType>ConfigDefence</CmdType><SN>1</SN><Type>true</Type><TollgateID>x</TollgateID><CarPlate>x</CarPlate><PlateType>x</PlateType><DefenceType>x</DefenceType><DefenceTime>not-a-time</DefenceTime></Notify>`},
		{name: "empty required string", xml: `<Notify><CmdType>ConfigDefence</CmdType><SN>1</SN><Type>true</Type><TollgateID> </TollgateID><CarPlate>x</CarPlate><PlateType>x</PlateType><DefenceType>x</DefenceType><DefenceTime>${testTime}</DefenceTime></Notify>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := strings.ReplaceAll(test.xml, "${testTime}", testTime)
			if _, err := Decode(Version2016, []byte(input)); err == nil {
				t.Fatalf("Decode() unexpectedly accepted %s", input)
			}
		})
	}
}

func TestClassifyEnvelopeRejectsAmbiguousIdentityEnvelope(t *testing.T) {
	tests := []string{
		`<Notify><CmdType>MPAlarm</CmdType><CmdType>ECSAlarm</CmdType></Notify>`,
		`<Notify><CmdType> MPAlarm </CmdType></Notify>`,
		`<Notify xmlns="urn:invalid"><CmdType>ECSAlarm</CmdType></Notify>`,
		`<Notify xmlns="http://www.w3.org/namespace/"><CmdType xmlns="">ECSAlarm</CmdType></Notify>`,
		`<Notify xmlns:x="urn:invalid"><CmdType>ECSAlarm</CmdType></Notify>`,
		`<Notify vendor="x"><CmdType>ECSAlarm</CmdType></Notify>`,
		`<Notify><CmdType vendor="x">ECSAlarm</CmdType></Notify>`,
		`<Notify><CmdType><Value>ECSAlarm</Value></CmdType></Notify>`,
	}
	for _, input := range tests {
		if root, command, err := ClassifyEnvelope([]byte(input)); err == nil {
			t.Fatalf("ClassifyEnvelope(%q) = %s/%s, want error", input, root, command)
		}
	}

	root, command, err := ClassifyEnvelope([]byte(`<Query><CmdType>ECSAlarmRecordList</CmdType></Query>`))
	if err != nil || root != "Query" || command != CommandECSAlarmRecordList {
		t.Fatalf("ClassifyEnvelope(valid) = %s/%s, %v", root, command, err)
	}

	root, command, err = ClassifyEnvelope([]byte(`<Query xmlns="http://www.w3.org/namespace/"><CmdType>ECSAlarmRecordList</CmdType></Query>`))
	if err != nil || root != "Query" || command != CommandECSAlarmRecordList {
		t.Fatalf("ClassifyEnvelope(standard namespace) = %s/%s, %v", root, command, err)
	}
}

func TestValidationBoundaries(t *testing.T) {
	t.Run("coordinates", func(t *testing.T) {
		message := &MPAlarmNotify{CmdType: CommandMPAlarm, SN: 1, AlarmContent: testMPRecord()}
		message.AlarmContent.Longitude = floatPointer(181)
		if err := message.Validate(Version2011); err == nil {
			t.Fatal("Validate() accepted invalid longitude")
		}
	})

	t.Run("record count", func(t *testing.T) {
		message := &MPAlarmRecordListResponse{
			CmdType: CommandMPAlarmRecordList, SN: 1, Result: ResultOK, RealRecordNum: 2, SendRecordNum: 2,
			RecordList: MPAlarmRecordList{AlarmRecords: []MPAlarmRecord{testMPRecord()}},
		}
		if err := message.Validate(Version2011); err == nil {
			t.Fatal("Validate() accepted mismatched record count")
		}
	})

	t.Run("query filters", func(t *testing.T) {
		message := &AlarmRecordQuery{CmdType: CommandTGSAlarmRecordList, SN: 1, AlarmMethod: stringPointer("2")}
		if err := message.Validate(Version2011); err == nil {
			t.Fatal("Validate() accepted a filter from another query")
		}
	})

	t.Run("query time order", func(t *testing.T) {
		message := &AlarmRecordQuery{
			CmdType: CommandMPAlarmRecordList, SN: 1,
			BeginTime: stringPointer("2026-08-27T11:00:00"), EndTime: stringPointer("2026-08-27T10:00:00"),
		}
		if err := message.Validate(Version2011); err == nil {
			t.Fatal("Validate() accepted an inverted time range")
		}
	})

	t.Run("query time zones", func(t *testing.T) {
		message := &AlarmRecordQuery{
			CmdType: CommandMPAlarmRecordList, SN: 1,
			BeginTime: stringPointer("2026-08-27T10:00:00"), EndTime: stringPointer("2026-08-27T10:30:00+08:00"),
		}
		if err := message.Validate(Version2011); err != nil {
			t.Fatalf("Validate() rejected equivalent Beijing time forms: %v", err)
		}
	})

	t.Run("Info unicode length", func(t *testing.T) {
		valid := &NotificationResponse{CmdType: CommandConfigDefence, SN: 1, Result: ResultOK, Info: []string{strings.Repeat("警", 1024)}}
		if err := valid.Validate(Version2011); err != nil {
			t.Fatalf("Validate() rejected 1024 characters: %v", err)
		}
		invalid := &NotificationResponse{CmdType: CommandConfigDefence, SN: 1, Result: ResultOK, Info: []string{strings.Repeat("警", 1025)}}
		if err := invalid.Validate(Version2011); err == nil {
			t.Fatal("Validate() accepted 1025 characters")
		}
	})
}

func TestDecodeAcceptsStandardNamespace(t *testing.T) {
	input := `<Response xmlns="http://www.w3.org/namespace/"><CmdType>MPAlarm</CmdType><SN>1</SN><Result>OK</Result></Response>`
	if _, err := Decode(Version2011, []byte(input)); err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
}

func TestDecodeAcceptsXMLSchemaBooleanLexemes(t *testing.T) {
	for _, value := range []string{"true", "false", "1", "0"} {
		input := `<Notify><CmdType>ConfigDefence</CmdType><SN>1</SN><Type>` + value + `</Type><TollgateID>x</TollgateID><CarPlate>x</CarPlate><PlateType>x</PlateType><DefenceType>x</DefenceType><DefenceTime>` + testTime + `</DefenceTime></Notify>`
		if _, err := Decode(Version2011, []byte(input)); err != nil {
			t.Errorf("Decode(Type=%q) error = %v", value, err)
		}
	}
}

func TestDecodeRejectsNonXMLSchemaBooleanLexemes(t *testing.T) {
	for _, value := range []string{"TRUE", "False", "t", "f"} {
		input := `<Notify><CmdType>ConfigDefence</CmdType><SN>1</SN><Type>` + value + `</Type><TollgateID>x</TollgateID><CarPlate>x</CarPlate><PlateType>x</PlateType><DefenceType>x</DefenceType><DefenceTime>` + testTime + `</DefenceTime></Notify>`
		if _, err := Decode(Version2011, []byte(input)); err == nil {
			t.Errorf("Decode(Type=%q) unexpectedly succeeded", value)
		}
	}
}

func TestParseVersionAliases(t *testing.T) {
	tests := map[string]Version{
		"1.0": Version2011, "2011": Version2011,
		"1.1": Version2014, "2014": Version2014, "2011-supplement-2014": Version2014,
		"2.0": Version2016, "2016": Version2016,
		"3.0": Version2022, "2022": Version2022,
	}
	for input, want := range tests {
		if got, ok := ParseVersion(input); !ok || got != want {
			t.Errorf("ParseVersion(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := ParseVersion("unknown"); ok {
		t.Fatal("ParseVersion() accepted unknown version")
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`<Response><CmdType>MPAlarm</CmdType><SN>1</SN><Result>OK</Result></Response>`))
	f.Add([]byte(`<Notify><CmdType>ConfigDefence</CmdType></Notify>`))
	f.Add([]byte(`<!DOCTYPE x><Response/>`))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Decode(Version2011, data)
	})
}

func TestDecodeRejectsXMLResourceAmplification(t *testing.T) {
	t.Run("body size", func(t *testing.T) {
		body := make([]byte, maximumXMLBytes+1)
		if _, err := Decode(Version2011, body); err == nil || !strings.Contains(err.Error(), "byte limit") {
			t.Fatalf("oversized XML error = %v", err)
		}
	})

	t.Run("depth", func(t *testing.T) {
		var body strings.Builder
		for range maximumXMLDepth + 1 {
			body.WriteString("<x>")
		}
		for range maximumXMLDepth + 1 {
			body.WriteString("</x>")
		}
		if _, err := Decode(Version2011, []byte(body.String())); err == nil || !strings.Contains(err.Error(), "maximum depth") {
			t.Fatalf("deep XML error = %v", err)
		}
	})

	t.Run("node count", func(t *testing.T) {
		var body strings.Builder
		body.Grow(maximumXMLNodes*4 + len("<x></x>"))
		body.WriteString("<x>")
		for range maximumXMLNodes {
			body.WriteString("<x/>")
		}
		body.WriteString("</x>")
		if _, err := Decode(Version2011, []byte(body.String())); err == nil || !strings.Contains(err.Error(), "node count") {
			t.Fatalf("high-node XML error = %v", err)
		}
	})
}
