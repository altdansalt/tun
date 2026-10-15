#!/usr/bin/env python3
"""Exercise real subprocess orchestration and HTTP telemetry without building locally."""
import http.server
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('agentbench', Path(__file__).with_name('agentbench.py'))
b = importlib.util.module_from_spec(spec)
spec.loader.exec_module(b)


class BenchmarkTest(unittest.TestCase):
    def test_percentiles_and_failures(self):
        rows = [dict(system='tun', n=3, wall_sec=i, exit_code=int(i == 10)) for i in range(1, 11)]
        self.assertIn('| tun | 3 | 10 | 1 | 5.000 | 9.000 | 10.000 | 55.000 |', b.summary(rows))

    def test_one_line_compiled_case(self):
        with tempfile.TemporaryDirectory() as tmp:
            repo = Path(tmp)
            file = repo / 'internal/workspace/sync_test.go'
            file.parent.mkdir(parents=True)
            original = '\t\t{"dotdot", []api.ManifestEntry{{Path: "../x"}}, ".."},\n'
            file.write_text(original)
            with patch.object(b.subprocess, 'run'):
                self.assertEqual(b.one_line_edit(repo, 2, 'nonce'), 'internal/workspace/sync_test.go')
            lines = file.read_text().splitlines()
            self.assertEqual(len(lines), 2)
            self.assertIn('Path: "../agentbench-nonce-2"', lines[0])
            self.assertEqual(lines[1], original.rstrip('\n'))

    def test_timeout(self):
        with tempfile.TemporaryDirectory() as tmp:
            code, wall, expired = b.invoke(['python3', '-c', 'import time; time.sleep(60)'], tmp, .1, Path(tmp) / 'log')
            self.assertEqual(code, 124)
            self.assertTrue(expired)
            self.assertLess(wall, 10)

    def test_concurrent_loop_http_and_restoration(self):
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                self.server.tokens.append(self.headers.get('X-Tun-Token'))
                obj = ({'id': 'test-job', 'queue_sec': 1.25, 'run_sec': 2, 'client': 'test-client'}
                       if self.path.startswith('/v1/jobs/') else {'running': 3, 'queued': 1, 'runners': [{'name': 'r1', 'load': 2}]})
                self.send_response(200)
                self.end_headers()
                self.wfile.write(json.dumps(obj).encode())
            def log_message(self, *args):
                pass
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            repo = root / 'repo'
            repo.mkdir()
            b.git(repo, 'init', '-q')
            b.git(repo, 'config', 'user.email', 'test@example.com')
            b.git(repo, 'config', 'user.name', 'test')
            for file in ['workspace/workspace_test.go', 'workspace/sync_test.go', 'server/identity_test.go']:
                path = repo / 'internal' / file
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text('package test\nimport "testing"\n')
            b.git(repo, 'add', '.')
            b.git(repo, 'commit', '-qm', 'fixture')
            b.git(repo, 'remote', 'add', 'public', str(repo))
            bin_dir = root / 'bin'
            bin_dir.mkdir()
            formatter = bin_dir / 'gofmt'
            formatter.write_text('#!/bin/sh\nexit 0\n')
            formatter.chmod(0o755)
            tun = bin_dir / 'tun'
            tun.write_text('#!/bin/sh\ngit diff | sha256sum\necho "tun: job test-job done"\necho "tun: bazel test exit=0 after 2s: 1 tests passed"\n')
            tun.chmod(0o755)
            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
            server.tokens = []
            thread = threading.Thread(target=server.serve_forever)
            thread.start()
            try:
                with patch.dict(os.environ, PATH=str(bin_dir) + ':' + os.environ['PATH'], TUN_TOKEN='test-token', TUN_URL='http://127.0.0.1:%d' % server.server_port):
                    subprocess.run(['python3', str(Path(b.__file__)), '--repo', str(repo), '--systems', 'tun', '--agents', '3', '--iterations', '3', '--baselines', '--out', str(root / 'out')], check=True)
                rows = [json.loads(line) for line in (root / 'out/results.jsonl').read_text().splitlines()][1:]
                self.assertEqual(len(rows), 15)
                self.assertEqual(sum(r["phase"] == "cold_session" and "--cold" in r["command"] for r in rows), 3)
                self.assertEqual(sum(r["phase"] == "warm_unchanged" and r["edit"] is None for r in rows), 3)
                self.assertEqual(len({r['command'][3] for r in rows}), 3)
                self.assertTrue(all(r['job']['queue_sec'] == 1.25 and r['summary'] and r['exit_code'] == 0 for r in rows))
                self.assertEqual(set(server.tokens), {'test-token'})
                self.assertEqual(b.git(repo, 'status', '--porcelain'), '')
                self.assertEqual(len({r['edit'] for r in rows if r['edit']}), 3)
                self.assertEqual(len({(root / 'out' / r['log']).read_text().splitlines()[0] for r in rows if r['edit']}), 9)
                self.assertTrue(all(r['started_at'] for r in rows))
                self.assertEqual(len({r['job']['id'] for r in rows}), 1)
                bb = bin_dir / 'bb'
                bb.write_text('#!/bin/sh\nbranch=$(git symbolic-ref --short HEAD) || exit 91\ntest "$(git rev-parse HEAD)" = "$(git rev-parse refs/remotes/origin/$branch)" || exit 92\n')
                bb.chmod(0o755)
                with patch.dict(os.environ, PATH=str(bin_dir) + ':' + os.environ['PATH']):
                    subprocess.run(['python3', str(Path(b.__file__)), '--repo', str(repo), '--systems', 'bb', '--agents', '1', '--iterations', '1', '--out', str(root / 'bb-out')], check=True)
                b.git(repo, 'checkout', '--detach', 'HEAD')
                rejected = subprocess.run(['python3', str(Path(b.__file__)), '--repo', str(repo), '--systems', 'bb', '--out', str(root / 'rejected')], capture_output=True, text=True)
                self.assertEqual(rejected.returncode, 2)
                self.assertIn('named source branch', rejected.stderr)
                self.assertFalse((root / 'rejected').exists())
            finally:
                server.shutdown()
                thread.join()
                server.server_close()


if __name__ == '__main__':
    unittest.main()
