Captured from real Bazel 9.3.0 via tun job 20261015T074837676783-9d8cb93f.
The eight commands took 26.769s total (including fresh server startup/downloads).
See generate.sh for the exact workspace and commands. JSONL is unmodified BEP;
stdout/stderr retain diagnostics, including the actual compile error. Query has an
empty JSONL because it does not support BEP. Absolute paths are capture-host paths,
not paths tests should open. Initial exploratory job 20261015T074731409404-a5939759
ran in 16.394s but its flaky marker was reset between attempts; it is not used.
