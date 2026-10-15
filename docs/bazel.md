# tun v1 direction: Bazel-native remote execution

Status: direction agreed 2026-10-15; the standalone M1 BEP results package is implemented
(see results below). SPEC.md stays the contract for
what exists (`tun run`); this file is the target and the order we get there.

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

## Open questions / risks
- **Stdlib only vs gRPC — decision revised 2026-10-15.** No HTTP cache detour and no
  third-party gRPC/protobuf dependencies. Go 1.25 `http.Server.Protocols` with
  `SetUnencryptedHTTP2` supplies h2c; HTTP trailers supply gRPC status. `internal/reapi`
  implements framing and the protobuf wire subset for cache services, citing the official
  remote-apis and ByteStream protos. Execution is implemented below (M3 core).
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
The advertised API version is 2.0, SHA256, with SHA256 execution enabled.

`--cache-action-writes=false` rejects action updates with PERMISSION_DENIED and advertises
updates disabled. Default is now deny; use `--cache-action-writes=true` explicitly for cache-only local builds. The handler's write
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
`DEV_TUN_SERVE_ARGS='--cache-dir=reapi --cache-action-writes=true'` in a tun job, or on the VM host with
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


## M3 execution core

`--cache-dir` now enables Execution alongside the cache. Use
`--remote_executor=grpc://HOST:PORT --remote_header=x-tun-token=TOKEN
--noremote_upload_local_results`. Client AC writes are **disabled by default**;
only successful executor results populate the AC unless the operator explicitly
allows client writes. Execute honors `skip_cache_lookup` and Action.do_not_cache.
Failed commands return their exit code and stdout/stderr CAS digests without
caching the result. Infrastructure errors use ExecuteResponse.status.

Execute and WaitExecution stream google.longrunning.Operation messages, with
ExecuteOperationMetadata and ExecuteResponse Any type URLs. Operations are kept
in memory for the hub lifetime, independently of the client's connection. They
are not durable across hub restarts and currently have no retention limit.
Actions share the hub's v0 FIFO and local/runner slot limits. Runner polls
advertise action support; older runners continue receiving only v0 jobs.
The existing assignment, heartbeat, lost-runner and completion protocol applies.
Action and Command bytes (including platform properties) are recorded with the
internal executor job. Unsupported platform properties return FAILED_PRECONDITION;
currently OSFamily=Linux/linux and an empty container-image are accepted. Other
properties, including a nonempty container-image, are rejected. Tools come from
the executor host or the action's hermetic inputs, never a container image.

Executors copy verified inputs from CAS into fresh exec roots. Each runner has
a persistent 10GB LRU CAS under its data dir (`reapi-cas`), so repeated inputs
are not downloaded again. Copies rather than hard links prevent actions from
mutating the CAS. bwrap actions have network off, without the v0 egress proxy
or extra environment variables; Command.environment_variables is the complete
environment. `--sandbox=none` remains the explicit unisolated development mode.
Working directories, v2.0 output_files/output_directories, v2.1 output_paths,
symlinks, output directory Trees, executable bits and action timeouts are handled.
Timeouts kill the whole process group. Default action timeout is 30 minutes;
explicit timeouts above 24h are rejected.

Deviations / limits: exec-root paths currently vary per action (fixed `/work`
mounts remain session work); runner CAS transport uses an authenticated HTTP/JSON
extension of the existing pull protocol, rather than an outbound HTTP/2 gRPC
client. It currently buffers each transferred blob and limits the JSON request
to 128MiB (about 96MiB raw), so very large individual inputs/outputs need a
streaming runner transfer follow-up. Hub CAS eviction can cause an in-flight
action's input fetch to fail; inputs are not pinned for the whole operation.
There is no Execution cancellation RPC yet. Warm-session integration and label
artifact retrieval remain outside this core.

Real Bazel integration tests run with bounded 90s subprocess contexts and Go's
CI timeout: dependency chains, outputs, failing-action stderr, a clean rebuild
with remote cache hits, and a separate runner process against a hub with zero
local slots. Codec and executor regressions cover operation Any wrappers,
WaitExecution reconnects, cache bypass/no-write, paths, output directories,
working directories, environments and timeouts.

`scripts/bench-exec.sh` generates 200 independent genrules, each sleeping 0.5s,
with `--jobs=50`; all output roots live under TMPDIR. Run `local` directly in a
tun job, or `remote` / `runners` inside scripts/dev-tun.sh with
`DEV_TUN_SERVE_ARGS='--cache-dir=reapi --local-slots=2'` / `--local-slots=0`.
The runners mode starts two private runner processes with two slots each.

Measured 2026-10-15, Bazel 9.3.0, cold JVM/output roots:

| Mode | Wall | Bazel elapsed | Process summary | tun job |
| --- | --- | --- | --- | --- |
| Local Bazel | 63.048s | 60.273s | 200 linux-sandbox | `20261015T083812607482-cf9f6d2c` |
| Remote, hub local slots=2 | 61.189s | 58.889s | 200 remote | `20261015T083844410219-6fe2d8b2` |
| Remote, hub local slots=0, 2 runners x 2 slots | 39.969s | 37.121s | 200 remote | `20261015T083957200089-badce30b` |

These are shared-host end-to-end observations, not isolated scaling measurements;
the two runner processes add slots on the same physical job host.
Local protocol/Bazel integration passed in `20261015T083709369850-ccfa30fd`;
the separate-runner Bazel integration and operation tests passed with race in
`20261015T084211471977-eea1803f` (44.047s job time).

Final hardening: bwrap itself injects PWD even after --clearenv; execution uses a
small in-namespace exec helper to install exactly the Command environment after
bwrap enters its namespaces. The helper has no egress proxy and replaces itself
with the action. A regression compares the complete environment and network
namespace against the host. WaitExecution honors gRPC deadlines without canceling
the action. Scheduler/runner infrastructure timeouts preserve DEADLINE_EXCEEDED.

Full vet/build/race tests, both real Bazel integrations, and both CLI smoke suites
passed in `20261015T084344724408-eec341c1` (306.947s under concurrent self-build
load). Subsequent vet/build/race tests covering the hardened paths and both local
and separate-runner Bazel builds passed in `20261015T084836390529-304e8ec6`
(111.518s). Exact-environment/network namespace tests and both Bazel execution
integrations passed in `20261015T085232460964-85588474`.

The M0 self-build benchmark uses `scripts/bench-exec-self.sh`. While M0 is a
separate branch, prepare its overlay on the VM before submitting the tun job:

```sh
git archive origin/feat/bazel-m0 $(git ls-tree -r --name-only origin/feat/bazel-m0 | rg '(^|/)(BUILD\.bazel|MODULE\.bazel(\.lock)?|\.bazelrc|\.bazelversion|bazel-e2e\.sh)$') > m0-build.tar
tun run --tail 100 --timeout 12m -e DEV_TUN_SERVE_ARGS='--cache-dir=reapi --local-slots=2' -c 'scripts/dev-tun.sh ./scripts/bench-exec-self.sh'
rm m0-build.tar
```

The script copies the workspace to TMPDIR, overlays the M0 files and refreshes
internal package libraries to include packages added since the M0 snapshot. It
runs `bazel build //...` with M0's downloaded Go 1.25.14 SDK. It does not modify
this branch's BUILD files or run heavy Bazel commands on the VM.

Self-build debugging exposed a required wire detail: output Directory blobs must
use canonical proto3 serialization, including field order and omission of default
scalar values. Bazel parses and reserializes Tree children to index them by digest;
equivalent but noncanonical protobuf made Bazel 9.3.0 crash in parseDirectory
(job `20261015T084928396704-ca5de92d`, 260.326s). The encoder now groups Directory
fields by number and omits false executable bits and zero digest sizes. Regressions
cover exact canonical bytes, mixed nested file/directory trees and empty files and
directories, plus a real Bazel tree-producing action and a dependent action.
