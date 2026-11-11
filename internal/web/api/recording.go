package api

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/recording/store/recordingdb"
	"github.com/grafov/m3u8"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

// RecordingAPI 为 http 提供业务方法
type RecordingAPI struct {
	recordingCore recording.Core
	conf          *conf.Bootstrap
}

// NewRecordingStore 创建录像存储层
func NewRecordingStore(db *gorm.DB) recording.Storer {
	return recordingdb.NewDB(db).AutoMigrate(orm.GetEnabledAutoMigrate())
}

// NewRecordingCore 创建录像管理核心服务
// 依赖 recording.SMSProvider 接口而非 sms.Core，避免循环依赖
func NewRecordingCore(
	store recording.Storer,
	cfg *conf.Bootstrap,
	provider recording.SMSProvider,
	ipcProvider recording.IPCProvider,
	playProvider recording.PlayProvider,
) (recording.Core, func()) {
	core := recording.NewCore(store,
		recording.WithConfig(&cfg.Server.Recording),
		recording.WithSMSProvider(provider),
		recording.WithIPCProvider(ipcProvider),
		recording.WithPlayProvider(playProvider),
	)

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := core.BackfillLocalRecordings(ctx); err != nil {
			slog.WarnContext(ctx, "旧录像本地归属回填未完成", "err", err)
		}
		core.StartCleanupWorker(ctx)
	}()
	syncDone := core.StartRecordingSyncLoop(ctx)

	return core, func() {
		cancel()
		wg.Wait()
		<-syncDone
	}
}

func NewRecordingAPI(core recording.Core, conf *conf.Bootstrap) RecordingAPI {
	return RecordingAPI{recordingCore: core, conf: conf}
}

// recordingIDInput 录像 ID 路径参数
type recordingIDInput struct {
	ID int64 `uri:"id" binding:"required"`
}

// updateRecordingInput 更新录像的请求参数（路径 ID + 请求体）
type updateRecordingInput struct {
	ID int64 `uri:"id" binding:"required"`
	recording.EditRecordingInput
}

func RegisterRecording(g gin.IRouter, api RecordingAPI, handler ...gin.HandlerFunc) {
	{
		group := g.Group("/recordings", handler...)
		group.GET("", web.WrapH(api.listRecordings))
		group.GET("/timeline", web.WrapH(api.getTimeline))
		group.GET("/monthly", web.WrapH(api.getMonthlyStats))
		// HLS 播放列表（根据通道 ID 和时间范围生成 m3u8）
		group.GET("/channels/:cid/index.m3u8", api.channelPlaylist)
		group.GET("/:id", web.WrapH(api.getRecording))
		group.PUT("/:id", web.WrapH(api.updateRecording))
		group.DELETE("/:id", web.WrapH(api.deleteRecording))
		group.GET("/:id/file", api.recordingFile)
		group.HEAD("/:id/file", api.recordingFile)
		group.GET("/:id/download", api.downloadRecording)
		group.HEAD("/:id/download", api.downloadRecording)
	}

	// 录像文件服务使用统一路径解析，拒绝目录穿越及指向根目录外的符号链接。
	// 路径格式: /static/recordings/xxx.mp4?token=xxx
	// Gin Static 支持 HTTP Range 请求，实现边下载边播放（秒播）
	if api.conf != nil && api.conf.Server.Recording.StorageDir != "" {
		slog.Info("注册录像静态文件服务", "path", "/static/recordings", "dir", api.conf.Server.Recording.StorageDir)
		g.Group("/static", handler...).GET("/recordings/*path", api.serveRecordingFile)
	}
}

func (a RecordingAPI) serveRecordingFile(c *gin.Context) {
	requestPath := strings.TrimPrefix(c.Param("path"), "/")
	filePath, err := a.recordingCore.ResolvePath(requestPath)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"code": 1, "msg": "invalid recording path"})
		return
	}
	info, err := os.Stat(filePath)
	if err != nil || !info.Mode().IsRegular() {
		c.JSON(http.StatusNotFound, gin.H{"code": 1, "msg": "recording file not found"})
		return
	}
	c.File(filePath)
}

// listRecordings 分页查询录像列表
func (a RecordingAPI) listRecordings(c *gin.Context, in *recording.FindRecordingInput) (any, error) {
	ctx := web.WithContext(c.Request)
	items, total, err := a.recordingCore.ListRecordings(ctx, in)
	return gin.H{"items": items, "total": total}, err
}

// getTimeline 获取时间轴数据
func (a RecordingAPI) getTimeline(c *gin.Context, in *recording.TimelineInput) (any, error) {
	items, err := a.recordingCore.GetTimeline(c.Request.Context(), in)
	return gin.H{"items": items}, err
}

func (a RecordingAPI) getRecording(c *gin.Context, in *recordingIDInput) (*recording.Recording, error) {
	return a.recordingCore.GetRecording(c.Request.Context(), in.ID)
}

func (a RecordingAPI) updateRecording(c *gin.Context, in *updateRecordingInput) (*recording.Recording, error) {
	return a.recordingCore.UpdateRecording(c.Request.Context(), &in.EditRecordingInput, in.ID)
}

func (a RecordingAPI) deleteRecording(c *gin.Context, in *recordingIDInput) (*recording.Recording, error) {
	return a.recordingCore.DeleteRecording(c.Request.Context(), in.ID)
}

// getMonthlyStats 获取月度录像统计
func (a RecordingAPI) getMonthlyStats(c *gin.Context, in *recording.MonthlyStatsInput) (*recording.MonthlyStatsOutput, error) {
	return a.recordingCore.GetMonthlyStats(c.Request.Context(), in)
}

// downloadRecording 下载录像文件
func (a RecordingAPI) downloadRecording(c *gin.Context) { a.serveRecordingByID(c, true) }
func (a RecordingAPI) recordingFile(c *gin.Context)     { a.serveRecordingByID(c, false) }

func (a RecordingAPI) serveRecordingByID(c *gin.Context, download bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "invalid recording id"})
		return
	}
	rec, err := a.recordingCore.GetRecording(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 1, "msg": "recording not found"})
		return
	}
	file, err := a.recordingCore.ResolveRecordingPath(c.Request.Context(), rec)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"code": 1, "msg": err.Error()})
		return
	}
	info, err := os.Stat(file)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"code": 1, "msg": "recording file not found"})
		} else {
			c.JSON(http.StatusServiceUnavailable, gin.H{"code": 1, "msg": "recording storage unavailable"})
		}
		return
	}
	if !info.Mode().IsRegular() {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if download {
		c.Header("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(file)}))
	}
	c.Header("Cache-Control", "private, no-store")
	// ServeFile/ServeContent 支持 Range、If-Range、HEAD；浏览器永远只访问平台鉴权地址。
	c.File(file)
}

// channelPlaylist 生成 HLS m3u8 播放列表
// 根据通道 ID 和时间范围，动态生成包含多个 MP4 片段的 m3u8 文件
// 路径: /recordings/channels/:cid/index.m3u8?start_ms=xxx&end_ms=xxx&token=xxx
func (a RecordingAPI) channelPlaylist(c *gin.Context) {
	cid := c.Param("cid")
	if cid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "cid is required"})
		return
	}

	startMs, _ := strconv.ParseInt(c.Query("start_ms"), 10, 64)
	endMs, _ := strconv.ParseInt(c.Query("end_ms"), 10, 64)
	token := c.Query("token")

	if startMs <= 0 || endMs <= startMs {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "start_ms and end_ms are required"})
		return
	}

	// 获取时间范围内的录像列表（需要完整路径信息）
	ctx := web.WithContext(c.Request)

	recordings, err := a.recordingCore.RecordingsInRange(ctx, cid, startMs, endMs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": 1, "msg": err.Error()})
		return
	}

	if len(recordings) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"code": 1, "msg": "no recordings found in time range"})
		return
	}

	// 构建请求的 base URL
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	baseURL := fmt.Sprintf("%s://%s", scheme, c.Request.Host)

	// 生成 m3u8 内容（带 token）
	m3u8Content := a.generateM3U8WithToken(recordings, baseURL, token)

	c.Header("Content-Type", "application/vnd.apple.mpegurl")
	c.Header("Cache-Control", "no-cache")
	c.String(http.StatusOK, m3u8Content)
}

// generateM3U8WithToken 根据录像列表生成 m3u8 播放列表（每个 MP4 URL 带 token）
func (a RecordingAPI) generateM3U8WithToken(recordings []*recording.Recording, baseURL, token string) string {
	count := len(recordings)
	if count == 0 {
		return ""
	}

	// 创建媒体播放列表 (winSize=0 表示 VOD，不使用滑动窗口)
	pl, err := m3u8.NewMediaPlaylist(0, uint(count))
	if err != nil {
		return ""
	}

	// 设置为 VOD 类型
	pl.MediaType = m3u8.VOD

	// 录像按时间升序排列
	sortedRecs := make([]*recording.Recording, len(recordings))
	copy(sortedRecs, recordings)

	sort.SliceStable(sortedRecs, func(i, j int) bool { return sortedRecs[i].StartedAt.Before(sortedRecs[j].StartedAt.Time) })

	// 添加每个录像片段
	// URL 格式: /recordings/{id}/file?token=xxx；每段独立解析所属存储节点。
	// 使用相对路径（以 / 开头），让浏览器相对于当前域名访问
	// 这样无论通过代理还是直接访问都能正常工作
	// ZLM 录制的 fMP4 每个文件 DTS 都从 0 开始，必须在每个片段间添加 DISCONTINUITY
	// 告诉 HLS.js 重置解码器，避免 DTS 不连续导致的解析错误
	for i, rec := range sortedRecs {
		// 每个片段之间都添加 EXT-X-DISCONTINUITY 标签
		// ZLM 每个录像文件都是独立的 fMP4，DTS 从 0 开始，必须重置解码器
		if i > 0 {
			pl.SetDiscontinuity()
		}

		uri := fmt.Sprintf("/recordings/%d/file", rec.ID)
		if token != "" {
			uri += "?token=" + url.QueryEscape(token)
		}
		_ = pl.Append(uri, rec.Duration, "")
	}

	// 关闭播放列表，添加 #EXT-X-ENDLIST 标签
	pl.Close()

	// 编码为字符串
	return pl.String()
}
