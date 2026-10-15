#!/usr/bin/env python3
"""Bounded edit-to-result benchmark. Python 3 stdlib only; no local Bazel."""
import argparse
import concurrent.futures
import datetime
import json
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import tempfile
import threading
import time
import urllib.request
import uuid


def git(repo, *args):
    return subprocess.check_output(['git', '-C', str(repo), *args], text=True).strip()


def edit(repo, profile, sequence, nonce):
    """Restore before calling: each iteration adds one new executable regression case."""
    value = 'agentbench-' + nonce + '-' + str(sequence)
    if profile == 'gazelle':
        path = 'v2/label/label_test.go'
        bodies = [
            'if got, err := Parse("//:%s"); err != nil || got.Name != "%s" { t.Fatalf("parse: %%v %%v", got, err) }' % (value, value),
            'if got := (Label{Name: "%s"}).String(); got != "//:%s" { t.Fatal(got) }' % (value, value),
        ]
    elif sequence % 3 == 0:
        path = 'internal/workspace/workspace_test.go'
        bodies = ['root := t.TempDir(); write(t, filepath.Join(root, "case.txt"), "%s", 0600); dst := roundTrip(t, root, []string{"case.txt"}); if b, err := os.ReadFile(filepath.Join(dst, "case.txt")); err != nil || string(b) != "%s" { t.Fatalf("roundtrip: %%q %%v", b, err) }' % (value, value)]
    elif sequence % 3 == 1:
        path = 'internal/workspace/sync_test.go'
        bodies = ['s := newSyncer(t); write(t, filepath.Join(s.src, "case.txt"), "%s", 0600); s.mustSync(); if b, err := os.ReadFile(filepath.Join(s.dst, "case.txt")); err != nil || string(b) != "%s" { t.Fatalf("sync: %%q %%v", b, err) }' % (value, value)]
    else:
        path = 'internal/server/identity_test.go'
        bodies = ['if got := cleanName(" %s\\t\\n"); got != "%s" { t.Fatal(got) }' % (value, value)]
    with (repo / path).open('a') as f:
        f.write('\nfunc TestAgentbenchRegression(t *testing.T) {\n' + bodies[sequence % len(bodies)] + '\n}\n')
    subprocess.run(['gofmt', '-w', str(repo / path)], check=True)
    return path


class API:
    def __init__(self):
        self.url = os.environ.get('TUN_URL', 'http://127.0.0.1:7070').rstrip('/')
        self.token = os.environ.get('TUN_TOKEN')
        if not self.token:
            self.token = (Path.home() / '.config/tun/token').read_text().strip()

    def get(self, path):
        try:
            req = urllib.request.Request(self.url + path, headers={'X-Tun-Token': self.token})
            with urllib.request.urlopen(req, timeout=10) as r:
                return json.load(r)
        except Exception as e:
            return {'telemetry_error': str(e)}


def invoke(command, cwd, timeout, log):
    # tun receives SIGINT first so it can cancel its remote job, then a bounded kill.
    start = time.monotonic()
    with log.open('w') as f:
        p = subprocess.Popen(command, cwd=cwd, stdout=f, stderr=subprocess.STDOUT,
                             start_new_session=True)
        expired = False
        try:
            code = p.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            expired = True
            os.killpg(p.pid, signal.SIGINT)
            try:
                p.wait(timeout=20)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait()
            code = 124
    return code, time.monotonic() - start, expired


def quantile(values, p):
    return sorted(values)[max(0, math.ceil(len(values) * p) - 1)]


def summary(rows):
    lines = ['| system | N | samples | failed | p50 s | p90 s | max s | sum wall s |',
             '|---|---:|---:|---:|---:|---:|---:|---:|']
    for system, n in sorted({(r['system'], r['n']) for r in rows}):
        group = [r for r in rows if (r['system'], r['n']) == (system, n)]
        times = [r['wall_sec'] for r in group]
        lines.append('| %s | %d | %d | %d | %.3f | %.3f | %.3f | %.3f |' % (
            system, n, len(group), sum(r['exit_code'] != 0 for r in group),
            quantile(times, .5), quantile(times, .9), max(times), sum(times)))
    return '\n'.join(lines)


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--repo', type=Path, default=Path(__file__).resolve().parent.parent,
                   help='local git repo; benchmark committed HEAD in isolated clones')
    p.add_argument('--profile', choices=['tun', 'gazelle'], default='tun')
    p.add_argument('--systems', nargs='+', choices=['tun', 'bb'], default=['tun', 'bb'])
    p.add_argument('--agents', nargs='+', type=int, default=[1, 3])
    p.add_argument('--iterations', type=int, default=5)
    p.add_argument('--baselines', action='store_true', help='prepend cold-session and unchanged warm commands before edits')
    p.add_argument('--timeout', type=int, default=480, help='seconds per command')
    p.add_argument('--budget', type=int, default=1800, help='total elapsed admission budget, seconds')
    p.add_argument('--out', type=Path, required=True, help='new directory for JSONL, summary and raw logs')
    p.add_argument('targets', nargs='*', help='default: workspace + server; Gazelle: v2/label')
    a = p.parse_args()
    if min(a.agents + [a.iterations, a.timeout, a.budget]) < 1:
        p.error('counts and limits must be positive')
    a.repo = a.repo.resolve()
    if 'bb' in a.systems:
        branch = subprocess.run(['git', '-C', str(a.repo), 'symbolic-ref', '--quiet', 'HEAD'], capture_output=True)
        if branch.returncode:
            p.error('bb remote requires a named source branch; check out a pushed branch before benchmarking')
    commit = git(a.repo, 'rev-parse', 'HEAD')
    remote = git(a.repo, 'remote', 'get-url', 'public' if a.profile == 'tun' else 'origin')
    targets = a.targets or (['//internal/workspace/...', '//internal/server/...'] if a.profile == 'tun' else ['//v2/label:label_test'])
    a.out.mkdir(parents=True, exist_ok=False)
    a.out = a.out.resolve()
    api = API() if 'tun' in a.systems else None
    nonce = uuid.uuid4().hex[:12]
    lock = threading.Lock()
    rows = []
    exhausted = threading.Event()
    deadline = time.monotonic() + a.budget
    def emit(row):
        with lock:
            with (a.out / 'results.jsonl').open('a') as f:
                f.write(json.dumps(row, sort_keys=True) + '\n')
    emit(dict(type='metadata', commit=commit, remote=remote, profile=a.profile,
              targets=targets, agents=a.agents, iterations=a.iterations, nonce=nonce,
              timeout_sec=a.timeout, budget_sec=a.budget,
              identity='VM token shared by all agents; distinct session names',
              started=datetime.datetime.now(datetime.timezone.utc).isoformat()))
    def agent(system, n, index):
        with tempfile.TemporaryDirectory(prefix='agentbench-') as tmp:
            repo = Path(tmp) / 'repo'
            subprocess.run(['git', 'clone', '-q', '--shared', str(a.repo), str(repo)], check=True)
            git(repo, 'checkout', '--detach', commit)
            git(repo, 'remote', 'set-url', 'origin', remote)
            git(repo, 'config', 'buildbuddy.remote-bazel-remote-name', 'origin')
            session = 'ab-%s-%d-%d' % (nonce, n, index)
            try:
                for iteration in range(-2 if a.baselines else 0, a.iterations):
                    if time.monotonic() >= deadline:
                        exhausted.set()
                        emit(dict(type='budget_exhausted', system=system, n=n, agent=index, iteration=iteration))
                        break
                    git(repo, 'reset', '--hard', commit)
                    before = api.get('/v1/healthz') if api else None
                    started_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
                    start = time.monotonic()
                    path = edit(repo, a.profile, iteration, '%s-n%d-a%d' % (nonce, n, index)) if iteration >= 0 else None
                    command = (['tun', 'bazel', '--session', session, '--timeout', str(a.timeout) + 's']
                               if system == 'tun' else ['bb', 'remote', '--remote_runner=grpcs://remote.buildbuddy.dev', '--timeout=' + str(a.timeout) + 's'])
                    if system == 'tun' and iteration == -2:
                        command += ['--cold']
                    command += ['test', *targets, '--jobs=2']
                    log = a.out / ('%s-n%d-a%d-i%d.log' % (system, n, index, iteration))
                    code, command_wall, expired = invoke(command, repo, min(a.timeout, max(1, deadline - time.monotonic())), log)
                    wall = time.monotonic() - start
                    output = log.read_text(errors='replace')
                    ids = re.findall(r'tun: job ([0-9A-Za-z-]+)', output)
                    job = api.get('/v1/jobs/' + ids[-1]) if api and ids else None
                    row = dict(type='iteration', system=system, n=n, agent=index, started_at=started_at,
                               iteration=iteration, phase={-2: 'cold_session', -1: 'warm_unchanged'}.get(iteration, 'edit'),
                               edit=path, wall_sec=wall, command_wall_sec=command_wall,
                               exit_code=code, timed_out=expired, command=command,
                               summary=[s for s in output.splitlines() if s.startswith('tun: bazel ')],
                               job=job, health_before=before,
                               health_after=api.get('/v1/healthz') if api else None,
                               invocation_urls=list(dict.fromkeys(re.findall(r'https://app.buildbuddy.dev/invocation/[\w-]+', output))),
                               log=log.name)
                    emit(row)
                    with lock:
                        rows.append(row)
                    print('%s N=%d agent=%d iteration=%d exit=%d wall=%.3fs' % (system, n, index, iteration, code, wall), flush=True)
            finally:
                git(repo, 'reset', '--hard', commit)
    for system in a.systems:
        for n in a.agents:
            with concurrent.futures.ThreadPoolExecutor(max_workers=n) as pool:
                list(pool.map(lambda i: agent(system, n, i), range(n)))
    if not rows:
        return 1
    table = summary(rows)
    for phase in ['cold_session', 'warm_unchanged', 'edit']:
        group = [r for r in rows if r['phase'] == phase]
        if group:
            table += '\n\n' + phase + '\n\n' + summary(group)
    (a.out / 'summary.md').write_text(table + '\n')
    print(table)
    return int(exhausted.is_set() or any(r['exit_code'] for r in rows))


if __name__ == '__main__':
    raise SystemExit(main())
