# Working on tun

Read SPEC.md first (what exists), then docs/bazel.md (where it's going). tun is a Go (stdlib-only) remote execution service for coding agents.

## Building and testing
- **Build and test with tun itself** (a shared tun server is at $TUN_URL, or the default local address; the
  `tun` binary is on PATH, built from main):
  - `tun run -- ./scripts/ci.sh` — vet, build, `go test -race -timeout=10m`, in a sandboxed copy of your working tree.
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
  - Test the changed binary with a private server: `tun run -c 'scripts/dev-tun.sh ./scripts/e2e.sh'`.
    The wrapper builds the working tree, adds the new binary to PATH, exports TUN_URL/TUN_TOKEN,
    runs the command, and cleans up the server and temporary files. Never replace the shared server.
    Use any command, e.g. `scripts/dev-tun.sh tun run -- echo hi`.
  - CI runs the real CLI smoke suites once at the end by default (scripts/e2e.sh, and
    scripts/e2e-runners.sh: a hub without local slots plus two `tun runner` processes);
    `-e E2E=0` disables them (with `-run` they are skipped unless `-e E2E=1`).
  - `tun bazel` (warm Bazel sessions) needs a server built from a branch that has it: try yours with
    `tun run -c 'scripts/dev-tun.sh tun bazel build //...'`. Its tests use a fake bazel; TestBazelReal
    and scripts/e2e.sh also run the real one when `bazel` is on PATH (it is on the tun hosts).
    It ends with a BEP summary line; `tun bazel --json ...` prints the result as one object.
    Bazel caches are per client on the server; dev-tun.sh seeds bazelisk from the job's cache.
    Sessions run on the hub or runners with host affinity; --max-sessions is per host.
    Sessions use the server's REAPI remote cache automatically (`tun serve --cache-dir`, on by
    default) through a per-session socket; action results are per client.
  - Hosts have 2 CPUs; the shared hub has six runners (currently one slot each). Jobs may
    queue; use the reported runner and job ID when comparing timings.
- **Bazel** (M0 of docs/bazel.md; `scripts/ci.sh` stays the default CI for now):
  - `tun run -- ./scripts/bazel-ci.sh` — checks that every Go file is in a BUILD.bazel
    (`-e GAZELLE=1`: gazelle's full diff, ~15s more), then `bazel test //...` (unit + integration
    tests, and the e2e suites as `//scripts:e2e_test`, `//scripts:e2e_runners_test` against the
    Bazel-built `//cmd/tun`). Arguments replace `//...` and go to `bazel test` as is:
    `tun run -- ./scripts/bazel-ci.sh --test_filter=TestEphemeral //internal/server:server_test`.
    `--config=race` adds the race detector (ci.sh always uses it; Bazel doesn't by default).
  - After adding/removing Go files or imports: `bazel run //:gazelle` (in a tun job, with
    `--out` for the BUILD files; e.g. `tun run --out internal -c 'bazel --output_user_root=$TMPDIR/b run //:gazelle'`)
    and commit the BUILD.bazel changes. Test data/tags/size in BUILD files are kept by gazelle.
  - Caches: output base in the job's `$TMPDIR` (deleted at exit; never let Bazel default to
    `$XDG_CACHE_HOME/bazel`), shared repository cache and disk cache in
    `$XDG_CACHE_HOME/tun-bazel/` on each tun host. The disk cache also caches test results:
    an unchanged test shows `(cached) PASSED`; `--nocache_test_results` reruns it. Every job on a
    host can write that cache (see the trust note in scripts/bazel-ci.sh; `BAZEL_DISK_CACHE=0` skips it).
- Fallback if tun is down or you suspect tun itself: `scripts/bb-ci.sh` (same CI on a BuildBuddy runner).
- If tun gets in your way (missing feature, confusing error, slow), note it in your final report under
  "tun friction" — that's the most valuable feedback for what to build next.
- Run long commands in the foreground and let the tool call wait; don't background + poll.
- Don't run `go test` directly on the host; `go build`/`go vet`/`gofmt` are fine.

- REAPI integration tests run real Bazel when installed (skip otherwise). Keep every
  Bazel output root under TMPDIR, never the shared XDG cache. `scripts/bench-reapi.sh`
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
