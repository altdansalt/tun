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
	"syscall"

	"github.com/altdansalt/tun/internal/api"
)

// DefaultMaxDownload is the default limit on the total file bytes of one files request.
const DefaultMaxDownload = 1 << 30

// maxFileEntries caps the entries of one files request, bounding the server's memory.
var maxFileEntries = 200_000

// completeTrailer is an HTTP trailer the server sets only after the whole tar was written,
// so a client can tell a complete archive from one cut short at an entry boundary.
const completeTrailer = "X-Tun-Complete"

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
		if strings.HasPrefix(p, "//") && j.SessionID != "" {
			continue
		}
		if err := api.ValidFilePath(p); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	snap := s.snapshot(j)
	if !api.Terminal(snap.State) {
		writeErr(w, 409, fmt.Sprintf("job %s is %s; files can be fetched once it has ended", snap.ID, snap.State))
		return
	}
	if snap.SessionID != "" || snap.Runner != "" && snap.Runner != api.LocalRunner {
		s.runnerFiles(w, j, paths)
		return
	}
	root, err := s.openWork(j.ID)
	if errors.Is(err, fs.ErrNotExist) {
		writeErr(w, 410, fmt.Sprintf("job %s's work dir no longer exists (work dirs are deleted some time after a job ends; see tun serve --keep-work)", j.ID))
		return
	}
	if err != nil {
		writeErr(w, 500, fmt.Sprintf("opening job %s's work dir: %v", j.ID, unwrapPathErr(err)))
		return
	}
	defer root.Close()

	entries, missing, skipped, err := collect(root, paths)
	if err != nil {
		writeErr(w, 413, err.Error())
		return
	}
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
	w.Header().Set("Trailer", completeTrailer)
	w.WriteHeader(200)
	tw := tar.NewWriter(w)
	for _, e := range entries {
		if err := writeEntry(tw, root, e); err != nil {
			// The status line is gone; break the connection so the client can't mistake a
			// short archive for a complete one.
			panic(http.ErrAbortHandler)
		}
	}
	if err := tw.Close(); err != nil {
		panic(http.ErrAbortHandler)
	}
	w.Header().Set(completeTrailer, "ok")
}

// openWork opens the job's sandbox/work dir as a Root without following symlinks at any
// level: the job can replace sandbox/work (or, under sandbox none, anything) with a link.
func (s *Server) openWork(id string) (*os.Root, error) { return openWorkDir(s.cfg.DataDir, id) }

func openWorkDir(dataDir, id string) (*os.Root, error) {
	r, err := os.OpenRoot(filepath.Join(dataDir, "jobs"))
	if err != nil {
		return nil, err
	}
	for _, name := range []string{id, "sandbox", "work"} {
		next, err := openDirNoFollow(r, name)
		r.Close()
		if err != nil {
			return nil, err
		}
		r = next
	}
	return r, nil
}

// openDirNoFollow opens parent/name as a Root only if name is a real directory, not a
// symlink. Root.OpenRoot follows links (within parent), so the Lstat before and the
// identity check after make sure the Root is the directory entry itself, with no window
// for a swap.
func openDirNoFollow(parent *os.Root, name string) (*os.Root, error) {
	fi, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory (%v)", name, fi.Mode().Type())
	}
	r, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	if st, err := r.Stat("."); err != nil || !os.SameFile(fi, st) {
		r.Close()
		return nil, fmt.Errorf("%s changed while being opened", name)
	}
	return r, nil
}

// collect expands the requested paths into tar entries (parents before children, no
// duplicates), plus the patterns that matched nothing and the entries skipped as unsafe.
func collect(root *os.Root, patterns []string) (entries []fileEntry, missing, skipped []string, err error) {
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
		case m.IsRegular():
			// Check it can be read now, so an unreadable file is reported up front rather
			// than breaking the stream.
			f, err := openRegular(root, name)
			if err != nil {
				skipped = append(skipped, name+": "+unwrapPathErr(err))
				return
			}
			f.Close()
		case m.IsDir():
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
			if len(entries) > maxFileEntries {
				break
			}
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
				if len(entries) > maxFileEntries {
					return fs.SkipAll
				}
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
		if len(entries) > maxFileEntries {
			return nil, nil, nil, fmt.Errorf("requested paths have more than %d entries; fetch fewer or narrower paths", maxFileEntries)
		}
	}
	return entries, missing, skipped, nil
}

// openRegular opens a file for reading without blocking (a FIFO swapped in for a listed
// file must not hang the handler) and checks it is still a regular file.
func openRegular(root *os.Root, name string) (*os.File, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is no longer a regular file", name)
	}
	return f, nil
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
	f, err := openRegular(root, e.name)
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
