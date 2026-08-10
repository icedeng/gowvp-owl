package gbs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
	"github.com/ixugo/goddd/pkg/conc"
)

// TestGB11SupplementSimulationFlow 串联验证 1.1 设备的主要上报和业务响应闭环。
func TestGB11SupplementSimulationFlow(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	defer func() { config = previousConfig }()

	conn := newFlowConnection()
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	defer sipServer.Close()
	memory := newFlowMemory(gb10DeviceID)
	memory.persistent.Ext.GBEffectiveVersion = string(GBVersion11)
	memory.persistent.Ext.GBDeclaredVersion = string(GBVersion11)
	memory.persistent.Ext.GBVersionSource = "header"
	memory.runtime.setGBVersion(GBVersion11)
	api := &GB28181API{
		cfg:              &conf.SIP{ID: gb10PlatformID, Domain: "3402000000"},
		catalogResponses: newMultiResponseCollector(func(item Channels) string { return item.ChannelID }),
		recordResponses:  newMultiResponseCollector(func(item RecordItem) string { return item.FilePath }),
		streams:          &conc.Map[string, *Streams]{},
	}
	server := &Server{
		Server:       sipServer,
		gb:           api,
		fromAddress:  *platform,
		memoryStorer: memory,
	}
	api.svr = server

	// 设备心跳：状态更新不能覆盖已协商的 1.1 档案。
	response := runFlowHandler(t, conn, api, sip.MethodMessage, "gb11-keepalive", readGB11Fixture(t, "keepalive.xml"), api.sipMessageKeepalive)
	assertFlowOK(t, response)
	if !memory.persistent.IsOnline || memory.persistent.Ext.GBEffectiveVersion != string(GBVersion11) {
		t.Fatalf("keepalive state = online:%v version:%q", memory.persistent.IsOnline, memory.persistent.Ext.GBEffectiveVersion)
	}

	// 目录扩展：响应被接受并进入统一多响应收集器。
	response = runFlowHandler(t, conn, api, sip.MethodMessage, "gb11-catalog", readGB11Fixture(t, "catalog-tree-response.xml"), api.sipMessageCatalog)
	assertFlowOK(t, response)

	// 配置应答：保存结构化结果和原始 XML，并唤醒等待方。
	pending := &pendingDeviceConfig{wait: make(chan *DeviceConfigResponse, 1)}
	api.pendingDeviceConfig.Store(buildPendingDeviceConfigKey(gb10DeviceID, 31), pending)
	response = runFlowHandler(t, conn, api, sip.MethodMessage, "gb11-config", readGB11Fixture(t, "device-config-basic-response.xml"), api.handleDeviceConfig)
	assertFlowOK(t, response)
	state, ok := api.GetQueryState(gb10DeviceID)
	if !ok || state.DeviceConfig == nil || !strings.Contains(state.DeviceConfig.RawXML, "VendorResult") {
		t.Fatalf("device config state = %+v", state)
	}

	// MediaStatus/121：历史会话幂等结束。
	key := historyKey(historyModeDownload, gb10DeviceID, gb10ChannelID)
	stream := &Streams{DeviceID: gb10DeviceID, ChannelID: gb10ChannelID, CallID: "gb11-media"}
	api.streams.Store(key, stream)
	mediaBody := readGB11Fixture(t, "media-status-notify.xml")
	response = runFlowHandler(t, conn, api, sip.MethodMessage, "gb11-media", mediaBody, api.sipMessageMediaStatus)
	assertFlowOK(t, response)
	if _, ok := api.streams.Load(key); ok || !stream.Stop {
		t.Fatalf("media status did not finish stream: %+v", stream)
	}

	// Broadcast Response：业务应答按 TargetID/SN 关联等待方。
	broadcastPending := &pendingBroadcastResponse{wait: make(chan *broadcastResponse, 1)}
	api.pendingBroadcast.Store(buildPendingBroadcastKey(gb10ChannelID, 60), broadcastPending)
	response = runFlowHandler(t, conn, api, sip.MethodMessage, "gb11-broadcast", readGB11Fixture(t, "broadcast-response.xml"), api.sipMessageBroadcastResponse)
	assertFlowOK(t, response)
	select {
	case result := <-broadcastPending.wait:
		if result.Result != "OK" {
			t.Fatalf("broadcast result = %+v", result)
		}
	default:
		t.Fatal("broadcast response did not resolve pending request")
	}
}

func readGB11Fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "gb28181", "1.1", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
