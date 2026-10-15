# tun on real projects other than tun

October 2026, with the shared server at `$TUN_URL` (bwrap, private network namespace with the
HTTP(S) egress proxy). That server runs jobs on its own 2 slots plus two runners (tun-r1, tun-r2,
2 slots each). Each runner has its own `$DATA/cache`, so a "second run" can land on a host with a
cold cache. Where that happened it is noted below. Each project was cloned into `~/scratch` and
run as an agent would: `cd project && tun run ...`.

## What the job environment has

| tool | in jobs | notes |
|---|---|---|
| go 1.27.2 | yes | `GOCACHE`/`GOMODCACHE` shared |
| python 3.12, pip, venv | yes | |
| uv | yes | |
| gcc 13, make | yes | no cmake, clang or ccache |
| git, curl | yes | |
| node, npm | **no** | not on the VM either |
| cargo, rustc | **no** | not on the VM either |
| java, ruby | no | |

The job sees the host root read-only, so it gets exactly what the server host has installed.

## Results

| project | command | result | first run | second run | upload |
|---|---|---|---|---|---|
| spf13/cobra (Go) | `go test ./...` | pass | 4.1s (4 modules downloaded) | 1.6-2.2s | 210 KB |
| prometheus/common (Go) | `go test -count=1 ./...` | 1 test fails (proxy env, below) | 14.3s | 6.0s | 196 KB |
| psf/requests (Python, pip + venv) | `pip install -r requirements-dev.txt && pytest tests` | 615 pass, **4 fail** (network, below) | 90s (pip 8.5s, pytest 78s) | same | 3.4 MB |
| pallets/click (Python, uv) | `uv sync && uv run pytest` | 2262 pass | 7.6s | 6.7s | 524 KB |
| expressjs/express (Node) | node from nodejs.org in the job, `npm install && npm test` | **55 of 1261 fail** (dot in path, below) | node 1.9s + npm 12-31s + tests 4s | npm 3.3s with this branch | 139 KB |
| madler/zlib (C) | `./configure && make && make test` | pass | 7.5s | 4.5s | 1.6 MB |
| BurntSushi/ripgrep (Rust) | rustup in the job, `cargo test --workspace` | pass | rustup 10s + test 131s | 81s (different runner: rustup again) | 645 KB |

Uploads are small throughout (0.1-3.4 MB compressed). `.venv`, `node_modules` and `target/` are
gitignored, so they never go up. Upload time never mattered. Queue wait was usually under 0.1s.
Once it was 61.5s, while other agents' CI held all slots.

### Go: works; Go's test-result cache never hits across jobs
Module downloads go through the proxy, and the shared `GOMODCACHE`/`GOCACHE` make repeat builds
fast (prometheus/common: 14.3s cold, 6.0s warm). But `go test ./...` without `-count=1` never
reports `(cached)` in a new job, even for an unchanged tree. `GODEBUG=gocachetest=1` shows a
different test ID every job. The test ID includes the package directory, which is
`$DATA/jobs/<id>/sandbox/work/...` and so differs per job. Inside one job, copying the tree to a
second path reproduces it: same tree, new path, no `(cached)`. `-trimpath` doesn't help. A
stable workspace path inside the sandbox would fix this (see "needs" 1). For an agent that
re-runs `go test ./...` after editing one package, that is the difference between re-running only
that package and re-running everything.

### Proxy env vars break tests that set their own proxy (prometheus/common)
`TestProxyConfig_Proxy/proxy_from_environment` sets `HTTP_PROXY` and `NO_PROXY` and expects
`httpproxy.FromEnvironment()` to return them. x/net's `httpproxy` reads `http_proxy` and `no_proxy`
**first**, and tun sets those too, so the test sees tun's proxy:
```
http_config_test.go:2661: expected result URL: http://127.0.0.1:37663, but got: http://127.0.0.1:36043
http_config_test.go:2653: expected no result URL, but got: http://127.0.0.1:36043
```
Workaround (now in `tun run -h` and SPEC, with a test): `tun run -e http_proxy= -e no_proxy= ...`.
An empty `-e` value keeps tun from setting that variable, and the uppercase ones still route
module downloads. With that, the package passes.

### Python: works. The pip cache is shared, but pip is CPU-bound; uv is ~8x faster
pip honors `XDG_CACHE_HOME`, so its cache was already shared (`$DATA/cache/xdg/pip`, 16 MB after
requests). It barely helps, because resolving and building the editable install dominates:
`pip install -r requirements-dev.txt` for requests took 8.5s without a cache and 7.6s with one.
`uv venv && uv pip install -r requirements-dev.txt` took 1.17s with no cache (`UV_NO_CACHE=1`)
and 0.8-1.0s with the shared one. Recommend uv to agents for Python.

requests: 4 failures, all `TestTimeout::test_connect_timeout` / `test_total_timeout_connect`. These
tests connect straight to the non-routable 10.255.255.1 and expect a connect *timeout*. In the job's
namespace there is no route, so they fail fast instead:
```
ConnectionError: HTTPConnectionPool(host='10.255.255.1', port=80): ... [Errno 101] Network is unreachable
```
On the VM the same suite passes (619 passed, 78.4s; the tun job also took 78s of pytest). This
is inherent to "no raw TCP egress". Agents need to know to deselect such tests
(`--deselect` / `-k 'not connect_timeout'`).

### Node: no toolchain; npm cache was per-job (fixed here); dot-directories in the job path break express
- There is no node on the server. An agent can install one into the shared cache inside the job:
  `curl -fsSL https://nodejs.org/dist/v22.20.0/node-v22.20.0-linux-x64.tar.xz | tar -xJ -C $XDG_CACHE_HOME/tools/node`
  takes about 2s through the proxy. It is ad hoc, though, and racy without a temp dir + rename.
- express has no lockfile, so `npm ci` fails: `npm error ... Run "npm help ci"` (the project's
  choice, not tun's). `npm install` works.
- npm's cache defaulted to `$HOME/.npm`, which is per job, so every job re-downloaded about 66 MB
  (12-31s depending on load). **Fixed on this branch:** `npm_config_cache=$DATA/cache/npm`
  (shared; npm's cacache is content-addressed and safe under concurrency), plus
  `npm_config_logs_dir=$HOME/.npm/_logs` so debug logs stay per job. Measured on a private
  server built from this branch (`scripts/dev-tun.sh`): express `npm install` took **14.0s cold,
  3.3s warm**.
- 55 express tests (`res.sendFile`, `res.download`) fail with `expected 200 "OK", got 404 "Not Found"`.
  express's `send` refuses to serve paths containing a dot-segment, and the job's work dir is
  `/home/exedev/.local/share/tun/jobs/<id>/sandbox/work`: `.local` is a dot-directory. The
  same tree copied to `/tmp/x` in the same job passes all 1261. Any tool that ignores hidden
  paths (static file servers, some glob/watch libraries, linters with default excludes) can hit
  this. A stable, neutral workspace path (needs 1) fixes it. So would a server data dir without
  dot-components (`tun serve --data /srv/tun`), which costs nothing.
- Without `--exit`, mocha hung after those failures (open handles) until I cancelled it at
  250s. Not tun's fault, but a reminder to always pass `--timeout` for unfamiliar suites.

### C: works
zlib `./configure && make && make test`: 7.5s, then 4.5s. There is no ccache, and nothing would
persist its cache anyway (it uses `~/.cache/ccache` → `XDG_CACHE_HOME`, so it would be shared if it
were installed).

### Rust: works if the job installs rustup into the shared cache; `target/` is the cost
With `RUSTUP_HOME`/`CARGO_HOME` under `$XDG_CACHE_HOME/tools`, `sh.rustup.rs --profile minimal`
took 10s and 593 MB. Then ripgrep's `cargo fetch` took 1.0s and `cargo test --workspace` 131s (all
pass). The second run landed on a different runner, so rustup ran again and `cargo test` took 81s.
Each job builds from scratch into a fresh 1.2 GB `target/` under the job's work dir. That is kept
for `--keep-work` (1h) and counts toward disk, not toward `--max-workspace` (which caps only the
upload). No registry cache is shared unless the agent points `CARGO_HOME` at one, as above.

## What tun would need (beyond this branch)

1. **Stable, neutral paths inside the sandbox.** Bind the job's work/home/tmp at fixed paths
   without dot-components under bwrap (e.g. `/tun/work`, `/tun/home`), instead of
   `$DATA/jobs/<id>/sandbox/...`. This fixes Go's cross-job test cache (every `go test ./...`
   currently re-runs every package) and the express-style "hidden path" failures. It also makes
   paths in logs identical across jobs. Cost: the read-only root has no free mount point, so it
   needs a tmpfs parent (bwrap can create dirs under a `--tmpfs`, as it already does for the
   hidden home). Env, services, the netns helper path and `none` mode (which can't remap) all need
   care. A cheap mitigation now: run the shared server with `--data` outside any dot-directory.
2. **Toolchain provisioning.** Node and Rust exist only if a job downloads them. Options: install
   them on server hosts; or a documented shared `$DATA/tools` with an atomic "install once"
   helper (e.g. `tun run --tool node@22`); or honor `.tool-versions`/`mise`. With rustup in the
   shared cache, the toolchain was about 600 MB per host.
3. **Cache affinity across runners.** With 3 hosts each keeping its own cache, warm-cache
   timings are a lottery: the ripgrep re-run paid rustup again. Options: prefer the runner that
   last ran the same workspace or client, or a shared/remote cache. (When this was measured the job object didn't
   say which runner ran it; it now has a `runner` field, so a slow run can at least be explained.)
4. **More shared caches, opt-in by tool.** `CARGO_HOME` can't simply be shared (it also holds
   `bin/` and credentials), but cargo's registry/git cache is safe under its file locks. Something
   like `CARGO_HOME=$DATA/cache/cargo` once cargo exists on hosts. pip, uv, yarn and ccache
   already follow `XDG_CACHE_HOME`. pnpm's store would not be shared (`XDG_DATA_HOME`), and its
   hard links can't cross the bind mounts anyway.
5. **Fetch files outside the work dir.** `tun get`/`--out` can only read the work dir, so a
   test log written to `$TMPDIR` (a common habit) can't be retrieved:
   `tun: warning: m.log: no such file in job ...'s work dir`.
6. **Network-test guidance.** Document that tests needing raw TCP, DNS or blackhole timeouts
   (requests' connect-timeout tests) fail fast with `ENETUNREACH`, and how to deselect them.
