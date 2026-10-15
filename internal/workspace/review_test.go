package workspace

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewSymlinkChainEscape(t *testing.T) {
	parent := t.TempDir()
	dst := filepath.Join(parent, "work")
	if err := os.Mkdir(dst, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(parent, "secret"), "outside workspace", 0600)
	err := Unpack(tarOf(t,
		entry{name: "d", link: ".", typ: tar.TypeSymlink},
		entry{name: "e", link: "d/../secret", typ: tar.TypeSymlink},
	), dst)
	if err == nil {
		b, readErr := os.ReadFile(filepath.Join(dst, "e"))
		t.Fatalf("escaping link accepted: read=%q err=%v", b, readErr)
	}
}

func TestReviewDirectoryModes(t *testing.T) {
	for _, inGit := range []bool{false, true} {
		name := "plain"
		if inGit {
			name = "git"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if inGit {
				git(t, root, "init", "-q")
			}
			write(t, filepath.Join(root, "private", "file"), "x", 0600)
			if err := os.Chmod(filepath.Join(root, "private"), 0710); err != nil {
				t.Fatal(err)
			}
			files, err := Files(root)
			if err != nil {
				t.Fatal(err)
			}
			dst := roundTrip(t, root, files)
			fi, err := os.Stat(filepath.Join(dst, "private"))
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != 0710 {
				t.Errorf("directory mode=%#o want=0710", fi.Mode().Perm())
			}
		})
	}
}

func TestReviewOddNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"space name", "tab\tname", "line\nname", "-dash", "back\\slash", "unicode-λ"} {
		write(t, filepath.Join(root, name), name, 0751)
	}
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	dst := roundTrip(t, root, files)
	for _, name := range files {
		b, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || string(b) != name {
			t.Errorf("%q: %q %v", name, b, err)
		}
	}
}

func TestReviewReadOnlyDirectoryMode(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "readonly", "file"), "x", 0644)
	if err := os.Chmod(filepath.Join(root, "readonly"), 0555); err != nil {
		t.Fatal(err)
	}
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	dst := roundTrip(t, root, files)
	fi, err := os.Stat(filepath.Join(dst, "readonly"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0555 {
		t.Errorf("directory mode=%#o want=0555", fi.Mode().Perm())
	}
}
