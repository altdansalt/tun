package server

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/altdansalt/tun/internal/api"
)

// DefaultMaxDownload is the default limit on the total file bytes of one files request.
const DefaultMaxDownload = 1 << 30

// fileEntry is one tar entry to send, collected before streaming so the size limit and the
// missing/skipped lists can be reported up front.
type fileEntry struct {
	name string // slash path relative to the work dir
	info fs.FileInfo
	link string // symlink target
}

// files serves GET /v1/jobs/{id}/files?path=P[&path=Q...]: a tar stream of the named files
// and directories (recursively) from the finished job's work dir. Paths may be path.Match
// globs. All access goes through an os.Root on the work dir, so neither ".." nor symlinks
// reach outside it; symlinks inside the tree are sent as links only if their target stays
// inside the work dir.
func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	j := s.lookup(w, r)
	if j == nil {
		return
	}
	paths := r.URL.Query()["path"]
	if len(paths) == 0 {
		writeErr(w, 400, "at least one path= query parameter is required (relative to the workspace root)")
		return
	}
	for _, p := range paths {
		if err := api.ValidFilePath(p); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if snap := s.snapshot(j); !api.Terminal(snap.State) {
		writeErr(w, 409, fmt.Sprintf("job %s is %s; files can be fetched once it has ended", snap.ID, snap.State))
		return
	}
	root, err := os.OpenRoot(filepath.Join(s.jobDir(j.ID), "work"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer root.Close()

	entries, missing, skipped := collect(root, paths)
	var total int64
	for _, e := range entries {
		if e.info.Mode().IsRegular() {
			total += e.info.Size()
		}
	}
	limit := s.cfg.MaxDownload
	if limit <= 0 {
		limit = DefaultMaxDownload
	}
	if total > limit {
		writeErr(w, 413, fmt.Sprintf("requested files total %d bytes, over the server limit of %d bytes (tun serve --max-download); fetch fewer or narrower paths", total, limit))
		return
	}
	for _, m := range missing {
		w.Header().Add(api.HeaderMissing, url.PathEscape(m))
	}
	for _, sk := range skipped {
		w.Header().Add(api.HeaderSkipped, url.PathEscape(sk))
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(200)
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if err := writeEntry(tw, root, e); err != nil {
			return // headers are gone; a truncated tar tells the client
		}
	}
	tw.Close()
}

// collect expands the requested paths into tar entries (parents before children, no
// duplicates), plus the patterns that matched nothing and the entries skipped as unsafe.
func collect(root *os.Root, patterns []string) (entries []fileEntry, missing, skipped []string) {
	fsys := root.FS()
	seen := map[string]bool{}
	skip := func(name, why string) {
		if !seen[name] {
			seen[name] = true
			skipped = append(skipped, name+": "+why)
		}
	}
	add := func(name string, info fs.FileInfo) {
		if seen[name] {
			return
		}
		seen[name] = true
		e := fileEntry{name: name, info: info}
		switch m := info.Mode(); {
		case m.IsRegular(), m.IsDir():
		case m&fs.ModeSymlink != 0:
			target, err := root.Readlink(name)
			if err != nil {
				skipped = append(skipped, name+": "+err.Error())
				return
			}
			if !linkInside(name, target) {
				skipped = append(skipped, fmt.Sprintf("%s: symlink to %q points outside the work dir", name, target))
				return
			}
			e.link = target
		default:
			skipped = append(skipped, fmt.Sprintf("%s: not a regular file, directory or symlink (%v)", name, m.Type()))
			return
		}
		entries = append(entries, e)
	}
	for _, pat := range patterns {
		pat = path.Clean(pat)
		matches := []string{pat}
		if strings.ContainsAny(pat, "*?[") {
			matches, _ = fs.Glob(fsys, pat) // only error is a bad pattern, already rejected
		}
		found := false
		for _, m := range matches {
			// Lstat so that a requested symlink is sent as a link; a symlinked parent dir
			// inside the work dir is followed by the Root, one pointing outside is refused.
			info, err := root.Lstat(m)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				continue
			case err != nil:
				found = true
				skip(m, "refused: "+unwrapPathErr(err))
				continue
			}
			found = true
			if !info.IsDir() {
				add(m, info)
				continue
			}
			fs.WalkDir(fsys, m, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					skip(p, unwrapPathErr(err))
					return nil
				}
				info, err := d.Info()
				if err != nil {
					skip(p, unwrapPathErr(err))
					return nil
				}
				add(p, info)
				return nil
			})
		}
		if !found {
			missing = append(missing, pat)
		}
	}
	return entries, missing, skipped
}

func unwrapPathErr(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// linkInside reports whether a relative symlink at name resolves (lexically) inside the root.
func linkInside(name, target string) bool {
	if target == "" || path.IsAbs(target) {
		return false
	}
	t := path.Join(path.Dir(name), target)
	return t != ".." && !strings.HasPrefix(t, "../")
}

func writeEntry(tw *tar.Writer, root *os.Root, e fileEntry) error {
	hdr := &tar.Header{Name: e.name, Mode: int64(e.info.Mode().Perm()), ModTime: e.info.ModTime(), Format: tar.FormatPAX}
	switch {
	case e.link != "":
		hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, e.link
		return tw.WriteHeader(hdr)
	case e.info.IsDir():
		hdr.Typeflag, hdr.Name = tar.TypeDir, e.name+"/"
		return tw.WriteHeader(hdr)
	}
	f, err := root.Open(e.name)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr.Typeflag, hdr.Size = tar.TypeReg, e.info.Size()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	// Exactly Size bytes: a file that changed since it was listed fails the stream rather
	// than exceeding the limit.
	_, err = io.CopyN(tw, f, hdr.Size)
	return err
}
