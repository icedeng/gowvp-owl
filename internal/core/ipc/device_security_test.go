package ipc_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
)

func TestDeviceJSONOmitsRegistrationPassword(t *testing.T) {
	data, err := json.Marshal(ipc.Device{ID: "GB_secret", Password: "registration-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "registration-secret") || strings.Contains(string(data), `"password"`) {
		t.Fatalf("device JSON exposed registration password: %s", data)
	}
}

func TestDeviceJSONOmitsRegisterTransactionMetadataButDatabaseValueKeepsIt(t *testing.T) {
	ext := ipc.DeviceExt{
		Manufacturer:                 "vendor",
		GBRegisterCallID:             "register-call-id",
		GBRegisterCSeq:               42,
		GBRegisterRequestFingerprint: "request-fingerprint",
		GBRegisterResponseDate:       "2026-08-30T01:02:03.004",
		GBRegisterResponseConfirmed:  true,
	}
	apiJSON, err := json.Marshal(ipc.Device{ID: "GB_register_metadata", Ext: ext})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"gb_register_call_id", "gb_register_cseq", "gb_register_request_fingerprint",
		"gb_register_response_date", "gb_register_response_confirmed", "register-call-id", "request-fingerprint",
	} {
		if strings.Contains(string(apiJSON), forbidden) {
			t.Fatalf("device API JSON exposed REGISTER transaction metadata %q: %s", forbidden, apiJSON)
		}
	}
	if !strings.Contains(string(apiJSON), `"manufacturer":"vendor"`) {
		t.Fatalf("device API JSON lost public ext fields: %s", apiJSON)
	}

	value, err := ext.Value()
	if err != nil {
		t.Fatal(err)
	}
	storedJSON, ok := value.([]byte)
	if !ok {
		t.Fatalf("DeviceExt.Value type = %T, want []byte", value)
	}
	for _, required := range []string{
		`"gb_register_call_id":"register-call-id"`, `"gb_register_cseq":42`,
		`"gb_register_request_fingerprint":"request-fingerprint"`,
		`"gb_register_response_date":"2026-08-30T01:02:03.004"`,
		`"gb_register_response_confirmed":true`,
	} {
		if !strings.Contains(string(storedJSON), required) {
			t.Fatalf("database DeviceExt JSON lost %s: %s", required, storedJSON)
		}
	}

	var restored ipc.DeviceExt
	if err := restored.Scan(storedJSON); err != nil {
		t.Fatal(err)
	}
	if restored.GBRegisterCallID != ext.GBRegisterCallID || restored.GBRegisterCSeq != ext.GBRegisterCSeq ||
		restored.GBRegisterRequestFingerprint != ext.GBRegisterRequestFingerprint ||
		restored.GBRegisterResponseDate != ext.GBRegisterResponseDate ||
		restored.GBRegisterResponseConfirmed != ext.GBRegisterResponseConfirmed {
		t.Fatalf("database DeviceExt round trip = %+v, want %+v", restored, ext)
	}
}

func TestEditDevicePasswordPatchSemantics(t *testing.T) {
	for _, test := range []struct {
		name         string
		requestBody  string
		wantPassword string
	}{
		{
			name:         "omitted password preserves current credential",
			requestBody:  `{"device_id":"34020000001320000008","name":"after"}`,
			wantPassword: "old-password",
		},
		{
			name:         "explicit empty password clears device credential",
			requestBody:  `{"device_id":"34020000001320000008","name":"after","password":""}`,
			wantPassword: "",
		},
		{
			name:         "non-empty password replaces current credential",
			requestBody:  `{"device_id":"34020000001320000008","name":"after","password":"new-password"}`,
			wantPassword: "new-password",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := openIPCTestDatabase(t)
			store := ipcdb.NewDB(db).AutoMigrate(true)
			device := &ipc.Device{
				ID: "GB_password_patch", DeviceID: "34020000001320000008", Type: ipc.TypeGB28181,
				Name: "before", Password: "old-password",
			}
			if err := store.Device().Create(t.Context(), device); err != nil {
				t.Fatal(err)
			}

			var input ipc.EditDeviceInput
			if err := json.Unmarshal([]byte(test.requestBody), &input); err != nil {
				t.Fatal(err)
			}
			core := ipc.NewCore(store, uniqueid.Core{}, map[string]ipc.Protocoler{})
			if _, err := core.EditDevice(t.Context(), &input, device.ID); err != nil {
				t.Fatal(err)
			}

			var stored ipc.Device
			if err := store.Device().Get(t.Context(), &stored, orm.Where("id = ?", device.ID)); err != nil {
				t.Fatal(err)
			}
			if stored.Password != test.wantPassword {
				t.Fatalf("stored password = %q, want %q", stored.Password, test.wantPassword)
			}
		})
	}
}
