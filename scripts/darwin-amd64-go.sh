#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "$0")" && pwd)
cache_root=${1:-"$script_dir/../.cache/tlsgo"}
go_version=${2:-$(go env GOVERSION)}
fail() { echo "darwin-amd64-go: $*" >&2; exit 1; }
log() { echo "darwin-amd64-go: $*" >&2; }

[[ $(uname -s) == Darwin ]] || fail "a macOS build host is required"
[[ $go_version =~ ^go1\.[0-9]+\.[0-9]+$ ]] || fail "an exact stable Go version is required"
# A toolchain identity is exactly what changes the produced binaries: the Go
# version, the host architecture, the cache layout schema, and the TLS patch
# itself. The lock, the heartbeat, pruning and logging in this script are
# deliberately not part of it, so maintaining that code never throws a verified
# toolchain away. The patch lives in its own data file so its hash follows the
# bytes that are applied instead of a hand-maintained revision number.
patch_file="$script_dir/darwin-amd64-go.patch.json"
[[ -f $patch_file ]] || fail "missing patch data $patch_file"
patch_hash=$(shasum -a 256 "$patch_file" | awk '{print $1}')
# The schema versions the cache layout together with the patch format, and it
# lives in the patch data so that bumping it also changes the hash the CI cache
# key is built from. A value this script does not implement must fail here, not
# rebuild an entry that nothing will ever hit.
patch_schema=$(python3 -I - "$patch_file" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as source:
    print(json.load(source)["schema"])
PY
)
[[ $patch_schema == 1 ]] || fail "patch data schema $patch_schema is not supported by this script"
host_arch=$(uname -m)
# The toolchain is built for the host, so a hit must report the host ABI: an
# amd64 toolchain is executable under Rosetta on an arm64 host, which would
# otherwise pass a version-only check and poison the build.
case $host_arch in
  x86_64) host_abi=amd64 ;;
  arm64) host_abi=arm64 ;;
  *) fail "unsupported host architecture $host_arch" ;;
esac
cache_id="$go_version-$host_arch-s$patch_schema-$patch_hash"
cache_marker="$go_version:$host_arch:s$patch_schema:$patch_hash"
mkdir -p "$cache_root"
cache_root=$(cd "$cache_root" && pwd)
cache_entry="$cache_root/$cache_id"

# A run killed after mktemp leaves a large staging directory behind, and CI
# saves the whole cache directory even on cancelled runs. Prune the ones that
# cannot belong to a live build: a live build refreshes its own directory from a
# heartbeat, so an old mtime means nobody has been working in it. Raise the age
# on runners where a toolchain build can take longer than an hour.
staging_max_age=${DARWIN_AMD64_GO_STAGING_MAX_AGE:-3600}
prune_staging() {
  local now dir mtime
  now=$(date +%s)
  for dir in "$cache_root"/.build.* "$cache_root"/.migrate.*; do
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

# A cached toolchain is usable only when it finished, carries the marker of this
# identity, and its own binary runs and reports the exact Go version. None of
# this needs the network or a bootstrap compiler.
toolchain_version() {
  env -u GOFLAGS -u GOOS -u GOARCH -u GOROOT GOTOOLCHAIN=local "$1/go/bin/go" version 2>/dev/null
}
entry_usable() {
  local dir=$1 marker=$2
  [[ -x "$dir/go/bin/go" ]] || return 1
  [[ $(cat "$dir/complete" 2>/dev/null) == "$marker" ]] || return 1
  [[ $(toolchain_version "$dir") == "go version $go_version darwin/$host_abi" ]] || return 1
}

# A marker only records what this script claims, so a migrated entry is checked
# against the patch data itself: every "to" appears exactly once in its own
# source tree and every "from" is gone. A source tree that still has slot 6 is
# rejected, whatever its marker says.
patch_applied() {
  python3 -I - "$1/go" "$patch_file" <<'PY'
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
with open(sys.argv[2], encoding="utf-8") as source:
    patch = json.load(source)
for replacement in patch["replacements"]:
    try:
        content = (root / replacement["file"]).read_text(encoding="utf-8")
    except OSError:
        sys.exit(1)
    if content.count(replacement["to"]) != 1 or replacement["from"] in content:
        sys.exit(1)
PY
}

if entry_usable "$cache_entry" "$cache_marker"; then
  log "cache hit $cache_id"
  echo "$cache_entry/go"
  exit 0
fi

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
# Another build can have finished this entry, or a previous run can have been
# killed before it published, so look again under the lock: a complete entry is
# reused, anything else under this identity is discarded and rebuilt.
if entry_usable "$cache_entry" "$cache_marker"; then
  log "cache hit $cache_id (completed while waiting for the lock)"
  echo "$cache_entry/go"
  exit 0
fi
if [[ -e $cache_entry ]]; then
  log "discarding incomplete cache entry $cache_id"
  rm -rf "$cache_entry"
fi
staging=""
migration_staging=""
trap 'stop_heartbeat; if [[ -n "$staging" ]]; then rm -rf "$staging"; fi; if [[ -n "$migration_staging" ]]; then rm -rf "$migration_staging"; fi' EXIT

# The toolchain released as v0.1.5 was built by the script at 064af60 with
# exactly this patch, and its entry sits under the old identity (the raw hash of
# that script). Copy it to the current identity instead of compiling it again.
# Each pair is literal - the released script hash and the patch it applied - and
# nothing is read from git at run time. A different patch, a different script, a
# wrong marker or an incomplete entry never matches, so nothing is adopted on
# trust. The old entry is copied, never moved or rewritten: a build started
# before this change may still be running out of it.
legacy_identities=(
  "f350b7956bfb197dcf8294186944926e5b3c98e49a77d1febd9cfc63985d84ee:f41aca582950e38084826b04012300817c6956780897e7a243c8de03b05f3661"
)
migrate_released_toolchain() {
  local pair legacy_sha legacy_patch legacy_entry legacy_marker
  for pair in "${legacy_identities[@]}"; do
    legacy_sha=${pair%%:*}
    legacy_patch=${pair##*:}
    if [[ $legacy_patch != "$patch_hash" ]]; then
      log "not migrating $legacy_sha: it was built with a different patch"
      continue
    fi
    legacy_entry="$cache_root/$go_version-$host_arch-$legacy_sha"
    legacy_marker="$go_version:$host_arch:$legacy_sha"
    [[ -d $legacy_entry ]] || continue
    if ! entry_usable "$legacy_entry" "$legacy_marker"; then
      log "not migrating $(basename "$legacy_entry"): not a complete toolchain of that identity"
      continue
    fi
    if ! patch_applied "$legacy_entry"; then
      log "not migrating $(basename "$legacy_entry"): the TLS patch is not applied to its source"
      continue
    fi
    log "migrating $(basename "$legacy_entry") to $cache_id (copy; the old entry is kept)"
    migration_staging=$(mktemp -d "$cache_root/.migrate.XXXXXXXX") || { migration_staging=""; continue; }
    if cp -R "$legacy_entry/." "$migration_staging/" \
        && echo "$cache_marker" >"$migration_staging/complete" \
        && entry_usable "$migration_staging" "$cache_marker" \
        && patch_applied "$migration_staging" \
        && [[ ! -e $cache_entry ]] \
        && mv "$migration_staging" "$cache_entry"; then
      migration_staging=""
      return 0
    fi
    log "migration failed; rebuilding instead"
    rm -rf "$migration_staging"
    migration_staging=""
  done
  return 1
}
migrate_released_toolchain || true
if entry_usable "$cache_entry" "$cache_marker"; then
  log "cache hit $cache_id (migrated from the released toolchain)"
  echo "$cache_entry/go"
  exit 0
fi

# Nothing reusable is left under this identity, so this run compiles a toolchain.
log "cache miss $cache_id; building go$go_version for $host_arch with patch ${patch_hash:0:12}"
# Prune only while holding the lock: no other build of this entry can be in
# flight, and any other live build keeps its own directory fresh via heartbeat.
prune_staging
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
python3 -I - "$staging/go" "$patch_file" <<'PY'
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
with open(sys.argv[2], encoding="utf-8") as source:
    patch = json.load(source)
if patch.get("schema") != 1:
    raise SystemExit(f"unsupported patch schema {patch.get('schema')!r}")
for replacement in patch["replacements"]:
    relative = replacement["file"]
    path = root / relative
    content = path.read_text(encoding="utf-8")
    if content.count(replacement["from"]) != 1:
        raise SystemExit(f"unsupported Go source layout: {relative}")
    path.write_text(content.replace(replacement["from"], replacement["to"]), encoding="utf-8")
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
# Publish only what a later hit would accept. A build that reports another Go
# version or host ABI, or whose source did not take the patch, must never become
# an entry: CI saves the cache right after this step succeeds, so a wrong
# toolchain would then be served to every later run. The EXIT trap removes the
# staging directory on this failure.
if ! entry_usable "$staging" "$cache_marker"; then
  fail "the built toolchain does not report go$go_version darwin/$host_abi; not caching it"
fi
if ! patch_applied "$staging"; then
  fail "the built toolchain source did not take the TLS patch; not caching it"
fi
# mv onto an existing directory nests instead of replacing it, which would hide
# a hundred megabytes of build garbage inside a valid-looking cache entry.
if entry_usable "$cache_entry" "$cache_marker"; then
  log "another build completed this entry first"
  echo "$cache_entry/go"
  exit 0
fi
[[ ! -e "$cache_entry" ]] || fail "cache entry $cache_entry already exists; remove it before rebuilding"
mv "$staging" "$cache_entry"
staging=""
trap - EXIT
echo "$cache_entry/go"
