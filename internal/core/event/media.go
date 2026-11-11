package event

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/ixugo/goddd/pkg/system"
)

func removeEventMedia(paths ...string) {
	root, err := filepath.EvalSymlinks(filepath.Join(system.Getwd(), "configs", "events"))
	if err != nil {
		return
	}
	for _, relative := range paths {
		if relative == "" || filepath.IsAbs(relative) {
			continue
		}
		full, err := filepath.EvalSymlinks(filepath.Join(root, relative))
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, full)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if err = os.Remove(full); err != nil && !os.IsNotExist(err) {
			slog.Warn("delete event evidence file", "err", err)
		}
	}
}
