package client

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/altdansalt/tun/internal/api"
	"github.com/altdansalt/tun/internal/workspace"
)

const bazelUsage = `usage: tun bazel [--session NAME] [--cold] [--timeout DUR] [--json] <bazel command> [args...]

Run a Bazel command in a warm session on the tun server, e.g. tun bazel test //... .
The bazel command and its args pass through unchanged; tun adds only startup options (the
session's output base, its own --bazelrc) and, through that rc file, --repository_cache and
--disk_cache (with a size cap). These caches and bazelisk's downloads are this client's own
on the server: they survive --cold and session expiry, and other clients can't touch them. Your workspace's .bazelrc applies as usual. Bazel's
output streams back; the exit status is Bazel's (124 on --timeout, 125 for tun's own errors,
as for tun run). Bazel startup options of your own are not supported.

Remote cache: when the server has one (tun serve's default), the rc file points Bazel at it
instead of the disk cache (--remote_cache through a unix socket in the session; no token or
network needed), so a
new or --cold session, or one on another host later, gets cache hits for what this client
built before. Action results you upload are yours: other clients don't see them. The
session's first line says "remote cache: tun's, ..." when it is on. To build without it for
one command, pass --remote_cache= (empty) like any Bazel flag.

A session is (this client, this workspace, NAME): a persistent copy of the workspace on the
server at a fixed path (/work), Bazel's output base, and a Bazel server that stays up between
commands, so the second command skips Bazel's startup and reuses its analysis cache. Commands
in one session run one at a time (a second one queues behind the first; use --session to run
in parallel, at the cost of a second Bazel server). The server deletes a session after
30 minutes without a command (tun serve --session-idle) or to make room for a new one (LRU);
then the next command starts it again, slower. tun sessions lists them.

Sync: instead of uploading the workspace (the same files tun run ships: git tracked and
untracked-not-ignored; Bazel's bazel-* convenience symlinks at the root are left out), tun
sends a manifest (path, sha256, mode), uploads only the contents the server lacks, and the
server updates the session's copy in place: unchanged files keep their mtimes. Stderr reports
"tun: sync: ..." (bytes sent) and the server's "tun: session ...: synced workspace ..." line.

Bazel runs with HOME=/tun/home and TMPDIR=/tmp (private to the session), the server's PATH,
and on servers with network isolation the HTTP(S) proxy variables (downloads work through
it, as in tun run). Build outputs stay in the session's output base on the server; tun get
can't fetch them yet. Test logs it can: test and coverage keep each test's logs with the
job, so tun get <id> //pkg:name_test writes bazel-testlogs/pkg/name_test/test.log (and
test.xml, and every attempt's) locally; a failed test run's summary line ends with that
command for the first failed test.

Results: for build, test, coverage and run, tun adds --build_event_json_file (in the
session) and, when Bazel exits, prints one summary line from the Build Event Protocol, e.g.
"tun: bazel test exit=3 after 12.4s: 2 of 40 tests failed: //a:b_test //c:d_test" (also the
job's "summary"). --json prints instead, on stdout, one JSON object: {job, state, command,
exit_code, exit_name, success, complete, duration_sec, targets [{label, success, outputs}],
tests [{label, status, attempts [{run, shard, attempt, status, duration_sec, logs}],
duration_sec, shards}], failed_actions [{label, message, logs}], target_count, test_count,
failed_test_count, failed_target_count, omitted {list: n}, error}; lists are capped at 20
(omitted counts the rest), paths are relative to the workspace (bazel-bin/..., bazel-out/...)
on the server. With --json, Bazel's own stdout goes to stderr.

Each command is a normal job (tun ls, tun status, tun logs, tun cancel) with "session" and
"session_id" fields. Stderr ends with "tun: job <id> <state> exit=<code> after <run>s
(<queue>s queued)". Canceling (or SIGINT/SIGTERM to tun bazel) interrupts Bazel like Ctrl-C.

flags:
`

func bazelCmd(args []string, e Env) int {
	fs := newFlags("bazel", bazelUsage, e)
	session := fs.String("session", "default", "run in session `NAME` (letters, digits, '.', '_', '-')")
	cold := fs.Bool("cold", false, "start the session afresh: discard its Bazel server, output base and workspace copy")
	timeout := fs.Duration("timeout", 30*time.Minute, "cancel the bazel command after this long (exit 124; max 168h)")
	jsonOut := fs.Bool("json", false, "print the result as one JSON object on stdout (Bazel's stdout goes to stderr)")
	// tun's flags end at the bazel command; anything else that looks like a flag before it
	// is a Bazel startup option, which tun doesn't pass on.
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") && args[i] != "--" {
		name, _, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch {
		case name == "h" || name == "help" || name == "cold" || name == "json":
		case name == "session" || name == "timeout":
			if !hasValue {
				i++
			}
		default:
			return errorf(e, "bazel: %q: put tun's flags (--session, --cold, --timeout, --json) first, then the bazel command; Bazel startup options are not supported (tun sets its own)", args[i])
		}
		i++
	}
	if err := fs.Parse(args[:min(i, len(args))]); err != nil {
		return flagErr(e, err)
	}
	bargs := args[min(i, len(args)):]
	if len(bargs) > 0 && bargs[0] == "--" {
		bargs = bargs[1:]
	}
	switch {
	case len(bargs) == 0:
		return errorf(e, "bazel: no bazel command; usage: tun bazel [--session NAME] [--cold] [--json] <bazel command> [args...]")
	case strings.HasPrefix(bargs[0], "-"):
		return errorf(e, "bazel: %q: put tun's flags (--session, --cold, --timeout, --json) first, then the bazel command; Bazel startup options are not supported (tun sets its own)", bargs[0])
	case *timeout <= 0 || *timeout > api.MaxTimeoutSec*time.Second:
		return errorf(e, "bazel: --timeout must be positive and at most %v", api.MaxTimeoutSec*time.Second)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newClient(e)
	start := time.Now()
	root, rel, manifest, err := bazelManifest(e)
	if err != nil {
		return errorf(e, "bazel: %v", err)
	}
	req := api.CreateJob{Cwd: rel, TimeoutSec: int((*timeout + time.Second - 1) / time.Second),
		Bazel: &api.BazelJob{Session: *session, Cold: *cold, Root: root, Manifest: manifest, Args: bargs}}
	j, st, err := c.createBazel(ctx, req, root)
	if err != nil {
		return errorf(e, "creating bazel job: %v", err)
	}
	fmt.Fprintf(e.Stderr, "tun: job %s (session %s)\n", j.ID, j.Session)
	fmt.Fprintf(e.Stderr, "tun: sync: %d manifest entries (%dB sent), uploaded %d of %d files (%dB sent) in %.3fs\n",
		len(manifest), st.manifestBytes, st.uploaded, st.files, st.uploadBytes, time.Since(start).Seconds())
	we := e
	if *jsonOut { // both streams to stderr, written by two goroutines
		w := &lockedWriter{w: e.Stderr}
		we.Stdout, we.Stderr = w, w
	}
	code, final := wait(ctx, cancel, c, j, false, -1, we)
	if final != nil {
		printCompletion(e.Stderr, *final, code)
	}
	if *jsonOut {
		printBazelJSON(ctx, c, j.ID, final, e)
	}
	return code
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// printBazelJSON prints job id's summary object (GET /v1/jobs/{id}/bep) with the job's id,
// state and error added; without one (the command never ran), just those.
func printBazelJSON(ctx context.Context, c *Client, id string, final *api.Job, e Env) {
	out := map[string]any{}
	if err := c.doJSON(ctx, "GET", "/v1/jobs/"+id+"/bep", nil, &out); err != nil {
		out = map[string]any{}
	}
	out["job"] = id
	if final != nil {
		out["state"] = final.State
		if final.Error != "" {
			out["error"] = final.Error
		}
		if _, ok := out["exit_code"]; !ok {
			out["exit_code"] = final.ExitCode
		}
	}
	printJSON(e, out)
}

// bazelManifest lists the workspace containing the cwd and builds its manifest. It returns
// the workspace root and the cwd relative to it.
func bazelManifest(e Env) (root, rel string, m []api.ManifestEntry, err error) {
	real, err := filepath.EvalSymlinks(e.Cwd)
	if err != nil {
		return "", "", nil, err
	}
	if root, err = workspace.Root(real); err != nil {
		return "", "", nil, err
	}
	if rel, err = filepath.Rel(root, real); err != nil {
		return "", "", nil, err
	}
	files, err := workspace.Files(root)
	if err != nil {
		return "", "", nil, fmt.Errorf("listing workspace files: %v", err)
	}
	kept := files[:0]
	for _, f := range files { // Bazel's convenience symlinks, if not gitignored
		if !strings.Contains(f, "/") && strings.HasPrefix(f, "bazel-") && isSymlink(filepath.Join(root, f)) {
			continue
		}
		kept = append(kept, f)
	}
	var cache *workspace.HashCache
	if dir := e.Getenv("XDG_CACHE_HOME"); dir != "" {
		cache = workspace.LoadHashCache(filepath.Join(dir, "tun"), root)
	} else if home := e.Getenv("HOME"); home != "" {
		cache = workspace.LoadHashCache(filepath.Join(home, ".cache", "tun"), root)
	}
	if m, err = workspace.BuildManifest(root, kept, cache); err != nil {
		return "", "", nil, fmt.Errorf("hashing workspace %s: %v", root, err)
	}
	if cache != nil {
		cache.Save() // only a cache: errors don't matter
	}
	return root, filepath.ToSlash(rel), m, nil
}

type syncStats struct {
	manifestBytes, uploadBytes int64
	files, uploaded            int
}

// createBazel creates a bazel job: it sends the manifest (gzip-compressed) and, if the server
// lacks some file contents, uploads them and tries again.
func (c *Client) createBazel(ctx context.Context, req api.CreateJob, root string) (api.Job, syncStats, error) {
	var st syncStats
	paths := map[string]string{} // sha -> a path with that content
	for _, m := range req.Bazel.Manifest {
		if m.Type == api.EntryFile {
			paths[m.Sha] = m.Path
		}
	}
	st.files = len(paths)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(req); err != nil {
		return api.Job{}, st, err
	}
	zw.Close()
	for attempt := 0; ; attempt++ {
		st.manifestBytes += int64(buf.Len())
		resp, err := c.requestH(ctx, "POST", "/v1/jobs", bytes.NewReader(buf.Bytes()), int64(buf.Len()), map[string]string{"Content-Encoding": "gzip", "Content-Type": "application/json"})
		var se *StatusError
		if errors.As(err, &se) && se.Code == 409 && se.missing != nil && attempt < 3 {
			up := map[string]string{}
			for _, sha := range se.missing {
				if p, ok := paths[sha]; ok {
					up[sha] = p
				}
			}
			if len(up) != len(se.missing) {
				return api.Job{}, st, fmt.Errorf("server asked for contents not in the manifest")
			}
			n, err := c.putBlobs(ctx, root, up)
			st.uploadBytes += n
			st.uploaded += len(up)
			if err != nil {
				return api.Job{}, st, fmt.Errorf("uploading %d files: %v", len(up), err)
			}
			continue
		}
		if err != nil {
			return api.Job{}, st, err
		}
		defer resp.Body.Close()
		var j api.Job
		if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
			return api.Job{}, st, fmt.Errorf("bad response from server: %v", err)
		}
		return j, st, nil
	}
}

// putBlobs uploads file contents (sha -> path relative to root) in batches of at most
// 64MB, each a gzip-compressed tar; it returns the bytes sent.
func (c *Client) putBlobs(ctx context.Context, root string, files map[string]string) (int64, error) {
	var sent int64
	batch, size := map[string]string{}, int64(0)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if err := workspace.WriteBlobs(zw, root, batch); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		sent += int64(buf.Len())
		resp, err := c.requestH(ctx, "POST", "/v1/cas", &buf, int64(buf.Len()), map[string]string{"Content-Encoding": "gzip", "Content-Type": "application/x-tar"})
		if err != nil {
			return err
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		batch, size = map[string]string{}, 0
		return nil
	}
	for sha, p := range files {
		fi, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return sent, err
		}
		if size > 0 && size+fi.Size() > 64<<20 {
			if err := flush(); err != nil {
				return sent, err
			}
		}
		batch[sha] = p
		size += fi.Size()
	}
	return sent, flush()
}

const sessionsUsage = `usage: tun sessions [--client NAME | --mine]

Print the server's tun bazel sessions, most recently used first, one JSON object per line:
{id, name, client, root (the client's workspace path), created_at, last_used, commands,
last_job, state (idle|busy|stopping), job (the running job, if busy), bazel_server (whether
its Bazel server is up), expires_at (deleted then unless used again)}.

flags:
`

func sessionsCmd(args []string, e Env) int {
	fs := newFlags("sessions", sessionsUsage, e)
	client := fs.String("client", "", "only sessions of client `NAME`")
	mine := fs.Bool("mine", false, "only this host's sessions")
	pos, err := parse(fs, args)
	if err != nil {
		return flagErr(e, err)
	}
	if len(pos) > 0 || (*mine && *client != "") {
		return errorf(e, "usage: tun sessions [--client NAME | --mine]")
	}
	q := url.Values{}
	if *client != "" {
		q.Set("client", *client)
	}
	if *mine {
		q.Set("mine", "1")
	}
	var list []api.Session
	if err := newClient(e).doJSON(context.Background(), "GET", "/v1/sessions?"+q.Encode(), nil, &list); err != nil {
		return errorf(e, "%v", err)
	}
	for _, s := range list {
		printJSON(e, s)
	}
	return 0
}

func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}
