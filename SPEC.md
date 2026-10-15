# tun v0 spec

tun is a remote execution service whose clients are autonomous coding agents. An agent in a
checkout runs `tun run -- <cmd>`; tun ships the working tree to a server, runs the command in an
isolated per-job sandbox, streams stdout/stderr back, and exits with the command's exit status.
Think `bb remote --script`, without Bazel, GitHub, or BuildBuddy.

## Constraints
- Go, **standard library only** (no third-party modules). One binary: `tun`.
- Linux. Server runs as an ordinary user (sudo not required).
- Clients are agents: output must be machine-friendly, errors explicit and actionable, no
  interactive prompts, no TTY assumptions. Help text (`tun help`, `tun run -h`) is the documentation agents read.

## CLI
```
tun serve [--addr 127.0.0.1:7070] [--data DIR] [--max-jobs N] [--max-services N] [--sandbox auto|bwrap|none]
          [--net auto|private|host] [--local-slots N] [--runner-timeout 30s]
          [--ephemeral] [--exit-on-eof]
          [--max-upload 512MB] [--max-workspace 2GB] [--keep-work 1h] [--keep-jobs 168h] [--keep-blobs 24h]
tun run [flags] -- <argv...>        run argv (no shell)
tun run [flags] -c '<shell script>' run via bash -c
    -C DIR          working dir, relative to the workspace root (default: the client's cwd relative to the root)
    -e K=V          set env var (repeatable); -e K copies K from the client's env
    --timeout DUR   default 30m; job is killed (whole process group) when exceeded
    --no-workspace  don't upload anything; run in an empty dir
    --detach        print the job id and exit 0 immediately
    --json          after the job ends, print one JSON object (id, exit_code, duration, ...) to stdout
                    instead of streaming (stdout/stderr then in the object, truncated to a limit)
    --service       long-running service (see Services); tun run exits once it is ready
    --port N        with --service: expose port N (repeatable)
    --ready-timeout DUR  with --service: default 2m
    --tail N        don't stream; after the job ends print only the last N lines of its stdout (to
                    stdout) and stderr (to stderr), then one "tun: job <id>: showed the last a of b
                    stdout lines and c of d stderr lines; full logs: tun logs <id>, tun logs --stderr
                    <id>" line. The opening "tun: job <id> (--tail N: ...)" line says the same.
                    Exit status as without it. Not with --json, --detach or --service. N must be
                    >= 0 (a negative N is a usage error, exit 125; also for tun logs --tail).
tun status <id>   (JSON)          tun logs [-f] [--stderr] [--tail N] <id>       tun cancel <id>
tun ls [--limit N] [--state S[,S...]] [--client NAME | --mine]   (JSON lines: recent jobs, newest first)
tun get [-o DIR] <id> PATH...     download files/dirs/globs from a finished job's work dir
tun runner --hub URL [--token-file F] [--slots N] [--name NAME] [--data DIR] [--sandbox auto|bwrap|none]
           [--net auto|private|host]
tun runners                       (JSON lines: the server's live runners)
```
`tun logs --tail N`: only the last N lines (a final line without a newline counts); with `-f`,
start at the last N lines present when it connects, then follow until the job ends.
`tun run --out PATH` (repeatable): after the job ends (any state), download PATH (relative to the
workspace root; file, dir, or `path.Match` glob) from the job's work dir into the same relative path
of the local workspace root (`tun get`: or under `-o DIR`), replacing existing files (temp + rename),
(the paths are also sent with the job as `outputs`: a job that runs on a runner keeps only those),
keeping modes/mtimes. A PATH matching nothing is a `tun: warning:` on stderr; the exit status stays
the command's. A failed download (size limit, network) turns exit 0 into 125. `tun get` exits 1 if
any PATH matched nothing or was refused, 125 on errors. Local writes go through an `os.Root`, so
neither the stream nor local symlinks can write outside the target dir.
Server address: `$TUN_URL` (default `http://127.0.0.1:7070`). Auth: bearer token, sent as `X-Tun-Token: <token>` (also accepted: `Authorization: Bearer <token>`; some proxies, e.g. exe.dev's, consume Authorization). The server
creates `$DATA/token` on first start (0600) if absent; clients read `$TUN_TOKEN`, else
`~/.config/tun/token`, else `$DATA/token` for the default data dir (`~/.local/share/tun`), so a local
client works with zero config.

For testing, `tun serve --ephemeral` uses a random loopback port and a fresh temporary
data directory, removed on shutdown (including read-only dirs jobs left; a failure to remove
it is reported on stderr). It rejects explicit `--addr` or `--data`. Once ready,
stdout contains exactly two lines: `TUN_URL=http://127.0.0.1:PORT` and `TUN_TOKEN=TOKEN`;
diagnostics go to stderr. SIGINT/SIGTERM stop the server and its jobs.
`--exit-on-eof` also stops it when stdin closes (stdin content is discarded).
`scripts/dev-tun.sh COMMAND [ARG...]` builds the working tree binary, starts this server
(with extra `tun serve` flags from `$DEV_TUN_SERVE_ARGS`, word-split),
exports its credentials and prepends the binary directory to PATH, runs COMMAND, and cleans up.
For example: `tun run -c 'scripts/dev-tun.sh ./scripts/e2e.sh'`.
CI runs this smoke suite by default; set `E2E=0` to skip or `E2E=1` to enable explicitly
(also with `-run` in `TEST_FLAGS`, where it is otherwise skipped). `scripts/ci.sh` splits
`TEST_FLAGS` like xargs: whitespace separates words, `'...'`/`"..."` group and are removed
(`-run TestA|TestB` and `-run "A|B"` both work); nothing is globbed or evaluated.

Exit status of `tun run`: the command's exit code; 128+signal if killed by a signal; 124 on timeout;
125 for tun's own errors (can't reach server, upload failed, bad flags) with a one-line message on
stderr that starts with `tun:`. On SIGINT/SIGTERM the client cancels the job and then exits.

Queueing is visible: if the created job is waiting in the FIFO (`queue_position` set), `tun run`
(not `--json`) prints once to stderr `tun: queued (N ahead, M running)` (N = position - 1, M =
`running` from healthz), polls the job every 500ms, and when it starts prints
`tun: started after Xs in queue` before streaming output. SIGINT/SIGTERM while queued cancel it.
`tun run --json` adds `queue_sec` and `run_sec` to its object.
On completion, non-JSON attached runs (including `--tail`) print a final stderr line:
`tun: job <id> <state> exit=<code> after <run>s (<queue>s queued)` with seconds to three
decimal places and the CLI exit code. Detached runs and ready services have not completed,
so they do not print a completion line.
Non-JSON runs with a workspace also report its compressed tar.gz size in bytes:
`tun: uploaded <size>B workspace` or `tun: workspace <size>B (already on server)`.
The workspace root diagnostic is retained. JSON mode keeps stderr free of these diagnostics.

## Visibility
- Every job object has `queue_sec` (creation to start; to now while queued, to end if it never
  started) and `run_sec` (start to end; to now while running; 0 if never started), in seconds
  with millisecond resolution, computed when served. While queued in the FIFO it also has
  `queue_position` (1 = next to start); omitted otherwise.
- `runner`: where the job runs or ran, set when it leaves the queue: `local` (the server
  itself; always for services) or a runner's name (see Runners). `outputs`: the `--out`
  paths the client declared.
- `client`: who submitted the job, recorded at creation: the `X-Exedev-Source-Vm` request header
  (set by the exe.dev proxy, which callers can't forge through it), else the client-sent
  `X-Tun-Client` (the CLI sends its hostname on every request), else the remote IP. Values are
  stripped to printable non-space ASCII, at most 128 bytes. Without the exe.dev proxy in front
  any token holder can claim any name: it is for visibility, not access control.
- `tun ls --state S` (comma-separated for several), `--client NAME`, `--mine` filter on the
  server before `--limit`. `--mine` means "client = what the server would record for this
  request", so it matches the source VM behind the exe.dev proxy and the hostname otherwise.

## Workspace
- Root = `git rev-parse --show-toplevel` of the client's cwd; if not in git, the cwd itself.
- Files = in git: `git ls-files -z --cached --others --exclude-standard` (tracked + untracked-not-ignored,
  deleted-but-tracked files skipped); not in git: walk the dir, skipping `.git`. Keep file and
  directory modes and symlinks (symlinks stored as links; server refuses links that are absolute or
  resolve outside the root, including via chains of other links in the workspace).
  `.git` itself is not shipped in v0.
- Packed as tar.gz, content addressed by sha256 of the bytes. Client `HEAD /v1/blobs/<sha>`;
  `PUT` only if missing. Size limit (default 512MB, server flag) with a clear error.
- Server extracts a fresh copy per job (never shared between jobs). Reject `..`, absolute paths,
  device files.
  Total unpacked file size is capped (`--max-workspace`, default 2GB): over it, job creation fails
  with 413 and a message naming the flag.

## Execution and isolation
- Each job: `$DATA/jobs/<id>/` containing the server's files `stdout`, `stderr`, `job.json`
  (spec + state), and `sandbox/` with `work/` (the workspace), `home/`, `tmp/`. Only `sandbox/` is
  given to the job; server-owned files are never inside anything the job can write, and the server
  opens them without following symlinks (and refuses hard-linked ones). Job ids: sortable, unique
  (time prefix + random).
- Env: a small base (`PATH`, `LANG`, `TERM=dumb`, `HOME=<job home>`, `TMPDIR=<job tmp>`, `USER`),
  plus shared build caches (`GOCACHE`, `GOMODCACHE`, `XDG_CACHE_HOME` pointing to `$DATA/cache/...`,
  shared across jobs because these tools are safe under concurrency), plus `-e` vars (which win).
  Also `TUN_JOB_ID`.
- Sandbox `bwrap` (default when `bwrap` works, `auto`): root filesystem read-only, the job's
  `sandbox/` dir and shared cache dirs read-write, the job's home/tmp mounted at the same paths as in
  env, `/dev` `/proc` fresh, new pid namespace, `--die-with-parent`; network: see Network isolation
  below. Jobs cannot see or write each other's directories, their own job.json/logs, or the
  server's data dir/token. The job env is set inside the sandbox (`--clearenv`/`--setenv`, passed
  via `--args <fd>`): bwrap itself runs with an empty env, so `-e LD_PRELOAD=...` can't affect the
  unsandboxed bwrap process, and env values don't appear in host `ps`. Shared caches are writable by
  every job, so a job can poison them for later jobs.
- Network isolation (`--net`, default `auto`): `private` gives each bwrap job its own network
  namespace (`bwrap --unshare-net`: only loopback). `auto` uses it when a self-test at startup
  (a real sandboxed helper, see below) works; otherwise it falls back to `host` with a
  `tun: warning:` line naming the error. `private` fails startup if unusable; `host` (and
  sandbox `none`) leave jobs on the host network. The startup line says `net private|host` and
  healthz has `network_isolation`. No runtime dependency beyond bwrap (no slirp4netns/pasta):
  - bwrap's command is a helper: the server's own executable (bound read-only into the
    sandbox from an fd opened at startup, so a replaced binary on disk doesn't matter),
    re-executed as `tun __tun-netns-helper <argv...>`. It reads the job env from a pipe (so
    `LD_PRELOAD` etc. don't act on it), listens on `127.0.0.1:<random>` in the namespace, sets
    `HTTP_PROXY`/`HTTPS_PROXY`/`http_proxy`/`https_proxy` to it and `NO_PROXY`/`no_proxy` to
    `localhost,127.0.0.1,::1` unless the job env has them, runs argv as its child (same
    process group, stdio, cwd), and exits with the child's status (dying of the same signal).
  - Over a socketpair it answers the server with file descriptors (SCM_RIGHTS): the egress
    listener, and TCP connections it dials to `127.0.0.1:<port>` inside the namespace. The
    server never enters the namespace (it can't, unprivileged and multithreaded).
  - Egress: the server accepts on that listener and serves an HTTP proxy: `CONNECT host:port`
    tunnels and absolute-URI requests, dialed from the host network (through the server's own
    `HTTPS_PROXY` if its environment has one, so a tun server inside a tun job works). So
    HTTP(S) works for tools that honor the proxy variables (go, git, curl, pip, npm); raw
    TCP/UDP and DNS lookups from the job don't leave it. The proxy itself doesn't restrict
    destinations (as before, a job can reach anything the server host can, including the
    host's loopback services, but not other jobs: they aren't on the host network).
  - Cost (2-CPU exe.dev VM, `scripts/bench-net.sh`): job startup +4-5ms (`true`: run_sec
    median 2ms host vs 7ms private; client wall time 6ms vs 10ms); a Go build that downloads
    github.com/google/uuid and golang.org/x/text into an empty module cache: 0.95-0.97s either
    way (the proxy hop is noise next to the download).
- `none`: plain subprocess in its own process group (for hosts without bwrap); still per-job dirs,
  but **no isolation at all**: the job runs as the server user with the server's filesystem access,
  so it can read `$DATA/token`, other jobs' workspaces and logs, and rewrite job state. Use it only
  when every job is as trusted as the server operator.
- `--max-jobs` (default NumCPU) jobs run at once on the server (`--local-slots` overrides it;
  runners add their slots, see Runners); the rest queue FIFO (state `queued`).
- Cancel is race-free: a job canceled after it left the queue but before its process started
  ends `canceled` and is never started; a job reaches a terminal state exactly once.
- Timeout and cancel kill the entire process group (SIGTERM, then SIGKILL after 10s).
- State machine: queued -> running -> exited | failed (couldn't start) | timeout | canceled.
  On server restart, jobs left `running`/`queued` are marked `failed` with a reason.
- A client disconnecting does NOT kill the job (agents' tool calls time out; they can `tun logs -f` back).
  Only explicit cancel (or the client's SIGINT handler) does.

## Retention
A sweep at server startup and then periodically (a quarter of the shortest retention, 1s..10m):
- `work/ home/ tmp/` of jobs that ended more than `--keep-work` (1h) ago are deleted; job.json and
  logs stay, and the job object gets `"work_deleted": true`. Status and logs keep working.
- Whole job dirs are deleted `--keep-jobs` (7d) after the job ended; the id then 404s.
- Blobs not referenced by a queued/running job and unused (HEAD/PUT/job creation bump the
  file's mtime) for `--keep-blobs` (24h) are deleted; clients just re-upload.
- A duration of 0 disables that deletion.

## Services
`tun run --service [--port N]... -- <cmd>` starts a job meant to run until canceled (dev server,
database, test fixture):
- No timeout unless `--timeout` is given explicitly (`timeout_sec: 0` = none).
- Not queued and not counted against `--max-jobs` (services are mostly idle and must not starve
  or be starved by builds). Instead at most `--max-services` (default 4) run at once; creating
  another fails with 429 and an explanation, rather than queueing indefinitely.
- Each requested port N is a name: the server allocates a free host port H (kernel-chosen, not
  held by another live service) and sets `TUN_PORT_<N>=H` in the job env, plus `PORT=H` when
  exactly one port was requested (these override `-e`). The service must listen on
  `127.0.0.1:H` (or all interfaces). With network isolation H exists only in the service's
  namespace (the name `host_port` is historical).
- The server probes each port (TCP connect, every 100ms; with isolation through the helper) until
  it accepts and marks the port `ready`. The job object has `service: true` and `ports: [{port, host_port, ready, path}]`; the
  CLI adds `url` (= `$TUN_URL` + path) in `tun run --service`, `tun status` and `tun ls`.
- `tun run --service` waits until the job is running and every port is ready, then prints the job
  id and one `N URL` line per port to stdout (`--json`: the job object) and exits 0. If the job
  ends first, it prints the job's stdout/stderr and exits like `tun run` would (125 if the code was
  0, since a service that exits isn't one). If not ready within `--ready-timeout`, it cancels the
  job, prints its output, exits 125. SIGINT/SIGTERM while waiting cancel it. `--detach` returns the
  id at once without waiting.
- Cancel and server shutdown kill services exactly like other jobs (whole process group; after
  a shutdown the next server marks them failed).
- `--port` without `--service` is an error; at most 16 ports, each 1-65535, no duplicates.
- With network isolation (healthz `network_isolation: true`) the authenticated proxy is the
  only way to a service: other jobs can't connect to its port (it is only in its namespace)
  and a job binding the same port number gets its own port, not the service's traffic.
- **Known limitation without network isolation** (`--net host`, sandbox `none`, or `auto` on a
  host where `bwrap --unshare-net` fails, which `tun serve` warns about): jobs share the host
  network namespace, so any job on the server (any client's) can connect to a service's
  `host_port` directly without the bearer token, and a job could bind a just-allocated port
  before the service does (the proxy would then reach the impostor). The bearer auth only
  protects the proxy path there.

## Runners
The server scales out to other machines with `tun runner`: a runner pulls jobs from the
server (the *hub*), which never opens a connection to it. Clients talk only to the hub; the
client API is unchanged. Job state, logs and outputs live on the hub.
- `tun runner --hub URL` (default `$TUN_URL`): `--slots N` (default NumCPU) jobs at once,
  `--name` (default hostname; unique per hub, it becomes the job's `runner`), `--data DIR`
  (default `~/.local/share/tun-runner`: blob cache, job dirs, shared build caches; a tun serve
  data dir is refused; job dirs left by a previous runner process are deleted at start),
  `--sandbox` and `--net auto|private|host` as for serve: with a private network the runner
  runs the job's network helper and serves its egress proxy itself, so the job's HTTP(S)
  traffic leaves from the runner host (healthz `runners[].network_isolation`). Token: `--token-file`, else `$TUN_TOKEN`, else
  `~/.config/tun/token`. For v1 it is the hub's client token; a separate runner credential
  (that can't submit or read jobs) is future work. On SIGINT/SIGTERM the runner stops taking
  work, kills its jobs and reports them `failed` ("runner N shut down"); a second signal quits
  at once.
- Scheduling: one FIFO; the head job goes to whichever free slot asks first: one of the hub's
  `--local-slots` (default `--max-jobs`; `0` = pure hub that only dispatches) or a runner's.
  Services always run on the hub (not counted in either). healthz `max_jobs` = local slots +
  live runners' slots, `running` counts jobs on both. With `--local-slots 0` the hub doesn't
  unpack workspaces at all; the runner enforces `--max-workspace` (the job then fails instead
  of a 413 at creation). With local slots the hub unpacks at creation as before (and deletes
  its copy once a runner starts the job).
- Protocol (all runner-initiated, bearer auth, `X-Tun-Runner: <name>` on job requests):
  - `POST /v1/runner/poll` `{name, instance, seq, slots, running: [ids], sandbox, network_isolation}` long-polls
    (up to 25s) and is the heartbeat. `instance` is random per runner process; `seq` grows
    with each poll and a poll at or below the highest seen is answered empty and ignored (a
    delayed request must not undo newer state). The answer
    `{jobs: [assignment], cancel: [ids]}`: assignments carry the full spec (argv/script, cwd,
    env, timeout_sec, workspace sha, outputs) and the hub's max_log/max_workspace/max_download;
    `cancel` lists held ids the runner must kill and drop (canceled, ended or unknown on the
    hub, or no longer its). A job assigned while a poll waits is sent at once.
  - An assigned job stays `queued` (with `runner` set, no `queue_position`) until
    `POST /v1/runner/jobs/{id}/started` (idempotent), sent after the workspace is fetched and
    unpacked and right before the command starts. This is the commit point: a job canceled
    before it ends `canceled` and the call is a 410, so the runner never starts it. If a poll
    that arrived after the assignment doesn't list the job, the answer was lost and the job
    goes back to the queue in creation order.
  - `GET /v1/blobs/{sha}` fetches the workspace; the runner verifies the hash and caches it by
    sha (unused for 24h: deleted).
  - `POST /v1/runner/jobs/{id}/stdout?offset=N` (and `/stderr`): a chunk (at most 8MB) that
    starts at byte N of the runner's log. Bytes the hub has are skipped, so retries and
    overlaps are harmless; a chunk past the hub's end is a 409; both answer `{offset}` = the
    hub's size, where the runner resumes. The runner caps logs at the hub's `--max-log` (same
    marker); the hub stops writing a little past it as a backstop.
  - `PUT /v1/runner/jobs/{id}/outputs`: a tar of the declared outputs, then
    `POST /v1/runner/jobs/{id}/done` `{state, exit_code, signal, error, outputs_missing,
    outputs_skipped, outputs_error}` (exited|failed|timeout|canceled; a kill the hub asked for
    decides the state). Logs are all shipped before done. 410 on any job endpoint means the
    hub no longer wants the job: the runner kills and drops it.
- Liveness: a runner that hasn't polled for `--runner-timeout` (default 30s) is lost: its
  running jobs fail ("runner N lost: no poll for Xs"), unstarted assignments are requeued.
  A poll from a new instance under a known name is a restart if it comes from the same source
  (exe.dev source VM, else the hostname the runner sends) or the old instance is overdue: the
  old instance's jobs fail. From another source while the old one is live it is a 409 (name
  clash). A running job that the runner's poll no longer lists fails.
- Timeouts are enforced by the runner (same semantics as local); the hub backstops: a runner
  job still running `timeout + kill grace + --runner-timeout` after it started ends `timeout`
  ("enforced by the hub"), and a cancel not confirmed within `kill grace + --runner-timeout`
  ends as requested. Cancel of a running runner job is delivered via the poll answer
  (the waiting poll is woken).
- Hub restart: jobs a previous hub left `running` on a runner are kept running and adopted
  when that runner polls and lists them (logs resume at the hub's offsets); if no runner
  claims them within `--runner-timeout` they fail as lost. Queued and hub-local jobs fail on
  restart as before.
- Outputs: the hub can't fetch files from a runner later, so `tun run --out` sends its paths
  as `outputs` in the job spec, and when the job ends (any state) the runner collects them from
  the work dir with the files endpoint's rules (os.Root, symlink and special-file rules,
  `--max-download`, entry limit) and uploads one tar. `GET /v1/jobs/{id}/files` for a runner
  job serves from that tar: a requested path must be one of the outputs or inside one (else
  409, naming the declared paths and suggesting `--out PATH`); missing/skipped headers and the
  complete trailer work as for local jobs; a size-limit failure is a 413 with the same
  message. Retention deletes the tar with the work dir (`--keep-work`, then 410). Hub-local
  jobs keep serving any path from the work dir.
- `GET /v1/healthz` adds `local_slots` and `runners: [{name, slots, running, last_seen,
  sandbox, network_isolation, source}]`; `tun runners` prints them as JSON lines.
- Not yet: services on runners; per-runner labels/affinity; reusing a runner's warm
  workspace; a runner-only credential (any token holder can act as a runner, and read any
  blob by sha with `GET /v1/blobs/{sha}`, though every token holder is already fully trusted).

## HTTP API (JSON, all under /v1, bearer auth)
- `HEAD|PUT /v1/blobs/{sha256}` — PUT verifies the hash.
- `GET /v1/blobs/{sha256}` — the blob's bytes (runners fetch workspaces with it).
- `POST /v1/jobs` `{argv|script, cwd, env, timeout_sec, workspace, service, ports, outputs}` -> job object (201).
  `outputs`: at most 256 paths valid for the files endpoint (see Runners).
  `timeout_sec` <= 0 means the default (1800), except for services where 0 (or less) means none; over 604800 (7 days) is a 400. NUL bytes in
  argv/script/cwd/env and env names that are empty or contain `=` are a 400. The body must be
  exactly one JSON object.
- `GET /v1/jobs/{id}` -> job object `{id, state, exit_code, signal, created_at, started_at, ended_at, argv, script, cwd, error, work_deleted, client, runner, outputs, queue_sec, run_sec, queue_position}`.
- `GET /v1/jobs?limit=N&state=S[,S...]&client=NAME&mine=1` -> list, newest first, filtered
  before the limit (`mine=1`: client = this request's client identity; with `client` it's a 400;
  unknown states are a 400). `POST /v1/jobs/{id}/cancel`.
- `GET /v1/jobs/{id}/stdout?offset=N&follow=1` (and `/stderr`): raw bytes from offset; with follow,
  streams (flush as written) until the job ends and all bytes are sent. Reattachable at any offset.
  `tail=N` instead of `offset` starts at the last N lines present at request time. Every 200 has
  `X-Tun-Offset: <start offset>`, so a client can reconnect at start + bytes received.
- `GET /v1/jobs/{id}/files?path=P&path=Q...` -> `application/x-tar` of those paths (dirs recursive)
  relative to the work dir. 409 while the job is not terminal; 400 for absolute paths, `..`, bad
  globs; 413 if regular files total more than `--max-download` (default 1GB). All reads go through an
  `os.Root` on the work dir: symlinks are never followed out of it; links inside the tree are sent as
  links only if relative and lexically inside; special files are left out. Paths that matched nothing
  come back as repeated `X-Tun-Missing` headers, left-out entries as `X-Tun-Skipped: path: reason`
  (values `url.PathEscape`d). The work dir is opened from the server-owned `jobs/` dir down through
  `<id>/sandbox/work` without following a symlink at any level; 410 if it no longer exists.
  Unreadable files are listed as skipped up front; regular files are opened `O_NONBLOCK` and
  re-checked. At most 200k entries (else 413). The trailer `X-Tun-Complete: ok` is set only after
  the whole archive is written (a mid-stream failure aborts the connection); clients require it.
- `ANY /v1/jobs/{id}/ports/{N}/{path...}` (also without the trailing slash): reverse proxy to the
  service's host port for N, path rewritten to `/{path...}` (escaping and query kept). Token auth as
  everywhere; the token is never forwarded: `X-Tun-Token` is dropped, and `Authorization` is
  dropped only if it carries the tun token (so with `X-Tun-Token` a service gets its own
  `Authorization`). Sets `X-Forwarded-For/Host/Proto` and
  `X-Forwarded-Prefix: /v1/jobs/{id}/ports/{N}`. (Without network isolation the service itself
  is not protected from other jobs on the host; see Services.) Responses are flushed as they stream; protocol
  upgrades (websockets) are passed through. 404 if the job has no port N, 409 if the job is not
  running, 502 (JSON error) if nothing accepts on the host port.
- `GET /v1/healthz` (no auth) -> `{ok, sandbox, network_isolation, max_jobs, max_services, running, queued, services, local_slots, runners}`:
  `running` non-service jobs (<= max_jobs), `queued` jobs waiting for a slot, `services` running.
- `/v1/runner/...`: the runner protocol (see Runners).
Errors: JSON `{"error": "..."}` with proper status codes; malformed query parameters (`limit`,
`offset`, `tail` must be plain decimal integers, `follow` a boolean; `offset` with `tail`) are a 400.

## Tests (required)
- Unit tests for workspace packing/unpacking (path safety, modes, symlinks, gitignore handling).
- Integration tests that start a real server in-process on a random port with a temp data dir and
  exercise the client (and in-process runners against a hub without local slots): exit codes, stdout/stderr separation, env, cwd, timeout, cancel, concurrency
  (N jobs run in parallel, isolated dirs), queueing beyond max-jobs, reattach logs, restart recovery.
- Sandbox tests skip (t.Skip) when bwrap is unusable, so the suite passes on hosts without it;
  network isolation tests likewise when `bwrap --unshare-net` with the helper is unusable.
- `scripts/ci.sh` (vet, build, test -race) must pass.

## Non-goals for v0 (later, driven by usage)
Service-to-service networking (with isolation, jobs can't reach each other at all),
raw TCP/UDP egress and DNS from isolated jobs, unauthenticated/public URLs, TLS
termination, multi-host scheduling beyond runners (labels, affinity, services on runners), persistent
workspaces, snapshots, incremental (per-file) upload.
