package recording

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gowvp/owl/internal/conf"
)

func TestRecordingPathStaysInsideStorageRoot(t *testing.T) {
	root := t.TempDir()
	core := NewCore(nil, WithConfig(&conf.ServerRecording{StorageDir: root}))

	got, err := core.ResolvePath("channel/day/video.mp4")
	if err != nil {
		t.Fatalf("ResolvePath() error = %v", err)
	}
	want := filepath.Join(root, "channel", "day", "video.mp4")
	if got != want {
		t.Fatalf("ResolvePath() = %q, want %q", got, want)
	}
	if _, err := core.ResolvePath("../outside.mp4"); err == nil {
		t.Fatal("ResolvePath() accepted traversal")
	}
	if _, err := core.ResolvePath(filepath.Join(filepath.Dir(root), "outside.mp4")); err == nil {
		t.Fatal("ResolvePath() accepted absolute path outside storage")
	}
}

func TestRecordingRelativePath(t *testing.T) {
	root := t.TempDir()
	core := NewCore(nil, WithConfig(&conf.ServerRecording{StorageDir: root}))
	want := "channel/video.mp4"
	got, err := core.RelativePath(filepath.Join(root, "channel", "video.mp4"))
	if err != nil {
		t.Fatalf("RelativePath() error = %v", err)
	}
	if got != want {
		t.Fatalf("RelativePath() = %q, want %q", got, want)
	}
}

func TestRecordingPathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	core := NewCore(nil, WithConfig(&conf.ServerRecording{StorageDir: root}))
	if _, err := core.ResolvePath("outside-link/video.mp4"); err == nil {
		t.Fatal("ResolvePath() accepted symlink escape")
	}
}
