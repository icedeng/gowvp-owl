package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/ipc/store/ipcdb"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/recording/adapter"
	"github.com/gowvp/owl/internal/core/recording/store/recordingdb"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/core/sms/store/smsdb"
	"github.com/ixugo/goddd/domain/uniqueid"
	"github.com/ixugo/goddd/pkg/orm"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestRecordingNodeWebhookPlaybackAndDeletion(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "recording.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	ipcStore := ipcdb.NewDB(db).AutoMigrate(true)
	nodeStore := smsdb.NewDB(db).AutoMigrate(true)
	recordStore := recordingdb.NewDB(db).AutoMigrate(true)
	local, edge := t.TempDir(), t.TempDir()
	for _, node := range []sms.MediaServer{
		{ID: "local", IP: "127.0.0.1", Secret: "local-secret", RecordPath: "/nodes/local", RecordMountPath: local},
		{ID: "edge", IP: "127.0.0.1", HookIP: "127.0.0.1", Secret: "edge-secret", Disabled: true, RTPPortRange: "30000-50000", Ports: sms.MediaServerPorts{HTTP: 8080}, RecordPath: "/nodes/edge", RecordMountPath: edge},
	} {
		if err := db.Create(&node).Error; err != nil {
			t.Fatal(err)
		}
	}
	nodes := sms.NewCore(nodeStore)
	defer nodes.Close()
	cfg := conf.DefaultConfig()
	cfg.Server.Recording.StorageDir = local
	core := recording.NewCore(recordStore, recording.WithConfig(&cfg.Server.Recording), recording.WithSMSProvider(adapter.NewSMSAdapter(nodes)))
	ch := ipc.Channel{ID: "ch1", App: "rtp", Stream: "ch1", Config: ipc.StreamConfig{MediaServerID: "local"}}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatal(err)
	}
	hook := WebHookAPI{smsCore: nodes, ipcCore: ipc.NewCore(ipcStore, uniqueid.Core{}, nil), recordingCore: core, conf: &cfg, log: slog.Default()}
	router := gin.New()
	registerZLMWebhookAPI(router, hook)
	RegisterRecording(router, NewRecordingAPI(core, &cfg), func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer test" {
			c.AbortWithStatus(401)
		}
	})
	body, _ := json.Marshal(onRecordMP4Input{MediaServerID: "edge", App: "rtp", Stream: "ch1", FilePath: "/nodes/edge/video.mp4", FileSize: 10, StartTime: time.Now().Unix(), TimeLen: 10, Vhost: recording.DefaultVhost})
	for range 2 {
		req := httptest.NewRequest("POST", "/webhook/on_record_mp4?media_server_id=edge&secret=edge-secret", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != 200 {
			t.Fatalf("停用节点末段回调失败: %d %s", res.Code, res.Body.String())
		}
	}
	var recs []recording.Recording
	db.Find(&recs)
	if len(recs) != 1 || recs[0].MediaServerID != "edge" || recs[0].CID != "ch1" {
		t.Fatalf("回调归属或幂等失败: %+v", recs)
	}
	rec := recs[0]
	// 尚未挂载时可入库，但不可误读本地的同名文件。
	if err := os.WriteFile(filepath.Join(local, "video.mp4"), []byte("local-file"), 0600); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, rangeHeader string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer test")
		}
		if rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	fileURL := fmt.Sprintf("/recordings/%d/file", rec.ID)
	if res := request("GET", fileURL, "", false); res.Code != 401 {
		t.Fatal("文件路由缺少鉴权", res.Code)
	}
	if res := request("GET", fileURL, "", true); res.Code != 503 {
		t.Fatal("失联挂载未返回不可用", res.Code)
	}
	if _, err := core.DeleteRecording(t.Context(), rec.ID); err == nil {
		t.Fatal("无挂载时删除了远程索引")
	}
	if err := os.WriteFile(filepath.Join(edge, recording.StorageMarker), []byte("edge"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(edge, "video.mp4"), []byte("edge-file!"), 0600); err != nil {
		t.Fatal(err)
	}
	res := request("GET", fileURL, "bytes=2-5", true)
	if res.Code != 206 || res.Body.String() != "ge-f" || res.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("节点 Range 读取错误: %d %s %v", res.Code, res.Body.String(), res.Header())
	}
	res = request("HEAD", fileURL, "", true)
	if res.Code != 200 || res.Header().Get("Content-Length") != "10" || res.Body.Len() != 0 {
		t.Fatal("HEAD 不兼容", res.Code, res.Header())
	}
	res = request("GET", fmt.Sprintf("/recordings/%d/download", rec.ID), "bytes=0-3", true)
	if res.Code != 206 || res.Body.String() != "edge" || !strings.Contains(res.Header().Get("Content-Disposition"), "attachment") {
		t.Fatal("节点下载失败", res.Code, res.Body.String())
	}
	// 开放平台沿用同一节点解析器，停用不妨碍历史录像读取。
	host := openHost{uc: &Usecase{DB: db, RecordingAPI: NewRecordingAPI(core, &cfg)}}
	if file, err := host.RecordingPath(t.Context(), rec.ID); err != nil || file != filepath.Join(edge, "video.mp4") {
		t.Fatal("开放平台读错节点", file, err)
	}
	if _, err := nodes.DeleteMediaServer(t.Context(), "edge"); err == nil {
		t.Fatal("含历史录像的节点被删除")
	}
	changed := "/changed"
	if _, err := nodes.UpdateMediaServer(t.Context(), &sms.EditMediaServerInput{IP: "127.0.0.1", HookIP: "127.0.0.1", RecordPath: &changed}, "edge", 15123); err == nil {
		t.Fatal("有历史录像时更换了存储根目录")
	}
	// 篡改来源不能借用 local secret 访问 edge 回调。
	req := httptest.NewRequest("POST", "/webhook/on_record_mp4?secret=local-secret", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	res = httptest.NewRecorder()
	router.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatal("跨节点回调伪造被接受")
	}
	res = request("DELETE", fmt.Sprintf("/recordings/%d", rec.ID), "", true)
	if res.Code != 200 {
		t.Fatalf("节点文件删除失败: %d %s", res.Code, res.Body.String())
	}
	if _, err := os.Stat(filepath.Join(edge, "video.mp4")); !os.IsNotExist(err) {
		t.Fatal("节点文件仍存在")
	}
	if data, err := os.ReadFile(filepath.Join(local, "video.mp4")); err != nil || string(data) != "local-file" {
		t.Fatal("误删了本地同名文件")
	}
	var count int64
	db.Model(&recording.Recording{}).Count(&count)
	if count != 0 {
		t.Fatal("删除后录像索引仍存在")
	}
	// 生成的播放列表只引用平台录像 ID，不泄露路径或节点密钥。
	rec.StartedAt = orm.Now()
	playlist := NewRecordingAPI(core, &cfg).generateM3U8WithToken([]*recording.Recording{&rec}, "", "test+token")
	if !strings.Contains(playlist, fileURL+"?token=test%2Btoken") || strings.Contains(playlist, "/nodes/") || strings.Contains(playlist, "edge-secret") {
		t.Fatal("播放列表暴露内部地址或凭据", playlist)
	}
}
