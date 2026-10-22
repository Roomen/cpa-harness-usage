#!/usr/bin/env python3
"""Synthetic tests for the toolchain cache lock in scripts/darwin-amd64-go.sh.

Everything is faked: curl/tar/go are PATH shims, so no network is used and no
toolchain is built. Each build runs in its own session (start_new_session) and is
cleaned up with killpg on that session only, never with a global pattern kill.

The lock oracle is fcntl.flock(LOCK_EX | LOCK_NB) on the lock file, which is the
same BSD lock lockf(1) takes, so it answers "is a build holding the lock right
now" without inspecting process tables.

Cases:
  1  a single build succeeds, releases the lock, and leaves nothing run-scoped
  2  barrier: a second build is refused while a live build holds the lock
  3  SIGKILL of the whole build session frees the lock, the lock file is reused
     and the next build runs
  4  N simultaneous builds never overlap
  5  the heartbeat drops the lock fd, the build keeps it
  6  barrier: a build parked between the cache check and the lock reuses the
     entry the other build just finished instead of rebuilding it
  7  mutation: the previous mv-reclaim protocol loses mutual exclusion under a
     deterministic barrier and the harness must catch it
  8  mutation: a plain mkdir lock never recovers from a killed run
  9  mutation: without any lock the barrier scenario shows two builds at once
"""

import fcntl
import hashlib
import os
import platform
import shutil
import signal
import subprocess
import sys
import tempfile
import time

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SCRIPT = os.path.join(REPO, "scripts", "darwin-amd64-go.sh")
VERSION = "go1.25.0"
ARCH = platform.machine()

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
printf '#!/bin/bash\nexit 0\n' >"$(dirname "$0")/../bin/go"
chmod +x "$(dirname "$0")/../bin/go"
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

SLEEP = r"""#!/bin/bash
echo "sleep ppid=$PPID fd9=$([[ -e /dev/fd/9 ]] && echo yes || echo no)" >>"$MOCK_SLEEP_LOG"
exec /bin/sleep "$@"
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


def lock_path(script, cache, lock_root):
    # The script canonicalises cache_root with cd+pwd before hashing it.
    digest = hashlib.sha256(open(script, "rb").read()).hexdigest()
    marker = f"{VERSION}:{ARCH}:{digest}"
    key = hashlib.sha256(f"{marker}:{os.path.realpath(cache)}".encode()).hexdigest()[:16]
    return os.path.join(lock_root, f"darwin-amd64-go-{key}.lock")


def lock_held(path):
    """True when some process holds the flock on path."""
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


class Sandbox:
    def __init__(self):
        self.root = os.path.realpath(tempfile.mkdtemp(prefix="go-lock-test."))
        self.bin = os.path.join(self.root, "bin")
        self.locks = os.path.join(self.root, "locks")
        self.cache = os.path.join(self.root, "cache")
        self.build_log = os.path.join(self.root, "build.log")
        self.sleep_log = os.path.join(self.root, "sleep.log")
        self.source = os.path.join(self.root, "fake-src.tar.gz")
        self.goroot = os.path.join(self.root, "goroot")
        for directory in (self.bin, self.locks, self.cache, self.goroot):
            os.makedirs(directory, exist_ok=True)
        with open(self.source, "wb") as handle:
            handle.write(b"not a real tarball\n")
        self.source_sha = hashlib.sha256(open(self.source, "rb").read()).hexdigest()
        mocks = (("curl", CURL), ("tar", TAR), ("go", GO), ("mv", MV), ("sleep", SLEEP),
                 ("shasum", SHASUM))
        for name, body in mocks:
            path = os.path.join(self.bin, name)
            with open(path, "w", encoding="utf-8") as handle:
                handle.write(body)
            os.chmod(path, 0o755)
        open(self.build_log, "w").close()
        open(self.sleep_log, "w").close()
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
            "MOCK_SOURCE": self.source,
            "MOCK_SOURCE_SHA": self.source_sha,
            "MOCK_GOROOT": self.goroot,
            "MOCK_BUILD_LOG": self.build_log,
            "MOCK_SLEEP_LOG": self.sleep_log,
        }
        env.update(extra or {})
        return env

    def start(self, script, cache, extra=None):
        proc = subprocess.Popen(
            ["bash", script, cache, VERSION],
            env=self.env(extra),
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            start_new_session=True,
        )
        self.procs.append(proc)
        return proc

    def run(self, script, cache, extra=None, timeout=120):
        proc = self.start(script, cache, extra)
        out, err = proc.communicate(timeout=timeout)
        return proc.returncode, out, err

    def events(self):
        with open(self.build_log, encoding="utf-8") as handle:
            return [line.split()[0] for line in handle if line.startswith(("enter", "leave"))]

    def fd9_notes(self):
        with open(self.build_log, encoding="utf-8") as handle:
            return [line.strip() for line in handle if line.startswith("build fd9=")]

    def heartbeat_notes(self):
        with open(self.sleep_log, encoding="utf-8") as handle:
            return [line.strip() for line in handle if "fd9=" in line]

    def reset_logs(self):
        open(self.build_log, "w").close()
        open(self.sleep_log, "w").close()


def serialized(events):
    for index, event in enumerate(events):
        if event != ("enter" if index % 2 == 0 else "leave"):
            return False
    return True


def mutant(script, root, name, replacement):
    """Copies the script with its lock section swapped for an older protocol."""
    text = open(script, encoding="utf-8").read()
    start = text.index('lock_file="$lock_root/')
    end = text.index("# Prune only while holding the lock:")
    directory = os.path.join(root, name, "scripts")
    os.makedirs(directory, exist_ok=True)
    path = os.path.join(directory, "darwin-amd64-go.sh")
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text[:start] + replacement + text[end:])
    return path


def case_single_build(box):
    cache = os.path.join(box.cache, "single")
    lock = lock_path(SCRIPT, cache, box.locks)
    rc, out, err = box.run(SCRIPT, cache)
    check(rc == 0, f"single build failed: {err}")
    goroot = out.strip()
    check(goroot.endswith("/go"), f"unexpected output {goroot!r}")
    check(os.access(os.path.join(goroot, "bin", "go"), os.X_OK), "no go binary")
    entry = os.path.dirname(goroot)
    digest = hashlib.sha256(open(SCRIPT, "rb").read()).hexdigest()
    check(entry == os.path.join(cache, f"{VERSION}-{ARCH}-{digest}"),
          f"unexpected cache entry {entry!r}")
    marker = open(os.path.join(entry, "complete"), encoding="utf-8").read().strip()
    check(marker == f"{VERSION}:{ARCH}:{digest}", f"bad completion marker {marker!r}")
    check(os.path.isfile(lock), "lock is not a plain file")
    check(not lock_held(lock), "lock still held after the build")
    leftovers = [n for n in os.listdir(cache) if n.startswith(".build.") or n.endswith(".lock")]
    check(not leftovers, f"run-scoped leftovers in the cache dir: {leftovers}")
    check(serialized(box.events()), f"builds overlapped: {box.events()}")


def barrier_scenario(box, script, cache, tag):
    """Starts one build, waits until it is inside make.bash, then starts another.

    Returns (returncode, stderr, build events) of the second build.
    """
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
    lock = lock_path(SCRIPT, cache, box.locks)
    rc, err, events = barrier_scenario(box, SCRIPT, cache, "live")
    check(rc != 0, "a second build was allowed to run next to a live one")
    check("another build owns this toolchain cache entry" in err, f"unexpected refusal: {err}")
    check(events.count("enter") == 1, f"the refused build started building: {events}")
    check(serialized(events), f"builds overlapped: {events}")
    check(not lock_held(lock), "lock not released after the holding build finished")


def case_sigkill_recovery(box):
    box.reset_logs()
    cache = os.path.join(box.cache, "killed")
    lock = lock_path(SCRIPT, cache, box.locks)
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


def case_mutation_reclaim_race(box):
    """The old reclaim protocol loses exclusion under a forced interleaving."""
    box.reset_logs()
    script = mutant(SCRIPT, box.root, "reclaim", OLD_LOCK)
    cache = os.path.join(box.cache, "mutant-reclaim")
    lock_dir = lock_path(script, cache, box.locks)
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
    dead = subprocess.run(["bash", "-c", "echo $$"], capture_output=True, text=True).stdout.strip()
    entry = os.path.join(cache, f"{VERSION}-{ARCH}-" + hashlib.sha256(
        open(script, "rb").read()).hexdigest())
    os.makedirs(os.path.join(entry + ".lock"), exist_ok=True)
    with open(os.path.join(entry + ".lock", "owner"), "w", encoding="utf-8") as handle:
        handle.write(dead + "\n")
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
    ]
    mutations = [
        ("7 mutant: reclaim race", case_mutation_reclaim_race),
        ("8 mutant: no recovery", case_mutation_no_recovery),
        ("9 mutant: no lock", case_mutation_no_lock),
    ]
    failures = 0
    for title, case in cases:
        box = Sandbox()
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
