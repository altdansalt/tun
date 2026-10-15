# tun v1 direction: Bazel-native remote execution

Status: direction agreed 2026-10-15. Built: M0 (tun builds and tests with Bazel; see "M0 results"),
the M1 core (`tun bazel` sessions, manifest sync, BEP summary on the hub; see "M1 status"),
tun's REAPI cache, the M3 execution core, and remote execution from sessions without asking
(see "Remote execution in sessions"). SPEC.md stays the contract for what exists; this file is the target and the order.

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
tun's own cache serves h2c on a per-session socket (`/tun/sock/cache.sock`, built: see
"Automatic cache and execution" below), authenticated by which socket the connection came in
on, so sessions never need a network or a token for it.

Only executors (tun itself) write the *shared* action cache namespace. (Revised 2026-10-15:
sessions do upload their local results, but into their own client's namespace, which only that
client reads; so no client can poison another's builds, and a client's new or moved session
gets hits for what it built before executors exist.)

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
Built: `tun bazel [--session NAME] [--cold] [--json] <cmd> [args...]`, `tun sessions`, sessions
on the hub's local slots (fixed `/work`, `/tun/home`, `/tmp`, output base `/tun/out/base`), a
long-lived session sandbox whose agent runs each command (so the Bazel server survives between
commands), manifest sync with in-place apply, idle expiry (`--session-idle`, 30m) and LRU
eviction (`--max-sessions`, 2), per-client Bazel caches (bazelisk, repository, disk), the BEP
summary line and `--json` (internal/bep). Contract in SPEC.md ("Bazel sessions").

Deviations from the model above, and why:
- **A busy session queues** the next command of the same session instead of starting a new
  cold one. A second Bazel server costs 1GB+ on a 4GB host and a cold analysis; `--session
  NAME` gets the parallel one explicitly. Waiting jobs show `queue_reason`.
- **All Bazel caches per client** (`$DATA/bazel/<client>/`), not per session and not per host:
  `--cold` and a second `--session` of the same client reuse them, and one client can't poison
  another's. The plan said "per-host repository cache"; a security review showed why not: Bazel
  8+/9 keep fetched repository trees (the repo contents cache) in `<repository_cache>/contents`
  by default (verified on 9.3.0, job 20261015T085624196657-2083995d) and trust them, and
  bazelisk runs a cached Bazel binary without checking it again, so a shared writable cache lets
  one client run code in another's builds. The cost is a Bazel download per client and
  version (`tun serve --bazelisk-seed DIR` copies a server-owned bazelisk cache into new client
  homes) and repository fetches per client. None of these dirs can be written by v0 jobs (they
  used to be: bazelisk's home was in the shared XDG cache). The disk cache is size-capped by
  Bazel (`--experimental_disk_cache_gc_max_size=5G`); tun doesn't delete per-client dirs yet.
  Isolation is as good as the client identity: unforgeable behind exe.dev's proxy only.
- **Bazel startup options from the agent are rejected** (tun's own must stay constant per
  session or Bazel restarts its server).
- The client identifies its workspace by its absolute root path; two checkouts of one repo are
  two sessions (they share the client's disk cache).

- **Summary only for build, test, coverage, run.** Bazel 9.3.0 accepts
  `--build_event_json_file` for query, cquery and aquery too (contrary to the note in "M1
  results" below, verified in job 20261015T085624196657-2083995d), and silently ignores it for
  info, clean, mod, config; a "N targets built" line would mislead there, so those get only
  the `--json` object (exit code, duration). The full uncapped snapshot is not kept (only the
  bounded JSON in the job dir), since `tun get` by label is not built yet.
- **Leftover processes are killed after each command** (bwrap only): everything in the session
  sandbox except the agent, its ancestors and the Bazel server (`server.pid.txt`) with its
  descendants, so a command can't leave something behind that tampers with the next one.

Not done yet in M1: the optional
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

After the security fixes and with the BEP summary (same setup, tun job
20261015T091046678975-d7e2d5f7, hub under load): first command 9.9s (Bazel elapsed 9.7s, new
server; job 20261015T091049121021-84c87d9a), no-op 0.40s (Bazel 0.36s;
20261015T091059030954-db282b89), one-line change 0.37s (Bazel 0.28s, 140B uploaded;
20261015T091059429520-40590e2e), each ending in `tun: bazel build exit=0 after 0.4s: 1 targets
built`; `--json` returns `bazel-bin/g.txt` as the output. The cost of the BEP file, its
parsing and the per-command sweep was not measured separately (the earlier run had a 0.11s
third no-op; a single sample on a loaded host can't tell them apart).

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
  remote-apis and ByteStream protos. Execution is implemented below (M3 core).
- **Nested sandboxing:** Bazel's linux-sandbox inside tun's bwrap needs nested user namespaces;
  verify in M0, else run Bazel with `processwrapper-sandbox` and rely on tun's boundary.

## M0 results (2026-10-15)
Done: `MODULE.bazel` (rules_go 0.64.2, gazelle 0.54.0, rules_shell 0.8.0; Go SDK 1.25.14 via
`go_sdk.download`, matching go.mod's `go 1.25`), gazelle-generated BUILD files (`bazel run //:gazelle`),
`scripts/bazel-ci.sh` (see AGENTS.md). `bazel test //...` is green in tun jobs: 4 go_test targets
plus the CLI smoke suites as `//scripts:e2e_test` and `//scripts:e2e_runners_test` against `//cmd/tun`.

What the tests needed under Bazel:
- **No special tags.** Nested sandboxing works both ways: Bazel's linux-sandbox inside tun's bwrap,
  and tun's own bwrap + `--unshare-net` jobs inside linux-sandbox (tests and e2e start servers that do
  this). All test traffic is loopback, so no `requires-network`; no `no-sandbox`; no `exclusive`
  (the timing-sensitive tests passed 3-8 runs per test). `client_test` is `size = "large"`.
- **Data deps:** scripts/ci.sh, bb-ci.sh, bazel-ci.sh (tests run them in jobs); the hermetic SDK
  (`@go_sdk//:files`) for tests that run `go` inside jobs, put on PATH via `TUN_TEST_GO`; `//cmd/tun`
  for the e2e suites (`scripts/dev-tun.sh` takes `DEV_TUN_BIN`).
- Host tools still used by tests: bash, bwrap, git, python3, curl (tests skip without bwrap/git/
  python3). On BuildBuddy's runner, 16 bwrap/netns tests skip, so its client_test does less work.
- **Go 1.25 found bugs that ci.sh (Go 1.27.2) hides:** the egress proxy left the connection open
  after a failed CONNECT (fixed), and two service tests raced a helper that exits at once (fixed;
  hidden by `-race` slowing the helper).

Deviations from this plan:
- gazelle 0.54.0, not the newer 1.0.0/2.0.0 betas.
- bazel-ci.sh shares a **per-host disk cache** across jobs (not per-session as in Trust below): it is
  what makes v0 jobs (fresh output base each time) usable. Every job on a host can write it, and it
  also caches test results (`(cached) PASSED`). `BAZEL_DISK_CACHE=0` opts out.
- `-race` is not the Bazel default (`--config=race`); it needs cgo and uses the host's C compiler,
  i.e. not hermetic.
- bazel-ci.sh deletes its output root at exit: tun keeps a job's TMPDIR for `--keep-work` (1h), and
  ~15 Bazel jobs in an hour filled a 25 GB host.
- The default BUILD check is "every .go file appears in its BUILD.bazel", not gazelle's diff:
  building gazelle in a fresh output base cost 11-21 s per job even with warm caches (`GAZELLE=1`).

### Measurements
`run_sec` of tun jobs on the shared 2-CPU tun host (other agents' jobs ran at the same time: "load"
is healthz `running` at submit, all hosts). Same tree as scripts/ci.sh (vet, build, `go test -race`,
both e2e suites). "Edit" = a one-line change to a string in internal/server/gc.go, made in the job
(new value each run), so server, client, cmd/tun and the tests depending on them rebuild and rerun.

| Scenario | Runs (s) | Jobs |
| --- | --- | --- |
| Cold: empty repo + disk cache, fresh output base | 159.9, 183.0 (old script, incl. its gazelle build; load 0-1); 239.5 (load 0); 545.7 (load 3); 330.3 (runner tun-r2, `GAZELLE=1`) | 20261015T083307159856-d5ec5511, 20261015T083557649170-11d597df, 20261015T093014942985-28ed1fdf, 20261015T094341404185-a4cd9131, 20261015T084418352435-94c9daef |
| Warm caches, same tree (everything cached, incl. tests) | 15.2, 26.3 (local); 24.6, 26.3, 27.2 (tun-r2) | 20261015T092954699697-67a04a85, 20261015T092928018850-247ee30e, 20261015T085033587248-eb19103d, 20261015T085058685975-f31d3e9f, 20261015T085005870409-065c6965 |
| Warm caches, `--nocache_test_results` | 66.0, 77.2, 97.7, 142.0 | 20261015T084028377092-82dbd090, 20261015T084134790010-3547bc3f, 20261015T085355934890-62d7d806, 20261015T085133485666-d41a2b59 |
| Edit, warm caches | 53.4, 65.5, 68.4 (load 0-1); 171.1, 227.7 (load 3-4) | 20261015T090733888683-41c92aa8, 20261015T092316469013-a693440b, 20261015T090953200936-84c0caec; 20261015T085534387731-3463bc8f, 20261015T085826379064-84912fe5 |
| Edit, warm caches, `--config=race` | 116.5, 141.5 | 20261015T091553349152-ac74bc92, 20261015T091331518150-d38e5d94 |
| scripts/ci.sh, same tree | 76.8, 99.8 | 20261015T090319907959-06a107fe, 20261015T090437079004-e3d230ea |
| scripts/ci.sh, edit | 71.4, 85.3 | 20261015T090622133533-4c237f62, 20261015T090827552814-b3e9f7f8 |
| `bb remote test //...` cold (new runner, empty cache) | 126.6 wall (Bazel 118.5) | invocation 68bbf30f-10de-4346-9585-26d03a201590 |
| `bb remote`, same tree | 54.0 (cold runner, remote cache hits), 10.8 (warm runner; Bazel 2.3) | |
| `bb remote`, edit (uncommitted change, warm runner) | 30.6, 30.7 (Bazel 21.9, 21.9) | |

(`bb remote` times are client wall clock from this VM, including its upload/patch and runner
attach; tun's include only the job's run time, upload is ~0.2 s for this repo.)

Where the time goes (Bazel `--profile` of a warm edit run, 62.6 s): ~4 s server start + install
extraction, ~7 s loading/analysis + repository setup (Go SDK and rules re-extracted from the
repository cache, `local_config_cc`), ~3 s runfiles trees (the Go SDK in runfiles), ~3 s compile and
link from the disk cache, then tests: e2e_test 19 s and e2e_runners_test 8 s took both CPU slots,
so client_test (the critical path, 36 s) waited 9 s after it was linked. A warm no-op (15 s) is
almost all start + analysis + fetching outputs from the disk cache.

What this means for M1: a warm session removes the ~15-25 s that every v0 Bazel job pays before
the first test starts (that is all of a no-op; `bb remote`'s warm no-op is 10.8 s wall, 2.3 s in
Bazel). On an edit, test runtime dominates (client_test ~36 s, e2e ~19 s on 2 CPUs), so warm
sessions cut 25-35% there; beating `bb remote`'s 30.6 s edit (which skips the bwrap tests) also
needs more parallelism (`--local_test_jobs` above CPU count for these I/O-bound tests: 61 s and
121 s in two tries, too noisy to judge) or remote execution (M3). Load from other agents moved
identical runs by 2-4x: M1 measurements need an otherwise idle host or many more runs.

## Open questions / risks
- **Stdlib only vs gRPC.** Bazel speaks gRPC only for remote execution. M2's HTTP cache needs
  nothing new; M3 needs HTTP/2 + protobuf. Plan: allow `google.golang.org/grpc`, `protobuf` and the
  remote-apis protos for M3 (built with Bazel by then), rather than hand-rolling gRPC.
- **Nested sandboxing:** verified in M0: Bazel's linux-sandbox works inside tun's bwrap (and tun's
  bwrap jobs inside linux-sandbox, for tun's own tests).
- **Memory:** 4 GB runners, JVM servers. Tune `--host_jvm_args`, cap sessions per host, evict LRU.
- **Trust:** shared per-host caches are writable by every v0 job today (v0 limitation). Session
  caches are per client (see M1 status: the repository contents cache and bazelisk downloads
  are not content-checked on use), until M2/M3 move shared results to the executor-written
  remote cache. Every bind mount source is opened without following symlinks and passed to
  bwrap by descriptor.
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
`x-tun-token` or `authorization: Bearer TOKEN` metadata. (The token-free sandbox socket is
built now: see "Automatic cache and execution".) Use `--remote_cache=grpc://HOST:PORT --remote_header=x-tun-token=TOKEN`;
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


## Automatic cache and execution (feat/bazel-auto)

Goal: the agent sends `tun bazel test //...`; remote caching (and later remote execution)
happens without the agent asking.

**Cache, built.** `tun serve` enables the REAPI cache by default (`--cache-dir=reapi`;
`--cache-dir=` disables it). The server, not `main`, owns the cache handler (`Config.Cache`),
so in-process tests get the same behavior. While a session's sandbox lives the server listens
on `sessions/<id>/sock/cache.sock` (listened through the dir's descriptor, so long data paths
work) and binds that dir read-only at `/tun/sock` (bind source opened like every other mount:
no symlinks, owned by the server, passed as a descriptor; connecting needs no write access to
the mount). tun.bazelrc adds `--remote_cache=grpc://tun --remote_proxy=unix:/tun/sock/cache.sock`
(verified with Bazel 9.3.0: plaintext HTTP/2 over the socket) and
`--experimental_build_event_upload_strategy=local` (otherwise the BEP names outputs as
`bytestream://` URIs and the summary loses them; internal/bep now also derives output paths
from bytestream files).

**Auth and trust.** The socket is per session, so the connection itself authenticates: the
handler for that listener (`reapi.Handler.ForClient`) needs no token and uses the session's
client as the action cache namespace, whatever the request says. The sandbox never sees the
server token. On the token-authenticated endpoints (TCP, `DATA/cache.sock`) the namespace is
the request's client identity, as for jobs (exe.dev source VM, else `X-Tun-Client`, else the
remote address). `instance_name` is ignored everywhere. The CAS is shared (verified on write);
action results live in `acc-<sha256(client NUL digest)>`, the shared namespace in `ac-<digest>`
(what reapi-exec's executors write). Lookups read the client's own result, then the shared one;
nothing reachable over gRPC writes the shared namespace. `--cache-action-writes=false` makes
sessions run with `--noremote_upload_local_results`.

**Execution, designed.** The bazelrc is generated per command from a `remoteSetup{socket,
executors, readOnly}`: `executors` is the number of action slots the server can run
(`Server.executionSlots`, 0 until the Execution service is in this server: then local
execution, as now). With executors > 0 the rc adds `--remote_executor=grpc://tun` (same socket,
so the same per-session auth; ForClient copies the handler, including a future Execution
field), `--jobs=<slots>` and `--remote_local_fallback`. Executor results go to the shared
namespace, which every session reads. Since the rc is rewritten before each command, `--jobs`
follows runners joining and leaving, and none of these are startup options, so the warm Bazel
server survives the change. Open point for when it lands: a session command holds a hub slot
while its actions need slots too; with only local slots and `--max-sessions` sessions busy,
actions could starve. Sessions should not count against action slots (or actions get their
own pool). (Built: see "Remote execution in sessions".)

Merging feat/reapi-exec (trial merge 2026-10-15 at db5eea0): conflicts in main.go/server.go
(both moved the cache setup; Execution should be created in `server.New` next to the cache
handler, so session sockets get it through `ForClient`), store.go `read` (take reapi-exec's
verified read; `readName` is the namespaced variant) and the docs. reapi-exec makes
`--cache-action-writes` default to false because writes went to the one shared namespace; with
per-client namespaces client writes can't poison anyone else, so the default can stay true.
Then `executionSlots` returns local slots plus action-capable runner slots when Execution is on.

Deviations from the plan above: sessions upload local results (own namespace, see Trust);
with a remote cache sessions get no `--disk_cache` (measured below: no faster with both); the socket lives under the
session dir (not `/tun/cache.sock`, since the bind source must be a server-owned directory).
With `--sandbox none` the rc uses the host path, and a session whose socket path is longer
than sun_path allows gets no remote cache (and says so).

### Measurements (2026-10-15, tun's own tree)

`scripts/bench-session.sh` in a tun job: `scripts/dev-tun.sh` starts a private server from
this branch (cache on by default) on the job's host, and the client runs on the same host (no
network round trips). The tree is this branch merged with feat/bazel-m0 plus gazelle (scratch
branch, not pushed). Bazel 9.3.0, Go SDK 1.25.14 from M0's MODULE.bazel.

Run 2, `tun bazel test -- //... -//internal/client:client_test -//internal/reapi:reapi_test
-//scripts:e2e_test` (5 test targets), sessions without a disk cache; outer job
`20261015T100741124967-924c9aa6` on runner tun-r4 (inner job ids are the private server's):

| step | wall | Bazel | processes | inner job |
|---|---|---|---|---|
| (a) new session, empty caches (repository cache too: Go SDK download) | 121.4s | 120.4s | 103 linux-sandbox | 20261015T100801391780-41962487 |
| (b) `--cold`, warm remote cache ("session on a new host") | **12.6s** | 10.8s | 103 remote cache hit | 20261015T101002693438-3fa8d8e2 |
| (b) again (`--cold`, remote only) | 13.3s | 11.5s | 103 remote cache hit | 20261015T101137235787-c333181e |
| (b') `--cold --remote_cache=` (no action cache at all, warm repository cache) | 81.9s | 81.0s | 103 linux-sandbox | 20261015T101015294282-05f286de |
| (c) warm no-op | **0.98s** | 0.95s | 7 action cache hit | 20261015T101150494377-ad38224e |
| (d) one-line change in internal/server/server.go | **2.67s** | 2.62s | 4 linux-sandbox | 20261015T101151478407-52e1a9a1 |
| (d) its no-op | 0.58s | 0.55s | 5 action cache hit | 20261015T101154148120-7e0661a2 |

The one-line change sent one 7.7KB file (12KB manifest); it was a trailing comment, so the
recompiled server library had the same output and the dependent tests stayed cached (4 actions
ran). The remote cache held 294MB afterwards.

Run 1, all of `//...` (8 test targets, 3 of which fail in a session, see below), sessions with
disk *and* remote cache; outer job `20261015T094443470466-e64bc22d` on runner tun-r2: (a)
447.6s; (b1) `--cold` with disk and remote warm 168.2s (113 disk cache hits); (b2) `--cold`
with only the remote cache (disk cache deleted) 167.2s (113 remote cache hits); (b3) `--cold
--remote_cache=` with only the disk cache 226.9s (0 disk cache hits: not understood; Bazel
re-executed all 117 actions); (c) no-op 96.4s; (d) one-line change 103.9s. The failing tests
are not cached, so they rerun every time and dominate (c) and (d) (critical path ~95s).
Disk + remote vs remote only: 168.2s vs 167.2s, hence no disk cache next to the remote cache.

For comparison, `tun run -- ./scripts/ci.sh` (go vet/build, `go test -race` of everything, both
smoke suites) took 233.7s (job 20261015T093529837825-42cbbe49) and 249.6s
(20261015T094450267997-c543fe16) on runners: every run pays the whole test suite.

The three targets that fail in a session run a real Bazel themselves (TestBazelReal,
reapi's TestBazelCache, scripts/e2e.sh's Bazel part): inside Bazel's test sandbox bazelisk has
no cached Bazel binary and can't download one (Bad Gateway through the session's proxy;
`latest` needs DNS for the version lookup). That is M0's tests' environment (they pass under
`go test` in tun run), not the session cache; fix in M0 by passing a Bazel binary as test data
or tagging them `manual`.

## M3 execution core

`--cache-dir` now enables Execution alongside the cache. Use
`--remote_executor=grpc://HOST:PORT --remote_header=x-tun-token=TOKEN
--noremote_upload_local_results`. Client AC writes are **disabled by default**;
only successful executor results populate the AC unless the operator explicitly
allows client writes. (Merged into feat/bazel-rbe with per-client namespaces, so the default
is allow again: see "Automatic cache and execution".) Execute honors `skip_cache_lookup` and Action.do_not_cache.
Failed commands return their exit code and stdout/stderr CAS digests without
caching the result. Infrastructure errors use ExecuteResponse.status.

Execute and WaitExecution stream google.longrunning.Operation messages, with
ExecuteOperationMetadata and ExecuteResponse Any type URLs. Operations are kept
in memory independently of the client's connection. Completed operations expire
after one hour (pruned on the next Execute). Random 128-bit names prevent restart
collisions; operations are not durable across hub restarts. Streams resend their
current operation every 30s and clear idle write deadlines after flushing.
Actions share the hub's v0 FIFO and local/runner slot limits. Runner polls
advertise action support; older runners continue receiving only v0 jobs.
The existing assignment, heartbeat, lost-runner and completion protocol applies.
Action and Command bytes (including platform properties) stay in memory with the
internal executor job; action jobs create no v0 job records and are hidden from tun ls. Unsupported platform properties return FAILED_PRECONDITION;
currently OSFamily=Linux/linux and an empty container-image are accepted. Other
properties, including a nonempty container-image, are rejected. Tools come from
the executor host or the action's hermetic inputs, never a container image.

Executors copy verified inputs from CAS into fresh exec roots. Each runner has
a persistent 10GB LRU CAS under its data dir (`reapi-cas`), so repeated inputs
are not downloaded again. Copies rather than hard links prevent actions from
mutating the CAS. bwrap actions have network off, without the v0 egress proxy
or extra environment variables; Command.environment_variables is the complete
environment. `--sandbox=none` remains an unisolated local development mode whose results never
populate the AC. Remote actions are assigned only to bwrap runners.
Working directories, v2.0 output_files/output_directories, v2.1 output_paths,
symlinks, output directory Trees, executable bits and action timeouts are handled.
Timeouts start after input materialization and kill the whole process group.
Cancellation interrupts input copies and runner fetches. Default action timeout
(including an explicit zero duration) is 30 minutes;
explicit timeouts above 24h are rejected.

Deviations / limits: exec-root paths currently vary per action (fixed `/work`
mounts remain session work); runner CAS transport uses an authenticated HTTP/JSON
extension of the existing pull protocol, rather than an outbound HTTP/2 gRPC
client. It buffers transferred blobs with an explicit 96MiB raw limit and bounded JSON
decoding; oversized reads fail before loading bytes. Larger blobs need a
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

Corrected self-build succeeded in job `20261015T085626697154-490ef40b`:
**330.740s wall**, Bazel elapsed **328.005s**, critical path **267.96s**,
**118 processes: 44 internal, 74 remote**. The overlay was M0 commit
`abd610e8053cb7398400a91f7bd0791c444ae16f`; package library sources/dependencies
were refreshed for this execution branch. The downloaded Go 1.25.14 SDK was
shipped as CAS inputs and ran with network disabled. No toolchain-environment
failure remained after the Directory encoding fix. This is a cold build under
shared-host load, including analysis, SDK setup and stdlib compilation; it is not
a warm-session measurement. The earlier 240s self-build budget was insufficient
(`20261015T084415612968-3e8f39fa`); the reproducible script now allows 600s with a
hard kill grace and an outer tun timeout.

Final **full** vet/build/race tests, both real Bazel integrations (including the
nested mixed output Tree), exact environment/network isolation, cache reuse,
and both CLI smoke suites passed in `20261015T085657372388-505fd9c8`:
283.089s running after 88.216s queued. The standalone canonical-encoding and real
Bazel Tree regressions also passed in `20261015T085609309999-7933381d`.

Tun friction observed: shared capacity produced 25.599s and 88.216s queue waits;
concurrent cold Go/Bazel builds substantially changed timings (full CI ranged
from roughly 1–5 minutes). `--tail` plus explicit job IDs made finished runs easy
to inspect, but redirected nested benchmark logs were unavailable through the
parent's stdout until the script exited. REAPI executor jobs currently have
empty v0 stdout/stderr files because their logs live in CAS; operation/stage/CAS
log inspection in the CLI is useful follow-up work.


## Execution review hardening

Deploy the upgraded hub first: it creates `$DATA/runner-token` with mode 0600.
Copy that file securely to each runner and restart its updated binary with
`tun runner --token-file /path/to/runner-token` (or `TUN_RUNNER_TOKEN`).
The client token is refused on every runner endpoint. Runner credentials work
only under /v1/runner/, including workspace and CAS downloads; requests require
the process instance header. Existing runners using the client token must migrate.

Executor limits are 96MiB per blob/output, 4MiB per stdout/stderr stream,
100000 materialized input nodes and 2GiB total expanded inputs. Exceeding a limit
returns RESOURCE_EXHAUSTED; log overflow cancels execution promptly. Local file
copies and output hashing/upload stream with bounded memory. The runner JSON
transport still buffers individual blobs; replacing it with streaming transfer
remains a documented deviation. Exec roots use chmod-aware cleanup and are swept
on startup. Missing inputs return FAILED_PRECONDITION with PreconditionFailure
MISSING violations; lost runners return UNAVAILABLE. Local CAS reads verify hashes.

Review validation (2026-10-15): full vet/build/race CI, real Bazel cache and
execution integration, and both CLI smoke suites passed in tun job
`20261015T092730806002-f76adfff` (155.516s; client 87.067s, REAPI 114.794s,
server 40.954s). Focused review regressions also passed on the BuildBuddy fallback
in invocation `e091c65f-a141-4bd4-9305-6f3c46539e83`.

The 200-genrule benchmark on a private dev server with zero local slots and two
runners with two slots each completed in **50.189s wall / 45.982s Bazel elapsed**,
with **200 remote actions**, in tun job `20261015T092604426090-0fd19469`
(54.051s total job time). The previous 39.969s runner measurement used the same
logical topology; these runs share remote hosts with other jobs, so the difference
is not a controlled throughput comparison. All Bazel roots stayed under TMPDIR.

Tun friction during review: job `20261015T091722785773-65cba55c` was lost when
shared runner tun-r3 shut down. The hub subsequently timed out on health requests
and returned a workspace-upload HTTP 502, requiring the documented BuildBuddy
fallback until it recovered. No shared server was restarted by this work.
Superseded CI jobs were canceled. Fallback testing also exposed inherited Bazel
configuration; integration builds and benchmarks now ignore host bazelrc files.

## Integration with warm sessions and disk retention (2026-10-15)

Merged origin/main (cd41306) into feat/reapi-exec. Runner scheduling skips warm
session commands and skips actions on runners without action support, preserving
ordinary job scheduling behind either. Polls retain both the disk report and action
capability. Network-disabled action sandboxes now use the common descriptor layout
(including the exact-environment helper) and openMountSource for every mutable bind
source. All /v1/runner/* routes continue to require the dedicated runner token and
instance header.

Before materializing action inputs, runners reclaim workspace blobs and then evict
their CAS in LRU order while below --min-free. This extends the existing retention
policy to action storage; it does not impose admission control when active data or
unrelated files still keep the disk below the threshold. Concurrent readers retain
their descriptors and evicted inputs can be fetched again. No new direction change.

The repeated 200-genrule benchmark (0.5s sleep per rule, private hub with zero local
slots, two runner processes with two slots each) completed with **200 remote actions**
in **70.607s wall / 63.532s Bazel elapsed**, job
`20261015T093730077050-86312200` (80.477s total job time, 0.809s queue).
All Bazel output roots were under TMPDIR. Shared-host contention and cold startup
are included; this is not a controlled comparison with the earlier 50.189s run.

Shutdown investigation: the historical job
`20261015T091722785773-65cba55c` remains queryable: runner tun-r3, started
09:17:22.937 UTC, failed 09:18:06.013 with signal 15 and "runner tun-r3 shut down
while the job was running". That establishes shutdown reporting, not the cause of
the subsequent health timeouts/HTTP 502. Inspection of the merged handlers and
origin/main shows runner body reads for poll, exited, done and leave outside
Server.mu; CAS and log/output transfers also perform network reads outside it.
Regression TestStalledRunnerReportsAllowHealth deliberately holds those JSON
reports incomplete over a pipe and checks health still responds through the HTTP
handler. No historical hub/proxy logs or goroutine/resource traces were available,
so overload versus another bug remains undetermined. State persistence and some
responses still run under the hub mutex; the test does not exclude disk stalls or
slow response writers. No shared server was restarted.

Validation of the merged tree: full vet/build/race tests (including real Bazel)
and both CLI smoke suites passed in tun job
`20261015T094423233532-c3ea51a8`: **527.658s** run time, 5.609s queued;
client package 345.023s, REAPI 445.823s, server 120.019s.
Focused disk-pressure/mount checks passed in
`20261015T094014563979-4c9dc4b9` (19.701s), and the Bazel/session diagnostic
passed in `20261015T094933108093-62ad2742` (client package 51.307s).

Tun friction: an earlier full run, `20261015T094025566091-2fc64155`,
was interrupted after 222.113s by "runner tun-r2 shut down while the job was
running". All four runners resumed polling. Five subsequent hub health samples
returned HTTP 200 in 75.8, 22.4, 26.4, 18.0 and 22.2ms; this shutdown did not
reproduce the earlier sustained health outage. The successful full run used a
hub-local slot and was much slower than the previous branch validation; no CPU
or memory trace was collected to attribute that difference. Normal go test output
was silent while packages ran; TEST_FLAGS='-json -run TestBazel' exposed live
test progress for the diagnostic. A precautionary BuildBuddy fallback invocation
(ca7d054a-9f74-4210-a41a-44ddc56e6783) completed before cancellation: its
vet/build/race packages passed, but the first CLI smoke suite exited 1 under
sandbox none (bwrap unavailable). The cleanup also reported a Bazel invocation
referencing a deleted temporary tun.bazelrc. This fallback smoke failure was not
investigated further; the complete tun run passed both suites with bwrap.
Superseded local iterations were canceled as well.

## Remote execution in sessions (feat/bazel-rbe)

Built on feat/bazel-auto + main (M0) + feat/reapi-exec. The agent sends `tun bazel test //...`;
when the server executes actions, the session's Bazel sends them to it without being asked.

- **Wiring.** `server.New` creates the Execution service next to the cache handler, so every
  session socket gets it through `ForClient` (same per-session auth: no token, the session's
  client namespace for its own uploads). The session's `tun.bazelrc` (rewritten before every
  command) adds `--remote_executor=grpc://tun` over the session socket, `--jobs=<executor
  slots>` (`Server.executionSlots`: local slots plus the slots of live runners that take
  actions, so it follows runners joining and leaving without restarting the Bazel server) and
  `--remote_local_fallback`. Executors write successful bwrap results to the shared action
  cache namespace, which every session reads after its own; session uploads (local fallback,
  `--remote_executor=` from the agent) stay in the client's namespace. `--cache-action-writes`
  keeps its default `true` (reapi-exec made it `false` because there was one namespace).
- **Starvation, solved by not counting orchestrating session commands.** A session command
  whose session executes remotely takes no job slot; such commands are bounded by
  `--max-sessions` (and one per session). Measured, `scripts/bench-starvation.sh` (job
  20261015T105653696427-2bbbab7d): one local slot, no runners, one session building 20
  genrules that sleep 1s. With the command holding the slot (the old rule, patched in) no
  action ever started: 0 executed, `tun bazel --timeout 2m` ended it (exit 124). Without: 20
  remote actions, 53.9s (first command of a new session; a loaded host). Why not count it at
  all: the session's Bazel server only analyzes and waits. Its CPU time during a warm remote
  build of 200 genrules was 5.9-9.2s over 72s of wall time (8-13% of one core; first build of a
  new session: 19.5s over 92s, JVM warm-up and analysis included), job
  20261015T112626731460-71f73009. The alternative (a separate pool of hub slots for actions)
  would idle those slots whenever no session builds.
- **--jobs = slots.** Measured with twice the slots too: 72.6s vs 71.9s (above job), so no
  headroom. (A first run seemed to show 0.28s per action saved by 2x, but compared a new
  session's first build with a warm one: 20261015T105112809976-cf993397.)
- **healthz `execution`**: `{slots, queued, running, executed: {local|<runner>: n},
  session_commands}`; tests and benchmarks read where actions ran from it.

### Overhead per remote action

`scripts/bench-rbe.sh genrules` in `scripts/dev-tun.sh` with `--local-slots 1` and two `tun
runner` processes with one slot each (like tun-r1..r4) on the job's host (2 CPUs): 200
independent genrules that sleep 1s, `--action_env=BENCH=<n>` per step so every action runs
again in the warm session. Job 20261015T112626731460-71f73009 (on the shared hub host, other agents'
jobs possible):

| step | Bazel elapsed | processes | overhead per action (elapsed x parallelism / 200 - 1s) |
|---|---|---|---|
| g1 new session, remote, --jobs=3 | 92.0s | 200 remote (67 hub, 66 + 67 runners) | (incl. Bazel start and analysis) |
| g2 warm, remote, --jobs=6 | 72.4s | 200 remote | 0.09s |
| g3 warm, remote, --jobs=3 (tun's) | 71.8s | 200 remote | **0.08s** |
| g4 warm, local linux-sandbox, --jobs=3 | 103.6s | 200 linux-sandbox | 0.55s |
| g5 warm, local linux-sandbox, --jobs=auto (2) | 109.8s | 200 linux-sandbox | 0.10s |

So a remote action costs ~80ms of slot time on top of its command (Execute round trip, FIFO,
input materialization, bwrap with its own network namespace, output upload, AC write), less
than Bazel's own linux-sandbox once the 2 CPUs are shared by 3 actions. The runners here are
on the same host as the hub; on other hosts each runner action adds its HTTP round trips to
the hub (assignment, CAS fetches, started, exited, outputs, done), ~35ms each from an exe.dev
VM, not measured here (see Deviations).

### Go actions: input materialization and the tree cache

Go compile actions name the Go SDK and the built standard library as inputs, so a remote
action first copies (and hashes) thousands of files into a fresh exec root. Executors now
report REAPI `ExecutedActionMetadata` (input fetch, execution, output upload) and healthz
sums it (`execution.input_fetch_sec` etc.). `scripts/bench-rbe.sh build` (`bazel build
//cmd/tun`, 11 remote actions; same topology as above):

| | remote (--cold, all actions again) | local in the session | input fetch per remote action | job |
|---|---|---|---|---|
| copy every input | 216.3s elapsed | 165.1s | 3.1s (33.9s / 11) | 20261015T120047873986-6a0d09c2 |
| tree cache (below) | 266.4s | 260.4s | 1.5s (17.0s / 11) | 20261015T125227334862-7f3f3996 |

(The second run's host was busier: local went from 165s to 260s. Compare within a row.)

**Tree cache** (internal/reapi/treecache.go): an input directory of 64+ files or 4MB+ that an
action names the second time with the same digest is materialized once per executor data dir
(`action-trees/`, verified while copied, LRU within 2GB, deleted at startup) and bound
read-only into later exec roots instead of copied; the topmost such directory is taken (a
varying `external/` is not, the SDK under it is). Never a directory that holds an output or
the working dir. Remote is now at parity with local execution for this build on the same
2-CPU host (the executors share its CPUs, so remote can't be faster here); the remaining
1.5s per action is unexplained (smaller copied inputs, the first sighting of each tree).

### Fixes from the review of feat/bazel-sessions

- **Mount order (FixedRoot).** The session's job-writable `/tmp` was bound before the tmpfs
  that hide the server user's home and the data dir; with the data dir under /tmp (every `tun
  serve --ephemeral`) bwrap then created the tmpfs mount point inside the persistent session
  tmp on the host, following a planted symlink. All hiding tmpfs now come first, and a session
  sandbox refuses a mount under an earlier writable one (TestBwrapArgsOrder). Jobs (no
  FixedRoot) keep binding their tree under /tmp when the data dir is there: it is new and
  empty when their sandbox starts.
- **Sweep.** The agent spared whatever pid `server.pid.txt` named, and commands can write that
  file. Now it records the Bazel server (pid and start time) when a command of the sandbox
  first ends with a live pid there, and never reads the file again; if that process is gone
  later, nothing is spared and the server starts a new sandbox for the next command
  (TestBazelSessionSweepForgedPid). Remaining hole: a session's first command (the workspace's
  `tools/bazel` or `.bazeliskrc` can run anything as "bazel") can still name its own process.
- **Identities.** Session ids, per-client Bazel caches and action cache namespaces are keyed by
  `vm:<X-Exedev-Source-Vm>`, `self:<X-Tun-Client>` or `addr:<remote address>`, so a
  self-reported name equal to a proxy-attested one is another client (TestIdentityOf,
  TestBazelSessionRemoteCache). The visible `client` and `tun ls --client` are unchanged.
  Session dirs from before are deleted at startup; old action cache entries are unreachable
  (one cold build per client).

### Deviations and limits

- Runners in all measurements are `tun runner` processes on the same 2-CPU host as the private
  hub (in the same tun job): a private hub in a job can't be reached from other hosts, so I
  could not attach tun-r1..r4 to it. Sleep-based genrules measure the protocol overhead
  correctly; CPU-bound builds can't get faster than local this way.
- Sessions still run only on a hub with local slots (`--local-slots 0` answers 409), though
  they no longer use one when executing remotely.
- `--remote_local_fallback` runs an action in the session when the executors fail; such
  results go to the client's own namespace.
- Real-Bazel tests (TestBazelReal, TestBazelRemoteExecution, reapi's TestBazelCache and
  TestBazelExecution, server's TestBazelRunnerExecution, e2e.sh's Bazel part) skip inside a
  Bazel test unless `TUN_TEST_REAL_BAZEL=1` (bazel-ci.sh sets it): in a session or on an
  executor bazelisk can't download Bazel. `//internal/client:client_test`,
  `//internal/reapi:reapi_test` and `//scripts:e2e_test` stay out of the session benchmark
  (client_test alone runs for minutes; the others still need a Bazel binary as test data).

### Deploying on the shared server

The hub creates `$DATA/runner-token` (0600) at start. Copy it to tun-r1..r4 and restart them
with the new binary and `tun runner --token-file <file>` (or `TUN_RUNNER_TOKEN`): the client
token is refused on runner endpoints, and only a runner that advertises actions (new binary,
bwrap) gets them. Execution needs no flag (it comes with the default `--cache-dir=reapi`);
then every session executes remotely on local slots + runners' slots, and its commands stop
holding a job slot. Existing sessions are deleted at the first start (identities).
