package recording

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/system"
	"gorm.io/gorm"
)

const LocalNode = "local"
const DefaultVhost = "__defaultVhost__"
const StorageMarker = ".gowvp-recording-node"

// NodeStorage 区分 ZLM 内部根目录与平台可访问的同一目录，不能混用两种路径。
type NodeStorage struct{ Root, Mount string }
type StorageProvider interface {
	RecordingStorage(context.Context, string) (NodeStorage, error)
}

func nodeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return LocalNode
	}
	return id
}
func normalizeVhost(vhost string) string {
	if vhost == "" {
		return DefaultVhost
	}
	return vhost
}
func StreamKey(vhost, app, stream string) string {
	if normalizeVhost(vhost) == DefaultVhost {
		return app + "/" + stream
	}
	return vhost + "/" + app + "/" + stream
}
func (info ChannelInfo) NodeID() string    { return nodeID(info.MediaServerID) }
func (info ChannelInfo) StreamKey() string { return StreamKey(info.Vhost, info.App, info.Stream) }

func (c Core) StorageFor(ctx context.Context, id string) (NodeStorage, error) {
	id = nodeID(id)
	var storage NodeStorage
	if c.storageProvider != nil {
		var err error
		storage, err = c.storageProvider.RecordingStorage(ctx, id)
		if err != nil {
			return storage, err
		}
	} else if id != LocalNode {
		return storage, fmt.Errorf("媒体节点 %s 的录像存储未配置", id)
	}
	if id == LocalNode && c.conf != nil {
		if storage.Root == "" {
			storage.Root = c.conf.StorageDir
		}
		if storage.Mount == "" {
			storage.Mount = c.conf.StorageDir
		}
	}
	if storage.Root == "" {
		return storage, fmt.Errorf("请配置媒体节点 %s 的 ZLM 录像目录", id)
	}
	if !filepath.IsAbs(storage.Root) {
		storage.Root = filepath.Join(system.Getwd(), storage.Root)
	}
	storage.Root = filepath.Clean(storage.Root)
	if storage.Mount != "" {
		if !filepath.IsAbs(storage.Mount) {
			storage.Mount = filepath.Join(system.Getwd(), storage.Mount)
		}
		storage.Mount = filepath.Clean(storage.Mount)
	}
	return storage, nil
}

// DescribeFile 只用所属节点的目录校验上报路径；远程回调不检查本机是否存在该文件。
func (c Core) DescribeFile(ctx context.Context, id, vhost, file string, size int64) (AddRecordingInput, error) {
	id = nodeID(id)
	if strings.ContainsAny(file, "\x00\r\n") {
		return AddRecordingInput{}, fmt.Errorf("录像路径包含非法字符")
	}
	storage, err := c.StorageFor(ctx, id)
	if err != nil {
		return AddRecordingInput{}, err
	}
	full := filepath.Clean(file)
	if !filepath.IsAbs(full) {
		if id != LocalNode {
			return AddRecordingInput{}, fmt.Errorf("远程录像须上报 ZLM 绝对路径")
		}
		full, err = c.ResolvePath(file)
		if err != nil {
			return AddRecordingInput{}, err
		}
	}
	if full == storage.Root || ensurePathBelowRoot(storage.Root, full) != nil || !strings.EqualFold(filepath.Ext(full), ".mp4") {
		return AddRecordingInput{}, fmt.Errorf("录像路径不在节点目录内或不是 MP4 文件")
	}
	rel, err := filepath.Rel(storage.Root, full)
	if err != nil {
		return AddRecordingInput{}, err
	}
	in := AddRecordingInput{MediaServerID: id, Vhost: normalizeVhost(vhost), StorageRoot: storage.Root, FilePath: full, Path: filepath.ToSlash(rel), Size: size}
	if id == LocalNode {
		rec := Recording{MediaServerID: id, StorageRoot: in.StorageRoot, FilePath: in.FilePath, Path: in.Path}
		local, err := c.ResolveRecordingPath(ctx, &rec)
		if err != nil {
			return AddRecordingInput{}, err
		}
		info, err := os.Stat(local)
		if err != nil || !info.Mode().IsRegular() || info.Size() != size {
			return AddRecordingInput{}, fmt.Errorf("本地录像文件不可用或大小不匹配")
		}
	}
	return in, nil
}

// ResolveRecordingPath 按录像自身归属解析，不使用通道当前绑定；停用节点仍可读取历史。
func (c Core) ResolveRecordingPath(ctx context.Context, rec *Recording) (string, error) {
	if rec.MediaServerID == "" {
		// 旧记录没有可靠节点身份：仅在本地确有匹配文件时兼容，不猜测当前通道绑定。
		full, err := c.ResolvePath(rec.Path)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() || (rec.Size > 0 && info.Size() != rec.Size) {
			return "", fmt.Errorf("旧录像归属未确认，已保留索引")
		}
		return full, nil
	}
	storage, err := c.StorageFor(ctx, rec.MediaServerID)
	if err != nil {
		return "", err
	}
	if rec.StorageRoot != "" && filepath.Clean(rec.StorageRoot) != storage.Root {
		return "", fmt.Errorf("录像原始目录与节点配置不一致，需要恢复原存储映射")
	}
	if storage.Mount == "" {
		return "", fmt.Errorf("请配置媒体节点 %s 的平台录像挂载目录", rec.MediaServerID)
	}
	if rec.MediaServerID != LocalNode {
		// 标记由运维在原存储创建，不能由平台自动创建；避免共享挂载掉线后落到空本地目录。
		marker, err := os.ReadFile(filepath.Join(storage.Mount, StorageMarker))
		if err != nil || strings.TrimSpace(string(marker)) != rec.MediaServerID {
			return "", fmt.Errorf("节点 %s 的共享录像存储不可用或身份标记不匹配", rec.MediaServerID)
		}
	}
	if filepath.IsAbs(rec.Path) || rec.Path == "" {
		return "", fmt.Errorf("录像相对路径无效")
	}
	if rec.FilePath != "" && filepath.Clean(rec.FilePath) != filepath.Join(storage.Root, filepath.FromSlash(rec.Path)) {
		return "", fmt.Errorf("录像路径与原始文件路径不一致")
	}
	// 复用现有符号链接和目录穿越保护，但根目录改为该节点的平台挂载目录。
	resolver := NewCore(nil, WithConfig(&conf.ServerRecording{StorageDir: storage.Mount}))
	return resolver.ResolvePath(rec.Path)
}

func (c Core) removeRecordingFile(ctx context.Context, rec *Recording) (bool, error) {
	full, err := c.ResolveRecordingPath(ctx, rec)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(full)
	if os.IsNotExist(err) && rec.MediaServerID != "" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("拒绝删除非普通录像文件")
	}
	if err := os.Remove(full); err != nil {
		return false, err
	}
	return true, nil
}

// BackfillLocalRecordings 只回填实际验证成功的本地旧文件；未知来源记录保持原样。
func (c Core) BackfillLocalRecordings(ctx context.Context) error {
	storage, err := c.StorageFor(ctx, LocalNode)
	if err != nil {
		return err
	}
	var cursor int64
	for {
		var batch []Recording
		err = c.store.Recording().Session(ctx, func(tx *gorm.DB) error {
			return tx.Where("media_server_id = '' AND id > ?", cursor).Order("id ASC").Limit(100).Find(&batch).Error
		})
		if err != nil || len(batch) == 0 {
			return err
		}
		for _, rec := range batch {
			cursor = rec.ID
			full, e := c.ResolveRecordingPath(ctx, &rec)
			if e != nil {
				continue
			}
			rel, e := filepath.Rel(storage.Mount, full)
			if e != nil || ensurePathBelowRoot(storage.Mount, full) != nil {
				continue
			}
			err = c.store.Recording().Session(ctx, func(tx *gorm.DB) error {
				return tx.Model(&Recording{}).Where("id = ? AND media_server_id = ''", rec.ID).Updates(map[string]any{"media_server_id": LocalNode, "vhost": DefaultVhost, "storage_root": storage.Root, "file_path": filepath.Join(storage.Root, rel), "path": filepath.ToSlash(rel), "updated_at": orm.Now()}).Error
			})
			if err != nil {
				return err
			}
		}
	}
}
