package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/capture"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func TestSnapshotConfigDefaultsPersistsAndRejectsInvalidValues(t *testing.T) {
	cfg := conf.DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	defaults := snapshotSettings(conf.Media{})
	if defaults.UploadTimeoutSeconds != 120 || defaults.FFmpegConcurrency != 2 {
		t.Fatal(defaults)
	}
	a := ConfigAPI{conf: &cfg, sipMu: new(sync.RWMutex)}
	if _, err := a.updateSnapshotConfig(nil, &snapshotConfig{240, 4}); err != nil {
		t.Fatal(err)
	}
	var restored conf.Bootstrap
	if err := conf.SetupConfig(&restored, cfg.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if restored.Media.GBSnapshotUploadTimeoutSeconds != 240 || restored.Media.GBSnapshotFFmpegConcurrency != 4 || restored.Sip.ID != cfg.Sip.ID || restored.Media.RTPPortRange != cfg.Media.RTPPortRange {
		t.Fatal("未持久化或覆盖其他配置")
	}
	for _, bad := range []snapshotConfig{{0, 2}, {3601, 2}, {120, 0}, {120, 33}} {
		if _, err := a.updateSnapshotConfig(nil, &bad); err == nil {
			t.Fatal("接受非法配置", bad)
		}
	}
	if cfg.Media.GBSnapshotUploadTimeoutSeconds != 240 || cfg.Media.GBSnapshotFFmpegConcurrency != 4 {
		t.Fatal("非法配置污染当前值")
	}
	cfg.ConfigPath = filepath.Join(t.TempDir(), "missing", "config.toml")
	// 使用普通文件作为父目录保证写盘失败。
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, []byte("x"), 0600)
	cfg.ConfigPath = filepath.Join(blocked, "config.toml")
	if _, err := a.updateSnapshotConfig(nil, &snapshotConfig{300, 8}); err == nil {
		t.Fatal("写盘错误未返回")
	}
	if cfg.Media.GBSnapshotUploadTimeoutSeconds != 240 {
		t.Fatal("写盘失败更新了内存")
	}
}

func TestCaptureModeUsesDeclaredProtocolWithoutProbeFallback(t *testing.T) {
	cases := []struct {
		protocol, version string
		disabled          []string
		want              string
	}{
		{ipc.TypeGB28181, "3.0", nil, "gb28181_device_upload"},
		{ipc.TypeGB28181, "2022", nil, "gb28181_device_upload"},
		{ipc.TypeGB28181, "3.0", []string{"snapshot"}, "ffmpeg"},
		{ipc.TypeGB28181, "2.0", nil, "ffmpeg"},
		{ipc.TypeGB28181, "1.0", nil, "ffmpeg"},
		{ipc.TypeOnvif, "3.0", nil, "ffmpeg"},
		{ipc.TypeRTSP, "", nil, "ffmpeg"},
		{ipc.TypeRTMP, "", nil, "ffmpeg"},
	}
	for _, tc := range cases {
		dev := &ipc.Device{Ext: ipc.DeviceExt{GBManualVersion: tc.version, GBDisabledCapabilities: tc.disabled}}
		if got := deviceSnapshotMode(&ipc.Channel{Type: tc.protocol}, dev); got != tc.want {
			t.Fatalf("%s/%s/%v got %s", tc.protocol, tc.version, tc.disabled, got)
		}
		if dev.Ext.GBVersionCapabilities != nil {
			t.Fatal("模式判断污染设备配置")
		}
	}
}

func TestSnapshotConcurrencyChangeDoesNotCreateAnotherSemaphore(t *testing.T) {
	c := newSnapshotCoordinator(2)
	first, _, err := c.acquire(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, _, err := c.acquire(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if release, _, err := c.acquire(ctx, 1); err == nil {
		release()
		t.Fatal("缩小并发后新任务绕过了正在执行的任务")
	}
	second()
	ctx2, cancel2 := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel2()
	if release, _, err := c.acquire(ctx2, 1); err == nil {
		release()
		t.Fatal("仍有一路运行时超过了新并发1")
	}
	first()
	release, _, err := c.acquire(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func captureTestAPI(t *testing.T) (IPCAPI, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "db.sqlite")))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := db.DB()
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	store := ipcdb.NewDB(db).AutoMigrate(true)
	ch := &ipc.Channel{ID: "rtsp_capture", Type: ipc.TypeRTSP, Name: "测试通道", App: "live", Stream: "test"}
	if err := store.Channel().Create(t.Context(), ch); err != nil {
		t.Fatal(err)
	}
	return IPCAPI{ipc: ipc.NewCore(store, uniqueid.Core{}, nil), captures: capture.New(db, filepath.Join(t.TempDir(), "captures"))}, db
}

func TestCaptureAsyncAPIAuthenticationImageRenameDeleteAndPoster(t *testing.T) {
	a, db := captureTestAPI(t)
	auth := AuthMiddleware("capture-test-secret", "")
	r := gin.New()
	registerCaptures(r, a, auth)
	r.POST("/channels/:id/captures", auth, web.WrapH(a.createCapture))
	r.GET("/channels/:id/snapshot", auth, a.getSnapshot)
	// 原图接口与元数据都必须鉴权。
	for _, path := range []string{"/captures", "/captures/missing/image"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatalf("未鉴权 %s=%d", path, w.Code)
		}
	}
	token, err := web.NewToken(map[string]any{}, "capture-test-secret", web.WithExpiresAt(time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	w := request("POST", "/channels/rtsp_capture/captures", "{}")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var row capture.Record
	if err := json.Unmarshal(w.Body.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.State != "queued" {
		t.Fatal("异步接口没有立即排队", row)
	}
	if w := request("GET", "/captures/"+row.ID+"/image", ""); w.Code != 404 {
		t.Fatal("未生成图片却返回成功")
	}
	db.Model(&capture.Record{}).Where("id = ?", row.ID).Update("state", "running")
	var body bytes.Buffer
	_ = jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil)
	saved, err := a.captures.Complete(t.Context(), row.ID, body.Bytes(), "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/captures/"+row.ID+"/image?download=true", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/jpeg") || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || w.Body.Len() == 0 {
		t.Fatal("图片无法下载", w.Code, w.Header())
	}
	w = request("GET", "/channels/rtsp_capture/snapshot", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), request("GET", "/captures/"+row.ID+"/image", "").Body.Bytes()) {
		t.Fatal("播放器封面没有使用最新抓拍")
	}
	w = request("PUT", "/captures/"+row.ID, `{"name":"new.jpg"}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	saved.Name = "new.jpg"
	path, _ := a.captures.Path(saved)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	w = request("PUT", "/captures/"+row.ID, `{"name":"../escape.jpg"}`)
	if w.Code == 200 {
		t.Fatal("允许路径穿越")
	}
	w = request("DELETE", "/captures/"+row.ID, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("接口未删除图片文件")
	}
	if w := request("GET", "/captures/"+row.ID+"/image", ""); w.Code != http.StatusNotFound {
		t.Fatal("删除后仍返回图片")
	}
}

func TestFastFFmpegPathDoesNotProbeWait75FramesOrWriteSharedCover(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("shell fake only")
	}
	a, _ := captureTestAPI(t)
	cfg := conf.DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	a.uc = &Usecase{Conf: &cfg}
	a.snapshot = newSnapshotCoordinator(2)
	var body bytes.Buffer
	_ = jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil)
	fixture := filepath.Join(t.TempDir(), "input.jpg")
	_ = os.WriteFile(fixture, body.Bytes(), 0600)
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	// 通过环境变量传递路径，避免路径转义和 shell 注入。
	t.Setenv("CAPTURE_FIXTURE", fixture)
	t.Setenv("CAPTURE_ARGS", argsFile)
	script := "#!/bin/sh\nfor arg do printf '%s\\n' \"$arg\" >> \"$CAPTURE_ARGS\"; output=$arg; done\ncp \"$CAPTURE_FIXTURE\" \"${output%frame-%03d.jpg}frame-001.jpg\"\n"
	_ = os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0700)
	_ = os.WriteFile(filepath.Join(bin, "ffprobe"), []byte("#!/bin/sh\nexit 99\n"), 0700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	result, err := a.refreshGBSnapshotByFFmpeg(context.Background(), "ch", "rtsp://127.0.0.1/live/ch", time.Now(), 1)
	if err != nil || len(result.Image) == 0 {
		t.Fatal("快路径仍依赖ffprobe或取帧失败", err)
	}
	args, _ := os.ReadFile(argsFile)
	if strings.Contains(string(args), "select=") || !strings.Contains(string(args), "-frames:v\n1\n") {
		t.Fatal(string(args))
	}
	if _, err := os.Stat(readCoverPath(cfg.ConfigDir, "ch")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("快路径仍写入共享封面")
	}
}

func TestSnapshotConcurrencyIncreaseWakesQueuedTask(t *testing.T) {
	c := newSnapshotCoordinator(1)
	first, _, err := c.acquire(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	admitted := make(chan func(), 1)
	go func() {
		release, _, err := c.acquire(ctx, 1)
		if err == nil {
			admitted <- release
		}
	}()
	select {
	case release := <-admitted:
		release()
		t.Fatal("超过原并发上限")
	case <-time.After(20 * time.Millisecond):
	}
	c.setLimit(2)
	select {
	case release := <-admitted:
		release()
	case <-ctx.Done():
		t.Fatal("调高并发没有唤醒排队任务")
	}
}

func TestTransientChannelReadErrorDoesNotDisableSchedule(t *testing.T) {
	a, db := captureTestAPI(t)
	if err := a.captures.SaveSchedule(t.Context(), &capture.Schedule{ChannelID: "rtsp_capture", Enabled: true, IntervalSeconds: 300}); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable(&ipc.Channel{}); err != nil {
		t.Fatal(err)
	}
	if err := a.runCapture(t.Context(), &capture.Record{ChannelID: "rtsp_capture"}); err == nil {
		t.Fatal("读库异常未返回")
	}
	plan, err := a.captures.GetSchedule(t.Context(), "rtsp_capture")
	if err != nil || !plan.Enabled {
		t.Fatal("读库故障被误判为通道删除，关闭了计划", plan, err)
	}
}
