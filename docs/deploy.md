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
A proxy 502 after forwarding a submission is still ambiguous; tun does not yet
provide submission idempotency keys.

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
