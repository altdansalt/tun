# Deploying tun

Deploys use bounded draining, not process handover or per-job supervisors.
The hub keeps HTTP live, rejects new job submissions with 503, and finishes all
admitted work, including its FIFO and queued Bazel commands. Runners advertise zero
slots while continuing heartbeats, log shipping and cancellation until their jobs
finish. Both commands accept --drain-timeout (default 15m). Restart latency is the
remaining admitted work, capped by that timeout. Do not drain every runner before
a hub with queued jobs: it needs executor capacity to empty its queue.

For the first rollout, wait for existing old-version processes to become idle before
restarting: replacing their binary cannot retrofit draining into a running old process.

Install the new clients first: they retry connection failures and HTTP 502/503
for up to 16m per request, with backoff and
`tun: server unavailable, retrying`. Log streams resume at their byte offset on
connection errors. POST transport errors are replayed only when the connection
failed during dialing, to avoid duplicating a job with an ambiguous submission.
Job creation sends a random `X-Tun-Request-Id`, reused on retries, so a proxy 502
after forwarding a submission cannot duplicate it. The server persists the key
with the job and returns that job for the same client/key for 24 hours (while the
job is retained), including after restart. Failed admissions do not consume keys.

Build and test through tun, then install with an atomic rename on each target:

```sh
tun run -- ./scripts/ci.sh
tun run --out tun -- go build -o tun ./cmd/tun
# Copy tun to the target as ~/.local/bin/tun.new, then on that target:
mv ~/.local/bin/tun.new ~/.local/bin/tun
systemctl --user restart tun
# On runner hosts, one at a time:
systemctl --user restart tun-runner
```

Before using restart, install this override for **both** user services
(`systemctl --user edit tun` and `systemctl --user edit tun-runner`):

```ini
[Service]
KillMode=mixed
TimeoutStopSec=17min
```

Then `systemctl --user daemon-reload`. KillMode=mixed sends the initial SIGTERM
only to tun, allowing it to drain its sandbox children. The usual control-group
mode would signal every running command at once. TimeoutStopSec must exceed the
configured drain timeout plus shutdown/kill grace. Forced SIGKILL, crashes and
timeouts still fail unfinished local jobs; this design protects orderly deploys.
Long-lived services can hold the drain open until its timeout; cancel or migrate
them explicitly before deploying if that is undesirable.

An active Bazel command finishes before shutdown. Session metadata, workspace and
on-disk output base survive, but the sandbox and JVM stop; the next command
recreates them (a cold JVM with retained disk state). Live session re-adoption is
not implemented. No change to the Bazel-native execution direction is required.

The real-process regression in scripts/e2e-drain.py (part of scripts/e2e.sh)
signals a hub mid-job, checks queued work and a new submission during draining,
restarts on the same data directory, and asserts exact stdout and exit 7. It also
restarts a runner mid-job with a 1s hub heartbeat timeout, verifying the runner
keeps heartbeating and delivers both streams and the original exit status.

Validation on 2026-10-15: remote tun job
`20261015T102812686051-bf3c8046` ran the real-process regression in 9.747s.
With three-second commands, SIGTERM-to-exit measured 2.855s for the hub and
2.723s for the runner (the signal arrives after the first output). Both retained
exact stdout/stderr and exit 7; queued work and a submission retry during the
hub drain also completed.

Full vet/build/race tests and both CLI smoke suites passed in remote tun job
`20261015T103157856778-48c41529` (177.261s, 0.021s queued). That run measured
hub drain 2.863s and runner drain 2.723s, and also checked runner loss, replacement,
network isolation and graceful SIGTERM completion in the existing runner suite.

After merging main's Bazel M0 and automatic cache changes, full vet/build/race CI
and both CLI smoke suites passed in `20261015T104057798547-68955cf4`
(301.511s, 0.116s queued). Hub and runner drain times were 2.863s and 2.691s.
The ambiguous-proxy retry/restart and short-interval heartbeat regressions also
passed in targeted job `20261015T103850336094-4482b18b` (11.230s).

Extra full Bazel validation, `20261015T104058978970-a6b0a516` (353.514s), passed
the BUILD source coverage check and built all targets. Five of eight test targets
passed. The three real-Bazel targets failed on the nested-sandbox download/DNS
limitation already recorded in docs/bazel.md (client TestBazelReal, reapi
TestBazelCache, scripts/e2e_test); the standard CI above passes these real-Bazel
checks. That environment limitation remains unresolved. Bazel setup on this runner
spent about 52s building its Go toolchain helper and 86s building its standard
library before project tests. No Bazel architecture deviation was needed.

## Migrating agent identities

After deploying a server with client tokens, issue one identity per agent VM on the hub:

```sh
tun token --help
tun token --data "$HOME/.local/share/tun" new agent-vm-name
```

Copy the printed secret securely to `~/.config/tun/token` on that agent VM (directory
0700, file 0600). Unset any old `TUN_TOKEN` environment override: it takes precedence over
the file. Verify `tun run --no-workspace -- true` and `tun ls --mine`. Keep the same client
name used previously to preserve session/cache ownership. Deploy clients normally; existing
clients already send tokens and need no protocol change. REAPI users use that same token
in `x-tun-token` or `authorization: Bearer TOKEN` metadata.

The original `$DATA/token` is an admin credential, with unrestricted job access and legacy
header-selected identity. **Sharing this token gives no isolation**, including to clients
already migrated. Remove it from every agent VM and retain it only for trusted operators
and existing runner authentication. Client tokens cannot act as runners. Issue a replacement
token with the same name to rotate, then remove the old `DATA/clients/<sha256(token)>` file
to revoke; issuance/revocation does not require restart. The files contain names, never raw
tokens. Back up this directory with the server data.

For a 25GB hub, the default `--max-shared-cache 6GB` supplements the independent
`--cache-max-size` REAPI budget and `--min-free` reserve. Account for both budgets, session
output bases and retained jobs when configuring disk. Tool cache trimming protects files
modified in the last hour and all live session clients' Bazel caches, so caps are soft.
It runs periodically and before disk-pressure retention; inspect `tun: trimmed ...` logs.

Extracted Go modules are evicted as whole directories, only when no local job/service is
running; individual module source files are never trimmed. Go download-cache entries and
other tool cache files use the ordinary age grace.
