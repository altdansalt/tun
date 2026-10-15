package sandbox

import (
	"os"
	"path/filepath"
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
