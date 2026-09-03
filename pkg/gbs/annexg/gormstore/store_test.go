package gormstore

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/pkg/gbs/annexg"
	"gorm.io/gorm"
)

func text(value string) *string { return &value }
func boolean(value bool) *bool  { return &value }

var testDatabaseSequence atomic.Uint64

func newTestStore(t *testing.T, options Options) (*Store, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), testDatabaseSequence.Add(1))))
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(db, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store, db
}

func mpRecord(number, alarmTime, deviceID, class string) annexg.MPAlarmRecord {
	return annexg.MPAlarmRecord{
		AlarmNO: number, AlarmTime: alarmTime, DeviceID: deviceID, AlarmClass: text(class),
		AlarmPriority: "1", AlarmMethod: "2", OriginalNO: "original-" + number,
		OriginalInfo: "original", Sender: "sender", Processor: "processor", AlarmLevel: "1",
		Disposal: "pending", AlarmInfo: "alarm",
	}
}

func ecsRecord(number, alarmTime, address string) annexg.ECSAlarmRecord {
	return annexg.ECSAlarmRecord{
		AlarmNO: number, AlarmTime: alarmTime, AlarmPriority: "1", AlarmClass: "2",
		AlarmAddress: address, AlarmMethod: "3", AlarmTelephone: "110", Processor: "processor",
		SrecipientName: "operator", NsStatus: "open", NCallType: "alarm", AlarmInfo: "alarm",
	}
}

func tgsRecord(alarmTime, tollgate, plate, plateType string) annexg.TGSAlarmRecord {
	return annexg.TGSAlarmRecord{
		AlarmTime: alarmTime, TollgateID: tollgate, CarPlate: plate,
		PlateType: plateType, DefenceType: "wanted",
	}
}

func TestMigrateCreatesAnnexGTables(t *testing.T) {
	_, db := newTestStore(t, Options{})
	for _, table := range []string{
		"gb_annex_g_alarm_records",
		"gb_annex_g_defence_states",
		"gb_annex_g_defence_audits",
		"gb_annex_g_pending_exchanges",
	} {
		if !db.Migrator().HasTable(table) {
			t.Errorf("missing table %s", table)
		}
	}
	if !db.Migrator().HasIndex(new(alarmRecordRow), "idx_annex_g_alarm_fingerprint") {
		t.Fatal("missing alarm fingerprint index")
	}
	if !db.Migrator().HasIndex(new(defenceStateRow), "idx_annex_g_defence_identity") {
		t.Fatal("missing defence identity index")
	}
	if !db.Migrator().HasIndex(new(pendingExchangeRow), "idx_annex_g_pending_identity") {
		t.Fatal("missing pending exchange identity index")
	}
}

func TestPendingExchangePersistenceLimitAndExpiry(t *testing.T) {
	store, db := newTestStore(t, Options{PendingTTL: time.Hour, MaxPending: 1})
	ctx := context.Background()
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 41}
	if err := store.SavePendingExchange(ctx, "34020000002000000002", annexg.Version2011, request); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePendingExchange(ctx, "34020000002000000002", annexg.Version2011, request); err != nil {
		t.Fatalf("idempotent save = %v", err)
	}
	items, err := store.LoadPendingExchanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].SystemID != "34020000002000000002" || items[0].Request.CommandType() != annexg.CommandECSAlarmRecordList {
		t.Fatalf("loaded pending exchanges = %#v", items)
	}
	response := &annexg.ECSAlarmRecordListResponse{
		CmdType: annexg.CommandECSAlarmRecordList, SN: 41, Result: annexg.ResultOK,
		RealRecordNum: 0, SendRecordNum: 0,
	}
	if err := store.SavePendingResponse(ctx, "34020000002000000002", annexg.Version2011, response); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePendingResponse(ctx, "34020000002000000002", annexg.Version2011, response); err != nil {
		t.Fatalf("idempotent response save = %v", err)
	}
	items, err = store.LoadPendingExchanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Response == nil || items[0].Response.CommandType() != annexg.CommandECSAlarmRecordList {
		t.Fatalf("loaded pending response = %#v", items)
	}
	different := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 41, AlarmAddressRange: text("different")}
	if err := store.SavePendingExchange(ctx, "34020000002000000002", annexg.Version2011, different); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("different correlation payload error = %v", err)
	}
	second := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 42}
	if err := store.SavePendingExchange(ctx, "34020000002000000002", annexg.Version2011, second); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("pending limit error = %v", err)
	}
	if err := db.Model(new(pendingExchangeRow)).Where("sn = ?", 41).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteExpiredPendingExchanges(ctx, time.Now())
	if err != nil || deleted != 1 {
		t.Fatalf("expired pending delete = %d, %v", deleted, err)
	}
}

func TestPendingExchangeProfileFingerprintBinding(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	ctx := context.Background()
	request := &annexg.AlarmRecordQuery{CmdType: annexg.CommandECSAlarmRecordList, SN: 51}
	first := strings.Repeat("a", 64)
	second := strings.Repeat("b", 64)
	if err := store.SavePendingExchange(ctx, "34020000002000000002", annexg.Version2011, request, first); err != nil {
		t.Fatal(err)
	}
	if err := store.SavePendingExchange(ctx, "34020000002000000002", annexg.Version2011, request, first); err != nil {
		t.Fatalf("same profile idempotent save = %v", err)
	}
	if err := store.SavePendingExchange(ctx, "34020000002000000002", annexg.Version2011, request, second); err == nil || !strings.Contains(err.Error(), "different system profile") {
		t.Fatalf("profile conflict error = %v", err)
	}
	items, err := store.LoadPendingExchanges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ProfileFingerprint != first {
		t.Fatalf("stored profile fingerprint = %#v", items)
	}
}

func TestAlarmRecordSaveQueryAndDeduplication(t *testing.T) {
	store, db := newTestStore(t, Options{MaxSendRecords: 1})
	ctx := context.Background()
	first := mpRecord("mp-1", "2026-08-27T10:00:00", "34020000001320000001", "A")
	second := mpRecord("mp-2", "2026-08-27T11:00:00", "35020000001320000001", "A")
	for _, record := range []annexg.MPAlarmRecord{first, first, second} {
		if err := store.SaveMPAlarmRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	var count int64
	if err := db.Model(new(alarmRecordRow)).Where("kind = ?", "mp").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("stored MP records = %d, want 2", count)
	}

	all, err := store.QueryMPAlarmRecords(ctx, annexg.AlarmRecordQuery{CmdType: annexg.CommandMPAlarmRecordList, SN: 10})
	if err != nil {
		t.Fatal(err)
	}
	if all.RealRecordNum != 2 || all.SendRecordNum != 1 || len(all.RecordList.AlarmRecords) != 1 || all.RecordList.AlarmRecords[0].AlarmNO != "mp-1" {
		t.Fatalf("limited query = %#v", all)
	}
	if err := all.Validate(annexg.Version2011); err != nil {
		t.Fatalf("query response validation = %v", err)
	}

	filtered, err := store.QueryMPAlarmRecords(ctx, annexg.AlarmRecordQuery{
		CmdType: annexg.CommandMPAlarmRecordList, SN: 11,
		AlarmDeviceRange: text("3402"), AlarmClass: text("A"),
		BeginTime: text("2026-08-27T09:00:00+08:00"), EndTime: text("2026-08-27T10:30:00"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.RealRecordNum != 1 || filtered.SendRecordNum != 1 || filtered.RecordList.AlarmRecords[0].AlarmNO != "mp-1" {
		t.Fatalf("filtered query = %#v", filtered)
	}

	escaped, err := store.QueryMPAlarmRecords(ctx, annexg.AlarmRecordQuery{
		CmdType: annexg.CommandMPAlarmRecordList, SN: 12, AlarmDeviceRange: text("3402%"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if escaped.RealRecordNum != 0 || escaped.SendRecordNum != 0 {
		t.Fatalf("escaped wildcard query = %#v", escaped)
	}
}

func TestECSAndTGSQueryFilters(t *testing.T) {
	store, _ := newTestStore(t, Options{MaxSendRecords: 10})
	ctx := context.Background()
	for _, record := range []annexg.ECSAlarmRecord{
		ecsRecord("ecs-1", "2026-08-27T10:00:00", "3402-road"),
		ecsRecord("ecs-2", "2026-08-27T11:00:00", "3502-road"),
	} {
		if err := store.SaveECSAlarmRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SaveTGSAlarmRecord(ctx, tgsRecord("2026-08-27T12:00:00", "gate-1", "浙A12345", "02")); err != nil {
		t.Fatal(err)
	}

	ecs, err := store.QueryECSAlarmRecords(ctx, annexg.AlarmRecordQuery{
		CmdType: annexg.CommandECSAlarmRecordList, SN: 20, AlarmAddressRange: text("3402"), AlarmMethod: text("3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ecs.RealRecordNum != 1 || ecs.RecordList.AlarmRecords[0].AlarmNO != "ecs-1" {
		t.Fatalf("ECS query = %#v", ecs)
	}

	tgs, err := store.QueryTGSAlarmRecords(ctx, annexg.AlarmRecordQuery{
		CmdType: annexg.CommandTGSAlarmRecordList, SN: 21,
		TollgateID: text("gate-1"), CarPlate: text("浙A12345"), PlateType: text("02"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if tgs.RealRecordNum != 1 || tgs.RecordList.AlarmRecords[0].TollgateID != "gate-1" {
		t.Fatalf("TGS query = %#v", tgs)
	}
}

func TestConfigDefenceStateAndAuditAreIdempotent(t *testing.T) {
	store, db := newTestStore(t, Options{})
	ctx := context.Background()
	active := annexg.ConfigDefenceNotify{
		CmdType: annexg.CommandConfigDefence, SN: 30, Type: boolean(true),
		TollgateID: "gate-1", CarPlate: "浙A12345", PlateType: "02", DefenceType: "wanted",
		DefenceTime: "2026-08-27T10:00:00",
	}
	for _, notify := range []annexg.ConfigDefenceNotify{active, func() annexg.ConfigDefenceNotify {
		duplicate := active
		duplicate.SN = 31
		duplicate.XMLName.Local = "Notify"
		return duplicate
	}()} {
		response, err := store.ApplyConfigDefence(ctx, notify)
		if err != nil || response.Result != annexg.ResultOK || response.SN != notify.SN {
			t.Fatalf("ApplyConfigDefence() = %#v, %v", response, err)
		}
	}
	assertDefenceCounts(t, db, 1, 1)
	var state defenceStateRow
	if err := db.First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if !state.Active {
		t.Fatal("active defence was not stored")
	}

	inactive := active
	inactive.SN = 32
	inactive.Type = boolean(false)
	inactive.DefenceTime = "2026-08-27T11:00:00"
	response, err := store.ApplyConfigDefence(ctx, inactive)
	if err != nil || response.Result != annexg.ResultOK {
		t.Fatalf("withdraw defence = %#v, %v", response, err)
	}
	assertDefenceCounts(t, db, 1, 2)
	if err := db.First(&state).Error; err != nil {
		t.Fatal(err)
	}
	if state.Active || state.DefenceTime.Hour() != 11 {
		t.Fatalf("withdrawn state = %#v", state)
	}
}

func TestAlarmAuditQueryFiltersAndPaginatesNewestFirst(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	ctx := context.Background()
	for _, record := range []annexg.MPAlarmRecord{
		mpRecord("mp-1", "2026-08-27T10:00:00", "34020000001320000001", "A"),
		mpRecord("mp-2", "2026-08-27T11:00:00", "34020000001320000002", "A"),
		mpRecord("mp-3", "2026-08-27T12:00:00", "35020000001320000001", "B"),
	} {
		if err := store.SaveMPAlarmRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	begin := time.Date(2026, 8, 27, 9, 30, 0, 0, time.Local)
	end := time.Date(2026, 8, 27, 11, 30, 0, 0, time.Local)
	page, err := store.ListAlarmAudits(ctx, AlarmAuditQuery{
		Kind: "MP", BeginTime: &begin, EndTime: &end, AlarmClass: "A",
		AlarmDeviceRange: "3402", Page: 2, PageSize: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.Page != 2 || page.PageSize != 1 || len(page.Items) != 1 {
		t.Fatalf("alarm audit page = %#v", page)
	}
	if item := page.Items[0]; item.AlarmNO != "mp-1" || item.Kind != "mp" || len(item.Payload) == 0 {
		t.Fatalf("alarm audit item = %#v", item)
	}
}

func TestDefenceAuditQueriesExposeCurrentAndHistory(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	ctx := context.Background()
	active := annexg.ConfigDefenceNotify{
		CmdType: annexg.CommandConfigDefence, SN: 50, Type: boolean(true), TollgateID: "gate-1",
		CarPlate: "浙A12345", PlateType: "02", DefenceType: "wanted", DefenceTime: "2026-08-27T10:00:00",
	}
	if _, err := store.ApplyConfigDefence(ctx, active); err != nil {
		t.Fatal(err)
	}
	inactive := active
	inactive.SN++
	inactive.Type = boolean(false)
	inactive.DefenceTime = "2026-08-27T11:00:00"
	if _, err := store.ApplyConfigDefence(ctx, inactive); err != nil {
		t.Fatal(err)
	}

	states, err := store.ListDefenceStates(ctx, DefenceAuditQuery{TollgateID: "gate-1", Active: boolean(false)})
	if err != nil {
		t.Fatal(err)
	}
	if states.Total != 1 || len(states.Items) != 1 || states.Items[0].Active {
		t.Fatalf("defence states = %#v", states)
	}
	if states.Page != 1 || states.PageSize != defaultAuditPageSize {
		t.Fatalf("defence state pagination = %#v", states)
	}

	audits, err := store.ListDefenceAudits(ctx, DefenceAuditQuery{CarPlate: "浙A12345", PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if audits.Total != 2 || len(audits.Items) != 1 || audits.Items[0].Active {
		t.Fatalf("defence audits = %#v", audits)
	}
}

func TestAuditQueriesRejectInvalidBoundsAndFilters(t *testing.T) {
	store, _ := newTestStore(t, Options{})
	now := time.Now()
	earlier := now.Add(-time.Hour)
	tests := []struct {
		name string
		call func() error
	}{
		{name: "nil context", call: func() error {
			_, err := store.ListAlarmAudits(nil, AlarmAuditQuery{Kind: "mp"})
			return err
		}},
		{name: "unknown alarm kind", call: func() error {
			_, err := store.ListAlarmAudits(context.Background(), AlarmAuditQuery{Kind: "unknown"})
			return err
		}},
		{name: "filter not allowed for kind", call: func() error {
			_, err := store.ListAlarmAudits(context.Background(), AlarmAuditQuery{Kind: "tgs", AlarmClass: "1"})
			return err
		}},
		{name: "reversed time", call: func() error {
			_, err := store.ListDefenceAudits(context.Background(), DefenceAuditQuery{BeginTime: &now, EndTime: &earlier})
			return err
		}},
		{name: "page too large", call: func() error {
			_, err := store.ListDefenceStates(context.Background(), DefenceAuditQuery{PageSize: maximumAuditPageSize + 1})
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func assertDefenceCounts(t *testing.T, db *gorm.DB, states, audits int64) {
	t.Helper()
	var stateCount, auditCount int64
	if err := db.Model(new(defenceStateRow)).Count(&stateCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(new(defenceAuditRow)).Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if stateCount != states || auditCount != audits {
		t.Fatalf("defence counts = states:%d audits:%d, want %d/%d", stateCount, auditCount, states, audits)
	}
}

func TestUnavailableAndInvalidStoresFailClosed(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("nil database constructor succeeded")
	}
	if db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s-options?mode=memory&cache=shared", t.Name()))); err != nil {
		t.Fatal(err)
	} else if _, err := New(db, Options{MaxSendRecords: maximumMaxSendRecords + 1}); err == nil {
		t.Fatal("invalid MaxSendRecords was accepted")
	}
	if err := (*Store)(nil).Migrate(context.Background()); err == nil {
		t.Fatal("nil store migration succeeded")
	}
	store, _ := newTestStore(t, Options{})
	if err := store.SaveMPAlarmRecord(nil, mpRecord("nil-context", "2026-08-27T10:00:00", "3402", "A")); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := store.SaveMPAlarmRecord(context.Background(), annexg.MPAlarmRecord{}); err == nil {
		t.Fatal("invalid MP record was stored")
	}
	response, err := store.ApplyConfigDefence(context.Background(), annexg.ConfigDefenceNotify{SN: 1})
	if err == nil || response.Result != annexg.ResultError {
		t.Fatalf("invalid defence response = %#v, %v", response, err)
	}
}
