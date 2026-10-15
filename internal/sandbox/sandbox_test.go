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
