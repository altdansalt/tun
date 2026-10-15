# Working on tun

Read SPEC.md first (what exists), then docs/bazel.md (where it's going). tun is a Go (stdlib-only) remote execution service for coding agents.

## Building and testing
- **Default CI: `tun bazel test //...`** (a shared tun server is at $TUN_URL, or the default local
  address; the `tun` binary is on PATH, built from main). It syncs your working tree to a warm
  Bazel session on the server (fixed `/work`, Bazel server kept between commands, remote cache)
  and runs every target: unit and integration tests and the CLI smoke suites
  (`//scripts:e2e_test`, `//scripts:e2e_runners_test`) against the Bazel-built `//cmd/tun`.
  Only what your change affects rebuilds and reruns; unchanged tests show `(cached) PASSED`.
  - Ends with one summary line: `tun: bazel test exit=3 after 61.2s: 1 of 14 tests failed:
    //internal/client:run_test; logs: tun get <id> //internal/client:run_test`. Failing
    tests' logs are printed already (`--test_output=errors`, .bazelrc); `tun get <id> //label`
    writes that test's full logs to `bazel-testlogs/<pkg>/<name>/` (`-o DIR` elsewhere).
  - Subsets: `tun bazel test //internal/server:server_test --test_filter=TestEphemeral`
    (`--test_filter` takes a Go -run regexp: `--test_filter='TestA|TestB'`). Rerun a cached
    test with `--nocache_test_results`; more output: `--test_output=all`.
  - Race detector: not in the default (it roughly doubles test time and has no cache entries
    until a race build ran; docs/bazel.md). For changes to concurrent code also run
    `tun bazel --session race test --config=race //...` (its own session, so the default one
    keeps its analysis cache) or the fallback `tun run -- ./scripts/ci.sh` (always `-race`).
  - The first command of a session (or `tun bazel --cold ...`) starts a Bazel server and
    re-analyzes: ~1-4 min; later commands reuse it. Sessions expire after 30 min idle and the
    server holds only a few (`tun sessions`); an evicted one just starts cold again.
  - After adding/removing Go files or imports: update the BUILD files with gazelle and commit
    them (`tun run --out internal --out cmd -c 'bazel --output_user_root=$TMPDIR/b run //:gazelle'`).
    A Go file missing from BUILD.bazel is silently not built/tested; `tun run -- ./scripts/bazel-ci.sh`
    checks that (`-e GAZELLE=1`: gazelle's full diff). Test data/tags/size are kept by gazelle.
  - internal/client's tests are split by area into separate targets (`run_test`, `sandbox_test`,
    `service_test`, `runner_test`, `bazel_test`, `bazel_real_test`, `review_test`; shared helpers
    in `lib_*_test.go`, the `:testlib` library), so Bazel caches and schedules them separately.
    Gazelle leaves them alone there: add a new `_test.go` to one of them by hand.
  - Tests that run real Bazel nested in a test (TestBazelReal, reapi's TestBazelCache, e2e.sh's
    `tun bazel` step) use the pinned `@bazel_binary` (MODULE.bazel) and reach the Bazel Central
    Registry through tun's egress proxy (`--test_env` in .bazelrc; tagged `requires-network`).
- **Fallback: `tun run -- ./scripts/ci.sh`** (when `tun bazel` is down, you suspect the session
  machinery, or you want Go's newer host toolchain): vet, build, `go test -race -timeout=10m` of
  everything in a fresh sandboxed copy of your tree, plus the smoke suites. It reruns everything
  every time (~2-4 min).
  - Subsets: `tun run -e TEST_FLAGS='-run TestX -v' -- ./scripts/ci.sh` (env is NOT forwarded unless `-e`).
    ci.sh splits TEST_FLAGS like xargs (quotes group, nothing is evaluated), so alternations work as
    `tun run -e TEST_FLAGS='-run "TestA|TestB" -v' -- ./scripts/ci.sh` (or `-e TEST_FLAGS="-run 'A|B'"`).
    With `-run`, ci.sh skips the separate vet/build (go test compiles what it tests; add
    `-e CI_FULL=1` to keep them) and exits 1 if the pattern matched no test in any package.
    Packages: `-e PKGS='./internal/reapi ./internal/client'` (default `./...`).
    `tun run` says where the job runs: `tun: running on hub` or `tun: running on runner NAME`.
  - Long output: `tun run --tail 80 -- ./scripts/ci.sh` prints only the last 80 lines of stdout and
    stderr once the job ends (same exit code), with a progress heartbeat every 30s;
    `tun logs --tail N [--stderr] <id>` for more.
  - CI runs the real CLI smoke suites once at the end by default (scripts/e2e.sh, and
    scripts/e2e-runners.sh: a hub without local slots plus two `tun runner` processes);
    `-e E2E=0` disables them (with `-run` they are skipped unless `-e E2E=1`).
  - `tun run -- ./scripts/bazel-ci.sh [bazel test args]`: the same Bazel build in a plain job
    (fresh output base under `$TMPDIR`, never `$XDG_CACHE_HOME/bazel`; per-host repository and
    disk cache in `$XDG_CACHE_HOME/tun-bazel/`, writable by every job: see its trust note,
    `BAZEL_DISK_CACHE=0` skips it). Slower than a warm session (~4-5 min), but independent of it.
- **Test the changed binary with a private server:** `tun run -c 'scripts/dev-tun.sh ./scripts/e2e.sh'`.
  The wrapper builds the working tree, adds the new binary to PATH, exports TUN_URL/TUN_TOKEN,
  runs the command, and cleans up the server and temporary files. Never replace the shared server.
  Use any command, e.g. `scripts/dev-tun.sh tun run -- echo hi`, or your branch's `tun bazel`:
  `tun run -c 'scripts/dev-tun.sh tun bazel build //...'` (dev-tun.sh seeds bazelisk from the
  job's cache, or from `DEV_TUN_BAZEL`, a Bazel binary). `tun bazel` prints its result as one
  object with `--json`. Bazel caches are per client on the server; sessions use the server's
  REAPI remote cache automatically (`tun serve --cache-dir`, on by default) through a
  per-session socket; action results are per client.
- The hosts have 2 CPUs; the hub runs 2 jobs (and sessions) at once; others queue. That's expected.
- Fallback if tun is down or you suspect tun itself: `scripts/bb-ci.sh` (same CI on a BuildBuddy runner).
- If tun gets in your way (missing feature, confusing error, slow), note it in your final report under
  "tun friction" — that's the most valuable feedback for what to build next.
- Run long commands in the foreground and let the tool call wait; don't background + poll.
- Don't run `go test` directly on the host; `go build`/`go vet`/`gofmt` are fine.

- Keep every Bazel output root of a `tun run` job under TMPDIR, never the shared XDG cache. `scripts/bench-reapi.sh`
  measures 200 actions; BuildBuddy measurements run on the VM host because jobs lack DNS.

## Conventions
- Keep it small and readable. Stdlib only. `gofmt` everything.
- Tests are required for behavior you add. Prefer integration tests through the real HTTP server.
- Commit with clear messages. Don't rewrite others' history.

## Deploy
See [docs/deploy.md](docs/deploy.md): atomic binary replacement, client retries,
SIGTERM draining, and required systemd KillMode/TimeoutStopSec overrides.

- Client identity: use `tun token [--data DIR] new NAME` on the server host, then install the
  secret as `~/.config/tun/token` on its agent VM. `$DATA/token` remains admin and provides
  no client isolation. Never put it on new agent VMs. See docs/deploy.md for migration.
- Tool caches: `--max-shared-cache` (5GB, Go/XDG caches) and `--max-client-cache` (1GB per
  client's Bazel repository cache) are soft budgets; trimming evicts whole trees (Go modules,
  tool dirs, repository cache entries), never while a job/session may use them (sharedcache.go).
- Uploaded content (workspace blobs, tun bazel CAS) is per uploader; other clients' argv,
  script and workspace are hidden in job views. Remaining isolation gaps: SPEC.md, Known gaps.
