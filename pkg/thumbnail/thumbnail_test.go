package thumbnail

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestEnsureResizesReusesAndPreservesOriginal(t *testing.T) {
	for _, size := range []image.Point{{X: 1600, Y: 900}, {X: 600, Y: 1200}, {X: 64, Y: 48}} {
		t.Run(size.String(), func(t *testing.T) {
			original := filepath.Join(t.TempDir(), "capture.png")
			var body bytes.Buffer
			if err := png.Encode(&body, image.NewNRGBA(image.Rectangle{Max: size})); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(original, body.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			// 多个首次预览请求必须得到同一张完整的 JPEG。
			var group sync.WaitGroup
			for range 8 {
				group.Go(func() {
					if _, err := Ensure(t.Context(), original); err != nil {
						t.Error(err)
					}
				})
			}
			group.Wait()
			preview, err := os.ReadFile(Path(original))
			if err != nil {
				t.Fatal(err)
			}
			img, err := jpeg.Decode(bytes.NewReader(preview))
			if err != nil {
				t.Fatal(err)
			}
			want := size
			if size.X > MaxSide || size.Y > MaxSide {
				if size.X >= size.Y {
					want = image.Pt(MaxSide, size.Y*MaxSide/size.X)
				} else {
					want = image.Pt(size.X*MaxSide/size.Y, MaxSide)
				}
			}
			if img.Bounds().Size() != want || color.GrayModel.Convert(img.At(0, 0)).(color.Gray).Y < 250 {
				t.Fatal("缩略图比例或透明图片白底错误", img.Bounds())
			}
			before, _ := os.Stat(Path(original))
			if _, err := Ensure(t.Context(), original); err != nil {
				t.Fatal(err)
			}
			after, _ := os.Stat(Path(original))
			if !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("缓存被重复生成")
			}
			if got, err := os.ReadFile(original); err != nil || !bytes.Equal(got, body.Bytes()) {
				t.Fatal("生成缩略图改变了原图", err)
			}
		})
	}
}

func TestRenameRemoveAndMissingOriginal(t *testing.T) {
	dir := t.TempDir()
	original, target := filepath.Join(dir, "old.jpg"), filepath.Join(dir, "new.jpg")
	for _, path := range []string{original, Path(original)} {
		if err := os.WriteFile(path, []byte("saved"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Rename(original, target); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{original, Path(original)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("改名后旧文件残留", path, err)
		}
	}
	for _, path := range []string{target, Path(target)} {
		if got, err := os.ReadFile(path); err != nil || string(got) != "saved" {
			t.Fatal("改名丢失文件", path, err)
		}
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(target)); !os.IsNotExist(err) {
		t.Fatal("原图不存在时未清理孤立缩略图", err)
	}
}

func TestEnsureRejectsSymlinkAndInvalidImages(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "capture.jpg")
	var body bytes.Buffer
	if err := jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, Path(original)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(t.Context(), original); err == nil {
		t.Fatal("允许缩略图覆盖符号链接目标")
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "private" {
		t.Fatal("修改了外部文件", err)
	}
	if err := os.Remove(Path(original)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(t.Context(), original); err == nil {
		t.Fatal("无效图片生成成功")
	}
	if _, err := os.Stat(Path(original)); !os.IsNotExist(err) {
		t.Fatal("失败遗留缩略图", err)
	}
}
