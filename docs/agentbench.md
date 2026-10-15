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

Pending final ledger. This exercise uses the shared deployment as observed, with no
remote-execution flags and no service restart or cache deletion. Bounded selected targets
replace a full-suite run to limit shared machine time; no milestone deviation.
