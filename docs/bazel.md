# tun v1 direction: Bazel-native remote execution

Status: direction agreed 2026-10-15; M0 done (see M0 results below), the rest is not built yet.
SPEC.md stays the contract for what exists (`tun run`); this file is the target and the order we get there.

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
- **Trust:** shared per-host caches are writable by every job today (v0 limitation); in M1 the
  repository cache is content-checked by Bazel, and the disk cache is per-session until M2/M3 move
  shared results to the executor-written remote cache.
- **Bazel versions:** bazelisk + the repo's `.bazelversion`; installs cached per host.
