# tun v1 direction: Bazel-native remote execution

Status: direction agreed 2026-10-15. Built: M0 (tun builds and tests with Bazel; see "M0 results"),
the M1 core (`tun bazel` sessions, manifest sync, BEP summary on the hub; see "M1 status") and
tun's REAPI cache. SPEC.md stays the contract for what exists; this file is the target and the order.

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
  remote-apis and ByteStream protos. Execution remains M3 work.
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

## Orderly deployment
Hub/runner deploys now drain admitted work (default 15m); see [deploy.md](deploy.md).
Queued Bazel commands finish before shutdown. Session disk state survives, but the
sandbox/JVM is recreated on the next command. Live sandbox re-adoption remains future
work; this does not change the milestone direction.
