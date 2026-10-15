package client

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

const getUsage = `usage: tun get [-o DIR] <id> PATH...

Download files or directories (recursively) from a finished job's work dir. PATHs are
relative to the workspace root (not the job's -C dir) and may be globs (*, ?, [...] within
one path element; no **). They are written to the same relative paths under the local
workspace root (git toplevel of the cwd, else the cwd), or under -o DIR. Existing files are
replaced; file modes and mtimes are kept; symlinks are recreated only if they stay inside
the work dir.

tun bazel jobs: a test or coverage command keeps its tests' logs (every attempt's test.log
and test.xml) as bazel-testlogs/<package>/<name>/... (Bazel's layout), up to 64MB per job.
A PATH that is a label (//pkg:name, //pkg, //:name) means that test's bazel-testlogs dir, so
tun get <id> //internal/server:server_test writes bazel-testlogs/internal/server/server_test/
test.log (add -o DIR if your workspace has Bazel's own bazel-testlogs symlink). Build outputs
stay in the session's output base: tun get can't fetch them yet.

exit status: 0 if every PATH matched; 1 if some matched nothing or were refused (one
"tun: warning:" line each on stderr; the rest is still written); 125 for tun errors
(job still running, unreachable server, over the server's --max-download limit).

flags:
`

// FilesResult describes a files download.
type FilesResult struct {
	Files   int      // regular files written
	Bytes   int64    // their total size
	Missing []string // requested paths/globs that matched nothing
	Skipped []string // "path: reason" for entries the server left out
}

// GetFiles downloads paths from job id's work dir and writes them under dir.
func (c *Client) GetFiles(ctx context.Context, id string, paths []string, dir string) (FilesResult, error) {
	q := url.Values{"path": paths}
	resp, err := c.request(ctx, "GET", "/v1/jobs/"+url.PathEscape(id)+"/files?"+q.Encode(), nil, 0)
	if err != nil {
		return FilesResult{}, err
	}
	defer resp.Body.Close()
	var res FilesResult
	for _, h := range []struct {
		name string
		dst  *[]string
	}{{api.HeaderMissing, &res.Missing}, {api.HeaderSkipped, &res.Skipped}} {
		for _, v := range resp.Header.Values(h.name) {
			if u, err := url.PathUnescape(v); err == nil {
				v = u
			}
			*h.dst = append(*h.dst, v)
		}
	}
	if err := extract(resp.Body, dir, &res); err != nil {
		return res, err
	}
	// The tar may end cleanly at an entry boundary even if the server gave up; it sets
	// this trailer only after writing everything.
	io.Copy(io.Discard, resp.Body)
	if resp.Trailer.Get("X-Tun-Complete") != "ok" {
		return res, fmt.Errorf("download from job %s was cut short (server did not confirm the archive was complete); %d files written", id, res.Files)
	}
	return res, nil
}

// extract writes a tar stream into dir, replacing existing files. Every write goes through
// an os.Root, so a hostile stream cannot write outside dir (via "..", absolute names or
// symlinks, local or in the stream).
func extract(r io.Reader, dir string, res *FilesResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	tr := tar.NewReader(r)
	type dirTime struct {
		name string
		t    time.Time
	}
	var dirs []dirTime
	defer func() { // after the files in them are written
		for _, d := range dirs {
			root.Chtimes(d.name, d.t, d.t)
		}
	}()
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading files from server: %w", err)
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if err := api.ValidFilePath(name); err != nil {
			return fmt.Errorf("server sent unsafe entry: %v", err)
		}
		name = path.Clean(name)
		if parent := path.Dir(name); parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		mode := fs.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if fi, err := root.Lstat(name); err == nil && !fi.IsDir() {
				root.Remove(name)
			}
			if err := root.MkdirAll(name, 0o755); err != nil {
				return err
			}
			root.Chmod(name, mode|0o700)
			dirs = append(dirs, dirTime{name, hdr.ModTime})
		case tar.TypeReg:
			if err := writeFile(root, name, mode, hdr.ModTime, tr); err != nil {
				return err
			}
			res.Files++
			res.Bytes += hdr.Size
		case tar.TypeSymlink:
			t := path.Join(path.Dir(name), hdr.Linkname)
			if hdr.Linkname == "" || path.IsAbs(hdr.Linkname) || t == ".." || strings.HasPrefix(t, "../") {
				return fmt.Errorf("server sent symlink %q -> %q that escapes the workspace", name, hdr.Linkname)
			}
			if err := removeNonDir(root, name); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
		default:
			return fmt.Errorf("server sent %q of unsupported type %q", name, hdr.Typeflag)
		}
	}
}

// writeFile writes a regular file via a temp file and rename, so a binary that is running
// locally can be replaced and a partial download never leaves a truncated file.
func writeFile(root *os.Root, name string, mode fs.FileMode, mtime time.Time, r io.Reader) error {
	tmp := path.Join(path.Dir(name), fmt.Sprintf(".tun-get-%d-%s", os.Getpid(), path.Base(name)))
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Chmod(tmp, mode)
	}
	if err == nil {
		err = root.Chtimes(tmp, mtime, mtime)
	}
	if err == nil {
		if fi, lerr := root.Lstat(name); lerr == nil && fi.IsDir() {
			err = fmt.Errorf("cannot write file %s: a directory is in the way", name)
		}
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		root.Remove(tmp)
	}
	return err
}

func removeNonDir(root *os.Root, name string) error {
	fi, err := root.Lstat(name)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.IsDir():
		return fmt.Errorf("cannot write symlink %s: a directory is in the way", name)
	}
	return root.Remove(name)
}

// localRoot is the workspace root containing cwd, where downloaded files go by default.
func localRoot(cwd string) (string, error) {
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", err
	}
	return workspace.Root(real)
}

// checkPaths validates PATH arguments client-side, before anything runs.
func checkPaths(paths []string) error {
	for _, p := range paths {
		if err := api.ValidFilePath(p); err != nil {
			return err
		}
	}
	return nil
}

// fetch downloads paths from job id into dir and reports the outcome on stderr. It returns
// false if the download failed and true otherwise (even with missing paths, which are
// warnings); all reports reports whether every path matched.
func fetch(ctx context.Context, c *Client, id string, paths []string, dir string, e Env) (ok, all bool) {
	res, err := c.GetFiles(ctx, id, paths, dir)
	for _, m := range res.Missing {
		fmt.Fprintf(e.Stderr, "tun: warning: %s: no such file in job %s's work dir\n", m, id)
	}
	for _, s := range res.Skipped {
		fmt.Fprintf(e.Stderr, "tun: warning: skipped %s\n", s)
	}
	if err != nil {
		fmt.Fprintf(e.Stderr, "tun: downloading files from job %s: %v\n", id, err)
		return false, false
	}
	fmt.Fprintf(e.Stderr, "tun: wrote %d files (%d bytes) from job %s to %s\n", res.Files, res.Bytes, id, dir)
	return true, len(res.Missing) == 0 && len(res.Skipped) == 0
}

// fetchOuts implements tun run --out once wait has returned code: it waits (briefly) for
// the job to reach a terminal state, then downloads. Missing paths only warn; a failed
// download turns a successful run into 125 so it isn't mistaken for success.
func fetchOuts(c *Client, id string, outs []string, dir string, code int, e Env) int {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	deadline := time.Now().Add(30 * time.Second) // a canceled job may take KillGrace to end
	for {
		j, err := c.Get(ctx, id)
		if err == nil && api.Terminal(j.State) {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(e.Stderr, "tun: job %s has not ended; not downloading --out files (later: tun get %s PATH...)\n", id, id)
			return failCode(code)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if ok, _ := fetch(ctx, c, id, outs, dir, e); !ok {
		return failCode(code)
	}
	return code
}

func failCode(code int) int {
	if code == 0 {
		return 125
	}
	return code
}

func getCmd(args []string, e Env) int {
	fs := newFlags("get", getUsage, e)
	out := fs.String("o", "", "write under `DIR` instead of the local workspace root")
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	if len(pos) < 2 {
		return errorf(e, "usage: tun get [-o DIR] <id> PATH... (job ids: tun ls)")
	}
	id, paths := pos[0], pos[1:]
	for i, p := range paths {
		if l, ok := testLogsPath(p); ok {
			paths[i] = l
		}
	}
	if err := checkPaths(paths); err != nil {
		return errorf(e, "get: %v", err)
	}
	dir := *out
	if dir == "" {
		if dir, err = localRoot(e.Cwd); err != nil {
			return errorf(e, "get: %v", err)
		}
	} else if !filepath.IsAbs(dir) {
		dir = filepath.Join(e.Cwd, dir)
	}
	ok, all := fetch(context.Background(), newClient(e), id, paths, dir, e)
	switch {
	case !ok:
		return 125
	case !all:
		return 1
	}
	return 0
}

// testLogsPath maps a test label (//pkg:name, //pkg for //pkg:<last element>, //:name) to
// where a tun bazel job keeps its logs: bazel-testlogs/pkg/name. ok is false for a non-label.
func testLogsPath(label string) (string, bool) {
	rest, ok := strings.CutPrefix(label, "//")
	if !ok {
		return "", false
	}
	pkg, name, found := strings.Cut(rest, ":")
	if !found {
		name = path.Base(pkg)
	}
	if name == "" || name == "." || name == "/" {
		return "", false
	}
	return path.Join("bazel-testlogs", pkg, name), true
}
