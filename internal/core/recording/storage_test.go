package recording_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/core/recording"
	"github.com/gowvp/owl/internal/core/recording/store/recordingdb"
	"github.com/ixugo/goddd/pkg/orm"
)

type storageNodes map[string]recording.NodeStorage

func (nodes storageNodes) RecordingStorage(_ context.Context, id string) (recording.NodeStorage, error) {
	return nodes[id], nil
}

func TestNodeRecordingStorageAndIdempotence(t *testing.T) {
	for _, engine := range []string{"sqlite", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			db := openQueryDatabase(t, engine)
			local, edge := t.TempDir(), t.TempDir()
			nodes := storageNodes{"local": {Root: "/zlm/local", Mount: local}, "edge": {Root: "/zlm/edge", Mount: edge}}
			core := recording.NewCore(recordingdb.NewDB(db), recording.WithConfig(&conf.ServerRecording{StorageDir: local}), recording.WithStorageProvider(nodes))
			// 重复旧行不应阻止新版本迁移，NULL 幂等键不会冲突。
			legacy := []recording.Recording{{Path: "legacy.mp4", Size: 5}, {Path: "legacy.mp4", Size: 5}, {Path: "missing.mp4"}}
			if err := db.Create(&legacy).Error; err != nil {
				t.Fatal(err)
			}
			recordingdb.NewDB(db).AutoMigrate(true)
			if err := os.WriteFile(filepath.Join(local, "legacy.mp4"), []byte("local"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := core.BackfillLocalRecordings(t.Context()); err != nil {
				t.Fatal(err)
			}
			var rows []recording.Recording
			db.Order("id").Find(&rows)
			if rows[0].MediaServerID != "local" || rows[1].MediaServerID != "local" || rows[2].MediaServerID != "" {
				t.Fatalf("旧录像归属回填错误: %+v", rows)
			}
			// 远程文件不存在于平台本机也能入库，不依赖挂载健康状态。
			input, err := core.DescribeFile(t.Context(), "edge", "", "/zlm/edge/video.mp4", 4)
			if err != nil {
				t.Fatal(err)
			}
			input.CID = "ch1"
			input.App = "rtp"
			input.Stream = "ch1"
			input.StartedAt = orm.Time{Time: time.Now().Truncate(time.Second)}
			first, err := core.CreateRecording(t.Context(), &input)
			if err != nil {
				t.Fatal(err)
			}
			second, err := core.CreateRecording(t.Context(), &input)
			if err != nil || second.ID != first.ID {
				t.Fatalf("重复回调未幂等: %v %v", second, err)
			}
			input.MediaServerID = "local"
			input.StorageRoot = "/zlm/local"
			input.FilePath = "/zlm/local/video.mp4"
			other, err := core.CreateRecording(t.Context(), &input)
			if err != nil || other.ID == first.ID {
				t.Fatal("不同节点的相对路径发生冲突", err)
			}
			if _, err := core.ResolveRecordingPath(t.Context(), first); err == nil {
				t.Fatal("无身份标记的挂载被接受")
			}
			if err := os.WriteFile(filepath.Join(edge, recording.StorageMarker), []byte("edge\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(edge, "video.mp4"), []byte("edge"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(local, "video.mp4"), []byte("local"), 0600); err != nil {
				t.Fatal(err)
			}
			full, err := core.ResolveRecordingPath(t.Context(), first)
			if err != nil || full != filepath.Join(edge, "video.mp4") {
				t.Fatal("节点文件解析错误", full, err)
			}
			nodes["edge"] = recording.NodeStorage{Root: "/changed", Mount: edge}
			if _, err := core.ResolveRecordingPath(t.Context(), first); err == nil {
				t.Fatal("原始目录被新配置悄悄覆盖")
			}
			nodes["edge"] = recording.NodeStorage{Root: "/zlm/edge", Mount: edge}
			if _, err := core.DeleteRecording(t.Context(), first.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(edge, "video.mp4")); !os.IsNotExist(err) {
				t.Fatal("远程原文件未删除")
			}
			if body, err := os.ReadFile(filepath.Join(local, "video.mp4")); err != nil || string(body) != "local" {
				t.Fatal("删除错了节点文件")
			}
			if _, err := core.DeleteRecording(t.Context(), legacy[2].ID); err == nil {
				t.Fatal("未知归属的缺失旧文件索引被删除")
			}
		})
	}
}

func TestRecordingStorageSchemaUpgrade(t *testing.T) {
	for _, engine := range []string{"sqlite", "postgres"} {
		t.Run(engine, func(t *testing.T) {
			db := openQueryDatabase(t, engine)
			if err := db.Migrator().DropTable(&recording.Recording{}); err != nil {
				t.Fatal(err)
			}
			type oldRecording struct {
				ID        int64 `gorm:"primaryKey"`
				Path      string
				Size      int64
				StartedAt orm.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
				EndedAt   orm.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
				CreatedAt orm.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
				UpdatedAt orm.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
			}
			if err := db.Table("recordings").AutoMigrate(&oldRecording{}); err != nil {
				t.Fatal(err)
			}
			if err := db.Table("recordings").Create(&[]oldRecording{{Path: "duplicate.mp4", Size: 10}, {Path: "duplicate.mp4", Size: 10}}).Error; err != nil {
				t.Fatal(err)
			}
			recordingdb.NewDB(db).AutoMigrate(true)
			// 再次启动迁移必须可重复执行，且不清空重复旧数据。
			recordingdb.NewDB(db).AutoMigrate(true)
			var rows []recording.Recording
			if err := db.Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 || rows[0].FileKey != nil || rows[1].FileKey != nil || rows[0].MediaServerID != "" {
				t.Fatal("迁移改写了旧记录身份", rows)
			}
			for _, index := range []string{"idx_recordings_file_key", "idx_recordings_node_started"} {
				if !db.Migrator().HasIndex(&recording.Recording{}, index) {
					t.Fatal("缺少索引", index)
				}
			}
		})
	}
}
