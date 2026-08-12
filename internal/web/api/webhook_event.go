package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/system"
	"github.com/ixugo/goddd/pkg/web"
)

const (
	maxDetectionsPerEvent = 1000
	maxEventTextBytes     = 4096
)

func validEventText(value string) bool {
	return len(value) <= maxEventTextBytes
}

func validScore(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

// onWebhookEvents 统一事件接收入口，兼容两种来源：
//
//   - Python AI：payload 为 AIDetectionInput 格式，使用 InternalSecret 鉴权
//   - 其他 gowvp 实例：payload 为 webhookForwardInput 格式，使用 RecvSecret 鉴权
//
// 鉴权：先从 Secret header 读取，无则从 query ?secret= 读取，匹配 AISecret 或 RecvSecret 任一通过
// 来源区分：InternalSecret 对应 AI，RecvSecret 对应 gowvp 转发
func (w WebHookAPI) onWebhookEvents(c *gin.Context) {
	ctx := c.Request.Context()
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxWebhookBodyBytes)

	secret := w.resolveSecret(c)
	if !w.validateSecret(secret) {
		c.JSON(http.StatusUnauthorized, gin.H{"code": 1, "msg": "unauthorized"})
		return
	}

	if secret == w.conf.AISecret {
		w.handleAIEvents(c)
		return
	}

	var in webhookForwardInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": err.Error()})
		return
	}
	if strings.TrimSpace(in.CID) == "" || !validEventText(in.CID) || !validEventText(in.DID) || !validEventText(in.Label) ||
		!validEventText(in.Zones) || !validEventText(in.Model) || !validEventText(in.ImagePath) || !validScore(float64(in.Score)) {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "invalid event payload"})
		return
	}

	w.log.InfoContext(ctx, "webhook forward event received", "cid", in.CID, "did", in.DID, "label", in.Label)

	imagePath := in.ImagePath
	if in.ImageBase64 != "" {
		// 优先解码 base64 图片落盘，避免依赖对方文件路径
		saved, err := saveEventSnapshot(in.CID, in.StartedAt, in.ImageBase64)
		if err != nil {
			w.log.WarnContext(ctx, "webhook forward: save image failed, skipping image", "err", err)
		} else {
			imagePath = saved
		}
	} else if imagePath != "" {
		validated, err := resolveEventImagePath(imagePath)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "invalid image path"})
			return
		}
		imagePath = validated
	}

	_, err := w.eventCore.CreateEvent(ctx, &event.AddEventInput{
		DID:       in.DID,
		CID:       in.CID,
		StartedAt: in.StartedAt,
		EndedAt:   in.EndedAt,
		Label:     in.Label,
		Score:     in.Score,
		Zones:     in.Zones,
		ImagePath: imagePath,
		Model:     in.Model,
	})
	if err != nil {
		slog.ErrorContext(ctx, "webhook forward event save failed", "err", err)
		c.JSON(http.StatusOK, gin.H{"code": 1, "msg": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "msg": "ok"})
}

// resolveSecret 从请求中提取 secret：优先 Secret header，其次 query 参数 ?secret=
func (w WebHookAPI) resolveSecret(c *gin.Context) string {
	if auth := c.GetHeader("Secret"); auth != "" {
		return auth
	}
	return c.Query("secret")
}

// validateSecret 校验 secret 是否匹配 InternalSecret 或 RecvSecret
func (w WebHookAPI) validateSecret(secret string) bool {
	return secret != "" && (constantTimeEqual(secret, w.conf.AISecret) || constantTimeEqual(secret, w.conf.Server.Webhook.RecvSecret))
}

// handleAIEvents 处理 Python AI 推送的检测事件
// 按 label 分别入库，图片保存到 configs/events 目录，入库后触发 webhook 推送
func (w WebHookAPI) handleAIEvents(c *gin.Context) {
	ctx := c.Request.Context()

	var in AIDetectionInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": err.Error()})
		return
	}
	if strings.TrimSpace(in.CameraID) == "" || !validEventText(in.CameraID) || len(in.Detections) > maxDetectionsPerEvent ||
		in.SnapshotWidth < 0 || in.SnapshotHeight < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "invalid AI event payload"})
		return
	}
	for _, det := range in.Detections {
		if !validEventText(det.Label) || !validScore(det.Confidence) || det.Area < 0 ||
			det.Box.XMin < 0 || det.Box.YMin < 0 || det.Box.XMax < det.Box.XMin || det.Box.YMax < det.Box.YMin {
			c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "invalid AI detection"})
			return
		}
	}

	w.log.InfoContext(ctx, "ai detection event",
		"camera_id", in.CameraID,
		"detection_count", len(in.Detections),
	)

	channel, err := w.ipcCore.GetChannel(ctx, in.CameraID)
	if err != nil || channel == nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": 1, "msg": "unknown camera_id"})
		return
	}
	did := channel.DID

	var imagePath string
	if in.Snapshot != "" {
		var err error
		imagePath, err = saveEventSnapshot(in.CameraID, in.Timestamp, in.Snapshot)
		if err != nil {
			w.log.ErrorContext(ctx, "save snapshot failed", "err", err)
		}
	}

	for _, det := range in.Detections {
		zonesJSON, _ := json.Marshal(det.Box)
		_, err := w.eventCore.CreateEventAndNotify(ctx, &event.AddEventInput{
			DID:       did,
			CID:       in.CameraID,
			StartedAt: in.Timestamp,
			EndedAt:   in.Timestamp,
			Label:     det.Label,
			Score:     float32(det.Confidence),
			Zones:     string(zonesJSON),
			ImagePath: imagePath,
			Model:     "default",
		})
		if err != nil {
			w.log.ErrorContext(ctx, "save event failed", "label", det.Label, "err", err)
		}
	}

	c.JSON(http.StatusOK, newDefaultOutputOK())
}

// webhookForwardInput 接收来自其他 gowvp 实例的告警事件 payload
// ImageBase64 非空时解码存文件，ImagePath 作为兜底（旧版本兼容）
type webhookForwardInput struct {
	DID         string   `json:"did"`
	CID         string   `json:"cid"`
	StartedAt   orm.Time `json:"started_at"`
	EndedAt     orm.Time `json:"ended_at"`
	Label       string   `json:"label"`
	Score       float32  `json:"score"`
	Zones       string   `json:"zones"`
	ImageBase64 string   `json:"image_base64"` // 推荐：base64 图片，接收方解码落盘
	ImagePath   string   `json:"image_path"`   // 兜底：旧格式，仅当 ImageBase64 为空时使用
	Model       string   `json:"model"`
}

// saveEventSnapshot 将 Base64 编码的快照保存到 configs/events/{cid}/ 目录
// 返回相对路径: cid/年月日时分秒_随机6位.jpg
func saveEventSnapshot(cid string, t orm.Time, snapshotB64 string) (string, error) {
	eventsDir := filepath.Join(system.Getwd(), "configs", "events")
	cid = strings.TrimSpace(cid)
	if cid == "" || cid == "." || filepath.Base(cid) != cid || strings.ContainsAny(cid, `/\\`) {
		return "", fmt.Errorf("invalid channel id")
	}
	if len(snapshotB64) > base64.StdEncoding.EncodedLen(maxSnapshotBytes) {
		return "", fmt.Errorf("snapshot exceeds %d bytes", maxSnapshotBytes)
	}

	data, err := base64.StdEncoding.DecodeString(snapshotB64)
	if err != nil {
		return "", fmt.Errorf("decode base64: %w", err)
	}

	if len(data) > maxSnapshotBytes {
		return "", fmt.Errorf("snapshot exceeds %d bytes", maxSnapshotBytes)
	}
	ext := ""
	switch http.DetectContentType(data) {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	default:
		return "", fmt.Errorf("unsupported snapshot content type")
	}

	randomSuffix := fmt.Sprintf("%06d", rand.IntN(1000000))
	filename := fmt.Sprintf("%s_%s%s", t.Format("20060102150405"), randomSuffix, ext)

	relativePath := filepath.Join(cid, filename)
	fullPath := filepath.Join(eventsDir, relativePath)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return "", fmt.Errorf("create events dir: %w", err)
	}

	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	slog.Info("event snapshot saved", "path", fullPath, "size", len(data))
	return relativePath, nil
}

func resolveEventImagePath(path string) (string, error) {
	eventsDir := filepath.Join(system.Getwd(), "configs", "events")
	root, err := filepath.Abs(eventsDir)
	if err != nil {
		return "", err
	}
	candidate := filepath.Clean(strings.TrimSpace(path))
	if candidate == "" || candidate == "." {
		return "", fmt.Errorf("empty event image path")
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("event image path escapes storage directory")
	}
	if info, statErr := os.Lstat(candidate); statErr == nil {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("event image is not a regular file")
		}
		evaluatedRoot, rootErr := filepath.EvalSymlinks(root)
		evaluatedPath, pathErr := filepath.EvalSymlinks(candidate)
		if rootErr != nil || pathErr != nil {
			return "", fmt.Errorf("resolve event image symlink")
		}
		evaluatedRel, relErr := filepath.Rel(evaluatedRoot, evaluatedPath)
		if relErr != nil || evaluatedRel == "." || evaluatedRel == ".." || strings.HasPrefix(evaluatedRel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("event image symlink escapes storage directory")
		}
	}
	return filepath.ToSlash(rel), nil
}

// 确保 web 包被引用（web.WrapH 在其他文件中已使用）
var _ = web.WrapH[struct{}, struct{}]
