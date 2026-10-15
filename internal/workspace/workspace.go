// Package workspace packs a client's working tree into a content-addressed tar.gz and
// unpacks it safely on the server.
package workspace

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Root returns the workspace root for dir: the git toplevel if dir is inside a git work
// tree, else dir itself. The result has symlinks resolved.
func Root(dir string) (string, error) {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	if out, err := cmd.Output(); err == nil {
		if top := strings.TrimSpace(string(out)); top != "" {
			return filepath.EvalSymlinks(top)
		}
	}
	return dir, nil
}

// Files lists the slash-separated relative paths to ship from root. In a git work tree that
// is tracked + untracked-not-ignored files (deleted tracked files skipped); otherwise every
// entry under root except .git directories.
func Files(root string) ([]string, error) {
	if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
		cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-files in %s: %v: %s", root, err, strings.TrimSpace(stderr.String()))
		}
		seen := map[string]bool{}
		var files []string
		for _, f := range strings.Split(string(out), "\x00") {
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			fi, err := os.Lstat(filepath.Join(root, f))
			if err != nil || fi.IsDir() { // deleted, or a submodule (not shipped in v0)
				continue
			}
			files = append(files, f)
		}
		sort.Strings(files)
		return files, nil
	}
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	return files, err
}

// Pack writes a tar.gz of files (relative to root) to w and returns its sha256 (hex) and
// size. Regular files and directories keep their permission bits, symlinks are stored as
// links, directories are kept (so empty dirs survive), anything else is skipped.
func Pack(root string, files []string, w io.Writer) (sum string, size int64, err error) {
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(w, h)}
	gz := gzip.NewWriter(cw)
	tw := tar.NewWriter(gz)
	dirs := map[string]bool{} // directory entries already written
	writeDir := func(d string, fi fs.FileInfo) error {
		dirs[d] = true
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: d + "/", Mode: int64(fi.Mode().Perm()),
			ModTime: fi.ModTime().Truncate(1e9), Format: tar.FormatPAX})
	}
	for _, f := range files {
		// Ship each parent directory once, before its contents, so directory modes survive
		// even when files lists only files (as git does).
		for i := range len(f) {
			if f[i] != '/' || dirs[f[:i]] {
				continue
			}
			if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(f[:i]))); err == nil && fi.IsDir() {
				if err := writeDir(f[:i], fi); err != nil {
					return "", 0, err
				}
			}
		}
		p := filepath.Join(root, filepath.FromSlash(f))
		fi, err := os.Lstat(p)
		if err != nil {
			return "", 0, err
		}
		if fi.IsDir() {
			if !dirs[f] {
				if err := writeDir(f, fi); err != nil {
					return "", 0, err
				}
			}
			continue
		}
		hdr := &tar.Header{Name: f, Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime().Truncate(1e9), Format: tar.FormatPAX}
		switch {
		case fi.Mode().IsRegular():
			hdr.Typeflag, hdr.Size = tar.TypeReg, fi.Size()
		case fi.Mode()&fs.ModeSymlink != 0:
			if hdr.Linkname, err = os.Readlink(p); err != nil {
				return "", 0, err
			}
			hdr.Typeflag = tar.TypeSymlink
		default:
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return "", 0, err
		}
		if hdr.Typeflag == tar.TypeReg {
			if err := copyFile(tw, p, fi.Size()); err != nil {
				return "", 0, err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), cw.n, nil
}

func copyFile(w io.Writer, p string, size int64) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err == nil && n != size {
		err = fmt.Errorf("%s changed size while packing", p)
	}
	return err
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Unpack extracts a tar.gz from r into dir (which must exist). It rejects absolute paths,
// "..", links that are absolute or resolve outside dir (also via other links), hard links
// and device/special files. All writes go through an os.Root, so nothing can be written
// outside dir even via symlinks. Directory modes are applied last, so a read-only directory
// can still be filled; use RemoveAll to delete the result.
func Unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("workspace is not gzip: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	dirModes := map[string]fs.FileMode{}
	var links []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("bad workspace tar: %w", err)
		}
		name, err := safeName(hdr.Name)
		if err != nil {
			return err
		}
		if name == "." {
			continue
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o700); err != nil {
				return err
			}
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o700); err != nil {
				return err
			}
			dirModes[name] = mode
		case tar.TypeReg:
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = root.Chmod(name, mode)
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := checkLink(name, hdr.Linkname); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
			links = append(links, name)
		default:
			return fmt.Errorf("workspace entry %q: unsupported type %q (only files, dirs and symlinks)", hdr.Name, hdr.Typeflag)
		}
	}
	// Each link passed the lexical check, but chains (d -> ., e -> d/../x) can still leave
	// the root once the kernel resolves them. Check every link against the final tree.
	for _, l := range links {
		target, _ := root.Readlink(l)
		if out, err := escapes(root, path.Dir(l)+"/"+target); err != nil || out {
			return fmt.Errorf("workspace symlink %q -> %q: resolves outside the workspace root", l, target)
		}
	}
	// Deepest first, so a parent losing its search bit doesn't block chmod of children.
	names := make([]string, 0, len(dirModes))
	for d := range dirModes {
		names = append(names, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, d := range names {
		if err := root.Chmod(d, dirModes[d]); err != nil {
			return err
		}
	}
	return nil
}

// escapes reports whether resolving rel (slash-separated, relative to root) would leave
// root, following the symlinks that exist in root the way the kernel would.
func escapes(root *os.Root, rel string) (bool, error) {
	var cur []string // resolved components, none of them a symlink
	todo := strings.Split(rel, "/")
	for hops := 0; len(todo) > 0; {
		c := todo[0]
		todo = todo[1:]
		switch c {
		case "", ".":
			continue
		case "..":
			if len(cur) == 0 {
				return true, nil
			}
			cur = cur[:len(cur)-1]
			continue
		}
		p := path.Join(append(cur, c)...)
		fi, err := root.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			cur = append(cur, c) // a dir, a file, or not there (yet): taken literally
			continue
		}
		if hops++; hops > 40 {
			return false, fmt.Errorf("too many levels of symlinks at %q", p)
		}
		t, err := root.Readlink(p)
		if err != nil {
			return false, err
		}
		if path.IsAbs(t) {
			return true, nil
		}
		todo = append(strings.Split(t, "/"), todo...)
	}
	return false, nil
}

// RemoveAll removes dir like os.RemoveAll, first restoring owner rwx on directories inside
// it (never following symlinks) so read-only directories from a workspace or a job don't
// make it undeletable.
func RemoveAll(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

func safeName(name string) (string, error) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\x00") {
		return "", fmt.Errorf("workspace entry %q: absolute or empty path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("workspace entry %q: contains ..", name)
		}
	}
	return path.Clean(name), nil
}

func checkLink(name, target string) error {
	if target == "" || path.IsAbs(target) {
		return fmt.Errorf("workspace symlink %q -> %q: absolute links are not allowed", name, target)
	}
	if t := path.Join(path.Dir(name), target); t == ".." || strings.HasPrefix(t, "../") {
		return fmt.Errorf("workspace symlink %q -> %q: escapes the workspace root", name, target)
	}
	return nil
}
