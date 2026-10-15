package client

import (
	"archive/tar"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/altdansalt/tun/internal/server"
)

// tarNames lists the entries of a local tar file.
func tarNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var names []string
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// scripts/gazelle.sh through tun run --out with a fake bazel: only the BUILD files gazelle
// changed or created come back, in gazelle.tar (empty when nothing changed). The first run
// builds gazelle with Bazel and keeps the binary in the shared cache; later runs call it
// directly.
func TestGazelleScriptFetchesChangedBuildFiles(t *testing.T) {
	t.Parallel()
	ts := startServer(t, server.Config{})
	ws := ciWorkspace(t, "gazelle.sh")
	for _, f := range []string{"MODULE.bazel", "MODULE.bazel.lock", "BUILD.bazel"} {
		os.WriteFile(filepath.Join(ws, f), []byte("# "+f+"\n"), 0o644)
	}
	for _, d := range []string{"a", "b", "bazel-out/x"} {
		os.MkdirAll(filepath.Join(ws, d), 0o755)
		os.WriteFile(filepath.Join(ws, d, "BUILD.bazel"), []byte("# "+d+"\n"), 0o644)
	}
	// The fake gazelle edits a/ and creates c/ unless NOOP is set; bazel-out/ is never listed.
	// The fake bazel "builds" it into bazel-bin's runfiles (rules_go's layout) and runs it.
	os.WriteFile(filepath.Join(ws, "bin/bazel"), []byte(`#!/bin/bash
r=$PWD/bazel-bin/gazelle.runfiles
case " $* " in
*" info "*) echo "$PWD/bazel-bin" ;;
*" run "*)
  echo "bazel $*" >&2
  mkdir -p $r/gazelle+/cmd/gazelle/gazelle_ $r/gazelle++go_deps+bazel_gazelle_go_repository_config
  echo cfg > $r/gazelle++go_deps+bazel_gazelle_go_repository_config/WORKSPACE
  cat > $r/gazelle+/cmd/gazelle/gazelle_/gazelle <<'G'
#!/bin/bash
echo "gazelle $*" >&2
[ -n "${NOOP:-}" ] && exit 0
cd "$BUILD_WORKSPACE_DIRECTORY"
echo 'go_library()' >> a/BUILD.bazel
mkdir -p c && echo '# new' > c/BUILD.bazel
echo changed > bazel-out/x/BUILD.bazel
G
  chmod +x $r/gazelle+/cmd/gazelle/gazelle_/gazelle
  shift $(( $(printf '%s\n' "$@" | grep -n -m1 -x -- -- | cut -d: -f1) ))
  BUILD_WORKSPACE_DIRECTORY=$PWD $r/gazelle+/cmd/gazelle/gazelle_/gazelle "$@" ;;
esac
`), 0o755)
	run := `PATH=$PWD/bin:$PATH exec ./scripts/gazelle.sh internal`
	r := ts.tun(ws, "run", "--out", "gazelle.tar", "-c", run)
	if r.code != 0 || !strings.Contains(r.stdout, "gazelle.sh: 2 BUILD file(s) changed: a/BUILD.bazel c/BUILD.bazel") ||
		!strings.Contains(r.stdout, "+go_library()") || !strings.Contains(r.stderr, "//:gazelle -- update internal") {
		t.Fatalf("gazelle.sh: %+v", r)
	}
	if got := tarNames(t, filepath.Join(ws, "gazelle.tar")); !slices.Equal(got, []string{"a/BUILD.bazel", "c/BUILD.bazel"}) {
		t.Errorf("gazelle.tar: %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "a/BUILD.bazel")); string(b) != "# a\n" {
		t.Errorf("local tree changed before extracting: %q", b)
	}

	// Cached: no Bazel at all, gazelle gets the wrapper's arguments.
	r = ts.tun(ws, "run", "-e", "NOOP=1", "-e", "GAZELLE_CHECK=1", "--out", "gazelle.tar", "-c", run)
	if r.code != 0 || !strings.Contains(r.stdout, "gazelle.sh: 0 BUILD file(s) changed (in gazelle.tar)") ||
		strings.Contains(r.stderr, "bazel ") || !strings.Contains(r.stderr, "gazelle.sh: cached gazelle") ||
		!regexp.MustCompile(`gazelle update -bzlmod -repo_config \S+/repo_config internal`).MatchString(r.stderr) {
		t.Fatalf("gazelle.sh without changes, cached: %+v", r)
	}
	if got := tarNames(t, filepath.Join(ws, "gazelle.tar")); len(got) != 0 {
		t.Errorf("gazelle.tar without changes: %q", got)
	}
	if r = ts.tun(ws, "run", "-e", "GAZELLE_CHECK=1", "-c", run); r.code != 1 || !strings.Contains(r.stderr, "cached gazelle") {
		t.Errorf("GAZELLE_CHECK=1 with changes: want exit 1, got %+v", r)
	}
	if r = ts.tun(ws, "run", "-e", "GAZELLE_CACHE=0", "-e", "NOOP=1", "-c", run); r.code != 0 || !strings.Contains(r.stderr, "bazel ") {
		t.Errorf("GAZELLE_CACHE=0 should go through Bazel: %+v", r)
	}
	// Another MODULE.bazel is another key: through Bazel again.
	os.WriteFile(filepath.Join(ws, "MODULE.bazel"), []byte("# changed\n"), 0o644)
	if r = ts.tun(ws, "run", "-e", "NOOP=1", "-c", run); r.code != 0 || !strings.Contains(r.stderr, "bazel ") {
		t.Errorf("new MODULE.bazel should go through Bazel: %+v", r)
	}
}
