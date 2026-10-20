#!/usr/bin/env bash
set -euo pipefail

version=0.15.2
case "$GOARCH" in
  amd64)
    zig_arch=x86_64
    archive_hash=02aa270f183da276e5b5920b1dac44a63f1a49e55050ebde3aecc9eb82f93239
    ;;
  arm64)
    zig_arch=aarch64
    archive_hash=958ed7d1e00d0ea76590d27666efbf7a932281b3d7ba0c6b01b0ff26498f667f
    ;;
  *) printf 'Unsupported Linux toolchain architecture\n' >&2; exit 1 ;;
esac
asset="zig-${zig_arch}-linux-${version}"
tools="$GITHUB_WORKSPACE/.tools"
toolchain="$tools/$asset"
if [ ! -x "$toolchain/zig" ]; then
  mkdir -p "$tools"
  archive="$RUNNER_TEMP/$asset.tar.xz"
  curl -fsSL --retry 3 "https://ziglang.org/download/$version/$asset.tar.xz" -o "$archive"
  printf '%s  %s\n' "$archive_hash" "$archive" | sha256sum --check --status
  tar -xJf "$archive" -C "$tools"
  rm -f "$archive"
fi
test "$("$toolchain/zig" version)" = "$version"
printf 'PLUGIN_CC=%s/zig cc -target %s-linux-gnu.2.17\n' "$toolchain" "$zig_arch" >> "$GITHUB_ENV"
printf 'PLUGIN_ZIG_GLOBAL_CACHE_DIR=%s/.cache/zig-build-%s\n' "$GITHUB_WORKSPACE" "$GOARCH" >> "$GITHUB_ENV"
