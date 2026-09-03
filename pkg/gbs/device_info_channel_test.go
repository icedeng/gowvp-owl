package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestDeviceInfoResponseAcceptsKnownChildChannel(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	connection := newFlowConnection()
	runtimeDevice := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtimeChannel := &Channel{ChannelID: persistentChannel.ChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	memory := &cascadeFlowMemory{
		flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice},
		channel:    runtimeChannel,
	}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api

	sn := 901
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", sn), pending)
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>901</SN><DeviceID>` + persistentChannel.ChannelID + `</DeviceID><DeviceName>Gate IPC</DeviceName><Result>OK</Result><Manufacturer>Vendor A</Manufacturer><Model>M-1</Model><Firmware>3.2.1</Firmware><Info><doorType><DeviceID>` + persistentChannel.ChannelID + `</DeviceID></doorType></Info></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "device-info-channel", body)
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("device-info-channel", connection), DeviceID: persistentDevice.DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), Log: slog.Default(),
	}
	api.sipMessageDeviceInfo(ctx)
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("DeviceInfo response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo response timeout")
	}
	select {
	case output := <-pending.wait:
		if output.DeviceID != persistentChannel.ChannelID || output.Result != "OK" {
			t.Fatalf("pending DeviceInfo output = %+v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("channel DeviceInfo did not resolve pending query")
	}

	var updated ipc.Channel
	if err := adapter.Store().Channel().Get(t.Context(), &updated, orm.Where("channel_id = ?", persistentChannel.ChannelID)); err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Gate IPC" || updated.Ext.Manufacturer != "Vendor A" || updated.Ext.Model != "M-1" || updated.Ext.Firmware != "3.2.1" {
		t.Fatalf("updated channel DeviceInfo = %+v", updated)
	}
	channelState, ok := api.GetQueryState(persistentChannel.ChannelID)
	if !ok || len(channelState.AppendixA4) != 1 || channelState.AppendixA4[0].Type != "doorType" {
		t.Fatalf("channel DeviceInfo Appendix A.4 state = %+v", channelState)
	}
	var parent ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &parent, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if parent.Ext.Manufacturer != "" || parent.Ext.Model != "" || parent.Ext.Firmware != "" {
		t.Fatalf("child DeviceInfo overwrote parent = %+v", parent.Ext)
	}
	if len(parent.Ext.GBAppendixA4) != 1 || parent.Ext.GBAppendixA4[0].Type != "doorType" {
		t.Fatalf("child DeviceInfo Appendix A.4 was not persisted on parent: %+v", parent.Ext.GBAppendixA4)
	}
}

func TestDeviceInfoResponseDistinguishesMissingAndExplicitEmptyMetadata(t *testing.T) {
	adapter, persistentDevice, persistentChannel := newCascadeMediaCore(t)
	if err := adapter.Update(persistentDevice.DeviceID, func(device *ipc.Device) {
		device.Ext.Name = "old device"
		device.Ext.Manufacturer = "old manufacturer"
		device.Ext.Model = "old model"
		device.Ext.Firmware = "old firmware"
	}); err != nil {
		t.Fatal(err)
	}
	var seededChannel ipc.Channel
	if err := adapter.Store().Channel().Update(t.Context(), &seededChannel, func(channel *ipc.Channel) error {
		channel.Name = "old channel"
		channel.Ext.Name = "old channel"
		channel.Ext.Manufacturer = "old manufacturer"
		channel.Ext.Model = "old model"
		channel.Ext.Firmware = "old firmware"
		return nil
	}, orm.Where("device_id = ? AND channel_id = ?", persistentDevice.DeviceID, persistentChannel.ChannelID)); err != nil {
		t.Fatal(err)
	}

	runtimeDevice := &Device{IsOnline: true, gbVersion: string(GBVersion30)}
	runtimeChannel := &Channel{ChannelID: persistentChannel.ChannelID, device: runtimeDevice}
	runtimeChannel.init("local.example")
	runtimeDevice.Channels.Store(persistentChannel.ChannelID, runtimeChannel)
	memory := &cascadeFlowMemory{
		flowMemory: &flowMemory{persistent: persistentDevice, runtime: runtimeDevice},
		channel:    runtimeChannel,
	}
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server}
	server.gb = api

	send := func(sn int, targetID, fields string) {
		t.Helper()
		api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", sn), &pendingQueryWait{
			wait: make(chan *DeviceQueryOutput, 1), targetID: targetID,
		})
		body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>` + fmt.Sprint(sn) + `</SN><DeviceID>` + targetID +
			`</DeviceID>` + fields + `<Result>OK</Result></Response>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, fmt.Sprintf("device-info-presence-%d", sn), body, api.sipMessageDeviceInfo)
		assertFlowOK(t, response)
	}

	// 字段缺省必须保留已有值。
	send(910, persistentDevice.DeviceID, "")
	send(911, persistentChannel.ChannelID, "")
	var device ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &device, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	var channel ipc.Channel
	if err := adapter.Store().Channel().Get(t.Context(), &channel, orm.Where("channel_id = ?", persistentChannel.ChannelID)); err != nil {
		t.Fatal(err)
	}
	if device.Ext.Name != "old device" || device.Ext.Manufacturer != "old manufacturer" || device.Ext.Model != "old model" || device.Ext.Firmware != "old firmware" {
		t.Fatalf("missing DeviceInfo metadata cleared device values: %+v", device.Ext)
	}
	if channel.Name != "old channel" || channel.Ext.Name != "old channel" || channel.Ext.Manufacturer != "old manufacturer" || channel.Ext.Model != "old model" || channel.Ext.Firmware != "old firmware" {
		t.Fatalf("missing DeviceInfo metadata cleared channel values: %+v", channel)
	}

	// string 元素显式出现但内容为空时，必须能清除旧值；不能与字段缺省混为一谈。
	emptyFields := `<DeviceName></DeviceName><Result>OK</Result><Manufacturer></Manufacturer><Model></Model><Firmware></Firmware>`
	sendWithResult := func(sn int, targetID string) {
		t.Helper()
		api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", sn), &pendingQueryWait{
			wait: make(chan *DeviceQueryOutput, 1), targetID: targetID,
		})
		body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>` + fmt.Sprint(sn) + `</SN><DeviceID>` + targetID + `</DeviceID>` + emptyFields + `</Response>`)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, fmt.Sprintf("device-info-empty-%d", sn), body, api.sipMessageDeviceInfo)
		assertFlowOK(t, response)
	}
	sendWithResult(912, persistentDevice.DeviceID)
	sendWithResult(913, persistentChannel.ChannelID)
	if err := adapter.Store().Device().Get(t.Context(), &device, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if err := adapter.Store().Channel().Get(t.Context(), &channel, orm.Where("channel_id = ?", persistentChannel.ChannelID)); err != nil {
		t.Fatal(err)
	}
	if device.Ext.Name != "" || device.Ext.Manufacturer != "" || device.Ext.Model != "" || device.Ext.Firmware != "" {
		t.Fatalf("explicit empty DeviceInfo metadata did not clear device values: %+v", device.Ext)
	}
	if channel.Name != "" || channel.Ext.Name != "" || channel.Ext.Manufacturer != "" || channel.Ext.Model != "" || channel.Ext.Firmware != "" {
		t.Fatalf("explicit empty DeviceInfo metadata did not clear channel values: %+v", channel)
	}
}

func TestDeviceInfoResponseDoesNotOverwriteRegistrationRoute(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	if err := adapter.Update(persistentDevice.DeviceID, func(device *ipc.Device) {
		device.Address = "198.51.100.10:5060"
		device.Transport = "tcp"
	}); err != nil {
		t.Fatal(err)
	}
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	connection := newFlowConnection()
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: persistentDevice.DeviceID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", 905), pending)
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>905</SN><DeviceID>` + persistentDevice.DeviceID +
		`</DeviceID><DeviceName>Updated NVR</DeviceName><Result>OK</Result><Manufacturer>Vendor B</Manufacturer></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "device-info-registration-route", body)
	api.sipMessageDeviceInfo(&sip.Context{
		Request: request, Tx: sip.NewTransaction("device-info-registration-route", connection), DeviceID: persistentDevice.DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), Log: slog.Default(),
	})
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("DeviceInfo response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo response timeout")
	}

	var updated ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &updated, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if updated.Ext.Name != "Updated NVR" || updated.Ext.Manufacturer != "Vendor B" {
		t.Fatalf("DeviceInfo metadata was not persisted: %+v", updated.Ext)
	}
	if updated.Address != "198.51.100.10:5060" || updated.Transport != "tcp" {
		t.Fatalf("DeviceInfo overwrote registration route: address=%q transport=%q", updated.Address, updated.Transport)
	}
}

func TestDeviceInfoPersistenceStopsWithServiceLifecycle(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion11)
	lifecycleCtx, cancel := context.WithCancel(t.Context())
	cancel()
	connection := newFlowConnection()
	server := &Server{memoryStorer: memory}
	api := &GB28181API{core: adapter, svr: server, lifecycleCtx: lifecycleCtx}
	server.gb = api
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1), targetID: persistentDevice.DeviceID}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", 906), pending)
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>906</SN><DeviceID>` + persistentDevice.DeviceID +
		`</DeviceID><DeviceName>stale name</DeviceName><Result>OK</Result><Manufacturer>stale vendor</Manufacturer></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "device-info-canceled-persistence", body)
	api.sipMessageDeviceInfo(&sip.Context{
		Request: request, Tx: sip.NewTransaction("device-info-canceled-persistence", connection), DeviceID: persistentDevice.DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), Log: slog.Default(),
	})
	select {
	case response := <-connection.writes:
		if !strings.Contains(string(response), "SIP/2.0 200 OK") {
			t.Fatalf("DeviceInfo response = %s", response)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo response timeout")
	}

	var stored ipc.Device
	if err := adapter.Store().Device().Get(t.Context(), &stored, orm.Where("device_id = ?", persistentDevice.DeviceID)); err != nil {
		t.Fatal(err)
	}
	if stored.Ext.Name == "stale name" || stored.Ext.Manufacturer == "stale vendor" {
		t.Fatalf("canceled DeviceInfo persistence updated device: %+v", stored.Ext)
	}
}

func TestDeviceInfoResponseRejectsInvalidEnvelopeBeforeWait(t *testing.T) {
	api, _ := newVersionGateAPI(GBVersion10)
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceInfo", 903), pending)
	tests := []struct {
		name string
		body string
	}{
		{name: "wrong root", body: `<Query><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Query>`},
		{name: "notify root", body: `<Notify><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Notify>`},
		{name: "wrong command", body: `<Response><CmdType>DeviceStatus</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "non-positive SN", body: `<Response><CmdType>DeviceInfo</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "missing result", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Response>`},
		{name: "invalid result", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>SUCCESS</Result></Response>`},
		{name: "unknown target", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>34020000001320000009</DeviceID><Result>OK</Result></Response>`},
		{name: "2011 device name", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><DeviceName>new field</DeviceName><Result>OK</Result></Response>`},
		{name: "negative channel", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Channel>-1</Channel></Response>`},
		{name: "negative max camera", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><MaxCamera>-1</MaxCamera></Response>`},
		{name: "negative max alarm", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><MaxAlarm>-1</MaxAlarm></Response>`},
		{name: "duplicate SN", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><SN>904</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "duplicate result", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Result>ERROR</Result></Response>`},
		{name: "unknown field", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result><Vendor>1</Vendor></Response>`},
		{name: "root attribute", body: `<Response vendor="x"><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result>OK</Result></Response>`},
		{name: "element attribute", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result vendor="x">OK</Result></Response>`},
		{name: "nested result", body: `<Response><CmdType>DeviceInfo</CmdType><SN>903</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Result><Value>OK</Value></Result></Response>`},
		{name: "out of order", body: `<Response><CmdType>DeviceInfo</CmdType><DeviceID>` + gb10DeviceID + `</DeviceID><SN>903</SN><Result>OK</Result></Response>`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-info-invalid-"+test.name, []byte(test.body), api.sipMessageDeviceInfo)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid DeviceInfo response = %s", response)
			}
		})
	}
	select {
	case output := <-pending.wait:
		t.Fatalf("invalid DeviceInfo resolved pending query: %+v", output)
	default:
	}
}

func TestDeviceInfoResponseAcceptsVersionFields(t *testing.T) {
	for _, test := range []struct {
		version GBProtocolVersion
		name    string
	}{
		{version: GBVersion10},
		{version: GBVersion11, name: "camera"},
		{version: GBVersion20, name: "camera"},
	} {
		t.Run(string(test.version), func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			body := `<Response><CmdType>DeviceInfo</CmdType><SN>904</SN><DeviceID>` + gb10DeviceID + `</DeviceID>`
			if test.name != "" {
				body += `<DeviceName>` + test.name + `</DeviceName>`
			}
			body += `<Result>ERROR</Result><DeviceType>DVR</DeviceType><MaxCamera>0</MaxCamera><MaxAlarm>0</MaxAlarm><Channel>0</Channel></Response>`
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "device-info-version-fields", []byte(body), api.sipMessageDeviceInfo)
			assertFlowOK(t, response)
		})
	}
}

func TestDeviceInfoResponse2022RejectsRemovedCompatibilityFields(t *testing.T) {
	for _, field := range []string{
		`<DeviceType>DVR</DeviceType>`,
		`<MaxCamera>1</MaxCamera>`,
		`<MaxAlarm>1</MaxAlarm>`,
	} {
		t.Run(field, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion30)
			body := `<Response><CmdType>DeviceInfo</CmdType><SN>904</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><DeviceName>camera</DeviceName><Result>ERROR</Result>` + field + `<Channel>0</Channel></Response>`
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"device-info-2022-removed-field", []byte(body), api.sipMessageDeviceInfo)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("protocol 3.0 accepted removed DeviceInfo field %s: %s", field, response)
			}
		})
	}
}

func TestDeviceInfoResponseRejectsInvalidStructureByVersion(t *testing.T) {
	tests := []struct {
		name    string
		version GBProtocolVersion
		body    string
	}{
		{
			name: "2014 DeviceName after Result", version: GBVersion11,
			body: `<Response><CmdType>DeviceInfo</CmdType><SN>910</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>OK</Result><DeviceName>camera</DeviceName></Response>`,
		},
		{
			name: "2014 duplicate DeviceName", version: GBVersion11,
			body: `<Response><CmdType>DeviceInfo</CmdType><SN>910</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><DeviceName>one</DeviceName><DeviceName>two</DeviceName><Result>OK</Result></Response>`,
		},
		{
			name: "2022 Info after ExtraInfo", version: GBVersion30,
			body: `<Response><CmdType>DeviceInfo</CmdType><SN>910</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>ERROR</Result><ExtraInfo>plain</ExtraInfo><Info><doorType><DeviceID>` + gb10DeviceID +
				`</DeviceID></doorType></Info></Response>`,
		},
		{
			name: "2022 nested ExtraInfo", version: GBVersion30,
			body: `<Response><CmdType>DeviceInfo</CmdType><SN>910</SN><DeviceID>` + gb10DeviceID +
				`</DeviceID><Result>ERROR</Result><ExtraInfo><Value>plain</Value></ExtraInfo></Response>`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(test.version)
			pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
			api.pendingDeviceQuery.Store(buildPendingQueryKey(gb10DeviceID, "DeviceInfo", 910), pending)
			response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
				"device-info-invalid-structure-"+test.name, []byte(test.body), api.sipMessageDeviceInfo)
			if !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("invalid DeviceInfo response = %s", response)
			}
			select {
			case output := <-pending.wait:
				t.Fatalf("invalid DeviceInfo resolved pending query: %+v", output)
			default:
			}
		})
	}
}

func TestDeviceInfoExtensionInfoVersionMatrix(t *testing.T) {
	legacyBody := `<Response><CmdType>DeviceInfo</CmdType><SN>905</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>ERROR</Result><Info> legacy </Info><Info>second</Info></Response>`
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		api, _ := newVersionGateAPI(version)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
			"device-info-legacy-info-"+string(version), []byte(legacyBody), api.sipMessageDeviceInfo)
		assertFlowOK(t, response)
	}
	api, _ := newVersionGateAPI(GBVersion30)
	if response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
		"device-info-modern-legacy-info", []byte(legacyBody), api.sipMessageDeviceInfo); !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("protocol 3.0 accepted legacy DeviceInfo Info: %s", response)
	}

	modernBody := `<Response><CmdType>DeviceInfo</CmdType><SN>906</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Result>ERROR</Result><ExtraInfo>plain</ExtraInfo></Response>`
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20} {
		api, _ := newVersionGateAPI(version)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
			"device-info-modern-info-"+string(version), []byte(modernBody), api.sipMessageDeviceInfo)
		if !strings.Contains(response, "SIP/2.0 400") {
			t.Fatalf("protocol %s accepted DeviceInfo ExtraInfo: %s", version, response)
		}
	}

	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	memory := newFlowMemory(persistentDevice.DeviceID)
	memory.runtime.setGBVersion(GBVersion30)
	api = &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	for index, body := range []string{
		modernBody,
		`<Response><CmdType>DeviceInfo</CmdType><SN>907</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><Result>ERROR</Result><Info><doorType><DeviceID>` + gb10DeviceID +
			`</DeviceID></doorType></Info></Response>`,
	} {
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
			fmt.Sprintf("device-info-modern-valid-%d", index), []byte(body), api.sipMessageDeviceInfo)
		assertFlowOK(t, response)
	}

	for index, body := range []string{
		`<Response><CmdType>DeviceInfo</CmdType><SN>908</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><Result>ERROR</Result><Info>` + strings.Repeat("测", 1025) + `</Info></Response>`,
		`<Response><CmdType>DeviceInfo</CmdType><SN>909</SN><DeviceID>` + gb10DeviceID +
			`</DeviceID><Result>ERROR</Result><ExtraInfo>` + strings.Repeat("测", 1025) + `</ExtraInfo></Response>`,
	} {
		version := GBVersion20
		if index == 1 {
			version = GBVersion30
		}
		api, _ := newVersionGateAPI(version)
		response := runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage,
			fmt.Sprintf("device-info-extension-too-long-%d", index), []byte(body), api.sipMessageDeviceInfo)
		if !strings.Contains(response, "SIP/2.0 400") {
			t.Fatalf("protocol %s accepted oversized DeviceInfo extension: %s", version, response)
		}
	}
}

func TestDeviceInfoErrorResolvesPendingQuery(t *testing.T) {
	adapter, persistentDevice, _ := newCascadeMediaCore(t)
	connection := newFlowConnection()
	memory := newFlowMemory(persistentDevice.DeviceID)
	api := &GB28181API{core: adapter, svr: &Server{memoryStorer: memory}}
	sn := 902
	pending := &pendingQueryWait{wait: make(chan *DeviceQueryOutput, 1)}
	api.pendingDeviceQuery.Store(buildPendingQueryKey(persistentDevice.DeviceID, "DeviceInfo", sn), pending)
	body := []byte(`<Response><CmdType>DeviceInfo</CmdType><SN>902</SN><DeviceID>` + persistentDevice.DeviceID + `</DeviceID><Result>ERROR</Result></Response>`)
	request := newFlowRequest(t, connection, sip.MethodMessage, "device-info-error", body)
	api.sipMessageDeviceInfo(&sip.Context{
		Request: request, Tx: sip.NewTransaction("device-info-error", connection), DeviceID: persistentDevice.DeviceID,
		Source: connection.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), Log: slog.Default(),
	})
	select {
	case output := <-pending.wait:
		if output.Result != "ERROR" {
			t.Fatalf("DeviceInfo error output = %+v", output)
		}
	case <-time.After(time.Second):
		t.Fatal("DeviceInfo ERROR did not resolve pending query")
	}
}
