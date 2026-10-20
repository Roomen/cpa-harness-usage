//go:build collectorintegration

package usage

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestCollectorIntegration(t *testing.T) {
	binary := os.Getenv("HARNESS_USAGE_TEST_BINARY")
	if binary == "" {
		cache := t.TempDir()
		for _, name := range []string{"HOME", "XDG_CACHE_HOME", "LOCALAPPDATA"} {
			t.Setenv(name, cache)
		}
		var err error
		binary, err = bundledBinary()
		if err != nil {
			t.Fatal("prepare the bundled collector with the bundledcollector build tag or set HARNESS_USAGE_TEST_BINARY")
		}
	}
	absoluteBinary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal("resolve collector executable")
	}
	if _, err := os.Stat(absoluteBinary); err != nil {
		t.Fatal("prepare the pinned native collector or set HARNESS_USAGE_TEST_BINARY")
	}
	for _, source := range []string{"claude", "codex", "opencode"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			environment := integrationEnvironment(t, root)
			runIntegrationCollector(t, absoluteBinary, root, environment, source, "daily", "--help")
			empty := runIntegrationCollector(t, absoluteBinary, root, environment, source, "daily", "--json", "--offline")
			emptyRows, err := parseDaily(empty)
			if err != nil || len(emptyRows) != 0 {
				t.Fatal("empty native report does not match the daily contract")
			}
			var target string
			switch source {
			case "claude":
				target = "claude/projects/synthetic-project/synthetic-session.jsonl"
			case "codex":
				target = "codex/sessions/2026/01/02/rollout-synthetic.jsonl"
			case "opencode":
				target = "opencode/storage/message/synthetic-session/synthetic-message.json"
			}
			extension := ".jsonl"
			if source == "opencode" {
				extension = ".json"
			}
			fixture, err := os.ReadFile(filepath.Join("testdata", "collectorintegration", source+extension))
			if err != nil {
				t.Fatal("read synthetic fixture")
			}
			fixtureTarget := filepath.Join(root, filepath.FromSlash(target))
			if err := os.MkdirAll(filepath.Dir(fixtureTarget), 0700); err != nil {
				t.Fatal("create synthetic fixture directory")
			}
			if err := os.WriteFile(fixtureTarget, fixture, 0600); err != nil {
				t.Fatal("write synthetic fixture")
			}
			output := runIntegrationCollector(t, absoluteBinary, root, environment, source, "daily", "--json", "--offline")
			rows, err := parseDaily(output)
			if err != nil {
				t.Fatalf("native daily schema rejected: %v", err)
			}
			expected := DailyUsage{Date: "2026-01-02", InputTokens: 100, OutputTokens: 50, CacheCreationTokens: 20, CacheReadTokens: 10, TotalTokens: 180, CostUSD: 0.125}
			if source == "codex" {
				expected = DailyUsage{Date: "2026-01-02", InputTokens: 270, OutputTokens: 125, CacheReadTokens: 30, ReasoningTokens: 15, TotalTokens: 425, CostUSD: 0.00159125}
			} else if source == "opencode" {
				expected.TotalTokens = 185
			}
			if len(rows) != 1 {
				t.Fatalf("got %d daily rows, want one", len(rows))
			}
			if !reflect.DeepEqual(rows[0], expected) {
				t.Fatalf("normalized counters = %+v, want %+v", rows[0], expected)
			}
		})
	}
}

func integrationEnvironment(t *testing.T, root string) []string {
	t.Helper()
	environment := []string{"TZ=UTC", "NO_COLOR=1"}
	for name, directory := range map[string]string{
		"HOME": "home", "USERPROFILE": "home", "APPDATA": "appdata", "LOCALAPPDATA": "localappdata",
		"CODEX_HOME": "codex", "CLAUDE_CONFIG_DIR": "claude", "OPENCODE_DATA_DIR": "opencode",
		"XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state",
		"TMPDIR": "tmp", "TMP": "tmp", "TEMP": "tmp",
	} {
		path := filepath.Join(root, directory)
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal("create isolated collector directory")
		}
		environment = append(environment, name+"="+path)
	}
	if err := os.MkdirAll(filepath.Join(root, "claude", "projects"), 0700); err != nil {
		t.Fatal("create isolated Claude projects directory")
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"SystemRoot", "WINDIR"} {
			if value := os.Getenv(name); value != "" {
				environment = append(environment, name+"="+value)
			}
		}
	}
	return environment
}

func runIntegrationCollector(t *testing.T, binary, directory string, environment []string, arguments ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, arguments...)
	command.Dir = directory
	command.Env = environment
	command.Stderr = io.Discard
	output, err := command.Output()
	if err != nil {
		t.Fatalf("isolated native command failed: %s", arguments[0])
	}
	return output
}
