# Working on tun

Read SPEC.md first (what exists), then docs/bazel.md (where it's going). tun is a Go (stdlib-only) remote execution service for coding agents.

## Building and testing
- **Build and test with tun itself** (a shared tun server is at $TUN_URL, or the default local address; the
  `tun` binary is on PATH, built from main):
  - `tun run -- ./scripts/ci.sh` — vet, build, `go test -race`, in a sandboxed copy of your working tree.
  - Subsets: `tun run -e TEST_FLAGS='-run TestX -v' -- ./scripts/ci.sh` (env is NOT forwarded unless `-e`).
    ci.sh splits TEST_FLAGS like xargs (quotes group, nothing is evaluated), so alternations work as
    `tun run -e TEST_FLAGS='-run "TestA|TestB" -v' -- ./scripts/ci.sh` (or `-e TEST_FLAGS="-run 'A|B'"`).
    With `-run`, ci.sh skips the separate vet/build (go test compiles what it tests; add
    `-e CI_FULL=1` to keep them) and exits 1 if the pattern matched no test in any package.
  - Long output: `tun run --tail 80 -- ./scripts/ci.sh` prints only the last 80 lines of stdout and
    stderr once the job ends (same exit code); `tun logs --tail N [--stderr] <id>` for more.
  - Test the changed binary with a private server: `tun run -c 'scripts/dev-tun.sh ./scripts/e2e.sh'`.
    The wrapper builds the working tree, adds the new binary to PATH, exports TUN_URL/TUN_TOKEN,
    runs the command, and cleans up the server and temporary files. Never replace the shared server.
    Use any command, e.g. `scripts/dev-tun.sh tun run -- echo hi`.
  - CI runs the real CLI smoke suites once at the end by default (scripts/e2e.sh, and
    scripts/e2e-runners.sh: a hub without local slots plus two `tun runner` processes);
    `-e E2E=0` disables them (with `-run` they are skipped unless `-e E2E=1`).
  - The host has 2 CPUs and the server runs 2 jobs at once; others queue. That's expected.
- Fallback if tun is down or you suspect tun itself: `scripts/bb-ci.sh` (same CI on a BuildBuddy runner).
- If tun gets in your way (missing feature, confusing error, slow), note it in your final report under
  "tun friction" — that's the most valuable feedback for what to build next.
- Run long commands in the foreground and let the tool call wait; don't background + poll.
- Don't run `go test` directly on the host; `go build`/`go vet`/`gofmt` are fine.

## Conventions
- Keep it small and readable. Stdlib only. `gofmt` everything.
- Tests are required for behavior you add. Prefer integration tests through the real HTTP server.
- Commit with clear messages. Don't rewrite others' history.
