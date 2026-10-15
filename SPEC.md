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
tun status <id>   (JSON)          tun logs [-f] <id> [--stderr]       tun cancel <id>
tun ls            (JSON lines: recent jobs, newest first)
tun get [-o DIR] <id> PATH...     download files/dirs/globs from a finished job's work dir
```
`tun run --out PATH` (repeatable): after the job ends (any state), download PATH (relative to the
workspace root; file, dir, or `path.Match` glob) from the job's work dir into the same relative path
of the local workspace root (`tun get`: or under `-o DIR`), replacing existing files (temp + rename),
keeping modes/mtimes. A PATH matching nothing is a `tun: warning:` on stderr; the exit status stays
the command's. A failed download (size limit, network) turns exit 0 into 125. `tun get` exits 1 if
any PATH matched nothing or was refused, 125 on errors. Local writes go through an `os.Root`, so
neither the stream nor local symlinks can write outside the target dir.
Server address: `$TUN_URL` (default `http://127.0.0.1:7070`). Auth: bearer token, sent as `X-Tun-Token: <token>` (also accepted: `Authorization: Bearer <token>`; some proxies, e.g. exe.dev's, consume Authorization). The server
creates `$DATA/token` on first start (0600) if absent; clients read `$TUN_TOKEN`, else
`~/.config/tun/token`, else `$DATA/token` for the default data dir (`~/.local/share/tun`), so a local
client works with zero config.

Exit status of `tun run`: the command's exit code; 128+signal if killed by a signal; 124 on timeout;
125 for tun's own errors (can't reach server, upload failed, bad flags) with a one-line message on
stderr that starts with `tun:`. On SIGINT/SIGTERM the client cancels the job and then exits.

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
  env, `/dev` `/proc` fresh, new pid namespace, `--die-with-parent`; network allowed (builds fetch
  modules). Jobs cannot see or write each other's directories, their own job.json/logs, or the
  server's data dir/token. The job env is set inside the sandbox (`--clearenv`/`--setenv`, passed
  via `--args <fd>`): bwrap itself runs with an empty env, so `-e LD_PRELOAD=...` can't affect the
  unsandboxed bwrap process, and env values don't appear in host `ps`. Shared caches are writable by
  every job, so a job can poison them for later jobs.
- `none`: plain subprocess in its own process group (for hosts without bwrap); still per-job dirs,
  but **no isolation at all**: the job runs as the server user with the server's filesystem access,
  so it can read `$DATA/token`, other jobs' workspaces and logs, and rewrite job state. Use it only
  when every job is as trusted as the server operator.
- `--max-jobs` (default NumCPU) jobs run at once; the rest queue FIFO (state `queued`).
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
  `127.0.0.1:H` (or all interfaces; the bwrap sandbox shares the host network).
- The server probes each host port (TCP connect, every 100ms) until it accepts and marks the port
  `ready`. The job object has `service: true` and `ports: [{port, host_port, ready, path}]`; the
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

## HTTP API (JSON, all under /v1, bearer auth)
- `HEAD|PUT /v1/blobs/{sha256}` — PUT verifies the hash.
- `POST /v1/jobs` `{argv|script, cwd, env, timeout_sec, workspace, service, ports}` -> job object (201).
  `timeout_sec` <= 0 means the default (1800), except for services where 0 (or less) means none; over 604800 (7 days) is a 400. NUL bytes in
  argv/script/cwd/env and env names that are empty or contain `=` are a 400. The body must be
  exactly one JSON object.
- `GET /v1/jobs/{id}` -> job object `{id, state, exit_code, signal, created_at, started_at, ended_at, argv, script, cwd, error, work_deleted}`.
- `GET /v1/jobs?limit=N` -> list. `POST /v1/jobs/{id}/cancel`.
- `GET /v1/jobs/{id}/stdout?offset=N&follow=1` (and `/stderr`): raw bytes from offset; with follow,
  streams (flush as written) until the job ends and all bytes are sent. Reattachable at any offset.
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
  `X-Forwarded-Prefix: /v1/jobs/{id}/ports/{N}`. Responses are flushed as they stream; protocol
  upgrades (websockets) are passed through. 404 if the job has no port N, 409 if the job is not
  running, 502 (JSON error) if nothing accepts on the host port.
- `GET /v1/healthz` (no auth).
Errors: JSON `{"error": "..."}` with proper status codes; malformed query parameters (`limit`,
`offset` must be plain decimal integers, `follow` a boolean) are a 400.

## Tests (required)
- Unit tests for workspace packing/unpacking (path safety, modes, symlinks, gitignore handling).
- Integration tests that start a real server in-process on a random port with a temp data dir and
  exercise the client: exit codes, stdout/stderr separation, env, cwd, timeout, cancel, concurrency
  (N jobs run in parallel, isolated dirs), queueing beyond max-jobs, reattach logs, restart recovery.
- Sandbox tests skip (t.Skip) when bwrap is unusable, so the suite passes on hosts without it.
- `scripts/ci.sh` (vet, build, test -race) must pass.

## Non-goals for v0 (later, driven by usage)
Service-to-service networking beyond the shared host network, unauthenticated/public URLs, TLS
termination, multi-host scheduling, persistent
workspaces, snapshots, incremental (per-file) upload.
