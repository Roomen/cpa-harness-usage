# Project rules

- This is a CLIProxyAPI native C ABI plugin for local harness usage reports.
- Keep collection behind internal/usage; never collect on the proxy request path.
- Do not expose raw logs, prompts, local paths, project names, session IDs, credentials, or account details.
- Browser resources are public shells. Usage data must use CPA authenticated management routes.
- Use synthetic fixtures in tests; do not inspect the user's real harness logs for verification.
- Pin upstream versions and verify actual output schemas before updating adapters.
- Do not sum provider quota metrics and local token counters together.
- Run make test and make build after Go changes.
- Stock Go darwin/amd64 shared libraries have a known TLS conflict in a Go host; this target is blocked until a patched toolchain is integrated.
