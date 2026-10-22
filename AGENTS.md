# Project rules

- This is a CLIProxyAPI native C ABI plugin for local harness usage reports.
- Keep collection behind internal/usage; never collect on the proxy request path.
- Do not expose raw logs, prompts, local paths, project names, session IDs, credentials, or account details.
- Browser resources are public shells. Usage data must use CPA authenticated management routes.
- Use synthetic fixtures in tests; do not inspect the user's real harness logs for verification.
- Pin upstream versions and verify actual output schemas before updating adapters.
- Do not sum provider quota metrics and local token counters together.
- Run make test and make build after Go changes.
- Compile each target exactly once per change. Pull request CI runs the vet and race tests only; the push that lands the change on main builds the six targets; a release tag on that commit promotes those artifacts and must not rebuild. Do not reintroduce a build matrix on pull requests.
- darwin/amd64 shared libraries are built using the isolated slot-11 patched Go toolchain (scripts/darwin-amd64-go.sh) to resolve the Go host TLS conflict, verified via native Go host smoke test, and checked by scripts/check-darwin.sh. Because CPA's host process does not isolate runtime address spaces, multiple Go plugins concurrently loaded in the same host that share TLS slot 11 conflict.
- Keep run-scoped state (toolchain locks) out of .cache/tlsgo: CI restores and saves that directory even on cancelled runs, so a lock saved there would fail every later restore. The toolchain lock is an flock(2) the kernel holds for the life of the build (taken via lockf(1) on a file under TMPDIR), never a directory a later run inspects and removes: the liveness check and the removal are separate steps, so two runs that both see a dead owner can each delete the other's fresh lock. scripts/test-darwin-amd64-go-lock.py covers the protocol with synthetic fixtures. Pin darwin builds to the toolchain version in go.mod, because the Go linker chooses the darwin floor itself (12.0 on Go 1.26, 13.0 on Go 1.27+) and MACOSX_DEPLOYMENT_TARGET only stamps the LC_BUILD_VERSION header.
