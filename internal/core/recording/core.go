package recording

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/system"
)

// Storer data persistence
type Storer interface {
	Recording() RecordingStorer
}

// SMSProvider 流媒体服务提供者接口，解耦录制领域与 sms 领域
type SMSProvider interface {
	StartRecord(app, stream, customPath string, maxSecond int) error
	StopRecord(app, stream string) error
	// ListRecordingStreams 批量获取所有在线流的录制状态
	// 返回 map[app/stream]bool，true 表示正在录制 MP4
	ListRecordingStreams() (map[string]bool, error)
}

// IPCProvider 通道信息提供者，解耦录制域与 ipc 域
// 用于定时同步时查询所有在线通道
type IPCProvider interface {
	ListOnlineChannels(ctx context.Context) ([]ChannelInfo, error)
}

// ChannelInfo 同步任务使用的通道信息摘要
type ChannelInfo struct {
	ID         string // 通道唯一 ID
	App        string // 应用名（如 rtp / live）
	Stream     string // 流 ID
	Type       string // 通道类型（gb28181 / onvif / rtmp / rtsp）
	RecordMode string // 录像模式（always / ai / none / 空串=always）
}

// PlayProvider 主动拉流能力，解耦录制域与播放域
// 仅用于"应录制但流不存在"时触发拉流
type PlayProvider interface {
	TriggerStream(ctx context.Context, info ChannelInfo) error
}

// Core business domain
type Core struct {
	store        Storer
	conf         *conf.ServerRecording
	smsProvider  SMSProvider
	ipcProvider  IPCProvider
	playProvider PlayProvider
	syncInterval time.Duration // 0 表示使用默认值 syncDefaultInterval
}

type Option func(*Core)

// WithSMSProvider 注入流媒体服务提供者，用于控制录制
func WithSMSProvider(provider SMSProvider) Option {
	return func(c *Core) {
		c.smsProvider = provider
	}
}

// WithConfig 注入录制配置
func WithConfig(conf *conf.ServerRecording) Option {
	return func(c *Core) {
		c.conf = conf
	}
}

// WithIPCProvider 注入通道信息提供者，用于定时同步时查询应录制的通道
func WithIPCProvider(provider IPCProvider) Option {
	return func(c *Core) {
		c.ipcProvider = provider
	}
}

// WithPlayProvider 注入拉流能力，用于流不存在时主动触发拉流
func WithPlayProvider(provider PlayProvider) Option {
	return func(c *Core) {
		c.playProvider = provider
	}
}

// WithSyncInterval 设置同步周期，用于测试时缩短等待时间
func WithSyncInterval(d time.Duration) Option {
	return func(c *Core) {
		c.syncInterval = d
	}
}

// NewCore create business domain
func NewCore(store Storer, opts ...Option) Core {
	c := Core{store: store}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}

// IsEnabled 检查是否启用录制（全局开关）
// 使用反转逻辑：Disabled=false 表示启用录制
func (c Core) IsEnabled() bool {
	return c.conf != nil && !c.conf.Disabled
}

// ResolvePath 将数据库相对路径解析为录像根目录内的绝对路径。
func (c Core) ResolvePath(path string) (string, error) {
	if c.conf == nil || c.conf.StorageDir == "" {
		return "", fmt.Errorf("recording storage directory is not configured")
	}
	storageDir := filepath.Clean(c.conf.StorageDir)
	if !filepath.IsAbs(storageDir) {
		storageDir = filepath.Join(system.Getwd(), storageDir)
	}
	root, err := filepath.Abs(storageDir)
	if err != nil {
		return "", err
	}
	comparisonRoot := root
	if evaluatedRoot, evalErr := filepath.EvalSymlinks(root); evalErr == nil {
		comparisonRoot = evaluatedRoot
	}
	candidate := filepath.Clean(strings.TrimSpace(path))
	if candidate == "" || candidate == "." {
		return "", fmt.Errorf("recording path is empty")
	}
	if !filepath.IsAbs(candidate) {
		// 兼容旧数据中包含 StorageDir 前缀的相对路径。
		storagePrefix := filepath.Clean(c.conf.StorageDir)
		if candidate == storagePrefix {
			return "", fmt.Errorf("recording path points to storage root")
		}
		if strings.HasPrefix(candidate, storagePrefix+string(filepath.Separator)) {
			candidate = strings.TrimPrefix(candidate, storagePrefix+string(filepath.Separator))
		}
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	if candidate == root {
		return "", fmt.Errorf("recording path points to storage root")
	}
	if err := ensurePathBelowRoot(root, candidate); err != nil {
		return "", err
	}

	// 已存在文件及其父目录需要解析符号链接，防止根目录内的链接指向外部文件。
	checkPath := candidate
	for {
		if _, statErr := os.Lstat(checkPath); statErr == nil {
			evaluated, evalErr := filepath.EvalSymlinks(checkPath)
			if evalErr != nil {
				return "", fmt.Errorf("resolve recording symlink: %w", evalErr)
			}
			if err := ensurePathBelowRoot(comparisonRoot, evaluated); err != nil {
				return "", err
			}
			break
		}
		parent := filepath.Dir(checkPath)
		if parent == checkPath {
			break
		}
		checkPath = parent
	}
	return candidate, nil
}

func ensurePathBelowRoot(root, candidate string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("recording path escapes storage directory")
	}
	return nil
}

// RelativePath 验证 Webhook 上报路径并转换为数据库相对路径。
func (c Core) RelativePath(path string) (string, error) {
	fullPath, err := c.ResolvePath(path)
	if err != nil {
		return "", err
	}
	storageDir := filepath.Clean(c.conf.StorageDir)
	if !filepath.IsAbs(storageDir) {
		storageDir = filepath.Join(system.Getwd(), storageDir)
	}
	root, err := filepath.Abs(storageDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, fullPath)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}
