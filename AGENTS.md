# Project rules

- This is a CLIProxyAPI native C ABI plugin for local harness usage reports.
- Keep collection behind internal/usage; never collect on the proxy request path.
- Do not expose raw logs, prompts, local paths, project names, session IDs, credentials, or account details.
- Browser resources are public shells. Usage data must use CPA authenticated management routes.
- Use synthetic fixtures in tests; do not inspect the user's real harness logs for verification.
- Pin upstream versions and verify actual output schemas before updating adapters.
- Do not sum provider quota metrics and local token counters together.
- Run make test and make build after Go changes.
- Compile each target exactly once per change. Pull request CI runs the vet and race tests only; the push that lands the change on main builds the five targets; a release tag on that commit promotes those artifacts and must not rebuild. Do not reintroduce a build matrix on pull requests.
- Stock Go darwin/amd64 shared libraries have a known TLS conflict in a Go host. The cached slot-11 patch is single-plugin experimental only and conflicts with other Go plugins using that slot; never include this target in normal releases.
