#!/usr/bin/env python3
"""Synthetic tests for scripts/darwin-amd64-go.sh.

Covers the toolchain cache identity, the v0.1.5 entry migration, and the lock
protocol. Everything is faked: curl/tar/go/shasum/uname are PATH shims, so no
network is used and no toolchain is built. Each build runs in its own session
(start_new_session) and is cleaned up with killpg on that session only, never
with a global pattern kill.

Cases:
  1  a single build succeeds, releases the lock, and leaves nothing run-scoped
  2  barrier: a second build is refused while a live build holds the lock
  3  SIGKILL of the whole build session frees the lock, the lock file is reused
     and the next build runs
  4  N simultaneous builds never overlap
  5  the heartbeat drops the lock fd, the build keeps it
  6  barrier: a build parked between the cache check and the lock reuses the
     entry the other build just finished instead of rebuilding it
  7  a cache hit does no build, no curl and no bootstrap go
  8  editing the builder (lock, heartbeat, logging, comments) keeps the cache
  9  changing the patch data misses
 10  changing the Go version misses
 11  changing the host architecture misses
 12  changing the cache schema in the patch data misses
 13  a patch schema this script does not implement fails instead of rebuilding
 14  the released v0.1.5 entry is copied to the new identity without a build,
     and the old entry is left untouched
 15  a legacy entry with a wrong marker, unknown script hash, different patch,
     wrong ABI or unpatched source is never reused
 16  build-target.yml keeps the cache path, follows the patch data, splits
     restore and save, and runs this suite before the toolchain build
 17  a build that reports another version or ABI, or did not take the patch, is
     never published as a cache entry
 18  mutation: the previous mv-reclaim protocol loses mutual exclusion under a
     deterministic barrier and the harness must catch it
 19  mutation: a plain mkdir lock never recovers from a killed run
 20  mutation: without any lock the barrier scenario shows two builds at once
"""

import fcntl
import hashlib
import json
import os
import platform
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPT = os.path.join(REPO, "scripts", "darwin-amd64-go.sh")
PATCH = os.path.join(REPO, "scripts", "darwin-amd64-go.patch.json")
VERSION = "go1.25.0"
ARCH = platform.machine()
SCHEMA = json.load(open(PATCH, encoding="utf-8"))["schema"]
ABI = "amd64" if ARCH == "x86_64" else "arm64"

CURL = r"""#!/bin/bash
out=""
url=""
while (($#)); do
  case $1 in
    -o) out=$2; shift 2 ;;
    --*) shift ;;
    *) url=$1; shift ;;
  esac
done
echo "$url" >>"$MOCK_CURL_LOG"
case $url in
  *mode=json*)
    printf '[{"version":"%s","files":[{"kind":"source","sha256":"%s"}]}]' \
      "$MOCK_VERSION" "$MOCK_SOURCE_SHA" >"$out" ;;
  *) cat "$MOCK_SOURCE" >"$out" ;;
esac
"""

TAR = r"""#!/bin/bash
dir=""
while (($#)); do
  case $1 in
    -C) dir=$2; shift 2 ;;
    -*) shift ;;
    *) shift ;;
  esac
done
root=$dir/go
mkdir -p "$root/src/cmd/link/internal/ld" "$root/src/runtime" "$root/bin"
printf 'package ld\n\nfunc f() { ctxt.Tlsoffset = 0x30 }\n' >"$root/src/cmd/link/internal/ld/sym.go"
printf 'TEXT x(SB)\n\tMOVQ\tDX, 0x30(GS)\n' >"$root/src/runtime/sys_darwin_amd64.s"
cat >"$root/src/make.bash" <<'MAKE'
#!/bin/bash
echo "enter $$" >>"$MOCK_BUILD_LOG"
echo "build fd9=$([[ -e /dev/fd/9 ]] && echo yes || echo no)" >>"$MOCK_BUILD_LOG"
mkdir -p "$(dirname "$0")/../bin"
cat >"$(dirname "$0")/../bin/go" <<GO
#!/bin/bash
echo "go version ${MOCK_BUILD_VERSION:-$MOCK_VERSION} darwin/${MOCK_BUILD_ABI:-$MOCK_ABI}"
GO
chmod +x "$(dirname "$0")/../bin/go"
if [[ -n ${MOCK_BUILD_UNPATCH:-} ]]; then
  printf 'package ld\n\nfunc f() { ctxt.Tlsoffset = 0x30 }\n' >cmd/link/internal/ld/sym.go
fi
if [[ -n ${MOCK_BUILD_READY:-} ]]; then
  : >"$MOCK_BUILD_READY"
  for _ in $(seq 1 1200); do
    [[ -e ${MOCK_BUILD_RELEASE:-/nonexistent} ]] && break
    /bin/sleep 0.05
  done
fi
echo "leave $$" >>"$MOCK_BUILD_LOG"
MAKE
chmod +x "$root/src/make.bash"
"""

GO = r"""#!/bin/bash
echo "$*" >>"$MOCK_GO_LOG"
[[ $1 == env && $2 == GOROOT ]] && echo "$MOCK_GOROOT"
exit 0
"""

# Parks the rename of a lock directory until the test opens the gate. Only the
# reclaiming implementation renames a lock path, so this is inert for the current
# protocol.
MV = r"""#!/bin/bash
if [[ -n ${MOCK_MV_GATE:-} && $1 == "$DARWIN_AMD64_GO_LOCK_DIR"/*.lock ]]; then
  : >"$MOCK_MV_GATE/arrived"
  for _ in $(seq 1 1200); do
    [[ -e $MOCK_MV_GATE/go ]] && break
    /bin/sleep 0.05
  done
fi
exec /bin/mv "$@"
"""

SLEEP = r"""#!/bin/bash
echo "sleep ppid=$PPID fd9=$([[ -e /dev/fd/9 ]] && echo yes || echo no)" >>"$MOCK_SLEEP_LOG"
exec /bin/sleep "$@"
"""

# Parks the lock-file name computation, which happens between the cache check and
# the lock call, so a late build can be held exactly there.
SHASUM = r"""#!/bin/bash
if [[ $# -eq 2 && $1 == -a && $2 == 256 && -n ${MOCK_SHASUM_GATE:-} ]]; then
  : >"$MOCK_SHASUM_GATE/arrived"
  for _ in $(seq 1 1200); do
    [[ -e $MOCK_SHASUM_GATE/go ]] && break
    /bin/sleep 0.05
  done
fi
exec /usr/bin/shasum "$@"
"""

UNAME = r"""#!/bin/bash
if [[ $1 == -m && -n ${MOCK_UNAME_M:-} ]]; then
  echo "$MOCK_UNAME_M"
  exit 0
fi
exec /usr/bin/uname "$@"
"""

# The reclaim protocol this change replaced, kept verbatim as a mutant.
OLD_LOCK = r"""lock_dir="$lock_root/darwin-amd64-go-$(printf '%s' "$cache_marker:$cache_root" | shasum -a 256 | awk '{print $1}' | cut -c1-16).lock"
lock_grace=${DARWIN_AMD64_GO_LOCK_GRACE:-60}
process_started() { ps -o lstart= -p "$1" 2>/dev/null | awk '{$1 = $1; print}'; }
claim_lock() { echo $$ >"$lock_dir/owner"; process_started $$ >"$lock_dir/started" 2>/dev/null || true; }
owner_alive() {
  local recorded current
  kill -0 "$1" 2>/dev/null || return 1
  recorded=$(cat "$lock_dir/started" 2>/dev/null) || recorded=""
  [[ -n $recorded ]] || return 0
  current=$(process_started "$1")
  [[ -z $current || $current == "$recorded" ]]
}
reclaim_lock() {
  local stale="$lock_dir.stale.$$"
  mv "$lock_dir" "$stale" 2>/dev/null || return 1
  rm -rf "$stale"
  mkdir "$lock_dir" 2>/dev/null || return 1
  claim_lock
}
take_lock() {
  local owner mtime now age
  if mkdir "$lock_dir" 2>/dev/null; then
    claim_lock
    return 0
  fi
  owner=$(cat "$lock_dir/owner" 2>/dev/null) || owner=""
  mtime=$(stat -f %m "$lock_dir" 2>/dev/null) || mtime=0
  now=$(date +%s)
  age=$(( now - mtime ))
  if [[ -z $owner ]]; then
    if (( age < lock_grace )); then
      return 1
    fi
  elif owner_alive "$owner"; then
    return 1
  fi
  echo "darwin-amd64-go: reclaiming stale toolchain lock $lock_dir (age ${age}s, owner ${owner:-unknown})" >&2
  reclaim_lock
}
release_lock() {
  [[ $(cat "$lock_dir/owner" 2>/dev/null) == $$ ]] || return 0
  rm -rf "$lock_dir"
}
take_lock || fail "another build owns this toolchain cache entry ($lock_dir)"
"""

# The lock this change replaced in the first place: no recovery at all.
MKDIR_LOCK = r"""lock_dir="$cache_entry.lock"
mkdir "$lock_dir" 2>/dev/null || fail "another build owns this toolchain cache entry ($lock_dir)"
"""


class Failure(Exception):
    pass


def check(condition, message):
    if not condition:
        raise Failure(message)


def wait_for(predicate, message, timeout=30.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.02)
    raise Failure("timed out waiting for " + message)


def sha256_file(path):
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def legacy_pair(script):
    match = re.search(r'"([0-9a-f]{64}):([0-9a-f]{64})"', open(script, encoding="utf-8").read())
    check(match is not None, "the script has no legacy identity pair")
    return match.group(1), match.group(2)


def snapshot(root):
    state = {}
    for base, _dirs, files in os.walk(root):
        for name in files:
            path = os.path.join(base, name)
            stat = os.stat(path)
            state[os.path.relpath(path, root)] = (stat.st_size, stat.st_mtime_ns, stat.st_mode)
    return state


class Sandbox:
    def __init__(self, arch=None, abi=None):
        self.host_arch = arch or ARCH
        self.host_abi = abi or ABI
        self.root = os.path.realpath(tempfile.mkdtemp(prefix="go-toolchain-test."))
        self.bin = os.path.join(self.root, "bin")
        self.locks = os.path.join(self.root, "locks")
        self.cache = os.path.join(self.root, "cache")
        self.build_log = os.path.join(self.root, "build.log")
        self.curl_log = os.path.join(self.root, "curl.log")
        self.go_log = os.path.join(self.root, "go.log")
        self.sleep_log = os.path.join(self.root, "sleep.log")
        self.source = os.path.join(self.root, "fake-src.tar.gz")
        self.goroot = os.path.join(self.root, "goroot")
        for directory in (self.bin, self.locks, self.cache, self.goroot):
            os.makedirs(directory, exist_ok=True)
        with open(self.source, "wb") as handle:
            handle.write(b"not a real tarball\n")
        self.source_sha = sha256_file(self.source)
        mocks = (("curl", CURL), ("tar", TAR), ("go", GO), ("mv", MV), ("sleep", SLEEP),
                 ("shasum", SHASUM), ("uname", UNAME))
        for name, body in mocks:
            path = os.path.join(self.bin, name)
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(body)
            os.chmod(path, 0o755)
        for path in (self.build_log, self.curl_log, self.go_log, self.sleep_log):
            open(path, "w").close()
        self.procs = []

    def close(self):
        for proc in self.procs:
            # start_new_session makes the child a session and group leader, so its
            # pid is the pgid of exactly the processes this test started.
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except (ProcessLookupError, PermissionError):
                pass
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                pass
        shutil.rmtree(self.root, ignore_errors=True)

    def env(self, extra=None):
        env = {
            "PATH": self.bin + ":/usr/bin:/bin:/usr/sbin:/sbin",
            "DARWIN_AMD64_GO_LOCK_DIR": self.locks,
            "MOCK_VERSION": VERSION,
            "MOCK_UNAME_M": self.host_arch,
            "MOCK_ABI": self.host_abi,
            "MOCK_SOURCE": self.source,
            "MOCK_SOURCE_SHA": self.source_sha,
            "MOCK_GOROOT": self.goroot,
            "MOCK_BUILD_LOG": self.build_log,
            "MOCK_CURL_LOG": self.curl_log,
            "MOCK_GO_LOG": self.go_log,
            "MOCK_SLEEP_LOG": self.sleep_log,
        }
        env.update(extra or {})
        return env

    def start(self, script, cache, extra=None, version=VERSION):
        proc = subprocess.Popen(
            ["bash", script, cache, version],
            env=self.env(extra),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
        )
        self.procs.append(proc)
        return proc

    def run(self, script, cache, extra=None, timeout=120, version=VERSION):
        proc = self.start(script, cache, extra, version)
        out, err = proc.communicate(timeout=timeout)
        return proc.returncode, out, err

    def read_log(self, path):
        with open(path, encoding="utf-8") as handle:
            return [line.strip() for line in handle if line.strip()]

    def events(self):
        return [line.split()[0] for line in self.read_log(self.build_log)
                if line.startswith(("enter", "leave"))]

    def curl_calls(self):
        return self.read_log(self.curl_log)

    def go_calls(self):
        return self.read_log(self.go_log)

    def fd9_notes(self):
        return [line for line in self.read_log(self.build_log) if line.startswith("build fd9=")]

    def heartbeat_notes(self):
        return [line for line in self.read_log(self.sleep_log) if "fd9=" in line]

    def reset_logs(self):
        for path in (self.build_log, self.curl_log, self.go_log, self.sleep_log):
            open(path, "w").close()

    def patch_sha(self):
        return sha256_file(PATCH)

    def entry_name(self, patch_hash=None, version=VERSION, arch=None, schema=SCHEMA):
        return f"{version}-{arch or self.host_arch}-s{schema}-{patch_hash or self.patch_sha()}"

    def marker(self, version=VERSION, arch=None, schema=SCHEMA):
        return f"{version}:{arch or self.host_arch}:s{schema}:{self.patch_sha()}"

    def lock_path(self, cache, marker=None):
        digest = hashlib.sha256(f"{marker or self.marker()}:{cache}".encode()).hexdigest()[:16]
        return os.path.join(self.locks, f"darwin-amd64-go-{digest}.lock")

    def copy_script(self, name, script_transform=None, patch_transform=None):
        directory = os.path.join(self.root, name, "scripts")
        os.makedirs(directory, exist_ok=True)
        text = open(SCRIPT, encoding="utf-8").read()
        if script_transform:
            text = script_transform(text)
        script = os.path.join(directory, "darwin-amd64-go.sh")
        with open(script, "w", encoding="utf-8") as handle:
            handle.write(text)
        target = os.path.join(directory, "darwin-amd64-go.patch.json")
        if patch_transform is None:
            shutil.copy(PATCH, target)
        else:
            with open(target, "w", encoding="utf-8") as handle:
                json.dump(patch_transform(json.load(open(PATCH, encoding="utf-8"))), handle, indent=2)
        return script


def serialized(events):
    for index, event in enumerate(events):
        if event != ("enter" if index % 2 == 0 else "leave"):
            return False
    return True


def mutant(script, root, name, replacement):
    """Copies the script with its lock section swapped for an older protocol."""
    text = open(script, encoding="utf-8").read()
    start = text.index('lock_file="$lock_root/')
    end = text.index("# Another build can have finished this entry")
    directory = os.path.join(root, name, "scripts")
    os.makedirs(directory, exist_ok=True)
    path = os.path.join(directory, "darwin-amd64-go.sh")
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text[:start] + replacement + text[end:])
    shutil.copy(PATCH, directory)
    return path


def case_single_build(box):
    cache = os.path.join(box.cache, "single")
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"single build failed: {err}")
    goroot = out.strip()
    check(goroot.endswith("/go"), f"unexpected output {goroot!r}")
    check(os.access(os.path.join(goroot, "bin", "go"), os.X_OK), "no go binary")
    entry = os.path.dirname(goroot)
    check(os.path.basename(entry) == box.entry_name(), f"unexpected cache entry {entry!r}")
    marker = open(os.path.join(entry, "complete"), encoding="utf-8").read().strip()
    check(marker == box.marker(), f"bad marker {marker!r}")
    lock = box.lock_path(cache)
    check(os.path.isfile(lock), "lock is not a plain file")
    check(not lock_held(lock), "lock still held after the build")
    leftovers = [n for n in os.listdir(cache) if n.startswith((".build.", ".migrate.")) or n.endswith(".lock")]
    check(not leftovers, f"run-scoped leftovers in the cache dir: {leftovers}")
    check(serialized(box.events()), f"builds overlapped: {box.events()}")


def lock_held(path):
    fd = os.open(path, os.O_RDWR | os.O_CREAT, 0o644)
    try:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            return True
        fcntl.flock(fd, fcntl.LOCK_UN)
        return False
    finally:
        os.close(fd)


def barrier_scenario(box, script, cache, tag):
    """Starts one build, waits until it is inside make.bash, then starts another."""
    ready = os.path.join(box.root, tag + ".ready")
    release = os.path.join(box.root, tag + ".release")
    holder = box.start(script, cache, {"MOCK_BUILD_READY": ready, "MOCK_BUILD_RELEASE": release})
    wait_for(lambda: os.path.exists(ready), "the first build to start")
    second = box.start(script, cache)
    _out, err = second.communicate(timeout=120)
    events = box.events()
    open(release, "w").close()
    holder.communicate(timeout=60)
    return second.returncode, err, events


def case_live_owner_barrier(box):
    box.reset_logs()
    cache = os.path.join(box.cache, "live")
    rc, err, events = barrier_scenario(box, SCRIPT, cache, "live")
    check(rc != 0, "a second build was allowed to run next to a live one")
    check("another build owns this toolchain cache entry" in err, f"unexpected refusal: {err}")
    check(events.count("enter") == 1, f"the refused build started building: {events}")
    check(serialized(events), f"builds overlapped: {events}")


def case_sigkill_recovery(box):
    box.reset_logs()
    cache = os.path.join(box.cache, "killed")
    lock = box.lock_path(cache)
    ready = os.path.join(box.root, "killed.ready")
    victim = box.start(SCRIPT, cache, {"MOCK_BUILD_READY": ready})
    wait_for(lambda: os.path.exists(ready), "the build to start")
    check(lock_held(lock), "the build does not hold the lock")
    os.killpg(victim.pid, signal.SIGKILL)
    victim.wait(timeout=10)
    wait_for(lambda: not lock_held(lock), "the kernel to drop the killed build's lock", timeout=5)
    check(os.path.isfile(lock), "the lock file should stay behind and be reusable")
    inode = os.stat(lock).st_ino
    staging = [n for n in os.listdir(cache) if n.startswith(".build.")]
    check(staging, "expected the killed run to leave a staging directory")

    rc, out, err = box.run(SCRIPT, cache, {"DARWIN_AMD64_GO_STAGING_MAX_AGE": "0"})
    check(rc == 0, f"recovery build failed: {err}")
    check(out.strip().endswith("/go"), f"unexpected output {out!r}")
    check(not [n for n in os.listdir(cache) if n.startswith(".build.")],
          "the orphaned staging directory was not pruned under the lock")
    check(os.stat(lock).st_ino == inode, "the lock file was replaced instead of reused")


def case_simultaneous(box):
    box.reset_logs()
    cache = os.path.join(box.cache, "race")
    procs = [box.start(SCRIPT, cache) for _ in range(6)]
    results = [proc.communicate(timeout=120) for proc in procs]
    check(serialized(box.events()), f"builds overlapped: {box.events()}")
    check(any(proc.returncode == 0 for proc in procs), "no build succeeded")
    for proc, (_out, err) in zip(procs, results):
        if proc.returncode != 0:
            check("another build owns this toolchain cache entry" in err,
                  f"unexpected failure: {err}")


def case_heartbeat_fd(box):
    box.reset_logs()
    cache = os.path.join(box.cache, "heartbeat")
    ready = os.path.join(box.root, "heartbeat.ready")
    release = os.path.join(box.root, "heartbeat.release")
    holder = box.start(SCRIPT, cache, {
        "MOCK_BUILD_READY": ready,
        "MOCK_BUILD_RELEASE": release,
        "DARWIN_AMD64_GO_STAGING_HEARTBEAT": "1",
    })
    wait_for(lambda: os.path.exists(ready), "the build to start")
    wait_for(lambda: any("fd9=" in n for n in box.heartbeat_notes()), "a heartbeat tick", timeout=15)
    open(release, "w").close()
    _out, err = holder.communicate(timeout=60)
    check(holder.returncode == 0, f"build failed: {err}")
    heartbeats = box.heartbeat_notes()
    check(heartbeats, "the heartbeat never ran")
    for note in heartbeats:
        check("fd9=no" in note, f"the heartbeat inherited the lock fd: {note}")
    builds = box.fd9_notes()
    check(builds and all("fd9=yes" in note for note in builds),
          f"the build should keep the lock fd until it finishes: {builds}")


def case_late_arrival(box):
    """A build held between the cache check and the lock must reuse the entry."""
    box.reset_logs()
    cache = os.path.join(box.cache, "late")
    gate = os.path.join(box.root, "shasum-gate")
    os.makedirs(gate, exist_ok=True)
    late = box.start(SCRIPT, cache, {"MOCK_SHASUM_GATE": gate})
    wait_for(lambda: os.path.exists(os.path.join(gate, "arrived")),
             "the late build to park before taking the lock")
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"the first build failed: {err}")
    check(box.events().count("enter") == 1, f"expected one build, saw {box.events()}")

    open(os.path.join(gate, "go"), "w").close()
    late_out, late_err = late.communicate(timeout=120)
    check(late.returncode == 0, f"the late build failed: {late_err}")
    check(late_out.strip() == out.strip(), f"late build reported {late_out.strip()!r}")
    check(box.events().count("enter") == 1,
          f"the late build rebuilt a finished entry: {box.events()}")


def case_cache_hit(box):
    cache = os.path.join(box.cache, "hit")
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    check("cache hit" not in err, f"the first run claimed a hit: {err}")
    check(f"cache miss {box.entry_name()}" in err, f"no miss log: {err}")
    check(f"building go{VERSION} for {box.host_arch} with patch {box.patch_sha()[:12]}" in err,
          f"the miss log does not name the identity: {err}")
    check(out.strip() == os.path.join(cache, box.entry_name(), "go"),
          f"stdout is not just the GOROOT: {out!r}")
    box.reset_logs()
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"second run failed: {err}")
    check(out.strip().endswith("/go"), f"stdout is not just the GOROOT: {out!r}")
    check("cache hit" in err, f"no hit log: {err}")
    check(box.events() == [], f"a hit rebuilt: {box.events()}")
    check(box.curl_calls() == [], f"a hit used the network: {box.curl_calls()}")
    check(box.go_calls() == [], f"a hit invoked the bootstrap go: {box.go_calls()}")


def case_builder_edits_keep_cache(box):
    cache = os.path.join(box.cache, "edits")
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    edited = box.copy_script(
        "edited",
        lambda text: text.replace('log "cache hit $cache_id"', 'log "cache hit $cache_id (edited)"')
        + "\n# a comment that must not invalidate a toolchain\n")
    box.reset_logs()
    rc, out2, err = box.run(edited, cache)
    check(rc == 0, f"edited builder failed: {err}")
    check(out2.strip() == out.strip(), f"different GOROOT {out2.strip()!r}")
    check("cache hit" in err, f"editing the builder rebuilt the toolchain: {err}")
    check(box.events() == [], f"editing the builder rebuilt: {box.events()}")


def case_patch_change_misses(box):
    cache = os.path.join(box.cache, "patch")
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    other = box.copy_script(
        "otherpatch",
        patch_transform=lambda patch: _replaced(patch, "0x58", "0x60"))
    box.reset_logs()
    rc, out2, err = box.run(other, cache)
    check(rc == 0, f"patched build failed: {err}")
    check(box.events().count("enter") == 1, "a different patch reused the toolchain")
    check("cache hit" not in err, f"a different patch was a hit: {err}")
    check("cache miss" in err, f"a different patch did not log a miss: {err}")
    check(out2.strip() != out.strip(), "both patches mapped to one entry")


def _replaced(patch, before, after):
    for replacement in patch["replacements"]:
        replacement["to"] = replacement["to"].replace(before, after)
    return patch


def case_version_change_misses(box):
    cache = os.path.join(box.cache, "version")
    rc, _out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    other = "go1.26.0"
    box.reset_logs()
    rc, out2, err = box.run(SCRIPT, cache, {"MOCK_VERSION": other}, version=other)
    check(rc == 0, f"second build failed: {err}")
    check(box.events().count("enter") == 1, "a different Go version reused the toolchain")
    check(os.path.basename(os.path.dirname(out2.strip())) == box.entry_name(version=other),
          f"unexpected entry {out2.strip()!r}")


def case_arch_change_misses(box):
    cache = os.path.join(box.cache, "arch")
    rc, _out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    other_arch = "arm64" if box.host_arch == "x86_64" else "x86_64"
    other_abi = "amd64" if other_arch == "x86_64" else "arm64"
    box.reset_logs()
    rc, out2, err = box.run(SCRIPT, cache, {"MOCK_UNAME_M": other_arch, "MOCK_ABI": other_abi})
    check(rc == 0, f"second build failed: {err}")
    check(box.events().count("enter") == 1, "a different host architecture reused the toolchain")
    check(os.path.basename(os.path.dirname(out2.strip())) == box.entry_name(arch=other_arch),
          f"unexpected entry {out2.strip()!r}")


def case_schema_change_misses(box):
    cache = os.path.join(box.cache, "schema")
    rc, _out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    # A builder that implements the next schema, with patch data that says so.
    script = box.copy_script(
        "schema2",
        lambda text: text.replace("[[ $patch_schema == 1 ]]", "[[ $patch_schema == 2 ]]")
        .replace('if patch.get("schema") != 1:', 'if patch.get("schema") != 2:'),
        lambda patch: dict(patch, schema=2))
    box.reset_logs()
    rc, out, err = box.run(script, cache)
    check(rc == 0, f"schema 2 build failed: {err}")
    check(box.events().count("enter") == 1, "a schema bump reused the old entry")
    check("cache hit" not in err, f"a schema bump was a hit: {err}")
    check("-s2-" in os.path.basename(os.path.dirname(out.strip())),
          f"unexpected entry {out.strip()!r}")


def case_schema_unsupported(box):
    cache = os.path.join(box.cache, "schema-unsupported")
    script = box.copy_script("schema9", patch_transform=lambda patch: dict(patch, schema=9))
    box.reset_logs()
    rc, _out, err = box.run(script, cache)
    check(rc != 0, "an unsupported schema was accepted")
    check("is not supported by this script" in err, f"unexpected failure: {err}")
    check(box.events() == [], f"an unsupported schema started a build: {box.events()}")


def case_workflow_cache(box):
    text = open(os.path.join(REPO, ".github/workflows/build-target.yml"), encoding="utf-8").read()
    keys = [line for line in text.splitlines() if line.strip().startswith("key: tlsgo-")]
    check(keys, "no tlsgo cache key")
    for line in keys:
        check("scripts/darwin-amd64-go.patch.json" in line, f"the key does not follow the patch data: {line}")
        check("darwin-amd64-go.sh" not in line, f"the key still follows the build script: {line}")
    check("path: .cache/tlsgo" in text, "the cache path changed; the v0.1.5 cache version would stop matching")
    check("actions/cache/restore@v5" in text and "actions/cache/save@v5" in text,
          "restore and save are not separate steps")
    lines = text.splitlines()
    at = next(i for i, line in enumerate(lines) if line.strip().startswith("restore-keys:"))
    check("go-version" in lines[at + 1], "the restore key is not scoped to the Go version")
    check(text.index("python3 scripts/test-darwin-amd64-go-lock.py")
          < text.index("Prepare patched Go toolchain"),
          "the cache protocol test does not run before the toolchain build")


def drop_entries(cache):
    for name in os.listdir(cache):
        if re.fullmatch(r".*-s[0-9]+-[0-9a-f]{64}", name):
            shutil.rmtree(os.path.join(cache, name), ignore_errors=True)


def _legacy_entry(box, cache, source, script_sha, marker=None, abi=None):
    target = os.path.join(cache, f"{VERSION}-{box.host_arch}-{script_sha}")
    shutil.copytree(source, target)
    with open(os.path.join(target, "complete"), "w", encoding="utf-8") as handle:
        handle.write((marker or f"{VERSION}:{box.host_arch}:{script_sha}") + "\n")
    if abi:
        go = os.path.join(target, "go", "bin", "go")
        with open(go, "w", encoding="utf-8") as handle:
            handle.write(f'#!/bin/bash\necho "go version {VERSION} darwin/{abi}"\n')
        os.chmod(go, 0o755)
    return target


def case_legacy_migration(box):
    cache = os.path.join(box.cache, "legacy")
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    script_sha, patch_sha = legacy_pair(SCRIPT)
    check(patch_sha == box.patch_sha(), "the legacy pair does not name the current patch")
    built = os.path.dirname(out.strip())
    legacy = _legacy_entry(box, cache, built, script_sha)
    shutil.rmtree(built)
    before = snapshot(legacy)
    box.reset_logs()

    rc, out2, err = box.run(SCRIPT, cache)
    check(rc == 0, f"migration run failed: {err}")
    check("migrating" in err, f"no migration log: {err}")
    check(box.events() == [], f"the migration compiled: {box.events()}")
    check(out2.strip() == os.path.join(cache, box.entry_name(), "go"), f"unexpected GOROOT {out2.strip()!r}")
    marker = open(os.path.join(cache, box.entry_name(), "complete"), encoding="utf-8").read().strip()
    check(marker == box.marker(), f"bad migrated marker {marker!r}")
    check(snapshot(legacy) == before, "the released entry was modified")
    check(not [n for n in os.listdir(cache) if n.startswith(".migrate.")], "migration staging left behind")


def case_legacy_refusals(box):
    cache = os.path.join(box.cache, "refuse")
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"first build failed: {err}")
    script_sha, _patch_sha = legacy_pair(SCRIPT)
    built = os.path.dirname(out.strip())

    def attempt(label, expected_log, script=SCRIPT, extra=None):
        drop_entries(cache)
        box.reset_logs()
        rc, _out, err = box.run(script, cache, extra)
        check(rc == 0, f"{label}: run failed: {err}")
        check(expected_log in err, f"{label}: unexpected log: {err}")
        check(box.events().count("enter") == 1, f"{label}: the legacy entry was reused")

    # A marker that does not belong to the legacy identity.
    target = _legacy_entry(box, cache, built, script_sha, marker=f"{VERSION}:{ARCH}:{'0' * 64}")
    attempt("wrong marker", "not a complete toolchain")
    shutil.rmtree(target)

    # A script hash the mapping does not know.
    target = _legacy_entry(box, cache, built, "deadbeef" * 8)
    drop_entries(cache)
    box.reset_logs()
    rc, _out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"unknown script: run failed: {err}")
    check(box.events().count("enter") == 1, "unknown script: the legacy entry was reused")
    shutil.rmtree(target)

    # A toolchain that reports another ABI.
    target = _legacy_entry(box, cache, built, script_sha, abi="amd64" if ABI == "arm64" else "arm64")
    attempt("wrong ABI", "not a complete toolchain")
    shutil.rmtree(target)

    # A source tree that still carries slot 6.
    target = _legacy_entry(box, cache, built, script_sha)
    for relative, old, new in (
        ("src/cmd/link/internal/ld/sym.go", "ctxt.Tlsoffset = 0x58", "ctxt.Tlsoffset = 0x30"),
        ("src/runtime/sys_darwin_amd64.s", "MOVQ\tDX, 0x58(GS)", "MOVQ\tDX, 0x30(GS)"),
    ):
        path = os.path.join(target, "go", relative)
        content = open(path, encoding="utf-8").read()
        check(old in content, f"fixture source lacks {old!r}")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(content.replace(old, new))
    attempt("unpatched source", "the TLS patch is not applied")
    shutil.rmtree(target)

    # The mapping exists, but the current patch is different.
    target = _legacy_entry(box, cache, built, script_sha)
    other = box.copy_script("otherpatch", patch_transform=lambda patch: _replaced(patch, "0x58", "0x60"))
    attempt("different patch", "different patch", script=other)
    shutil.rmtree(target)


def case_build_identity_rejected(box):
    cache = os.path.join(box.cache, "reject")
    entry = os.path.join(cache, box.entry_name())

    def attempt(label, extra, expected):
        box.reset_logs()
        rc, out, err = box.run(SCRIPT, cache, extra)
        check(rc != 0, f"{label}: a mismatched toolchain was published")
        check(expected in err, f"{label}: unexpected failure: {err}")
        check(not os.path.isdir(entry), f"{label}: the entry was published")
        check(not [n for n in os.listdir(cache) if n.startswith(".build.")],
              f"{label}: the staging directory was left behind")
        check(out.strip() == "", f"{label}: stdout is not empty: {out!r}")

    attempt("wrong version", {"MOCK_BUILD_VERSION": "go1.24.0"}, "does not report go")
    attempt("wrong ABI",
            {"MOCK_BUILD_ABI": "amd64" if box.host_abi == "arm64" else "arm64"},
            "does not report go")
    attempt("unpatched source", {"MOCK_BUILD_UNPATCH": "1"}, "did not take the TLS patch")


def case_mutation_reclaim_race(box):
    """The old reclaim protocol loses exclusion under a forced interleaving."""
    box.reset_logs()
    script = mutant(SCRIPT, box.root, "reclaim", OLD_LOCK)
    cache = os.path.join(box.cache, "mutant-reclaim")
    lock_dir = box.lock_path(cache)
    dead = subprocess.run(["bash", "-c", "echo $$"], capture_output=True, text=True).stdout.strip()
    os.makedirs(lock_dir, exist_ok=True)
    with open(os.path.join(lock_dir, "owner"), "w", encoding="utf-8") as handle:
        handle.write(dead + "\n")
    os.utime(lock_dir, (time.time() - 7200, time.time() - 7200))

    gate = os.path.join(box.root, "gate")
    os.makedirs(gate, exist_ok=True)
    ready_a = os.path.join(box.root, "a.ready")
    ready_b = os.path.join(box.root, "b.ready")
    release = os.path.join(box.root, "mutant.release")
    common = {"MOCK_BUILD_RELEASE": release}

    second = box.start(script, cache, dict(common, MOCK_MV_GATE=gate, MOCK_BUILD_READY=ready_b))
    wait_for(lambda: os.path.exists(os.path.join(gate, "arrived")),
             "the second build to park before its rename")
    first = box.start(script, cache, dict(common, MOCK_BUILD_READY=ready_a))
    wait_for(lambda: os.path.exists(ready_a), "the first build to take the reclaimed lock")
    open(os.path.join(gate, "go"), "w").close()
    wait_for(lambda: os.path.exists(ready_b), "the second build to claim a lock", timeout=15)

    events = box.events()
    check(not serialized(events), f"mutant not caught, builds were serialised: {events}")
    check(events.count("enter") == 2, f"expected two concurrent builds, saw {events}")

    open(release, "w").close()
    for proc in (first, second):
        try:
            proc.communicate(timeout=60)
        except subprocess.TimeoutExpired:
            pass
    return "two builds ran at once"


def case_mutation_no_recovery(box):
    """A plain mkdir lock never recovers from a killed run."""
    box.reset_logs()
    script = mutant(SCRIPT, box.root, "mkdironly", MKDIR_LOCK)
    cache = os.path.join(box.cache, "mutant-mkdir")
    entry = os.path.join(cache, box.entry_name())
    os.makedirs(entry + ".lock", exist_ok=True)
    rc, _out, err = box.run(script, cache)
    check(rc != 0, "mutant not caught: a killed run's lock was ignored")
    check("another build owns" in err, f"unexpected failure: {err}")
    return "a stale lock blocks every later run"


def case_mutation_no_lock(box):
    """Without the lock the barrier scenario must show two builds at once."""
    box.reset_logs()
    script = mutant(SCRIPT, box.root, "nolock", ":\n")
    cache = os.path.join(box.cache, "mutant-nolock")
    _rc, _err, events = barrier_scenario(box, script, cache, "nolock")
    check(not serialized(events), f"mutant not caught, builds were serialised: {events}")
    check(events.count("enter") == 2, f"expected two concurrent builds, saw {events}")
    return "two builds ran at once"


def main():
    cases = [
        ("1 single build", case_single_build),
        ("2 live owner barrier", case_live_owner_barrier),
        ("3 SIGKILL recovery", case_sigkill_recovery),
        ("4 simultaneous builds", case_simultaneous),
        ("5 heartbeat and build fds", case_heartbeat_fd),
        ("6 late arrival after the lock", case_late_arrival),
        ("8 builder edits keep the cache", case_builder_edits_keep_cache),
        ("9 patch change misses", case_patch_change_misses),
        ("10 Go version change misses", case_version_change_misses),
        ("12 schema change misses", case_schema_change_misses),
        ("13 unsupported schema fails", case_schema_unsupported),
        ("15 legacy refusals", case_legacy_refusals),
        ("16 workflow cache wiring", case_workflow_cache),
        ("17 built identity is verified before publishing", case_build_identity_rejected),
    ]
    # The arch-sensitive cases run on the native host and on the other host
    # architecture, so both ABI checks are exercised without compiling anything.
    native = (ARCH, ABI)
    other = ("arm64", "arm64") if ARCH == "x86_64" else ("x86_64", "amd64")
    for arch, abi in (native, other):
        cases += [
            (f"7 cache hit on {arch}", case_cache_hit, {"arch": arch, "abi": abi}),
            (f"11 host architecture change on {arch}", case_arch_change_misses, {"arch": arch, "abi": abi}),
            (f"14 legacy migration on {arch}", case_legacy_migration, {"arch": arch, "abi": abi}),
        ]
    mutations = [
        ("18 mutant: reclaim race", case_mutation_reclaim_race),
        ("19 mutant: no recovery", case_mutation_no_recovery),
        ("20 mutant: no lock", case_mutation_no_lock),
    ]
    failures = 0
    for entry in cases:
        title, case = entry[0], entry[1]
        box = Sandbox(**entry[2]) if len(entry) > 2 else Sandbox()
        try:
            case(box)
            print(f"ok   {title}")
        except Failure as error:
            failures += 1
            print(f"FAIL {title}: {error}")
        finally:
            box.close()
    for title, case in mutations:
        box = Sandbox()
        try:
            effect = case(box)
            print(f"ok   {title}: caught ({effect})")
        except Failure as error:
            failures += 1
            print(f"FAIL {title}: {error}")
        finally:
            box.close()
    print("FAILED" if failures else "PASS")
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())
