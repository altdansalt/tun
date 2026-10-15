package sandbox

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOpenMountSource(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "real", "sub"), 0o700)
	os.WriteFile(filepath.Join(dir, "file"), nil, 0o600)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link"))
	for _, c := range []struct {
		path string
		ro   bool
		err  string
	}{
		{"real/sub", false, ""},
		{"file", true, ""},
		{"file", false, "not a directory"},
		{"link", false, "is a symlink"},
		{"link/sub", false, "is a symlink"},
		{"missing", false, "no such file"},
	} {
		f, err := openMountSource(filepath.Join(dir, c.path), c.ro)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s (ro %v): got %v, want %q", c.path, c.ro, err, c.err)
		}
		if f != nil {
			f.Close()
		}
	}
}

// NetworkOff actions use the same descriptor-based mount validation as jobs/sessions.
func TestActionMountRejectsSymlink(t *testing.T) {
	if !BwrapUsable() {
		t.Skip("bwrap unavailable")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, _, err := Start(Spec{Mode: Bwrap, Tmp: real, Root: link, DataDir: dir, Dir: link, NetworkOff: true, Argv: []string{"true"}}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("unsafe action mount accepted: %v", err)
	}
}

// Hiding tmpfs come before every bind a session can write: with the data dir under /tmp (tun
// serve --ephemeral), a tmpfs after the session's /tmp bind would make bwrap mkdir its mount
// point inside that persistent, sandbox-writable dir on the host, following planted links.
func TestBwrapArgsOrder(t *testing.T) {
	s := Spec{Mode: Bwrap, FixedRoot: true, Tmp: "/tmp/tun-ephemeral-1/sessions/a/sandbox/tmp", DataDir: "/tmp/tun-ephemeral-1",
		Dir: "/work", ExePath: "/tun/bin/tun", Mounts: []Mount{{Src: "/tmp/tun-ephemeral-1/w", Dst: "/work"}, {Src: "/tmp/tun-ephemeral-1/rc", Dst: "/tun/rc", RO: true}}}
	a, _, err := bwrapArgs(s)
	if err != nil {
		t.Fatal(err)
	}
	at := func(op, dst string) int {
		for i := range a {
			if a[i] == op && (strings.HasPrefix(op, "--tmpfs") && a[i+1] == dst || i+2 < len(a) && a[i+2] == dst) {
				return i
			}
		}
		t.Fatalf("no %s %s in %q", op, dst, a)
		return -1
	}
	if tmp := at("--bind-fd", "/tmp"); at("--tmpfs", s.DataDir) > tmp {
		t.Fatalf("the data dir's tmpfs comes after the writable /tmp bind: %q", a)
	}
	for _, bad := range []Mount{{Src: "/x", Dst: "/tmp/sub"}, {Src: "/x", Dst: "/work/sub", RO: true}} {
		s2 := s
		s2.Mounts = append(slices.Clone(s.Mounts), bad)
		if _, _, err := bwrapArgs(s2); err == nil || !strings.Contains(err.Error(), "which the sandbox can write") {
			t.Errorf("mount %s under a writable mount: %v", bad.Dst, err)
		}
	}
	// Jobs (no FixedRoot) are made fresh: their tree under /tmp is fine.
	if _, _, err := bwrapArgs(Spec{Mode: Bwrap, Tmp: "/tmp/d/jobs/1/sandbox/tmp", Root: "/tmp/d/jobs/1/sandbox", DataDir: "/tmp/d", Dir: "/tmp/d/jobs/1/sandbox/work"}); err != nil {
		t.Fatal(err)
	}
}
