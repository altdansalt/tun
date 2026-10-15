# Test speed measurements (2026-10-15)

Measured through the remote shared two-CPU tun server. Baseline is origin/main
54f8fa4, using a detached worktree for the last two baseline runs. All measurements
run the complete CI: vet, build, uncached race tests, and the real CLI smoke suite.

```sh
tun run --json -e TEST_FLAGS='-count=1 -v -json' -- ./scripts/ci.sh
tun logs <id>  # retrieve full go test JSON; --json retains only the last 64KiB
```

Full CI seconds below are the server's `run_sec` (excluding queue/upload time).
Client seconds are the package's final `go test -json` Elapsed value. The server
is shared, so these are observed timings, not a controlled CPU benchmark. Baseline
runs overlapped development jobs; the three final measurements ran sequentially
without other jobs from this task.

| Version | Job | Full CI (s) | Client (s) | Queue (s) |
| --- | --- | ---: | ---: | ---: |
| Before 1 | 20261015T052806967465-8f806805 | 54.325 | 50.137 | 0.007 |
| Before 2 | 20261015T052926147895-8a5e5876 | 62.908 | 55.711 | 14.222 |
| Before 3 | 20261015T053043415844-f5e156b9 | 62.352 | 54.547 | 0.014 |
| After 1 | 20261015T053234219651-34734544 | 23.783 | 19.517 | 0.007 |
| After 2 | 20261015T053258154309-cc1e43c2 | 23.413 | 19.192 | 0.009 |
| After 3 | 20261015T053321719779-8032b233 | 23.961 | 19.673 | 0.010 |
| Before median | | **62.352** | **54.547** | |
| After median | | **23.783** | **19.517** | |

Median improvement: 61.9% full CI, 64.2% client package. Compared with the fastest
baseline, the median full CI is still 56.2% faster. All six measured runs passed.
Each final run used `-count=1`, with no observed flakes, and retained all 81 original
client pass events (including subtests), plus two new integration tests.

## Profile and changes

| Test | First baseline (s) | Final run (s) |
| --- | ---: | ---: |
| TestReviewServiceCancelRace | 8.00 | 3.10 |
| TestBwrapGoBuild | 6.39 | 6.53 |
| TestCancelStartRace | 5.02 | 1.51 |
| TestRetentionBlobs | 3.09 | 3.10 |
| TestTimeout | 3.01 | 1.08 |
| TestQueueVisibility | 2.82 | 2.82 |
| TestBwrapEnvNotOnHostProcess | 2.04 | 0.03 |

Independent client tests now use t.Parallel; each owns its server, temporary data,
HTTP listener, CLI environment and output buffers. Go's default test parallelism
still bounds execution by GOMAXPROCS (two here). The helper subprocess test stays
synchronous. Race-loop iteration counts and sandboxed compilation coverage remain
unchanged.

Fixture polling is 5ms instead of 50ms; the existing server.Config.KillGrace is
50ms instead of two seconds in tests, preserving the ten-second production default.
Restart recovery waits for log output; the sandbox environment test waits for a
release file; the streaming proxy test holds its second chunk until an explicit
release request. These replace startup/inspection/streaming sleeps with events.
Retention, queue timing, delayed readiness and race-regression observation windows
remain where elapsed time is the behavior under test. A 200ms timing assertion
now allows 50ms tolerance: an intermediate run observed 198ms because StartedAt
is recorded after process startup.

Non-JSON CLI runs report compressed workspace bytes, whether uploaded or cached,
and finish with job state, CLI exit code, run seconds and queue seconds. The final
line follows --out downloads too, and starts on its own line after unterminated
stderr. Integration tests cover success, nonzero exit, timeout, failed startup,
--tail, --out, exact uploaded/cached blob sizes, JSON silence and --no-workspace.
SPEC.md and CLI help describe the new diagnostics.

## tun friction

- Queue time varied from 0.007s to 14.222s; execution varied from 54.325s to
  62.908s on unchanged baseline code. Separating queue and run time matters on
  the shared server.
- Profiling output exceeds --json's 64KiB stdout cap. `tun logs <id>` was needed
  to recover complete test events; the truncation flag makes this detectable.
- The PATH binary remains main, so outer runs do not yet show the new timing/size
  lines. The changed binary was exercised through dev-tun.sh and every CI smoke suite.
- Remote demo job 20261015T053357440377-f84bad02 built the changed binary and ran
  two nested commands on a private server: 81,674B uploaded, then 81,674B already
  on server; runs 0.003s/0.002s, queues 0.003s/0.005s. The outer job took 0.829s
  with 0.011s queued. CI smoke tests also exposed 29B empty workspaces and a
  roughly 5,001,567B workspace containing the service helper binary.
