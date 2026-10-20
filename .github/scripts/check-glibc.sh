#!/usr/bin/env bash
set -euo pipefail

versions=$(readelf --version-info "$1" | sed -n 's/.*Name: GLIBC_\([0-9.]*\).*/\1/p' | sort -Vu)
if [ -z "$versions" ]; then
  printf 'Expected GLIBC version requirements in Linux plugin\n' >&2
  exit 1
fi
maximum=$(printf '%s\n' "$versions" | tail -n 1)
printf 'Maximum required GLIBC version: %s\n' "$maximum"
if [ "$(printf '2.17\n%s\n' "$maximum" | sort -V | tail -n 1)" != 2.17 ]; then
  printf 'Linux plugin exceeds GLIBC 2.17 baseline\n' >&2
  exit 1
fi
