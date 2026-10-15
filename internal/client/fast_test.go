package client

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/server"
)

func TestLogsTail(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	id := ts.detach("", "-c", "seq 1 5; printf partial; printf 'e1\\ne2\\n' >&2")
	ts.waitDone(id)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--tail", "2"}, "5\npartial"},
		{[]string{"--tail", "1"}, "partial"},
		{[]string{"--tail", "0"}, ""},
		{[]string{"--tail", "100"}, "1\n2\n3\n4\n5\npartial"},
		{[]string{"--tail", "1", "--stderr"}, "e2\n"},
		{[]string{"--stderr", "--tail", "5"}, "e1\ne2\n"},
		{[]string{"-f", "--tail", "3"}, "4\n5\npartial"}, // already ended: like without -f
	} {
		r := ts.tun("", append(append([]string{"logs"}, c.args...), id)...)
		if r.code != 0 || r.stdout != c.want {
			t.Errorf("logs %v: got %+v, want %q", c.args, r, c.want)
		}
	}

	// -f --tail on a running job: the last lines so far, then everything after them.
	id = ts.detach("", "-c", "seq 1 3; while [ ! -e go ]; do sleep 0.05; done; echo 4; echo 5")
	ts.waitFor(id, func(api.Job) bool {
		var b strings.Builder
		ts.client().Logs(context.Background(), id, "stdout", 0, false, &b)
		return b.String() == "1\n2\n3\n"
	})
	// Let the job continue only once the follower has printed its tail, else it may connect
	// after 4 and 5 were written and (correctly) show just "5".
	fw := &firstWrite{ch: make(chan struct{})}
	done := make(chan error)
	go func() { done <- ts.client().LogsTail(context.Background(), id, "stdout", 1, true, fw) }()
	<-fw.ch
	os.WriteFile(filepath.Join(ts.data, "jobs", id, "sandbox", "work", "go"), nil, 0o644)
	if err := <-done; err != nil || fw.String() != "3\n4\n5\n" {
		t.Errorf("logs -f --tail 1: %q, %v", fw.String(), err)
	}

	for _, q := range []string{"tail=-1", "tail=x", "tail=1&offset=0"} {
		req, _ := http.NewRequest("GET", ts.url+"/v1/jobs/"+id+"/stdout?"+q, nil)
		req.Header.Set("X-Tun-Token", ts.srv.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("%s: HTTP %d, want 400", q, resp.StatusCode)
		}
	}
}

func TestRunTail(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	r := ts.tun("", "run", "--tail", "2", "-c", "seq 1 10; printf 'e1\\ne2\\ne3' >&2; exit 3")
	id := ts.lastID()
	if r.code != 3 || r.stdout != "9\n10\n" {
		t.Fatalf("run --tail 2: %+v", r)
	}
	lines := strings.Split(r.stderr, "\n")
	if len(lines) < 4 || !strings.HasPrefix(lines[0], "tun: job "+id+" (--tail 2") || !strings.Contains(lines[0], "tun logs "+id) {
		t.Errorf("first line should name the job and how to get the full logs: %q", r.stderr)
	}
	if !strings.Contains(r.stderr, "\ne2\ne3\n") || strings.Contains(r.stderr, "\ne1\n") {
		t.Errorf("stderr tail: %q", r.stderr)
	}
	for _, want := range []string{"last 2 of 10 stdout lines and 2 of 3 stderr lines", "tun logs " + id + ", tun logs --stderr " + id} {
		if !strings.Contains(r.stderr, want) {
			t.Errorf("stderr %q missing %q", r.stderr, want)
		}
	}

	// Short output is shown whole; a job failure message still follows.
	r = ts.tun("", "run", "--tail", "50", "--timeout", "1s", "-c", "echo hi; sleep 5")
	if r.code != 124 || r.stdout != "hi\n" || !strings.Contains(r.stderr, "last 1 of 1 stdout lines") || !strings.Contains(r.stderr, "timed out") {
		t.Errorf("run --tail 50 with timeout: %+v", r)
	}

	for _, args := range [][]string{
		{"--tail", "1", "--json"}, {"--tail", "1", "--detach"}, {"--tail", "1", "--service"},
	} {
		r := ts.tun("", append(append([]string{"run"}, args...), "--", "true")...)
		if r.code != 125 || !strings.Contains(r.stderr, "--tail") {
			t.Errorf("run %v: %+v", args, r)
		}
	}
}

func TestLineTail(t *testing.T) {
	t.Parallel()
	lt := &lineTail{n: 2}
	for _, p := range []string{"a\nb", "\nc\n", "", "d"} {
		lt.Write([]byte(p))
	}
	if kept, total := lt.counts(); string(lt.buf) != "c\nd" || kept != 2 || total != 4 {
		t.Errorf("got %q %d/%d", lt.buf, kept, total)
	}
	big := &lineTail{n: 3}
	big.Write([]byte(strings.Repeat("x", 3<<20)))
	if len(big.buf) != 1<<20 || !big.cut {
		t.Errorf("1MiB cap: %d %v", len(big.buf), big.cut)
	}
}

// TestCIScript runs scripts/ci.sh through tun run -e TEST_FLAGS=..., the way agents do, with a
// fake go that records its arguments and prints canned go test output.
func TestCIScript(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	ci, err := os.ReadFile("../../scripts/ci.sh")
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	os.MkdirAll(filepath.Join(ws, "scripts"), 0o755)
	os.MkdirAll(filepath.Join(ws, "bin"), 0o755)
	os.WriteFile(filepath.Join(ws, "scripts/ci.sh"), ci, 0o755)
	os.WriteFile(filepath.Join(ws, "bin/go"), []byte(`#!/bin/bash
case $1 in
version) echo "go version go1.27.2 linux/amd64" ;;
test) for a in "$@"; do echo "arg:$a"; done; printf "$FAKE_OUT"; exit ${FAKE_CODE:-0} ;;
*) echo "step:$1" ;;
esac
`), 0o755)
	const script = `E2E=0 PATH=$PWD/bin:$PATH exec ./scripts/ci.sh`
	ci2 := func(flags, out string, extra ...string) result {
		args := []string{"run", "-e", "TEST_FLAGS=" + flags, "-e", "FAKE_OUT=" + out}
		return ts.tun(ws, append(append(args, extra...), "-c", script)...)
	}
	ran := `ok  \tpkg/a\t0.5s\nok  \tpkg/b\t0.01s [no tests to run]\n?   \tpkg/c\t[no test files]\n`
	none := `ok  \tpkg/a\t0.01s [no tests to run]\nok  \tpkg/b\t0.01s [no tests to run]\n?   \tpkg/c\t[no test files]\n`

	// The quoting from the task: -run "A|B" reaches go test as one argument, without quotes.
	r := ci2(`-run "TestA|TestB" -v`, ran)
	if r.code != 0 || !strings.Contains(r.stdout, "arg:-run\narg:TestA|TestB\narg:-v\n") {
		t.Errorf("quoted -run: %+v", r)
	}
	if strings.Contains(r.stdout, "step:vet") || strings.Contains(r.stdout, "step:build") {
		t.Errorf("-run should skip vet/build: %q", r.stdout)
	}
	if r := ci2(`-run 'A|B'`, ran); r.code != 0 || !strings.Contains(r.stdout, "arg:A|B\n") {
		t.Errorf("single-quoted -run: %+v", r)
	}
	if r := ci2(`-run=TestA`, ran, "-e", "CI_FULL=1"); r.code != 0 || !strings.Contains(r.stdout, "step:vet\nstep:build\n") {
		t.Errorf("CI_FULL=1 should vet and build: %+v", r)
	}
	if r := ci2("", ran); r.code != 0 || !strings.Contains(r.stdout, "step:vet\nstep:build\n") || strings.Contains(r.stdout, "arg:\n") {
		t.Errorf("no TEST_FLAGS: %+v", r)
	}
	// TEST_FLAGS is only unquoted, never evaluated: no globbing, substitution or pipes.
	if r := ci2(`-run $(echo\ X)|* -v`, ran); r.code != 0 || !strings.Contains(r.stdout, "arg:-run\narg:$(echo X)|*\narg:-v\n") {
		t.Errorf("TEST_FLAGS evaluated: %+v", r)
	}

	// A -run pattern matching nothing anywhere fails loudly.
	r = ci2(`-run TestNope`, none)
	if r.code != 1 || !strings.Contains(r.stderr, "matched no tests in any package") {
		t.Errorf("no match: %+v", r)
	}
	// ... but not when go test itself failed (its status wins), or with -bench/-list.
	if r := ci2(`-run TestNope`, none, "-e", "FAKE_CODE=2"); r.code != 2 {
		t.Errorf("go test failure: %+v", r)
	}
	if r := ci2(`-run '^$' -bench .`, none); r.code != 0 {
		t.Errorf("-bench: %+v", r)
	}
	if r := ci2(`-run "unterminated`, ran); r.code != 2 || !strings.Contains(r.stderr, "cannot parse TEST_FLAGS") {
		t.Errorf("bad quoting: %+v", r)
	}
}

// firstWrite is a writer that closes ch on the first write.
type firstWrite struct {
	strings.Builder
	ch   chan struct{}
	once sync.Once
}

func (w *firstWrite) Write(p []byte) (int, error) {
	defer w.once.Do(func() { close(w.ch) })
	return w.Builder.Write(p)
}
