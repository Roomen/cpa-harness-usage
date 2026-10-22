#!/usr/bin/env bash
# Checks a darwin plugin library before it is packaged.
#
# Usage: scripts/check-darwin.sh <library> <goarch> <min-macos>
#   - is a macOS library (LC_BUILD_VERSION platform)
#   - targets <min-macos> (LC_BUILD_VERSION minos), not the build machine's macOS
#   - exports cliproxy_plugin_init
#   - amd64 only: keeps g in TLS slot 11 (0x58(GS)) and never in slot 6 (0x30(GS)), see
#     scripts/darwin-amd64-go.sh
set -euo pipefail

usage() { echo "check-darwin: usage: check-darwin.sh <library> <goarch> <min-macos>" >&2; exit 2; }
[ $# -eq 3 ] || usage
lib=$1
arch=$2
min=$3
die() { echo "check-darwin: $lib: $*" >&2; exit 1; }

[ -f "$lib" ] || die "no such library"
build=$(vtool -show-build "$lib")
platform=$(printf '%s\n' "$build" | awk '/platform/ { print $2 }')
got=$(printf '%s\n' "$build" | awk '/minos/ { print $2 }')
[ "$platform" = MACOS ] || die "platform ${platform:-unknown}, want MACOS"
[ "$got" = "$min" ] || die "targets macOS $got, want $min"
nm -gU "$lib" | grep -q ' T _cliproxy_plugin_init$' || die "does not export cliproxy_plugin_init"

if [ "$arch" = amd64 ]; then
  counts=$(otool -tv "$lib" | awk '/%gs:0x30/ { s6++ } /%gs:0x58/ { s11++ } END { printf "%d %d", s6, s11 }')
  read -r s6 s11 <<<"$counts"
  [ "$s6" = 0 ] && [ "$s11" -gt 0 ] || die "g must live in TLS slot 11 only (0x30(GS): $s6, 0x58(GS): $s11)"
  echo "check-darwin: $lib: minos $got, TLS slot 11 ($s11 accesses, none to slot 6)"
else
  echo "check-darwin: $lib: minos $got"
fi
