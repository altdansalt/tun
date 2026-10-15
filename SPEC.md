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
tun serve [--addr 127.0.0.1:7070] [--data DIR] [--max-jobs N] [--sandbox auto|bwrap|none]
tun run [flags] -- <argv...>        run argv (no shell)
tun run [flags] -c '<shell script>' run via bash -c
    -C DIR          working dir, relative to the workspace root (default: the client's cwd relative to the root)
    -e K=V          set env var (repeatable); -e K copies K from the client's env
    --timeout DUR   default 30m; job is killed (whole process group) when exceeded
    --no-workspace  don't upload anything; run in an empty dir
    --detach        print the job id and exit 0 immediately
    --json          after the job ends, print one JSON object (id, exit_code, duration, ...) to stdout
                    instead of streaming (stdout/stderr then in the object, truncated to a limit)
tun status <id>   (JSON)          tun logs [-f] <id> [--stderr]       tun cancel <id>
tun ls            (JSON lines: recent jobs, newest first)
```
Server address: `$TUN_URL` (default `http://127.0.0.1:7070`). Auth: bearer token. The server
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

## HTTP API (JSON, all under /v1, bearer auth)
- `HEAD|PUT /v1/blobs/{sha256}` — PUT verifies the hash.
- `POST /v1/jobs` `{argv|script, cwd, env, timeout_sec, workspace}` -> job object (201).
  `timeout_sec` <= 0 means the default (1800); over 604800 (7 days) is a 400. NUL bytes in
  argv/script/cwd/env and env names that are empty or contain `=` are a 400. The body must be
  exactly one JSON object.
- `GET /v1/jobs/{id}` -> job object `{id, state, exit_code, signal, created_at, started_at, ended_at, argv, script, cwd, error}`.
- `GET /v1/jobs?limit=N` -> list. `POST /v1/jobs/{id}/cancel`.
- `GET /v1/jobs/{id}/stdout?offset=N&follow=1` (and `/stderr`): raw bytes from offset; with follow,
  streams (flush as written) until the job ends and all bytes are sent. Reattachable at any offset.
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
Long-running services / port exposure, artifact download, multi-host scheduling, persistent
workspaces, snapshots, incremental (per-file) upload.
