// Package thumbnail 保存图片的轻量预览，原图保持不变。
package thumbnail

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/image/draw"
)

const MaxSide = 640

var locks [64]sync.Mutex
var slots = make(chan struct{}, 2)

// Path 使用隐藏的同目录文件，避免与用户可重命名的原图冲突。
func Path(original string) string {
	return filepath.Join(filepath.Dir(original), "."+filepath.Base(original)+".thumb.jpg")
}

func lockIndex(path string) uint32 {
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(path)))
	return h.Sum32() % uint32(len(locks))
}

// Ensure 为新图片生成预览，也为历史图片按需补齐；并发请求复用已有文件。
func Ensure(ctx context.Context, original string) (string, error) {
	mu := &locks[lockIndex(original)]
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Lstat(original)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 20<<20 || strings.HasPrefix(filepath.Base(original), ".") {
		return "", fmt.Errorf("无效原图")
	}
	target := Path(original)
	if cached, err := os.Lstat(target); err == nil {
		if !cached.Mode().IsRegular() {
			return "", fmt.Errorf("无效缩略图路径")
		}
		if cached.Size() > 0 && !cached.ModTime().Before(info.ModTime()) {
			return target, nil
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	file, err := os.Open(original)
	if err != nil {
		return "", err
	}
	defer file.Close()
	cfg, format, err := image.DecodeConfig(file)
	if err != nil || (format != "jpeg" && format != "png") || cfg.Width < 1 || cfg.Height < 1 || int64(cfg.Width)*int64(cfg.Height) > 32000000 {
		return "", fmt.Errorf("无效图片或分辨率过大")
	}
	if _, err := file.Seek(0, 0); err != nil {
		return "", err
	}
	source, _, err := image.Decode(file)
	if err != nil {
		return "", err
	}
	w, h := cfg.Width, cfg.Height
	if w > MaxSide || h > MaxSide {
		if w >= h {
			h, w = max(1, h*MaxSide/w), MaxSide
		} else {
			w, h = max(1, w*MaxSide/h), MaxSide
		}
	}
	preview := image.NewRGBA(image.Rect(0, 0, w, h))
	// 透明 PNG 在白色背景上缩放，避免 JPEG 预览出现黑底。
	draw.Draw(preview, preview.Bounds(), image.White, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(preview, preview.Bounds(), source, source.Bounds(), draw.Over, nil)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".thumbnail-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	encodeErr := jpeg.Encode(tmp, preview, &jpeg.Options{Quality: 75})
	modeErr := tmp.Chmod(0640)
	closeErr := tmp.Close()
	if err := errors.Join(encodeErr, modeErr, closeErr); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return "", err
	}
	return target, nil
}

func remove(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Remove 与生成操作互斥，避免删除期间补生成孤立缩略图。
func Remove(original string) error {
	mu := &locks[lockIndex(original)]
	mu.Lock()
	defer mu.Unlock()
	return errors.Join(remove(original), remove(Path(original)))
}

// Rename 同步移动原图及已有缩略图，失败时恢复原图名称。
func Rename(original, target string) error {
	if original == target {
		return nil
	}
	a, b := lockIndex(original), lockIndex(target)
	if a > b {
		a, b = b, a
	}
	locks[a].Lock()
	defer locks[a].Unlock()
	if a != b {
		locks[b].Lock()
		defer locks[b].Unlock()
	}
	if err := os.Rename(original, target); err != nil {
		return err
	}
	if err := os.Rename(Path(original), Path(target)); err != nil {
		if os.IsNotExist(err) {
			// 历史原图尚未生成缩略图，移除目标旧缓存以便下次补齐。
			if removeErr := remove(Path(target)); removeErr != nil {
				return errors.Join(removeErr, os.Rename(target, original))
			}
			return nil
		}
		return errors.Join(err, os.Rename(target, original))
	}
	return nil
}
