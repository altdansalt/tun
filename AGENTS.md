# Working on tun

Read SPEC.md first. tun is a Go (stdlib-only) remote execution service for coding agents.

## Building and testing
- **Build and test with tun itself** (a tun server runs on this host at the default address; the
  `tun` binary is on PATH, built from main):
  - `tun run -- ./scripts/ci.sh` — vet, build, `go test -race`, in a sandboxed copy of your working tree.
  - Subsets: `tun run -e TEST_FLAGS='-run TestX -v' -- ./scripts/ci.sh` (env is NOT forwarded unless `-e`).
  - To test your *changed* tun binary end-to-end, start it inside the job (tests do this already:
    integration tests run an in-process server), or run `go run ./cmd/tun serve --addr 127.0.0.1:<random>
    --data $(mktemp -d)` inside a `tun run -c` script. Never restart or replace the shared server.
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
