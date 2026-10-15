#!/usr/bin/env python3
"""Restart real hubs/runners with attached and queued clients."""
import json
import os
import signal
import socket
import subprocess
import tempfile
import time
import urllib.request

with tempfile.TemporaryDirectory() as tmp:
    children = []
    def launch(args, name, env=None):
        p = subprocess.Popen(["tun"] + args, stdout=open(tmp+"/"+name+".out", "w"),
                             stderr=open(tmp+"/"+name+".err", "w"), env=env)
        children.append(p)
        return p

    def until(check):
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            try:
                if check():
                    return
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.05)
        raise AssertionError("timed out waiting for process state")

    def contents(name, stream):
        return open(tmp+"/"+name+"."+stream).read()

    try:
        for runner in (False, True):
            name = "runner" if runner else "hub"
            data = tmp+"/"+name
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            url = "http://127.0.0.1:"+str(port)
            args = ["serve", "--addr", "127.0.0.1:"+str(port), "--data", data,
                    "--local-slots", "0" if runner else "1", "--runner-timeout", "1s",
                    "--drain-timeout", "20s"]
            hub = launch(args, name+"-server")
            until(lambda: urllib.request.urlopen(url+"/v1/healthz").status == 200)
            env = dict(os.environ, TUN_URL=url, TUN_TOKEN=open(data+"/token").read().strip())
            if runner:
                rargs = ["runner", "--hub", url, "--name", "drain-test", "--slots", "1",
                         "--data", data+"-runner", "--drain-timeout", "20s"]
                worker = launch(rargs, name+"-worker", env)
                until(lambda: json.loads(urllib.request.urlopen(url+"/v1/healthz").read())["runners"])
            job = launch(["run", "--no-workspace", "-c",
                          "echo before; echo err-before >&2; sleep 3; echo after; echo err-after >&2; exit 7"],
                         name+"-job", env)
            until(lambda: "before" in contents(name+"-job", "out"))
            if not runner:
                queued = launch(["run", "--no-workspace", "-c", "echo queued"], name+"-queued", env)
                until(lambda: "queued (" in contents(name+"-queued", "err"))
            target = worker if runner else hub
            drain_start = time.monotonic()
            target.send_signal(signal.SIGTERM)
            if not runner:
                def draining():
                    req = urllib.request.Request(url+"/v1/jobs", data=b"{}",
                                                 headers={"X-Tun-Token": env["TUN_TOKEN"]})
                    try:
                        urllib.request.urlopen(req)
                    except urllib.error.HTTPError as e:
                        return e.code == 503
                    return False
                until(draining)
                retry = launch(["run", "--no-workspace", "-c", "echo resumed"], name+"-retry", env)
            assert job.wait(timeout=25) == 7
            assert contents(name+"-job", "out") == "before\nafter\n"
            errs = contents(name+"-job", "err")
            assert "err-before\nerr-after\n" in errs, errs
            assert target.wait(timeout=25) == 0
            drain_seconds = time.monotonic() - drain_start
            if runner:
                worker = launch(rargs, name+"-worker2", env)
                until(lambda: json.loads(urllib.request.urlopen(url+"/v1/healthz").read())["runners"])
            else:
                assert queued.wait(timeout=10) == 0
                assert contents(name+"-queued", "out") == "queued\n"
                hub = launch(args, name+"-server2")
                assert retry.wait(timeout=25) == 0
                assert contents(name+"-retry", "out") == "resumed\n"
                assert "tun: server unavailable, retrying" in contents(name+"-retry", "err")
            subprocess.run(["tun", "run", "--no-workspace", "--", "true"], env=env, check=True)
            if runner:
                worker.send_signal(signal.SIGTERM)
                assert worker.wait(timeout=25) == 0
            hub.send_signal(signal.SIGTERM)
            assert hub.wait(timeout=25) == 0
            print("e2e-drain:", name, "restart preserved full output and exit 7;",
                  "drain %.3fs" % drain_seconds, flush=True)
    except BaseException:
        import glob
        for path in glob.glob(tmp+"/*.err"):
            print(path, open(path).read(), flush=True)
        raise
    finally:
        for p in children:
            if p.poll() is None:
                p.kill()
            p.wait()
