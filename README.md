# tun

Remote execution for autonomous software development agents.

```
tun serve &                       # server on 127.0.0.1:7070, data in ~/.local/share/tun
tun run -- go test ./...          # ship the working tree, run there, stream output, same exit code
tun help                          # full CLI docs
```

See SPEC.md for the contract (CLI, HTTP API, sandboxing). `scripts/ci.sh` builds and tests.
