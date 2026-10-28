package main

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func backupMembers(state string) ([]string, error) {
	names := []string{"release.json", "data.img"}
	if _, err := os.Lstat(filepath.Join(state, "gateway.json")); err == nil {
		names = append(names, "gateway.json")
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	root := filepath.Join(state, "gateway")
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return names, nil
	} else if err != nil {
		return nil, err
	}
	var total int64
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("gateway backup rejects symlinks")
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(state, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !info.Mode().IsRegular() || !validBackupMember(rel, info.Size()) {
			return errors.New("invalid gateway backup member")
		}
		names = append(names, rel)
		total += info.Size()
		if len(names) > 4096 || total > 60<<20 {
			return errors.New("gateway backup metadata exceeds limit")
		}
		return nil
	})
	return names, err
}

func validBackupMember(name string, size int64) bool {
	if size < 0 || strings.ContainsAny(name, "\\:") || !fs.ValidPath(name) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") || strings.ContainsAny(part, "\x00\r\n") {
			return false
		}
	}
	switch name {
	case "data.img":
		return size <= maxDataDiskBytes
	case sparseMapName:
		return size <= sparseMapLimit
	case "release.json", "gateway.json":
		return size <= 1<<20
	default:
		return strings.HasPrefix(name, "gateway/caddy/") && size <= 16<<20
	}
}
