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
scripts/agentbench.sh --systems tun --agents 1 3 --iterations 5 \
  --timeout 480 --budget 2400 --out /tmp/agentbench-tun
scripts/agentbench.sh --systems bb --agents 1 --iterations 5 \
  --timeout 480 --budget 1800 --out /tmp/agentbench-bb

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
HEAD, then applies the uncommitted edit; push your branch before running BB. Tun needs
neither that push nor a Git server. For tun's own repo, the `public` git remote is used
for BB; for Gazelle, `origin`. Neither remote is changed in the caller checkout.

Edits rotate through workspace archive round-trip, incremental sync, and server identity
regression tests, appending one small executable assertion with fresh input data in an
existing compiled `_test.go` file. Gazelle rotates label parse/string round-trip cases.
These are test-development edits, not production algorithm changes; they compile and
execute changed code and invalidate the selected test result. A per-run nonce prevents
reusing identical edited test results from an earlier benchmark. Edits are replaced,
not accumulated. There is no baseline warmup: first iterations include cold JVM/analysis
and cache state as encountered; all iterations count in the headline table.

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

### BuildBuddy

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

### Tun measurement ledger

Pending bounded shared-deployment runs.
