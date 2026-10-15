package server

import (
	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrimSharedCaches(t *testing.T) {
	data := t.TempDir()
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	put := func(path string, age time.Duration) {
		t.Helper()
		p := filepath.Join(data, path)
		os.MkdirAll(filepath.Dir(p), 0700)
		if err := os.WriteFile(p, make([]byte, 100), 0600); err != nil {
			t.Fatal(err)
		}
		tm := time.Now().Add(-age)
		os.Chtimes(p, tm, tm)
	}
	put("cache/go-build/old", 3*time.Hour)
	put("cache/xdg/newer", 2*time.Hour)
	put("cache/gomod/recent", 0)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0600)
	os.Symlink(outside, filepath.Join(data, "cache/xdg/link"))
	s.cfg.MaxSharedCache = 200
	s.trimSharedCaches()
	if _, err := os.Stat(filepath.Join(data, "cache/go-build/old")); !os.IsNotExist(err) {
		t.Fatal("old file retained", err)
	}
	for _, p := range []string{"cache/xdg/newer", "cache/gomod/recent", "cache/xdg/link/keep"} {
		if _, err := os.Stat(filepath.Join(data, p)); err != nil {
			t.Fatal(p, err)
		}
	}
	s.cfg.MaxSharedCache = 1
	s.trimSharedCaches()
	if _, err := os.Stat(filepath.Join(data, "cache/gomod/recent")); err != nil {
		t.Fatal("recent file deleted")
	}
}

func TestTrimSharedCachesPressure(t *testing.T) {
	data := t.TempDir()
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// No size cap: min-free alone must reclaim old entries, protecting active caches.
	inactive := filepath.Join(s.clientCaches("inactive"), "repo", "old")
	active := filepath.Join(s.clientCaches("active"), "repo", "keep")
	for _, p := range []string{inactive, active} {
		os.MkdirAll(filepath.Dir(p), 0700)
		os.WriteFile(p, make([]byte, 100), 0600)
		tm := time.Now().Add(-2 * time.Hour)
		os.Chtimes(p, tm, tm)
	}
	os.Chmod(filepath.Dir(inactive), 0555)
	defer os.Chmod(filepath.Dir(inactive), 0700)
	s.mu.Lock()
	s.sessions["active"] = &session{Session: api.Session{ID: "active", Client: "active"}}
	s.mu.Unlock()
	old := diskStat
	defer func() { diskStat = old }()
	diskStat = func(string) (int64, int64, error) {
		if _, err := os.Stat(inactive); err == nil {
			return 0, 1000, nil
		}
		return 800, 1000, nil
	}
	s.cfg.MinFree = MinFree{Bytes: 500}
	s.reclaimDisk()
	if _, err := os.Stat(inactive); !os.IsNotExist(err) {
		t.Fatal("pressure did not remove old read-only module cache", err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatal("active session cache removed", err)
	}
}

func TestTrimExtractedModule(t *testing.T) {
	data := t.TempDir()
	s, err := New(Config{DataDir: data, Sandbox: sandbox.None, MaxJobs: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	module := filepath.Join(data, "cache/gomod/example.org/mod@v1.0.0")
	os.MkdirAll(filepath.Join(module, "pkg"), 0755)
	for _, name := range []string{"go.mod", "pkg/source.go"} {
		os.WriteFile(filepath.Join(module, name), make([]byte, 100), 0444)
	}
	tm := time.Now().Add(-2 * time.Hour)
	filepath.WalkDir(module, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		os.Chtimes(path, tm, tm)
		if d.IsDir() {
			os.Chmod(path, 0555)
		}
		return nil
	})
	t.Cleanup(func() {
		filepath.WalkDir(module, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(path, 0755)
			}
			return nil
		})
	})
	s.cfg.MaxSharedCache = 1
	s.mu.Lock()
	s.running = 1
	s.mu.Unlock()
	s.trimSharedCaches()
	if _, err := os.Stat(filepath.Join(module, "pkg/source.go")); err != nil {
		t.Fatal("module touched during local job", err)
	}
	s.mu.Lock()
	s.running = 0
	s.mu.Unlock()
	s.trimSharedCaches()
	if _, err := os.Stat(module); !os.IsNotExist(err) {
		t.Fatal("module not evicted as a whole", err)
	}
}
