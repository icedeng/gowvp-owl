package state

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

const AlarmLimit = 4096
const AlarmBytesLimit = 32 << 20

// 独立文件避免每条报警重写主状态；目录和记录均受 os.Root 限制。
func (s *Store) alarmFiles() ([]os.DirEntry, error) {
	if err := s.root.Mkdir("alarms", 0700); err != nil && !os.IsExist(err) {
		return nil, err
	}
	info, err := s.root.Lstat("alarms")
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("invalid alarm spool directory")
	}
	dir, err := s.root.Open("alarms")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
func (s *Store) AlarmStats() (int, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alarmStats()
}
func (s *Store) alarmStats() (int, int64, error) {
	entries, err := s.alarmFiles()
	if err != nil {
		return 0, 0, err
	}
	n := 0
	var size int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return 0, 0, fmt.Errorf("invalid alarm spool entry")
		}
		n++
		size += info.Size()
	}
	return n, size, nil
}
func (s *Store) SaveAlarm(a protocol.AlarmRecord) error {
	if err := protocol.ValidateAlarm(a); err != nil {
		return err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if len(data) > 16<<10 {
		return fmt.Errorf("alarm record too large")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n, size, err := s.alarmStats()
	if err != nil {
		return err
	}
	path := "alarms/" + a.ID + ".json"
	if info, statErr := s.root.Lstat(path); statErr == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("invalid existing alarm")
		}
		file, openErr := s.root.Open(path)
		if openErr != nil {
			return openErr
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 16<<10+1))
		file.Close()
		var existing protocol.AlarmRecord
		if readErr != nil || len(raw) > 16<<10 || json.Unmarshal(raw, &existing) != nil || protocol.ValidateAlarm(existing) != nil || existing.ID != a.ID {
			return fmt.Errorf("corrupt existing alarm; not acknowledged")
		}
		return s.syncAlarms()
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	if n >= AlarmLimit || size+int64(len(data)) > AlarmBytesLimit {
		return protocol.Failure(protocol.Exhausted, "报警补传队列已满，未确认设备报警")
	}
	tmp := path + ".tmp"
	f, err := s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		if err = s.root.Remove(tmp); err != nil {
			return err
		}
		f, err = s.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	}
	if err != nil {
		return err
	}
	defer s.root.Remove(tmp)
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = s.root.Rename(tmp, path); err != nil {
		return err
	}
	return s.syncAlarms()
}
func (s *Store) syncAlarms() error {
	d, e := s.root.Open("alarms")
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (s *Store) PullAlarms(after string) (protocol.AlarmBatch, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := protocol.AlarmBatch{Items: []protocol.AlarmRecord{}}
	entries, err := s.alarmFiles()
	if err != nil {
		return out, err
	}
	inspected := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json") || name <= after+".json" {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		if !protocol.ValidHash(id) {
			out.StoreError = true
			continue
		}
		a, e := s.readAlarm(name, entry)
		if e != nil {
			out.StoreError = true
		} else {
			out.Items = append(out.Items, a)
		}
		inspected++
		if inspected == 64 {
			out.Next = id
			break
		}
	}

	return out, nil
}

// A 只有业务事务提交后才确认；删除和目录同步失败均保留至少一次投递语义。
func (s *Store) AckAlarms(ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range ids {
		if !protocol.ValidHash(id) {
			return n, fmt.Errorf("invalid alarm id")
		}
	}
	for _, id := range ids {
		err := s.root.Remove("alarms/" + id + ".json")
		if err != nil && !os.IsNotExist(err) {
			return n, err
		}
		if err == nil {
			n++
		}
	}
	return n, s.syncAlarms()
}

// 损坏记录保留待人工恢复，后面的正常报警继续补传；错误由批次显式上报。
func (s *Store) readAlarm(name string, entry os.DirEntry) (protocol.AlarmRecord, error) {
	var a protocol.AlarmRecord
	info, err := entry.Info()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return a, fmt.Errorf("invalid alarm file")
	}
	file, err := s.root.Open("alarms/" + name)
	if err != nil {
		return a, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 16<<10+1))
	if err != nil || len(raw) > 16<<10 {
		return a, fmt.Errorf("alarm read failed")
	}
	if err = json.Unmarshal(raw, &a); err != nil {
		return a, err
	}
	if err = protocol.ValidateAlarm(a); err != nil || a.ID != strings.TrimSuffix(name, ".json") {
		return a, fmt.Errorf("invalid stored alarm")
	}
	return a, nil
}

// 仅启动时清理尚未提交的临时文件，它们从未向设备确认成功。
func (s *Store) cleanAlarmTemps() error {
	entries, err := s.alarmFiles()
	if err != nil {
		return err
	}
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".json.tmp") || !protocol.ValidHash(strings.TrimSuffix(name, ".json.tmp")) {
			continue
		}
		info, e := entry.Info()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("invalid alarm temporary file")
		}
		if e = s.root.Remove("alarms/" + name); e != nil {
			return e
		}
		changed = true
	}
	if changed {
		return s.syncAlarms()
	}
	return nil
}
