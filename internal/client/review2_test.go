package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/server"
)

// Review round 2: failing tests for feat/visibility, feat/fast-iteration and feat/dev-server.

// --tail 0 shows nothing, but the summary claims a line was shown when the output's last line
// has no trailing newline (lineTail.counts adds the partial line even when buf is empty).
func TestReview2RunTailZeroCounts(t *testing.T) {
	ts := startServer(t, server.Config{})
	r := ts.tun("", "run", "--tail", "0", "-c", "printf x; printf y >&2")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("run --tail 0: %+v", r)
	}
	if !strings.Contains(r.stderr, "showed the last 0 of 1 stdout lines and 0 of 1 stderr lines") {
		t.Errorf("summary should say 0 lines were shown: %q", r.stderr)
	}
}

// A negative --tail is silently treated as "no --tail" (full logs / live streaming) instead of
// being rejected like other bad flag values.
func TestReview2NegativeTailRejected(t *testing.T) {
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", "seq 1 5")
	ts.waitDone(id)
	if r := ts.tun("", "logs", "--tail", "-2", id); r.code != 125 || !strings.Contains(r.stderr, "tail") {
		t.Errorf("logs --tail -2: want exit 125 naming --tail, got %+v", r)
	}
	if r := ts.tun("", "run", "--tail", "-2", "--", "echo", "hi"); r.code != 125 || !strings.Contains(r.stderr, "tail") {
		t.Errorf("run --tail -2: want exit 125 naming --tail, got %+v", r)
	}
}

// ciWorkspace is a copy of scripts/ci.sh (and the named extra scripts) with a fake go on PATH
// that prints its test arguments, and a fake scripts/dev-tun.sh that reports it ran.
func ciWorkspace(t *testing.T, extra ...string) string {
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "scripts"), 0o755)
	os.MkdirAll(filepath.Join(ws, "bin"), 0o755)
	for _, name := range append([]string{"ci.sh"}, extra...) {
		b, err := os.ReadFile("../../scripts/" + name)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(ws, "scripts", name), b, 0o755)
	}
	os.WriteFile(filepath.Join(ws, "bin/go"), []byte(`#!/bin/bash
case $1 in
version) echo "go version go1.27.2 linux/amd64" ;;
test) for a in "$@"; do echo "arg:$a"; done; printf 'ok  \tpkg/a\t0.5s\n' ;;
*) echo "step:$1" ;;
esac
`), 0o755)
	os.WriteFile(filepath.Join(ws, "scripts/dev-tun.sh"), []byte("#!/bin/bash\necho e2e-ran\n"), 0o755)
	return ws
}

// TEST_FLAGS='-run TestA|TestB' (no inner quotes) worked before ci.sh started eval'ing
// TEST_FLAGS: plain word splitting passed TestA|TestB to go test. Now the | is a shell
// pipe inside `flags=(...)`, a syntax error, and ci.sh exits 2. Same for -run 'Test(A|B)'.
func TestReview2CIScriptUnquotedAlternation(t *testing.T) {
	ts := startServer(t, server.Config{})
	ws := ciWorkspace(t)
	for _, c := range []struct{ flags, want string }{
		{`-run TestA|TestB`, "arg:-run\narg:TestA|TestB\n"},
		{`-run Test(A|B)`, "arg:-run\narg:Test(A|B)\n"},
	} {
		r := ts.tun(ws, "run", "-e", "TEST_FLAGS="+c.flags, "-c", `E2E=0 PATH=$PWD/bin:$PATH exec ./scripts/ci.sh`)
		if r.code != 0 || !strings.Contains(r.stdout, c.want) {
			t.Errorf("TEST_FLAGS=%q: want go test to get %q, got %+v", c.flags, c.want, r)
		}
	}
}

// SPEC.md: "CI runs this smoke suite by default; set E2E=0 to skip or E2E=1 to enable
// explicitly." With -run, E2E=1 is silently ignored.
func TestReview2CIScriptExplicitE2EWithRun(t *testing.T) {
	ts := startServer(t, server.Config{})
	ws := ciWorkspace(t)
	r := ts.tun(ws, "run", "-e", "TEST_FLAGS=-run TestA", "-e", "E2E=1", "-c", `PATH=$PWD/bin:$PATH exec ./scripts/ci.sh`)
	if r.code != 0 || !strings.Contains(r.stdout, "e2e-ran") {
		t.Errorf("E2E=1 with -run should run the smoke suite: %+v", r)
	}
}

// scripts/bb-ci.sh wraps TEST_FLAGS in single quotes when building the remote --script, so
// the form AGENTS.md recommends, TEST_FLAGS="-run 'A|B'", becomes
// TEST_FLAGS='-run 'A|B” ./scripts/ci.sh: a pipeline into a command named B.
func TestReview2BBCIQuotesTestFlags(t *testing.T) {
	ts := startServer(t, server.Config{})
	ws := ciWorkspace(t, "bb-ci.sh")
	os.WriteFile(filepath.Join(ws, "scripts/ci.sh"), []byte("#!/bin/bash\necho \"TEST_FLAGS=<$TEST_FLAGS>\"\n"), 0o755)
	os.WriteFile(filepath.Join(ws, "bin/git"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	// Fake bb: run the --script argument the way the remote runner would.
	os.WriteFile(filepath.Join(ws, "bin/bb"), []byte(`#!/bin/bash
for a in "$@"; do case $a in --script=*) exec bash -c "${a#--script=}" ;; esac; done
exit 99
`), 0o755)
	const want = "TEST_FLAGS=<-run 'A|B'>"
	r := ts.tun(ws, "run", "-e", "TEST_FLAGS=-run 'A|B'", "-e", "BUILDBUDDY_API_KEY=x",
		"-c", `PATH=$PWD/bin:$PATH exec ./scripts/bb-ci.sh`)
	if r.code != 0 || !strings.Contains(r.stdout, want) {
		t.Errorf("bb-ci.sh should pass TEST_FLAGS through unchanged (%q): %+v", want, r)
	}
}
