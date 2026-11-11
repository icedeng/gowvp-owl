package event

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gowvp/owl/pkg/thumbnail"
	"github.com/ixugo/goddd/pkg/system"
)

func removeEventMedia(paths ...string) int {
	root, err := filepath.EvalSymlinks(filepath.Join(system.Getwd(), "configs", "events"))
	if err != nil {
		return 0
	}
	deleted := 0
	for _, relative := range paths {
		if relative == "" || filepath.IsAbs(relative) {
			continue
		}
		full := filepath.Clean(filepath.Join(root, relative))
		parent, err := filepath.EvalSymlinks(filepath.Dir(full))
		if err != nil {
			continue
		}
		full = filepath.Join(parent, filepath.Base(full))
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		_, statErr := os.Lstat(full)
		if err = thumbnail.Remove(full); err != nil {
			slog.Warn("delete event evidence file", "err", err)
		} else if statErr == nil {
			deleted++
		}
	}
	return deleted
}
