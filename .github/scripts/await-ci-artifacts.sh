#!/usr/bin/env bash
# Waits for the CI run of one commit and prints its id once that run has all six
# build artifacts, so a tag release can promote them.
#
# A tag never compiles, so every other outcome is an error: no run at all, a run
# that failed or was cancelled, a run still going when the wait expires, or a run
# without the six artifacts. Retries cover a run that is not registered yet.
#
# Usage: await-ci-artifacts.sh <sha> [wait-seconds]
#   POLL_SECONDS  delay between polls (default 20)
# Needs GH_TOKEN and GITHUB_REPOSITORY, like the step that calls it.
set -euo pipefail

sha=${1:?usage: await-ci-artifacts.sh <sha> [wait-seconds]}
wait_seconds=${2:-3600}
poll=${POLL_SECONDS:-20}
repo=${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}

expected=(
  build-darwin-amd64 build-darwin-arm64
  build-linux-amd64 build-linux-arm64
  build-windows-amd64 build-windows-arm64
)

die() { echo "await-ci-artifacts: $*" >&2; exit 1; }
url_for() { echo "https://github.com/${repo}/actions/runs/$1"; }

# The push that landed this commit on main is the only run a tag may promote.
runs_url="repos/${repo}/actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=main&per_page=1"
deadline=$(( $(date +%s) + wait_seconds ))

while :; do
  if ! response=$(gh api "$runs_url" 2>&1); then
    if (( $(date +%s) >= deadline )); then
      die "cannot query CI runs for ${sha} after ${wait_seconds}s: ${response}"
    fi
    sleep "$poll"
    continue
  fi
  # python3 keeps this readable and avoids depending on gh's --jq for the wait.
  read -r run_id status conclusion <<<"$(printf '%s' "$response" | python3 -c '
import json, sys
runs = json.load(sys.stdin).get("workflow_runs") or []
run = runs[0] if runs else {}
print(run.get("id", ""), run.get("status", ""), run.get("conclusion") or "")
')"

  if [ -z "$run_id" ]; then
    if (( $(date +%s) >= deadline )); then
      die "no CI push run on main for ${sha} after ${wait_seconds}s; a tag promotes CI artifacts and never compiles (use workflow_dispatch with rebuild to compile instead)"
    fi
    echo "await-ci-artifacts: no CI run for ${sha} yet; waiting" >&2
    sleep "$poll"
    continue
  fi

  if [ "$status" != "completed" ]; then
    if (( $(date +%s) >= deadline )); then
      die "CI run ${run_id} is still ${status} after ${wait_seconds}s: $(url_for "$run_id")"
    fi
    echo "await-ci-artifacts: CI run ${run_id} is ${status}; waiting" >&2
    sleep "$poll"
    continue
  fi

  [ "$conclusion" = "success" ] ||
    die "CI run ${run_id} concluded ${conclusion}: $(url_for "$run_id")"
  break
done

if ! response=$(gh api "repos/${repo}/actions/runs/${run_id}/artifacts?per_page=100" 2>&1); then
  die "cannot list the artifacts of CI run ${run_id}: ${response}"
fi
if ! problem=$(printf '%s' "$response" | EXPECTED="${expected[*]}" python3 -c '
import json, os, sys
expected = os.environ["EXPECTED"].split()
artifacts = {a["name"]: bool(a.get("expired")) for a in json.load(sys.stdin).get("artifacts") or []}
problems = []
missing = [name for name in expected if name not in artifacts]
expired = [name for name in expected if artifacts.get(name)]
if missing:
    problems.append("missing " + ", ".join(missing))
if expired:
    problems.append("expired " + ", ".join(expired))
print("; ".join(problems))
sys.exit(1 if problems else 0)
'); then
  die "CI run ${run_id} does not have six usable build artifacts (${problem}): $(url_for "$run_id")"
fi

echo "await-ci-artifacts: CI run ${run_id} has all six artifacts" >&2
echo "$run_id"
