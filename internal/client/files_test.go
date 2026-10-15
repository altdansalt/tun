package client

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/server"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunOut(t *testing.T) {
	ts := startServer(t, server.Config{})
	ws := gitRepo(t) // the workspace root is the git toplevel, not the cwd
	os.WriteFile(filepath.Join(ws, "src.txt"), []byte("src"), 0o644)
	os.MkdirAll(filepath.Join(ws, "bin"), 0o755)
	os.WriteFile(filepath.Join(ws, "bin", "tool"), []byte("stale"), 0o644) // replaced
	os.MkdirAll(filepath.Join(ws, "sub"), 0o755)
	script := `mkdir -p ../bin ../cov/deep && printf new > ../bin/tool && chmod 755 ../bin/tool &&
		echo a > ../cov/a.out && echo b > ../cov/deep/b.out && ln -s a.out ../cov/link &&
		echo r1 > ../r1.xml && echo r2 > ../r2.xml && echo nope > ../notasked && exit 3`
	// cwd is sub/, but --out paths are relative to the workspace root.
	r := ts.tun(filepath.Join(ws, "sub"), "run", "--out", "bin/tool", "--out", "cov", "--out", "*.xml",
		"--out", "missing/thing", "-c", script)
	if r.code != 3 {
		t.Fatalf("exit %d, want the command's 3: %+v", r.code, r)
	}
	if !strings.Contains(r.stderr, "tun: warning: missing/thing: no such file") || !strings.Contains(r.stderr, "tun: wrote 5 files") {
		t.Errorf("stderr: %q", r.stderr)
	}
	for name, want := range map[string]string{"bin/tool": "new", "cov/a.out": "a\n", "cov/deep/b.out": "b\n", "r1.xml": "r1\n", "r2.xml": "r2\n", "src.txt": "src"} {
		if got := readFile(t, filepath.Join(ws, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(ws, "bin", "tool")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("bin/tool mode: %v", fi)
	}
	if l, err := os.Readlink(filepath.Join(ws, "cov", "link")); err != nil || l != "a.out" {
		t.Errorf("cov/link = %q, %v", l, err)
	}
	if _, err := os.Stat(filepath.Join(ws, "notasked")); err == nil {
		t.Error("unrequested file was downloaded")
	}
	ents, _ := os.ReadDir(filepath.Join(ws, "bin"))
	if len(ents) != 1 {
		t.Errorf("temp files left behind: %v", ents)
	}

	// Missing-only: still the command's exit code, files on timeout/any state too.
	r = ts.tun(ws, "run", "--out", "nothing", "--", "true")
	if r.code != 0 || !strings.Contains(r.stderr, "tun: warning: nothing:") {
		t.Errorf("missing only: %+v", r)
	}
	r = ts.tun(ws, "run", "--timeout", "1s", "--out", "late", "-c", "echo x > late; sleep 30")
	if r.code != 124 || readFile(t, filepath.Join(ws, "late")) != "x\n" {
		t.Errorf("timeout: %+v", r)
	}
	r = ts.tun(ws, "run", "--json", "--out", "j", "-c", "echo j > j")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "{") || readFile(t, filepath.Join(ws, "j")) != "j\n" {
		t.Errorf("--json: %+v", r)
	}
	for _, bad := range [][]string{{"--out", "../x"}, {"--out", "/etc/passwd"}, {"--out", "a/../../x"}, {"--detach", "--out", "x"}} {
		r := ts.tun(ws, append(append([]string{"run"}, bad...), "--", "true")...)
		if r.code != 125 || !strings.HasPrefix(r.stderr, "tun: run:") {
			t.Errorf("%q: %+v", bad, r)
		}
	}
}

func TestGet(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", "mkdir -p out && echo 1 > out/one && echo 2 > two")
	ts.waitDone(id)
	ws := t.TempDir()
	r := ts.tun(ws, "get", id, "out", "two")
	if r.code != 0 || readFile(t, filepath.Join(ws, "out", "one")) != "1\n" || readFile(t, filepath.Join(ws, "two")) != "2\n" {
		t.Fatalf("get: %+v", r)
	}
	r = ts.tun(ws, "get", "-o", "dest", id, "two", "gone")
	if r.code != 1 || readFile(t, filepath.Join(ws, "dest", "two")) != "2\n" || !strings.Contains(r.stderr, "tun: warning: gone:") {
		t.Errorf("get -o with missing: %+v", r)
	}
	running := ts.detach("", "--", "sleep", "30")
	ts.waitFor(running, func(j api.Job) bool { return j.State == "running" })
	if r := ts.tun(ws, "get", running, "x"); r.code != 125 || !strings.Contains(r.stderr, "is running") {
		t.Errorf("get on running job: %+v", r)
	}
	ts.tun("", "cancel", running)
	if r := ts.tun(ws, "get", "nojob", "x"); r.code != 125 || !strings.Contains(r.stderr, "no such job") {
		t.Errorf("get on unknown job: %+v", r)
	}
	if r := ts.tun(ws, "get", id); r.code != 125 {
		t.Errorf("get without paths: %+v", r)
	}
}

func TestFilesPathSafety(t *testing.T) {
	ts := startServer(t, server.Config{})
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("SECRET"), 0o644)
	id := ts.detach("", "-c", `mkdir -p d/in && echo ok > d/ok && echo in > d/in/f &&
		ln -s `+secret+` d/abs && ln -s ../../stdout d/up && ln -s in d/rel && ln -s / root &&
		ln -s .. parent && mkfifo d/fifo && echo out > ../outside`)
	ts.waitDone(id)

	// Raw API: bad paths are rejected outright.
	c := ts.client()
	for _, p := range []string{"../stdout", "/etc/passwd", "d/../../job.json", "", "a\\b", "[x"} {
		_, err := c.GetFiles(context.Background(), id, []string{p}, t.TempDir())
		var se *StatusError
		if !errors.As(err, &se) || se.Code != 400 {
			t.Errorf("path %q: err %v, want HTTP 400", p, err)
		}
	}
	req, _ := http.NewRequest("GET", ts.url+"/v1/jobs/"+id+"/files?path=%2e%2e/outside", nil)
	req.Header.Set("Authorization", "Bearer "+ts.srv.Token())
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 400 {
		t.Errorf("encoded ..: %v %v", resp, err)
	}
	req, _ = http.NewRequest("GET", ts.url+"/v1/jobs/"+id+"/files?path=d", nil)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 401 {
		t.Errorf("no token: %v %v", resp, err)
	}

	// Through symlinks: links that escape are never followed or sent; the rest is.
	dst := t.TempDir()
	res, err := c.GetFiles(context.Background(), id, []string{"d", "root/etc/passwd", "parent/stdout", "root", "*/abs"}, dst)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dst, "d", "ok")) != "ok\n" || readFile(t, filepath.Join(dst, "d", "rel", "f")) != "in\n" {
		t.Errorf("safe files not delivered: %+v", res)
	}
	for _, p := range []string{"d/abs", "d/up", "d/fifo", "root", "root/etc/passwd", "parent/stdout", "stdout"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); err == nil {
			t.Errorf("%s was delivered", p)
		}
	}
	skipped := strings.Join(res.Skipped, "\n")
	for _, want := range []string{"d/abs: symlink", "d/up: symlink", "d/fifo: not a regular", "root/etc/passwd: refused", "parent/stdout: refused", "root: symlink"} {
		if !strings.Contains(skipped, want) {
			t.Errorf("skipped list lacks %q:\n%s", want, skipped)
		}
	}
	filepath.Walk(dst, func(p string, fi os.FileInfo, err error) error {
		if fi != nil && fi.Mode().IsRegular() && strings.Contains(readFile(t, p), "SECRET") {
			t.Errorf("secret leaked into %s", p)
		}
		return nil
	})
	// tun get reports the refusals as warnings and exits 1.
	if r := ts.tun(t.TempDir(), "get", id, "d/abs", "d/ok"); r.code != 1 || !strings.Contains(r.stderr, "tun: warning: skipped d/abs") {
		t.Errorf("get skipped: %+v", r)
	}

	// Client side: a local symlink that escapes the workspace is not written through.
	ws := t.TempDir()
	elsewhere := t.TempDir()
	os.Symlink(elsewhere, filepath.Join(ws, "d"))
	if r := ts.tun(ws, "get", id, "d/ok"); r.code != 125 {
		t.Errorf("write through escaping local symlink: %+v", r)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("wrote outside the workspace: %v", ents)
	}
}

// TestExtractHostileTar feeds the client a tar a malicious server might send.
func TestExtractHostileTar(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]tar.Header{
		"dotdot":   {{Name: "../evil", Typeflag: tar.TypeReg}},
		"absolute": {{Name: outside + "/evil", Typeflag: tar.TypeReg}},
		"abs link": {{Name: "l", Typeflag: tar.TypeSymlink, Linkname: outside}},
		"up link":  {{Name: "a/l", Typeflag: tar.TypeSymlink, Linkname: "../.."}},
		"hardlink": {{Name: "h", Typeflag: tar.TypeLink, Linkname: "x"}},
	}
	for name, hdrs := range cases {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range hdrs {
			tw.WriteHeader(&h)
		}
		tw.Close()
		dir := t.TempDir()
		if err := extract(&buf, dir, &FilesResult{}); err == nil {
			t.Errorf("%s: extracted without error", name)
		}
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Errorf("wrote outside: %v", ents)
	}
}

func TestFilesSizeLimit(t *testing.T) {
	ts := startServer(t, server.Config{MaxDownload: 1000})
	ws := t.TempDir()
	r := ts.tun(ws, "run", "--out", "big", "--out", "small", "-c", "head -c 2000 /dev/zero > big; echo s > small")
	if r.code != 125 || !strings.Contains(r.stderr, "over the server limit of 1000 bytes") {
		t.Errorf("over limit, exit 0 command: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(ws, "big")); err == nil {
		t.Error("big was written")
	}
	r = ts.tun(ws, "run", "--out", "big", "-c", "head -c 2000 /dev/zero > big; exit 7")
	if r.code != 7 {
		t.Errorf("over limit keeps a failing command's code: %+v", r)
	}
	if r := ts.tun(ws, "run", "--out", "small", "-c", "head -c 2000 /dev/zero > big; echo s > small"); r.code != 0 {
		t.Errorf("under limit: %+v", r)
	}
}
