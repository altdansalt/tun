package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/server"
)

// A job owns its job dir (the sandbox mounts it read-write), so it can replace work/ with a
// symlink. The files endpoint opens the Root on filepath.Join(jobDir, "work"), and
// os.OpenRoot follows a symlink for the root path itself, so the Root is now anywhere the
// job wants: here the whole data dir, i.e. the server token, other jobs' dirs and blobs.
func TestReviewFilesWorkSymlinkEscape(t *testing.T) {
	ts := startServer(t, server.Config{})
	victim := ts.detach("", "-c", "echo victim-secret > private.txt")
	ts.waitDone(victim)
	id := ts.detach("", "-c", `cd .. && mv work work.orig && ln -s ../.. work`) // jobs/<id>/../.. = data dir
	ts.waitDone(id)
	dst := t.TempDir()
	r := ts.tun("", "get", "-o", dst, id, "token", "jobs/"+victim+"/work/private.txt", "jobs/"+id+"/job.json")
	token, _ := os.ReadFile(filepath.Join(dst, "token"))
	if strings.TrimSpace(string(token)) == ts.srv.Token() {
		t.Errorf("job read the server token through tun get (exit %d, stderr %q)", r.code, r.stderr)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "jobs", victim, "work", "private.txt")); err == nil {
		t.Errorf("job read another job's file through tun get: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dst, "jobs", id, "job.json")); err == nil {
		t.Errorf("job read its own server-owned job.json")
	}
}

// If the server cannot open a listed file (mode 000), writeEntry fails after the headers
// are sent; the handler returns and the chunked body ends cleanly at an entry boundary,
// which archive/tar reports as io.EOF. The client then exits 0 with files silently missing.
func TestReviewFilesTruncatedStreamLooksLikeSuccess(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", `mkdir out && echo a > out/a && echo b > out/b && echo z > out/z && chmod 000 out/b`)
	ts.waitDone(id)
	dst := t.TempDir()
	r := ts.tun("", "get", "-o", dst, id, "out")
	_, errZ := os.Stat(filepath.Join(dst, "out", "z"))
	if r.code == 0 && errZ != nil {
		t.Errorf("tun get exited 0 but out/z is missing (stream silently truncated): %+v", r)
	}
}

// Same as TestReviewFilesWorkSymlinkEscape but under bwrap (the job dir is a read-write mount).
func TestReviewFilesWorkSymlinkEscapeBwrap(t *testing.T) {
	ts := bwrapServer(t)
	id := ts.detach("", "-c", `cd .. && mv work work.orig && ln -s ../.. work`)
	ts.waitDone(id)
	dst := t.TempDir()
	ts.tun("", "get", "-o", dst, id, "token")
	token, _ := os.ReadFile(filepath.Join(dst, "token"))
	if strings.TrimSpace(string(token)) == ts.srv.Token() {
		t.Errorf("bwrap job read the server token through tun get")
	}
}
