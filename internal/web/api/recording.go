package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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
func NewRecordingCore(store recording.Storer, cfg *conf.Bootstrap, provider recording.SMSProvider) recording.Core {
	core := recording.NewCore(store,
		recording.WithConfig(&cfg.Server.Recording),
		recording.WithSMSProvider(provider),
	)

	// 启动清理协程
	go core.StartCleanupWorker()

	return core
}

func NewRecordingAPI(core recording.Core, conf *conf.Bootstrap) RecordingAPI {
	return RecordingAPI{recordingCore: core, conf: conf}
}

func RegisterRecording(g gin.IRouter, api RecordingAPI, handler ...gin.HandlerFunc) {
	{
		group := g.Group("/recordings", handler...)
		group.GET("", web.WrapH(api.findRecordings))
		group.GET("/timeline", web.WrapH(api.getTimeline))
		group.GET("/monthly", web.WrapH(api.getMonthlyStats))
		// HLS 播放列表（根据通道 ID 和时间范围生成 m3u8）
		group.GET("/channels/:cid/index.m3u8", api.channelPlaylist)
		group.GET("/:id", web.WrapH(api.getRecording))
		group.PUT("/:id", web.WrapH(api.editRecording))
		group.DELETE("/:id", web.WrapH(api.delRecording))
		group.GET("/:id/download", api.downloadRecording)
	}

	// 静态文件服务，用于访问录像 MP4 文件
	// 路径格式: /static/recordings/xxx.mp4?token=xxx
	// Gin Static 支持 HTTP Range 请求，实现边下载边播放（秒播）
	if api.conf != nil && api.conf.Server.Recording.StorageDir != "" {
		slog.Info("注册录像静态文件服务", "path", "/static/recordings", "dir", api.conf.Server.Recording.StorageDir)
		g.Static("/static/recordings", api.conf.Server.Recording.StorageDir)
	}
}

// findRecordings 分页查询录像列表
// findRecordings godoc
// @Summary 查询录像列表
// @Description 查询本地录像列表，可按通道、应用名、流 ID、时间范围分页筛选。
// @Description 常见场景：录像管理页列表展示、按通道检索录像、按日期范围检索录像。
// @Description 响应中 `items` 为录像记录列表，`total` 为符合条件的总条数。
// @Description 筛选条件说明：`cid` 为通道 ID；`app` 为流应用名；`stream` 为流 ID；`start_ms/end_ms` 为录像开始时间范围过滤。
// @Description 调用前置条件：本地已启用录像并存在录像记录。
// @Description 失败场景：1. 分页参数非法；2. 时间范围非法；3. 数据库查询失败。
// @Tags Recording
// @Security BearerAuth
// @Produce json
// @Param page query int false "页码"
// @Param size query int false "每页数量"
// @Param cid query string false "通道ID"
// @Param app query string false "应用名"
// @Param stream query string false "流ID"
// @Param start_ms query int false "开始毫秒时间戳"
// @Param end_ms query int false "结束毫秒时间戳"
// @Success 200 {object} SwaggerRecordingsResponse
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings [get]
func (a RecordingAPI) findRecordings(c *gin.Context, in *recording.FindRecordingInput) (any, error) {
	items, total, err := a.recordingCore.FindRecordings(c.Request.Context(), in)
	return gin.H{"items": items, "total": total}, err
}

// getTimeline 获取时间轴数据
// getTimeline godoc
// @Summary 获取录像时间轴
// @Description 返回某个通道在指定时间范围内的录像时间轴片段，适合前端时间轴控件直接使用。
// @Description 请求示例：`/recordings/timeline?cid=GB_34020000001320000001&start_ms=1710864000000&end_ms=1710950400000`
// @Description 响应示例：`{ "items": [ { "id": 1, "start_ms": 1710864000000, "end_ms": 1710867600000, "duration": 3600 } ] }`
// @Description 调用前置条件：指定通道已有录像记录。
// @Description 失败场景：1. `cid` 缺失；2. 时间范围非法；3. 查询失败。
// @Tags Recording
// @Security BearerAuth
// @Produce json
// @Param cid query string true "通道ID"
// @Param start_ms query int false "开始毫秒时间戳"
// @Param end_ms query int false "结束毫秒时间戳"
// @Success 200 {object} SwaggerTimelineResponse
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings/timeline [get]
func (a RecordingAPI) getTimeline(c *gin.Context, in *recording.TimelineInput) (any, error) {
	items, err := a.recordingCore.GetTimeline(c.Request.Context(), in)
	return gin.H{"items": items}, err
}

// getRecording godoc
// @Summary 获取录像详情
// @Tags Recording
// @Security BearerAuth
// @Produce json
// @Param id path int true "录像ID"
// @Success 200 {object} recording.Recording
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings/{id} [get]
func (a RecordingAPI) getRecording(c *gin.Context, _ *struct{}) (*recording.Recording, error) {
	recordingID, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	return a.recordingCore.GetRecording(c.Request.Context(), recordingID)
}

// editRecording godoc
// @Summary 修改录像
// @Tags Recording
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param id path int true "录像ID"
// @Param body body recording.EditRecordingInput true "录像更新参数"
// @Success 200 {object} recording.Recording
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings/{id} [put]
func (a RecordingAPI) editRecording(c *gin.Context, in *recording.EditRecordingInput) (*recording.Recording, error) {
	recordingID, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	return a.recordingCore.EditRecording(c.Request.Context(), in, recordingID)
}

// delRecording godoc
// @Summary 删除录像
// @Tags Recording
// @Security BearerAuth
// @Produce json
// @Param id path int true "录像ID"
// @Success 200 {object} recording.Recording
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings/{id} [delete]
func (a RecordingAPI) delRecording(c *gin.Context, _ *struct{}) (*recording.Recording, error) {
	recordingID, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	return a.recordingCore.DelRecording(c.Request.Context(), recordingID)
}

// getMonthlyStats 获取月度录像统计
// getMonthlyStats godoc
// @Summary 获取月度录像统计
// @Description 返回指定月份每天是否存在录像的位图字符串，适合月历视图快速渲染。
// @Description 请求示例：`/recordings/monthly?cid=GB_34020000001320000001&year=2024&month=3`
// @Description 响应示例：`{ "year": 2024, "month": 3, "days": 31, "has_video": "1110010000000000000000000000000" }`
// @Description 调用前置条件：已存在录像数据。
// @Description 失败场景：1. 年月参数非法；2. 查询失败。
// @Tags Recording
// @Security BearerAuth
// @Produce json
// @Param cid query string false "通道ID"
// @Param year query int true "年份"
// @Param month query int true "月份"
// @Success 200 {object} recording.MonthlyStatsOutput
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings/monthly [get]
func (a RecordingAPI) getMonthlyStats(c *gin.Context, in *recording.MonthlyStatsInput) (*recording.MonthlyStatsOutput, error) {
	return a.recordingCore.GetMonthlyStats(c.Request.Context(), in)
}

// downloadRecording 下载录像文件
// downloadRecording godoc
// @Summary 下载录像文件
// @Tags Recording
// @Security BearerAuth
// @Produce application/octet-stream
// @Param id path int true "录像ID"
// @Success 200 {file} file "录像文件"
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings/{id}/download [get]
func (a RecordingAPI) downloadRecording(c *gin.Context) {
	recordingID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "invalid recording id"})
		return
	}

	rec, err := a.recordingCore.GetRecording(c.Request.Context(), recordingID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": 1, "msg": err.Error()})
		return
	}

	// 构建文件完整路径
	filePath := a.recordingCore.GetFullPath(rec.Path)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.JSON(http.StatusNotFound, gin.H{"code": 1, "msg": "recording file not found"})
		return
	}

	// 设置下载文件名
	fileName := filepath.Base(filePath)
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", fileName))
	c.File(filePath)
}

// channelPlaylist 生成 HLS m3u8 播放列表
// 根据通道 ID 和时间范围，动态生成包含多个 MP4 片段的 m3u8 文件
// 路径: /recordings/channels/:cid/index.m3u8?start_ms=xxx&end_ms=xxx&token=xxx
// channelPlaylist godoc
// @Summary 生成录像 HLS 播放列表
// @Description 根据通道 ID 和时间范围动态生成 HLS m3u8 播放列表，便于浏览器直接回放录像。
// @Description 请求示例：`/recordings/channels/GB_34020000001320000001/index.m3u8?start_ms=1710864000000&end_ms=1710950400000&token=xxx`
// @Description 返回内容为标准 m3u8 文本，不是 JSON。
// @Description 调用前置条件：指定时间范围内存在录像文件，且录像文件仍在磁盘中。
// @Description 失败场景：1. `cid` 缺失；2. `start_ms/end_ms` 缺失；3. 范围内无录像；4. 录像文件已被清理。
// @Tags Recording
// @Security BearerAuth
// @Produce application/vnd.apple.mpegurl
// @Param cid path string true "通道ID"
// @Param start_ms query int true "开始毫秒时间戳"
// @Param end_ms query int true "结束毫秒时间戳"
// @Param token query string false "访问令牌"
// @Success 200 {string} string "M3U8 内容"
// @Failure 400 {object} SwaggerErrorResponse
// @Router /recordings/channels/{cid}/index.m3u8 [get]
func (a RecordingAPI) channelPlaylist(c *gin.Context) {
	cid := c.Param("cid")
	if cid == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "cid is required"})
		return
	}

	startMs, _ := strconv.ParseInt(c.Query("start_ms"), 10, 64)
	endMs, _ := strconv.ParseInt(c.Query("end_ms"), 10, 64)
	token := c.Query("token")

	if startMs <= 0 || endMs <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "start_ms and end_ms are required"})
		return
	}

	// 获取时间范围内的录像列表（需要完整路径信息）
	recordings, _, err := a.recordingCore.FindRecordings(c.Request.Context(), &recording.FindRecordingInput{
		CID:         cid,
		PagerFilter: web.PagerFilter{Page: 1, Size: 10000},
		DateFilter:  web.DateFilter{StartMs: startMs, EndMs: endMs},
	})
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
	// 按开始时间升序排序
	for i := 0; i < len(sortedRecs)-1; i++ {
		for j := i + 1; j < len(sortedRecs); j++ {
			if sortedRecs[i].StartedAt.After(sortedRecs[j].StartedAt.Time) {
				sortedRecs[i], sortedRecs[j] = sortedRecs[j], sortedRecs[i]
			}
		}
	}

	// 添加每个录像片段
	// URL 格式: /static/recordings/{path}?token=xxx
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

		// 构建相对路径，去掉前导斜杠
		relativePath := strings.TrimPrefix(rec.Path, "/")

		// 使用相对路径（不带域名），让浏览器根据当前页面域名访问
		// 这样开发时通过 Vite 代理、生产时通过后端都能正常访问
		var uri string
		if token != "" {
			uri = fmt.Sprintf("/static/recordings/%s?token=%s", relativePath, token)
		} else {
			uri = fmt.Sprintf("/static/recordings/%s", relativePath)
		}
		_ = pl.Append(uri, rec.Duration, "")
	}

	// 关闭播放列表，添加 #EXT-X-ENDLIST 标签
	pl.Close()

	// 编码为字符串
	return pl.String()
}
