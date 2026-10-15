package server

import (
	"crypto/rand"
	"io/fs"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Tool cache trimming. Tools trust whole trees, not single files: Go treats an extracted
// module dir (and its download files: a missing .ziphash makes go delete and re-extract the
// module) as one thing, Bazel a repository cache entry or a fetched repository tree, and
// any tool its own install dir. So the unit of eviction is what the tool can lose without
// being misled:
//
//   - cache/go-build: single files (Go handles each missing entry as a miss).
//   - cache/gomod: an extracted module <path>@<version> with its cache/download/<path>/@v/
//     <version>.* files. Download files without an extracted module are never trimmed.
//   - cache/xdg: each top-level dir (a tool's whole cache), except a Bazel repository cache
//     (tun-bazel/repo, as bazel-ci.sh uses it), trimmed per entry, and a Bazel disk cache
//     (tun-bazel/disk), which Bazel's own GC bounds (see below).
//   - per-client Bazel caches (BazelCaches/<client>/): repository cache entries,
//     content_addressable/<algo>/<hash> and contents/<hash> (fetched repository trees);
//     the disk cache is left to Bazel's GC (--experimental_disk_cache_gc_max_size), and
//     bazelisk/ (the client's Bazel binaries) is never trimmed.
//
// A tree in the shared caches is only evicted while no local job or service runs (they may
// be reading it: Bazel symlinks fetched repositories, Go compiles module sources), and a
// client's Bazel caches only while it has no live session; both are checked under s.mu, so
// none can start until the tree is renamed away. Files are removed oldest-use first. Use is
// approximated by the newest mtime in the unit (dirs included: an extracted archive keeps
// its old file mtimes, its dirs do not), clamped to now: a future mtime is reset to now, so
// it can't pin an entry forever. Go refreshes a used go-build entry's mtime at most hourly,
// so go-build entries get Go's own trim grace (5 days + 1 hour) except under disk pressure.
//
// Budgets: --max-shared-cache bounds go-build, gomod and xdg together (Bazel disk caches
// excluded); --max-client-cache bounds each client's Bazel caches (repository cache; disk
// cache and bazelisk excluded). Under --min-free pressure every unit past its grace is a
// candidate, Bazel disk cache files too (Bazel treats a missing entry as a miss), oldest first.

const (
	trimGrace    = time.Hour
	goBuildGrace = 5*24*time.Hour + time.Hour
	trashPrefix  = ".tun-trim-"
)

// cacheUnit is what trimming evicts at once.
type cacheUnit struct {
	root      *os.Root
	path      string   // relative to root: a file or a tree
	extra     []string // files evicted with it (an extracted module's download files)
	size      int64
	newest    time.Time
	info      fs.FileInfo // of path, to see it unchanged before removal
	grace     time.Duration
	client    string // client cache dir name: needs no live session of that client
	diskOnly  bool   // left to Bazel's GC; trimmed only under disk pressure
	filesOnly bool   // dirs are not units, only the files in them
	hasRoot   bool   // gomod: download files whose module is not extracted have none
}

// trimSharedCaches applies the tool cache budgets (see above). Concurrent calls skip.
func (s *Server) trimSharedCaches() {
	low := s.diskLow()
	if s.cfg.MaxSharedCache <= 0 && s.cfg.MaxClientCache <= 0 && !low {
		return
	}
	if !s.gc.trimMu.TryLock() {
		return
	}
	defer s.gc.trimMu.Unlock()
	now := time.Now()
	var units []*cacheUnit
	var shared int64
	perClient := map[string]int64{}
	var roots []*os.Root
	defer func() {
		for _, r := range roots {
			r.Close()
		}
	}()
	scan := func(dir string, classify func(rel []string) (key string, u cacheUnit, ok bool)) {
		fi, err := os.Lstat(dir) // the mount root is server-owned; refuse a symlink there too
		if err != nil || !fi.IsDir() {
			return
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			return
		}
		roots = append(roots, root)
		byKey := map[string]*cacheUnit{}
		var leftovers []string
		fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == "." {
				return nil
			}
			parts := strings.Split(p, "/")
			if strings.HasPrefix(parts[len(parts)-1], trashPrefix) {
				leftovers = append(leftovers, p) // an interrupted eviction
				return fs.SkipDir
			}
			key, proto, ok := classify(parts)
			if key == "-" {
				return fs.SkipDir // never trimmed nor counted
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			var size int64
			if fi.Mode().IsRegular() {
				size = fi.Size()
			}
			if !proto.diskOnly {
				if proto.client != "" {
					perClient[proto.client] += size
				} else {
					shared += size
				}
			}
			if !ok || proto.filesOnly && d.IsDir() {
				return nil
			}
			u := byKey[key]
			if u == nil {
				u = &proto
				u.root, u.path = root, key
				byKey[key] = u
				units = append(units, u)
			}
			if p == key {
				u.info, u.hasRoot = fi, true
			} else if proto.extra != nil {
				u.extra = append(u.extra, p)
			}
			u.size += size
			mt := fi.ModTime()
			if mt.After(now) {
				root.Chtimes(p, now, now)
				mt = now
			}
			if mt.After(u.newest) {
				u.newest = mt
			}
			return nil
		})
		for _, p := range leftovers {
			removeTree(root, p)
		}
	}
	data := s.cfg.DataDir + "/cache/"
	scan(data+"go-build", func(rel []string) (string, cacheUnit, bool) {
		return strings.Join(rel, "/"), cacheUnit{grace: goBuildGrace, filesOnly: true}, true
	})
	scan(data+"gomod", func(rel []string) (string, cacheUnit, bool) {
		if rel[0] == "cache" {
			// cache/download/<path>/@v/<version>.<ext> goes with <path>@<version>.
			n := len(rel)
			if n >= 5 && rel[1] == "download" && rel[n-2] == "@v" {
				ver := strings.TrimSuffix(rel[n-1], path.Ext(rel[n-1]))
				return path.Join(rel[2:n-2]...) + "@" + ver, cacheUnit{grace: trimGrace, extra: []string{}}, true
			}
			return "", cacheUnit{}, false
		}
		for i, c := range rel {
			if strings.Contains(c, "@") {
				return strings.Join(rel[:i+1], "/"), cacheUnit{grace: trimGrace}, true
			}
		}
		return "", cacheUnit{}, false
	})
	scan(data+"xdg", func(rel []string) (string, cacheUnit, bool) {
		u := cacheUnit{grace: trimGrace}
		if rel[0] == "tun-bazel" {
			return bazelCacheKey(rel, u)
		}
		return rel[0], u, true
	})
	scan(s.cfg.BazelCaches, func(rel []string) (string, cacheUnit, bool) {
		if len(rel) < 2 {
			return "", cacheUnit{client: rel[0]}, false
		}
		if rel[1] == "bazelisk" {
			return "-", cacheUnit{}, false
		}
		return bazelCacheKey(rel, cacheUnit{client: rel[0], grace: trimGrace})
	})
	for _, u := range units {
		if u.extra != nil && !u.hasRoot {
			u.grace = -1 // download files alone are never trimmed
		}
	}
	sort.Slice(units, func(i, j int) bool { return units[i].newest.Before(units[j].newest) })

	var removed int64
	evict := func(over func(u *cacheUnit) bool, pressure bool) {
		for _, u := range units {
			if u.size < 0 || !over(u) || u.grace < 0 || u.diskOnly && !pressure {
				continue
			}
			grace := u.grace
			if pressure {
				grace = min(grace, trimGrace)
			}
			if now.Sub(u.newest) < grace || !s.evictUnit(u, now.Add(-grace)) {
				continue
			}
			if !u.diskOnly {
				if u.client != "" {
					perClient[u.client] -= u.size
				} else {
					shared -= u.size
				}
			}
			removed += u.size
			u.size = -1 // gone
		}
	}
	if s.cfg.MaxSharedCache > 0 {
		evict(func(u *cacheUnit) bool { return u.client == "" && shared > s.cfg.MaxSharedCache }, false)
	}
	if s.cfg.MaxClientCache > 0 {
		evict(func(u *cacheUnit) bool { return u.client != "" && perClient[u.client] > s.cfg.MaxClientCache }, false)
	}
	if s.diskLow() {
		evict(func(*cacheUnit) bool { return s.diskLow() }, true)
	}
	if removed > 0 {
		log.Printf("tun: trimmed %s of tool caches; %s shared remain (recently used entries, running jobs' and live sessions' caches are protected)", fmtBytes(removed), fmtBytes(shared))
	}
}

// bazelCacheKey is the unit of rel in a Bazel cache dir rel[0] (holding repo/ and disk/).
func bazelCacheKey(rel []string, u cacheUnit) (string, cacheUnit, bool) {
	n := len(rel)
	switch {
	case n < 2:
		return "", u, false
	case rel[1] == "disk":
		u.diskOnly, u.filesOnly = true, true
		return strings.Join(rel, "/"), u, true
	case rel[1] != "repo":
		return strings.Join(rel[:2], "/"), u, true
	case n >= 5 && rel[2] == "content_addressable": // repo/content_addressable/<algo>/<hash>
		return strings.Join(rel[:5], "/"), u, true
	case n >= 4 && rel[2] == "contents": // repo/contents/<hash>: a fetched repository's trees
		return strings.Join(rel[:4], "/"), u, true
	case n >= 3 && rel[2] != "content_addressable" && rel[2] != "contents":
		return strings.Join(rel[:3], "/"), u, true
	}
	return "", u, false
}

// evictUnit removes u if it is unchanged since the scan, still unused since cutoff, and
// nothing that may use it runs.
func (s *Server) evictUnit(u *cacheUnit, cutoff time.Time) bool {
	fi, err := u.root.Lstat(u.path)
	if err != nil || !os.SameFile(fi, u.info) {
		return false
	}
	tree := fi.IsDir()
	if tree {
		if _, newest, err := cacheTree(u.root, u.path); err != nil || newest.After(cutoff) {
			return false
		}
	} else if !fi.ModTime().Equal(u.info.ModTime()) || fi.Size() != u.info.Size() {
		return false
	}
	// Under the admission lock: no job, service or session can start meanwhile.
	s.mu.Lock()
	busy := false
	if u.client != "" {
		for _, sess := range s.sessions {
			busy = busy || s.clientCaches(sess.Client) == s.cfg.BazelCaches+"/"+u.client
		}
	} else if tree || u.extra != nil {
		busy = s.running > 0 || s.svc.running > 0
	}
	if busy {
		s.mu.Unlock()
		return false
	}
	trash := trashPrefix + rand.Text()
	if tree {
		err = detach(u.root, u.path, trash)
	} else {
		err = removeCacheFile(u.root, u.path)
	}
	for _, p := range u.extra {
		if err == nil {
			err = removeCacheFile(u.root, p)
		}
	}
	s.mu.Unlock()
	if tree && err == nil {
		err = removeTree(u.root, trash)
	}
	if err != nil {
		log.Printf("tun: trimming cache %s: %v", u.path, err)
	}
	return err == nil
}

// detach renames tree path to trash at the root. Moving a dir to another parent updates its
// "..", which needs write permission on the dir (Go's module dirs are read-only), and on
// the parent.
func detach(root *os.Root, p, trash string) error {
	fi, err := root.Lstat(p)
	if err != nil {
		return err
	}
	if err := root.Chmod(p, fi.Mode().Perm()|0o700); err != nil {
		return err
	}
	err = root.Rename(p, trash)
	if parent, perr := root.Lstat(path.Dir(p)); err != nil && perr == nil && parent.IsDir() {
		if root.Chmod(path.Dir(p), parent.Mode().Perm()|0o700) == nil {
			err = root.Rename(p, trash)
			root.Chmod(path.Dir(p), parent.Mode().Perm())
		}
	}
	if err != nil {
		root.Chmod(p, fi.Mode().Perm())
	}
	return err
}

// removeTree deletes a detached tree, making its read-only dirs writable first.
func removeTree(root *os.Root, p string) error {
	fs.WalkDir(root.FS(), p, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			root.Chmod(p, 0o700)
		}
		return nil
	})
	return root.RemoveAll(p)
}

// cacheTree includes directory mtimes so newly extracted trees get the grace too.
func cacheTree(root *os.Root, p string) (size int64, newest time.Time, err error) {
	err = fs.WalkDir(root.FS(), p, func(p string, d fs.DirEntry, err error) error {
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

// removeCacheFile unlinks a file; tool cache dirs may be read-only, so it restores owner
// write permission on the parent temporarily, without following paths outside the root.
func removeCacheFile(root *os.Root, p string) error {
	err := root.Remove(p)
	if err != nil && os.IsPermission(err) {
		parent := path.Dir(p)
		if fi, statErr := root.Lstat(parent); statErr == nil && fi.IsDir() {
			if root.Chmod(parent, fi.Mode().Perm()|0o200) == nil {
				err = root.Remove(p)
				root.Chmod(parent, fi.Mode().Perm())
			}
		}
	}
	return err
}
