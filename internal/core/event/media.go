package event

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gowvp/owl/pkg/thumbnail"
	"github.com/ixugo/goddd/pkg/system"
)

// removeEventMedia 返回真实错误供持久任务重试；仅允许删除证据目录内的文件或链接本身。
func removeEventMedia(relative string) error {
	if relative == "" {
		return nil
	}
	if filepath.IsAbs(relative) {
		return fmt.Errorf("附件路径不能是绝对路径")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("附件路径超出证据目录")
	}
	root, err := filepath.EvalSymlinks(filepath.Join(system.Getwd(), "configs", "events"))
	if err != nil {
		return err
	}
	full := filepath.Join(root, clean)
	// 拒绝父目录链接，既避免越界，也避免不存在的链接目标被误判成已清理。
	parent := root
	parts := strings.Split(filepath.Dir(clean), string(filepath.Separator))
	for _, part := range parts {
		if part == "." {
			continue
		}
		parent = filepath.Join(parent, part)
		info, err := os.Lstat(parent)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("附件父路径不是普通目录")
		}
	}
	for _, path := range []string{full, thumbnail.Path(full)} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("附件不是普通文件或符号链接")
		}
	}
	return thumbnail.Remove(full)
}
