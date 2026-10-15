package workspace

// Incremental sync (tun bazel): instead of a tarball, the client sends a manifest of the
// same file set Pack would ship (path -> sha256, mode; symlinks; directories), the server
// asks for the file contents it lacks (by digest), and Apply updates a persistent copy in
// place. Files whose content, mode and on-disk stat are unchanged are not touched, so their
// mtimes (and every stat-based cache, Bazel's included) stay valid.

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/altdansalt/tun/internal/api"
)

// HashCache remembers file digests by (size, mtime, ctime, inode) so a client doesn't rehash
// an unchanged tree for every command. The zero value works (and caches nothing on disk).
type HashCache struct {
	path    string
	Entries map[string]hashEntry `json:"entries"`
	used    map[string]bool
}

type hashEntry struct {
	Size  int64  `json:"n"`
	Mtime int64  `json:"m"`
	Ctime int64  `json:"c"`
	Ino   uint64 `json:"i"`
	Sha   string `json:"s"`
}

// LoadHashCache reads the cache for workspace root from dir (created on Save). Errors just
// mean an empty cache.
func LoadHashCache(dir, root string) *HashCache {
	sum := sha256.Sum256([]byte(root))
	c := &HashCache{path: filepath.Join(dir, "hashes-"+hex.EncodeToString(sum[:8])+".json")}
	if b, err := os.ReadFile(c.path); err == nil {
		json.Unmarshal(b, c)
	}
	return c
}

// Save writes the entries used since loading (dropping files no longer in the workspace).
func (c *HashCache) Save() error {
	if c.path == "" {
		return nil
	}
	for p := range c.Entries {
		if !c.used[p] {
			delete(c.Entries, p)
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", c.path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// digest returns the sha256 of the regular file p (relative name rel), from the cache when
// its stat matches. Files modified in the last two seconds are always rehashed: a write
// within the mtime granularity right after we looked would otherwise go unnoticed.
func (c *HashCache) digest(rel, p string, fi fs.FileInfo) (string, error) {
	st, _ := fi.Sys().(*syscall.Stat_t)
	var e hashEntry
	if st != nil {
		e = hashEntry{Size: fi.Size(), Mtime: fi.ModTime().UnixNano(), Ctime: st.Ctim.Nano(), Ino: st.Ino}
	}
	if c.used == nil {
		c.used = map[string]bool{}
	}
	if c.Entries == nil {
		c.Entries = map[string]hashEntry{}
	}
	c.used[rel] = true
	if old, ok := c.Entries[rel]; ok && st != nil && old.Sha != "" && old == (hashEntry{e.Size, e.Mtime, e.Ctime, e.Ino, old.Sha}) &&
		time.Since(fi.ModTime()) > 2*time.Second {
		return old.Sha, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	if n != fi.Size() {
		return "", fmt.Errorf("%s changed size while hashing", p)
	}
	e.Sha = hex.EncodeToString(h.Sum(nil))
	if st != nil {
		c.Entries[rel] = e
	}
	return e.Sha, nil
}

// BuildManifest describes files (relative to root, as from Files) like Pack would ship them:
// each parent directory once before its contents, regular files with their digest, symlinks
// as links; anything else is skipped. Paths come out sorted with parents first.
func BuildManifest(root string, files []string, cache *HashCache) ([]api.ManifestEntry, error) {
	if cache == nil {
		cache = &HashCache{}
	}
	var out []api.ManifestEntry
	dirs := map[string]bool{}
	addDir := func(d string, fi fs.FileInfo) {
		dirs[d] = true
		out = append(out, api.ManifestEntry{Path: d, Type: api.EntryDir, Mode: uint32(fi.Mode().Perm())})
	}
	for _, f := range files {
		for i := range len(f) {
			if f[i] != '/' || dirs[f[:i]] {
				continue
			}
			if fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(f[:i]))); err == nil && fi.IsDir() {
				addDir(f[:i], fi)
			}
		}
		p := filepath.Join(root, filepath.FromSlash(f))
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		switch {
		case fi.IsDir():
			if !dirs[f] {
				addDir(f, fi)
			}
		case fi.Mode().IsRegular():
			sha, err := cache.digest(f, p, fi)
			if err != nil {
				return nil, err
			}
			out = append(out, api.ManifestEntry{Path: f, Type: api.EntryFile, Mode: uint32(fi.Mode().Perm()), Sha: sha, Size: fi.Size()})
		case fi.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return nil, err
			}
			out = append(out, api.ManifestEntry{Path: f, Type: api.EntryLink, Target: t})
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return pathLess(out[a].Path, out[b].Path) })
	return out, nil
}

// pathLess orders paths component by component, so a directory sorts right before its
// contents ("a", "a/b", "a-b" rather than "a", "a-b", "a/b").
func pathLess(a, b string) bool {
	return strings.ReplaceAll(a, "/", "\x00") < strings.ReplaceAll(b, "/", "\x00")
}

// SyncState is what the server knows about a synced tree: the last applied manifest plus,
// for regular files, the stat they had right after being written. A file whose stat differs
// (something in the sandbox modified it) is rewritten on the next sync. The ctime counts:
// unlike the mtime, the sandbox can't set it back after modifying a file.
type SyncState map[string]SyncRecord

// SyncRecord is one path of a SyncState.
type SyncRecord struct {
	api.ManifestEntry
	Mtime int64  `json:"mt,omitempty"`
	Ctime int64  `json:"ct,omitempty"`
	Ino   uint64 `json:"ino,omitempty"`
}

// SyncStats counts what Apply did.
type SyncStats struct {
	Added, Updated, Deleted, Unchanged int
	BytesWritten                       int64
}

func (s SyncStats) String() string {
	return fmt.Sprintf("%d added, %d updated, %d deleted, %d unchanged (%dB written)", s.Added, s.Updated, s.Deleted, s.Unchanged, s.BytesWritten)
}

// CheckManifest validates a manifest from a client: safe relative paths, known types,
// digests, parents listed before children, no duplicates, links that stay inside the tree
// (lexically; Apply checks chains on the final tree), and the total file size under max (0:
// no limit).
func CheckManifest(m []api.ManifestEntry, max int64) error {
	seen := make(map[string]string, len(m))
	var total int64
	for _, e := range m {
		name, err := safeName(e.Path)
		if err != nil {
			return err
		}
		if name != e.Path || name == "." {
			return fmt.Errorf("manifest path %q is not clean", e.Path)
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("manifest path %q appears twice", name)
		}
		if parent := path.Dir(name); parent != "." && seen[parent] != api.EntryDir {
			return fmt.Errorf("manifest path %q: parent %q must be listed (as a directory) before it", name, parent)
		}
		seen[name] = e.Type
		if e.Mode&^0o777 != 0 {
			return fmt.Errorf("manifest path %q: mode %o has more than permission bits", name, e.Mode)
		}
		switch e.Type {
		case api.EntryDir:
		case api.EntryFile:
			if len(e.Sha) != 64 || strings.Trim(e.Sha, "0123456789abcdef") != "" || e.Size < 0 {
				return fmt.Errorf("manifest path %q: bad digest or size", name)
			}
			if total += e.Size; max > 0 && total > max {
				return &TooLargeError{Limit: max, Entry: name}
			}
		case api.EntryLink:
			if err := checkLink(name, e.Target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("manifest path %q: unknown type %q (f, d or l)", name, e.Type)
		}
	}
	return nil
}

// Apply makes dir (which must exist) match manifest m (already checked with CheckManifest),
// given the state of the previous Apply (nil for a fresh dir). blob opens a file's content
// by digest. Paths of the old state missing from m are deleted; anything else in dir that
// tun never wrote (e.g. bazel-* links Bazel created) is left alone unless m needs its path.
// All writes go through an os.Root, so links in dir (which the sandbox can create) never
// lead outside it. The returned state is valid even when err != nil (it describes what was
// done), so the caller should keep it either way.
func Apply(dir string, old SyncState, m []api.ManifestEntry, blob func(sha string) (*os.File, error)) (SyncState, SyncStats, error) {
	var st SyncStats
	root, err := os.OpenRoot(dir)
	if err != nil {
		return old, st, err
	}
	defer root.Close()
	next := make(SyncState, len(m))
	for p, r := range old { // until replaced below: what is on disk
		next[p] = r
	}
	want := make(map[string]bool, len(m))
	for _, e := range m {
		want[e.Path] = true
	}
	// Directories we wrote may be read-only; make them writable until the end.
	for p, r := range old {
		if r.Type == api.EntryDir && r.Mode&0o300 != 0o300 {
			root.Chmod(p, 0o700|fs.FileMode(r.Mode))
		}
	}
	// Deletions, deepest first.
	var gone []string
	for p := range old {
		if !want[p] {
			gone = append(gone, p)
		}
	}
	sort.Slice(gone, func(a, b int) bool { return pathLess(gone[b], gone[a]) })
	for _, p := range gone {
		err := root.Remove(p)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			st.Deleted++
			delete(next, p)
		} else if old[p].Type == api.EntryDir {
			delete(next, p) // not empty: it holds files tun didn't write; leave it to them
		} else {
			return next, st, fmt.Errorf("deleting %s: %v", p, err)
		}
	}
	for _, e := range m {
		changed, n, err := applyEntry(root, e, old[e.Path], blob)
		if err != nil {
			return next, st, fmt.Errorf("syncing %s: %v", e.Path, err)
		}
		r := SyncRecord{ManifestEntry: e}
		if e.Type == api.EntryFile {
			if fi, err := root.Lstat(e.Path); err == nil {
				r.Mtime = fi.ModTime().UnixNano()
				if s, ok := fi.Sys().(*syscall.Stat_t); ok {
					r.Ino, r.Ctime = s.Ino, s.Ctim.Nano()
				}
			}
		}
		_, existed := old[e.Path]
		switch {
		case !changed:
			st.Unchanged++
		case existed:
			st.Updated++
		default:
			st.Added++
		}
		st.BytesWritten += n
		next[e.Path] = r
	}
	for _, e := range m {
		if e.Type != api.EntryLink {
			continue
		}
		if out, err := escapes(root, path.Dir(e.Path)+"/"+e.Target); err != nil || out {
			root.Remove(e.Path)
			delete(next, e.Path)
			return next, st, fmt.Errorf("workspace symlink %q -> %q: resolves outside the workspace root", e.Path, e.Target)
		}
	}
	for i := len(m) - 1; i >= 0; i-- { // deepest first, as in Unpack
		if e := m[i]; e.Type == api.EntryDir {
			if err := root.Chmod(e.Path, fs.FileMode(e.Mode)); err != nil {
				return next, st, err
			}
		}
	}
	return next, st, nil
}

// applyEntry makes one path match e. It reports whether it changed anything and how many
// file bytes it wrote.
func applyEntry(root *os.Root, e api.ManifestEntry, old SyncRecord, blob func(string) (*os.File, error)) (bool, int64, error) {
	fi, err := root.Lstat(e.Path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, 0, err
	}
	switch e.Type {
	case api.EntryDir:
		if exists && fi.IsDir() {
			if fi.Mode().Perm() != fs.FileMode(e.Mode)|0o700 {
				root.Chmod(e.Path, fs.FileMode(e.Mode)|0o700) // final mode comes last
			}
			return old.Type != api.EntryDir || old.Mode != e.Mode, 0, nil
		}
		if exists {
			if err := root.RemoveAll(e.Path); err != nil {
				return false, 0, err
			}
		}
		return true, 0, root.Mkdir(e.Path, 0o700)
	case api.EntryLink:
		if exists && fi.Mode()&fs.ModeSymlink != 0 {
			if t, err := root.Readlink(e.Path); err == nil && t == e.Target {
				return false, 0, nil
			}
		}
		if exists {
			if err := root.RemoveAll(e.Path); err != nil {
				return false, 0, err
			}
		}
		return true, 0, root.Symlink(e.Target, e.Path)
	}
	if exists && fi.Mode().IsRegular() && old.Type == api.EntryFile && old.Sha == e.Sha && fi.Size() == e.Size &&
		fi.ModTime().UnixNano() == old.Mtime {
		if s, ok := fi.Sys().(*syscall.Stat_t); ok && s.Ino == old.Ino && s.Ctim.Nano() == old.Ctime {
			if fi.Mode().Perm() != fs.FileMode(e.Mode) {
				return true, 0, root.Chmod(e.Path, fs.FileMode(e.Mode))
			}
			return false, 0, nil
		}
	}
	src, err := blob(e.Sha)
	if err != nil {
		return false, 0, err
	}
	defer src.Close()
	tmp := path.Join(path.Dir(e.Path), ".tun-sync-"+randName())
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, 0, err
	}
	n, err := io.Copy(f, io.LimitReader(src, e.Size+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n != e.Size {
		err = fmt.Errorf("blob %s has %d bytes, the manifest says %d", e.Sha, n, e.Size)
	}
	if err == nil {
		err = root.Chmod(tmp, fs.FileMode(e.Mode))
	}
	if err == nil && exists && fi.IsDir() {
		err = root.RemoveAll(e.Path)
	}
	if err == nil {
		err = root.Rename(tmp, e.Path)
	}
	if err != nil {
		root.Remove(tmp)
		return false, 0, err
	}
	return true, n, nil
}

func randName() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ErrBlobsTooLarge: a blob upload's content is over the limit.
var ErrBlobsTooLarge = errors.New("blob upload over the size limit")

// WriteBlobs writes the contents of the files (relative to root) with the given digests as
// a tar whose entries are named by digest: the body of POST /v1/cas.
func WriteBlobs(w io.Writer, root string, files map[string]string) error {
	tw := tar.NewWriter(w)
	shas := make([]string, 0, len(files))
	for sha := range files {
		shas = append(shas, sha)
	}
	sort.Strings(shas)
	for _, sha := range shas {
		p := filepath.Join(root, filepath.FromSlash(files[sha]))
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: sha, Typeflag: tar.TypeReg, Mode: 0o600, Size: fi.Size()}); err != nil {
			return err
		}
		if err := copyFile(tw, p, fi.Size()); err != nil {
			return err
		}
	}
	return tw.Close()
}

// ReadBlobs stores the blobs of a WriteBlobs tar (gzip-compressed if gz) in dir, verifying
// each digest, and returns how many it stored. More than max content bytes in total is
// ErrBlobsTooLarge. A non-nil store moves each verified temp file (in dir) into place
// instead of renaming it to dir/sha.
func ReadBlobs(r io.Reader, gz bool, max int64, dir string, store func(tmp, sha string) error) (int, error) {
	if gz {
		z, err := gzip.NewReader(r)
		if err != nil {
			return 0, err
		}
		r = z
	}
	tr := tar.NewReader(r)
	var total int64
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		if hdr.Typeflag != tar.TypeReg || len(hdr.Name) != 64 || strings.Trim(hdr.Name, "0123456789abcdef") != "" {
			return n, fmt.Errorf("entry %q: want regular files named by their sha256", hdr.Name)
		}
		if total += hdr.Size; total > max {
			return n, ErrBlobsTooLarge
		}
		tmp, err := os.CreateTemp(dir, "upload-*")
		if err != nil {
			return n, err
		}
		h := sha256.New()
		_, err = io.Copy(io.MultiWriter(tmp, h), tr)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil && hex.EncodeToString(h.Sum(nil)) != hdr.Name {
			err = fmt.Errorf("entry %s: content hashes to %x", hdr.Name, h.Sum(nil))
		}
		if err == nil {
			if store != nil {
				err = store(tmp.Name(), hdr.Name)
			} else {
				err = os.Rename(tmp.Name(), filepath.Join(dir, hdr.Name))
			}
		}
		if err != nil {
			os.Remove(tmp.Name())
			return n, err
		}
		n++
	}
}
