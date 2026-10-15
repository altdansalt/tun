package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

// Server-owned files (job.json, stdout, stderr) live beside the job's sandbox dir, never in
// it: under bwrap the job can't even see them.
func TestBwrapJobCannotSeeServerFiles(t *testing.T) {
	ts := bwrapServer(t)
	r := ts.tun("", "run", "--no-workspace", "-c", `[ "$(ls -A "$HOME/../..")" = sandbox ] && [ "$(ls -A "$HOME/..")" = "$(printf 'home\ntmp\nwork')" ]`)
	if r.code != 0 {
		t.Fatalf("job dir layout visible to the job: %+v", r)
	}
}

// With --sandbox none the job runs as the server user and can reach the job dir; the server
// must still not follow links or hard links planted in place of its own files. (None mode
// is documented as giving no isolation; this only keeps the logs API from being the leak.)
func TestNoneModeServerFilesNotFollowed(t *testing.T) {
	ts := startServer(t, server.Config{Sandbox: sandbox.None})
	target := filepath.Join(ts.data, "server-owned")
	os.WriteFile(target, []byte("unchanged"), 0o600)
	for name, plant := range map[string]string{
		"symlink stdout":   `ln -sf "$D/../../token" "$D/stdout"`,
		"hardlink stdout":  `rm "$D/stdout" && ln "$D/../../token" "$D/stdout"`,
		"symlink job.json": `ln -sf ` + target + ` "$D/job.json"`,
	} {
		t.Run(name, func(t *testing.T) {
			id := ts.detach("", "--no-workspace", "-c", `D=$HOME/../..; `+plant)
			j := ts.waitDone(id)
			if j.ExitCode == nil || *j.ExitCode != 0 {
				t.Fatalf("setup: %+v", j)
			}
			var out bytes.Buffer
			ts.client().Logs(context.Background(), id, "stdout", 0, false, &out)
			if strings.Contains(out.String(), ts.srv.Token()) {
				t.Fatal("logs API disclosed the token")
			}
			if b, _ := os.ReadFile(target); string(b) != "unchanged" {
				t.Fatalf("persistence wrote through a planted link: %s", b)
			}
		})
	}
}

// The job's env (client-controlled) is applied inside the sandbox, not to the unsandboxed
// bwrap process, and isn't visible in bwrap's argv on the host.
func TestBwrapEnvNotOnHostProcess(t *testing.T) {
	ts := bwrapServer(t)
	const marker = "tun-marker-6b1f"
	id := ts.detach("", "--no-workspace", "-e", "SECRET="+marker, "-e", "LD_PRELOAD=/nonexistent/"+marker+".so",
		"-c", `[ "${SECRET#tun-}" = marker-6b1f ] && touch "$HOME/ready" && sleep 2`)
	ts.waitFor(id, func(j api.Job) bool {
		_, err := os.Stat(filepath.Join(ts.data, "jobs", id, "sandbox", "home", "ready"))
		return err == nil || api.Terminal(j.State)
	})
	pids, _ := filepath.Glob("/proc/[0-9]*")
	found := 0
	for _, p := range pids {
		if comm, _ := os.ReadFile(p + "/comm"); strings.TrimSpace(string(comm)) != "bwrap" {
			continue
		}
		found++
		for _, f := range []string{"cmdline", "environ"} {
			if b, _ := os.ReadFile(p + "/" + f); bytes.Contains(b, []byte(marker)) {
				t.Errorf("%s/%s of bwrap contains the job env: %q", p, f, b)
			}
		}
	}
	if found == 0 {
		t.Error("no bwrap process found while the job ran")
	}
	if j := ts.waitDone(id); j.ExitCode == nil || *j.ExitCode != 0 {
		t.Fatalf("job didn't see its env: %+v", j)
	}
	if r := ts.tun("", "run", "--no-workspace", "-e", "LD_PRELOAD=/nonexistent/x.so", "--", "true"); strings.Count(r.stderr, "x.so") > 1 {
		t.Errorf("LD_PRELOAD applied outside the sandbox too: %+v", r)
	}
}

func TestBadRequests(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "--no-workspace", "-c", "printf abc")
	ts.waitDone(id)
	cases := []struct{ name, method, path, body, want string }{
		{"junk follow", "GET", "/v1/jobs/" + id + "/stdout?follow=yes", "", "follow"},
		{"negative offset", "GET", "/v1/jobs/" + id + "/stdout?offset=-1", "", "offset"},
		{"hex limit", "GET", "/v1/jobs?limit=0x10", "", "limit"},
		{"two objects", "POST", "/v1/jobs", `{"script":"true"}{"script":"true"}`, "after the job object"},
		{"timeout over max", "POST", "/v1/jobs", fmt.Sprintf(`{"script":"true","timeout_sec":%d}`, api.MaxTimeoutSec+1), "maximum"},
		{"NUL in env", "POST", "/v1/jobs", `{"script":"true","env":{"A":"x\u0000--bind"}}`, "NUL"},
		{"= in env name", "POST", "/v1/jobs", `{"script":"true","env":{"A=B":"x"}}`, "env"},
		{"NUL in argv", "POST", "/v1/jobs", `{"argv":["true","a\u0000b"]}`, "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(tc.method, ts.url+tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+ts.srv.Token())
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != 400 || !strings.HasPrefix(string(b), `{"error":`) || !strings.Contains(string(b), tc.want) {
				t.Errorf("status=%d body=%s; want 400 JSON error mentioning %q", resp.StatusCode, b, tc.want)
			}
		})
	}
	j, err := ts.client().Create(context.Background(), api.CreateJob{Script: "true", TimeoutSec: api.MaxTimeoutSec})
	if err != nil || ts.waitDone(j.ID).State != api.Exited {
		t.Errorf("max timeout rejected: %v", err)
	}
	if r := ts.tun("", "run", "--timeout", "169h", "--", "true"); r.code != 125 || !strings.Contains(r.stderr, "maximum") {
		t.Errorf("client --timeout over max: %+v", r)
	}
}

// Directory modes survive the trip, and the server can still clean up read-only trees.
func TestReadOnlyWorkspaceDir(t *testing.T) {
	ts := startServer(t, server.Config{})
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "ro", "sub"), 0o755)
	os.WriteFile(filepath.Join(ws, "ro", "sub", "f"), []byte("x"), 0o644)
	os.Chmod(filepath.Join(ws, "ro"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(ws, "ro"), 0o755) })
	r := ts.tun(ws, "run", "-C", "ro/sub", "-c", `stat -c %a ..; cat f; ! touch ../new 2>/dev/null`)
	if r.code != 0 || r.stdout != "555\nx" {
		t.Fatalf("got %+v", r)
	}
}
