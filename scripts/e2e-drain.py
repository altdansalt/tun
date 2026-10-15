#!/usr/bin/env python3
"""Restart real hubs/runners with attached and queued clients."""
import json
import os
import signal
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.request

with tempfile.TemporaryDirectory() as tmp:
    children = []
    def launch(args, name, env=None, cwd=None):
        p = subprocess.Popen(["tun"] + args, stdout=open(tmp+"/"+name+".out", "w"),
                             stderr=open(tmp+"/"+name+".err", "w"), env=env, cwd=cwd)
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
        for name in ("hub", "runner", "pure-hub"):
            runner = name == "runner"
            remote = name != "hub"
            data = tmp+"/"+name
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            url = "http://127.0.0.1:"+str(port)
            args = ["serve", "--addr", "127.0.0.1:"+str(port), "--data", data,
                    "--local-slots", "0" if remote else "1", "--runner-timeout", "1s",
                    "--drain-timeout", "20s"]
            hub = launch(args, name+"-server")
            until(lambda: urllib.request.urlopen(url+"/v1/healthz").status == 200)
            env = dict(os.environ, TUN_URL=url, TUN_TOKEN=open(data+"/token").read().strip())
            if remote:
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
                # The queued command can finish before its client next polls. Restart
                # before waiting for that client to fetch persisted logs and status.
                hub = launch(args, name+"-server2")
                assert queued.wait(timeout=25) == 0
                assert contents(name+"-queued", "out") == "queued\n"
                assert retry.wait(timeout=25) == 0
                assert contents(name+"-retry", "out") == "resumed\n"
                assert "tun: server unavailable, retrying" in contents(name+"-retry", "err")
            subprocess.run(["tun", "run", "--no-workspace", "--", "true"], env=env, check=True)
            if remote:
                worker.send_signal(signal.SIGTERM)
                assert worker.wait(timeout=25) == 0
            hub.send_signal(signal.SIGTERM)
            assert hub.wait(timeout=25) == 0
            print("e2e-drain:", name, "restart preserved full output and exit 7;",
                  "drain %.3fs" % drain_seconds, flush=True)
        if shutil.which("bazel"):
            for signal_runner in (False, True):
                name = "session-runner" if signal_runner else "session-hub"
                data = tmp+"/"+name
                ws = data+"-ws"
                os.mkdir(ws)
                open(ws+"/MODULE.bazel", "w").write('module(name="drain_session")\n')
                open(ws+"/.bazelversion", "w").write('9.3.0\n')
                open(ws+"/BUILD.bazel", "w").write('genrule(name="slow", outs=["result.txt"], cmd="sleep 3; echo retained > $@")\n')
                with socket.socket() as sock:
                    sock.bind(("127.0.0.1", 0))
                    port = sock.getsockname()[1]
                url = "http://127.0.0.1:"+str(port)
                args = ["serve", "--addr", "127.0.0.1:"+str(port), "--data", data,
                        "--local-slots", "0", "--runner-timeout", "1s", "--drain-timeout", "2m"]
                hub = launch(args, name+"-server")
                until(lambda: urllib.request.urlopen(url+"/v1/healthz").status == 200)
                env = dict(os.environ, TUN_URL=url, TUN_TOKEN=open(data+"/token").read().strip())
                rargs = ["runner", "--hub", url, "--name", "session-worker", "--slots", "1",
                         "--data", data+"-runner", "--drain-timeout", "2m"]
                worker = launch(rargs, name+"-worker", env)
                until(lambda: json.loads(urllib.request.urlopen(url+"/v1/healthz").read())["runners"])
                job = launch(["bazel", "build", "//:slow"], name+"-job", env, ws)
                def jobs():
                    req = urllib.request.Request(url+"/v1/jobs", headers={"X-Tun-Token": env["TUN_TOKEN"]})
                    return json.loads(urllib.request.urlopen(req).read())
                until(lambda: any(j["state"] == "running" for j in jobs()))
                jid = jobs()[0]["id"]
                target = worker if signal_runner else hub
                drain_start = time.monotonic()
                target.send_signal(signal.SIGTERM)
                assert job.wait(timeout=120) == 0, contents(name+"-job", "err")
                assert target.wait(timeout=120) == 0
                seconds = time.monotonic()-drain_start
                if signal_runner:
                    worker = launch(rargs, name+"-worker2", env)
                else:
                    hub = launch(args, name+"-server2")
                    until(lambda: urllib.request.urlopen(url+"/v1/healthz").status == 200)
                logs = subprocess.check_output(["tun", "logs", "--stderr", jid], env=env, text=True)
                assert "tun: bazel build exit=0" in logs, logs
                subprocess.run(["tun", "get", "-o", ws+"/got", jid, "//:slow"], env=env, check=True)
                assert open(ws+"/got/bazel-bin/result.txt").read() == "retained\n"
                again = launch(["bazel", "build", "//:slow"], name+"-again", env, ws)
                assert again.wait(timeout=120) == 0, contents(name+"-again", "err")
                assert "remote cache hit" in contents(name+"-again", "err"), contents(name+"-again", "err")
                worker.send_signal(signal.SIGTERM)
                assert worker.wait(timeout=30) == 0
                hub.send_signal(signal.SIGTERM)
                assert hub.wait(timeout=30) == 0
                print("e2e-drain:", name, "drain %.3fs; retained artifacts/logs and cache hit after restart" % seconds, flush=True)
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
