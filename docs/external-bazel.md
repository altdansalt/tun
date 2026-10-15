# Outside Bazel projects: shared tun customer exercise

Measured 2026-10-15, approximately 11:12–11:20 UTC, from VM `tun-a3`.
All three projects built and tested successfully on deployed shared tun and BuildBuddy.
Warm tun edits took **1.79 / 6.10 / 6.20 seconds**, versus BuildBuddy's
**10.87 / 13.17 / 14.58 seconds** (Go / C++ / Python). The main qualification is
session availability: unchanged tun commands also took **98.75** and **42.54 seconds**
after eviction. The fast path is useful; the shared-service experience is less predictable.

## Method and scope

No tun code or deployment changes. Branch starts at origin/main
`3ec5aef048446fd2097d0723da47d95aaff08a05`. Used `tun` on PATH, deployed `TUN_URL`,
and its automatic remote cache; no private dev server or manually configured cache.
Client binary SHA256: `bf460a28e7a83eabce2e71b95063f2fa04d4e495a8459119ee93a95f34051716`.
BuildBuddy CLI SHA256: `e9e996a1a01555e45433acf9432f9d40d5bda437812a830d6701fe9b09853df9`.

Shallow public clones live under `/home/exedev/ext`, outside tun. Repository pins and
Bazel versions were kept as supplied upstream (GoogleTest has no .bazelversion and
used installed default 9.3.0 on both services). All use bzlmod.

| Kind / public repo | Commit | Bazel | Selected target / toolchain |
|---|---|---|---|
| Go: [bazel-gazelle](https://github.com/bazelbuild/bazel-gazelle) | `97454a6a7c6088327f0d68938d90ce49facf2132` | 8.2.1 | `//v2/label:label_test`; rules_go 0.59.0, downloaded Go 1.24.12 |
| C++: [googletest](https://github.com/google/googletest) | `b66cffcd5f5d6ee34a4f96d57bf016d9a22cb023` | 9.3.0 | `//:gtest_samples`; rules_cc 0.2.22, host C++ compiler |
| Python: [rules_python](https://github.com/bazelbuild/rules_python) | `b7ec5bd151df5f725ab8f76446fd548ac4fedce1` | 9.1.1 | `//tests/pytest_test:pytest_default_test`; downloaded CPython 3.14, pytest, Pyrefly; two shards |

One bounded test target per project keeps work appropriate for the shared 2-CPU/4GB
host. `test` also builds the target and its transitive dependencies; no separate build
sweep or `//...` suite is claimed. This is a development-loop sample, not whole-project
coverage. GoogleTest's sample target compiles 31 sandboxed actions; Gazelle builds its
Go toolchain/stdlib; Python's default aspect analyzes ~26,000 configured targets despite
its tiny smoke test. Python is a rules repository, not a production Python application.

Commands, run from each clone root (substitute the target above):

```sh
tun bazel --cold --timeout 8m test TARGET --jobs=2
tun bazel --timeout 8m test TARGET --jobs=2
# Make the one-line edit below, then repeat the preceding command.
bb remote --remote_runner=grpcs://remote.buildbuddy.dev --timeout=8m test TARGET --jobs=2
# Repeat unchanged, then repeat with exactly the same local edit.
```

BuildBuddy fetched upstream commits and applied local diffs successfully; no fork,
new remote commit, or public push was necessary. Its edit logs explicitly show the
patches applying. Credentials came from the existing environment and are not recorded.
No heavy builds ran on this VM. Initial cold attempts across services overlapped;
tun is shared with other agents, so CPU, queueing and eviction are uncontrolled.

Wall time is client process lifetime (Python monotonic clock; the first attempts used
`/usr/bin/time`). Includes sync, queue, streaming and completion. Tun's BEP duration
excludes queue and workspace apply; the separate job run duration includes workspace
apply. BuildBuddy's Bazel elapsed excludes runner provisioning, checkout, patch and
cleanup. One sample each, with extra no-ops to expose eviction; these are observations,
not medians or a controlled capacity benchmark.

“Cold” means fresh session/output base on tun and the first request for this checkout
on BuildBuddy. Neither service's shared caches were cleared. Gazelle's initial wrong-label
attempt had already fetched some dependencies before the corrected cold run. All
subsequent tun runs used automatic remote caching. No v0 Bazel build was needed; tun
managed session output bases, so none were left in the shared XDG Bazel output cache.

Edits change executable test inputs, rather than adding trailing comments:

- Go: add `{str: "//:a-b", want: Label{Name: "a-b"}},` in `v2/label/label_test.go`'s parse table.
- C++: change `EXPECT_EQ(1, Factorial(-5));` to `EXPECT_EQ(1, Factorial(-6));` in `googletest/samples/sample1_unittest.cc`.
- Python: replace the first `assert True` with `assert __file__.endswith("basic_test.py")` in `tests/pytest_test/basic_test.py`, strengthening the existing bootstrap smoke assertion.

Each edit reran its test successfully on both services. Baseline files were restored
for the unchanged tun measurements if the BuildBuddy edit had run first; final outside
checkouts retain only these one-line diffs. No MODULE, lockfile, rc or version changes.

## End-to-end comparison

Seconds, all passing. Detailed job IDs and exact summary lines follow.

| Project | tun cold | tun warm no-op | tun edit | BB first | BB repeat | BB edit |
|---|---:|---:|---:|---:|---:|---:|
| Gazelle / Go | 290.815 | 0.622* | 1.789 | 139.09 | 73.276 | 10.870 |
| GoogleTest / C++ | 81.62 | 4.291 | 6.100 | 58.372 | 40.487 | 13.171 |
| rules_python | 93.911 | 4.903 | 6.195 | 56.425 | 48.971 | 14.575 |

*Gazelle 0.622s is a final no-op of the edited tree after restoring a warm session.
Its original baseline no-op was submitted while cold was still running: 30.930s wall,
29.072s queued, 1.607s job run, 0.9s BEP duration. This deliberately overlapping request
must not be read as a 31-second no-op execution. The immediate post-edit extra request
was unexpectedly evicted and took 42.535s; the next request waited 3.165s for it, then
a final unqueued repeat gave 0.622s. All samples are retained below.

GoogleTest's first unchanged repeat took 98.751s: 56.867s queue + 41.603s job run,
new sandbox/JVM, 31 remote cache hits. Its subsequent actual warm no-op took 4.291s.
Python's warm no-op includes 2.616s queue (2.164s job run). Gazelle's corrected cold
includes 62.976s queue; Python cold includes 2.493s; C++ cold includes 1.797s.
Do not silently substitute the best warm times for the eviction experience.

## What broke, what was slow, and tun friction

Ranked by impact on an agent's edit/test loop:

1. **Warm sessions disappear under modest shared use.** The server reports a maximum
   of two sessions and queues for idle eviction. C++ no-op job
   `20261015T111454944044-19150eca` waited 56.867s, recreated its sandbox, and took
   98.751s wall. Gazelle job `20261015T111900163919-5fdaa994` recreated its sandbox
   seconds after the prior edit and took 42.535s despite 19 remote cache hits.
   Initial cross-project overlap contributed pressure; other users also ran work.
   This is capacity/placement, not a failed cache: cached actions worked, but JVM,
   extraction and analysis were repaid. **Fix first: session placement with affinity
   across runners and visible retention/eviction reasons, with memory-aware admission.**
   Keep the agreed warm-session → execution direction; merely increasing JVM count on
   the 4GB hub risks memory exhaustion. A normal `tun run` probe landed on tun-r3 with
   ~3.7GB available RAM while all session builds were restricted to the hub.
2. **Cold toolchain/analysis costs are minutes on the hub.** Gazelle cold job
   `20261015T111346772019-53da4337`: Bazel 225.973s, critical path 112.14s, with
   builder compilation and GoStdlib logged at 81s. BB: Bazel 110.334s, critical path
   55.72s. Python cold job `20261015T111712892443-f1b81911`: Bazel 86.900s,
   critical path only 2.12s; ~26K configured targets and Pyrefly come from upstream.
   Downloads of Bazel 8.2.1/9.1.1 and downloaded SDK/repository setup are part of cold
   cost. Attribution among download, extraction, analysis and contention is not fully
   profiled; no network error was observed. Preserve warm state and cache/toolchain
   downloads; then M3 execution can address compilations, but cannot remove analysis.
3. **Passing outputs cannot be retrieved.** After C++ edit job
   `20261015T111658905402-bcf255a5`, `tun get -o /tmp/external-bazel-artifacts ID
   bazel-bin/gtest_samples` returned CLI exit 125 / HTTP 409: outputs are in the
   session output base, which tun get cannot fetch yet. This is documented M1 scope,
   but prevents the next customer step of inspecting/running a produced artifact.
   Implement the planned BEP label/output retrieval and preserve command artifacts
   before another build or eviction overwrites them.
4. **Even genuinely warm no-ops have noticeable, variable overhead.** C++ warm job
   `20261015T111654474361-a7479c6c`: 4.291s wall vs Bazel 2.822s and BEP 4.1s;
   Python warm job `20261015T111852994741-ec8458fc`: 4.903s includes queue;
   final Go no-op `20261015T111942996348-f8e70d23`: 0.622s wall vs Bazel 0.448s.
   Don't attribute every difference to the network: queue, Bazel work and post-command
   handling differ. Instrument these stages before optimizing. BB repeats themselves
   started fresh Bazel servers and took 40–73s, whereas its edits reused warm servers
   (0 packages loaded), so cache state and runner reuse matter more than product names.
5. **Small edits still send the whole manifest twice.** Go's one 1,850B compressed
   blob requires 192,278B manifest traffic; Python's 204B blob requires 213,956B.
   This follows the missing-digest handshake and is visible in the ledger. Measured
   edit sync was 0.156s Go / 0.122s C++ / 0.279s Python, already near the direction's
   300ms overhead target for Python sync alone. Consider incremental manifests or a
   cheaper missing-blob round trip after measuring larger real repositories.

Additional concrete findings:

- Host probe `20261015T111234668894-dbadce8d` on the hub: GCC 13.3.0
  (Ubuntu 13.3.0-6ubuntu2~24.04.1), no clang, no `java` on PATH, python3 present;
  3,909MiB RAM, 2,346MiB available, no swap; 25GB disk, 20GB used, 3.7GB free (85%).
  Bazel's bundled JVM worked, so missing host java was not a failure. The C++ build
  passed using the project's host compiler setup; this is explicitly nonhermetic.
  Compiler parity with BuildBuddy was not measured, so C++ is not a controlled compiler
  throughput comparison.
- Probe `20261015T111629791927-04230363` on runner tun-r3: GCC present, clang absent,
  17GB disk free (28% used), 3,741MiB available RAM. These two probes do not establish
  compiler inventory on every runner. No OOM, ENOSPC or sandbox denial occurred.
  Hub disk headroom is a concern, not evidence of a disk-caused failure.
- rules_python's upstream .bazelrc overrides tun's repository-cache location and adds
  `--disk_cache=~/.cache/bazel/bazel-disk-cache`. Its actions say `disk-cache, remote-cache`.
  Thus automatic remote caching worked, but this repo also kept a redundant session-local
  disk cache. Repository rc precedence matters to storage accounting and reuse; no
  override was removed to improve the comparison.
- Python warned about resolved versions of bazel_features, platforms, protobuf,
  rules_shell and gazelle differing from direct declarations, on both services. Builds
  passed unchanged. `size too big` test warnings also occurred; not failures.
- My first Gazelle label `//label:label_test` was obsolete. Job
  `20261015T111255368637-aed7e8d6` failed normally with a useful Bazel error after
  30.308s run; wall 32.00s (see ledger's recorded timing). Correct label is under
  `//v2/label`. This was operator error, not a tun failure.
- No egress-proxy/DNS/toolchain-download failure occurred in these selected targets.
  Linux sandbox execution worked without flags disabling sandboxing. No shared server
  restart, cache deletion, or deployment was attempted. BB fetched public upstreams
  and applied all three diffs; its checkout/upload bytes are not exposed by this CLI
  output, so they are **not measured**, not zero. Tun sync numbers below are compressed
  manifest + content request payloads, not total network traffic or REAPI cache bytes.

## Limits and next work

Finished three cold/repeat/edit comparisons, additional contention no-ops, artifact
retrieval probe, and host probes. Did not run full suites, repeated statistical trials,
profiles, hermetic C++ toolchains, every runner's inventory, or inspect server cache
occupancy. No local Bazel timing baseline (per the request to avoid heavy VM builds).
The report adds no behavior, so no tun CI/test-code changes are required; validation
is the real remote runs recorded below plus documentation checks.

No change to the agreed direction. The explicit C++ host-toolchain exception and
bounded target scope are exercise limitations, recorded here and linked from
[bazel.md](bazel.md). Prioritize runner session placement/retention, then artifact
access and M3 execution; retain remote cache automation as the default.
