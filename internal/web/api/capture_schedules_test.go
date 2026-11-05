package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/capture"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/ixugo/goddd/pkg/web"
)

func TestCaptureSchedulesListsOnlyEnabledExistingChannels(t *testing.T) {
	a, db := captureTestAPI(t)
	a.uc = &Usecase{DB: db}
	device := ipc.Device{ID: "dev1", DeviceID: "34020000001110000202", Name: "录像机 Alpha", Password: "do-not-expose"}
	if err := db.Create(&device).Error; err != nil {
		t.Fatal(err)
	}
	channels := []ipc.Channel{
		{ID: "a", DID: device.ID, DeviceID: device.DeviceID, ChannelID: "34020000001320000001", Name: "A_1%", Type: ipc.TypeGB28181, IsOnline: true},
		{ID: "b", DID: device.ID, DeviceID: device.DeviceID, ChannelID: "34020000001320000002", Name: "B", Type: ipc.TypeGB28181, IsOnline: false},
		{ID: "disabled", DID: device.ID, Name: "停用通道", Type: ipc.TypeGB28181},
	}
	if err := db.Create(&channels).Error; err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"a", "b", "disabled", "deleted", "rtsp_capture"} {
		if err := a.captures.SaveSchedule(t.Context(), &capture.Schedule{ChannelID: channel, Enabled: channel != "disabled", IntervalSeconds: 300}); err != nil {
			t.Fatal(err)
		}
	}
	lastRun := time.Now().Add(-time.Minute).UTC()
	if err := db.Model(&capture.Schedule{}).Where("channel_id = ?", "b").Updates(map[string]any{"last_run_at": lastRun, "last_error": "设备超时"}).Error; err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	registerCaptures(r, a, AuthMiddleware("schedule-test-secret", ""))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/capture_schedules", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatal("列表未鉴权", w.Code)
	}
	token, err := web.NewToken(map[string]any{}, "schedule-test-secret", web.WithExpiresAt(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	get := func(query string) captureSchedulePage {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/capture_schedules"+query, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "do-not-expose") || strings.Contains(w.Body.String(), `"password"`) {
			t.Fatal("泄露设备凭据")
		}
		var result captureSchedulePage
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	all := get("")
	if all.Total != 3 || all.DeviceCount != 1 || all.StandaloneCount != 1 || len(all.Items) != 3 {
		t.Fatal("计划或设备统计错误", all)
	}
	foundOffline := false
	for _, row := range all.Items {
		if !row.Enabled || row.ChannelID == "disabled" || row.ChannelID == "deleted" {
			t.Fatal("返回停用或已删除通道", row)
		}
		if row.ChannelID == "b" {
			foundOffline = true
			if row.IsOnline || row.DeviceID != device.ID || row.DeviceCode != device.DeviceID || row.LastError != "设备超时" || row.LastRunAt == nil {
				t.Fatal("离线通道或执行状态信息丢失", row)
			}
		}
	}
	if !foundOffline {
		t.Fatal("离线但启用的通道被排除")
	}
	first, second := get("?page=1&size=1"), get("?page=2&size=1")
	if first.Total != 3 || second.Total != 3 || len(first.Items) != 1 || len(second.Items) != 1 || first.Items[0].ChannelID == second.Items[0].ChannelID {
		t.Fatal("分页失效", first, second)
	}
	if result := get("?key=alpha"); result.Total != 2 || result.DeviceCount != 1 || result.StandaloneCount != 0 {
		t.Fatal("设备搜索或去重统计错误", result)
	}
	if result := get("?key=34020000001320000002"); result.Total != 1 || result.Items[0].ChannelID != "b" {
		t.Fatal("通道编码搜索失效", result)
	}
	if result := get("?key=_1%25"); result.Total != 1 || result.Items[0].ChannelID != "a" {
		t.Fatal("搜索通配符未按文字处理", result)
	}
	if result := get("?key=missing"); result.Total != 0 || result.Items == nil || len(result.Items) != 0 {
		t.Fatal("空结果错误", result)
	}
	if err := db.Model(&capture.Schedule{}).Where("channel_id = ?", "b").Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if result := get("?key=34020000001320000002"); result.Total != 0 {
		t.Fatal("停用后仍在列表", result)
	}
}
