package usage

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

var fakeCCUsage string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "harness-usage-fake")
	if err != nil {
		panic(err)
	}
	name := "fakeccusage"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	fakeCCUsage = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", fakeCCUsage, "./testdata/fakeccusage")
	if output, err := build.CombinedOutput(); err != nil {
		_ = os.RemoveAll(dir)
		panic("build fake ccusage: " + err.Error() + "\n" + string(output))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestSnapshotReportsSourcesIndependently(t *testing.T) {
	setBody(t, "CLAUDE", `{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":2,"cacheCreationTokens":3,"cacheReadTokens":4,"totalTokens":10,"totalCost":0.5}],"totals":{}}`)
	setBody(t, "CODEX", `{"daily":[],"totals":{}}`)
	t.Setenv("FAKE_OPENCODE_MODE", "fail")

	report := New(Config{Binary: fakeCCUsage, Sources: []string{"claude", "codex", "opencode"}}).Snapshot(context.Background())
	if report.SchemaVersion != 1 || report.GeneratedAt == "" {
		t.Fatalf("invalid report metadata: %+v", report)
	}
	if len(report.Sources) != 3 {
		t.Fatalf("sources = %d", len(report.Sources))
	}
	if got := report.Sources[0]; got.Source != "claude" || got.Status != StatusOK || len(got.Daily) != 1 {
		t.Fatalf("claude = %+v", got)
	}
	if got := report.Sources[1]; got.Source != "codex" || got.Status != StatusEmpty || len(got.Daily) != 0 {
		t.Fatalf("codex = %+v", got)
	}
	if got := report.Sources[2]; got.Source != "opencode" || got.Status != StatusError || got.Error != ErrorCommandFailed {
		t.Fatalf("opencode = %+v", got)
	}
	out, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"synthetic diagnostic", "/example/", "private-model", "session-id"} {
		if strings.Contains(string(out), private) {
			t.Fatalf("report leaked %q: %s", private, out)
		}
	}
}

func TestSnapshotPreservesConfiguredOrder(t *testing.T) {
	for _, source := range []string{"claude", "codex", "opencode"} {
		t.Setenv("FAKE_"+strings.ToUpper(source)+"_BODY", `{"daily":[],"totals":{}}`)
	}
	report := New(Config{Binary: fakeCCUsage, Sources: []string{"opencode", "claude", "codex"}}).Snapshot(context.Background())
	want := []string{"opencode", "claude", "codex"}
	for index, source := range want {
		if report.Sources[index].Source != source {
			t.Fatalf("source %d = %q, want %q", index, report.Sources[index].Source, source)
		}
	}
}

func TestSnapshotClassifiesFailures(t *testing.T) {
	tests := []struct {
		name   string
		mode   string
		body   string
		limit  int
		cancel bool
		want   string
	}{
		{name: "timeout", mode: "sleep", want: ErrorTimeout},
		{name: "unknown schema", mode: "unknown", want: ErrorUnknownSchema},
		{name: "invalid json", mode: "badjson", want: ErrorUnknownSchema},
		{name: "output limit", mode: "large", limit: 16, want: ErrorOutputTooLarge},
		{name: "canceled", cancel: true, want: ErrorCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FAKE_CODEX_MODE", tt.mode)
			t.Setenv("FAKE_CODEX_SLEEP", "2s")
			t.Setenv("FAKE_CODEX_BODY", tt.body)
			ctx, cancel := context.WithCancel(context.Background())
			if tt.cancel {
				cancel()
			} else {
				defer cancel()
			}
			collector := New(Config{Binary: fakeCCUsage, Sources: []string{"codex"}, Timeout: 40 * time.Millisecond, MaxOutputBytes: tt.limit})
			report := collector.Snapshot(ctx)
			if got := report.Sources[0]; got.Status != StatusError || got.Error != tt.want {
				t.Fatalf("source = %+v, want %s", got, tt.want)
			}
			if report.Sources[0].Daily != nil && len(report.Sources[0].Daily) != 0 {
				t.Fatal("failed source retained rows")
			}
		})
	}
}

func TestParseActualFocusedSchemas(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    DailyUsage
		reason  uint64
		comment string
	}{
		{
			name: "claude",
			body: `{"daily":[{"cacheCreationTokens":5,"cacheReadTokens":3,"date":"2026-10-19","inputTokens":11,"modelBreakdowns":[{"cacheCreationTokens":5,"cacheReadTokens":3,"cost":0.11,"inputTokens":11,"modelName":"claude-sonnet-4-20250514","outputTokens":7,"storedCost":0.11}],"modelsUsed":["claude-sonnet-4-20250514"],"outputTokens":7,"totalCost":0.11,"totalTokens":26}],"totals":{}}`,
			want: DailyUsage{Date: "2026-10-19", InputTokens: 11, OutputTokens: 7, CacheCreationTokens: 5, CacheReadTokens: 3, TotalTokens: 26, CostUSD: 0.11,
				Models: []ModelUsage{{Model: "claude-sonnet-4-20250514", InputTokens: 11, OutputTokens: 7, CacheCreationTokens: 5, CacheReadTokens: 3, TotalTokens: 26, CostUSD: floatPtr(0.11)}}},
		},
		{
			name: "codex",
			body: `{"daily":[{"cacheCreationTokens":0,"cacheReadTokens":10,"costUSD":0.00061375,"date":"2026-10-19","inputTokens":90,"models":{"gpt-5":{"cacheCreationTokens":0,"cacheReadTokens":10,"inputTokens":90,"outputTokens":50,"reasoningOutputTokens":5,"totalTokens":155}},"outputTokens":50,"reasoningOutputTokens":5,"totalTokens":155}],"totals":{}}`,
			want: DailyUsage{Date: "2026-10-19", InputTokens: 90, OutputTokens: 50, CacheCreationTokens: 0, CacheReadTokens: 10, ReasoningTokens: 5, TotalTokens: 155, CostUSD: 0.00061375,
				Models: []ModelUsage{{Model: "gpt-5", InputTokens: 90, OutputTokens: 50, CacheReadTokens: 10, ReasoningTokens: 5, TotalTokens: 155}}},
			reason: 5,
		},
		{
			name: "opencode",
			body: `{"daily":[{"cacheCreationTokens":6,"cacheReadTokens":4,"date":"2026-10-19","inputTokens":20,"modelBreakdowns":[{"cacheCreationTokens":6,"cacheReadTokens":4,"cost":0.07,"inputTokens":20,"modelName":"claude-sonnet-4-20250514","outputTokens":9,"storedCost":0.07}],"outputTokens":9,"totalCost":0.07,"totalTokens":41}],"totals":{}}`,
			want: DailyUsage{Date: "2026-10-19", InputTokens: 20, OutputTokens: 9, CacheCreationTokens: 6, CacheReadTokens: 4, TotalTokens: 41, CostUSD: 0.07,
				Models: []ModelUsage{{Model: "claude-sonnet-4-20250514", InputTokens: 20, OutputTokens: 9, CacheCreationTokens: 6, CacheReadTokens: 4, TotalTokens: 39, CostUSD: floatPtr(0.07)}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, err := parseDaily([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows = %d", len(rows))
			}
			if got := rows[0]; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("row = %+v, want %+v", got, tt.want)
			}
			if rows[0].ReasoningTokens != tt.reason {
				t.Fatalf("reasoning = %d, want %d", rows[0].ReasoningTokens, tt.reason)
			}
		})
	}
}

func TestParseRejectsUnknownAndInconsistentSchemas(t *testing.T) {
	inputs := []string{
		`not json`,
		`{"daily":null}`,
		`{"daily":{}}`,
		`{"data":[]}`,
		`{"daily":[{"date":"private-path","inputTokens":1,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":3,"totalCost":0}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":-1,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":3,"totalCost":0}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":3,"totalCost":"0"}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":3}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":2,"totalCost":0}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":1}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":1.5,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":3,"totalCost":0}]}`,
	}
	for _, input := range inputs {
		if _, err := parseDaily([]byte(input)); err == nil {
			t.Errorf("expected schema rejection for %s", input)
		}
	}
}

func TestParseAllowsReasoningOutsideReportedTotal(t *testing.T) {
	// OpenCode records reasoning but may not include it in the reported total.
	rows, err := parseDaily([]byte(`{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"reasoningOutputTokens":5,"totalTokens":3,"totalCost":0.01}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ReasoningTokens != 5 || rows[0].TotalTokens != 3 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestParseEmptyIsSuccessful(t *testing.T) {
	for _, input := range []string{
		`{"daily":[],"totals":{}}`,
		`{"daily":[],"totals":{"inputTokens":0}}`,
	} {
		rows, err := parseDaily([]byte(input))
		if err != nil || len(rows) != 0 {
			t.Fatalf("empty logs: %v %v", rows, err)
		}
	}
}

func TestParseDropsPrivateFields(t *testing.T) {
	rows, err := parseDaily([]byte(`{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":2,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":3,"totalCost":0.1,"project":"private-project","session":"private-session","modelsUsed":["private-model"],"projectPath":"/Users/example/private"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-", "/Users/example"} {
		if strings.Contains(string(out), private) {
			t.Fatalf("private field escaped the allowlist: %s", out)
		}
	}
}

func TestCappedBufferEnforcesLimit(t *testing.T) {
	var b cappedBuffer
	b.limit = 4
	if n, err := b.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("write at limit: %d %v", n, err)
	}
	if _, err := b.Write([]byte("5")); err != errOutputTooLarge {
		t.Fatalf("expected cap error, got %v", err)
	}
	if b.String() != "1234" {
		t.Fatalf("unexpected buffered content: %q", b.String())
	}
}

func TestUnsupportedSourceDoesNotRun(t *testing.T) {
	t.Setenv("FAKE_CLAUDE_MODE", "fail")
	report := New(Config{Binary: fakeCCUsage, Sources: []string{"unsupported"}}).Snapshot(context.Background())
	if got := report.Sources[0]; got.Source != "unknown" || got.Status != StatusError || got.Error != ErrorUnsupported {
		t.Fatalf("source = %+v", got)
	}
}

func setBody(t *testing.T, source, body string) {
	t.Helper()
	t.Setenv("FAKE_"+source+"_MODE", "ok")
	t.Setenv("FAKE_"+source+"_BODY", body)
}

func floatPtr(value float64) *float64 { return &value }

func TestOMPUsesPiAdapterWithSessionsPath(t *testing.T) {
	store := t.TempDir()
	body := `{"daily":[{"cacheCreationTokens":0,"cacheReadTokens":10,"date":"2026-10-19","inputTokens":20,"modelBreakdowns":[{"cacheCreationTokens":0,"cacheReadTokens":10,"cost":0.5,"inputTokens":20,"modelName":"[pi] synthetic-model","outputTokens":30,"storedCost":0.5}],"outputTokens":30,"totalCost":0.5,"totalTokens":60}],"totals":{}}`
	t.Setenv("FAKE_PI_MODE", "path")
	t.Setenv("FAKE_PI_PI_PATH", store)
	t.Setenv("FAKE_PI_BODY", body)

	report := New(Config{Binary: fakeCCUsage, Sources: []string{"omp"}, OMPPath: store}).Snapshot(context.Background())
	item := report.Sources[0]
	if item.Source != "omp" || item.Status != StatusOK || len(item.Daily) != 1 {
		t.Fatalf("omp source = %+v", item)
	}
	models := item.Daily[0].Models
	if len(models) != 1 || models[0].Model != "synthetic-model" {
		t.Fatalf("model names were not normalized: %+v", models)
	}
}

func TestOMPReportsMissingStoreSafely(t *testing.T) {
	collector := New(Config{Binary: fakeCCUsage, Sources: []string{"omp"}, OMPPath: " "})
	collector.ompPath = ""
	report := collector.Snapshot(context.Background())
	if item := report.Sources[0]; item.Status != StatusError || item.Error != ErrorCommandFailed {
		t.Fatalf("source = %+v", item)
	}
}

func TestOMPDefaultSessionsPath(t *testing.T) {
	home := t.TempDir()
	// os.UserHomeDir reads USERPROFILE on Windows and HOME elsewhere.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if got, want := defaultOMPSessionsPath(), filepath.Join(home, ".omp", "agent", "sessions"); got != want {
		t.Fatalf("default path = %q, want %q", got, want)
	}

	custom := filepath.Join(home, "custom-agent")
	t.Setenv("PI_CODING_AGENT_DIR", custom)
	if got, want := defaultOMPSessionsPath(), filepath.Join(custom, "sessions"); got != want {
		t.Fatalf("configured path = %q, want %q", got, want)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "")
	cfg := New(Config{Sources: []string{"omp"}, OMPPath: "~/synthetic-sessions"}).ompPath
	if cfg != filepath.Join(home, "synthetic-sessions") {
		t.Fatalf("expanded path = %q", cfg)
	}
}

func TestParseModelsFromBothShapes(t *testing.T) {
	body := `{"daily":[{"date":"2026-10-19","inputTokens":10,"outputTokens":5,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":40,"totalCost":0.2,"modelBreakdowns":[
		{"modelName":"small","inputTokens":1,"outputTokens":1,"cacheCreationTokens":0,"cacheReadTokens":0,"cost":0.01},
		{"modelName":"large","inputTokens":9,"outputTokens":4,"cacheCreationTokens":0,"cacheReadTokens":0,"cost":0.19},
		{"modelName":"name-only"}
	]}]}`
	rows, err := parseDaily([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	models := rows[0].Models
	if len(models) != 2 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].Model != "large" || models[1].Model != "small" {
		t.Fatalf("models are not sorted by tokens: %+v", models)
	}
	// Unattributed usage stays with the day total and is never invented.
	if models[0].TotalTokens != 13 || models[1].TotalTokens != 2 {
		t.Fatalf("model totals = %+v", models)
	}
}

func TestParseRejectsMalformedModels(t *testing.T) {
	for _, input := range []string{
		`{"daily":[{"date":"2026-10-19","inputTokens":0,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":10,"totalCost":0,"modelBreakdowns":[{"inputTokens":1,"outputTokens":1,"cacheCreationTokens":0,"cacheReadTokens":0}]}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":0,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":10,"totalCost":0,"modelBreakdowns":[{"modelName":"x","inputTokens":-1,"outputTokens":1,"cacheCreationTokens":0,"cacheReadTokens":0}]}]}`,
		`{"daily":[{"date":"2026-10-19","inputTokens":0,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":10,"totalCost":0,"models":{"x":{"inputTokens":20,"outputTokens":0,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":20}}}]}`,
	} {
		if _, err := parseDaily([]byte(input)); err == nil {
			t.Fatalf("accepted malformed model breakdown: %s", input)
		}
	}
}

func TestParseSanitizesModelNames(t *testing.T) {
	rows, err := parseDaily([]byte(`{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":1,"cacheCreationTokens":0,"cacheReadTokens":0,"totalTokens":2,"totalCost":0,"modelBreakdowns":[{"modelName":"bad\u0000name\n","inputTokens":1,"outputTokens":1,"cacheCreationTokens":0,"cacheReadTokens":0}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := rows[0].Models[0].Model; got != "badname" {
		t.Fatalf("model name = %q", got)
	}
}
