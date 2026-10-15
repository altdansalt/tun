package client

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/sandbox"
	"github.com/altdansalt/tun/internal/server"
)

func bwrapServer(t *testing.T) *testServer {
	t.Helper()
	if !sandbox.BwrapUsable() {
		t.Skip("bwrap not installed or unusable on this host")
	}
	return startServer(t, server.Config{Sandbox: sandbox.Bwrap})
}

func TestBwrapIsolation(t *testing.T) {
	t.Parallel()
	ts := bwrapServer(t)
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "f"), []byte("ws"), 0o644)
	first := ts.tun(ws, "run", "-c", "echo secret > mine; echo $TUN_JOB_ID")
	if first.code != 0 {
		t.Fatalf("first job: %+v", first)
	}
	other := filepath.Join(ts.data, "jobs", strings.TrimSpace(first.stdout))
	checks := map[string]string{
		"other job dir hidden": "! ls " + other + "/work",
		"other job unreadable": "! cat " + other + "/work/mine",
		"token unreadable":     "! cat " + ts.data + "/token",
		"blobs hidden":         "! ls " + ts.data + "/blobs",
		"only own job visible": `[ "$(ls ` + ts.data + `/jobs)" = "$TUN_JOB_ID" ]`,
		"root read-only":       "! touch /etc/tun-test 2>/dev/null && ! touch /usr/tun-test 2>/dev/null",
		"writable dirs":        `touch "$HOME/h" "$TMPDIR/t" /tmp/t2 "$GOCACHE/t" "$XDG_CACHE_HOME/t" ./w`,
		"tmp is private":       `touch /tmp/t3 && [ -e "$TMPDIR/t3" ]`,
		"pid namespace":        `[ "$(ls /proc | grep -c '^[0-9]')" -lt 10 ]`,
		"workspace present":    `[ "$(cat f)" = ws ]`,
	}
	if home, err := os.UserHomeDir(); err == nil {
		checks["server home hidden"] = `[ -z "$(ls -A ` + home + ` | grep -v -e '^go$' -e '^.local$' -e '^.cache$' -e '^bin$')" ]`
	}
	for name, script := range checks {
		if r := ts.tun(ws, "run", "-c", script); r.code != 0 {
			t.Errorf("%s: %+v", name, r)
		}
	}
}

func TestBwrapGoBuild(t *testing.T) {
	t.Parallel()
	ts := bwrapServer(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	ws := t.TempDir()
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module hello\n\ngo 1.21\n"), 0o644)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello from tun\") }\n"), 0o644)
	r := ts.tun(ws, "run", "-e", "GOTOOLCHAIN=local", "-c", "go build -o hello . && ./hello && go vet ./...")
	if r.code != 0 || r.stdout != "hello from tun\n" {
		t.Fatalf("go build in sandbox: %+v", r)
	}
}

func TestBwrapTimeoutKillsTree(t *testing.T) {
	t.Parallel()
	ts := bwrapServer(t)
	r := ts.tun("", "run", "--timeout", "1s", "-c", "trap '' TERM; sleep 60 & sleep 60")
	if r.code != 124 {
		t.Fatalf("got %+v", r)
	}
}
