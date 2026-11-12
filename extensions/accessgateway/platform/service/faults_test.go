package service

import (
	"context"
	"errors"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/migrations"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"testing"
	"time"
)

func TestFaultDebounceRestartRetryAndRecovery(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	defer sql.Close()
	if err = migrations.Apply(db); err != nil {
		t.Fatal(err)
	}
	g := store.Gateway{Base: store.Base{ID: "gatewaypk"}, StableID: "b1", Name: "B", DesiredConfig: core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig()}, AdminState: "enabled"}
	if err = db.Create(&g).Error; err != nil {
		t.Fatal(err)
	}
	s := &Service{repo: store.New(db)}
	ctx := context.Background()
	now := time.Now().UTC()
	calls := 0
	incident := ""
	fail := true
	notify := func(_ context.Context, _, _, id, _ string, recovered bool) error {
		calls++
		if id == "" {
			t.Fatal("故障没有稳定身份")
		}
		if incident == "" {
			incident = id
		} else if incident != id {
			t.Fatal("重启重试改变了事件身份")
		}
		if fail {
			return errors.New("notification not committed")
		}
		return nil
	}
	if err = s.observeFault(ctx, g, "disconnected", true, now, notify); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("首次采样没有防抖")
	}
	if err = s.observeFault(ctx, g, "disconnected", true, now.Add(31*time.Second), notify); err == nil {
		t.Fatal("通知失败却记录成功")
	}
	fail = false
	s = &Service{repo: store.New(db)}
	if err = s.observeFault(ctx, g, "disconnected", true, now.Add(32*time.Second), notify); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	_ = s.observeFault(ctx, g, "disconnected", true, now.Add(50*time.Second), notify)
	if calls != 2 {
		t.Fatal("持续故障重复通知")
	}
	_ = s.observeFault(ctx, g, "disconnected", false, now.Add(51*time.Second), notify)
	if calls != 2 {
		t.Fatal("恢复未防抖")
	}
	if err = s.observeFault(ctx, g, "disconnected", false, now.Add(72*time.Second), notify); err != nil {
		t.Fatal(err)
	}
	var fault store.Fault
	_ = db.First(&fault).Error
	if fault.Active || fault.IncidentID != "" || calls != 3 {
		t.Fatal(fault, calls)
	}
}

func TestFaultRecoveryAfterCreateAcknowledgementWasLost(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sql, _ := db.DB()
	defer sql.Close()
	if err = migrations.Apply(db); err != nil {
		t.Fatal(err)
	}
	g := store.Gateway{Base: store.Base{ID: "g"}, StableID: "b", AdminState: "enabled", DesiredConfig: core.DesiredConfig{EffectiveConfig: protocol.DefaultEffectiveConfig()}}
	if err = db.Create(&g).Error; err != nil {
		t.Fatal(err)
	}
	s := &Service{repo: store.New(db)}
	at := time.Now().UTC()
	ctx := t.Context()
	created, resolved := false, false
	lost := true
	notify := func(_ context.Context, _, _, _, _ string, recovered bool) error {
		if !recovered {
			created = true
			if lost {
				return errors.New("reply lost")
			}
			return nil
		}
		if !created {
			t.Fatal("recovery before first event")
		}
		resolved = true
		return nil
	}
	if err = s.observeFault(ctx, g, "disconnected", true, at, notify); err != nil {
		t.Fatal(err)
	}
	if err = s.observeFault(ctx, g, "disconnected", true, at.Add(31*time.Second), notify); err == nil {
		t.Fatal("lost acknowledgement not detected")
	}
	lost = false
	if err = s.observeFault(ctx, g, "disconnected", false, at.Add(32*time.Second), notify); err != nil {
		t.Fatal(err)
	}
	if err = s.observeFault(ctx, g, "disconnected", false, at.Add(53*time.Second), notify); err != nil {
		t.Fatal(err)
	}
	var f store.Fault
	db.First(&f)
	if !resolved || f.Active || f.IncidentID != "" {
		t.Fatal("unresolved stale incident", f)
	}
}
