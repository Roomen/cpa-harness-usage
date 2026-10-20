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
if [[ -x "$cache_entry/go/bin/go" ]] && [[ $(cat "$cache_entry/complete" 2>/dev/null) == "$cache_marker" ]]; then
  echo "$cache_entry/go"
  exit 0
fi
[[ ! -e "$cache_entry" ]] || fail "incomplete cache entry; remove it before rebuilding"

lock_dir="$cache_entry.lock"
mkdir "$lock_dir" 2>/dev/null || fail "another build owns this toolchain cache entry"
staging=""
trap 'if [[ -n "$staging" ]]; then rm -rf "$staging"; fi; rmdir "$lock_dir"' EXIT
staging=$(mktemp -d "$cache_root/.build.XXXXXXXX")
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
echo "$cache_marker" > "$staging/complete"
mv "$staging" "$cache_entry"
staging=""
rmdir "$lock_dir"
trap - EXIT
echo "$cache_entry/go"
