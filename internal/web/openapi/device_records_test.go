package openapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

type deviceRecordHost struct {
	op.Host
	query func(context.Context, string, *ipc.RecordQueryInput) (*op.DeviceRecords, error)
}

func (h deviceRecordHost) QueryDeviceRecords(ctx context.Context, channel string, in *ipc.RecordQueryInput) (*op.DeviceRecords, error) {
	return h.query(ctx, channel, in)
}

const recordQueryBody = `{"start_at":1710864000,"end_at":1710950400}`
const recordQueryPath = codePath + "/records/query"

func TestDeviceRecordQueryCodesAndInternalID(t *testing.T) {
	r, s, p, key, oldHost := codeFixture(t)
	if err := s.DB.Model(&ipc.Device{}).Where("id = ?", "d1").Updates(map[string]any{"type": ipc.TypeGB28181, "is_online": true}).Error; err != nil {
		t.Fatal(err)
	}
	// 只授予录像读取，编码入口不应额外要求设备/通道读取权限。
	s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("scopes", `["recording:read"]`)
	calls := 0
	s.Host = deviceRecordHost{Host: oldHost, query: func(ctx context.Context, channel string, in *ipc.RecordQueryInput) (*op.DeviceRecords, error) {
		calls++
		deadline, ok := ctx.Deadline()
		if channel != "c1" || in.StartAt != 1710864000 || in.EndAt != 1710950400 || in.Timeout != 10 || in.Type != "all" || !ok || time.Until(deadline) > 15*time.Second {
			t.Fatalf("incorrect query: %s %+v deadline=%v", channel, in, deadline)
		}
		return &op.DeviceRecords{Complete: true, RecordQueryOutput: &ipc.RecordQueryOutput{DayTotal: 1, TimeNum: 1, Data: []ipc.RecordDate{{Date: "2024-03-20", Items: []ipc.RecordSegment{{Start: 1710864000, End: 1710867600}}}}}}, nil
	}}
	for _, path := range []string{recordQueryPath, "/openapi/v1/channels/c1/records/query"} {
		out := codeRequest(r, p, key, http.MethodPost, path, "", recordQueryBody, "")
		var body struct {
			Data op.DeviceRecords `json:"data"`
		}
		if err := json.Unmarshal(out.Body.Bytes(), &body); err != nil || out.Code != 200 || !body.Data.Complete || body.Data.TimeNum != 1 || body.Data.Data[0].Items[0].End != 1710867600 || strings.Contains(out.Body.String(), "MUST_NOT_LEAK") {
			t.Fatal(out.Code, out.Body.String(), err)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestDeviceRecordQueryValidationAndAuthorization(t *testing.T) {
	r, s, p, key, oldHost := codeFixture(t)
	s.DB.Model(&ipc.Device{}).Where("id = ?", "d1").Updates(map[string]any{"type": ipc.TypeGB28181, "is_online": true})
	calls := 0
	s.Host = deviceRecordHost{Host: oldHost, query: func(context.Context, string, *ipc.RecordQueryInput) (*op.DeviceRecords, error) {
		calls++
		return nil, op.ErrUnavailable
	}}
	for _, tc := range []struct {
		path, body, code string
		status           int
	}{
		{recordQueryPath, `{}`, "INVALID_TIME_RANGE", 400},
		{recordQueryPath, `{"start_at":1710950400,"end_at":1710864000}`, "INVALID_TIME_RANGE", 400},
		{recordQueryPath, `{"start_at":1710864000,"end_at":1711555200}`, "INVALID_TIME_RANGE", 400},
		{recordQueryPath, `{"start_at":1710864000,"end_at":1710950400,"timeout":0}`, "INVALID_RECORD_TIMEOUT", 400},
		{recordQueryPath, `{"start_at":1710864000,"end_at":1710950400,"timeout":61}`, "INVALID_RECORD_TIMEOUT", 400},
		{recordQueryPath, `{"start_at":1710864000,"end_at":1710950400,"type":"unknown"}`, "INVALID_RECORD_TYPE", 400},
		{recordQueryPath, `{"start_at":1710864000,"end_at":1710950400,"channel_id":"c2"}`, "INVALID_JSON", 400},
		{strings.Replace(recordQueryPath, codeDevice, otherDevice, 1), recordQueryBody, "NOT_FOUND", 404},
		{"/openapi/v1/channels/c2/records/query", recordQueryBody, "NOT_FOUND", 404},
		{strings.Replace(recordQueryPath, codeChannel, "unknown", 1), recordQueryBody, "NOT_FOUND", 404},
	} {
		out := codeRequest(r, p, key, http.MethodPost, tc.path, "", tc.body, "")
		if out.Code != tc.status || !strings.Contains(out.Body.String(), tc.code) {
			t.Fatal(tc, out.Code, out.Body.String())
		}
	}
	for _, tc := range []struct {
		field  string
		value  any
		code   string
		status int
	}{
		{"type", ipc.TypeOnvif, "UNSUPPORTED_RECORD_QUERY", 400},
		{"type", ipc.TypeGB28181, "", 0},
		{"is_online", false, "DEVICE_OFFLINE", 409},
		{"is_online", true, "", 0},
	} {
		s.DB.Model(&ipc.Device{}).Where("id = ?", "d1").Update(tc.field, tc.value)
		if tc.status == 0 {
			continue
		}
		out := codeRequest(r, p, key, http.MethodPost, recordQueryPath, "", recordQueryBody, "")
		if out.Code != tc.status || !strings.Contains(out.Body.String(), tc.code) {
			t.Fatal(out.Code, out.Body.String())
		}
	}
	s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("scopes", `["channel:read"]`)
	out := codeRequest(r, p, key, http.MethodPost, recordQueryPath, "", recordQueryBody, "")
	if out.Code != 403 || calls != 0 {
		t.Fatal(out.Code, out.Body.String(), calls)
	}
}

func TestDeviceRecordQueryPartialRevocationAndLimits(t *testing.T) {
	r, s, p, key, oldHost := codeFixture(t)
	s.DB.Model(&ipc.Device{}).Where("id = ?", "d1").Updates(map[string]any{"type": ipc.TypeGB28181, "is_online": true})
	calls := 0
	s.Host = deviceRecordHost{Host: oldHost, query: func(_ context.Context, _ string, in *ipc.RecordQueryInput) (*op.DeviceRecords, error) {
		calls++
		if calls == 2 {
			// 查询过程中撤回设备权限，已收到的目录也不能返回。
			s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("device_ids", `[]`)
		}
		return &op.DeviceRecords{RecordQueryOutput: &ipc.RecordQueryOutput{TimeNum: 1, Data: []ipc.RecordDate{}}, Incomplete: &op.DeviceRecordIncomplete{ReceivedCount: 1, ExpectedCount: 2}}, nil
	}}
	for _, want := range []int{200, 404} {
		out := codeRequest(r, p, key, http.MethodPost, recordQueryPath, "", recordQueryBody, "")
		if out.Code != want || want == 200 && (!strings.Contains(out.Body.String(), `"complete":false`) || !strings.Contains(out.Body.String(), `"expected_count":2`)) || want == 404 && strings.Contains(out.Body.String(), "expected_count") {
			t.Fatal(out.Code, out.Body.String())
		}
	}
	s.DB.Model(&op.App{}).Where("id = ?", p.App.ID).Update("device_ids", `["d1"]`)
	for i := 0; i < 2; i++ {
		release, err := s.AcquireConnection(p, "device-record-query")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	out := codeRequest(r, p, key, http.MethodPost, recordQueryPath, "", recordQueryBody, "")
	if out.Code != 429 || calls != 2 {
		t.Fatal(out.Code, out.Body.String(), calls)
	}
	// 新接口依旧要求AK/SK签名。
	unsigned := httptest.NewRecorder()
	r.ServeHTTP(unsigned, httptest.NewRequest(http.MethodPost, recordQueryPath, strings.NewReader(recordQueryBody)))
	if unsigned.Code != 401 {
		t.Fatal(unsigned.Code, unsigned.Body.String())
	}
}
