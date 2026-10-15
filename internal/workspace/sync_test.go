package workspace

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// syncer applies manifests of a source tree to a destination like the server does,
// keeping blobs and state between applies.
type syncer struct {
	t        *testing.T
	src, dst string
	blobs    map[string][]byte
	state    SyncState
}

func newSyncer(t *testing.T) *syncer {
	dst := t.TempDir()
	t.Cleanup(func() { RemoveAll(dst) })
	return &syncer{t: t, src: t.TempDir(), dst: dst, blobs: map[string][]byte{}}
}

func (s *syncer) sync() (SyncStats, error) {
	s.t.Helper()
	files, err := Files(s.src)
	if err != nil {
		s.t.Fatal(err)
	}
	m, err := BuildManifest(s.src, files, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	if err := CheckManifest(m, 0); err != nil {
		s.t.Fatal(err)
	}
	for _, e := range m {
		if e.Type == api.EntryFile {
			b, _ := os.ReadFile(filepath.Join(s.src, e.Path))
			s.blobs[e.Sha] = b
		}
	}
	next, st, err := Apply(s.dst, s.state, m, s.open)
	s.state = next
	return st, err
}

func (s *syncer) open(sha string) (*os.File, error) {
	p := filepath.Join(s.t.TempDir(), sha)
	if err := os.WriteFile(p, s.blobs[sha], 0o600); err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (s *syncer) mustSync() SyncStats {
	s.t.Helper()
	st, err := s.sync()
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

func mtime(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func TestSyncApply(t *testing.T) {
	s := newSyncer(t)
	write(t, filepath.Join(s.src, "a.txt"), "a", 0o644)
	write(t, filepath.Join(s.src, "bin/run.sh"), "#!/bin/sh\n", 0o755)
	write(t, filepath.Join(s.src, "d/e/f.txt"), "f", 0o600)
	write(t, filepath.Join(s.src, "gone.txt"), "x", 0o644)
	write(t, filepath.Join(s.src, "to-dir"), "file first", 0o644)
	os.Symlink("../a.txt", filepath.Join(s.src, "d/link"))
	if st := s.mustSync(); st.Added != 9 || st.Updated+st.Deleted+st.Unchanged != 0 { // 6 files/links + bin, d, d/e
		t.Fatalf("first sync: %v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(s.dst, "d/link")); string(b) != "a" {
		t.Fatalf("link reads %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(s.dst, "bin/run.sh")); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// Bazel creates things in the tree that tun never wrote; they must survive.
	os.Symlink("/tun/out/bin", filepath.Join(s.dst, "bazel-bin"))
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(s.dst, "a.txt"), old, old) // as if a build had run long ago
	fi, _ := os.Lstat(filepath.Join(s.dst, "a.txt"))
	s.state["a.txt"] = SyncRecord{ManifestEntry: s.state["a.txt"].ManifestEntry, Mtime: old.UnixNano(), Ino: s.state["a.txt"].Ino,
		Ctime: fi.Sys().(*syscall.Stat_t).Ctim.Nano()}

	if st := s.mustSync(); st.Unchanged != 9 || st.Added+st.Updated+st.Deleted != 0 || st.BytesWritten != 0 {
		t.Fatalf("no-op sync: %v", st)
	}
	if !mtime(t, filepath.Join(s.dst, "a.txt")).Equal(old) {
		t.Fatal("no-op sync touched an unchanged file")
	}

	write(t, filepath.Join(s.src, "d/e/f.txt"), "f2", 0o600)
	os.Remove(filepath.Join(s.src, "gone.txt"))
	os.Remove(filepath.Join(s.src, "to-dir"))
	write(t, filepath.Join(s.src, "to-dir/inner"), "now a dir", 0o644)
	os.Chmod(filepath.Join(s.src, "bin/run.sh"), 0o700)
	st := s.mustSync()
	// updated: f.txt, run.sh (mode), to-dir (file -> dir); added: to-dir/inner; deleted: gone.txt
	if st.Updated != 3 || st.Added != 1 || st.Deleted != 1 {
		t.Fatalf("change sync: %v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(s.dst, "d/e/f.txt")); string(b) != "f2" {
		t.Fatalf("f.txt = %q", b)
	}
	if _, err := os.Lstat(filepath.Join(s.dst, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("gone.txt not deleted")
	}
	if !mtime(t, filepath.Join(s.dst, "a.txt")).Equal(old) {
		t.Fatal("change sync touched an unchanged file")
	}
	if _, err := os.Lstat(filepath.Join(s.dst, "bazel-bin")); err != nil {
		t.Fatal("sync deleted a path it never wrote")
	}

	// Something in the sandbox edits a synced file: the next sync restores it.
	write(t, filepath.Join(s.dst, "a.txt"), "edited on the server", 0o644)
	if st := s.mustSync(); st.Updated != 1 {
		t.Fatalf("restore sync: %v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(s.dst, "a.txt")); string(b) != "a" {
		t.Fatalf("a.txt = %q", b)
	}

	// The sandbox replaces a synced dir with a link out of the tree: writes stay inside.
	outside := t.TempDir()
	RemoveAll(filepath.Join(s.dst, "d"))
	os.Symlink(outside, filepath.Join(s.dst, "d"))
	s.mustSync()
	if fi, err := os.Lstat(filepath.Join(s.dst, "d")); err != nil || !fi.IsDir() {
		t.Fatalf("d is not a dir again: %v", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("sync wrote outside the tree: %v", ents)
	}
}

func TestSyncReadOnlyDir(t *testing.T) {
	s := newSyncer(t)
	write(t, filepath.Join(s.src, "ro/x"), "1", 0o644)
	os.Chmod(filepath.Join(s.src, "ro"), 0o555)
	defer os.Chmod(filepath.Join(s.src, "ro"), 0o755)
	s.mustSync()
	os.Chmod(filepath.Join(s.src, "ro"), 0o755)
	write(t, filepath.Join(s.src, "ro/x"), "2", 0o644)
	write(t, filepath.Join(s.src, "ro/y"), "3", 0o644)
	os.Chmod(filepath.Join(s.src, "ro"), 0o555)
	if st := s.mustSync(); st.Updated != 1 || st.Added != 1 {
		t.Fatalf("%v", st)
	}
	if fi, _ := os.Stat(filepath.Join(s.dst, "ro")); fi.Mode().Perm() != 0o555 {
		t.Fatalf("ro mode %v", fi.Mode())
	}
}

func TestSyncLinkChainEscape(t *testing.T) {
	s := newSyncer(t)
	os.Mkdir(filepath.Join(s.src, "d"), 0o755)
	os.Symlink(".", filepath.Join(s.src, "d/self"))
	os.Symlink("d/self/../../etc", filepath.Join(s.src, "esc")) // lexically inside, escapes via the chain
	if _, err := s.sync(); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.dst, "esc")); !os.IsNotExist(err) {
		t.Fatal("escaping link left in place")
	}
}

func TestCheckManifest(t *testing.T) {
	sha := strings.Repeat("a", 64)
	d := api.ManifestEntry{Path: "d", Type: api.EntryDir, Mode: 0o755}
	for _, tc := range []struct {
		name string
		m    []api.ManifestEntry
		want string
	}{
		{"dotdot", []api.ManifestEntry{{Path: "../x", Type: api.EntryFile, Sha: sha}}, ".."},
		{"abs", []api.ManifestEntry{{Path: "/x", Type: api.EntryFile, Sha: sha}}, "absolute"},
		{"unclean", []api.ManifestEntry{d, {Path: "d//x", Type: api.EntryFile, Sha: sha}}, "not clean"},
		{"no parent", []api.ManifestEntry{{Path: "d/x", Type: api.EntryFile, Sha: sha}}, "parent"},
		{"file parent", []api.ManifestEntry{{Path: "d", Type: api.EntryFile, Sha: sha}, {Path: "d/x", Type: api.EntryFile, Sha: sha}}, "parent"},
		{"dup", []api.ManifestEntry{d, d}, "twice"},
		{"bad sha", []api.ManifestEntry{{Path: "x", Type: api.EntryFile, Sha: "zz"}}, "digest"},
		{"setuid", []api.ManifestEntry{{Path: "x", Type: api.EntryFile, Sha: sha, Mode: 0o4755}}, "permission"},
		{"abs link", []api.ManifestEntry{{Path: "x", Type: api.EntryLink, Target: "/etc"}}, "absolute"},
		{"up link", []api.ManifestEntry{{Path: "x", Type: api.EntryLink, Target: "../etc"}}, "escapes"},
		{"type", []api.ManifestEntry{{Path: "x", Type: "c"}}, "unknown type"},
		{"size", []api.ManifestEntry{{Path: "x", Type: api.EntryFile, Sha: sha, Size: 11}}, "more than 10"},
	} {
		err := CheckManifest(tc.m, 10)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestHashCache(t *testing.T) {
	root, cdir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "f"), "one", 0o644)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(filepath.Join(root, "f"), old, old)
	m1, _ := BuildManifest(root, []string{"f"}, LoadHashCache(cdir, root))
	c := LoadHashCache(cdir, root)
	if len(c.Entries) != 0 {
		t.Fatal("cache saved without Save")
	}
	c = LoadHashCache(cdir, root)
	BuildManifest(root, []string{"f"}, c)
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	c = LoadHashCache(cdir, root)
	e := c.Entries["f"]
	e.Sha = strings.Repeat("0", 64) // prove the cached digest is used for an unchanged stat
	c.Entries["f"] = e
	m2, _ := BuildManifest(root, []string{"f"}, c)
	if m2[0].Sha != e.Sha || m1[0].Sha == e.Sha {
		t.Fatalf("cache not used: %s", m2[0].Sha)
	}
	write(t, filepath.Join(root, "f"), "two", 0o644) // same size, new mtime/ctime
	os.Chtimes(filepath.Join(root, "f"), old.Add(time.Second), old.Add(time.Second))
	m3, _ := BuildManifest(root, []string{"f"}, c)
	if m3[0].Sha == e.Sha || m3[0].Sha == m1[0].Sha {
		t.Fatalf("changed file not rehashed: %s", m3[0].Sha)
	}
}

func TestBlobsRoundTrip(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	write(t, filepath.Join(root, "a"), "alpha", 0o644)
	m, _ := BuildManifest(root, []string{"a"}, nil)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := WriteBlobs(zw, root, map[string]string{m[0].Sha: "a"}); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	raw := buf.Bytes()
	if n, err := ReadBlobs(bytes.NewReader(raw), true, 1<<20, dir, nil); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, m[0].Sha)); string(b) != "alpha" {
		t.Fatalf("blob = %q", b)
	}
	if _, err := ReadBlobs(bytes.NewReader(raw), true, 3, dir, nil); err != ErrBlobsTooLarge {
		t.Fatalf("limit: %v", err)
	}
	var bad bytes.Buffer // content that doesn't match its name
	write(t, filepath.Join(root, "b"), "beta", 0o644)
	WriteBlobs(&bad, root, map[string]string{m[0].Sha: "b"})
	if _, err := ReadBlobs(&bad, false, 1<<20, t.TempDir(), nil); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Fatalf("bad hash: %v", err)
	}
}

// Content changed in the sandbox with the same size and the old mtime restored is still
// noticed (by its ctime) and rewritten.
func TestSyncNoticesRestoredMtime(t *testing.T) {
	s := newSyncer(t)
	write(t, filepath.Join(s.src, "f"), "good", 0o644)
	s.mustSync()
	p := filepath.Join(s.dst, "f")
	old := mtime(t, p)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(p, []byte("evil"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, old, old)
	if st := s.mustSync(); st.Updated != 1 {
		t.Fatalf("tampered file not rewritten: %v", st)
	}
	if b, _ := os.ReadFile(p); string(b) != "good" {
		t.Fatalf("f = %q", b)
	}
}
