package openapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

func TestDeviceStatusSubscriptionSelectorsAndPatch(t *testing.T) {
	r, s, p, key, _ := codeFixture(t)
	if err := s.DB.Create(&op.CallbackTarget{ID: "status-target", AppID: p.App.ID, Status: "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	body := `{"callback_target_id":"status-target","event_types":["device.online","device.offline"],"device_codes":["` + codeDevice + `","` + codeDevice + `"]}`
	out := codeRequest(r, p, key, "POST", "/openapi/v1/subscriptions", "", body, "device-status-create")
	var response struct {
		Data op.Subscription `json:"data"`
	}
	if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &response) != nil || len(response.Data.DeviceIDs) != 1 || response.Data.DeviceIDs[0] != "d1" || len(response.Data.EventTypes) != 2 || response.Data.Status != "paused" {
		t.Fatal(out.Code, out.Body.String())
	}
	path := "/openapi/v1/subscriptions/" + response.Data.ID
	for _, tc := range []struct {
		extra string
		code  int
	}{
		{`"device_codes":[],"device_ids":[]`, 400},
		{`"device_codes":null`, 400},
		{`"device_ids":null`, 400},
		{`"event_types":null`, 400},
		{`"event_types":["device.up"]`, 400},
		{`"device_codes":["` + otherDevice + `"]`, 404},
		{`"device_codes":["missing"]`, 404},
		{`"device_ids":["d2"]`, 404},
		{`"channel_ids":["c1"]`, 400},
		{`"labels":["person"]`, 400},
	} {
		out = codeRequest(r, p, key, "PATCH", path, "", `{"revision":1,`+tc.extra+`}`, "")
		if out.Code != tc.code {
			t.Fatal(tc.extra, out.Code, out.Body.String())
		}
	}
	// 未传类型的 PATCH 不能把设备订阅变成旧告警订阅；空设备数组只清除附加筛选。
	out = codeRequest(r, p, key, "PATCH", path, "", `{"revision":1,"device_codes":[]}`, "")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"device_ids":[]`) || !strings.Contains(out.Body.String(), `"event_types":["device.online","device.offline"]`) {
		t.Fatal(out.Code, out.Body.String())
	}
	out = codeRequest(r, p, key, "PATCH", path, "", `{"revision":2,"event_types":[],"device_ids":["d1"]}`, "")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"event_types":[]`) {
		t.Fatal(out.Code, out.Body.String())
	}
	// 设备按相同编码重建后，订阅继续绑定原内部 ID，不自动继承新资源。
	if err := s.DB.Delete(&ipc.Device{}, "id = ?", "d1").Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Create(&ipc.Device{ID: "replacement", DeviceID: codeDevice}).Error; err != nil {
		t.Fatal(err)
	}
	out = codeRequest(r, p, key, "GET", path, "", "", "")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"device_ids":["d1"]`) {
		t.Fatal(out.Code, out.Body.String())
	}
	out = codeRequest(r, p, key, "PATCH", path, "", `{"revision":3,"device_codes":["`+codeDevice+`"]}`, "")
	if out.Code != 404 {
		t.Fatal("同码重建继承了设备授权", out.Code, out.Body.String())
	}
}

func TestDeviceStatusSubscriptionRequiresDeviceRead(t *testing.T) {
	r, s, p, key := apiFixture(t)
	if err := s.DB.Create(&op.CallbackTarget{ID: "target", AppID: p.App.ID, Status: "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("scopes", `["event:subscribe"]`).Error; err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		types string
		code  int
	}{
		{`["device.online"]`, 403},
		{`["event.created"]`, 200},
	} {
		out := codeRequest(r, p, key, "POST", "/openapi/v1/subscriptions", "", `{"callback_target_id":"target","event_types":`+tc.types+`}`, fmt.Sprintf("status-scope-%d", i))
		if out.Code != tc.code {
			t.Fatal(out.Code, out.Body.String())
		}
	}
}
