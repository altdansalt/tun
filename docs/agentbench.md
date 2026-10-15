# Agent edit-to-result benchmark

The success metric is elapsed time from starting an edit to receiving the test result,
including local edit/format, manifest/diff upload, admission, queue, build/test and result
streaming. `scripts/agentbench.sh` generates that workload from an agent VM; it never
runs Bazel locally and changes no server configuration.

## Reproduce

Requires Python 3 (stdlib only), git, gofmt, the deployed `tun` CLI, and optionally `bb`
with its normal credentials (`BUILDBUDDY_API_KEY`, e.g. from `~/bb-token`). Use the VM's
per-client tun token in `~/.config/tun/token` or `TUN_TOKEN`, and `TUN_URL` for the shared
server. Do not copy an admin token onto an agent VM. Tokens are never recorded.

```sh
scripts/agentbench.sh --systems tun --agents 1 3 --iterations 5 --baselines \
  --timeout 480 --budget 2400 --out /tmp/agentbench-tun
scripts/agentbench.sh --systems bb --agents 1 --iterations 5 \
  --timeout 480 --budget 1800 --out /tmp/agentbench-bb

# Literal one-line manifest test case, same targets:
scripts/agentbench.sh --systems tun bb --agents 1 --iterations 5 --one-line \
  --out /tmp/agentbench-one-line

# External profile from docs/external-bazel.md; upstream commit must be accessible to BB.
scripts/agentbench.sh --repo /home/exedev/ext/bazel-gazelle --profile gazelle \
  --systems tun bb --agents 1 --iterations 5 --out /tmp/agentbench-gazelle

# Custom target set (use -- to end option lists):
scripts/agentbench.sh --systems tun --agents 1 --iterations 5 \
  --out /tmp/agentbench-full -- //...

# Harness integration tests, including a real in-process HTTP telemetry server:
tun run -- python3 scripts/agentbench_test.py
```

Only committed HEAD is benchmarked: uncommitted caller edits are deliberately excluded.
Each worker clones locally into a temporary directory, retains the same clone for K
iterations, restores HEAD before each edit and in a finally block, then deletes the clone.
The caller's checkout is untouched. For tun the default targets are
`//internal/workspace/... //internal/server/...`; Gazelle uses `//v2/label:label_test`.
Override with positional targets. Both services use `test ... --jobs=2` with the same
source edits and selected targets. BB fetches the configured public remote and committed
HEAD, then applies the uncommitted edit; push your branch before running BB.
BB also requires the source checkout to be on a named branch; detached source HEAD is
rejected by the harness before launching any agents. Tun needs
neither that push nor a Git server. For tun's own repo, the `public` git remote is used
for BB; for Gazelle, `origin`. Neither remote is changed in the caller checkout.

Edits rotate through workspace archive round-trip, incremental sync, and server identity
regression tests, appending one small executable assertion with fresh input data in an
existing compiled `_test.go` file. Gazelle rotates label parse/string round-trip cases.
These are test-development edits, not production algorithm changes; they compile and
execute changed code and invalidate the selected test result. A per-run nonce prevents
reusing identical edited test results from an earlier benchmark. Edits are replaced,
not accumulated. By default there is no baseline warmup. `--baselines` prepends a fresh-session
command (`tun --cold`) and an unchanged warm command before the K edits. Shared
caches are never cleared; BB first requests may reuse existing runners. Summaries
separate these phases as well as showing the pooled table. `--one-line` uses exactly
one added manifest traversal table row in sync_test.go per edit (tun profile only).

All agents use this VM's one authenticated client identity, with distinct explicit
`--session` names. They share its remote action cache and per-host repository caches;
this does not simulate N independently isolated customer accounts. N workers run loops
independently without think time, starting after their own clone; no per-iteration
barrier artificially holds fast agents for slow agents. N is offered concurrency, not
proof that all N commands are executing at every instant. Unrelated deployment traffic
is uncontrolled. Tun sessions persist until the service's normal idle eviction; the
harness has no session-delete API and does not clear shared caches.

`results.jsonl` starts with metadata, then one object per iteration. Records include
edit path, client wall time, command wall time, exit code, timeout marker, exact tun
summary lines, full final tun job object (`id`, `client`, `runner`, `queue_sec`, `run_sec`),
healthz before/after, raw-log filename, and BB invocation URLs. Telemetry errors are
recorded rather than turned into zeroes. Healthz snapshots expose hub admission counts,
disk, and runner activity/load/memory when the deployment supplies them; they are not
continuous CPU utilization measurements. The current API has no hub CPU-load field.
Raw command logs and `summary.md` sit beside JSONL. Use `tun status ID` and `tun logs
--stderr ID` while retention keeps those jobs.

Percentiles use nearest rank (with five samples p90 equals max). Failures and client
timeouts stay in the table and are counted; their latency is not a passing-result time.
`--timeout` bounds each client process as well as the remote command: on expiry SIGINT
allows cancellation, followed by SIGKILL after 20s. `--budget` stops admissions at an
elapsed deadline and caps the next client wait to remaining time. Cleanup/telemetry and
the cancellation grace can exceed that deadline slightly. Nonzero results or exhausted
budget make the harness exit nonzero. Client cancellation before receiving a job ID
cannot prove whether a remote submission was admitted; inspect the client's jobs before
claiming zero machine work in that case.

## Measurements (2026-10-15)

Runs use tun commit `b806fc89b3cf720062f8a5f8568b71826b309a63` (benchmark only;
production/test source matches origin/main `aacd144`) and Gazelle
`97454a6a7c6088327f0d68938d90ce49facf2132`. Tun uses Bazel 9.3.0; Gazelle retains
upstream's 8.2.1. Builds run remotely, from this agent VM's checkout.

The initial version used the same nonce/input for all agents and N values within one
invocation, so concurrent workers could share compiled edited-test results through their
common client cache. The current script includes N and agent index in input values to
avoid that shortcut. Separate service invocations have different nonce bytes but the
same test operations and payload length. Do not compare these tiny regression cases as
production algorithm rebuild throughput.

### Initial BuildBuddy trial

All samples passed. The first two tun-repo iterations started fresh Bazel servers;
subsequent iterations reused analysis. No caches were cleared. [Raw logs and JSONL](agentbench-data/bb/)
include outer remote-run and inner Bazel invocation URLs. [Gazelle smoke data](agentbench-data/gazelle-bb/)
contains two iterations to verify the external profile; it is not a five-sample trial.

| system | N | samples | failed | p50 s | p90 s | max s | sum wall s |
|---|---:|---:|---:|---:|---:|---:|---:|
| bb | 1 | 5 | 0 | 13.596 | 39.290 | 39.290 | 111.576 |

Gazelle N=1, K=2: p50 12.884s, p90/max 17.144s, sum 30.028s.

| repo | iteration | edit | wall s | BB outer remote-run ID |
|---|---:|---|---:|---|

| tun | 0 | internal/workspace/workspace_test.go | 39.290 | [9466d82f-300a-408c-bd9d-1d951b19b9b9](https://app.buildbuddy.dev/invocation/9466d82f-300a-408c-bd9d-1d951b19b9b9) |
| tun | 1 | internal/workspace/sync_test.go | 35.947 | [d0f93eba-8e2f-4de6-800f-d23b8b16cdd1](https://app.buildbuddy.dev/invocation/d0f93eba-8e2f-4de6-800f-d23b8b16cdd1) |
| tun | 2 | internal/server/identity_test.go | 13.596 | [21c43cd1-90b1-4283-b080-c10bf25e6750](https://app.buildbuddy.dev/invocation/21c43cd1-90b1-4283-b080-c10bf25e6750) |
| tun | 3 | internal/workspace/workspace_test.go | 11.395 | [7219a502-e246-49e6-b41b-7239b8284911](https://app.buildbuddy.dev/invocation/7219a502-e246-49e6-b41b-7239b8284911) |
| tun | 4 | internal/workspace/sync_test.go | 11.349 | [97c02d9d-481f-4e76-ba1b-1866a8d28b8f](https://app.buildbuddy.dev/invocation/97c02d9d-481f-4e76-ba1b-1866a8d28b8f) |
| Gazelle | 0 | v2/label/label_test.go | 17.144 | [4265476d-964f-4059-ad69-eb4da9ca9c80](https://app.buildbuddy.dev/invocation/4265476d-964f-4059-ad69-eb4da9ca9c80) |
| Gazelle | 1 | v2/label/label_test.go | 12.884 | [06dc2343-8c37-46b8-89d1-083d06603d96](https://app.buildbuddy.dev/invocation/06dc2343-8c37-46b8-89d1-083d06603d96) |

### Validation

The three harness tests passed in [74297ff4-a556-4acc-ac07-ef942a04c496](https://app.buildbuddy.dev/invocation/74297ff4-a556-4acc-ac07-ef942a04c496)
([captured log](agentbench-data/harness-test.log)): concurrent clone/edit/reset loops,
a real in-process HTTP telemetry endpoint, token forwarding, distinct sessions, summary
and job capture, nearest-rank percentiles/failure counts, and subprocess timeout cleanup.
The fallback fixture stubs gofmt because BuildBuddy's default PATH lacks it; actual
benchmark edits were gofmt'd on the agent VM and compiled/tested remotely on BB.
An initial harness run failed because the fixture depended on gofmt being installed;
that fixture dependency is fixed. These are script tests, so no new Go server tests or
BUILD entries are needed; no server or CLI code changed.

### Deployment transition diagnostic

The first attempt overlapped the shared rollout. At 14:24–14:35 UTC healthz still
reported local_slots=2, max_sessions=2, and no runner CPU-load/memory fields. Submission
returned HTTP 503 `server draining for restart; retry submission` even though healthz
reported ok=true. A client-only timeout lasted **500.005s** (480s + 20s kill grace),
with no job ID. Its retry eventually ran on the old hub: **334.057s wall**, including
225.729s in the CLI's sync/admission phase, **108.219s job execution**, **0.000s queued**,
exit 0, two tests passed; job `20261015T143620484825-1cebd1ed`.

At 14:38 UTC the endpoint switched to local_slots=0, six one-slot runners and
max_sessions=1 per host, with runner load/memory fields. The next command recreated
the same session on tun-r3; it was canceled deliberately at **75.595s execution**,
**0.177s queued**, job `20261015T143811878781-26f01569`. The mixed-topology trial was
stopped and its local clone restored/deleted before starting the stable trial. These
samples are kept in [rollout diagnostics](agentbench-data/rollout/) and excluded from
the stable table: they cannot establish runner-only concurrency performance.
[Deployment snapshots](agentbench-data/deployment.jsonl) include client binary hashes.
No shared service was restarted, redeployed or reconfigured by this task.

The harness itself also passed through tun on tun-r5: job
`20261015T143811858480-7ec94584`, **3.202s execution**, **0.207s queued**
([log](agentbench-data/harness-test-tun.log)).

### Post-rollout diagnostic

A fresh runner-only attempt queued **161.794s** on the hub, then spent **139.695s**
on tun-r5 before client cancellation at **300.208s wall**:
`20261015T143927562900-c38d30da`. The next command reused its analysis graph but
failed with exit **34**, **8.839s wall**, **0.408s queued**, **7.692s execution**:
`20261015T144428658180-a82403bb`. Its exact diagnostic was:

```text
Failed to query remote execution capabilities: INVALID_ARGUMENT: gRPC requires HTTP/2 POST
```

Neither test ran; this is an infrastructure capabilities handshake failure, not an
assertion failure. The command used tun's automatic cache and no remote-execution flag.
A third command, `20261015T144437849944-26b5707a`, stayed queued with its session
reported as "being stopped" for **299.505s**, executed for **0s**, then was canceled
by the client's deadline. The next request encountered draining again. This long
trial was stopped before K=5 to avoid repeating five-minute waits; it is a diagnostic,
not a completed K=5 trial. [Raw logs and final job objects](agentbench-data/post-rollout/)
preserve both the original observations and final durations (a timeout snapshot can
still show a job running while cancellation completes).

### Bounded N=1/N=3 trial

The canonical trial uses K=5 and a **60s client/remote command timeout**, plus the
20s cancellation grace. It is bounded by a **750s total admission budget**, so the
last attempts may be capped by remaining budget. Same source pin and targets as above.
The matching BB trial uses 60s and a 400s admission budget. Existing service caches
were already warmed by earlier attempts; none were cleared. Every edit remains unique.

```sh
scripts/agentbench.sh --repo /tmp/agentbench-source --systems tun --agents 1 3 \
  --iterations 5 --timeout 60 --budget 750 --out /tmp/agentbench-tun-final
scripts/agentbench.sh --repo /tmp/agentbench-source --systems bb --agents 1 \
  --iterations 5 --timeout 60 --budget 400 --out /tmp/agentbench-bb-60
```

`/tmp/agentbench-source` is a local clone on named branch `feat/agentbench`, pinned to
`b806fc8`; the harness itself is the current script. An extra BB diagnostic used a
detached source checkout and all five local invocations failed before emitting a
remote-run URL. This exposed BB's named-source-branch requirement; the harness now
rejects that input up front and tests the error. [Failed preflight data](agentbench-data/bb-detached-source/).
Another successful 300s-limit repeat is retained in [BB repeat data](agentbench-data/bb-repeat-300/):
p50 12.094s, p90/max 14.898s; it is not pooled with the canonical trial.

The earlier canonical trial was interrupted by the cache rollout; it is superseded
by the cache-enabled trial below. Retained BB-only 60s-limit data is in
[bb-final](agentbench-data/bb-final/). No completed pre-fix tun K=5 result is claimed.

## Cache-enabled deployed trial (2026-10-15, 17:29–17:36 UTC)

**No passing tun edit-to-result measurement was obtained.** Healthz reported
`remote_cache=true`, `local_slots=0`, six one-slot runners and one session per host.
All tun commands failed with Bazel exit 34 before tests could run:

```text
Failed to query remote execution capabilities: UNAVAILABLE: HTTP status code 502
invalid content-type: null
```

This differs from the pre-fix HTTP/2 diagnostic above. Cache stayed enabled for every
command; no shared process, cache or deployment setting was changed. These are observed
failure latencies, never successful test-result times, and establish no tun speedup.
The user-reported deployment/cache-hit success could not be reproduced by this VM.

Tun source pin: `dc480b92d97d447b80e2b287f74fb04807b87d46` (contains main `3d077cd`).
Gazelle pin: `97454a6a7c6088327f0d68938d90ce49facf2132`. Targets are the defaults
above; all commands use `--jobs=2`. Tun uses Bazel 9.3.0; Gazelle uses 8.2.1.
All simulated tun agents authenticate as **tun-a3**, with different session names.
No remote execution flags were used. No milestone deviation or server changes.

“Cold” below means a fresh tun session/output base and the first BB request in that
worker, **with shared caches left intact**. BB can reuse runner/JVM state from other
requests. “Warm” is unchanged input after that command; for tun the preceding command
failed, so it is warm analysis after a failure, not a successful cached-result baseline.
The normal edits rotate compiled regression functions; the separate one-line workload
adds exactly one traversal case to an existing manifest-test table. Edits use fresh
agent-specific data. These measure test development, not production algorithm rebuilds.

| repo/workload | system | N | phase | samples | failed | p50 s | p90 s | max s |
|---|---|---:|---|---:|---:|---:|---:|---:|
| tun | bb | 1 | cold_session | 1 | 0 | 13.994 | 13.994 | 13.994 |
| tun | bb | 1 | warm_unchanged | 1 | 0 | 10.028 | 10.028 | 10.028 |
| tun | bb | 1 | edit | 5 | 0 | 12.708 | 17.649 | 17.649 |
| tun | bb | 3 | cold_session | 3 | 0 | 13.786 | 13.988 | 13.988 |
| tun | bb | 3 | warm_unchanged | 3 | 0 | 11.740 | 14.686 | 14.686 |
| tun | bb | 3 | edit | 15 | 0 | 11.843 | 13.095 | 13.394 |
| tun | tun | 1 | cold_session | 1 | 1 | 25.165 | 25.165 | 25.165 |
| tun | tun | 1 | warm_unchanged | 1 | 1 | 7.114 | 7.114 | 7.114 |
| tun | tun | 1 | edit | 5 | 5 | 7.075 | 7.347 | 7.347 |
| tun | tun | 3 | cold_session | 3 | 3 | 112.542 | 117.845 | 117.845 |
| tun | tun | 3 | warm_unchanged | 3 | 3 | 7.827 | 8.583 | 8.583 |
| tun | tun | 3 | edit | 15 | 15 | 7.747 | 8.174 | 8.192 |
| gazelle | bb | 1 | cold_session | 1 | 0 | 10.990 | 10.990 | 10.990 |
| gazelle | bb | 1 | warm_unchanged | 1 | 0 | 12.225 | 12.225 | 12.225 |
| gazelle | bb | 1 | edit | 5 | 0 | 10.873 | 11.745 | 11.745 |
| gazelle | tun | 1 | cold_session | 1 | 1 | 130.492 | 130.492 | 130.492 |
| gazelle | tun | 1 | warm_unchanged | 1 | 1 | 10.078 | 10.078 | 10.078 |
| gazelle | tun | 1 | edit | 5 | 5 | 8.739 | 10.393 | 10.393 |
| one-line | bb | 1 | edit | 5 | 0 | 10.016 | 10.645 | 10.645 |
| one-line | tun | 1 | edit | 5 | 5 | 7.632 | 48.930 | 48.930 |

Nearest-rank percentiles; five edits per agent, so N=1 edit p90 equals max.
Cold/warm N=1 values are single observations, not stable percentile estimates.

### Reproduction and retained evidence

```sh
# Freeze a named checkout at the pushed pin; do not advance it during the run.
git clone --shared ~/tun /tmp/agentbench-cache-source
git -C /tmp/agentbench-cache-source reset --hard dc480b9
git -C /tmp/agentbench-cache-source remote add public https://github.com/altdansalt/tun.git
scripts/agentbench.sh --repo /tmp/agentbench-cache-source --systems tun --agents 1 3 --iterations 5 --baselines --timeout 600 --budget 2400 --out /tmp/tun-trial
scripts/agentbench.sh --repo /tmp/agentbench-cache-source --systems bb --agents 1 3 --iterations 5 --baselines --timeout 180 --budget 1200 --out /tmp/bb-trial
scripts/agentbench.sh --repo /tmp/agentbench-cache-source --systems tun bb --agents 1 --iterations 5 --one-line --timeout 180 --budget 600 --out /tmp/one-line-trial
scripts/agentbench.sh --repo /home/exedev/ext/bazel-gazelle --profile gazelle --systems tun bb --agents 1 --iterations 5 --baselines --timeout 300 --budget 1200 --out /tmp/gazelle-trial
```

The actual tun trial used the working branch checkout, whose benchmarked HEAD was
pinned by the harness; the fixed clone was used for the corrected BB N=3 and one-line
runs. First BB N=1 was valid. Its following N=3 trial is excluded in full: the earlier
harness left clone branch refs pointing at a later local commit, which BB fetched
instead of the recorded detached HEAD. Ten commands failed fetching an unpushed ref;
eleven later passed after the branch was pushed. This is a harness error, not BB
capacity failure. The fix pins both the named branch and its upstream ref; the entire
corrected N=3 trial passed. Invalid data is retained for accounting/transparency.

Raw logs, JSONL and summaries:

- [Tun N=1/3](agentbench-data/cache-enabled-tun/)
- [BB N=1 and excluded initial N=3](agentbench-data/cache-enabled-bb/)
- [Corrected BB N=3](agentbench-data/cache-enabled-bb-n3-fixed/)
- [One-line edits](agentbench-data/cache-enabled-one-line/)
- [Gazelle](agentbench-data/cache-enabled-gazelle/)
- [All job IDs and BB invocation URLs](agentbench-data/cache-enabled-jobs.md)
- [Deployment snapshot and client binary hashes](agentbench-data/cache-enabled-deployment.json)

### Queue, load and bounded runtime

Tun queue p50/p90/max: **0.039 / 0.145 / 1.192s** across 40 commands; sum 3.380s.
Runner one-minute load observations ranged **0.020–1.800** across before/after snapshots.
Health snapshots are not continuous utilization measurements and the API exposes no
hub CPU load. Hub admission/disk counts and runner load/memory are in every tun row.

Resumed benchmark accounting, including the excluded BB attempt: **101 commands**,
**791.918s (13.20 runner-minutes)** of tun job execution, and **294.767s (4.91 runner-minutes)**
of BB runner log timestamp spans. Combined benchmark execution occupancy: **18.11 machine-minutes**.
Six remote harness validation jobs added **14.993s**, bringing total observed
execution occupancy for this resumed work to **18.36 machine-minutes**
([validation job objects](agentbench-data/cache-enabled-validation-jobs.json)).
BB spans exclude allocation before its first log and are an estimate, not billed CPU time.
Sum of all client waits is **1518.706s (25.31 agent-minutes)**, including upload, queue and
BB provisioning; concurrent waits overlap. Idle session residency and unrelated traffic
are excluded. Tun runner capacity is two CPUs; actual CPU-seconds and BB allocation
are not exposed, so no exact CPU-time claim is made. Each run had the explicit command
and admission limits shown above; no timeout was needed in these resumed trials.

### Validation and tun friction

Four harness tests passed through deployed tun in job `20261015T173349258976-f2941c40`
(2.904s execution, 0.306s queued): concurrency/HTTP telemetry/restoration, subprocess
timeout, percentiles/failures, and literal one-line case generation. The concurrency
fixture also verifies named BB branch/upstream pins. [Captured output](agentbench-data/harness-test-cache-enabled.log).
An initial baseline-fixture run failed because its new diff-stat output displaced the
hash used by the uniqueness assertion; that test-only error was corrected and reruns passed.

- Automatic cache capabilities returned HTTP 502 on every tun request, on multiple
  runners and both repositories. Healthz remained ok with remote_cache=true; it does
  not prove the end-to-end session cache path works. Example job:
  `20261015T172955579690-947fc614` on tun-r5 (24.788s execution, 0.009s queued).
- Fresh-session analysis was much slower than repeats even though neither reached tests.
  N=3 cold requests took 105–118s while repeat failures took about 8s.
- There is no session-delete API; benchmark JVM/output-base residency remains until
  normal idle eviction. Fresh names can evict sessions and perturb shared traffic.
- Hub CPU load and BB CPU/runtime allocation are unavailable, limiting machine-time
  accounting. Queue/job objects and health snapshots are useful and are retained.

All requested bounded K=5 loop shapes and the external profile were exercised; the
passing tun performance comparison remains unavailable because of the deployed cache
failure. Full `//...` and isolated-customer-token trials were not run. No shared cache
was purged to manufacture a cold result.
