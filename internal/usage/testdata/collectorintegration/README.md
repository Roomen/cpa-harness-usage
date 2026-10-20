# Native collector contract fixtures

These files contain invented data only. They exercise the official ccusage
20.0.36 native executable with isolated home, configuration, data, and cache
directories and `--offline` pricing.

Input formats were checked against the pinned upstream sources:

- <https://github.com/ryoppippi/ccusage/blob/v20.0.36/rust/adapters/claude/src/lib.rs>
- <https://github.com/ryoppippi/ccusage/blob/v20.0.36/rust/adapters/codex/src/lib.rs>
- <https://github.com/ryoppippi/ccusage/blob/v20.0.36/rust/adapters/opencode/src/loader.rs>

The Claude fixture repeats a message to check deduplication and preserves its
recorded USD cost. The Codex fixture contains two cumulative token events, so
cached input must be separated from input and cumulative counters must not be
summed. Its USD cost uses the native executable's bundled offline prices.
OpenCode preserves its recorded USD cost and includes reasoning in total
tokens, although this version's focused daily JSON omits a separate reasoning
counter. The adapter must preserve that reported total.

Run `make collector-test` to prepare the pinned native collector and execute
`go test -tags 'bundledcollector collectorintegration' ./internal/usage`.
The test materializes the embedded executable in a temporary private cache.
Alternatively, point `HARNESS_USAGE_TEST_BINARY` at an absolute executable path.
The tests also check successful empty reports for all three sources.
