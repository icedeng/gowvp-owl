package state

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

type TransferResult struct {
	CleanupReserve string               `json:"cleanup_reserve,omitempty"`
	PlatformID     string               `json:"platform_id"`
	TaskID         string               `json:"task_id"`
	TransferID     string               `json:"transfer_id"`
	Ref            protocol.ResourceRef `json:"ref"`
	Received       protocol.Decimal     `json:"received"`
	Sent           protocol.Decimal     `json:"sent"`
	SHA256         *string              `json:"sha256"`
	Committed      bool                 `json:"committed"`
	Phase          string               `json:"phase"`
	State          string               `json:"state"`
	ExpiresAt      time.Time            `json:"expires_at"`
}
type Data struct {
	AlarmSpoolEnabled bool                      `json:"alarm_spool_enabled,omitempty"`
	Version           int                       `json:"version"`
	GatewayID         string                    `json:"gateway_id"`
	PlatformID        string                    `json:"platform_id"`
	OwnerEpoch        protocol.Decimal          `json:"owner_epoch"`
	ClientInstanceID  string                    `json:"client_instance_id"`
	AppliedRevision   protocol.Decimal          `json:"applied_revision"`
	AppliedConfig     *protocol.EffectiveConfig `json:"applied_config"`
	ConfigSHA256      string                    `json:"config_sha256"`
	HadResources      bool                      `json:"had_resources"`
	MaxLeaseTTLMS     int                       `json:"max_lease_ttl_ms"`
	Ports             map[string]time.Time      `json:"ports"`
	Transfers         []TransferResult          `json:"transfers"`
}
type Store struct {
	mu   sync.Mutex
	root *os.Root
	data Data
}

func Open(dir, gateway, platform string) (*Store, error) {
	path, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(path, 0700); err != nil {
		return nil, err
	}
	leaf, err := os.Lstat(path)
	if err != nil || leaf.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("state directory must not be a symlink")
	}
	// 规范化操作系统目录别名（如 macOS 的 /var），后续文件访问始终锚定 os.Root。
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	path = resolved
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("state directory must have mode 0700")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	s := &Store{root: root, data: Data{Version: 1, GatewayID: gateway, PlatformID: platform, OwnerEpoch: "0", AppliedRevision: "0", Ports: map[string]time.Time{}, Transfers: []TransferResult{}}}
	if info, err := root.Lstat("state.json"); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			root.Close()
			return nil, fmt.Errorf("state file must be regular and private")
		}
		file, e := root.Open("state.json")
		if e != nil {
			root.Close()
			return nil, e
		}
		data, e := io.ReadAll(io.LimitReader(file, 16<<20+1))
		file.Close()
		if e == nil && len(data) > 16<<20 {
			e = fmt.Errorf("state file too large")
		}
		if e == nil {
			e = json.Unmarshal(data, &s.data)
		}
		if e != nil {
			root.Close()
			return nil, e
		}
		if s.data.Version != 1 || s.data.GatewayID != gateway || s.data.PlatformID != platform {
			root.Close()
			return nil, fmt.Errorf("persisted gateway identity/schema mismatch")
		}
		if _, e = s.data.OwnerEpoch.Int64(); e != nil {
			root.Close()
			return nil, e
		}
		if s.data.Ports == nil {
			s.data.Ports = map[string]time.Time{}
		}
	} else if !os.IsNotExist(err) {
		root.Close()
		return nil, err
	}
	if err := s.cleanAlarmTemps(); err != nil {
		root.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.root.Close() }
func clone(v Data) Data {
	data, _ := json.Marshal(v)
	var result Data
	_ = json.Unmarshal(data, &result)
	return result
}
func (s *Store) Snapshot() Data { s.mu.Lock(); defer s.mu.Unlock(); return clone(s.data) }

// Update 先原子持久化，再发布内存状态；失败不能对外确认已应用。
func (s *Store) Update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.data)
	if e := fn(&next); e != nil {
		return e
	}
	data, e := json.Marshal(next)
	if e != nil {
		return e
	}
	// 先验证可被当前 schema 重新读取，避免写入无法恢复的零值/非法十进制字段。
	var validated Data
	if e := json.Unmarshal(data, &validated); e != nil {
		return fmt.Errorf("invalid recovery journal: %w", e)
	}
	if len(data) > 16<<20 {
		return protocol.Failure(protocol.Exhausted, "gateway recovery journal byte budget exceeded")
	}
	name := ".state-" + uuid.NewString()
	file, e := s.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	defer s.root.Remove(name)
	if _, e = file.Write(data); e == nil {
		e = file.Sync()
	}
	closeErr := file.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = s.root.Rename(name, "state.json"); e != nil {
		return e
	}
	dir, e := s.root.Open(".")
	if e != nil {
		return e
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return e
	}
	s.data = next
	return nil
}
