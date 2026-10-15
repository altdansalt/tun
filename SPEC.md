# tun v0 spec

> Direction for v1 (Bazel-native remote execution): docs/bazel.md. This file describes what exists.

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
          [--ephemeral] [--exit-on-eof] [--drain-timeout 15m]
          [--cache-dir reapi] [--cache-max-size 10GB] [--cache-action-writes=true] [--cache-socket PATH]
          [--max-upload 512MB] [--max-workspace 2GB] [--keep-work 1h] [--keep-jobs 168h] [--keep-blobs 24h]
          [--min-free 10%] [--max-shared-cache 5GB] [--max-client-cache 1GB] [--max-sessions 2] [--session-idle 30m] [--bazelisk-seed DIR]
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
                    Every 30s while running, stderr reports elapsed seconds, stdout/stderr line
                    counts, and a tun logs command. Exit status as without it.
                    Not with --json, --detach or --service. N must be
                    >= 0 (a negative N is a usage error, exit 125; also for tun logs --tail).
tun status <id>   (JSON)          tun logs [-f] [--stderr] [--tail N] <id>       tun cancel <id>
tun ls [--limit N] [--state S[,S...]] [--client NAME | --mine]   (JSON lines: recent jobs, newest first)
tun get [-o DIR] <id> PATH...     download files/dirs/globs from a finished job's work dir
tun runner --hub URL [--token-file F] [--slots N] [--name NAME] [--data DIR] [--sandbox auto|bwrap|none]
           [--net auto|private|host] [--min-free 10%] [--drain-timeout 15m]
tun token [--data DIR] new NAME   issue a client token on the server host
tun runners                       (JSON lines: the server's live runners)
tun bazel [--session NAME] [--cold] [--timeout DUR] [--json] <bazel command> [args...]   (see Bazel sessions)
tun sessions [--client NAME | --mine]   (JSON lines: the server's Bazel sessions)
tun serve ... [--max-sessions 2] [--session-idle 30m]
```
`tun logs --tail N`: only the last N lines (a final line without a newline counts); with `-f`,
start at the last N lines present when it connects, then follow until the job ends.
`tun run --out PATH` (repeatable; with `--detach` it only declares the outputs, for a later
`tun get`): after the job ends (any state), download PATH (relative to the
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
diagnostics go to stderr. SIGINT/SIGTERM drain admitted jobs (including queued commands) for up to --drain-timeout
(default 15m), rejecting new submissions with retryable 503s; then stop the server.
See docs/deploy.md for required systemd settings and session restart behavior.
`--exit-on-eof` also stops it when stdin closes (stdin content is discarded).
`scripts/dev-tun.sh COMMAND [ARG...]` builds the working tree binary, starts this server
(with extra `tun serve` flags from `$DEV_TUN_SERVE_ARGS`, word-split),
exports its credentials and prepends the binary directory to PATH, runs COMMAND, and cleans up.
For example: `tun run -c 'scripts/dev-tun.sh ./scripts/e2e.sh'`.
CI runs this smoke suite by default; set `E2E=0` to skip or `E2E=1` to enable explicitly
(also with `-run` in `TEST_FLAGS`, where it is otherwise skipped). `scripts/ci.sh` splits
`TEST_FLAGS` like xargs: whitespace separates words, `'...'`/`"..."` group and are removed
(`-run TestA|TestB` and `-run "A|B"` both work); nothing is globbed or evaluated. `PKGS`
(whitespace-separated, default `./...`) selects the packages to vet, build and test; with it
the smoke suites run only with `E2E=1`.

Exit status of `tun run`: the command's exit code; 128+signal if killed by a signal; 124 on timeout;
125 for tun's own errors (can't reach server, upload failed, bad flags) with a one-line message on
stderr that starts with `tun:`. On SIGINT/SIGTERM the client cancels the job and then exits.

Queueing is visible: if the created job is waiting in the FIFO (`queue_position` set), `tun run`
(not `--json`) prints once to stderr `tun: queued (N ahead, M running)` (N = position - 1, M =
`running` from healthz), polls the job every 500ms, and when it starts prints
`tun: started after Xs in queue` before streaming output. Non-JSON attached runs then print
where the job runs, once the server decided: `tun: running on hub` (the server itself) or
`tun: running on runner NAME` (also `tun bazel`, with session affinity). SIGINT/SIGTERM while queued cancel it.
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
- `client`: the name bound to the authenticated client token. Headers cannot override it.
  Run `tun token [--data DIR] new NAME` on the server host to issue a 256-bit random token.
  Only its SHA256 hash is stored, as a 0600 file under `$DATA/clients/` containing the name.
  Tokens work immediately; deleting a hash file revokes that token without restart. Several
  tokens can name the same client (useful for rotation); names are 1-128 printable non-space
  ASCII characters. Keep names unique per trust boundary.
- The original `$DATA/token` remains an **admin** token for compatibility. It can access every
  job and select any client identity through `X-Exedev-Source-Vm`, then `X-Tun-Client`, then
  remote IP, as before. **This mode gives no client isolation.** Keep it off agent VMs once
  migrated. Runner authentication is unchanged; client tokens cannot call runner endpoints.
- All authenticated clients can list jobs and read job status/metadata, useful for spotting
  load; another client's `argv`, `script` and `workspace` are omitted (commands often carry
  secrets; the workspace digest names its content). Logs, files/outputs, BEP results, service
  proxying and cancellation require the owner or admin. Sessions listed to client tokens are
  restricted to that client; session identity and reuse always include the authenticated client.
- Uploaded content is per uploader. A digest is not a capability (it leaks through metadata
  or logs, and low-entropy content such as a config file with a short secret can be guessed
  and confirmed), so proof of possession is uploading the bytes. A workspace blob (`PUT
  /v1/blobs/{sha}`) or tun bazel file content (`POST /v1/cas`) is stored once, and each client
  that uploaded it is recorded as an owner (a hard link under `$DATA/owners/`, removed with
  the content by retention). For a client token, `HEAD /v1/blobs/{sha}` is 200 only for its
  own uploads, `POST /v1/jobs` with another client's workspace says it is not uploaded, and a
  tun bazel manifest's digests the client did not upload are reported missing (409), so the
  CLI uploads them; nothing reveals whether another client has the content. Content already
  on the server costs each further client one upload, and content uploaded before this
  change one re-upload. Admin requests may use any content; `GET /v1/blobs/{sha}` is admin
  (runner) only.
- `tun ls --state S` (comma-separated), `--client NAME`, `--mine` filter before `--limit`.
  `--mine` uses the token's client name, or the legacy header identity for admin requests.
- REAPI TCP and authenticated Unix endpoints use the same tokens, via `x-tun-token` or
  `authorization: Bearer TOKEN` metadata. Action-cache writes and lookups use the token's
  client namespace regardless of headers or `instance_name`. Session sockets keep their
  fixed client identity. The REAPI CAS is shared and verified by digest: unlike tun's own
  uploads it is **not** per uploader, so any token holder can test whether a digest exists
  (FindMissingBlobs) and read content by digest. Clients cannot learn each other's digests
  from tun (action results, BEP and job details are per client), but low-entropy content
  whose digest can be guessed is not confidential there (see Known gaps).

### Tool cache budgets

Tool caches are trimmed on each GC sweep (at most ten minutes apart) and, under `--min-free`
pressure, before finished workspaces and idle sessions are deleted (internal/server/
sharedcache.go has the details):
- `--max-shared-cache` (default 5GB; 0: none) bounds `$DATA/cache/{go-build,gomod,xdg}`
  together, except Bazel disk caches in them (`xdg/tun-bazel/disk`, which bazel-ci.sh bounds
  with Bazel's own `--experimental_disk_cache_gc_max_size`).
- `--max-client-cache` (default 1GB; 0: none) bounds each client's Bazel repository cache
  (`$DATA/bazel/<client>/repo`). Its disk cache (used only without the REAPI cache) has
  Bazel's own GC (5G); its `bazelisk/` (Bazel binaries) is never trimmed, and is re-seeded
  from `--bazelisk-seed` whenever its downloads are missing.
- Tools trust whole trees, so trimming evicts what a tool can lose without being misled:
  single go-build entries; an extracted Go module together with its `cache/download` files
  (download files are never trimmed alone: a missing `.ziphash` makes go re-extract a module
  others may be compiling from, a missing `.lock` breaks locking); a whole top-level dir of
  `xdg` (a tool's install); a whole Bazel repository cache entry
  (`content_addressable/<algo>/<hash>`, or `contents/<hash>`: fetched repository trees).
  Trees in the shared caches are only evicted while no local job or service runs, and a
  client's Bazel caches only while it has no live session (checked under the admission lock,
  so none starts until the tree is renamed away); a busy server trims them only between jobs.
- Least recently used first, approximated by the newest mtime in the unit (dirs count: an
  extracted archive keeps old file mtimes). Future mtimes are reset to now. Grace: an hour
  since the last change, and for go-build entries Go's own 5 days + 1 hour (Go refreshes a
  used entry's mtime at most hourly), except under disk pressure (an hour). Under pressure
  Bazel disk cache files are candidates too.
- Soft caps: reads need not update mtimes, and protected or unremovable entries can keep
  usage over budget. Trimming never follows symlinks out of a cache root. Runners do not
  trim tool caches yet.

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

Disk space: when free space on the data dir's filesystem is under `--min-free` (bytes with
KB/MB/GB, or a percentage of the filesystem; default 10%; 0 disables), the server deletes
finished jobs' work dirs early, oldest first (as `--keep-work` would; `work_deleted: true`),
until it is above again, then idle Bazel sessions, least recently used first. It checks
before each job starts, before it unpacks a workspace, and every minute; it logs what it
deleted. healthz `disk: {free_bytes, total_bytes, min_free_bytes, low, reclaimed_work_dirs,
reclaimed_sessions}` (`low`: still under the minimum). A runner (whose job dirs are deleted
when the job ends) does the same with its cached workspace blobs, least recently used first,
before each poll (`tun runner --min-free`, default 10%), and reports its `disk` (with
`reclaimed_blobs`) in polls; healthz shows it as `runners[].disk`.

Logs: if writing a job's stdout or stderr log fails (e.g. the disk is full), the rest of that
stream is discarded (the job never blocks on it) and the job's `error` says so; `tun run`
prints it ("tun: job ID exited; server error: ...").

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
  work, finishes its jobs up to --drain-timeout (default 15m) and tells the hub it is
  leaving (`POST /v1/runner/leave {name, instance}`: its slots disappear at once); a second
  signal quits at once. A runner flocks its data dir: a second runner on the same dir refuses
  to start. All its hub requests have timeouts (2 minutes; polls 90s).
- Scheduling: one FIFO; the head job goes to whichever free slot asks first: one of the hub's
  `--local-slots` (default `--max-jobs`; `0` = pure hub that only dispatches) or a runner's.
  Services always run on the hub (not counted in either). healthz `max_jobs` = local slots +
  live runners' slots, `running` counts jobs on both. With `--local-slots 0` the hub doesn't
  unpack workspaces at all; the runner enforces `--max-workspace` (the job then fails instead
  of a 413 at creation). With local slots the hub unpacks at creation as before (and deletes
  its copy once a runner starts the job).
- Protocol (all runner-initiated, bearer auth, `X-Tun-Runner: <name>` and
  `X-Tun-Runner-Instance: <instance>` on job requests; a job is only touched by the instance
  it was assigned to):
  - `POST /v1/runner/poll` `{name, instance, previous, seq, slots, running: [ids], sandbox, network_isolation}` long-polls
    (up to 25s) and is the heartbeat. `instance` is random per runner process; `seq` grows
    with each poll and a poll at or below the highest seen is answered empty and ignored (a
    delayed request must not undo newer state). The answer
    `{jobs: [assignment], cancel: [ids], superseded}`: assignments carry the full spec (argv/script, cwd,
    env, timeout_sec, workspace sha, outputs) and the hub's max_log/max_workspace/max_download;
    `cancel` lists held ids the runner must kill and drop (canceled, ended or unknown on the
    hub, or no longer its). A job assigned while a poll waits is sent at once.
  - An assigned job stays `queued` (with `runner` set, no `queue_position`) until
    `POST /v1/runner/jobs/{id}/started` (idempotent), sent after the workspace is fetched and
    unpacked and right before the command starts. This is the commit point: a job canceled
    before it ends `canceled` and the call is a 410, so the runner never starts it. If a poll
    that arrived after the assignment doesn't list the job, the answer was lost and the job
    goes back to the queue in creation order, and so does an assignment not started within
    10 minutes (the runner can't fetch or unpack it). The check and the transition are one
    critical section on the hub, as for done.
  - `GET /v1/blobs/{sha}` fetches the workspace; the runner verifies the hash and caches it by
    sha (unused for 24h: deleted).
  - `POST /v1/runner/jobs/{id}/stdout?offset=N` (and `/stderr`): a chunk (at most 8MB) that
    starts at byte N of the runner's log. Bytes the hub has are skipped, so retries and
    overlaps are harmless; a chunk past the hub's end is a 409; both answer `{offset}` = the
    hub's size, where the runner resumes. The runner caps logs at the hub's `--max-log` (same
    marker); the hub stops writing a little past it as a backstop.
  - `POST /v1/runner/jobs/{id}/exited` `{state, exit_code, signal, error}` as soon as the
    command has ended, before the rest of the logs and the outputs are shipped: from then on
    the hub's timeout backstop doesn't apply, and if the runner is lost before done the job
    ends with this result (its error notes that logs/outputs may be incomplete).
  - `PUT /v1/runner/jobs/{id}/outputs`: a tar of the declared outputs, then
    `POST /v1/runner/jobs/{id}/done` `{state, exit_code, signal, error, outputs_missing,
    outputs_skipped, outputs_error}` (exited|failed|timeout|canceled; a kill the hub asked for
    decides the state). Logs are all shipped before done. 410 on any job endpoint means the
    hub no longer wants the job: the runner kills and drops it.
- Liveness: a runner that hasn't polled for `--runner-timeout` (default 30s) is lost: its
  running jobs fail ("runner N lost: no poll for Xs"), unstarted assignments are requeued.
  The newest instance of a name wins. A poll from a new instance takes the name over: if the
  old instance is overdue, or the new one's `previous` (the instance that last held its data
  dir, read from the dir at start) names it, the old instance is gone and its jobs fail at
  once ("runner N restarted"). Otherwise the old instance is *retired*: its unstarted jobs
  are requeued, it gets no more work, and its polls (still its heartbeat) are answered with
  `superseded` set; it finishes and reports the jobs it holds, then exits (status 1). So two
  live runners with one name don't fight: the older one drains. Polls from retired instances
  never touch the current one (retired ids are remembered for an hour after their last job),
  and within one instance `seq` is checked before anything is updated. A running job that
  its instance's poll no longer lists fails.
- Timeouts are enforced by the runner (same semantics as local); the hub backstops a live
  runner that never reports the end: a job whose runner hasn't said `exited` by `timeout +
  kill grace + 5 minutes` after it started ends `timeout` ("enforced by the hub"), and a
  cancel not confirmed within `kill grace + 5 minutes` ends as requested. (A dead runner is
  handled by liveness, within `--runner-timeout`.) Cancel of a running runner job is delivered via the poll answer
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
  409, naming the declared paths and suggesting `--out PATH`, also with `--detach`); missing/skipped headers and the
  complete trailer work as for local jobs; a size-limit failure is a 413 with the same
  message. Retention deletes the tar with the work dir (`--keep-work`, then 410). Hub-local
  jobs keep serving any path from the work dir.
- `GET /v1/healthz` adds `local_slots` and `runners: [{name, slots, running, last_seen,
  sandbox, network_isolation, source}]`; `tun runners` prints them as JSON lines.
- Not yet: services on runners; per-runner labels/affinity; reusing a runner's warm
  workspace; a runner-only credential (the legacy admin credential still authenticates runners and can
  read blobs by sha with `GET /v1/blobs/{sha}`; client tokens cannot).

## Bazel sessions (tun bazel)
First step of docs/bazel.md (M1, hub-local only). `tun bazel [--session NAME] [--cold]
[--timeout DUR] [--json] <bazel command> [args...]` runs `bazel <command> [args...]` in a warm session
on the server and streams its output; the exit status is Bazel's (124 on timeout, 125 for
tun's errors). The agent's args pass through unchanged; anything flag-like before the bazel
command other than tun's flags (a Bazel startup option) is a usage error.
- **Session** = (client, workspace root path on the client, name; default `default`), id =
  first 20 hex of sha256(client NUL root NUL name). It lives in `$DATA/sessions/<id>/`:
  `session.json`, `sync.json` (the last applied manifest plus the stat of each file tun
  wrote), `tun.bazelrc`, `agent.log` (the sandbox's own output: the server copies it from a
  pipe, capped at `--max-log` and at most 8MB, so the sandbox never holds a descriptor of a
  server-owned file), and `sandbox/{work,home,tmp,out}`.
- **Fixed paths.** Under bwrap the session sandbox has an empty root with the host's
  top-level directories bound read-only (so new top-level paths can exist), and binds
  `work` at `/work`, `home` at `/tun/home` (HOME), `tmp` at `/tmp` (TMPDIR), `out` at
  `/tun/out` (Bazel's `--output_user_root`, output base `/tun/out/base`), and the client's
  own Bazel caches from `$DATA/bazel/<sha256(client)[:16 hex]>/`: `repo` at `/tun/cache/repo`
  (`--repository_cache`; Bazel 8+ keep the repository contents cache, fetched repository
  trees, in its `contents/` by default), `disk` at `/tun/cache/disk` (`--disk_cache`),
  `bazelisk` at `/tun/cache/bazelisk` (BAZELISK_HOME; a new one is seeded with a copy of
  `--bazelisk-seed DIR`'s `downloads/` if given), `tun.bazelrc` read-only at `/tun/tun.bazelrc`,
  the tun executable at `/tun/bin/tun`, and with a remote cache the session's `sock/` dir
  read-only at `/tun/sock` (see Bazel remote cache). The server user's home and the data dir are hidden as
  for jobs; with network isolation the session has its own network namespace and egress
  proxy. With sandbox `none` the same dirs are used at their host paths. The root is
  remounted read-only once everything is mounted. Every bind source of a job or session
  sandbox (its tree, tmp, the shared caches, the session mounts) is opened one path component
  at a time without following symlinks, must be a directory owned by the server user (or a
  regular file, read-only) and is handed to bwrap as a descriptor (`--bind-fd`), so a symlink
  planted where a sandbox can write never redirects a mount. The caches are per client
  because none can be trusted once a sandbox wrote it (bazelisk runs a cached Bazel without
  checking it again; Bazel trusts its repository contents and disk caches): a client can
  only poison its own. Clients are told apart by the identity in `client` (see visibility):
  behind exe.dev's proxy it can't be forged; otherwise it is self-reported.
- **Long-lived sandbox.** The session sandbox's first process is an agent
  (`tun __tun-session-agent`, under the network helper when isolated) that runs one command
  at a time on request over a socketpair (fd 7): the server sends a `run` message with the
  job's stdout/stderr pipes and the read end of a pipe (SCM_RIGHTS) that then carries argv and
  dir (so argv may be larger than a socket buffer; up to 16MB); the agent starts the command
  in its own process group, reports `started` and later the exit code or signal, delivers
  `signal` requests to the group, and kills stragglers in the group when the command ends.
  Under bwrap it then also kills every other process left in the sandbox (setsid'd or
  daemonized leftovers) except itself, its ancestors and the Bazel server (the pid in
  `out/base/server/server.pid.txt`) with its descendants, so nothing a command started can
  read or forge a later command's output or modify `/work` during a sync. (A command can
  still protect a process by writing its pid there; without bwrap there is no sweep.) Bazel's
  server daemonizes into its own session inside the sandbox (its pid and network namespace),
  so it survives
  between commands. The sandbox is started by the first command of a session (output: "tun:
  session N: started a new session sandbox ...") and lives until the session is stopped or
  the server exits (`--die-with-parent`). Before starting a bwrap sandbox for an existing
  session, `out/base/server` (pid/port files of a server in a dead pid namespace) is removed.
- **Command:** `bazel --output_user_root=<out> --output_base=<out>/base --bazelrc=<rc>
  --max_idle_secs=<session-idle + 5m> <args...>` in `<work>/<cwd>`, with env PATH, LANG,
  TERM=dumb, USER, HOME, TMPDIR, BAZELISK_HOME, TUN_SESSION (constant per session, so Bazel's
  client env doesn't change between commands). `tun.bazelrc` (rewritten before each command)
  has `common --repository_cache=...` and either (no remote cache) `common --disk_cache=...`,
  `common --experimental_disk_cache_gc_max_size=5G`, or (remote cache, no disk cache: it would
  be a second copy on the same host) `common --remote_cache=grpc://tun`, `common --remote_proxy=unix:/tun/sock/cache.sock`, `common
  --experimental_build_event_upload_strategy=local` (BEP keeps local file URIs) and, if the
  server denies action writes, `common --noremote_upload_local_results`; it is the "user" rc,
  so the workspace's own `.bazelrc` still applies, and an agent's flags (e.g.
  `--remote_cache=`) override it. The first command of a sandbox says "started a new session
  sandbox ...; remote cache: tun's, through /tun/sock/cache.sock" (or why there is none).
- **Jobs.** Each command is a job (`POST /v1/jobs` with `bazel`, see the API) in the normal
  FIFO; it needs a slot on its session host (hub or runner). New sessions are placed by
  free job slots first, then available memory, CPU-normalized load, free disk and per-host session capacity; existing sessions retain
  affinity to one host instance, even while its slots are busy. A pure hub queues until a
  session-capable runner is available. It also needs its session: a session runs one command at a time, so the
  scheduler skips a bazel job whose session is busy (later jobs may start first) and sets
  `queue_reason` on it (tun prints it with the queued line). The job object has `session`,
  `session_id`, and `argv` = `["bazel", args...]`. Once it has a slot and its session, the job
  is `running`: with `--cold` (or `cold: true`) the session's sandbox is stopped and its dir
  deleted first ("tun: --cold: discarded ..."); then the manifest is applied (stderr: "tun:
  session N (id): synced workspace in Xs: a added, u updated, d deleted, k unchanged (BB
  written)"); then the command runs in the session sandbox. Timeout and cancel send SIGINT to
  the bazel client (Bazel cancels the command), SIGKILL after the kill grace, and if the
  command still hasn't ended after twice the grace, kill the session sandbox. A server
  orderly shutdown finishes admitted commands before killing the sandboxes; after a crash
  the next server marks running bazel jobs failed. `tun get` of
  a bazel job is a 409 (outputs stay in the session's output base).
- **Sync.** The client lists the same files as for `tun run` (minus top-level `bazel-*`
  symlinks: Bazel's convenience links) and builds a manifest: entries `{p, t: f|d|l, m (perm
  bits), s (sha256), n (size), l (link target)}`, each parent directory listed before its
  contents. File digests are cached in `$XDG_CACHE_HOME/tun` (else `~/.cache/tun`) by size,
  mtime, ctime and inode (files modified in the last 2s are always rehashed). The job request
  is sent gzip-compressed (`Content-Encoding: gzip`). If the server lacks some contents it
  answers 409 `{error, missing: [sha...]}`; the client uploads them with `POST /v1/cas` (a
  gzip-compressed tar, entries named by sha256, batches of at most 64MB) and creates the job
  again (up to 3 times). The client prints "tun: sync: N manifest entries (XB sent), uploaded
  k of n files (YB sent) in Zs". The server validates the manifest (clean relative paths, no
  `..`, parents listed as directories, no duplicates, sha256 digests, permission bits only,
  links relative and lexically inside; total file size under `--max-workspace`, else 413),
  and when the job starts applies it in place through an `os.Root`: paths of the previous
  manifest that are gone are deleted (directories only if empty); a file whose sha256, mode,
  size, mtime, ctime and inode match what tun last wrote is left alone (its mtime stays, so Bazel's
  and other stat-based caches stay valid); other files are written to a temp file and renamed
  over; paths tun never wrote (e.g. `bazel-*` links) are left alone unless the manifest needs
  them; links are checked against the final tree (chains that leave it are removed and fail
  the job); directory modes are applied last.
- **Results.** For `build`, `test`, `coverage` and `run`, tun inserts
  `--build_event_json_file=<out>/tun-bep.json` right after the command (any earlier file is
  removed first). When Bazel exits the server reads it (through an `os.Root`, no links,
  regular file, at most 256MB) with `internal/bep`, sets Bazel's exit status as the result,
  appends one line to the job's stderr (`tun: bazel test exit=3 after 12.4s: 2 of 40 tests
  failed: //a:b_test //c:d_test`; labels capped at 20; a BEP read error is appended in
  parentheses) and records it as the job's `summary`. For every command whose Bazel ran,
  the bounded summary object (`bep.Summary.JSON`: command, exit_code, exit_name, success,
  complete, duration_sec, targets, tests, failed_actions, counts, omitted) is stored as
  `jobs/<id>/bep.json` and served by `GET /v1/jobs/{id}/bep` (404 without one). `tun bazel
  --json` sends Bazel's stdout to stderr and prints that object on stdout, adding `job`,
  `state` and `error` (only those, plus `exit_code`, when the command never ran).
- **Lifetime.** A session is deleted (Bazel `shutdown` through the agent, then the sandbox is
  closed and the dir removed) after `--session-idle` (default 30m) without a command. At most
  `--max-sessions` (default 2) exist **per host**; memory can lower the effective capacity
  (1.5GiB reserved per new JVM, always allowing one session). A new session first tries
  another host with capacity; when all hosts are full it
  evicts the least recently used idle one (one eviction at a time) and waits for it. Sessions
  on the hub survive a server restart on disk (`bazel_server: false` until the next command
  starts a new sandbox). Runner leases belong to a process instance: host loss fails its
  active commands and the next command recreates the session elsewhere, using the hub
  remote cache. A hub restart also recreates remote leases.
- `GET /v1/sessions?client=NAME|mine=1` / `tun sessions`: `{id, name, client, root, host (local or runner name),
  created_at, last_used, commands, last_job, state (idle|busy|stopping), job, bazel_server,
  expires_at}`, most recently used first. healthz adds `sessions` and `max_sessions`.

## HTTP API (JSON, all under /v1, bearer auth)
- `HEAD|PUT /v1/blobs/{sha256}` — PUT verifies the hash and records the uploader; HEAD is
  200 only for the client's own uploads (any blob for admin; see Visibility).
- `GET /v1/blobs/{sha256}` — the blob's bytes (admin only: runners fetch workspaces with it).
- `POST /v1/cas`: tun bazel's file contents, a tar (gzip if `Content-Encoding: gzip`) of
  regular files named by their sha256; each is verified; over `--max-upload` is a 413.
  Stored in `$DATA/cas/`, deleted unused for `--keep-blobs` (job creation marks the ones a
  manifest names as used). Owned per uploader like workspace blobs.
- `POST /v1/jobs` `{argv|script, cwd, env, timeout_sec, workspace, service, ports, outputs}` -> job object (201).
  Or `{cwd, timeout_sec, bazel: {session, cold, root, manifest, args}}` for tun bazel (see
  Bazel sessions; 409 `{error, missing}` if file contents must be uploaded first). The body
  may be gzip-compressed (`Content-Encoding: gzip`); it is limited to `--max-upload`
  (compressed) and 256MB of JSON.
- `GET /v1/sessions` (see Bazel sessions). `GET /v1/jobs/{id}/bep`: a tun bazel job's
  summary object (see Bazel sessions, Results).
  `outputs`: at most 256 paths valid for the files endpoint (see Runners).
  `timeout_sec` <= 0 means the default (1800), except for services where 0 (or less) means none; over 604800 (7 days) is a 400. NUL bytes in
  argv/script/cwd/env and env names that are empty or contain `=` are a 400. The body must be
  exactly one JSON object.
- `GET /v1/jobs/{id}` -> job object `{id, state, exit_code, signal, created_at, started_at, ended_at, argv, script, cwd, error, work_deleted, client, runner, outputs, queue_sec, run_sec, queue_position, session, session_id, queue_reason, summary}`
  (`argv`, `script` and `workspace` only for the owner or admin, here and in the list).
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
- `GET /v1/healthz` (no auth) -> `{ok, sandbox, network_isolation, max_jobs, max_services, running, queued, services, local_slots, runners, sessions, max_sessions, remote_cache, disk}`:
  `running` non-service jobs (<= max_jobs), `queued` jobs waiting for a slot, `services` running.
- `/v1/runner/...`: the runner protocol (see Runners).
Errors: JSON `{"error": "..."}` with proper status codes; malformed query parameters (`limit`,
`offset`, `tail` must be plain decimal integers, `follow` a boolean; `offset` with `tail`) are a 400.

## Bazel remote cache

`--cache-dir DIR` (default `reapi`; relative DIR is under DATA; empty disables it) serves
stdlib-only REAPI gRPC over h2c on the existing HTTP listener and `DATA/cache.sock`
(override with `--cache-socket`). Both endpoints accept `x-tun-token` or `authorization:
Bearer TOKEN` gRPC metadata. Supports Capabilities, CAS (FindMissingBlobs, BatchUpdateBlobs,
BatchReadBlobs, GetTree), ActionCache and ByteStream (Read, Write, QueryWriteStatus), SHA256
and identity compression. No execution service. `--cache-max-size` bounds disk storage with
LRU eviction, including partial uploads; `--cache-action-writes=false` denies action updates
(default allow). Socket permissions are 0600. See docs/bazel.md for protocol limits and
Bazel flags.
- **Namespaces.** The CAS is shared (every write is verified against its digest). Action
  results are not: an UpdateActionResult is stored in the namespace of the request's client
  and answers only that client's GetActionResult; a lookup tries the client's own namespace,
  then the shared one, which only tun's executors write (none yet: no gRPC request can).
  The client is the token-bound identity (legacy admin requests retain header-selected
  identity as described under Visibility); REAPI `instance_name` is
  ignored. Capabilities advertise action updates only to requests with a namespace.
- **Sessions.** Every `tun bazel` session gets the cache without asking: while its sandbox
  lives, the server listens on the session's own socket `sessions/<id>/sock/cache.sock`
  (opened through the dir's descriptor, so long data dir paths work), whose dir is bound
  read-only at `/tun/sock` in the session sandbox. Only that sandbox can reach it, so the
  socket needs no token (the sandbox never sees one) and uses the session's client's
  namespace whatever the request claims. The listener stops with the sandbox (`--cold`,
  expiry, eviction, server exit). With sandbox `none` the bazelrc uses the host path,
  and if that is longer than a unix socket address allows the session has no remote cache.

## Tests (required)
- Unit tests for workspace packing/unpacking (path safety, modes, symlinks, gitignore handling).
- Integration tests that start a real server in-process on a random port with a temp data dir and
  exercise the client (and in-process runners against a hub without local slots): exit codes, stdout/stderr separation, env, cwd, timeout, cancel, concurrency
  (N jobs run in parallel, isolated dirs), queueing beyond max-jobs, reattach logs, restart recovery.
- Sandbox tests skip (t.Skip) when bwrap is unusable, so the suite passes on hosts without it;
  network isolation tests likewise when `bwrap --unshare-net` with the helper is unusable.
- `scripts/ci.sh` (vet, build, test -race) must pass.
- `bazel test //...` (`scripts/bazel-ci.sh`) must pass too: MODULE.bazel with rules_go, gazelle-generated
  BUILD files, hermetic Go SDK at go.mod's version (so the code must build and pass with Go 1.25).

## Known gaps in client isolation
Client tokens (see Visibility) separate jobs, logs, outputs, sessions, action results and
uploaded content. These remain shared or weak; treat clients as mutually trusted for
integrity until they are fixed:
- Shared tool caches (`$DATA/cache/{go-build,gomod,xdg}`) are writable by every job of every
  client. A job can plant a Go build cache entry, alter an extracted module, or put a binary
  in a tool's XDG cache (e.g. bazelisk's, or bazel-ci.sh's Bazel disk cache) that later jobs
  of other clients run or link. There is no integrity isolation between clients there; only
  Bazel sessions' caches are per client. (docs/bazel.md: executor-written caches.)
- The REAPI CAS is shared by digest (see Bazel remote cache): any client can confirm
  whether content with a guessed digest exists and read it.
- `--sandbox none` gives jobs the server user's full access: they can read `$DATA` (every
  client's workspaces, blobs, CAS, logs and the admin token) and the client token files'
  hashes. Client tokens isolate nothing from a job in that mode.
- `--net host` (or `auto` falling back to it) leaves jobs on the host network: a job can
  connect directly to other clients' services' host ports (bypassing the token-checked
  proxy) and to anything else listening on the host's loopback.
- Jobs of different clients run as the same Unix user; isolation relies on bwrap.
- The legacy admin token gives no isolation (see Visibility).

## Non-goals for v0 (later, driven by usage)
Service-to-service networking (with isolation, jobs can't reach each other at all),
raw TCP/UDP egress and DNS from isolated jobs, unauthenticated/public URLs, TLS
termination, multi-host scheduling beyond runners (labels, affinity, services on runners), persistent
workspaces and incremental upload for `tun run` (tun bazel has both), snapshots.

## Orderly deploys
See docs/deploy.md. CLI requests retry connection failures and HTTP 502/503 for up to
16m with backoff. Runners finish jobs while heartbeating with zero advertised slots.
Crashes and expired drain timeouts retain the failure behavior described above.

Job creation sends a random `X-Tun-Request-Id` per request, reused on retries. The server
returns the existing job for the same client and key for 24 hours, including across restarts,
while the job remains retained. Failed admissions do not consume the key.

### Runner session transport
Runners advertise `session_support`, `memory_available` (MemAvailable bytes), `load`
(one-minute load average divided by CPU count) and live `sessions` IDs in polls; `tun runners` exposes
memory, load and session count. Assignments carry a Bazel manifest, session metadata and
limits (including upload, workspace and disk-cache limits); runners fetch verified file contents
in batches from `POST /v1/runner/cas` (up to 256 requested digests, tar response),
with four concurrent requests. Both hosts use the same session engine, agent, manifest sync, per-client caches and BEP parser.
Logs use the existing offset protocol; exit/done also carries the summary and BEP JSON.
The hub's poll response carries authoritative session leases; revoked idle sessions stop.

On runners `/tun/sock/cache.sock` serves an HTTP/2 reverse proxy to the hub REAPI cache.
The server issues a random per-session credential in the assignment, kept in runner memory;
the sandbox sees only its socket. `/v1/session-cache/{rpc}` authenticates with `X-Tun-Session` and
`X-Tun-Session-Credential` headers, using a session map lookup and constant-time comparison,
and fixes the client namespace from the lease, ignoring request identity headers. It grants no ordinary API access
and is revoked with the lease. Hub and runner traffic must use a trusted network or HTTPS,
as with existing runner credentials. Cache action writes follow the hub's policy.

Runner session revocation retains the hub lease in a stopping state until a later poll no
longer lists that sandbox, preventing JVM overlap during eviction. Idle leases on draining
runners are revoked immediately; commands already running keep their leases. A failed
command that never created a sandbox releases its reservation through the same handshake.
Local sessions from disk are discarded when restarting with no local slots. Local
placement resource samples are cached for one second. Runner CAS retention uses the
runner's KeepBlobs interval (24h by default), and disk pressure also reclaims session CAS;
abandoned inner job dirs expire after an hour.
