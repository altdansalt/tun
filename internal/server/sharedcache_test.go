package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
)

// cacheFixture is a server whose GC doesn't run on its own, with helpers to lay out caches.
type cacheFixture struct {
	t    *testing.T
	s    *Server
	data string
}

func newCacheFixture(t *testing.T) *cacheFixture {
	data := t.TempDir()
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.Close()
		// Read-only dirs (Go modules) must be writable for t.TempDir's cleanup.
		filepath.WalkDir(data, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	return &cacheFixture{t, s, data}
}

// put writes a 100-byte file (and its dirs) under the data dir, all aged by age.
func (f *cacheFixture) put(rel string, age time.Duration) {
	f.t.Helper()
	p := filepath.Join(f.data, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, 100), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.age(rel, age)
}

// age sets the mtime of rel and of its dirs below the cache roots.
func (f *cacheFixture) age(rel string, age time.Duration) {
	tm := time.Now().Add(-age)
	for p := rel; p != "." && p != "cache" && p != "bazel" && filepath.Dir(p) != "cache"; p = filepath.Dir(p) {
		os.Chtimes(filepath.Join(f.data, p), tm, tm)
	}
}

func (f *cacheFixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.data, rel))
	return err == nil
}

func (f *cacheFixture) want(present bool, rels ...string) {
	f.t.Helper()
	for _, rel := range rels {
		if f.exists(rel) != present {
			f.t.Errorf("%s: present=%v, want %v", rel, !present, present)
		}
	}
}

func (f *cacheFixture) running(n int) {
	f.s.mu.Lock()
	f.s.running = n
	f.s.mu.Unlock()
}

func TestTrimSharedCaches(t *testing.T) {
	f := newCacheFixture(t)
	const day = 24 * time.Hour
	f.put("cache/go-build/aa/old-d", 7*day)
	f.put("cache/go-build/aa/hours-d", 3*time.Hour) // Go refreshes mtimes only hourly: keep
	f.put("cache/go-build/aa/future-d", -365*day)
	f.put("cache/xdg/recent/file", 0)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600)
	os.Symlink(outside, filepath.Join(f.data, "cache/xdg/link"))
	f.s.cfg.MaxSharedCache = 1
	f.s.trimSharedCaches()
	f.want(false, "cache/go-build/aa/old-d")
	f.want(true, "cache/go-build/aa/hours-d", "cache/go-build/aa/future-d", "cache/xdg/recent/file")
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("followed a symlink out of the cache", err)
	}
	// A future mtime is reset to now: it ages from here instead of being pinned forever.
	fi, _ := os.Stat(filepath.Join(f.data, "cache/go-build/aa/future-d"))
	if fi.ModTime().After(time.Now()) {
		t.Fatalf("future mtime kept: %v", fi.ModTime())
	}

	// Under disk pressure go-build entries get only an hour's grace.
	old := diskStat
	defer func() { diskStat = old }()
	diskStat = func(string) (int64, int64, error) {
		if f.exists("cache/go-build/aa/hours-d") {
			return 0, 1000, nil
		}
		return 800, 1000, nil
	}
	f.s.cfg.MinFree = MinFree{Bytes: 500}
	f.s.trimSharedCaches()
	f.want(false, "cache/go-build/aa/hours-d")
	f.want(true, "cache/xdg/recent/file")
}

// A tool's cache dir is evicted whole, never file by file, and only with no local job.
func TestTrimToolDirsWhole(t *testing.T) {
	f := newCacheFixture(t)
	f.put("cache/xdg/tool/lib/old", 48*time.Hour)
	f.put("cache/xdg/tool/bin/new", 0)
	f.put("cache/xdg/other/lib/a", 48*time.Hour)
	f.put("cache/xdg/other/lib/b", 48*time.Hour)
	// bazel-ci.sh's repository cache: a fetched repository (extracted with old mtimes, in a
	// dir created recently), and an old entry; the disk cache is Bazel's to trim.
	f.put("cache/xdg/tun-bazel/repo/contents/abc/uuid1/BUILD", 90*24*time.Hour)
	f.put("cache/xdg/tun-bazel/repo/contents/abc/uuid1/src/x.go", 90*24*time.Hour)
	f.age("cache/xdg/tun-bazel/repo/contents/abc/uuid1", 0)
	f.put("cache/xdg/tun-bazel/repo/content_addressable/sha256/old/file", 48*time.Hour)
	f.put("cache/xdg/tun-bazel/disk/cas/aa/blob", 48*time.Hour)
	f.s.cfg.MaxSharedCache = 1
	f.running(1)
	f.s.trimSharedCaches()
	f.want(true, "cache/xdg/other/lib/a", "cache/xdg/tun-bazel/repo/content_addressable/sha256/old/file")
	f.running(0)
	f.s.trimSharedCaches()
	f.want(false, "cache/xdg/other", "cache/xdg/tun-bazel/repo/content_addressable/sha256/old")
	f.want(true, "cache/xdg/tool/lib/old", "cache/xdg/tool/bin/new",
		"cache/xdg/tun-bazel/repo/contents/abc/uuid1/BUILD", "cache/xdg/tun-bazel/repo/contents/abc/uuid1/src/x.go",
		"cache/xdg/tun-bazel/repo/content_addressable/sha256", "cache/xdg/tun-bazel/disk/cas/aa/blob")
	ents, _ := os.ReadDir(filepath.Join(f.data, "cache/xdg"))
	for _, e := range ents {
		if e.Name() != "tool" && e.Name() != "tun-bazel" {
			t.Errorf("left %s behind", e.Name())
		}
	}
}

// Go's download files belong to their extracted module: never trimmed alone (a missing
// .ziphash makes go re-extract a module others compile from), evicted with it.
func TestTrimGoModules(t *testing.T) {
	f := newCacheFixture(t)
	const mod = "cache/gomod/example.org/!mod@v1.0.0"
	const dl = "cache/gomod/cache/download/example.org/!mod/@v/"
	f.put(mod+"/go.mod", 48*time.Hour)
	f.put(mod+"/pkg/source.go", 48*time.Hour)
	for _, ext := range []string{"zip", "ziphash", "info", "mod", "lock"} {
		f.put(dl+"v1.0.0."+ext, 48*time.Hour)
		f.put("cache/gomod/cache/download/example.org/other/@v/v2.0.0."+ext, 48*time.Hour)
	}
	filepath.WalkDir(filepath.Join(f.data, mod), func(p string, d os.DirEntry, err error) error {
		if err == nil {
			if d.IsDir() {
				os.Chmod(p, 0o555)
			} else {
				os.Chmod(p, 0o444)
			}
		}
		return nil
	})
	f.s.cfg.MaxSharedCache = 1
	f.running(1)
	f.s.trimSharedCaches()
	f.want(true, mod+"/pkg/source.go", dl+"v1.0.0.ziphash", dl+"v1.0.0.lock")
	f.running(0)
	f.s.trimSharedCaches()
	f.want(false, mod, dl+"v1.0.0.zip", dl+"v1.0.0.ziphash", dl+"v1.0.0.lock", dl+"v1.0.0.info")
	f.want(true, "cache/gomod/cache/download/example.org/other/@v/v2.0.0.ziphash",
		"cache/gomod/cache/download/example.org/other/@v/v2.0.0.lock")
}

// Each client's Bazel caches have their own budget; live sessions and bazelisk are kept.
func TestTrimClientCaches(t *testing.T) {
	f := newCacheFixture(t)
	dir := func(client string) string {
		rel, _ := filepath.Rel(f.data, f.s.clientCaches(client))
		return rel
	}
	big, small, live := dir("big"), dir("small"), dir("live")
	f.put(big+"/repo/content_addressable/sha256/one/file", 3*time.Hour)
	f.put(big+"/repo/content_addressable/sha256/two/file", 2*time.Hour)
	f.put(big+"/repo/contents/abc/uuid/BUILD", 30*24*time.Hour)
	f.put(big+"/repo/contents/abc/uuid/sub/x", 30*24*time.Hour)
	f.age(big+"/repo/contents/abc/uuid", 0)
	f.put(big+"/bazelisk/downloads/bazel", 30*24*time.Hour)
	f.put(big+"/disk/cas/aa/x", 30*24*time.Hour)
	f.put(small+"/repo/content_addressable/sha256/one/file", 30*24*time.Hour)
	for _, e := range []string{"one", "two", "three", "four"} {
		f.put(live+"/repo/content_addressable/sha256/"+e+"/file", 30*24*time.Hour)
	}
	f.s.mu.Lock()
	f.s.sessions["s"] = &session{Session: api.Session{ID: "s", Client: "live"}}
	f.s.mu.Unlock()
	f.s.cfg.MaxClientCache = 300 // big has 400 bytes (and bazelisk and a disk cache)
	f.s.trimSharedCaches()
	f.want(false, big+"/repo/content_addressable/sha256/one")
	f.want(true, big+"/repo/content_addressable/sha256/two/file", big+"/repo/contents/abc/uuid/sub/x",
		big+"/bazelisk/downloads/bazel", big+"/disk/cas/aa/x", small+"/repo/content_addressable/sha256/one/file",
		live+"/repo/content_addressable/sha256/one/file")

	// Disk pressure: everything past an hour's grace except live sessions and bazelisk,
	// including the disk cache; trees still go whole.
	old := diskStat
	defer func() { diskStat = old }()
	diskStat = func(string) (int64, int64, error) { return 0, 1000, nil }
	f.s.cfg.MinFree = MinFree{Bytes: 500}
	os.Chmod(filepath.Join(f.data, big, "repo/content_addressable/sha256"), 0o555)
	f.s.trimSharedCaches()
	f.want(false, big+"/repo/content_addressable/sha256/two", big+"/disk/cas/aa/x", small+"/repo/content_addressable/sha256/one")
	f.want(true, big+"/repo/contents/abc/uuid/BUILD", big+"/repo/contents/abc/uuid/sub/x",
		big+"/bazelisk/downloads/bazel", live+"/repo/content_addressable/sha256/two/file")
}

func TestPeriodicSharedCacheTrim(t *testing.T) {
	data := t.TempDir()
	p := filepath.Join(data, "cache/go-build/aa/old")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, make([]byte, 200), 0o600)
	tm := time.Now().Add(-7 * 24 * time.Hour)
	os.Chtimes(p, tm, tm)
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1, MaxSharedCache: 100, GCInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(p); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic trim did not run without retention or min-free")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A client's bazelisk downloads are seeded again when missing, not only for a new client.
func TestBazeliskReseed(t *testing.T) {
	data, seed := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(seed, "downloads/v"), 0o700)
	os.WriteFile(filepath.Join(seed, "downloads/v/bazel"), []byte("bin"), 0o700)
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1, BazeliskSeed: seed})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	sess := &session{Session: api.Session{ID: "reseed", Name: "default", Client: "c"}}
	in, host, _ := s.sessionLayout(sess)
	bin := filepath.Join(host.bazelisk, "downloads/v/bazel")
	for i := 0; i < 2; i++ {
		if err := s.prepareSession(sess, in, host); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("prepare %d: not seeded: %v", i, err)
		}
		os.RemoveAll(filepath.Join(host.bazelisk, "downloads"))
	}
}
