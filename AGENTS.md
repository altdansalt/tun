# Working on tun

Read SPEC.md first. tun is a Go (stdlib-only) remote execution service for coding agents.

## Building and testing
- **Use remote execution, not local builds.** Run the CI on a remote runner:
  - `scripts/bb-ci.sh` — runs `scripts/ci.sh` (vet, build, `go test -race`) on a BuildBuddy runner via
    `bb remote`, mirroring your working tree (uncommitted changes included). ~30s+.
  - `TEST_FLAGS='-run TestX -v' scripts/bb-ci.sh` for a subset.
  - Once a tun server is running (see below), prefer `tun run -- ./scripts/ci.sh`: same thing on tun.
- Run long commands in the foreground and let the tool call wait; don't background + poll.
- Don't run `go test` directly on the host except trivially (`go vet`, `gofmt`); the host is small.

## Conventions
- Keep it small and readable. Stdlib only. `gofmt` everything.
- Tests are required for behavior you add. Prefer integration tests through the real HTTP server.
- Commit with clear messages. Don't rewrite others' history.
