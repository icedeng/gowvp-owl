package api

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/gowvp/owl/pkg/thumbnail"
	"github.com/ixugo/goddd/pkg/system"
	"gorm.io/gorm"
)

func TestOpenEventHostThumbnailOriginalVideoAndPathBoundary(t *testing.T) {
	t.Chdir(t.TempDir())
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := db.DB()
	t.Cleanup(func() { raw.Close() })
	if err = db.AutoMigrate(&event.Event{}); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(system.Getwd(), "configs", "events")
	if err = os.MkdirAll(root, 0750); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err = jpeg.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1600, 900)), nil); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "photo.jpg"), b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "clip.mp4"), []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	ev := event.Event{ImagePath: "photo.jpg", VideoPath: "clip.mp4"}
	if err = db.Create(&ev).Error; err != nil {
		t.Fatal(err)
	}
	h := openHost{uc: &Usecase{DB: db}}
	original, err := h.EventImagePath(context.Background(), ev.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(original)
	if !bytes.Equal(body, b.Bytes()) {
		t.Fatal("原图被替换")
	}
	path, err := h.EventMediaPath(context.Background(), ev.ID, "event-thumbnail")
	if err != nil || path != thumbnail.Path(original) {
		t.Fatal(path, err)
	}
	data, _ := os.ReadFile(path)
	preview, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil || preview.Bounds().Size() != image.Pt(640, 360) {
		t.Fatal(err)
	}
	path, err = h.EventMediaPath(context.Background(), ev.ID, "event-video")
	if err != nil || filepath.Base(path) != "clip.mp4" {
		t.Fatal(path, err)
	}
	outside := filepath.Join(t.TempDir(), "secret.mp4")
	os.WriteFile(outside, []byte("secret"), 0600)
	if err = os.Symlink(outside, filepath.Join(root, "escape.mp4")); err != nil {
		t.Fatal(err)
	}
	if err = db.Model(&ev).Update("video_path", "escape.mp4").Error; err != nil {
		t.Fatal(err)
	}
	if _, err = h.EventMediaPath(context.Background(), ev.ID, "event-download"); err == nil {
		t.Fatal("符号链接越界未拒绝")
	}
}
