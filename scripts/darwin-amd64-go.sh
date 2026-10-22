#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
cache_root=${1:-"$script_dir/../.cache/tlsgo"}
go_version=${2:-$(go env GOVERSION)}
fail() { echo "darwin-amd64-go: $*" >&2; exit 1; }

[[ $(uname -s) == Darwin ]] || fail "a macOS build host is required"
[[ $go_version =~ ^go1\.[0-9]+\.[0-9]+$ ]] || fail "an exact stable Go version is required"
script_hash=$(shasum -a 256 "$script_dir/darwin-amd64-go.sh" | awk '{print $1}')
host_arch=$(uname -m)
mkdir -p "$cache_root"
cache_root=$(cd "$cache_root" && pwd)
cache_entry="$cache_root/$go_version-$host_arch-$script_hash"
cache_marker="$go_version:$host_arch:$script_hash"

# A run killed after mktemp leaves a large staging directory behind, and CI
# saves the whole cache directory even on cancelled runs. Prune the ones that
# cannot belong to a live build: a live build refreshes its own directory from a
# heartbeat, so an old mtime means nobody has been working in it. Raise the age
# on runners where a toolchain build can take longer than an hour.
staging_max_age=${DARWIN_AMD64_GO_STAGING_MAX_AGE:-3600}
prune_staging() {
  local now dir mtime
  now=$(date +%s)
  for dir in "$cache_root"/.build.*; do
    [[ -d "$dir" ]] || continue
    mtime=$(stat -f %m "$dir" 2>/dev/null) || mtime=0
    if (( now - mtime >= staging_max_age )); then
      echo "darwin-amd64-go: removing stale staging directory $(basename "$dir")" >&2
      rm -rf "$dir"
    fi
  done
}

# Only nested directories change while a toolchain builds, so the staging
# directory itself would look idle for hours without an explicit heartbeat.
# The loop watches the process that started it: a SIGKILLed parent never runs
# its EXIT trap, and a heartbeat left behind would keep a dead staging directory
# looking alive forever, defeating the prune above.
# It also drops the lock fd (9): it outlives the script by up to one interval,
# and a heartbeat holding the fd would keep the kernel lock after the build is
# gone. The build itself may keep fd 9, because it is still writing into the
# cache entry while it runs.
staging_heartbeat=${DARWIN_AMD64_GO_STAGING_HEARTBEAT:-60}
heartbeat=""
start_heartbeat() {
  local parent=$$
  ( while kill -0 "$parent" 2>/dev/null && [[ -d "$staging" ]]; do
      touch "$staging" 2>/dev/null
      sleep "$staging_heartbeat"
    done ) >/dev/null 2>&1 9>&- &
  heartbeat=$!
  # Otherwise bash reports the killed job on stderr later in the run.
  disown "$heartbeat" 2>/dev/null || true
}
stop_heartbeat() {
  [[ -n $heartbeat ]] || return 0
  kill "$heartbeat" 2>/dev/null || true
  heartbeat=""
}

if [[ -x "$cache_entry/go/bin/go" ]] && [[ $(cat "$cache_entry/complete" 2>/dev/null) == "$cache_marker" ]]; then
  echo "$cache_entry/go"
  exit 0
fi
[[ ! -e "$cache_entry" ]] || fail "incomplete cache entry; remove it before rebuilding"

# The lock must never live inside the cache directory: CI restores and saves
# that directory even when a run is cancelled, so a lock left behind by a killed
# run would be restored forever and fail every later run.
lock_root=${DARWIN_AMD64_GO_LOCK_DIR:-${TMPDIR:-/tmp}}
mkdir -p "$lock_root"
lock_file="$lock_root/darwin-amd64-go-$(printf '%s' "$cache_marker:$cache_root" | shasum -a 256 | awk '{print $1}' | cut -c1-16).lock"
# Exclusion is an flock(2) the kernel holds for as long as this build lives, not
# a directory that a later run reads and then deletes. Reclaiming a lock by
# inspecting it first is unavoidably racy: the liveness check and the removal are
# separate steps, so two runs that both see a dead owner can each remove the
# other's freshly taken lock and then build the same entry in parallel. The
# kernel drops this lock when the build dies, however it dies, so there is
# nothing to reclaim, nothing to steal, and no PID whose reuse could matter.
# lockf(1) is the only way to take an flock from sh: it locks fd 9 and exits, and
# the lock lives in the shared open file description, so it lasts until this
# script and every child that inherited fd 9 are gone.
command -v lockf >/dev/null || fail "lockf(1) is required to serialise toolchain builds"
: >>"$lock_file" 2>/dev/null || fail "cannot create toolchain lock file ($lock_file)"
exec 9>>"$lock_file"
lockf_rc=0
lockf -s -t 0 9 || lockf_rc=$?
if (( lockf_rc == 75 )); then
  fail "another build owns this toolchain cache entry ($lock_file)"
elif (( lockf_rc != 0 )); then
  fail "cannot acquire toolchain lock ($lock_file, lockf exit $lockf_rc)"
fi
# Prune only while holding the lock: no other build of this entry can be in
# flight, and any other live build keeps its own directory fresh via heartbeat.
# Another build can also finish between the cache check above and the lock, so
# look again before pruning or rebuilding a toolchain that already exists.
if [[ -x "$cache_entry/go/bin/go" ]] && [[ $(cat "$cache_entry/complete" 2>/dev/null) == "$cache_marker" ]]; then
  echo "$cache_entry/go"
  exit 0
fi
prune_staging
staging=""
trap 'stop_heartbeat; if [[ -n "$staging" ]]; then rm -rf "$staging"; fi' EXIT
staging=$(mktemp -d "$cache_root/.build.XXXXXXXX")
start_heartbeat
curl --fail --silent --show-error --location 'https://go.dev/dl/?mode=json&include=all' -o "$staging/releases.json"
source_sha=$(python3 -I - "$staging/releases.json" "$go_version" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    releases = json.load(source)
matches = [entry["sha256"] for release in releases if release["version"] == sys.argv[2]
           for entry in release["files"] if entry["kind"] == "source"]
if len(matches) != 1:
    raise SystemExit("expected exactly one official Go source checksum")
print(matches[0])
PY
)
mkdir -p "$cache_root/downloads"
source_archive="$cache_root/downloads/$go_version.src.tar.gz"
if [[ ! -f "$source_archive" ]] || ! echo "$source_sha  $source_archive" | shasum -a 256 -c - >/dev/null 2>&1; then
  curl --fail --silent --show-error --location "https://go.dev/dl/$go_version.src.tar.gz" -o "$staging/source.tar.gz"
  echo "$source_sha  $staging/source.tar.gz" | shasum -a 256 -c - >&2
  mv "$staging/source.tar.gz" "$source_archive"
fi
tar -xzf "$source_archive" -C "$staging"
python3 -I - "$staging/go" <<'PY'
from pathlib import Path
import sys

root = Path(sys.argv[1])
replacements = {
    "src/cmd/link/internal/ld/sym.go": ("ctxt.Tlsoffset = 0x30", "ctxt.Tlsoffset = 0x58"),
    "src/runtime/sys_darwin_amd64.s": ("MOVQ\tDX, 0x30(GS)", "MOVQ\tDX, 0x58(GS)"),
}
for relative, (before, after) in replacements.items():
    path = root / relative
    content = path.read_text(encoding="utf-8")
    if content.count(before) != 1:
        raise SystemExit(f"unsupported Go source layout: {relative}")
    path.write_text(content.replace(before, after), encoding="utf-8")
PY
bootstrap=$(go env GOROOT)
if ! (cd "$staging/go/src" && env -u GOTOOLCHAIN -u GOFLAGS -u GOOS -u GOARCH -u GOROOT \
    -u CGO_CFLAGS -u CGO_LDFLAGS -u MACOSX_DEPLOYMENT_TARGET \
    GOROOT_BOOTSTRAP="$bootstrap" ./make.bash >"$staging/build.log" 2>&1); then
  cp "$staging/build.log" "$cache_root/failed-$go_version-$host_arch.log"
  tail -40 "$staging/build.log" >&2
  fail "toolchain build failed; build log retained in cache"
fi
stop_heartbeat
echo "$cache_marker" > "$staging/complete"
# mv onto an existing directory nests instead of replacing it, which would hide
# a hundred megabytes of build garbage inside a valid-looking cache entry.
if [[ -x "$cache_entry/go/bin/go" ]] && [[ $(cat "$cache_entry/complete" 2>/dev/null) == "$cache_marker" ]]; then
  echo "darwin-amd64-go: another build completed this entry first" >&2
  echo "$cache_entry/go"
  exit 0
fi
[[ ! -e "$cache_entry" ]] || fail "cache entry $cache_entry already exists; remove it before rebuilding"
mv "$staging" "$cache_entry"
staging=""
trap - EXIT
echo "$cache_entry/go"
