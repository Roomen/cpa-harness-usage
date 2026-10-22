#!/usr/bin/env python3
"""Synthetic tests for the tag release path: .github/scripts/await-ci-artifacts.sh
and the `Find reusable CI artifacts` step of .github/workflows/release.yml.

No workflow runs and nothing is compiled: `gh` is a shim that replays canned API
responses, and the step scripts are executed locally with bash.

Cases:
  1  a queued run is followed to success and reused
  2  a failed CI run fails the release with its link
  3  a cancelled CI run fails the release with its link
  4  a missing build artifact fails the release
  5  an expired build artifact fails the release
  6  a run that is not registered yet is retried, then reused
  7  no CI run at all times out without compiling
  8  a run still queued at the deadline times out without compiling
  9  tag step: CI success promotes, no CI fails and never compiles
 10  tag step: a dev-version tag still waits instead of compiling
 11  manual step: fallback build and explicit rebuild still compile
 12  release.yml keeps a prepare timeout and the manual fallback
"""

import json
import os
import shutil
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
AWAIT = os.path.join(ROOT, ".github", "scripts", "await-ci-artifacts.sh")
RELEASE = os.path.join(ROOT, ".github", "workflows", "release.yml")
SHA = "0123456789abcdef0123456789abcdef01234567"
RUN_ID = 1234
SIX = [
    "build-darwin-amd64", "build-darwin-arm64",
    "build-linux-amd64", "build-linux-arm64",
    "build-windows-amd64", "build-windows-arm64",
]

FAKE_GH = '''#!/usr/bin/env python3
import json, os, re, subprocess, sys

state = os.environ["FAKE_GH_STATE"]
with open(os.path.join(state, "scenario.json"), encoding="utf-8") as fh:
    scenario = json.load(fh)
args = sys.argv[1:]
with open(os.path.join(state, "calls.log"), "a", encoding="utf-8") as fh:
    fh.write(" ".join(args) + "\\n")

if not args or args[0] != "api":
    sys.exit("fake gh: only 'api' is supported")
path = args[1]
jq = args[args.index("--jq") + 1] if "--jq" in args else None


def take(name):
    file = os.path.join(state, name)
    value = int(open(file).read()) if os.path.exists(file) else 0
    with open(file, "w") as fh:
        fh.write(str(value + 1))
    return value


if "/actions/workflows/ci.yml/runs" in path:
    responses = scenario["runs"]
    payload = responses[min(take("runs.calls"), len(responses) - 1)]
elif (match := re.search(r"/actions/runs/(\\d+)/artifacts", path)):
    payload = scenario["artifacts"][match.group(1)]
else:
    sys.exit("fake gh: unexpected path " + path)

text = json.dumps(payload)
if jq is None:
    print(text)
    sys.exit(0)
result = subprocess.run(["jq", "-r", jq], input=text, capture_output=True, text=True)
sys.stdout.write(result.stdout)
sys.stderr.write(result.stderr)
sys.exit(result.returncode)
'''


class Failure(Exception):
    pass


def check(condition, message):
    if not condition:
        raise Failure(message)


def runs(*items):
    return {"workflow_runs": list(items)}


def run(run_id=RUN_ID, status="completed", conclusion="success"):
    return {"id": run_id, "status": status, "conclusion": conclusion}


def artifacts(names, expired=()):
    return {"artifacts": [{"name": name, "expired": name in expired} for name in names]}


class Box:
    def __init__(self):
        self.root = os.path.realpath(tempfile.mkdtemp(prefix="await-ci-test."))
        self.bin = os.path.join(self.root, "bin")
        self.state = os.path.join(self.root, "state")
        self.checkout = os.path.join(self.root, "checkout")
        for path in (self.bin, self.state, os.path.join(self.checkout, ".github", "scripts")):
            os.makedirs(path, exist_ok=True)
        gh = os.path.join(self.bin, "gh")
        with open(gh, "w", encoding="utf-8") as fh:
            fh.write(FAKE_GH)
        os.chmod(gh, 0o755)
        shutil.copy(AWAIT, os.path.join(self.checkout, ".github", "scripts"))

    def close(self):
        shutil.rmtree(self.root, ignore_errors=True)

    def scenario(self, responses, artifact_map=None):
        with open(os.path.join(self.state, "scenario.json"), "w", encoding="utf-8") as fh:
            json.dump({"runs": responses, "artifacts": artifact_map or {}}, fh)
        for name in ("runs.calls", "calls.log"):
            path = os.path.join(self.state, name)
            if os.path.exists(path):
                os.remove(path)

    def env(self, extra=None):
        env = {
            "PATH": self.bin + ":/usr/bin:/bin:/usr/sbin:/sbin",
            "GITHUB_REPOSITORY": "owner/repo",
            "GH_TOKEN": "fake",
            "FAKE_GH_STATE": self.state,
            "POLL_SECONDS": "0",
        }
        env.update(extra or {})
        return env

    def calls(self):
        path = os.path.join(self.state, "calls.log")
        if not os.path.exists(path):
            return []
        return open(path, encoding="utf-8").read().splitlines()

    def await_ci(self, wait_seconds=1):
        return subprocess.run(
            ["bash", AWAIT, SHA, str(wait_seconds)],
            env=self.env(), capture_output=True, text=True, timeout=180,
        )

    def step(self, event, version="0.1.5", rebuild="false", wait_seconds=1):
        script = step_script(RELEASE, "Find reusable CI artifacts")
        path = os.path.join(self.root, "step.sh")
        with open(path, "w", encoding="utf-8") as fh:
            fh.write(script)
        output = os.path.join(self.root, "output")
        summary = os.path.join(self.root, "summary")
        open(output, "w").close()
        open(summary, "w").close()
        env = self.env({
            "EVENT": event,
            "VERSION": version,
            "REBUILD": rebuild,
            "WAIT_SECONDS": str(wait_seconds),
            "GITHUB_SHA": SHA,
            "GITHUB_OUTPUT": output,
            "GITHUB_STEP_SUMMARY": summary,
        })
        result = subprocess.run(
            ["bash", "-eo", "pipefail", path],
            cwd=self.checkout, env=env, capture_output=True, text=True, timeout=180,
        )
        return result, open(output, encoding="utf-8").read(), open(summary, encoding="utf-8").read()


def step_script(path, name):
    """Returns the run: block of one step, without depending on a YAML library."""
    lines = open(path, encoding="utf-8").read().splitlines()
    start = next(i for i, line in enumerate(lines) if line.strip() == f"- name: {name}")
    indent = len(lines[start]) - len(lines[start].lstrip())
    block = []
    for line in lines[start + 1:]:
        if line.strip() and len(line) - len(line.lstrip()) <= indent:
            break
        block.append(line)
    run_at = next(i for i, line in enumerate(block) if line.strip() == "run: |")
    body_indent = len(block[run_at + 1]) - len(block[run_at + 1].lstrip())
    body = []
    for line in block[run_at + 1:]:
        if line.strip() and len(line) - len(line.lstrip()) < body_indent:
            break
        body.append(line[body_indent:] if line.strip() else "")
    return "\n".join(body) + "\n"


def case_queued_then_success(box):
    box.scenario([runs(run(status="queued")), runs(run(status="in_progress")),
                  runs(run())], {str(RUN_ID): artifacts(SIX)})
    result = box.await_ci(wait_seconds=60)
    check(result.returncode == 0, f"failed: {result.stderr}")
    check(result.stdout.strip() == str(RUN_ID), f"unexpected id {result.stdout!r}")
    check("is queued; waiting" in result.stderr, f"did not wait: {result.stderr}")
    queries = [line for line in box.calls() if "ci.yml/runs" in line]
    check(len(queries) >= 3, f"expected several polls, saw {queries}")
    for line in queries:
        check(f"head_sha={SHA}" in line, f"missing head_sha: {line}")
        check("event=push" in line, f"missing event filter: {line}")
        check("branch=main" in line, f"missing branch filter: {line}")


def case_ci_failure(box):
    box.scenario([runs(run(conclusion="failure"))])
    result = box.await_ci()
    check(result.returncode != 0, "a failed CI run was accepted")
    check("concluded failure" in result.stderr, f"unexpected message: {result.stderr}")
    check(f"https://github.com/owner/repo/actions/runs/{RUN_ID}" in result.stderr,
          f"no run link: {result.stderr}")


def case_ci_cancelled(box):
    box.scenario([runs(run(conclusion="cancelled"))])
    result = box.await_ci()
    check(result.returncode != 0, "a cancelled CI run was accepted")
    check("concluded cancelled" in result.stderr, f"unexpected message: {result.stderr}")


def case_missing_artifact(box):
    box.scenario([runs(run())], {str(RUN_ID): artifacts(SIX[:5])})
    result = box.await_ci()
    check(result.returncode != 0, "a run without six artifacts was accepted")
    check("missing build-windows-arm64" in result.stderr, f"unexpected message: {result.stderr}")


def case_expired_artifact(box):
    box.scenario([runs(run())], {str(RUN_ID): artifacts(SIX, expired=["build-linux-arm64"])})
    result = box.await_ci()
    check(result.returncode != 0, "an expired artifact was accepted")
    check("expired build-linux-arm64" in result.stderr, f"unexpected message: {result.stderr}")


def case_late_run(box):
    box.scenario([runs(), runs(), runs(run())], {str(RUN_ID): artifacts(SIX)})
    result = box.await_ci(wait_seconds=60)
    check(result.returncode == 0, f"failed: {result.stderr}")
    check(result.stdout.strip() == str(RUN_ID), f"unexpected id {result.stdout!r}")
    check("no CI run" in result.stderr, f"did not report the wait: {result.stderr}")


def case_no_ci_timeout(box):
    box.scenario([runs()])
    result = box.await_ci(wait_seconds=1)
    check(result.returncode != 0, "a missing CI run was accepted")
    check("no CI push run on main" in result.stderr, f"unexpected message: {result.stderr}")
    check("never compiles" in result.stderr, f"unexpected message: {result.stderr}")


def case_stuck_queue_timeout(box):
    box.scenario([runs(run(status="queued"))])
    result = box.await_ci(wait_seconds=1)
    check(result.returncode != 0, "a run that never finished was accepted")
    check("still queued" in result.stderr, f"unexpected message: {result.stderr}")
    check(f"actions/runs/{RUN_ID}" in result.stderr, f"no run link: {result.stderr}")


def case_tag_step(box):
    box.scenario([runs(run())], {str(RUN_ID): artifacts(SIX)})
    result, output, summary = box.step("push")
    check(result.returncode == 0, f"tag step failed on a good run: {result.stderr}")
    check(f"run_id={RUN_ID}" in output, f"run id not published: {output!r}")
    check("promoting artifacts" in summary, f"unexpected summary: {summary!r}")

    box.scenario([runs()])
    result, output, summary = box.step("push")
    check(result.returncode != 0, "tag step accepted a commit without CI")
    check("run_id=\n" in output or output.strip() == "run_id=", f"run id published: {output!r}")
    check("compiling every target" not in summary, f"tag path fell back to a build: {summary!r}")


def case_dev_tag_step(box):
    """A tag whose version skips the version check must not reach a rebuild."""
    box.scenario([runs()])
    result, output, summary = box.step("push", version="0.0.0-dev")
    check(result.returncode != 0, "a dev-version tag was allowed to compile")
    check(output.strip() == "run_id=", f"run id published: {output!r}")
    check("compiling every target" not in summary, f"tag path fell back to a build: {summary!r}")
    check("no CI push run on main" in result.stderr, f"unexpected message: {result.stderr}")

    box.scenario([runs(run())], {str(RUN_ID): artifacts(SIX)})
    result, output, summary = box.step("push", version="0.0.0-dev")
    check(result.returncode == 0, f"dev-version tag did not promote: {result.stderr}")
    check(f"run_id={RUN_ID}" in output, f"run id not published: {output!r}")


def case_manual_step(box):
    box.scenario([runs()])
    result, output, summary = box.step("workflow_dispatch")
    check(result.returncode == 0, f"manual step failed: {result.stderr}")
    check(output.strip() == "run_id=", f"manual step reused something: {output!r}")
    check("compiling every target" in summary, f"no fallback build: {summary!r}")

    box.scenario([runs()])
    result, output, summary = box.step("workflow_dispatch", rebuild="true")
    check(result.returncode == 0, f"rebuild step failed: {result.stderr}")
    check(output.strip() == "run_id=", f"rebuild step reused something: {output!r}")
    check("rebuild requested" in summary, f"no rebuild notice: {summary!r}")
    check(box.calls() == [], f"rebuild still queried CI: {box.calls()}")

    box.scenario([runs()])
    result, output, summary = box.step("workflow_dispatch", version="0.0.0-dev")
    check(result.returncode == 0, f"dev step failed: {result.stderr}")
    check("rebuild requested" in summary, f"dev dispatch did not build: {summary!r}")

    if shutil.which("jq") is None:
        print("     (manual reuse case skipped: jq is not installed)")
        return
    box.scenario([runs(run())], {str(RUN_ID): artifacts(SIX)})
    result, output, summary = box.step("workflow_dispatch")
    check(result.returncode == 0, f"manual reuse failed: {result.stderr}")
    check(f"run_id={RUN_ID}" in output, f"manual reuse did not promote: {output!r}")
    check("reusing build artifacts" in summary, f"unexpected summary: {summary!r}")


def case_workflow_shape(box):
    text = open(RELEASE, encoding="utf-8").read()
    lines = text.splitlines()
    start = lines.index("  prepare:")
    block = []
    for line in lines[start + 1:]:
        if line.startswith("  ") and not line.startswith("   ") and line.strip() and \
                len(line) - len(line.lstrip()) == 2:
            break
        block.append(line)
    check(any(line.strip().startswith("timeout-minutes:") for line in block),
          "prepare has no timeout")
    script = step_script(RELEASE, "Find reusable CI artifacts")
    check("await-ci-artifacts.sh" in script, "the tag path does not wait for CI")
    check('if [ "$EVENT" = "push" ]' in script, "the tag path is not guarded by the event")
    start = script.index('if [ "$EVENT" = "push" ]')
    tag_block = script[start:script.index("fi", start)]
    check("compiling every target" not in tag_block,
          "the tag path can still fall back to compiling")
    check(start < script.index("compiling every target"),
          "the tag decision must come before any fallback to compiling")
    check("compiling every target" in script, "the manual fallback disappeared")


def main():
    cases = [
        ("1 queued then success", case_queued_then_success),
        ("2 CI failure", case_ci_failure),
        ("3 CI cancelled", case_ci_cancelled),
        ("4 missing artifact", case_missing_artifact),
        ("5 expired artifact", case_expired_artifact),
        ("6 late CI run", case_late_run),
        ("7 no CI timeout", case_no_ci_timeout),
        ("8 stuck queue timeout", case_stuck_queue_timeout),
        ("9 tag step", case_tag_step),
        ("10 dev tag step", case_dev_tag_step),
        ("11 manual step", case_manual_step),
        ("12 workflow shape", case_workflow_shape),
    ]
    failures = 0
    for title, case in cases:
        box = Box()
        try:
            case(box)
            print(f"ok   {title}")
        except Failure as error:
            failures += 1
            print(f"FAIL {title}: {error}")
        finally:
            box.close()
    print("FAILED" if failures else "PASS")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
