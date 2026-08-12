package sms

import (
	"encoding/json"
	"testing"
)

// TestStreamLiveAddrJSONFieldNames 保证播放地址同时兼容新旧字段名。
func TestStreamLiveAddrJSONFieldNames(t *testing.T) {
	raw, err := json.Marshal(StreamLiveAddr{FLV: "http", WSFLV: "websocket"})
	if err != nil {
		t.Fatalf("序列化播放地址失败: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解析播放地址失败: %v", err)
	}
	for _, key := range []string{"flv", "ws-flv", "http_flv", "ws_flv"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("播放地址缺少字段 %q", key)
		}
	}
	if payload["flv"] != payload["http_flv"] || payload["ws-flv"] != payload["ws_flv"] {
		t.Fatalf("新旧播放地址字段值不一致: %v", payload)
	}
}

func TestStreamLiveAddrLegacyHTTPFLV(t *testing.T) {
	raw, err := json.Marshal(StreamLiveAddr{HTTPFLV: "legacy"})
	if err != nil {
		t.Fatalf("序列化旧字段失败: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解析旧字段失败: %v", err)
	}
	if payload["flv"] != "legacy" || payload["http_flv"] != "legacy" {
		t.Fatalf("旧 HTTPFLV 字段未映射到新旧 JSON 字段: %v", payload)
	}
}
