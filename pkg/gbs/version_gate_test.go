package gbs

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestVersionGatesFor2011AndSupplement(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)

	if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, &deviceControlA23Request{}); err == nil {
		t.Fatal("1.0 must reject IFrame control")
	}
	if err := api.requireConfigTypeVersion("device", "BasicParam"); err == nil {
		t.Fatal("1.0 must reject ConfigDownload")
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err == nil {
		t.Fatal("1.0 must reject RTP over TCP")
	}

	memory.device.setGBVersion(GBVersion11)
	if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, &deviceControlA23Request{}); err == nil {
		t.Fatal("1.1 must reject 2.0 IFrame control")
	}
	if err := api.requireConfigTypeVersion("device", "BasicParam"); err != nil {
		t.Fatalf("1.1 ConfigDownload rejected: %v", err)
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionPresetQuery, ""); err != nil {
		t.Fatalf("1.1 PresetQuery rejected: %v", err)
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err == nil {
		t.Fatal("1.1 direct TCP download must not enable RTP over TCP")
	}

	memory.device.setGBVersion(GBVersion20)
	if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, &deviceControlA23Request{}); err != nil {
		t.Fatalf("2.0 IFrame control rejected: %v", err)
	}
	if _, err := api.resolveDeviceQueryCmdType("device", deviceQueryActionPresetQuery, ""); err != nil {
		t.Fatalf("2.0 PresetQuery rejected: %v", err)
	}
	if err := api.requireMediaTransport("device", 1, "实时点播"); err != nil {
		t.Fatalf("2.0 RTP over TCP rejected: %v", err)
	}
}

func TestRequireMediaTransportRejectsInvalidStreamMode(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion30)
	for _, streamMode := range []int8{-1, 3} {
		if err := api.requireMediaTransport("device", streamMode, "实时点播"); err == nil ||
			!strings.Contains(err.Error(), "invalid RTP stream mode") {
			t.Fatalf("stream mode %d error = %v", streamMode, err)
		}
	}
}

func TestIFrameCommandXMLFieldByProtocolVersion(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	tests := []struct {
		version   GBProtocolVersion
		want      string
		forbidden string
	}{
		{version: GBVersion20, want: "<IFameCmd>Send</IFameCmd>", forbidden: "<IFrameCmd>"},
		{version: GBVersion30, want: "<IFrameCmd>Send</IFrameCmd>", forbidden: "<IFameCmd>"},
	}
	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			memory.device.setGBVersion(test.version)
			request := &deviceControlA23Request{
				CmdType: ptzCmdTypeDeviceControl, SN: 1, DeviceID: "device",
			}
			if err := api.fillDeviceControlRequest("device", deviceControlActionIFrameSend, &DeviceControlInput{}, request); err != nil {
				t.Fatalf("fill force-I-frame control: %v", err)
			}
			body, err := sip.XMLEncode(request)
			if err != nil {
				t.Fatal(err)
			}
			text := string(body)
			if !strings.Contains(text, test.want) || strings.Contains(text, test.forbidden) {
				t.Fatalf("force-I-frame XML for %s = %s", test.version, text)
			}
		})
	}
}

func TestQueryConfigDownloadBasicUsesConfigQueryVersionGate(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion10)
	if err := api.QueryConfigDownloadBasic("device"); err == nil ||
		!strings.Contains(err.Error(), "设备配置查询(ConfigDownload)") {
		t.Fatalf("2011 ConfigDownload query error = %v", err)
	}

	// 2014 应通过版本门禁；测试桩未提供 SIP Server，后续由传输层返回不可用。
	memory.device.setGBVersion(GBVersion11)
	if err := api.QueryConfigDownloadBasic("device"); err == nil ||
		strings.Contains(err.Error(), "不受当前协议档案") {
		t.Fatalf("2014 ConfigDownload query did not pass version gate: %v", err)
	}
}

func TestDeviceQueryRejectsBroadcastNotifyFlow(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			api, _ := newVersionGateAPI(version)
			if _, err := api.resolveDeviceQueryCmdType("device", "broadcast", ""); err == nil ||
				!strings.Contains(err.Error(), "unsupported device query action") {
				t.Fatalf("broadcast query error = %v", err)
			}
		})
	}
}

func TestPresetQuery11AcceptsSupplementSpelling(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion11)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "PresetQuery", 71), pending)
	conn := newFlowConnection()
	body := []byte(`<Response><CmdType>PersetQuery</CmdType><SN>71</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><SumNum>0</SumNum><PresetList Num="0"></PresetList></Response>`)
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "preset-spelling", body, api.sipMessageQueryGeneric)
	assertFlowOK(t, response)
	select {
	case out := <-pending.wait:
		if out.CmdType != "PresetQuery" {
			t.Fatalf("canonical command = %q", out.CmdType)
		}
	default:
		t.Fatal("PersetQuery response did not resolve PresetQuery wait")
	}
}

func TestPresetQueryWireSpellingByVersion(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		want    string
	}{
		{name: "2011", version: GBVersion10, want: "PresetQuery"},
		{name: "2014 supplement", version: GBVersion11, want: "PersetQuery"},
		{name: "2016", version: GBVersion20, want: "PresetQuery"},
		{name: "2022", version: GBVersion30, want: "PresetQuery"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := gbQueryCmdTypeForVersion("PresetQuery", test.version); got != test.want {
				t.Fatalf("wire command = %q, want %q", got, test.want)
			}
			if got := gbQueryCmdTypeForVersion("PersetQuery", test.version); got != test.want {
				t.Fatalf("legacy wire command = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDeviceQuery11WritesSupplementPresetSpelling(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	flow := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: flow}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	memory.device.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = flow.remote
		device.to = remote
	})
	t.Cleanup(sipServer.Close)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := api.DeviceQuery(ctx, &DeviceQueryInput{DeviceID: gb10DeviceID, Action: deviceQueryActionPresetQuery})
		done <- err
	}()

	select {
	case payload := <-flow.writes:
		if body := string(payload); !strings.Contains(body, "<CmdType>PersetQuery</CmdType>") {
			t.Fatalf("2014 DeviceQuery body = %s", body)
		}
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("2014 DeviceQuery was not written")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled DeviceQuery error = %v", err)
	}
}

func TestDeviceQueryCatalogWritesOptionalTimeRangeByVersion(t *testing.T) {
	start := int64(1710864000)
	end := int64(1710950400)
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			api, flow := newDeviceQueryWireAPI(t, version)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := api.DeviceQuery(ctx, &DeviceQueryInput{
					DeviceID: gb10DeviceID, Action: deviceQueryActionCatalog, Start: start, End: end,
				})
				done <- err
			}()

			select {
			case payload := <-flow.writes:
				body := string(payload)
				for _, expected := range []string{
					"<CmdType>Catalog</CmdType>",
					"<StartTime>" + sip.FormatGBTime(time.Unix(start, 0), "2006-01-02T15:04:05") + "</StartTime>",
					"<EndTime>" + sip.FormatGBTime(time.Unix(end, 0), "2006-01-02T15:04:05") + "</EndTime>",
				} {
					if !strings.Contains(body, expected) {
						t.Fatalf("%s Catalog body missing %q: %s", version.StandardName(), expected, body)
					}
				}
				cancel()
			case err := <-done:
				cancel()
				t.Fatalf("%s Catalog returned before write: %v", version.StandardName(), err)
			case <-time.After(time.Second):
				cancel()
				t.Fatalf("%s Catalog was not written", version.StandardName())
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s Catalog error = %v", version.StandardName(), err)
			}
		})
	}
}

func TestDeviceQueryCatalogOmitsUnsetTimesAndRejectsInvalidRange(t *testing.T) {
	api, flow := newDeviceQueryWireAPI(t, GBVersion10)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := api.DeviceQuery(ctx, &DeviceQueryInput{DeviceID: gb10DeviceID, Action: deviceQueryActionCatalog})
		done <- err
	}()
	select {
	case payload := <-flow.writes:
		body := string(payload)
		if strings.Contains(body, "<StartTime>") || strings.Contains(body, "<EndTime>") {
			t.Fatalf("Catalog emitted unset time range: %s", body)
		}
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("Catalog without time range was not written")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Catalog error = %v", err)
	}

	api, flow = newDeviceQueryWireAPI(t, GBVersion30)
	if _, err := api.DeviceQuery(t.Context(), &DeviceQueryInput{
		DeviceID: gb10DeviceID, Action: deviceQueryActionCatalog, Start: 2, End: 1,
	}); err == nil || !strings.Contains(err.Error(), "catalog requires valid start/end") {
		t.Fatalf("invalid Catalog time range error = %v", err)
	}
	select {
	case payload := <-flow.writes:
		t.Fatalf("invalid Catalog query was written: %s", payload)
	default:
	}
}

func TestDeviceQueryAlarmWritesVersionSpecificFilters(t *testing.T) {
	start := "2026-08-25T08:00:00"
	end := "2026-08-25T09:00:00"
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			api, flow := newDeviceQueryWireAPI(t, version)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			input := &DeviceQueryInput{
				DeviceID: gb10DeviceID, Action: deviceQueryActionAlarm,
				StartAlarmPriority: "1", EndAlarmPriority: "4",
				AlarmMethod: "25", StartAlarmTime: start, EndAlarmTime: end,
			}
			if version.AtLeast(GBVersion20) {
				input.AlarmType = "2"
			}
			go func() {
				_, err := api.DeviceQuery(ctx, input)
				done <- err
			}()
			select {
			case payload := <-flow.writes:
				body := string(payload)
				for _, expected := range []string{
					"<CmdType>Alarm</CmdType>",
					"<StartAlarmPriority>1</StartAlarmPriority>",
					"<EndAlarmPriority>4</EndAlarmPriority>",
					"<StartAlarmTime>" + start + "</StartAlarmTime>",
					"<EndAlarmTime>" + end + "</EndAlarmTime>",
				} {
					if !strings.Contains(body, expected) {
						t.Fatalf("%s Alarm body missing %q: %s", version.StandardName(), expected, body)
					}
				}
				method := "<AlarmMethod>25</AlarmMethod>"
				if version == GBVersion30 {
					method = "<AlarmMethod>2/5</AlarmMethod>"
				}
				if !strings.Contains(body, method) {
					t.Fatalf("%s Alarm method encoding: %s", version.StandardName(), body)
				}
				if version.AtLeast(GBVersion20) && !strings.Contains(body, "<AlarmType>2</AlarmType>") {
					t.Fatalf("%s AlarmType missing: %s", version.StandardName(), body)
				}
				if !version.AtLeast(GBVersion20) && strings.Contains(body, "<AlarmType>") {
					t.Fatalf("%s emitted unsupported AlarmType: %s", version.StandardName(), body)
				}
				cancel()
			case err := <-done:
				cancel()
				t.Fatalf("%s Alarm returned before write: %v", version.StandardName(), err)
			case <-time.After(time.Second):
				cancel()
				t.Fatalf("%s Alarm was not written", version.StandardName())
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s Alarm error = %v", version.StandardName(), err)
			}
		})
	}
}

func TestDeviceQueryAlarmRejectsInvalidFiltersBeforeWrite(t *testing.T) {
	api, flow := newDeviceQueryWireAPI(t, GBVersion11)
	for _, input := range []*DeviceQueryInput{
		{DeviceID: gb10DeviceID, Action: deviceQueryActionAlarm, StartAlarmPriority: "4", EndAlarmPriority: "1"},
		{DeviceID: gb10DeviceID, Action: deviceQueryActionAlarm, AlarmType: "2"},
		{DeviceID: gb10DeviceID, Action: deviceQueryActionAlarm, StartAlarmTime: "2026-08-25T10:00:00", EndAlarmTime: "2026-08-25T09:00:00"},
	} {
		if _, err := api.DeviceQuery(t.Context(), input); err == nil {
			t.Fatalf("invalid Alarm input %+v was accepted", input)
		}
		select {
		case payload := <-flow.writes:
			t.Fatalf("invalid Alarm input %+v was written: %s", input, payload)
		default:
		}
	}
}

func TestDeviceQueryCatalogAdministrativeTargetUsesRegisteredSystemRoute(t *testing.T) {
	const administrativeID = "340200"
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			api, flow := newDeviceQueryWireAPI(t, version)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := api.DeviceQuery(ctx, &DeviceQueryInput{
					DeviceID: gb10DeviceID, TargetID: administrativeID, Action: deviceQueryActionCatalog,
				})
				done <- err
			}()

			select {
			case payload := <-flow.writes:
				text := string(payload)
				if !strings.HasPrefix(text, "MESSAGE sip:"+gb10DeviceID+"@") || !strings.Contains(text, "<DeviceID>"+administrativeID+"</DeviceID>") {
					t.Fatalf("%s administrative Catalog route = %s", version.StandardName(), text)
				}
				cancel()
			case err := <-done:
				cancel()
				t.Fatalf("%s administrative Catalog returned before write: %v", version.StandardName(), err)
			case <-time.After(time.Second):
				cancel()
				t.Fatalf("%s administrative Catalog was not written", version.StandardName())
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s administrative Catalog error = %v", version.StandardName(), err)
			}
		})
	}

	api, flow := newDeviceQueryWireAPI(t, GBVersion10)
	_, err := api.DeviceQuery(t.Context(), &DeviceQueryInput{
		DeviceID: gb10DeviceID, TargetID: administrativeID, Action: deviceQueryActionCatalog,
	})
	if err == nil || !strings.Contains(err.Error(), "requires GB/T 28181-2014") {
		t.Fatalf("2011 administrative Catalog error = %v", err)
	}
	select {
	case payload := <-flow.writes:
		t.Fatalf("2011 administrative Catalog was written: %s", payload)
	default:
	}
}

func TestDeviceQueryCatalogAdministrativeTargetRespectsCapabilityOverride(t *testing.T) {
	const administrativeID = "340200"
	api, flow := newDeviceQueryWireAPI(t, GBVersion11)
	memory, ok := api.svr.memoryStorer.(*versionGateMemory)
	if !ok {
		t.Fatalf("memory storer type = %T", api.svr.memoryStorer)
	}
	memory.device.setGBProfile(GBVersion11, []string{"catalog_extension"})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := api.DeviceQuery(ctx, &DeviceQueryInput{
			DeviceID: gb10DeviceID, TargetID: administrativeID, Action: deviceQueryActionCatalog,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "Catalog") {
			t.Fatalf("disabled Catalog extension error = %v", err)
		}
	case payload := <-flow.writes:
		cancel()
		<-done
		t.Fatalf("disabled Catalog extension was written: %s", payload)
	case <-time.After(time.Second):
		t.Fatal("disabled Catalog extension neither failed nor wrote a request")
	}
}

func TestQueryRecordListWritesStandardIndistinctQueryByVersion(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			api, flow := newRecordQueryWireAPI(t, version)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			indistinct := 1
			go func() {
				_, err := api.QueryRecordList(ctx, &RecordQueryInput{
					DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
					Start: 1, End: 2, IndistinctQuery: &indistinct,
				})
				done <- err
			}()

			select {
			case payload := <-flow.writes:
				body := string(payload)
				if !strings.Contains(body, "<IndistinctQuery>1</IndistinctQuery>") || strings.Contains(body, "<DistinctQuery>") {
					t.Fatalf("%s RecordInfo body = %s", version.StandardName(), body)
				}
				cancel()
			case err := <-done:
				cancel()
				t.Fatalf("%s RecordInfo returned before write: %v", version.StandardName(), err)
			case <-time.After(time.Second):
				cancel()
				t.Fatalf("%s RecordInfo was not written", version.StandardName())
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s RecordInfo error = %v", version.StandardName(), err)
			}
		})
	}
}

func TestDeviceQueryRecordInfoSupportsDeviceTargetForAllVersions(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(version.StandardName(), func(t *testing.T) {
			api, flow := newRecordQueryWireAPI(t, version)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			var indistinct *int
			if version.AtLeast(GBVersion11) {
				value := 0
				indistinct = &value
			}
			go func() {
				_, err := api.DeviceQuery(ctx, &DeviceQueryInput{
					DeviceID:        gb10DeviceID,
					Action:          deviceQueryActionRecordInfo,
					Start:           1,
					End:             2,
					IndistinctQuery: indistinct,
				})
				done <- err
			}()

			select {
			case payload := <-flow.writes:
				request := string(payload)
				if !strings.HasPrefix(request, "MESSAGE sip:"+gb10DeviceID+"@") {
					t.Fatalf("%s RecordInfo request target = %s", version.StandardName(), strings.SplitN(request, "\r\n", 2)[0])
				}
				if !strings.Contains(request, "<DeviceID>"+gb10DeviceID+"</DeviceID>") {
					t.Fatalf("%s RecordInfo body does not use device/system ID: %s", version.StandardName(), request)
				}
				if version.AtLeast(GBVersion11) && !strings.Contains(request, "<IndistinctQuery>0</IndistinctQuery>") {
					t.Fatalf("%s RecordInfo body does not preserve system-location query: %s", version.StandardName(), request)
				}
				if !version.AtLeast(GBVersion11) && strings.Contains(request, "<IndistinctQuery>") {
					t.Fatalf("%s RecordInfo body emitted unsupported IndistinctQuery: %s", version.StandardName(), request)
				}
				cancel()
			case err := <-done:
				cancel()
				t.Fatalf("%s device-target RecordInfo returned before write: %v", version.StandardName(), err)
			case <-time.After(time.Second):
				cancel()
				t.Fatalf("%s device-target RecordInfo was not written", version.StandardName())
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s device-target RecordInfo error = %v", version.StandardName(), err)
			}
		})
	}
}

func TestQueryRecordListLegacyOptionalTimesAndVersionGates(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11} {
		t.Run(version.StandardName()+" optional times", func(t *testing.T) {
			api, flow := newRecordQueryWireAPI(t, version)
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				_, err := api.QueryRecordList(ctx, &RecordQueryInput{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID})
				done <- err
			}()

			select {
			case payload := <-flow.writes:
				body := string(payload)
				if strings.Contains(body, "<StartTime>") || strings.Contains(body, "<EndTime>") {
					t.Fatalf("%s optional RecordInfo times were emitted: %s", version.StandardName(), body)
				}
				cancel()
			case err := <-done:
				cancel()
				t.Fatalf("%s RecordInfo returned before write: %v", version.StandardName(), err)
			case <-time.After(time.Second):
				cancel()
				t.Fatalf("%s RecordInfo was not written", version.StandardName())
			}
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled %s RecordInfo error = %v", version.StandardName(), err)
			}
		})
	}

	t.Run("2011 rejects IndistinctQuery", func(t *testing.T) {
		api, flow := newRecordQueryWireAPI(t, GBVersion10)
		indistinct := 1
		_, err := api.QueryRecordList(t.Context(), &RecordQueryInput{
			DeviceID: gb10DeviceID, ChannelID: gb10ChannelID,
			Start: 1, End: 2, IndistinctQuery: &indistinct,
		})
		if err == nil || !strings.Contains(err.Error(), "requires GB/T 28181-2014") {
			t.Fatalf("2011 IndistinctQuery error = %v", err)
		}
		select {
		case payload := <-flow.writes:
			t.Fatalf("2011 invalid RecordInfo was written: %s", payload)
		default:
		}
	})

	for _, version := range []GBProtocolVersion{GBVersion20, GBVersion30} {
		t.Run(version.StandardName()+" requires times", func(t *testing.T) {
			api, flow := newRecordQueryWireAPI(t, version)
			_, err := api.QueryRecordList(t.Context(), &RecordQueryInput{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID})
			if err == nil || !strings.Contains(err.Error(), "invalid record query time range") {
				t.Fatalf("%s missing time error = %v", version.StandardName(), err)
			}
			select {
			case payload := <-flow.writes:
				t.Fatalf("%s invalid RecordInfo was written: %s", version.StandardName(), payload)
			default:
			}
		})
	}
}

func newRecordQueryWireAPI(t *testing.T, version GBProtocolVersion) (*GB28181API, *flowConnection) {
	t.Helper()
	flow := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: flow}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.setGBVersion(version)
	memory.persistent.Ext.GBEffectiveVersion = string(version)
	channel := &Channel{ChannelID: gb10ChannelID, device: memory.runtime}
	if err := channel.init("3402000000"); err != nil {
		t.Fatal(err)
	}
	memory.runtime.Channels.Store(gb10ChannelID, channel)
	memory.runtime.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = flow.remote
		device.to = remote
	})
	sipServer := sip.NewServer(local)
	api := &GB28181API{
		recordResponses: newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
	}
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	t.Cleanup(sipServer.Close)
	return api, flow
}

func newDeviceQueryWireAPI(t *testing.T, version GBProtocolVersion) (*GB28181API, *flowConnection) {
	t.Helper()
	api, memory := newVersionGateAPI(version)
	flow := newFlowConnection()
	connection := &tcpFlowConnection{flowConnection: flow}
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	memory.device.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = flow.remote
		device.to = remote
	})
	t.Cleanup(sipServer.Close)
	return api, flow
}

func TestDeviceQuerySkipsActiveSequenceAfterWrap(t *testing.T) {
	api, flow := newDeviceQueryWireAPI(t, GBVersion11)
	api.querySN.Store(0)
	oldKey := buildPendingQueryKey(gb10DeviceID, "DeviceStatus", 1)
	oldPending := &pendingQueryWait{targetID: gb10DeviceID}
	api.pendingDeviceQuery.Store(oldKey, oldPending)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := api.DeviceQuery(ctx, &DeviceQueryInput{
			DeviceID: gb10DeviceID,
			Action:   deviceQueryActionDeviceStatus,
			Timeout:  time.Second,
		})
		done <- err
	}()

	select {
	case payload := <-flow.writes:
		body := string(payload)
		if !strings.Contains(body, "<SN>2</SN>") {
			t.Fatalf("query did not skip occupied SN 1: %s", body)
		}
	case err := <-done:
		t.Fatalf("query returned before write: %v", err)
	case <-time.After(time.Second):
		t.Fatal("query was not written")
	}
	if current, ok := api.pendingDeviceQuery.Load(oldKey); !ok || current != oldPending {
		t.Fatalf("occupied query generation was overwritten: %#v, exists=%v", current, ok)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query error = %v", err)
	}
	if current, ok := api.pendingDeviceQuery.Load(oldKey); !ok || current != oldPending {
		t.Fatalf("old query generation was deleted by new cleanup: %#v, exists=%v", current, ok)
	}
}

func TestRecordQuerySkipsActiveSequenceAfterWrap(t *testing.T) {
	api, flow := newRecordQueryWireAPI(t, GBVersion11)
	api.querySN.Store(0)
	oldKey := buildMultiResponseKey(gb10ChannelID, "RecordInfo", 1)
	oldEntry, started := api.recordResponses.TryStart(oldKey)
	if oldEntry == nil || !started {
		t.Fatal("failed to reserve old RecordInfo generation")
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := api.DeviceQuery(ctx, &DeviceQueryInput{
			DeviceID: gb10DeviceID,
			TargetID: gb10ChannelID,
			Action:   deviceQueryActionRecordInfo,
			Start:    1,
			End:      2,
			Timeout:  time.Second,
		})
		done <- err
	}()

	select {
	case payload := <-flow.writes:
		body := string(payload)
		if !strings.Contains(body, "<SN>2</SN>") {
			t.Fatalf("RecordInfo did not skip occupied SN 1: %s", body)
		}
	case err := <-done:
		t.Fatalf("RecordInfo returned before write: %v", err)
	case <-time.After(time.Second):
		t.Fatal("RecordInfo was not written")
	}
	if !api.recordResponses.Has(oldKey) {
		t.Fatal("occupied RecordInfo generation was overwritten")
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled RecordInfo error = %v", err)
	}
	if !api.recordResponses.Has(oldKey) {
		t.Fatal("old RecordInfo generation was deleted by new cleanup")
	}
	api.recordResponses.CancelEntry(oldKey, oldEntry)
}

func TestAutomaticRegistrationQueriesStopWithServiceLifecycle(t *testing.T) {
	tests := []struct {
		name string
		call func(*GB28181API) error
	}{
		{name: "DeviceInfo", call: func(api *GB28181API) error {
			return api.QueryDeviceInfoContext(context.Background(), gb10DeviceID)
		}},
		{name: "ConfigDownload", call: func(api *GB28181API) error {
			return api.QueryConfigDownloadBasicContext(context.Background(), gb10DeviceID)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, flow := newDeviceQueryWireAPI(t, GBVersion11)
			done := make(chan error, 1)
			go func() { done <- test.call(api) }()
			select {
			case <-flow.writes:
			case err := <-done:
				t.Fatalf("automatic query failed before write: %v", err)
			case <-time.After(time.Second):
				t.Fatal("automatic query was not written")
			}
			api.beginClose()
			select {
			case err := <-done:
				if !errors.Is(err, ErrServiceStopped) {
					t.Fatalf("automatic query close error = %v; want %v", err, ErrServiceStopped)
				}
			case <-time.After(time.Second):
				t.Fatal("automatic query did not stop with service lifecycle")
			}
			api.close()
		})
	}
}

func TestMobilePositionQueryUsesNotifyInsteadOfBusinessResponse(t *testing.T) {
	if deviceQueryWaitsForBusinessResponse("MobilePosition") {
		t.Fatal("MobilePosition query incorrectly waits for a Response that is not defined by the standard")
	}
	if _, ok := genericQueryResponseMinimumVersion("MobilePosition"); ok {
		t.Fatal("MobilePosition incorrectly accepts a business Response envelope")
	}
	for _, cmdType := range []string{"DeviceStatus", "PresetQuery", "ConfigDownload"} {
		if !deviceQueryWaitsForBusinessResponse(cmdType) {
			t.Errorf("%s query stopped waiting for its business response", cmdType)
		}
	}
}

func TestDeviceQueryMobilePositionReturnsAfterSIP200(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	client, peer := net.Pipe()
	connection := sip.NewTCPConnection(client)
	server := &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	api.svr = server
	memory.device.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = connection.RemoteAddr()
		device.to = remote
	})
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = peer.Close()
		sipServer.Close()
	})

	type queryResult struct {
		out *DeviceQueryOutput
		err error
	}
	done := make(chan queryResult, 1)
	go func() {
		out, err := api.DeviceQuery(t.Context(), &DeviceQueryInput{
			DeviceID: gb10DeviceID, Action: deviceQueryActionMobilePosition, Interval: 5, Timeout: 5 * time.Second,
		})
		done <- queryResult{out: out, err: err}
	}()

	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 8192)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	request := string(buffer[:n])
	if !strings.Contains(request, "<CmdType>MobilePosition</CmdType>") || !strings.Contains(request, "<Interval>5</Interval>") {
		t.Fatalf("MobilePosition request = %s", request)
	}
	to := cascadeTestHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=device-mobile-position"
	}
	response := "SIP/2.0 200 OK\r\n" +
		"Via: " + cascadeTestHeader(request, "Via") + "\r\n" +
		"From: " + cascadeTestHeader(request, "From") + "\r\n" +
		"To: " + to + "\r\n" +
		"Call-ID: " + cascadeTestHeader(request, "Call-ID") + "\r\n" +
		"CSeq: " + cascadeTestHeader(request, "CSeq") + "\r\n" +
		"Content-Length: 0\r\n\r\n"
	if _, err := peer.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.out == nil || result.out.CmdType != "MobilePosition" || result.out.DeviceID != gb10DeviceID || result.out.Result != "" || result.out.XML != "" {
			t.Fatalf("MobilePosition acceptance result = %+v", result.out)
		}
	case <-time.After(time.Second):
		t.Fatal("MobilePosition query still waited for a nonexistent business Response after SIP 200")
	}
}

func TestDeviceQueryRejectsNegativeMobilePositionInterval(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion20)
	_, err := api.DeviceQuery(t.Context(), &DeviceQueryInput{
		DeviceID: gb10DeviceID, Action: deviceQueryActionMobilePosition, Interval: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "interval must not be negative") {
		t.Fatalf("negative MobilePosition interval error = %v", err)
	}
}

func TestSubscribeRejectsNegativeInterval(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion20)
	err := api.Subscribe(t.Context(), &SubscribeInput{
		DeviceID: gb10DeviceID, Event: "MobilePosition", Expires: 60, Interval: -1,
	})
	if err == nil || !strings.Contains(err.Error(), "interval must not be negative") {
		t.Fatalf("negative subscription interval error = %v", err)
	}
}

type versionGateMemory struct {
	device *Device
}

func newVersionGateAPI(version GBProtocolVersion) (*GB28181API, *versionGateMemory) {
	memory := &versionGateMemory{device: &Device{IsOnline: true, gbVersion: string(version)}}
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	return api, memory
}

func (m *versionGateMemory) LoadOrStore(string, *Device)             {}
func (m *versionGateMemory) LoadDeviceToMemory(sip.Connection) error { return nil }
func (m *versionGateMemory) RangeDevices(func(string, *Device) bool) {}
func (m *versionGateMemory) Change(string, func(*ipc.Device) error, func(*Device)) error {
	return nil
}
func (m *versionGateMemory) Load(string) (*Device, bool)                { return m.device, true }
func (m *versionGateMemory) Store(_ string, device *Device)             { m.device = device }
func (m *versionGateMemory) GetChannel(string, string) (*Channel, bool) { return nil, false }
