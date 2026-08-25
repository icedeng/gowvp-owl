package sms

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/pkg/zlm"
	"github.com/ixugo/goddd/pkg/conc"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/reason"
	"github.com/ixugo/goddd/pkg/web"
)

const KeepaliveInterval = 2 * 15 * time.Second

type WarpMediaServer struct {
	mu            sync.RWMutex
	IsOnline      bool
	LastUpdatedAt time.Time
	Config        *MediaServer
}

func (m *WarpMediaServer) status() (bool, time.Time, *MediaServer) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.IsOnline, m.LastUpdatedAt, m.Config
}

func (m *WarpMediaServer) update(isOnline bool, lastUpdatedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.IsOnline = isOnline
	if !lastUpdatedAt.IsZero() {
		m.LastUpdatedAt = lastUpdatedAt
	}
}

func (m *WarpMediaServer) touch(lastUpdatedAt time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.LastUpdatedAt = lastUpdatedAt
}

type NodeManager struct {
	storer Storer

	drivers      map[string]Driver
	cacheServers conc.Map[string, *WarpMediaServer]
	quit         chan struct{}
	connectionMu sync.Mutex
	closeOnce    sync.Once
	wg           sync.WaitGroup
	serverPort   atomic.Int64
}

func NewNodeManager(storer Storer) *NodeManager {
	n := NodeManager{
		storer:  storer,
		drivers: make(map[string]Driver),
		quit:    make(chan struct{}, 1),
	}
	n.RegisterDriver(ProtocolZLMediaKit, NewZLMDriver())
	n.RegisterDriver(ProtocolLalmax, NewLalmaxDriver())
	n.wg.Add(1)
	go n.tickCheck()
	return &n
}

func (n *NodeManager) RegisterDriver(name string, driver Driver) {
	n.drivers[name] = driver
}

func (n *NodeManager) getDriver(name string) (Driver, error) {
	if name == "" {
		name = "zlm"
	}
	d, ok := n.drivers[name]
	if !ok {
		return nil, fmt.Errorf("driver [%s] not found", name)
	}
	return d, nil
}

func (n *NodeManager) Close() {
	if n == nil {
		return
	}
	n.closeOnce.Do(func() {
		close(n.quit)
		n.wg.Wait()
	})
}

// tickCheck 定时检查服务是否离线
func (n *NodeManager) tickCheck() {
	defer n.wg.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-n.quit:
			return
		case <-ticker.C:
			n.cacheServers.Range(func(_ string, ms *WarpMediaServer) bool {
				n.checkMediaServer(ms)
				return true
			})
		}
	}
}

func (n *NodeManager) checkMediaServer(ms *WarpMediaServer) {
	if ms == nil {
		return
	}
	isOnline, lastUpdatedAt, config := ms.status()
	if isOnline && !lastUpdatedAt.IsZero() && time.Since(lastUpdatedAt) < KeepaliveInterval {
		return
	}
	if config == nil {
		ms.update(false, time.Time{})
		return
	}

	driver, err := n.getDriver(config.Type)
	if err != nil {
		ms.update(false, time.Time{})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err = driver.Ping(ctx, config)
	cancel()
	if err != nil {
		ms.update(false, time.Time{})
		return
	}

	// 节点可能已经重启并丢失动态 Hook 配置，恢复时必须执行完整连接流程。
	if err := n.connection(config, int(n.serverPort.Load())); err != nil {
		slog.Error("Reconnect media server failed", "id", config.ID, "err", err)
		ms.update(false, time.Time{})
	}
}

// 读取 config.ini 文件，通过正则表达式，获取 secret 的值
func getSecret(configDir string) (string, error) {
	for _, file := range []string{"zlm.ini", "config.ini"} {
		content, err := os.ReadFile(filepath.Join(configDir, file))
		if err != nil {
			continue
		}
		sectionPattern := regexp.MustCompile(`^\s*\[([^]]+)]\s*$`)
		prefixedPattern := regexp.MustCompile(`(?i)^\s*api\.secret\s*=\s*([^\s#;]+)\s*$`)
		secretPattern := regexp.MustCompile(`(?i)^\s*secret\s*=\s*([^\s#;]+)\s*$`)
		inAPI := false
		for _, line := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
			if matches := prefixedPattern.FindStringSubmatch(line); len(matches) == 2 {
				return matches[1], nil
			}
			if matches := sectionPattern.FindStringSubmatch(line); len(matches) == 2 {
				inAPI = strings.EqualFold(strings.TrimSpace(matches[1]), "api")
				continue
			}
			if inAPI {
				if matches := secretPattern.FindStringSubmatch(line); len(matches) == 2 {
					return matches[1], nil
				}
			}
		}
	}
	return "", fmt.Errorf("unknow")
}

// TODO: 发现配置会导致程序延迟 1~2s 才能启动
func setupSecret(bc *conf.Bootstrap) {
	if bc.Media.Secret != "" {
		return
	}
	// 六六大顺
	for range 6 {
		secret, err := getSecret(bc.ConfigDir)
		if err == nil {
			if conf.IsRevokedSecret(secret) {
				slog.Error("ZLM 配置仍使用已泄露的历史密钥，请设置 OWL_MEDIA_SECRET 并同步更新媒体服务")
				return
			}
			slog.Info("发现 ZLM 配置并加载媒体密钥")
			bc.Media.Secret = secret
			return
		}
		time.Sleep(200 * time.Millisecond)
		continue
	}
	if bc.Media.Secret == "" {
		slog.Warn("未发现 zlm 配置，请通过 OWL_MEDIA_SECRET 或配置文件提供媒体密钥")
	}
}

func (n *NodeManager) Run(bc *conf.Bootstrap, serverPort int) error {
	ctx := context.Background()
	n.serverPort.Store(int64(serverPort))
	setupSecret(bc)
	if bc.Media.Secret == "" {
		return fmt.Errorf("media secret is required; set OWL_MEDIA_SECRET or provide zlm.ini/config.ini")
	}
	cfg := bc.Media
	setValueFn := func(ms *MediaServer) {
		ms.ID = DefaultMediaServerID
		ms.IP = cfg.IP
		ms.Ports.HTTP = cfg.HTTPPort
		ms.Secret = cfg.Secret
		ms.Type = cfg.Type
		// TODO: 应该读取环境变量
		if ms.Type == "" {
			ms.Type = ProtocolZLMediaKit
		}
		ms.Status = false
		ms.RTPPortRange = cfg.RTPPortRange
		ms.HookIP = cfg.WebHookIP
		ms.SDPIP = cfg.SDPIP
	}

	var ms MediaServer
	if err := n.storer.MediaServer().Update(ctx, &ms, func(b *MediaServer) {
		setValueFn(b)
	}, orm.Where("id=?", DefaultMediaServerID)); err != nil {
		if !orm.IsErrRecordNotFound(err) {
			return err
		}
		setValueFn(&ms)
		if err := n.storer.MediaServer().Create(ctx, &ms); err != nil {
			return err
		}
	}

	mediaServers, _, err := n.listMediaServers(ctx, &FindMediaServerInput{
		PagerFilter: web.NewPagerFilterMaxSize(),
	})
	if err != nil {
		return err
	}

	for _, ms := range mediaServers {
		n.wg.Add(1)
		go func(ms *MediaServer) {
			defer n.wg.Done()
			if err := n.connection(ms, serverPort); err != nil {
				slog.Error("Connect media server failed", "id", ms.ID, "err", err)
			}
		}(ms)
	}

	return nil
}

func (n *NodeManager) connection(server *MediaServer, serverPort int) error {
	if server == nil {
		return fmt.Errorf("media server is required")
	}
	n.connectionMu.Lock()
	defer n.connectionMu.Unlock()

	state := &WarpMediaServer{Config: server}
	n.cacheServers.Store(server.ID, state)

	driver, err := n.getDriver(server.Type)
	if err != nil {
		slog.Error("获取驱动失败", "type", server.Type, "err", err)
		return err
	}

	log := slog.With("id", server.ID, "type", server.Type)
	log.Info("MediaServer 连接中...")

	ctx := context.Background()
	if err := driver.Connect(ctx, server); err != nil {
		log.Error("MediaServer 连接失败", "err", err)
		return err
	}
	log.Info("MediaServer 连接成功")

	log.Info("MediaServer 配置设置...")
	hookPrefix := fmt.Sprintf("http://%s:%d/webhook", server.HookIP, serverPort)
	u, err := url.Parse(hookPrefix)
	if err != nil {
		return fmt.Errorf("build media webhook URL: %w", err)
	}
	query := u.Query()
	query.Set("secret", server.Secret)
	u.RawQuery = query.Encode()
	hookPrefix = u.String()
	if err := driver.Setup(ctx, server, hookPrefix); err != nil {
		log.Error("MediaServer 配置设置失败", "err", err)
		return err
	}

	// 只有连接和动态配置都成功后，才持久化并发布在线状态。
	if err := n.storer.MediaServer().Update(ctx, &MediaServer{}, func(b *MediaServer) {
		b.Ports = server.Ports
		b.HookAliveInterval = server.HookAliveInterval
		b.Status = server.Status
	}, orm.Where("id=?", server.ID)); err != nil {
		return fmt.Errorf("save media server: %w", err)
	}
	state.update(true, time.Now())

	return nil
}

func (n *NodeManager) Keepalive(serverID string) {
	value, ok := n.cacheServers.Load(serverID)
	if !ok {
		return
	}
	value.touch(time.Now())
}

func (n *NodeManager) IsOnline(serverID string) bool {
	value, ok := n.cacheServers.Load(serverID)
	if !ok {
		return false
	}
	isOnline, _, _ := value.status()
	return isOnline
}

// listMediaServers Paginated search
func (n *NodeManager) listMediaServers(ctx context.Context, in *FindMediaServerInput) ([]*MediaServer, int64, error) {
	items := make([]*MediaServer, 0)
	total, err := n.storer.MediaServer().List(ctx, &items, in)
	if err != nil {
		return nil, 0, reason.ErrDB.Withf(`List err[%s]`, err.Error())
	}
	return items, total, nil
}

// OpenRTPServer 开启RTP服务器
func (n *NodeManager) OpenRTPServer(server *MediaServer, in zlm.OpenRTPServerRequest) (*zlm.OpenRTPServerResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.OpenRTPServer(context.Background(), server, &in)
}

// CloseRTPServer 关闭RTP服务器
func (n *NodeManager) CloseRTPServer(server *MediaServer, in zlm.CloseRTPServerRequest) (*zlm.CloseRTPServerResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.CloseRTPServer(context.Background(), server, &in)
}

// StartSendRTP 从媒体服务器中的已有流启动 RTP 发送。
func (n *NodeManager) StartSendRTP(server *MediaServer, in zlm.StartSendRTPRequest) (*zlm.StartSendRTPResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.StartSendRTP(context.Background(), server, &in)
}

// StartSendRTPTalk 复用设备 RTP 接收链路发送双向对讲音频。
func (n *NodeManager) StartSendRTPTalk(server *MediaServer, in zlm.StartSendRTPTalkRequest) (*zlm.StartSendRTPResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.StartSendRTPTalk(context.Background(), server, &in)
}

// StopSendRTP 停止媒体服务器中的 RTP 发送任务。
func (n *NodeManager) StopSendRTP(server *MediaServer, in zlm.StopSendRTPRequest) (*zlm.StopSendRTPResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.StopSendRTP(context.Background(), server, &in)
}

// CloseStreams 关闭指定流
func (n *NodeManager) CloseStreams(server *MediaServer, in zlm.CloseStreamsRequest) (*zlm.CloseStreamsResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.CloseStreams(context.Background(), server, &in)
}

// CreateStreamProxy 添加流代理
func (n *NodeManager) CreateStreamProxy(server *MediaServer, in AddStreamProxyRequest) (*zlm.AddStreamProxyResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.AddStreamProxy(context.Background(), server, &in)
}

func (n *NodeManager) GetSnapshot(server *MediaServer, in GetSnapRequest) ([]byte, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.GetSnapshot(context.Background(), server, &in)
}

func (n *NodeManager) GetStreamLiveAddr(server *MediaServer, httpPrefix, host, app, stream, token string) StreamLiveAddr {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return StreamLiveAddr{Label: err.Error()}
	}
	return driver.GetStreamLiveAddr(context.Background(), server, httpPrefix, host, app, stream, token)
}

// GetMediaInfo 获取指定流的详细音视频轨道信息
func (n *NodeManager) GetMediaInfo(server *MediaServer, app, stream string) ([]zlm.MediaItem, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.GetMediaInfo(context.Background(), server, app, stream)
}

// StartRecord 开始录制指定流
func (n *NodeManager) StartRecord(server *MediaServer, in zlm.StartRecordRequest) (*zlm.StartRecordResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.StartRecord(context.Background(), server, &in)
}

// StopRecord 停止录制指定流
func (n *NodeManager) StopRecord(server *MediaServer, in zlm.StopRecordRequest) (*zlm.StopRecordResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.StopRecord(context.Background(), server, &in)
}

// GetMediaList 批量获取所有在线流列表（含录制状态）
func (n *NodeManager) GetMediaList(server *MediaServer) (*zlm.GetMediaListResponse, error) {
	driver, err := n.getDriver(server.Type)
	if err != nil {
		return nil, err
	}
	return driver.GetMediaList(context.Background(), server)
}
