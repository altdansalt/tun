# tun v1 direction: Bazel-native remote execution

Status: direction agreed 2026-10-15. The core of M1 exists (branch feat/bazel-sessions:
`tun bazel`, sessions, manifest sync on the hub; see "M1 status" below). SPEC.md stays the
contract for what exists; this file is the target and the order we get there.
The standalone M1 BEP results package is implemented (see results below).

## Goal
An agent sends a Bazel command; tun makes it fast. Caching, warm servers and remote execution
happen without the agent asking for them.

```
tun bazel test //internal/...      # that's the whole interface
tun bazel build //cmd/tun && tun bazel run //cmd/tun -- help
tun bazel query 'rdeps(//..., //internal/store)'
```

Customers are agents on thin VMs (2 CPU / 4 GB). The only metric that matters is wall-clock time
from "agent edited a file" to "agent has the result it needs", measured against `bb remote` on
the same repo.

End state is **remote execution** (actions spread over all runners) behind a **warm Bazel
server**. Warm Bazel alone is the first stepping stone.

## Decisions
- **Self-contained.** tun owns its cache and executor. BuildBuddy's remote cache (`~/bb-token`) is
  allowed only to bootstrap (M1), behind one server flag, and is removed once tun's cache exists.
  GitHub stays bootstrap-only too: workspaces come from the client, never from a git remote.
- **Bazel is the constraint.** Projects built by `tun bazel` use hermetic toolchains (rules_go's
  SDK download, hermetic C/C++/Node/Rust rules). tun hosts provide only bazelisk, bwrap and a
  JVM-friendly amount of memory. Anything that isn't hermetic is that project's bug, not tun's.
- **No flag mirroring.** `tun bazel <cmd> <bazel args>` passes the agent's args through unchanged
  and adds its own (caches, executor, BEP, output base) itself. Agents never type
  `--remote_cache`. tun's own flags are few and go before the bazel command
  (`tun bazel [--session NAME] [--cold] test ...`).
- **`tun run` stays** for non-Bazel work and scripts; it is not the focus.

## Model

**Session** = one warm Bazel server for one (client, repo, session name) on one host.
- Fixed workspace path inside the sandbox (`/work`), so the output base, Bazel's in-memory graph and
  every path-sensitive tool cache are reused. This also fixes v0's "job paths vary" problem.
- Persistent output base and Bazel server across commands; a session expires after idle time
  (default 30 min) or under memory pressure (LRU). A Bazel server is ~1-2 GB, so a 4 GB runner
  holds 1-2 sessions; that is why remote execution, not more sessions, is the scaling path.
- Commands in a session run one at a time (Bazel's own lock). A second concurrent command from the
  same client goes to a new session (cold) rather than queueing behind a long build, unless
  `--session` names the busy one.
- **Sync, not upload:** the client sends the manifest of its tracked files (path → digest, mode);
  the server answers with the digests it lacks; the client sends only those; the server applies
  the diff in place (writes, deletes). A one-line edit costs one small blob, not a tarball.
- **Placement:** a session lives on one host; commands route there. If that host is gone or full,
  a new session starts elsewhere and is "cold" only in-memory: the shared caches make it fast.

**Caches** (outermost is the fallback):
1. The warm Bazel server (analysis cache, in memory).
2. Per-host `--repository_cache` (downloads) and `--disk_cache`.
3. tun's remote cache on the hub: CAS + action cache. Bootstrap: BuildBuddy's.

**How jobs reach the cache/executor: a unix socket, not the network.** Verified 2026-10-15: Bazel's
gRPC client ignores HTTPS_PROXY, and jobs have no DNS, so `--remote_cache=grpcs://...` fails in a
job. `--remote_proxy=unix:PATH` works: Bazel speaks grpcs (TLS, authority from the URL) over the
socket; a host-side forwarder to remote.buildbuddy.dev:443 gave remote cache hits. So tun binds a
socket into the sandbox (e.g. `/tun/cache.sock`) and adds `--remote_proxy=unix:...` itself. The
bootstrap forwarder is a fixed-destination TCP passthrough (the BuildBuddy key is then a job flag);
tun's own cache/executor later serves h2c on that socket with auth on the host side, so jobs never
need a network or a token for it.

Only executors (tun itself) write the action cache once remote execution exists; sessions
upload no local results (`--noremote_upload_local_results`), so a job can't poison the cache.

**Remote execution** (M3): the hub implements the Remote Execution API (Capabilities, CAS,
ByteStream, ActionCache, Execution); runners become executors: fetch inputs from CAS into a
per-action exec root, run in tun's existing bwrap sandbox (network off by default), upload outputs.
The warm session then does analysis and scheduling only; actions fan out over every runner.
Results stay in CAS ("build without the bytes"); `tun get` fetches outputs by label on demand.

**Results to the agent:** stream Bazel's own stderr (agents already read it), then one summary line
from the Build Event Protocol, like v0's: `tun: bazel test exit=3 after 12.4s: 2 of 40 tests failed:
//a:b_test //c:d_test; logs: tun get <id> //a:b_test.log`. Exit status is Bazel's.
`--json` gives the BEP-derived summary (targets, test results, output paths) as one object.

**`bazel run`:** tun builds in the session, then starts the binary (with runfiles) as a v0 job or
`--service`, outside the Bazel server, so the session's lock is released and the process may be
long-lived. `tun bazel run --service --port 8080 //svc` reuses v0's port proxy.

## Milestones (each ends with a measurement, not an agent's report)
- **M0 — tun builds with Bazel.** MODULE.bazel with rules_go + gazelle, hermetic Go SDK,
  `bazel test //...` green inside `tun run` on a runner (tests that need bwrap/network get tags).
  CI keeps `scripts/ci.sh` until `tun bazel` replaces it. Measure: cold and no-op `bazel test //...`.
- **M1 — warm sessions, one host.** `tun bazel` with sessions, manifest sync, BEP summary, shared
  repository/disk caches, optional BuildBuddy remote cache. Measure on tun and one outside repo:
  no-op, one-line change, cold session; vs `bb remote` and vs the same Bazel run locally.
- **M2 — tun's cache, many hosts.** Hub serves the remote cache (REAPI gRPC over h2c, stdlib-only), BuildBuddy flag removed, sessions placed across runners with affinity.
  Measure: cold session on a fresh runner with a warm cache.
- **M3 — remote execution.** REAPI on the hub, runners as executors, build without the bytes.
  Measure: clean build and full `test //...` time vs number of runners (1, 2, 4); several agents
  at once.
- **M4 — long-lived and services.** `bazel run` as jobs/services; test sharding comes from M3.

Targets to beat, set at M1 from real numbers: warm one-line-change `test` faster than `bb remote`;
no-op within 1 s of local Bazel's no-op; overhead of sync + routing under 300 ms.

## M1 status (feat/bazel-sessions)
Built: `tun bazel [--session NAME] [--cold] <cmd> [args...]`, `tun sessions`, sessions on the
hub's local slots (fixed `/work`, `/tun/home`, `/tmp`, output base `/tun/out/base`), a
long-lived session sandbox whose agent runs each command (so the Bazel server survives between
commands), manifest sync with in-place apply, idle expiry (`--session-idle`, 30m) and LRU
eviction (`--max-sessions`, 2), shared repository cache, per-client disk cache. Contract in
SPEC.md ("Bazel sessions").

Deviations from the model above, and why:
- **A busy session queues** the next command of the same session instead of starting a new
  cold one. A second Bazel server costs 1GB+ on a 4GB host and a cold analysis; `--session
  NAME` gets the parallel one explicitly. Waiting jobs show `queue_reason`.
- **Disk cache per client**, not per session: `--cold` and a second `--session` of the same
  client reuse action results, and one client still can't poison another's cache. Size-capped
  by Bazel (`--experimental_disk_cache_gc_max_size=5G`); the per-client dirs are not deleted
  by tun yet.
- **Bazel startup options from the agent are rejected** (tun's own must stay constant per
  session or Bazel restarts its server).
- The client identifies its workspace by its absolute root path; two checkouts of one repo are
  two sessions (they share the client's disk cache).

Not done yet in M1: the BEP summary line and `--json` (feat/bazel-bep is on it); the optional
BuildBuddy remote cache; sessions on runners and placement; `tun get` of outputs; `bazel run`
as a separate job; measuring tun's own `bazel test //...` (needs M0's BUILD files) and an
outside repo, and against `bb remote`.

Measured (2026-10-15, private dev server on a tun host via `tun run -c 'scripts/dev-tun.sh
...'`, client on the same host, so no network round trips; tun job 20261015T081338167547-8eeb4d65):

| genrule workspace (4 files)      | wall  | Bazel elapsed | sent (manifest + contents) | job |
|----------------------------------|-------|---------------|---------------------------|-----|
| first command, new session       | 6.60s | 6.44s, "Starting local Bazel server" | 840B + 409B | 20261015T081339359149-6bcd15ac |
| second (no-op)                   | 0.37s | 0.33s, 0 packages loaded | 420B + 0B | 20261015T081345972324-a7ea86da |
| third (no-op)                    | 0.11s | 0.09s | 420B + 0B | 20261015T081346347993-a343996c |
| one-line change of an input      | 0.17s | 0.12s | 838B + 140B | 20261015T081346468837-bed213e2 |
| `--cold`                         | 6.91s | 6.34s, new server | 427B + 0B | 20261015T081346640293-da0e4076 |
| v0: `tun run -- bazel build` (every time) | 6.5-7.8s | new server, full analysis | tarball | |

Cost of a warm session on the trivial workspace: Bazel server RSS 223MB, 117MB on disk
(nearly all Bazel's install base, extracted per session; sharing it across sessions would save
that and ~1s per new session, but makes it writable by every session).

tun's own tree (78 entries, 66 files) with `bazel info`: first sync 6.9KB manifest + 176KB
contents; no-op 3.5KB; one-line change 6.9KB (the manifest is sent twice: once answered with
the missing digest) + 2.8KB; `tun run` uploads a 164KB tarball for every change. From a
remote client add ~4 round trips (create, two log streams, status; ~35ms each from an exe.dev
VM to the shared server) and 2 more for a change (409, upload).

## Open questions / risks
- **Stdlib only vs gRPC — decision revised 2026-10-15.** No HTTP cache detour and no
  third-party gRPC/protobuf dependencies. Go 1.25 `http.Server.Protocols` with
  `SetUnencryptedHTTP2` supplies h2c; HTTP trailers supply gRPC status. `internal/reapi`
  implements framing and the protobuf wire subset for cache services, citing the official
  remote-apis and ByteStream protos. Execution remains M3 work.
- **Nested sandboxing:** Bazel's linux-sandbox inside tun's bwrap needs nested user namespaces;
  verify in M0, else run Bazel with `processwrapper-sandbox` and rely on tun's boundary.
- **Memory:** 4 GB runners, JVM servers. Tune `--host_jvm_args`, cap sessions per host, evict LRU.
- **Trust:** shared per-host caches are writable by every job today (v0 limitation); in M1 the
  repository cache is content-checked by Bazel, and the disk cache is per-session until M2/M3 move
  shared results to the executor-written remote cache.
- **Bazel versions:** bazelisk + the repo's `.bazelversion`; installs cached per host.

## M1 results (implemented in internal/bep)

`bep.New(command)` creates a parser. Feed newly read bytes from
`--build_event_json_file` to `Write`; it retains partial lines between reads. Call
`Finish` only after Bazel exits (a truncated final event returns an error, while
`Snapshot` retains preceding results), then `SetResult(processExit, elapsed)` to
supply the authoritative process result. `Snapshot` is the full artifact index;
`Line` renders `tun: bazel test exit=3 after 12.4s: 2 of 40 tests failed: //a:b //c:d`.
`JSON` returns one object with exit code/name, success/completeness, duration,
targets, tests and failed actions. Lists are capped at 20, with full target/test
counts and an `omitted` map (including nested list omissions). The diagnostic caps
labels at 20 and reports how many were omitted. Unknown test statuses count as
failures; FLAKY is a successful final status, with individual failed attempts retained.
The full snapshot is intentionally uncapped for subsequent label lookups.

Test results include each run/shard/attempt (BEP IDs are 1-based), status, duration
and local log paths, including earlier retry logs. Failed action messages and
stdout/stderr paths are retained when available. Top-level default output groups
resolve transitive named file sets, mapping `bazel-out/<config>/bin/...` to
workspace-relative `bazel-bin/...`. Non-file URIs are currently excluded: CAS/remote
artifact fetching belongs to M3. Multiple configurations of the same label are
currently collapsed; M1's API indexes by label.

The sessions integration must keep the full snapshot with the command ID. To wire
`tun get <id> //pkg:test.log`, resolve the label to **all** attempt log paths,
validate those paths against the session output base/exec root, and copy or stream
the files while that command's results still exist. For output labels, resolve
`Target.Outputs` in that session's workspace; do not assume current v0 `tun get`
can follow Bazel's workspace symlinks outside its root. Preserve/stage artifacts
before another session command overwrites them, and declare staged paths when a
runner must ship them to the hub. Never accept an agent-supplied absolute path.
The short JSON view must not be used as the lookup index because of list caps.

Deviation from the proposed universal BEP flow: Bazel 9 `query` rejects
`--build_event_json_file`; omit that flag for query and use an empty parser plus
`SetResult`. Empty targets are valid. Parse failures and incomplete streams should
be reported by the caller without replacing Bazel's actual exit code.

Real Bazel 9.3.0 fixtures and their generation script live in
`internal/bep/testdata`. Generate with
`tun run --out internal/bep/testdata -- bash internal/bep/testdata/generate.sh`;
all Bazel output roots are under the job's TMPDIR. The flaky fixture uses a
job-local marker and standalone test execution so its second attempt reliably
passes; the other test fixtures exercise linux-sandbox. The compile-error fixture
uses a genrule invoking the host C compiler solely to provoke a real failed action
(it is not a recommended hermetic project toolchain).

## M2 cache implementation

`tun serve --cache-dir=reapi` enables the REAPI cache on the existing listener and
`DATA/cache.sock`. Relative cache dirs resolve under DATA. `--cache-max-size` defaults
to 10GB; committed CAS, action results and interrupted uploads share an on-disk LRU
budget. Reads touch mtimes; writes verify SHA256 before atomic publication. Instances
share the same content and action namespace. GetTree streams all reachable directories
(no pagination); batch RPCs are limited to 2MiB of payload. Identity compression only.
The advertised API version is 2.0, SHA256, with no execution capability.

`--cache-action-writes=false` rejects action updates with PERMISSION_DENIED and advertises
updates disabled. Default is allow while builds execute locally. The handler's write
policy is the future executor-only authorization seam; executor credentials and sandbox
socket binding are not part of this change. Both TCP and Unix endpoints accept
`x-tun-token` or `authorization: Bearer TOKEN` metadata. Thus the future token-free sandbox socket described above remains
integration work. Use `--remote_cache=grpc://HOST:PORT --remote_header=x-tun-token=TOKEN`;
add `--remote_proxy=unix:PATH` to use the Unix endpoint. `--cache-socket` overrides its
path. Startup prints a short `/proc/PID/fd/FD/name` alias usable when DATA paths exceed
Linux's Unix socket path length limit. A stale socket is removed on restart only after
connection refusal; a live listener is never replaced.

ByteStream uploads persist partial files under the cache directory, can resume at the
reported offset, and are evictable (NOT_FOUND means restart). Reads pin open files through
concurrent eviction and stream 64KiB messages. Requests honor uncapped `grpc-timeout` deadlines. A two-minute idle
timeout refreshes on each stream message; an expired request read returns
DEADLINE_EXCEEDED trailers. Stalled response consumers are bounded by the same
per-message idle write limit. ActionResult lookups check referenced CAS data so eviction produces cache misses.
Opaque ActionResult protobuf is retained, preserving fields unknown
to this cache. This is a cache milestone, not the complete remote execution protocol.

`scripts/bench-reapi.sh` generates 200 independent genrules, seeds the remote cache, then
rebuilds twice with fresh output roots under TMPDIR. Run via `scripts/dev-tun.sh` with
`DEV_TUN_SERVE_ARGS='--cache-dir=reapi'` in a tun job, or on the VM host with
`CACHE_URL=grpcs://remote.buildbuddy.dev` and `BUILDBUDDY_API_KEY`. Wall times include
Bazel installation extraction and cold JVM/analysis; Bazel's elapsed time is printed too.

Measured 2026-10-15 with Bazel 9.3.0 (fresh output root for each invocation):

| Cache / location | Seed wall | Rebuild 1 wall (Bazel elapsed) | Rebuild 2 wall (Bazel elapsed) |
| --- | --- | --- | --- |
| tun in-job server, job `20261015T080145924447-8ebe8f69` | 13.560s | 6.611s (4.457s) | 7.061s (4.893s) |
| BuildBuddy from VM host | 22.924s | 26.754s (24.008s) | 27.949s (25.317s) |

Every rebuild reported **200 remote cache hits**, with no local action execution.
BuildBuddy ran directly on the VM host as requested (no tun job ID); tun ran inside
an isolated job with its own cache server. Different hosts/network paths and concurrent
shared-server load mean these are end-to-end observations, not a controlled cache
implementation throughput comparison. A failed first tun benchmark
(`20261015T080037454959-c1928e5c`) exposed the Unix socket path length issue, now fixed
by binding through a directory FD alias. Tun's rebuild mean was 6.836s versus 27.352s.

Validation: full vet/build/race tests plus both CLI smoke suites passed in job
`20261015T080720200161-56b3e1c6` (75.251s). This includes real Bazel with 3MiB
artifacts exercising multi-message ByteStream uploads/downloads, Unix proxy, and an
assertion that minimal downloads leave intermediate output bytes absent. Final storage
restart/budget, concurrency, protocol, and serve-flag checks passed with vet/build in
`20261015T080907179208-0fe8e4b8` (7.837s).

Review hardening: storage builds its size/LRU index once at Open; writes and chunks
update it in constant time and only evict above budget. Disk I/O runs outside the
index mutex. Partial uploads keep incremental SHA-256 state in memory and rehash
only on first resume after restart. Failed eviction removals are logged and skipped,
without failing committed writes. Bazel test invocations have bounded contexts,
WaitDelay and shutdown cleanup; CI has an explicit ten-minute Go test timeout.

Review validation (2026-10-15): full vet/build/race tests, real Bazel cache checks,
and both CLI smoke suites passed in job `20261015T082314203271-b9f1367f`
(87.666s; REAPI package 57.694s). The explicit incremental-hash regression also
passed in `20261015T082226870100-792525f3`: a 3MiB upload keeps the same hash state
through 192 chunks. HTTP/2 deadline/auth/routing regressions passed in
`20261015T082040636635-1aa817bd`.

`BenchmarkStoreWrite` (300 replacements per case, job
`20261015T082115698248-7206b4c0`) measured 350.901us/op with 10 existing entries,
119.072us/op with 10,000, and 79.213us/op with 100,000. Each case creates real files
before Open and excludes startup indexing from timing. The decreasing times show
shared-host noise; the useful result is absence of growth with entry count.
An earlier superseded full CI job (`20261015T082110926143-4d167784`) stopped
producing package results after REAPI/sandbox; it was canceled after the final full
run passed. Its stall cause was not established. Separate server/workspace race
tests passed in `20261015T082410099198-c3a8b234` (4.655s job time).
