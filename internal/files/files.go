// Package files handles locating completed slskd downloads on disk and moving
// them into the Navidrome library. Downloads are located by basename so we don't
// depend on slskd's exact folder layout, and moves fall back to copy+remove when
// source and destination are on different filesystems (the common case with
// separate mounted volumes).
package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// BaseName returns the final path component of a slskd remote filename, which
// uses backslash separators.
func BaseName(remoteFilename string) string {
	s := strings.ReplaceAll(remoteFilename, "\\", "/")
	return filepath.Base(s)
}

// FindByBasename searches root recursively for a file with the given basename
// and returns its full path. ok is false if not found.
func FindByBasename(root, basename string) (string, bool, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if !d.IsDir() && d.Name() == basename {
			found = path
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return found, found != "", nil
}

// SanitizeFilename makes s safe to use as a single path component: it replaces
// filesystem-reserved characters, collapses whitespace, drops control
// characters, and trims problematic leading/trailing spaces and dots. It
// deliberately preserves the original casing and punctuation otherwise.
func SanitizeFilename(s string) string {
	s = strings.NewReplacer(
		"/", "_", "\\", "_", ":", "_", "*", "_", "?", "_",
		"\"", "_", "<", "_", ">", "_", "|", "_",
	).Replace(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.Trim(s, " .")
}

// Move moves src into dstDir as filename, returning the final path. If filename
// is empty, src's base name is used. If a file with the same name already
// exists, a numeric suffix is appended. Falls back to copy+remove across
// filesystems.
func Move(src, dstDir, filename string) (string, error) {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return "", fmt.Errorf("create dest dir: %w", err)
	}
	if filename == "" {
		filename = filepath.Base(src)
	}
	dst := uniquePath(filepath.Join(dstDir, filename))

	// Fast path: same-filesystem rename. Falls back to copy+remove on any
	// failure (typically EXDEV when src and dst are on different mounts).
	if err := os.Rename(src, dst); err == nil {
		return dst, nil
	}
	if err := copyFile(src, dst); err != nil {
		return "", fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	if err := os.Remove(src); err != nil {
		return dst, fmt.Errorf("copied but failed to remove source: %w", err)
	}
	return dst, nil
}

// uniquePath returns p, or p with a " (n)" suffix before the extension if p
// already exists.
func uniquePath(p string) string {
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate
		}
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
