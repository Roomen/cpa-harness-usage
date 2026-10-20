# CPA Harness Usage

CLIProxyAPI native plugin for local coding-harness usage reports. It collects
daily token and estimated-cost history for every harness the pinned
[ccusage](https://github.com/ccusage/ccusage) collector supports — Claude Code,
Codex, OpenCode, oh-my-pi (omp), pi, Amp, Droid, Codebuff, Hermes, Goose, Kilo,
GitHub Copilot CLI, Gemini CLI, Antigravity, Kimi, Qwen, OpenClaw, Grok CLI, and
ZCode — and shows it in an authenticated management dashboard.

The plugin reads local harness history, not proxy traffic. Usage from harness
sessions that never went through CLIProxyAPI is included. Nothing is collected
on the proxy request path, and the plugin registers no proxy hooks.

## What you get

- Daily aggregates per source: input, output, cache read, cache write, total
  tokens, and estimated cost.
- Per-model breakdowns inside each day, so usage can be classified by model.
  Missing costs use locally saved CloudPrice rates when possible and remain
  explicitly unknown otherwise. Download prices only with the update button.
- One independent status per source: `ok`, `empty`, or `error` with a safe error
  code. A failed source never hides the others.
- Claude and Codex account quota (5-hour and weekly windows) for the local login and for the CPA credential pool, on its own route and schema. Quota is never added to or mixed with the local token counters.
- A responsive browser page: a sticky toolbar (today/7/30/90 days/all/custom range, source filter, auto-refresh off/5 min/15 min, refresh), quota cards with reset countdown and a pace marker, a usage summary, a stacked trend chart (day/week/month, grouped by source or model, a composition chart for a single day), a model ranking that doubles as the model filter, and details grouped by date. A custom range may start before the earliest recorded day; that part simply shows no data, and the applied range is validated as entered (never silently clamped). Below 960 px the filters fold into a bottom sheet. Light/dark follows CPA or the system.
- UI languages follow CPA: `zh-CN`, `zh-TW`, `en`, `ru`, `vi`, `ko`.
- A standalone JSON CLI (`bin/harness-usage`) that uses the same collector.

Estimated cost is API-price equivalent, not subscription spend, credits, or an invoice. Local token counters and account quota are separate metrics with separate routes; they are never summed or mixed.

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
      # Omit to collect every supported harness.
      sources: [claude, codex, opencode, omp]
      # oh-my-pi sessions. Omit to use PI_CODING_AGENT_DIR/sessions, then
      # ~/.omp/agent/sessions.
      omp_path: ""
```

`binary`, `sources`, and `omp_path` are validated in `plugin.register` and
`plugin.reconfigure`. Invalid configuration is rejected and the previous
collector stays active. Omitted fields use the defaults above. Unknown fields
are preserved by CPA and ignored by the plugin.

`sources` accepts any subset of `claude`, `codex`, `opencode`, `omp`, `pi`,
`amp`, `droid`, `codebuff`, `hermes`, `goose`, `kilo`, `copilot`, `gemini`,
`antigravity`, `kimi`, `qwen`, `openclaw`, `grok`, and `zcode`. The default is
all of them. The pinned collector starts one process per selected source, and
the plugin caps that parallelism at four, so a full scan does not fan out to
nineteen processes at once.

oh-my-pi stores pi-format sessions under `~/.omp/agent/sessions`, which ccusage
only finds when pointed at it. The plugin passes that path to ccusage's pi
adapter, honours `PI_CODING_AGENT_DIR` when set, and strips the adapter's
`[pi]`/`[omp]` prefix from model names in the report.

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
| `GET /v0/management/harness-usage/quota` | CPA management key | Account quota |
| `GET /v0/management/harness-usage/pricing` | CPA management key | Saved CloudPrice status |
| `POST /v0/management/harness-usage/pricing` | CPA management key | Download and save CloudPrice rates |

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
          "costUSD": 0.00061375,
          "models": [
            {
              "model": "gpt-5",
              "inputTokens": 90,
              "outputTokens": 50,
              "cacheCreationTokens": 0,
              "cacheReadTokens": 10,
              "reasoningOutputTokens": 5,
              "totalTokens": 155
            }
          ]
        }
      ]
    }
  ]
}
```

`sources[].status` is `ok` (rows present), `empty` (scan succeeded, no local
history), or `error`. Error codes are privacy-safe: `timeout`,
`command_failed`, `output_too_large`, `unknown_schema`, `canceled`,
`unsupported_source`. Raw logs, prompts, paths, project names, session IDs,
account data, and credentials never leave the collector. Model names are the
only per-request detail included, because the dashboard classifies usage by
model; they are treated as opaque labels, stripped of control characters, and
truncated to 120 characters.

`models` may be empty when an adapter reports no breakdown, and upstream
adapters may omit per-model `costUSD`; the plugin fills it when downloaded price
data and token counters support an estimate. A model's `totalTokens` is the
source's reported value when present, otherwise the sum of its non-overlapping
components. Model totals may leave usage unattributed, but they can never exceed
the day's reported total; that is rejected as `unknown_schema`. The dashboard
shows the remainder as "unattributed" so a day always adds up to its reported
total.

The plugin ships no fallback price list. Click **Update prices (CloudPrice)**
to download the public token-price map documented at
<https://cloudprice.net/models/api> from
<https://ai.cloudprice.net/api/v1/litellm_model_prices.json>. The map uses
LiteLLM-compatible field names, but CloudPrice is the download source.
Only this explicit click makes a price request. Opening the page, collecting
usage, ordinary refresh, and auto-refresh never download prices.

The backend downloads through CPA's `host.http.do` (including CPA proxy
settings), with a 45-second timeout and a 32 MiB size limit. No management key,
account credentials, model selection, or usage data is sent to CloudPrice.
Token prices are validated as finite, nonnegative USD-per-token values; entries
without both input and output rates are omitted. A failed download, invalid map,
or failed disk write leaves the previous cache intact. Concurrent update requests
return `409` instead of starting a second download.

Validated prices and timestamps are saved atomically to
`<OS user cache>/cpa-harness-usage/pricing/cloudprice.json` with a private
directory and file permissions. The plugin and standalone CLI load this file on
startup and use it offline on later calculations, including after a restart.
The dashboard shows the last successful download time, usable model count, and
CloudPrice's dataset date when provided. These dates describe the saved data;
prices are not refreshed automatically. The cache belongs to the OS account
running CPA, which can differ from the account running the standalone CLI.

Model lookup is exact, with explicit DeepSeek spelling aliases only when their
target exists in the downloaded map. Provider prefixes are preserved; the plugin
does not borrow the cheapest price from another provider. A model without a
matching rate for its used counters (including cache rates) is marked
`missingPricing`, and the dashboard shows unknown cost rather than zero.
Without a saved price map, estimates such as Codex model costs remain unknown.
Positive recorded source amounts are retained when available; costs derived by
the native collector are recalculated when their provenance is distinguishable.
Codex's native prices are always replaced by downloaded standard rates, so its
daily and model estimates use the same map. Estimates are marked `costEstimated`
and do not reconstruct request-level speed, context tiers, or historical price
changes. These figures are API-price equivalents, not subscription charges.

Authenticated management endpoints:

| Route | Purpose |
| --- | --- |
| `GET /v0/management/harness-usage/pricing` | Saved price status; never downloads, even with `?refresh=1` |
| `POST /v0/management/harness-usage/pricing` | Explicit download, validation, atomic save, and usage-cache invalidation |

Status responses contain `source: "CloudPrice"`, `status: "empty"` or `"ready"`,
`modelCount`, and optional `updatedAt` / `upstreamUpdatedAt`. No cache path or raw
price document is exposed through the management response.

### Token accounting

`totalTokens` is the value reported by the pinned ccusage version and is never
recomputed by adding components. That avoids double counting because the
sources differ:

| Source | Cost field | Reported total |
| --- | --- | --- |
| Claude Code | `totalCost` | input + output + cache creation + cache read |
| Codex | `costUSD` | non-cached input + cached input + output + reasoning |
| OpenCode | `totalCost` | input + output + cache read + cache write (reasoning is folded into the reported total) |
| oh-my-pi (pi adapter) | `totalCost` | input + output + cache read + cache write (the session's own total wins) |

Codex reports cached input separately from `inputTokens`; the plugin therefore
shows non-cached input as "input" and never adds cache again. Cache and
reasoning counters are displayed for diagnosis but the total is authoritative.
For estimated costs, the adapter's standard price calculation uses its input,
cached-input, and output counters; it does not price the separate reasoning
counter a second time.
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

## Account quota

`GET /v0/management/harness-usage/quota` reports subscription windows for Claude and Codex accounts. It is independent of `/daily`: a quota failure never affects usage and the reverse.

Report shape:

```json
{
  "schemaVersion": 1,
  "generatedAt": "2026-10-20T03:00:00Z",
  "refreshedAt": "2026-10-20T02:58:00Z",
  "poolStatus": "ok",
  "accounts": [
    {
      "id": "local-claude",
      "provider": "claude",
      "origin": "local",
      "mergedPool": [2],
      "plan": "Max 20x",
      "status": "ok",
      "source": "live",
      "observedAt": "2026-10-20T02:58:00Z",
      "windows": [
        { "kind": "session", "windowSeconds": 18000, "usedPercent": 72, "resetsAt": "2026-10-20T04:12:00Z" },
        { "kind": "weekly", "windowSeconds": 604800, "usedPercent": 41, "resetsAt": "2026-10-23T12:00:00Z" }
      ]
    },
    { "id": "pool-3", "provider": "codex", "origin": "pool", "poolNumber": 3, "status": "error", "error": "token_expired", "windows": [] }
  ]
}
```

- `id` is `local-claude`, `local-codex`, or `pool-<n>`; `poolNumber` is the account's position in the CPA pool ordered by auth index. `mergedPool` lists pool numbers that are the same account as this card and were not queried again.
- `source` is `live` (fetched in this refresh), `cache` (reused from an earlier refresh within five minutes), or `session_log` (Codex's own rate-limit snapshot from its latest local session). `liveError` is set when the live query failed and a session-log value is shown instead.
- `kind` is `session`, `daily`, `weekly`, `monthly`, or `other`, derived from the window length. `usedPercent` is omitted when the balance is unknown: `resetSinceObserved` marks a session-log window whose reset time passed after it was recorded, so the old percentage no longer applies and the page shows "unknown" instead of 100% remaining. The page applies the same rule while it holds a report: as soon as a window's `resetsAt` passes (live, cached, or session-log), that window renders as "unknown" until the next refresh, and a timer re-renders at that instant without a network request.
- `poolStatus` is `ok`, `unavailable` (no host callbacks), or `error` (the pool could not be listed; local accounts are still reported).
- Error codes: `not_configured`, `token_expired`, `upstream_error`, `rate_limited`, `keychain_denied`, `unknown_schema`, `canceled`.

Account sources:

- Local Claude: `CLAUDE_CODE_OAUTH_TOKEN`, then the macOS keychain item `Claude Code-credentials`, then `$CLAUDE_CONFIG_DIR/.credentials.json` or `~/.claude/.credentials.json`.
- Local Codex: `$CODEX_HOME/auth.json` or `~/.codex/auth.json` (ChatGPT login only; an API-key login is `not_configured`). Its session logs under `sessions/YYYY/MM/DD/rollout-*.jsonl` provide the offline fallback. Files are chosen by modification time across the whole tree (the six newest, at most 4096 directories visited), because a resumed old session keeps appending to the directory it was created in; only the tail of each file is read and an observation older than eight days is ignored.
- CPA pool: enabled `claude` and `codex` auth files from `host.auth.list` / `host.auth.get`. Disabled and runtime-only entries are skipped.
- Accounts are identified by provider plus account ID (Codex `account_id`, Claude account UUID from the credential or from the OAuth profile). A pool entry that matches the local login is merged into the local card. Credentials whose recorded expiry has passed are never sent: they stay in their account's group so a usable credential of the same account still supplies the card, and an account whose credentials are all expired reports one `token_expired` card.

Requests: Claude uses `GET https://api.anthropic.com/api/oauth/usage` (and `/api/oauth/profile` for identity, cached for one hour per token fingerprint) with `anthropic-beta: oauth-2025-04-20`. Codex uses `GET https://chatgpt.com/backend-api/wham/usage` with `chatgpt-account-id`. Every request goes through CPA's `host.http.do` callback, so CPA's proxy settings apply; the plugin opens no network connection itself. Each request has a 15-second timeout, a refresh is capped at 45 seconds, and at most four accounts are queried in parallel.

Refresh and cache: a plain `GET` never touches the network; it returns the last refresh plus the Codex session log. `?refresh=1` queries providers, but each account's result is reused for five minutes (errors for one minute), and concurrent refreshes share one run. The plugin has no background timer. The page asks for `?refresh=1` once when it opens and the last refresh is older than five minutes, on ⟳, and on auto-refresh ticks while it is visible; a hidden or closed page sends nothing.

Privacy: the report never contains tokens, emails, account IDs, file paths, file names, or raw upstream bodies. Accounts are labelled only by provider, origin, and pool number; plan names are reduced to a fixed character whitelist.

Do not refresh OAuth tokens in this plugin. A 401/403 is reported as `token_expired`; the CLI (local login) or CPA (pool) refreshes its own token. Refreshing here would rotate a single-use refresh token behind the owner's back and log the CLI or CPA out.

Upstream bodies are validated before they are cached. A Codex usage response must contain `rate_limit`; `rate_limit: null` is the documented free-plan shape and reports no windows, while a missing object, a body without `rate_limit`, or a window without `used_percent`/`limit_window_seconds` is `unknown_schema` and leaves the session-log fallback in place.

Do not call the usage endpoints with `net/http` directly. CPA's `host.http.do` applies the configured proxy; a direct client would bypass it and fail on hosts that only reach the providers through the proxy.

On macOS, the first keychain read shows a system prompt asking whether CPA may read `Claude Code-credentials`. Denying it reports `keychain_denied`; "Always Allow" avoids later prompts. The keychain reader needs cgo on darwin; other builds skip it and use the credential file.

The Codex session-log format was checked against openai/codex `rust-v0.163.0` (`token_count` events with `rate_limits.primary/secondary`, `used_percent`, `window_minutes`, `resets_at`). The older `resets_in_seconds` field is still accepted. Compressed rollouts (`.jsonl.zst`, older than seven days) are skipped because their windows have already reset.

## Standalone CLI

```sh
./bin/harness-usage \
  --sources claude,codex,opencode,omp \
  --omp-path ~/.omp/agent/sessions \
  --timeout 45s
```

Without `--sources`, every supported harness is collected. `--omp-path`
overrides the oh-my-pi sessions directory.

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
    PI_CODING_AGENT_DIR="$TMP/omp-agent" \
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
- Sources run in parallel, capped at four collector processes. Each source has
  its own timeout and output cap, so one slow harness cannot block the report.
- The dashboard's auto-refresh (off, 5 min, 15 min) reuses the plugin's 60-second usage cache and never forces a rescan, runs only while the page is open and visible, and stops when the key is rejected. Only an explicit Refresh requests `/daily?refresh=1`. Quota network queries from the page are at least five minutes apart.
- `plugin.quiesce` pauses collection and cancels the active scan. A subsequent
  reconfigure (including a host rollback) resumes it.
- `plugin.shutdown` cancels scans, clears the cache, and is idempotent. Shutdown
  never blocks on a running ccusage process.
- While paused or closed, the data and quota routes return HTTP 503 with a safe JSON body. Shutdown cancels in-flight quota requests through `host.http.cancel` and drops the host callbacks.

## Screenshots

Rendered from the built page with a local mock serving synthetic data:

| Width | State | File |
| --- | --- | --- |
| 360 | Normal, light | `docs/screenshots/360-normal-light.png` |
| 360 | Normal, dark | `docs/screenshots/360-normal-dark.png` |
| 360 | Filter sheet, light | `docs/screenshots/360-filters-light.png` |
| 360 | Authentication, dark | `docs/screenshots/360-auth-dark.png` |
| 360 | Loading, dark | `docs/screenshots/360-loading-dark.png` |
| 768 | Normal, light | `docs/screenshots/768-normal-light.png` |
| 768 | Normal, dark | `docs/screenshots/768-normal-dark.png` |
| 768 | CPA pool expanded, light | `docs/screenshots/768-pool-light.png` |
| 768 | Empty, light | `docs/screenshots/768-empty-light.png` |
| 1440 | Normal, light | `docs/screenshots/1440-normal-light.png` |
| 1440 | Normal, dark | `docs/screenshots/1440-normal-dark.png` |
| 1440 | CPA pool expanded, dark | `docs/screenshots/1440-pool-dark.png` |
| 1440 | Single day, light | `docs/screenshots/1440-today-light.png` |
| 1440 | Partial failure (usage source, keychain, pool), dark | `docs/screenshots/1440-partial-dark.png` |

## Verification status

Verified in this repository:

- `make test` (vet + race detector) and `make build` on macOS arm64.
- `make collector-test` against the embedded native collector with synthetic
  Claude, Codex, OpenCode, and oh-my-pi histories, plus empty reports for each
  source. Model breakdowns from each adapter are covered.
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
- Collector and CLI against synthetic Claude Code, Codex, OpenCode, and oh-my-pi
  data in an isolated environment, including a source failure that left the
  others usable. A full default scan of all 19 sources against empty synthetic
  homes finished in 0.4 s on macOS arm64.
- Dashboard behavior with a local mock of the two routes and with a report
  produced by the real embedded collector: normal, empty, partial failure,
  loading, authentication failure, retry/refresh, source/model/date filters,
  chart grouping by source and by model, model ranking, saved-key auto-load from
  the management center, language switching, light/dark, and layout at 360, 768,
  and 1440 px. Document width never exceeded the viewport at any of the three
  widths; the table scrolls inside its own container on phones. Auto-refresh was
  checked to pause while the tab is hidden and to perform zero DOM mutations
  when the snapshot is unchanged. Deselecting every model keeps the model panel
  and its checkboxes visible so the selection can be restored, and the initial
  date-range widening runs only on the first snapshot, never over a range the
  user chose. Screenshots are in `docs/screenshots/`.
- Quota: table-driven tests with a fake host for credential sources, response parsing, status mapping, dedup and pool merge, privacy of the report, cache and error TTLs, shared concurrent refresh, cancellation, and session-log parsing; `make smoke` checks the offline quota route through the C ABI. The page's quota cards, pool expansion, auto-refresh requests (none while hidden), and zero usage-DOM mutations on an unchanged auto-refresh tick were checked against the local mock.

Not verified:

- Loading into a running CLIProxyAPI instance. No real CPA configuration was
  modified and the plugin was not installed into a live service. CPA-side menu
  rendering, hot reload against a real process, and the actual management-key
  middleware remain integration checks for a disposable instance.
- Real browser rendering inside the official management center shell.
- The manual macOS Intel experiment.
- Quota against the real Anthropic/OpenAI endpoints and a real CPA pool; the Claude OAuth profile shape is unverified offline.

## Releases and plugin store

The canonical repository is
[Roomen/cpa-harness-usage](https://github.com/Roomen/cpa-harness-usage).
The current release is [v0.1.0](https://github.com/Roomen/cpa-harness-usage/releases/tag/v0.1.0).
`Version` and `Repository` default to the current release values and can be
overridden at build time:

```sh
make collector
go build -tags bundledcollector -buildmode=c-shared -ldflags \
  '-X cpa-harness-usage/internal/plugin.Version=0.1.0 \
   -X cpa-harness-usage/internal/plugin.Repository=https://github.com/Roomen/cpa-harness-usage' \
  -o bin/harness-usage.dylib ./cmd/plugin
```

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`. A library is
compiled once per commit: the CI matrix is the only place a target build runs
(`.github/workflows/build-target.yml`), and a release on the same commit
**reuses those CI artifacts** instead of rebuilding them. The release packages
`<pluginID>_<version>_<goos>_<goarch>.zip` plus `checksums.txt` from the
downloaded libraries and publishes them.

Reuse requires a successful CI run for the tagged commit with all five build
artifacts. Otherwise, including `make`-local runs, manual `workflow_dispatch`
with `rebuild`, and commits pushed before CI existed, the release falls back to
building the five targets once and logs that decision in the job summary. The
release also checks that `internal/plugin/plugin.go` declares the tagged
version, because the library embeds it from source rather than from
`-ldflags`; a mismatch fails before anything is published.

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
creating a release is only triggered by a version tag. Commits that only touch
documentation, screenshots, or license files skip CI, since they cannot change
a binary; a release on such a commit rebuilds on demand.
Runner images use explicit versions. A newer CI run cancels an older run on the
same branch; release workflows are not automatically canceled.

GitHub Actions caches npm downloads, Go modules, Go compilation results, and C
toolchains. Module caches follow the dependency lock; compilation caches are
saved per run and restore the latest matching platform, Go version, and build
profile. Test and plugin-build profiles are separate, and CI and the release
fallback share the same build cache keys. Zig compilation results have a separate rolling cache
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
internal/quota/        Account quota: credentials, host HTTP, providers, session log, cache
internal/usage/testdata/fakeccusage/  Synthetic collector test double
web/                   Embedded dashboard page (no external assets)
configs/               CPA configuration example
```

See `NOTICE` and the bundled license files for upstream attribution. ccusage is
invoked as a separate native process; its fixed-version binary and license are
embedded at build time, without copying its source into this project.
