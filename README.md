# CPA Harness Usage

CLIProxyAPI native plugin for local coding-harness usage reports. It collects
daily token and estimated-cost history for **Claude Code**, **Codex**, and
**OpenCode** from this machine, using [ccusage](https://github.com/ccusage/ccusage)
as a separate collector, and shows it in an authenticated management dashboard.

The plugin reads local harness history, not proxy traffic. Usage from harness
sessions that never went through CLIProxyAPI is included. Nothing is collected
on the proxy request path, and the plugin registers no proxy hooks.

## What you get

- Daily aggregates per source: input, output, cache read, cache write, total
  tokens, and estimated cost.
- One independent status per source: `ok`, `empty`, or `error` with a safe error
  code. A failed source never hides the others.
- A responsive browser page with summary cards, a stacked daily chart, source
  and date-range filters, a detail table, and light/dark following the system.
- UI languages follow CPA: `zh-CN`, `zh-TW`, `en`, `ru`, `vi`, `ko`.
- A standalone JSON CLI (`bin/harness-usage`) that uses the same collector.

Estimated cost is API-price equivalent, not subscription spend, credits, or an
invoice. Local token counters and account quota are separate metrics and are
never mixed here.

## Requirements

- CLIProxyAPI v8.0.29 or newer with native plugin support (RPC schema 6).
  The contract in this repository was checked against the local v8.0.29 source.
- Go 1.26+, CGO, and a C compiler for the plugin build.
- Node.js/npm on build hosts to install ccusage **20.0.36** from
  `package-lock.json`. Published plugins embed its native executable; CPA hosts
  do not need Node.js/npm or a separate collector installation.
- The CPA process must run as the user whose harness logs should be read.
  A remote CPA or container cannot see workstation logs without explicit access.

## Build and test

```sh
make deps    # npm ci --ignore-scripts  (installs ccusage 20.0.36)
make test    # go vet + go test -race
make build   # bin/harness-usage.dylib|so|dll and bin/harness-usage
make collector-test # real pinned collector with isolated synthetic histories
make smoke   # stock Go host: synthetic collector and isolated embedded collector
```

`make build` prepares and embeds the native collector for the selected target.
Local builds use the host's native npm package; arbitrary `GOOS`/`GOARCH`
cross-builds also require installing the corresponding pinned native package.
`darwin/amd64` remains excluded from normal releases. A single-plugin experiment
is available with `make smoke EXPERIMENTAL_DARWIN_AMD64=1` on an Intel Mac. It
uses `scripts/darwin-amd64-go.sh` to build an isolated Go toolchain using TLS slot
11. Other plugins using the same patch conflict with that slot; CPA's loader
does not isolate their runtimes. A successful single-plugin smoke test does not
establish multi-plugin safety. The host and standalone CLI use stock Go. The
script verifies the official Go source archive, refuses source layouts it cannot
patch exactly, and never modifies the system toolchain. Python 3 is needed for
this experimental toolchain preparation.

Local builds retain `.cache/go-build`, `.cache/go-mod`, `.cache/npm`, and
`.cache/tlsgo`; these directories are ignored by Git. Existing environment
overrides for the Go/npm caches remain respected. `make deps` reuses the
installed dependencies while the package files are unchanged. Collector asset
preparation does not rewrite unchanged files, preserving Go's incremental build
cache. Patched toolchains are keyed by exact Go version, host architecture, and
patch script hash. No build target cleans these caches automatically.

The collector has no shell invocation: it executes `ccusage <source> daily
--json --offline` directly, with a per-source timeout (45 s by default), a
16 MiB output cap, discarded stderr, and a 60-second in-memory aggregate cache.

## Install into CLIProxyAPI

1. Install collector dependencies and build the library.
2. Copy `bin/harness-usage.<ext>` into `<plugins.dir>/<goos>/<goarch>/`
   (for example `plugins/darwin/arm64/harness-usage.dylib`).
3. Merge `configs/plugin.example.yaml` into CPA's configuration. Omit `binary`
   or use `bundled` for the embedded native collector. An explicit native
   executable path overrides it; on Windows, use an `.exe`, not an npm `.cmd` shim.
4. Restart or hot-reload CPA, then confirm the plugin is registered under
   `GET /v0/management/plugins`.
5. Open `/v0/resource/plugins/harness-usage/usage` and enter the CPA management
   key.

Configuration example:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    harness-usage:
      enabled: true
      priority: 0
      binary: bundled
      sources: [claude, codex, opencode]
```

`binary` and `sources` are validated in `plugin.register` and
`plugin.reconfigure`. Invalid configuration is rejected and the previous
collector stays active. Omitted fields use the defaults above. Unknown fields
are preserved by CPA and ignored by the plugin.

The plugin ID is `harness-usage`; the management UI reads it from the library
filename.

The CPA store installs only the dynamic library from its zip, so the collector
and its license are embedded in that library. On first collection, the native
program is verified and atomically extracted into a private directory under the
current user's OS cache directory. Its content hash and platform identify the
cache entry; later scans verify and reuse the executable. A writable, executable
cache directory is required. Failed extraction returns the safe
`command_failed` status, never a local path. The plugin never downloads updates
at runtime and never reads histories during registration or proxy requests.

## HTTP surface

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /v0/resource/plugins/harness-usage/usage` | Public static shell | Dashboard page |
| `GET /v0/management/harness-usage/daily` | CPA management key | Usage report |

The page contains no usage data and no third-party scripts. It requests the
report from the management route with `Authorization: Bearer <key>`:

- The key is held in page memory only. It is never put in a URL, logged,
  written to `localStorage`/`sessionStorage`, or persisted by the plugin.
- When the page runs on the same origin as the CPA management center, it reads
  the management center's existing saved key once, in memory, to avoid a second
  login prompt. Cross-origin deployments fall back to manual entry.
- `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: no-referrer`, and a restrictive CSP are set. The page loads
  no third-party scripts.

`GET /v0/management/harness-usage/daily?refresh=1` bypasses the in-memory cache.
Concurrent requests share a single running scan, so repeated retries cannot
start multiple collectors.

Report shape:

```json
{
  "schemaVersion": 1,
  "generatedAt": "2026-10-19T09:31:26Z",
  "sources": [
    {
      "source": "codex",
      "status": "ok",
      "daily": [
        {
          "date": "2026-10-19",
          "inputTokens": 90,
          "outputTokens": 50,
          "cacheCreationTokens": 0,
          "cacheReadTokens": 10,
          "reasoningOutputTokens": 5,
          "totalTokens": 155,
          "costUSD": 0.00061375
        }
      ]
    }
  ]
}
```

`sources[].status` is `ok` (rows present), `empty` (scan succeeded, no local
history), or `error`. Error codes are privacy-safe: `timeout`,
`command_failed`, `output_too_large`, `unknown_schema`, `canceled`,
`unsupported_source`. Raw logs, prompts, paths, project names, model names,
session IDs, account data, and credentials never leave the collector.

### Token accounting

`totalTokens` is the value reported by the pinned ccusage version and is never
recomputed by adding components. That avoids double counting because the
sources differ:

| Source | Cost field | Reported total |
| --- | --- | --- |
| Claude Code | `totalCost` | input + output + cache creation + cache read |
| Codex | `costUSD` | non-cached input + cached input + output + reasoning |
| OpenCode | `totalCost` | input + output + cache read + cache write (reasoning is folded into the reported total) |

Codex reports cached input separately from `inputTokens`; the plugin therefore
shows non-cached input as "input" and never adds cache again. Cache and
reasoning counters are displayed for diagnosis but the total is authoritative.
The chart stacks the four displayed components plus a gray "other tokens"
remainder (for example reasoning tokens) so the stack still equals the total.
Rows whose reported total is smaller than the non-overlapping components are
rejected as an incompatible schema instead of being displayed incorrectly.

### Empty data versus schema drift

`{"daily": []}` is a successful empty scan and is shown as "no data" per
source. Missing `daily`, non-array rows, unknown date shapes, missing counters,
negative or fractionally invalid values, and inconsistent totals are treated as
`unknown_schema`, so a changed upstream contract is visible instead of being
silently rendered as zero usage.

## Standalone CLI

```sh
./bin/harness-usage \
  --sources claude,codex,opencode \
  --timeout 45s
```

The CLI prints the same JSON report. Its exit status stays zero when individual
sources fail; inspect `sources[].status` and `sources[].error`. It never reads
CPA configuration or credentials.

`make build` embeds the collector in the CLI as well. Plain `go build` without
the `bundledcollector` tag needs an explicit `--binary` override; it does not
silently fall back to another installed ccusage version.

## Local development and test isolation

Tests use synthetic fixtures only. `internal/usage/testdata/fakeccusage` is a
compiled test double, so no test reads real harness logs. When exercising the
real collector manually, isolate every data location:

```sh
env HOME="$TMP/home" \
    CLAUDE_CONFIG_DIR="$TMP/claude" \
    CODEX_HOME="$TMP/codex" \
    OPENCODE_DATA_DIR="$TMP/opencode" \
    XDG_CONFIG_HOME="$TMP/xdg/config" \
    XDG_DATA_HOME="$TMP/xdg/data" \
    XDG_CACHE_HOME="$TMP/xdg/cache" \
    XDG_STATE_HOME="$TMP/xdg/state" \
    ./bin/harness-usage --binary ./node_modules/.bin/ccusage
```

Synthetic daily rows for each source can be produced from tiny JSONL/JSON
files under those directories. `--offline` keeps ccusage on locally cached
pricing and avoids network fetches.

## Lifecycle and concurrency

- `plugin.register` and `plugin.reconfigure` validate configuration and swap
  the collector without waiting for a running scan. Results from the old
  generation are not cached.
- One scan runs at a time; callers that arrive during a scan wait for its
  result instead of starting another one.
- `plugin.quiesce` pauses collection and cancels the active scan. A subsequent
  reconfigure (including a host rollback) resumes it.
- `plugin.shutdown` cancels scans, clears the cache, and is idempotent. Shutdown
  never blocks on a running ccusage process.
- While paused or closed, the data route returns HTTP 503 with a safe JSON body.

## Screenshots

Rendered from the built page with synthetic data:

| Width | State | File |
| --- | --- | --- |
| 360 | Normal, dark | `docs/screenshots/360-normal.png` |
| 360 | Empty, dark | `docs/screenshots/360-empty.png` |
| 360 | Auth failure, dark | `docs/screenshots/360-auth.png` |
| 360 | Loading, dark | `docs/screenshots/360-loading.png` |
| 768 | Normal, light | `docs/screenshots/768-normal.png` |
| 768 | Empty, light | `docs/screenshots/768-empty.png` |
| 1440 | Normal, light | `docs/screenshots/1440-normal.png` |
| 1440 | Partial failure, dark | `docs/screenshots/1440-partial.png` |

## Verification status

Verified in this repository:

- `make test` (vet + race detector) and `make build` on macOS arm64.
- `make collector-test` against the embedded native collector with synthetic
  Claude, Codex, and OpenCode histories, plus empty reports for each source.
- `make smoke` from a stock Go host on macOS arm64, covering both a synthetic
  collector override and the real embedded collector in a fully isolated process.
- Linux amd64/arm64 shared libraries cross-built with Go 1.26.0 and Zig 0.15.2;
  ELF checks passed the GLIBC 2.17 ceiling.
- [GitHub CI](https://github.com/Roomen/cpa-harness-usage/actions/runs/39352631238)
  passed on all five native target runners, including both Windows architectures,
  real collector fixtures, stock Go host loading, artifact upload, and cache saving.
- The built library exports a working C ABI: a local `dlopen` smoke test called
  `cliproxy_plugin_init`, `plugin.register`, `management.register`, and
  `management.handle` successfully.
- Collector and CLI against synthetic Claude Code, Codex, and OpenCode data in
  an isolated environment, including a source failure that left the others
  usable.
- Dashboard behavior with a local mock of the two routes: normal, empty, partial
  failure, loading, authentication failure, retry/refresh, source/date filters,
  saved-key auto-load from the management center, language switching,
  light/dark, and layout at 360, 768, and 1440 px. Document width never exceeded
  the viewport at any of the three widths; the table scrolls inside its own
  container on phones. Screenshots are in `docs/screenshots/`.

Not verified:

- Loading into a running CLIProxyAPI instance. No real CPA configuration was
  modified and the plugin was not installed into a live service. CPA-side menu
  rendering, hot reload against a real process, and the actual management-key
  middleware remain integration checks for a disposable instance.
- Real browser rendering inside the official management center shell.
- The manual macOS Intel experiment.

## Releases and plugin store

The canonical repository is
[Roomen/cpa-harness-usage](https://github.com/Roomen/cpa-harness-usage).
The first release is [v0.0.1](https://github.com/Roomen/cpa-harness-usage/releases/tag/v0.0.1).
`Version` and `Repository` default to the current release values and can be
overridden at build time:

```sh
make collector
go build -tags bundledcollector -buildmode=c-shared -ldflags \
  '-X cpa-harness-usage/internal/plugin.Version=0.0.1 \
   -X cpa-harness-usage/internal/plugin.Repository=https://github.com/Roomen/cpa-harness-usage' \
  -o bin/harness-usage.dylib ./cmd/plugin
```

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`, which tests the
code, builds the five supported targets below, verifies native collector output and Go-host
library loading, and publishes
`<pluginID>_<version>_<goos>_<goarch>.zip` plus `checksums.txt`.

| Platform | Architectures | Build requirements |
| --- | --- | --- |
| macOS | arm64 | Intel is a manual single-plugin experiment, excluded from releases |
| Linux | amd64, arm64 | CI uses pinned Zig and checks the plugin's maximum GLIBC requirement is at most 2.17 |
| Windows | amd64, arm64 | Native LLVM-MinGW C compiler; collector runs directly as `.exe` |
| FreeBSD | Not yet supported | No official ccusage native package; source build and runtime verification remain required |

Windows hosts need the Microsoft Visual C++ Redistributable matching their
architecture: the official native collector imports the VC runtime and UCRT.
Preinstalled runtime libraries on CI runners do not remove this host requirement.

Pushing to `main` or opening a pull request automatically builds all five targets
on GitHub Actions. The CI workflow can also be started manually. Its successful
builds upload the plugin library and standalone CLI as per-platform artifacts;
creating a release is only triggered by a version tag.

GitHub Actions caches npm downloads, Go modules, Go compilation results, and C
toolchains. Module caches follow the dependency lock; compilation caches are
saved per run and restore the latest matching platform, Go version, and build
profile. Test and plugin-build profiles are separate, while CI and Release can
reuse matching caches. Zig compilation results have a separate rolling cache
from the fixed-version compiler installation. Build matrices run on native
target runners. The separate
manual `Darwin Intel experiment` workflow also caches its patched Go toolchain
and only uploads an experimental artifact; it never publishes to the store.
Adding matrix entries alone does not establish a successful build: remote CI
must complete before claiming target verification.

Dependabot checks ccusage weekly and opens an upgrade PR with an exact dependency
pin and updated lockfile. Every target runs the real collector on isolated
synthetic histories to check empty results, counters, costs, deduplication, and
upstream JSON compatibility. An incompatible upgrade must update the adapter
and fixtures through review before merge. There is no automatic merge or
in-process dependency update; the tested collector ships with the next plugin
release.

The personal registry
[Roomen/CLIProxyAPI-Plugins-Store](https://github.com/Roomen/CLIProxyAPI-Plugins-Store)
lists this plugin. Add its raw `registry.json` to `plugins.store-sources` to
install from the CPA plugin store:

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/Roomen/CLIProxyAPI-Plugins-Store/main/registry.json"
```

## Layout

```text
cmd/plugin/            Native C ABI entry point
cmd/harness-usage/     Standalone JSON CLI
internal/plugin/       Lifecycle, configuration, management routes, cache
internal/usage/        ccusage subprocess boundary and schema normalization
internal/usage/testdata/fakeccusage/  Synthetic collector test double
web/                   Embedded dashboard page (no external assets)
configs/               CPA configuration example
```

See `NOTICE` and the bundled license files for upstream attribution. ccusage is
invoked as a separate native process; its fixed-version binary and license are
embedded at build time, without copying its source into this project.
