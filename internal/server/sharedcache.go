package server

import (
	"crypto/rand"
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
		size int64
		tree bool
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
			// A partially deleted extracted module looks valid to Go. Evict the
			// entire module atomically, never individual source files.
			if dir == dirs[1] && d.IsDir() && !strings.HasPrefix(path, "cache/") && (strings.Contains(d.Name(), "@") || strings.HasPrefix(d.Name(), ".tun-trim-")) {
				fi, err := d.Info()
				if err != nil {
					return fs.SkipDir
				}
				size, newest, err := cacheTree(root, path)
				total += size
				if err == nil && newest.Before(cutoff) {
					entries = append(entries, entry{root, path, fi, size, true})
				}
				return fs.SkipDir
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
					entries = append(entries, entry{root, path, fi, fi.Size(), false})
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
		if e.tree {
			// Serialize detaching an extracted module with local job admission.
			_, newest, scanErr := cacheTree(e.root, e.path)
			s.mu.Lock()
			if s.running > 0 || s.svc.running > 0 {
				s.mu.Unlock()
				continue
			}
			trash := ".tun-trim-" + rand.Text()
			if scanErr == nil && newest.Before(cutoff) {
				// Moving a read-only module across parents needs write permission
				// on the directory itself to update its parent link.
				err = e.root.Chmod(e.path, fi.Mode().Perm()|0700)
				if err == nil {
					err = e.root.Rename(e.path, trash)
				}
				if err != nil {
					e.root.Chmod(e.path, fi.Mode().Perm())
				}
			} else {
				err = fs.ErrInvalid
			}
			s.mu.Unlock()
			if err == nil {
				fs.WalkDir(e.root.FS(), trash, func(path string, d fs.DirEntry, err error) error {
					if err == nil && d.IsDir() {
						return e.root.Chmod(path, 0700)
					}
					return err
				})
				err = e.root.RemoveAll(trash)
			}
		} else {
			// Sessions may start after the scan; unlink under the admission lock.
			if e.root.Name() == s.cfg.BazelCaches {
				s.mu.Lock()
				clientDir, _, _ := strings.Cut(e.path, string(filepath.Separator))
				busy := false
				for _, sess := range s.sessions {
					if filepath.Base(s.clientCaches(sess.Client)) == clientDir {
						busy = true
						break
					}
				}
				if busy {
					s.mu.Unlock()
					continue
				}
				err = removeCacheFile(e.root, e.path)
				s.mu.Unlock()
			} else {
				err = removeCacheFile(e.root, e.path)
			}
		}
		if err != nil {
			log.Printf("tun: trimming cache %s: %v", e.path, err)
		}
		if err == nil {
			total -= e.size
			removed += e.size
		}
	}
	if removed > 0 {
		log.Printf("tun: trimmed %s shared cache files; %s remain (recent files/live session caches protected)", fmtBytes(removed), fmtBytes(total))
	}
}

// cacheTree includes directory mtimes so newly extracted empty dirs get the grace too.
func cacheTree(root *os.Root, path string) (size int64, newest time.Time, err error) {
	err = fs.WalkDir(root.FS(), path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		if fi.Mode().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	return
}

// Tool cache directories may be read-only. Restore owner write permission
// temporarily, without following paths outside the cache root.
func removeCacheFile(root *os.Root, path string) error {
	err := root.Remove(path)
	if errors.Is(err, fs.ErrPermission) {
		parent := filepath.Dir(path)
		if p, statErr := root.Stat(parent); statErr == nil && p.IsDir() {
			if root.Chmod(parent, p.Mode().Perm()|0200) == nil {
				err = root.Remove(path)
				root.Chmod(parent, p.Mode().Perm())
			}
		}
	}
	return err
}
