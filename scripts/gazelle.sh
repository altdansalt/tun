#!/bin/bash
# gazelle.sh: fix BUILD files in a tun job and bring back only the ones gazelle changed:
#   tun run --out gazelle.tar -- ./scripts/gazelle.sh && tar xvf gazelle.tar && rm gazelle.tar
# Runs `bazel run //:gazelle -- ARGS` (e.g. ARGS internal/server limits it to that dir), prints
# each changed or new BUILD file with its diff, and writes them to gazelle.tar (GAZELLE_TAR) at
# the workspace root: an empty tar when nothing changed, so the --out above always succeeds.
# GAZELLE_CHECK=1 exits 1 if anything changed (a CI check without fetching anything).
#
# Caches as in bazel-ci.sh: the output root under $TMPDIR (deleted at exit), the shared
# repository cache and disk cache in $XDG_CACHE_HOME/tun-bazel (TUN_BAZEL_CACHE overrides;
# BAZEL_DISK_CACHE=0 skips the disk cache; see the trust note in bazel-ci.sh). Even warm, Bazel
# spends ~50s loading and analyzing //:gazelle in a fresh output base, so the built gazelle
# binary is also kept in $cache/gazelle/KEY (KEY: MODULE.bazel, its lockfile, the root
# BUILD.bazel) and later runs on that host call it directly, without Bazel, like the
# `bazel run` wrapper does. Same trust caveat as the disk cache (any job can write it);
# GAZELLE_CACHE=0 always goes through Bazel.
set -euo pipefail
cd "$(dirname "$0")/.."
out=${GAZELLE_TAR:-gazelle.tar}
cache=${TUN_BAZEL_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/tun-bazel}
flags=(--repository_cache="$cache/repo")
if [ "${BAZEL_DISK_CACHE:-1}" != 0 ]; then
  flags+=(--disk_cache="$cache/disk" --experimental_disk_cache_gc_max_size=3G)
fi
tmp=$(mktemp -d)
root=${TMPDIR:-/tmp}/tun-gazelle-root
bazel=(bazel --output_user_root="$root")
cleanup() {
  if [ -d "$root" ]; then
    "${bazel[@]}" shutdown 2>/dev/null || true
    chmod -R u+w "$root" 2>/dev/null || true
    rm -rf "$root"
  fi
  rm -rf "$tmp"
}
trap cleanup EXIT
# BUILD files, sorted; bazel-* are Bazel's convenience symlinks.
builds() {
  find . -name 'bazel-*' -prune -o \( -name BUILD.bazel -o -name BUILD \) -type f -print | sed 's|^\./||' | sort
}
builds >"$tmp/list"
tr '\n' '\0' <"$tmp/list" | xargs -0 -r sha256sum >"$tmp/before"
mkdir "$tmp/orig"
tar cf - -T "$tmp/list" | tar xf - -C "$tmp/orig"
key=$(cat MODULE.bazel MODULE.bazel.lock BUILD.bazel 2>/dev/null | sha256sum | cut -c1-32)
bin=$cache/gazelle/$key
cmd=update
case "${1-}" in
fix | update) cmd=$1 && shift ;;
help | update-repos) cmd= ;; # not cached: through Bazel
esac
if [ -n "$cmd" ] && [ "${GAZELLE_CACHE:-1}" != 0 ] && [ -x "$bin/gazelle" ]; then
  echo "gazelle.sh: cached gazelle $bin" >&2
  if command -v go >/dev/null; then
    GOROOT=$(go env GOROOT) && export GOROOT
  fi
  BUILD_WORKSPACE_DIRECTORY=$PWD GOTOOLCHAIN=local "$bin/gazelle" "$cmd" -bzlmod -repo_config "$bin/repo_config" "$@"
else
  "${bazel[@]}" run "${flags[@]}" //:gazelle -- ${cmd:+"$cmd"} "$@"
  # Keep the binary for next time (written aside, then renamed: concurrent jobs race safely).
  runfiles=$("${bazel[@]}" info "${flags[@]}" bazel-bin 2>/dev/null)/gazelle.runfiles
  g=$runfiles/gazelle+/cmd/gazelle/gazelle_/gazelle
  c=$runfiles/gazelle++go_deps+bazel_gazelle_go_repository_config/WORKSPACE
  if [ "${GAZELLE_CACHE:-1}" != 0 ] && [ ! -e "$bin" ] && [ -f "$g" ] && [ -f "$c" ]; then
    mkdir -p "$cache/gazelle"
    new=$(mktemp -d "$cache/gazelle/.new.XXXXXX")
    cp "$g" "$new/gazelle" && cp "$c" "$new/repo_config" && chmod 755 "$new" "$new/gazelle" &&
      mv -T "$new" "$bin" 2>/dev/null || rm -rf "$new"
  fi
fi
builds | tr '\n' '\0' | xargs -0 -r sha256sum >"$tmp/after"
# New (hash, path) pairs: changed or created files (gazelle doesn't delete BUILD files).
mapfile -t changed < <(comm -13 <(sort "$tmp/before") <(sort "$tmp/after") | sed 's/^[0-9a-f]*  //' | sort)
for f in "${changed[@]}"; do
  old=$tmp/orig/$f
  [ -e "$old" ] || old=/dev/null
  diff -u --label "a/$f" --label "b/$f" "$old" "$f" || true
done
if [ "${#changed[@]}" -gt 0 ]; then
  tar cf "$out" -- "${changed[@]}"
else
  tar cf "$out" -T /dev/null
fi
echo "gazelle.sh: ${#changed[@]} BUILD file(s) changed${changed[*]:+: ${changed[*]}} (in $out)"
if [ "${GAZELLE_CHECK:-}" = 1 ] && [ "${#changed[@]}" -gt 0 ]; then
  exit 1
fi
