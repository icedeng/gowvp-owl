package openapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/event"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestResourceCodesAllChannelResourceLists(t *testing.T) {
	r, s, p, key, _ := codeFixture(t)
	if err := s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Updates(map[string]any{"qps": 1000, "burst": 1000}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.DB.AutoMigrate(&recording.Recording{}, &event.Event{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	for i, ch := range []string{"c1", "c2"} {
		rows := []any{
			&recording.Recording{ID: int64(101 + i), CID: ch, StartedAt: orm.Time{Time: now}, Path: "MUST_NOT_LEAK"},
			&event.Event{ID: int64(101 + i), CID: ch, StartedAt: orm.Time{Time: now}, ImagePath: "MUST_NOT_LEAK", Label: "person"},
			&op.Session{ID: fmt.Sprint(101 + i), AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: ch, CreatedAt: now, Source: op.Source{BaseURL: "MUST_NOT_LEAK"}},
			&op.Snapshot{ID: fmt.Sprint(101 + i), AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: ch, CreatedAt: now, Path: "MUST_NOT_LEAK"},
			&op.Command{ID: fmt.Sprint(101 + i), AppID: p.App.ID, CredentialID: p.Credential.ID, ChannelID: ch, CreatedAt: now},
		}
		for _, row := range rows {
			if err := s.DB.Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	// 同一通道的其他应用记录不可见，设备级告警只出现在设备列表中。
	for _, row := range []any{
		&op.Session{ID: "other-app", AppID: "another", ChannelID: "c1", CreatedAt: now},
		&op.Snapshot{ID: "other-app", AppID: "another", ChannelID: "c1", CreatedAt: now},
		&op.Command{ID: "other-app", AppID: "another", ChannelID: "c1", CreatedAt: now},
		&event.Event{ID: 103, DID: "d2", CID: "c1", StartedAt: orm.Time{Time: now}},
		&event.Event{ID: 104, DID: "d1", StartedAt: orm.Time{Time: now}},
	} {
		if err := s.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	devicePath := "/openapi/v1/devices/by-code/" + codeDevice
	check := func(path, query string, want int) string {
		t.Helper()
		out := codeRequest(r, p, key, "GET", path, query, "", "")
		var response struct {
			Data struct {
				Items []map[string]any `json:"items"`
				Next  *string          `json:"next_cursor"`
			} `json:"data"`
		}
		if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &response) != nil || len(response.Data.Items) != want || strings.Contains(out.Body.String(), "MUST_NOT_LEAK") {
			t.Fatal(path, query, out.Code, out.Body.String())
		}
		if want > 0 && fmt.Sprint(response.Data.Items[0]["id"]) != "101" {
			t.Fatal(path, response.Data.Items)
		}
		if response.Data.Next != nil {
			return *response.Data.Next
		}
		return ""
	}
	for _, resource := range []string{"recordings", "events", "live-sessions", "snapshot-tasks", "ptz-commands"} {
		root := "/openapi/v1/" + resource
		check(root, "device_code="+codeDevice+"&channel_code="+codeChannel, 1)
		check(root, "channel_id=c1&device_code="+otherDevice, 0)
		check(root, "device_code=unknown", 0)
		check(root, "device_code="+otherDevice, 0)
		check(root, "channel_code="+codeChannel, 1)
		check(codePath+"/"+resource, "", 1)
		count := 1
		if resource == "events" {
			count = 2
		}
		check(devicePath+"/"+resource, "", count)
		out := codeRequest(r, p, key, "GET", codePath+"/"+resource, "device_code="+otherDevice, "", "")
		if out.Code != 400 {
			t.Fatal("path/query conflict", resource, out.Code, out.Body.String())
		}
		out = codeRequest(r, p, key, "GET", root, "device_code=", "", "")
		if out.Code != 400 {
			t.Fatal("empty filter expanded selection", resource, out.Code)
		}
	}
	out := codeRequest(r, p, key, "GET", devicePath+"/channels", "", "", "")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"id":"c1"`) || strings.Contains(out.Body.String(), `"id":"c2"`) {
		t.Fatal(out.Code, out.Body.String())
	}
	cursor := check(devicePath+"/events", "size=1", 1)
	if cursor == "" {
		t.Fatal("expected next cursor")
	}
	out = codeRequest(r, p, key, "GET", codePath+"/events", "cursor="+cursor+"&size=1", "", "")
	if out.Code != 400 {
		t.Fatal("cursor reused across code paths", out.Code)
	}
	if err := s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("scopes", `["snapshot:capture"]`).Error; err != nil {
		t.Fatal(err)
	}
	check(codePath+"/snapshot-tasks", "", 1)
	out = codeRequest(r, p, key, "GET", codePath+"/recordings", "", "", "")
	if out.Code != 403 {
		t.Fatal("scope bypass", out.Code)
	}
}

func TestResourceCodesBodySelectorsAndSubscriptions(t *testing.T) {
	r, s, p, key, h := codeFixture(t)
	s.Start(context.Background())
	t.Cleanup(s.Close)
	selector := `"device_code":"` + codeDevice + `","channel_code":"` + codeChannel + `"`
	for _, tc := range []struct{ path, body string }{
		{"/live-sessions", `{` + selector + `,"protocol":"hls"}`},
		{"/snapshot-tasks", `{` + selector + `}`},
	} {
		out := codeRequest(r, p, key, "POST", "/openapi/v1"+tc.path, "", tc.body, "codebody"+tc.path)
		if out.Code != 200 || !strings.Contains(out.Body.String(), `"channel_id":"c1"`) {
			t.Fatal(out.Code, out.Body.String())
		}
		for _, body := range []string{`{"channel_id":"c2",` + selector + `}`, `{"device_code":"` + codeDevice + `"}`, `{"channel_code":"` + codeChannel + `"}`} {
			out = codeRequest(r, p, key, "POST", "/openapi/v1"+tc.path, "", body, "bad"+tc.path)
			if out.Code != 400 {
				t.Fatal("mixed or partial selector", out.Code, out.Body.String())
			}
		}
	}
	if err := s.DB.Create(&op.CallbackTarget{ID: "target", AppID: p.App.ID, Status: "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	body := `{"callback_target_id":"target","channel_codes":[{` + selector + `}]}`
	out := codeRequest(r, p, key, "POST", "/openapi/v1/subscriptions", "", body, "subscription-codes")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"channel_ids":["c1"]`) {
		t.Fatal(out.Code, out.Body.String())
	}
	var response struct {
		Data op.Subscription `json:"data"`
	}
	json.Unmarshal(out.Body.Bytes(), &response)
	for _, tc := range []struct {
		extra  string
		status int
	}{
		{`"channel_codes":[{"device_code":"` + otherDevice + `","channel_code":"` + codeChannel + `"}]`, 404},
		{`"channel_codes":[],"channel_ids":[]`, 400},
		{`"channel_codes":null`, 400},
		{`"channel_codes":[{"device_code":"` + codeDevice + `"}]`, 400},
		{`"channel_codes":[{` + selector + `,"typo":"value"}]`, 400},
	} {
		out = codeRequest(r, p, key, "PATCH", "/openapi/v1/subscriptions/"+response.Data.ID, "", `{"revision":1,`+tc.extra+`}`, "")
		if out.Code != tc.status {
			t.Fatal(tc.extra, out.Code, out.Body.String())
		}
	}
	out = codeRequest(r, p, key, "PATCH", "/openapi/v1/subscriptions/"+response.Data.ID, "", `{"revision":1,"channel_codes":[]}`, "")
	if out.Code != 200 || !strings.Contains(out.Body.String(), `"channel_ids":[]`) {
		t.Fatal(out.Code, out.Body.String())
	}
	s.Close()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.started) != 1 || h.started[0] != "c1" || len(h.captured) != 1 || h.captured[0] != "c1" {
		t.Fatal(h.started, h.captured)
	}
}
