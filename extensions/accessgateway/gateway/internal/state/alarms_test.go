package state

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

func alarmRecord(n int) protocol.AlarmRecord {
	return protocol.AlarmRecord{ID: fmt.Sprintf("%064x", n), SubjectID: "device", DeviceCode: "34020000001320000001", ChannelCode: "34020000001320000002", BindingRevision: "1", ReceivedAt: time.Now().UTC().Format(time.RFC3339Nano), AlarmTime: "2026-11-12T10:00:00", Description: "移动侦测", Historical: true}
}
func TestAlarmSpoolRestartPaginationAckAndCapacity(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	s, err := Open(dir, "b1", "a")
	if err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 65; n++ {
		if err = s.SaveAlarm(alarmRecord(n)); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SaveAlarm(alarmRecord(1)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(dir, "b1", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	batch, err := s.PullAlarms("")
	if err != nil || len(batch.Items) != 64 || batch.Next == "" {
		t.Fatal(batch, err)
	}
	tail, err := s.PullAlarms(batch.Next)
	if err != nil || len(tail.Items) != 1 || tail.Next != "" {
		t.Fatal(tail, err)
	}
	if !batch.Items[0].Historical || batch.Items[0].Description != "移动侦测" || batch.Items[0].AlarmTime != "2026-11-12T10:00:00" {
		t.Fatal("报警内容或原始时间丢失")
	}
	if _, err = s.AckAlarms([]string{batch.Items[0].ID, "../bad"}); err == nil {
		t.Fatal("接纳非法确认")
	}
	n, _, err := s.AlarmStats()
	if err != nil || n != 65 {
		t.Fatal(n, err)
	}
	if n, err = s.AckAlarms([]string{batch.Items[0].ID}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if n, err = s.AckAlarms([]string{batch.Items[0].ID}); err != nil || n != 0 {
		t.Fatal("ACK 重试不幂等", n, err)
	}
	// 空间预算超出时，不能成功接纳新报警，也不能挤掉尚未确认记录。
	oversized := filepath.Join(dir, "alarms", strings.Repeat("f", 64)+".json")
	f, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(AlarmBytesLimit)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveAlarm(alarmRecord(66)); err == nil {
		t.Fatal("空间不足仍接纳报警")
	}
}
func TestAlarmSpoolRejectsSymlinkAndCorruption(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	s, err := Open(dir, "b1", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	outside := t.TempDir()
	if err = os.Remove(filepath.Join(dir, "alarms")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "alarms")); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveAlarm(alarmRecord(1)); err == nil {
		t.Fatal("允许越界报警目录")
	}
	_ = os.Remove(filepath.Join(dir, "alarms"))
	_ = os.Mkdir(filepath.Join(dir, "alarms"), 0700)
	file := filepath.Join(dir, "alarms", alarmRecord(1).ID+".json")
	if err = os.WriteFile(file, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveAlarm(alarmRecord(2)); err != nil {
		t.Fatal(err)
	}
	batch, err := s.PullAlarms("")
	if err != nil || !batch.StoreError || len(batch.Items) != 1 || batch.Items[0].ID != alarmRecord(2).ID {
		t.Fatal("损坏记录阻塞后续或未上报", batch, err)
	}
	if _, err = os.Stat(file); err != nil {
		t.Fatal("损坏报警被静默删除")
	}
}

func TestAlarmSpoolCleansUncommittedTempsOnRestart(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0700)
	s, err := Open(dir, "b1", "a")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveAlarm(alarmRecord(1)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	tmp := filepath.Join(dir, "alarms", alarmRecord(2).ID+".json.tmp")
	if err = os.WriteFile(tmp, []byte("uncommitted"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, "b1", "a")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("未清理未提交文件", err)
	}
	batch, err := s.PullAlarms("")
	if err != nil || len(batch.Items) != 1 {
		t.Fatal(batch, err)
	}
}
