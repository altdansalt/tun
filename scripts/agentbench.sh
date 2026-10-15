#!/bin/sh
# All builds happen remotely; Python is only the load generator and recorder.
exec python3 "$(dirname "$0")/agentbench.py" "$@"
