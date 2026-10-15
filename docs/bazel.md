# tun v1 direction: Bazel-native remote execution

Status: direction agreed 2026-10-15. The core of M1 exists (branch feat/bazel-sessions:
`tun bazel`, sessions, manifest sync on the hub; see "M1 status" below). SPEC.md stays the
contract for what exists; this file is the target and the order we get there.

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
- **M2 — tun's cache, many hosts.** Hub serves the remote cache (HTTP cache protocol first: it is
  stdlib-only), BuildBuddy flag removed, sessions placed across runners with affinity.
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
- **Stdlib only vs gRPC.** Bazel speaks gRPC only for remote execution. M2's HTTP cache needs
  nothing new; M3 needs HTTP/2 + protobuf. Plan: allow `google.golang.org/grpc`, `protobuf` and the
  remote-apis protos for M3 (built with Bazel by then), rather than hand-rolling gRPC.
- **Nested sandboxing:** Bazel's linux-sandbox inside tun's bwrap needs nested user namespaces;
  verify in M0, else run Bazel with `processwrapper-sandbox` and rely on tun's boundary.
- **Memory:** 4 GB runners, JVM servers. Tune `--host_jvm_args`, cap sessions per host, evict LRU.
- **Trust:** shared per-host caches are writable by every job today (v0 limitation); in M1 the
  repository cache is content-checked by Bazel, and the disk cache is per-session until M2/M3 move
  shared results to the executor-written remote cache.
- **Bazel versions:** bazelisk + the repo's `.bazelversion`; installs cached per host.
