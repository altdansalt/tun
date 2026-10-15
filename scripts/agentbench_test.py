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
            tun.write_text('#!/bin/sh\ngit diff --exit-code >/dev/null && exit 99\necho "tun: job test-job done"\necho "tun: bazel test exit=0 after 2s: 1 tests passed"\n')
            tun.chmod(0o755)
            server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
            server.tokens = []
            thread = threading.Thread(target=server.serve_forever)
            thread.start()
            try:
                with patch.dict(os.environ, PATH=str(bin_dir) + ':' + os.environ['PATH'], TUN_TOKEN='test-token', TUN_URL='http://127.0.0.1:%d' % server.server_port):
                    subprocess.run(['python3', str(Path(b.__file__)), '--repo', str(repo), '--systems', 'tun', '--agents', '3', '--iterations', '3', '--out', str(root / 'out')], check=True)
                rows = [json.loads(line) for line in (root / 'out/results.jsonl').read_text().splitlines()][1:]
                self.assertEqual(len(rows), 9)
                self.assertEqual(len({r['command'][3] for r in rows}), 3)
                self.assertTrue(all(r['job']['queue_sec'] == 1.25 and r['summary'] and r['exit_code'] == 0 for r in rows))
                self.assertEqual(set(server.tokens), {'test-token'})
                self.assertEqual(b.git(repo, 'status', '--porcelain'), '')
                self.assertEqual(len({r['edit'] for r in rows}), 3)
                self.assertTrue(all(r['started_at'] for r in rows))
                self.assertEqual(len({r['job']['id'] for r in rows}), 1)
            finally:
                server.shutdown()
                thread.join()
                server.server_close()


if __name__ == '__main__':
    unittest.main()
