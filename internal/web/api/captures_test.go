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
	"github.com/gowvp/owl/pkg/thumbnail"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

func TestSnapshotConfigDefaultsPersistsAndRejectsInvalidValues(t *testing.T) {
	cfg := conf.DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	defaults := snapshotSettings(conf.Media{})
	if defaults != snapshotSettings(cfg.Media) || defaults.UploadTimeoutSeconds != 120 || defaults.FFmpegConcurrency != 2 || defaults.ImmediateDispatch || defaults.ScanIntervalMS != 1000 || defaults.PollIntervalMS != 2000 || defaults.FFmpegMode != "multi_frame" || defaults.FrameCount != 5 || defaults.SkipFrames != 75 {
		t.Fatal(defaults)
	}
	a := ConfigAPI{conf: &cfg, sipMu: new(sync.RWMutex)}
	update := func(body string) (*snapshotConfig, error) {
		var patch map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &patch); err != nil {
			t.Fatal(err)
		}
		return a.updateSnapshotConfig(nil, &patch)
	}
	if _, err := update(`{"upload_timeout_seconds":240,"ffmpeg_concurrency":4,"immediate_dispatch":true,"scan_interval_ms":1500,"poll_interval_ms":500,"ffmpeg_mode":"single_frame","stream_timeout_seconds":180,"ffmpeg_timeout_seconds":90,"rtsp_timeout_seconds":20,"probe_timeout_seconds":15,"analyze_duration_ms":8000,"probe_size_bytes":8000000,"frame_count":8,"skip_frames":100,"frame_interval_ms":500,"jpeg_quality":3}`); err != nil {
		t.Fatal(err)
	}
	want := snapshotSettings(cfg.Media)
	var restored conf.Bootstrap
	if err := conf.SetupConfig(&restored, cfg.ConfigPath); err != nil {
		t.Fatal(err)
	}
	if snapshotSettings(restored.Media) != want || restored.Sip.ID != cfg.Sip.ID || restored.Media.RTPPortRange != cfg.Media.RTPPortRange {
		t.Fatal("未持久化或覆盖其他配置")
	}
	for _, bad := range []string{
		`{"upload_timeout_seconds":0}`, `{"upload_timeout_seconds":3601}`, `{"ffmpeg_concurrency":0}`, `{"ffmpeg_concurrency":33}`,
		`{"scan_interval_ms":99}`, `{"poll_interval_ms":0}`, `{"ffmpeg_mode":"custom"}`, `{"frame_count":1}`, `{"skip_frames":-1}`,
		`{"frame_interval_ms":0}`, `{"ffmpeg_timeout_seconds":200}`, `{"rtsp_timeout_seconds":100}`, `{"ffmpeg_mode":"multi_frame","probe_timeout_seconds":200}`,
		`{"analyze_duration_ms":99}`, `{"probe_size_bytes":0}`, `{"jpeg_quality":32}`, `{"poll_interval_ms":null}`, `{"unknown":1}`, `{"poll_interval_ms":0.5}`,
	} {
		if _, err := update(bad); err == nil {
			t.Fatal("接受非法配置", bad)
		}
	}
	if snapshotSettings(cfg.Media) != want {
		t.Fatal("非法配置污染当前值")
	}
	// 旧客户端省略新增字段时沿用当前值；显式false可关闭即时执行。
	if _, err := update(`{"upload_timeout_seconds":240,"ffmpeg_concurrency":4}`); err != nil {
		t.Fatal(err)
	}
	if snapshotSettings(cfg.Media) != want {
		t.Fatal("旧客户端覆盖了新增配置")
	}
	if _, err := update(`{"immediate_dispatch":false}`); err != nil {
		t.Fatal(err)
	}
	want.ImmediateDispatch = false
	if snapshotSettings(cfg.Media) != want {
		t.Fatal("显式false未生效")
	}
	blocked := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(blocked, []byte("x"), 0600)
	cfg.ConfigPath = filepath.Join(blocked, "config.toml")
	if _, err := update(`{"upload_timeout_seconds":300,"ffmpeg_concurrency":8}`); err == nil {
		t.Fatal("写盘错误未返回")
	}
	if snapshotSettings(cfg.Media) != want {
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
	for _, path := range []string{"/captures", "/captures/missing/image", "/captures/missing/image?thumbnail=true", "/channels/rtsp_capture/snapshot?thumbnail=true"} {
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
	_ = jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 1280, 960)), nil)
	saved, err := a.captures.Complete(t.Context(), row.ID, body.Bytes(), "ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/captures/"+row.ID+"/image?download=true", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "image/jpeg") || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") || w.Body.Len() == 0 {
		t.Fatal("图片无法下载", w.Code, w.Header())
	}
	if !bytes.Equal(w.Body.Bytes(), body.Bytes()) {
		t.Fatal("下载没有保留原图")
	}
	originalPath, _ := a.captures.Path(saved)
	if err := os.Remove(thumbnail.Path(originalPath)); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/captures/"+row.ID+"/image?thumbnail=true", "")
	preview, err := jpeg.Decode(bytes.NewReader(w.Body.Bytes()))
	if w.Code != 200 || err != nil || preview.Bounds().Size() != image.Pt(640, 480) {
		t.Fatal("历史图片没有补生成缩略图", w.Code, err)
	}
	if _, err := os.Stat(thumbnail.Path(originalPath)); err != nil {
		t.Fatal("补生成的缩略图未保存", err)
	}
	w = request("GET", "/captures/"+row.ID+"/image?thumbnail=true&download=true", "")
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), body.Bytes()) {
		t.Fatal("下载受到缩略图参数影响", w.Code)
	}
	w = request("GET", "/channels/rtsp_capture/snapshot?thumbnail=true", "")
	preview, err = jpeg.Decode(bytes.NewReader(w.Body.Bytes()))
	if w.Code != 200 || err != nil || preview.Bounds().Size() != image.Pt(640, 480) {
		t.Fatal("播放器封面没有读取缩略图", w.Code, err)
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
	inputOptions := strings.SplitN(string(args), "-i\n", 2)[0]
	if !strings.Contains(inputOptions, "-threads\n1\n") || !strings.Contains(inputOptions, "-allowed_media_types\nvideo\n") || !strings.Contains(inputOptions, "-probesize\n5000000\n") {
		t.Fatal("单帧解码未限制输入线程/视频，或意外缩短了兼容探测预算", string(args))
	}
	for _, option := range []string{"-rtsp_transport\ntcp\n", "-timeout\n10000000\n", "-analyzeduration\n5000000\n"} {
		if !strings.Contains(inputOptions, option) {
			t.Fatal("取帧优化改变了传输或网络等待参数", string(args))
		}
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

func TestCaptureConfiguredModesSaveValidCandidate(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("shell fake only")
	}
	for _, mode := range []string{"multi_frame", "single_frame"} {
		t.Run(mode, func(t *testing.T) {
			a, db := captureTestAPI(t)
			cfg := conf.DefaultConfig()
			cfg.ConfigDir = t.TempDir()
			if mode == "single_frame" {
				cfg.Media.GBSnapshotFFmpegMode = mode
				cfg.Media.GBSnapshotRTSPTimeoutSeconds = 15
				cfg.Media.GBSnapshotAnalyzeDurationMS = 7000
				cfg.Media.GBSnapshotProbeSizeBytes = 8000000
				cfg.Media.GBSnapshotJPEGQuality = 3
			}
			a.uc = &Usecase{Conf: &cfg}
			a.snapshot = newSnapshotCoordinator(2)
			img := image.NewRGBA(image.Rect(0, 0, 256, 256))
			for y := 0; y < 256; y++ {
				for x := 0; x < 256; x++ {
					i := img.PixOffset(x, y)
					img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(x*y), byte(x*3+y*7), byte(x*11+y*17), 255
				}
			}
			var body bytes.Buffer
			if err := jpeg.Encode(&body, img, nil); err != nil {
				t.Fatal(err)
			}
			fixture := filepath.Join(t.TempDir(), "input.jpg")
			if err := os.WriteFile(fixture, body.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			argsFile, probeFile := filepath.Join(bin, "args"), filepath.Join(bin, "probe")
			t.Setenv("CAPTURE_FIXTURE", fixture)
			t.Setenv("CAPTURE_ARGS", argsFile)
			t.Setenv("CAPTURE_PROBE", probeFile)
			t.Setenv("CAPTURE_MODE", mode)
			script := `#!/bin/sh
for arg do printf '%s\n' "$arg" >> "$CAPTURE_ARGS"; output=$arg; done
if [ "$CAPTURE_MODE" = "multi_frame" ]; then
  printf 'broken image' > "${output%frame-%03d.jpg}frame-001.jpg"
  cp "$CAPTURE_FIXTURE" "${output%frame-%03d.jpg}frame-002.jpg"
else
  cp "$CAPTURE_FIXTURE" "${output%frame-%03d.jpg}frame-001.jpg"
fi
`
			if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			probe := "#!/bin/sh\nprintf 'probe' > \"$CAPTURE_PROBE\"\nprintf 'h264,256,256\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "ffprobe"), []byte(probe), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			row, err := a.captures.EnqueueURL(t.Context(), "rtsp_capture", "manual", "rtsp://127.0.0.1/live/test")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(row).Update("state", "running").Error; err != nil {
				t.Fatal(err)
			}
			if err := a.runCapture(t.Context(), row); err != nil {
				t.Fatal(err)
			}
			saved, err := a.captures.Get(t.Context(), row.ID)
			if err != nil || saved.State != "succeeded" {
				t.Fatal("未永久保存", saved, err)
			}
			path, err := a.captures.Path(saved)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(actual, body.Bytes()) {
				t.Fatal("未选择有效候选图片", err)
			}
			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			_, probeErr := os.Stat(probeFile)
			if mode == "multi_frame" {
				if probeErr != nil || !strings.Contains(string(args), "-frames:v\n5\n") || !strings.Contains(string(args), "select=gte(n\\,75),fps=1000/1000") {
					t.Fatal("默认未走原多帧模式", string(args), probeErr)
				}
				inputOptions := strings.SplitN(string(args), "-i\n", 2)[0]
				if strings.Contains(inputOptions, "-threads\n1\n") || strings.Contains(inputOptions, "-allowed_media_types") {
					t.Fatal("多帧模式意外启用单帧参数", string(args))
				}
			} else {
				if !errors.Is(probeErr, os.ErrNotExist) || strings.Contains(string(args), "-vf") || !strings.Contains(string(args), "-frames:v\n1\n") {
					t.Fatal("单帧模式仍探测或多帧采样", string(args), probeErr)
				}
				for _, option := range []string{"-timeout\n15000000\n", "-analyzeduration\n7000000\n", "-probesize\n8000000\n", "-q:v\n3\n"} {
					if !strings.Contains(string(args), option) {
						t.Fatal("未采用配置的FFmpeg参数", string(args))
					}
				}
			}
			if _, err := os.Stat(readCoverPath(cfg.ConfigDir, "rtsp_capture")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("持久抓拍提前写共享封面", err)
			}
		})
	}
}

func TestSnapshotSettingsHTTPPatchAndRead(t *testing.T) {
	cfg := conf.DefaultConfig()
	cfg.ConfigPath = filepath.Join(t.TempDir(), "config.toml")
	a := ConfigAPI{conf: &cfg, sipMu: new(sync.RWMutex)}
	r := gin.New()
	registerConfig(r, a)
	request := func(method, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/configs/info/snapshot", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		return w
	}
	w := request("PUT", `{"poll_interval_ms":500,"immediate_dispatch":true}`)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("GET", "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var response snapshotConfig
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := snapshotSettings(conf.DefaultConfig().Media)
	want.PollIntervalMS, want.ImmediateDispatch = 500, true
	if response != want {
		t.Fatal("HTTP绑定或返回配置不一致", w.Body.String())
	}
	w = request("PUT", `{"ffmpeg_mode":"unsupported"}`)
	if w.Code == http.StatusOK {
		t.Fatal("HTTP接口接受不支持的模式", w.Body.String())
	}
}
