package gbs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

// 夹具来自现场海康 2022 设备的三个响应，仅替换设备编号、IP 和查询序号。
func TestDevice2022CompatibilityResponseReplay(t *testing.T) {
	adapter, device, _ := newCascadeMediaCore(t)
	if err := adapter.Update(device.DeviceID, func(d *ipc.Device) {
		d.Ext.GBDeclaredVersion, d.Ext.GBEffectiveVersion = "3.0", "3.0"
	}); err != nil {
		t.Fatal(err)
	}
	api, memory := newVersionGateAPI(GBVersion30)
	api.core = adapter
	api.catalogResponses = newMultiResponseCollector(func(item Channels) string { return item.ChannelID })
	catalogKey := buildMultiResponseKey(gb10DeviceID, "Catalog", 301)
	api.catalogResponses.Start(catalogKey)
	for _, test := range []struct {
		cmd, fixture string
		sn           int
		handler      func(*sip.Context)
	}{
		{"Catalog", "device-compat-catalog.xml", 301, api.sipMessageCatalog},
		{"DeviceInfo", "device-compat-info.xml", 302, api.sipMessageDeviceInfo},
		{"ConfigDownload", "device-compat-config.xml", 303, api.sipMessageConfigDownload},
	} {
		pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
		key := buildPendingQueryKey(gb10DeviceID, test.cmd, test.sn)
		api.pendingDeviceQuery.Store(key, pending)
		body := readGBVersionFixture(t, "3.0", test.fixture)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, test.cmd, body, test.handler)
		assertFlowOK(t, response)
		select {
		case out := <-pending.wait:
			if out.SN != test.sn || out.DeviceID != gb10DeviceID || out.CmdType != test.cmd {
				t.Fatalf("%s pending result = %+v", test.cmd, out)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s did not complete its query", test.cmd)
		}
		api.pendingDeviceQuery.Delete(key)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := api.catalogResponses.Wait(ctx, catalogKey)
	if !result.Complete || len(result.Items) != 1 || result.Items[0].ChannelID != gb10ChannelID ||
		result.Items[0].Owner != "Owner" || result.Items[0].CivilCode != "5001070000" {
		t.Fatalf("Catalog was not collected with its original metadata: %+v", result)
	}
	var stored ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &stored, orm.Where("device_id = ?", gb10DeviceID)); err != nil {
		t.Fatal(err)
	}
	if stored.Ext.Manufacturer != "Hikvision" || stored.Ext.Firmware != "V5.8.21" || stored.Ext.GBEffectiveVersion != "3.0" {
		t.Fatalf("persisted DeviceInfo or protocol changed: %+v", stored.Ext)
	}
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.ConfigDownload == nil || state.ConfigDownload.BasicParam == nil ||
		!state.ConfigDownload.BasicParam.present.PositionCapability || state.ConfigDownload.BasicParam.PositionCapability != 0 {
		t.Fatalf("legacy positioning field was not retained: %+v", state)
	}
	if memory.device.GBVersion() != "3.0" || memory.device.keepaliveInterval != 60 || memory.device.keepaliveTimeout != 3 {
		t.Fatal("compatibility changed protocol version or lost heartbeat settings")
	}
}

func TestDevice2022CompatibilityRejectsInvalidResponses(t *testing.T) {
	for _, test := range []struct {
		name, cmd, fixture, old, replacement string
		sn                                   int
	}{
		{"duplicate owner", "Catalog", "device-compat-catalog.xml", "<Owner>Owner</Owner>", "<Owner>Owner</Owner><Owner>duplicate</Owner>", 301},
		{"invalid safety way", "Catalog", "device-compat-catalog.xml", "<SafetyWay>0</SafetyWay>", "<SafetyWay>9</SafetyWay>", 301},
		{"invalid civil code", "Catalog", "device-compat-catalog.xml", "5001070000", "500107000x", 301},
		{"mismatched count", "Catalog", "device-compat-catalog.xml", `Num="1"`, `Num="2"`, 301},
		{"duplicate name", "DeviceInfo", "device-compat-info.xml", "<DeviceName>IP CAMERA</DeviceName>", "<DeviceName>one</DeviceName><DeviceName>two</DeviceName>", 302},
		{"wrong target", "DeviceInfo", "device-compat-info.xml", gb10DeviceID, "34020000001320000999", 302},
		{"duplicate positioning", "ConfigDownload", "device-compat-config.xml", "<PositionCapability>0</PositionCapability>", "<PositionCapability>0</PositionCapability><PositionCapability>1</PositionCapability>", 303},
		{"invalid positioning", "ConfigDownload", "device-compat-config.xml", "<PositionCapability>0</PositionCapability>", "<PositionCapability>3</PositionCapability>", 303},
		{"nonfinite longitude", "ConfigDownload", "device-compat-config.xml", "</BasicParam>", "<Longitude>NaN</Longitude></BasicParam>", 303},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, test.cmd, test.sn), pending)
			handlers := map[string]func(*sip.Context){"Catalog": api.sipMessageCatalog, "DeviceInfo": api.sipMessageDeviceInfo, "ConfigDownload": api.sipMessageConfigDownload}
			body := strings.Replace(string(readGBVersionFixture(t, "3.0", test.fixture)), test.old, test.replacement, 1)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, test.name, []byte(body), handlers[test.cmd])
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid response accepted: %s", response)
			}
			select {
			case <-pending.wait:
				t.Fatal("invalid response completed a query")
			default:
			}
		})
	}
}

func TestDeviceReceiveCompatibility2016And2022(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			api, _ := newVersionGateAPI(version)
			// 通过真实接收处理器确认顺序和普通扩展兼容，不仅验证结构函数。
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceStatus", 401), pending)
			body := `<Response><Status>OK</Status><Info>vendor extension</Info>` +
				`<DeviceID>` + gb10DeviceID + `</DeviceID><Online>ONLINE</Online><SN>401</SN>` +
				`<Alarmstatus Num="1"><Item><DutyStatus>ONDUTY</DutyStatus><DeviceID>` + gb10ChannelID +
				`</DeviceID></Item></Alarmstatus><Result>OK</Result><CmdType>DeviceStatus</CmdType></Response>`
			assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "compat-status", []byte(body), api.sipMessageQueryGeneric))
			select {
			case result := <-pending.wait:
				if result.SN != 401 || result.DeviceID != gb10DeviceID {
					t.Fatal("compatibility resolved a different query")
				}
			default:
				t.Fatal("compatible status did not complete pending query")
			}
			for _, bad := range []string{
				strings.Replace(body, `<SN>401</SN>`, `<SN>401</SN><SN>402</SN>`, 1),
				strings.Replace(body, `Num="1"`, `Num="1" num="1"`, 1),
				strings.Replace(body, `Num="1"`, `Num="2"`, 1),
				strings.Replace(body, `<Info>vendor extension</Info>`, `<Info>`+strings.Repeat("x", 1025)+`</Info>`, 1),
			} {
				if err := validateGenericQueryPayloadWithCompatibility(version, "DeviceStatus", []byte(bad), true); err == nil {
					t.Fatal("invalid compatible DeviceStatus accepted")
				}
			}
			for _, fixture := range []struct {
				file, cmd string
				rules     []notificationElementRule
			}{
				{"keepalive.xml", "Keepalive", keepaliveRules},
				{"record-info-response.xml", "RecordInfo", deviceRecordInfoRules(version)},
			} {
				data := string(readGBVersionFixture(t, string(version), fixture.file))
				cmd := "<CmdType>" + fixture.cmd + "</CmdType>"
				data = strings.Replace(data, cmd, "", 1)
				root := "Response"
				if fixture.cmd == "Keepalive" {
					root = "Notify"
				}
				data = strings.Replace(data, "</"+root+">", cmd+"</"+root+">", 1)
				if err := validateDeviceMANSCDPStructure([]byte(data), version, root, fixture.cmd, fixture.rules); err != nil {
					t.Fatalf("%s rejected reordered device response: %v", fixture.cmd, err)
				}
				// 验证兼容函数没有改写共享规则或放宽对外验证。
				if err := validateMANSCDPStructure([]byte(data), root, fixture.cmd, fixture.rules); err == nil {
					t.Fatal("receive compatibility modified strict validation")
				}
			}
			config := []byte(`<Response><Result>OK</Result><BasicParam><HeartBeatCount>3</HeartBeatCount><Expiration>3600</Expiration><Name>camera</Name><HeartBeatInterval>60</HeartBeatInterval></BasicParam><SN>402</SN><DeviceID>` + gb10DeviceID + `</DeviceID><CmdType>ConfigDownload</CmdType></Response>`)
			if err := validateConfigDownloadResponseSingletonsWithCompatibility(config, true); err != nil {
				t.Fatal(err)
			}
			if err := validateBasicParamResponseStructureWithCompatibility(config, version, true); err != nil {
				t.Fatal(err)
			}
			if err := validateBasicParamResponseStructure(config, version); err == nil {
				t.Fatal("strict ConfigDownload accepted reordered fields")
			}
			configPending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: gb10DeviceID}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "ConfigDownload", 402), configPending)
			assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "compat-config", config, api.sipMessageConfigDownload))
			select {
			case <-configPending.wait:
			default:
				t.Fatal("reordered ConfigDownload did not complete pending query")
			}
			alarm := string(readGBVersionFixture(t, string(version), "alarm-notify.xml"))
			alarm = strings.Replace(alarm, "<CmdType>Alarm</CmdType>", "", 1)
			alarm = strings.Replace(alarm, "</Notify>", "<CmdType>Alarm</CmdType></Notify>", 1)
			if err := validateAlarmStructureWithCompatibility([]byte(alarm), version, true); err != nil {
				t.Fatal("reordered Alarm rejected:", err)
			}
			position := `<Notify><SN>403</SN><CmdType>MobilePosition</CmdType><DeviceID>` + gb10DeviceID + `</DeviceID><Time>2026-11-11T12:00:00</Time>`
			if version == GBVersion20 {
				position += `<Latitude>30</Latitude><Longitude>120</Longitude>`
			} else {
				position += `<SumNum>1</SumNum><DeviceList Num="1"><Item><Latitude>30</Latitude><DeviceID>` + gb10ChannelID + `</DeviceID><Longitude>120</Longitude><CaptureTime>2026-11-11T12:00:00</CaptureTime></Item></DeviceList>`
			}
			if err := validateMobilePositionNotifyStructureWithCompatibility([]byte(position+`</Notify>`), version, true); err != nil {
				t.Fatal("reordered MobilePosition rejected:", err)
			}
		})
	}
}
