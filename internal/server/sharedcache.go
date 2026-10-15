package server

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// trimSharedCaches bounds disposable tool caches, never following symlinks. os.Root
// also prevents a job replacing a parent with a symlink during traversal/removal.
// mtime is an approximation of use: reads need not update it. Recent writes get an
// hour's grace, and live session clients' Bazel caches are excluded entirely.
func (s *Server) trimSharedCaches() {
	if s.cfg.MaxSharedCache <= 0 && !s.diskLow() {
		return
	}
	s.gc.workMu.Lock()
	defer s.gc.workMu.Unlock()
	active := map[string]bool{}
	s.mu.Lock()
	for _, sess := range s.sessions {
		active[filepath.Base(s.clientCaches(sess.Client))] = true
	}
	s.mu.Unlock()
	type entry struct {
		root *os.Root
		path string
		info fs.FileInfo
	}
	var entries []entry
	var total int64
	cutoff := time.Now().Add(-time.Hour)
	dirs := []string{filepath.Join(s.cfg.DataDir, "cache/go-build"), filepath.Join(s.cfg.DataDir, "cache/gomod"), filepath.Join(s.cfg.DataDir, "cache/xdg"), s.cfg.BazelCaches}
	for _, dir := range dirs {
		// The mount root itself is server-owned; refuse symlinks here as well.
		fi, err := os.Lstat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			continue
		}
		defer root.Close()
		fs.WalkDir(root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			clientDir, _, _ := strings.Cut(path, string(filepath.Separator))
			protected := dir == s.cfg.BazelCaches && active[clientDir]
			if d.Type().IsRegular() {
				fi, err := d.Info()
				if err != nil {
					return nil
				}
				total += fi.Size()
				if !protected && fi.ModTime().Before(cutoff) {
					entries = append(entries, entry{root, path, fi})
				}
			}
			return nil
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].info.ModTime().Before(entries[j].info.ModTime()) })
	var removed int64
	for _, e := range entries {
		if (s.cfg.MaxSharedCache <= 0 || total <= s.cfg.MaxSharedCache) && !s.diskLow() {
			break
		}
		fi, err := e.root.Lstat(e.path)
		if err != nil || !os.SameFile(fi, e.info) || fi.ModTime() != e.info.ModTime() || fi.Size() != e.info.Size() {
			continue
		}
		err = e.root.Remove(e.path)
		// Extracted Go modules have read-only directories. Temporarily restore owner
		// write permission on the containing directory, as go clean -modcache does.
		if errors.Is(err, fs.ErrPermission) {
			parent := filepath.Dir(e.path)
			if p, statErr := e.root.Stat(parent); statErr == nil && p.IsDir() {
				if e.root.Chmod(parent, p.Mode().Perm()|0200) == nil {
					err = e.root.Remove(e.path)
					e.root.Chmod(parent, p.Mode().Perm())
				}
			}
		}
		if err == nil {
			total -= fi.Size()
			removed += fi.Size()
		}
	}
	if removed > 0 {
		log.Printf("tun: trimmed %s shared cache files; %s remain (recent files/live session caches protected)", fmtBytes(removed), fmtBytes(total))
	}
}
