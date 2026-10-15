package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/server"
	"github.com/altdansalt/tun/internal/workspace"
)

// Review regressions (adapted from origin/review/features-artifacts and
// origin/review/features-gc-artifacts to the jobs/<id>/sandbox/{work,home,tmp} layout).

// A job owns sandbox/, so it can replace sandbox/work with a symlink. The files endpoint
// must not follow it: not to the data dir (token, other jobs) nor to its own server-owned
// job dir (job.json, stdout).
func TestReviewFilesWorkSymlinkEscape(t *testing.T) {
	ts := startServer(t, server.Config{})
	victim := ts.detach("", "-c", "echo victim-secret > private.txt")
	ts.waitDone(victim)
	for _, target := range []string{"../../..", ".."} { // data dir; jobs/<id>
		id := ts.detach("", "-c", `cd .. && mv work work.orig && ln -s `+target+` work`)
		ts.waitDone(id)
		dst := t.TempDir()
		r := ts.tun("", "get", "-o", dst, id, "token", "jobs/"+victim+"/sandbox/work/private.txt", "job.json", "stdout")
		if r.code != 125 {
			t.Errorf("work -> %s: exit %d, want 125: %+v", target, r.code, r)
		}
		if strings.Contains(r.stderr, ts.data) {
			t.Errorf("error leaks the server's data dir path: %q", r.stderr)
		}
		filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
			if fi != nil && !fi.IsDir() {
				t.Errorf("work -> %s: delivered %s", target, p)
			}
			return nil
		})
	}
}

// Same under bwrap: sandbox/ is a read-write mount, so the job can still plant the link.
func TestReviewFilesWorkSymlinkEscapeBwrap(t *testing.T) {
	ts := bwrapServer(t)
	id := ts.detach("", "-c", `cd .. && mv work work.orig && ln -s ../../.. work`)
	ts.waitDone(id)
	dst := t.TempDir()
	ts.tun("", "get", "-o", dst, id, "token")
	token, _ := os.ReadFile(filepath.Join(dst, "token"))
	if strings.TrimSpace(string(token)) == ts.srv.Token() {
		t.Errorf("bwrap job read the server token through tun get")
	}
}

// An unreadable file is reported up front (skipped, exit 1) instead of silently cutting
// the stream short with exit 0; the rest is delivered.
func TestReviewFilesTruncatedStreamLooksLikeSuccess(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can read mode 000 files")
	}
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", `mkdir out && echo a > out/a && echo b > out/b && echo z > out/z && chmod 000 out/b`)
	ts.waitDone(id)
	dst := t.TempDir()
	r := ts.tun("", "get", "-o", dst, id, "out")
	if _, err := os.Stat(filepath.Join(dst, "out", "z")); err != nil || r.code != 1 || !strings.Contains(r.stderr, "skipped out/b: permission denied") {
		t.Errorf("unreadable file: %+v (out/z: %v)", r, err)
	}
}

// After the work dir is deleted (retention in feat/gc), the endpoint says so (410).
func TestReviewFilesAfterWorkDeleted(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", "echo hi > out.txt")
	ts.waitDone(id)
	if err := workspace.RemoveAll(filepath.Join(ts.data, "jobs", id, "sandbox", "work")); err != nil {
		t.Fatal(err)
	}
	r := ts.tun(t.TempDir(), "get", id, "out.txt")
	if r.code != 125 || !strings.Contains(r.stderr, "HTTP 410") || !strings.Contains(r.stderr, "keep-work") || strings.Contains(r.stderr, ts.data) {
		t.Errorf("get after work deleted: %+v", r)
	}
}
