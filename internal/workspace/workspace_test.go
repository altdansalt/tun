package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, data string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func roundTrip(t *testing.T, root string, files []string) string {
	t.Helper()
	var buf bytes.Buffer
	sum, size, err := Pack(root, files, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(buf.Len()) || len(sum) != 64 {
		t.Fatalf("size %d vs %d, sum %q", size, buf.Len(), sum)
	}
	dst := t.TempDir()
	t.Cleanup(func() { RemoveAll(dst) }) // runs before TempDir's cleanup; handles read-only dirs
	if err := Unpack(&buf, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestGitWorkspace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	git(t, root, "init", "-q")
	write(t, filepath.Join(root, ".gitignore"), "*.log\nbuild/\n", 0o644)
	write(t, filepath.Join(root, "a.txt"), "hello", 0o644)
	write(t, filepath.Join(root, "bin/run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	write(t, filepath.Join(root, "deleted.txt"), "gone", 0o644)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	if err := os.Symlink("../a.txt", filepath.Join(root, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", ".")
	git(t, root, "commit", "-qm", "x")
	os.Remove(filepath.Join(root, "deleted.txt"))
	write(t, filepath.Join(root, "untracked.txt"), "new", 0o600)
	write(t, filepath.Join(root, "debug.log"), "ignored", 0o644)
	write(t, filepath.Join(root, "build/out"), "ignored", 0o644)

	sub := filepath.Join(root, "bin")
	got, err := Root(sub)
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(root); got != want {
		t.Fatalf("Root(%s) = %s, want %s", sub, got, want)
	}
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".gitignore", "a.txt", "bin/run.sh", "sub/link", "untracked.txt"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("Files = %q, want %q", files, want)
	}
	dst := roundTrip(t, root, files)
	if b, _ := os.ReadFile(filepath.Join(dst, "sub/link")); string(b) != "hello" {
		t.Errorf("symlink content = %q", b)
	}
	if l, err := os.Readlink(filepath.Join(dst, "sub/link")); err != nil || l != "../a.txt" {
		t.Errorf("symlink = %q, %v", l, err)
	}
	for name, mode := range map[string]os.FileMode{"bin/run.sh": 0o755, "a.txt": 0o644, "untracked.txt": 0o600} {
		fi, err := os.Stat(filepath.Join(dst, name))
		if err != nil || fi.Mode().Perm() != mode {
			t.Errorf("%s: mode %v, err %v; want %v", name, fi.Mode(), err, mode)
		}
	}
	for _, name := range []string{".git", "deleted.txt", "debug.log", "build"} {
		if _, err := os.Lstat(filepath.Join(dst, name)); err == nil {
			t.Errorf("%s should not be shipped", name)
		}
	}
	// Packing is deterministic: same tree, same hash.
	var b1, b2 bytes.Buffer
	s1, _, _ := Pack(root, files, &b1)
	s2, _, _ := Pack(root, files, &b2)
	if s1 != s2 {
		t.Errorf("pack not deterministic: %s vs %s", s1, s2)
	}
}

func TestPlainDirWorkspace(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "x/y.txt"), "y", 0o640)
	write(t, filepath.Join(root, "nested/.git/config"), "skip me", 0o644)
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"empty", "nested", "x", "x/y.txt"}
	if !reflect.DeepEqual(files, want) {
		t.Fatalf("Files = %q, want %q", files, want)
	}
	dst := roundTrip(t, root, files)
	if fi, err := os.Stat(filepath.Join(dst, "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty dir not kept: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "x/y.txt")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("x/y.txt: %v %v", fi, err)
	}
}

type entry struct {
	name, link string
	typ        byte
}

func tarOf(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: 0o644}
		if e.typ == tar.TypeReg {
			hdr.Size = 1
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestUnpackRejectsUnsafe(t *testing.T) {
	reg, sym := byte(tar.TypeReg), byte(tar.TypeSymlink)
	cases := map[string][]entry{
		"dotdot":         {{name: "../evil", typ: reg}},
		"nested dotdot":  {{name: "a/../../evil", typ: reg}},
		"absolute":       {{name: "/tmp/evil", typ: reg}},
		"abs symlink":    {{name: "l", link: "/etc/passwd", typ: sym}},
		"escape symlink": {{name: "a/l", link: "../../x", typ: sym}},
		"device":         {{name: "dev", typ: tar.TypeChar}},
		"fifo":           {{name: "fifo", typ: tar.TypeFifo}},
		"hardlink":       {{name: "h", link: "x", typ: tar.TypeLink}},
		// Each link looks harmless lexically, but together they would point outside;
		// writing through them must still fail.
		"write through link": {{name: "d", link: ".", typ: sym}, {name: "e", link: "d/../outside", typ: sym}, {name: "e/f", typ: reg}},
		"duplicate":          {{name: "f", typ: reg}, {name: "f", typ: reg}},
		// Link chains that only escape once resolved, in either creation order.
		"link chain":          {{name: "d", link: ".", typ: sym}, {name: "e", link: "d/../outside", typ: sym}},
		"link chain reversed": {{name: "e", link: "d/../outside", typ: sym}, {name: "d", link: ".", typ: sym}},
		"nested link chain":   {{name: "a/b/up", link: "..", typ: sym}, {name: "a/x", link: "b/up/../../outside", typ: sym}},
		"link loop":           {{name: "p", link: "q", typ: sym}, {name: "q", link: "p", typ: sym}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			dst := filepath.Join(parent, "dst")
			os.Mkdir(dst, 0o755)
			os.Mkdir(filepath.Join(parent, "outside"), 0o755)
			err := Unpack(tarOf(t, entries...), dst)
			if err == nil {
				t.Fatal("Unpack succeeded, want error")
			}
			if _, err := os.Stat(filepath.Join(parent, "outside", "f")); err == nil {
				t.Fatal("wrote outside the destination")
			}
			if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
				t.Fatal("wrote outside the destination")
			}
		})
	}
	// Relative in-tree links are fine.
	if err := Unpack(tarOf(t, entry{name: "a/l", link: "../b", typ: sym}), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// Chains that stay inside are fine too.
	if err := Unpack(tarOf(t, entry{name: "d", link: "a", typ: sym}, entry{name: "a/b/l", link: "../../d/../c", typ: sym}), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := Unpack(strings.NewReader("not gzip"), t.TempDir()); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestRemoveAllReadOnlyDirs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "job")
	write(t, filepath.Join(dir, "a", "b", "f"), "x", 0o444)
	outside := filepath.Join(t.TempDir(), "keep")
	write(t, filepath.Join(outside, "f"), "x", 0o644)
	os.Chmod(outside, 0o755)
	os.Symlink(outside, filepath.Join(dir, "a", "link"))
	os.Chmod(filepath.Join(dir, "a", "b"), 0o500)
	os.Chmod(filepath.Join(dir, "a"), 0o000)
	if err := RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still there: %v", err)
	}
	if fi, err := os.Stat(outside); err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("symlink target touched: %v %v", fi, err)
	}
}
