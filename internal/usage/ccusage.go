// Package usage invokes ccusage and normalizes its focused daily JSON reports.
//
// The collector is deliberately independent of the CPA request path: it reads
// local harness history only when the management API asks for a snapshot.
package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// DefaultCommandTimeout bounds one source scan. Sources run in parallel.
	DefaultCommandTimeout = 45 * time.Second
	// DefaultMaxOutputBytes bounds the JSON read from one source.
	DefaultMaxOutputBytes = 16 << 20
	// DefaultConcurrency bounds parallel collector processes so a full scan
	// stays cheap on small machines instead of starting one per source.
	DefaultConcurrency = 4
	maxDailyRows       = 100_000
)

// KnownSources lists every ccusage agent covered by the pinned collector,
// plus omp (oh-my-pi), which is read through ccusage's pi adapter.
var KnownSources = []string{
	"claude", "codex", "opencode", "omp", "pi", "amp", "droid", "codebuff",
	"hermes", "goose", "kilo", "copilot", "gemini", "antigravity", "kimi",
	"qwen", "openclaw", "grok", "zcode",
}

// SupportedSource reports whether source is a known harness.
func SupportedSource(source string) bool {
	for _, known := range KnownSources {
		if source == known {
			return true
		}
	}
	return false
}

// Source status values. Empty is a successful scan that found no rows, which
// is different from a failed scan or an unsupported upstream schema.
const (
	StatusOK    = "ok"
	StatusEmpty = "empty"
	StatusError = "error"
)

// Privacy-safe error codes returned in SourceReport.Error.
const (
	ErrorTimeout        = "timeout"
	ErrorCanceled       = "canceled"
	ErrorCommandFailed  = "command_failed"
	ErrorOutputTooLarge = "output_too_large"
	ErrorUnknownSchema  = "unknown_schema"
	ErrorUnsupported    = "unsupported_source"
)

// Config selects the ccusage executable and focused agent reports to collect.
type Config struct {
	Binary  string
	Sources []string
	// OMPPath overrides the oh-my-pi sessions directory. Empty uses
	// PI_CODING_AGENT_DIR/sessions, then ~/.omp/agent/sessions.
	OMPPath string

	// Timeout, MaxOutputBytes, and Concurrency are optional overrides used by
	// tests and embedders.
	Timeout        time.Duration
	MaxOutputBytes int
	Concurrency    int
}

// Collector invokes ccusage and converts its output to a privacy-safe report.
type Collector struct {
	binary      string
	sources     []string
	ompPath     string
	timeout     time.Duration
	maxOutput   int
	concurrency int
}

// Report contains one independently successful, empty, or failed result per
// source. GeneratedAt is the UTC completion time of the scan.
type Report struct {
	SchemaVersion int            `json:"schemaVersion"`
	GeneratedAt   string         `json:"generatedAt"`
	Sources       []SourceReport `json:"sources"`
}

// SourceReport deliberately excludes source paths, session IDs, project names,
// prompts, and credentials. Model names are included because the dashboard
// classifies usage by model; they never appear in URLs or logs.
type SourceReport struct {
	Source string       `json:"source"`
	Status string       `json:"status"`
	Error  string       `json:"error,omitempty"`
	Daily  []DailyUsage `json:"daily"`
}

// DailyUsage contains only allowlisted aggregate counters. TotalTokens is the
// value reported by ccusage and is never recomputed from the components:
// sources differ in whether reasoning or cache tokens are already included.
// Models carries the same day's per-model split when the collector reports it.
type DailyUsage struct {
	Date                string       `json:"date"`
	InputTokens         uint64       `json:"inputTokens"`
	OutputTokens        uint64       `json:"outputTokens"`
	CacheCreationTokens uint64       `json:"cacheCreationTokens"`
	CacheReadTokens     uint64       `json:"cacheReadTokens"`
	ReasoningTokens     uint64       `json:"reasoningOutputTokens"`
	TotalTokens         uint64       `json:"totalTokens"`
	CostUSD             float64      `json:"costUSD"`
	Models              []ModelUsage `json:"models"`
}

// ModelUsage is one model's contribution to a day. Model totals are reported
// by ccusage for Codex; the other adapters omit them, so the non-overlapping
// components are summed there. CostUSD is absent when the source does not
// report per-model cost (Codex), and is never estimated by this plugin.
type ModelUsage struct {
	Model               string   `json:"model"`
	InputTokens         uint64   `json:"inputTokens"`
	OutputTokens        uint64   `json:"outputTokens"`
	CacheCreationTokens uint64   `json:"cacheCreationTokens"`
	CacheReadTokens     uint64   `json:"cacheReadTokens"`
	ReasoningTokens     uint64   `json:"reasoningOutputTokens"`
	TotalTokens         uint64   `json:"totalTokens"`
	CostUSD             *float64 `json:"costUSD,omitempty"`
}

// New creates a collector. An empty binary uses the bundled collector.
func New(cfg Config) *Collector {
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = DefaultBinary
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultCommandTimeout
	}
	maxOutput := cfg.MaxOutputBytes
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutputBytes
	}
	ompPath := strings.TrimSpace(cfg.OMPPath)
	if ompPath == "" {
		ompPath = defaultOMPSessionsPath()
	} else {
		ompPath = absolutePath(expandHome(ompPath))
	}
	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	return &Collector{
		binary:      binary,
		sources:     append([]string(nil), cfg.Sources...),
		ompPath:     ompPath,
		timeout:     timeout,
		maxOutput:   maxOutput,
		concurrency: concurrency,
	}
}

// defaultOMPSessionsPath follows oh-my-pi's own data location. The explicit
// --pi-path is needed because ccusage only auto-detects ~/.pi by default.
func defaultOMPSessionsPath() string {
	if directory := strings.TrimSpace(os.Getenv("PI_CODING_AGENT_DIR")); directory != "" {
		return absolutePath(filepath.Join(expandHome(directory), "sessions"))
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".omp", "agent", "sessions")
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") && !strings.HasPrefix(path, `~\`) {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

func absolutePath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

// Snapshot collects every configured source in parallel. A failure for one
// source never prevents reports from the remaining sources. Results keep the
// configured source order.
func (c *Collector) Snapshot(ctx context.Context) Report {
	sources := make([]SourceReport, len(c.sources))
	var wg sync.WaitGroup
	limit := c.concurrency
	if limit <= 0 || limit > len(c.sources) {
		limit = len(c.sources)
	}
	slots := make(chan struct{}, limit)
	for index, source := range c.sources {
		wg.Add(1)
		go func(index int, source string) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				// Still produce a classified row so the report shape stays
				// stable when the caller cancels mid-scan.
				sources[index] = SourceReport{Source: safeSource(source), Status: StatusError, Error: ErrorCanceled, Daily: []DailyUsage{}}
				return
			}
			defer func() { <-slots }()
			sources[index] = c.snapshotOne(ctx, source)
		}(index, source)
	}
	wg.Wait()
	return Report{
		SchemaVersion: 1,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Sources:       sources,
	}
}

func (c *Collector) snapshotOne(ctx context.Context, source string) SourceReport {
	item := SourceReport{Source: safeSource(source), Status: StatusError, Daily: []DailyUsage{}}
	if !supportedSource(source) {
		item.Error = ErrorUnsupported
		return item
	}
	rows, errorCode := c.collectOne(ctx, source)
	if errorCode != "" {
		item.Error = errorCode
		return item
	}
	if len(rows) == 0 {
		item.Status = StatusEmpty
	} else {
		item.Status = StatusOK
		item.Daily = rows
	}
	return item
}

func safeSource(source string) string {
	if supportedSource(source) {
		return source
	}
	return "unknown"
}

func supportedSource(source string) bool {
	return SupportedSource(source)
}

func (c *Collector) collectOne(ctx context.Context, source string) ([]DailyUsage, string) {
	commandCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	binary := c.binary
	if binary == DefaultBinary {
		var err error
		binary, err = bundledBinary()
		if err != nil {
			return nil, ErrorCommandFailed
		}
	}
	args := []string{source, "daily", "--json", "--offline"}
	if source == "omp" {
		if c.ompPath == "" {
			return nil, ErrorCommandFailed
		}
		// oh-my-pi writes pi-format sessions. ccusage exposes those through
		// its pi adapter, but only auto-detects ~/.pi, so pass the store path.
		args = []string{"pi", "daily", "--json", "--offline", "--pi-path", c.ompPath}
	}
	cmd := exec.CommandContext(commandCtx, binary, args...)
	var stdout cappedBuffer
	stdout.limit = c.maxOutput
	cmd.Stdout = &stdout
	// Never retain or surface upstream stderr: it can contain local paths.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second

	stdout.onLimit = cancel
	errRun := cmd.Run()
	if errors.Is(stdout.err, errOutputTooLarge) {
		return nil, ErrorOutputTooLarge
	}
	if errRun != nil {
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return nil, ErrorTimeout
		}
		if errors.Is(commandCtx.Err(), context.Canceled) {
			return nil, ErrorCanceled
		}
		return nil, ErrorCommandFailed
	}

	rows, err := parseSourceDaily(stdout.Bytes(), source)
	if err != nil {
		if errors.Is(err, errTooManyRows) {
			return nil, ErrorOutputTooLarge
		}
		return nil, ErrorUnknownSchema
	}
	return rows, ""
}

// parseSourceDaily applies source-specific normalization on top of the shared
// daily contract.
func parseSourceDaily(data []byte, source string) ([]DailyUsage, error) {
	rows, err := parseDaily(data)
	if err != nil {
		return nil, err
	}
	if source == "omp" {
		normalizeOMPModels(rows)
	}
	return rows, nil
}

// The pi adapter tags models with its adapter name. oh-my-pi is reported
// through that adapter, so drop the implementation tag from the public name.
func normalizeOMPModels(rows []DailyUsage) {
	for rowIndex := range rows {
		for modelIndex := range rows[rowIndex].Models {
			name := rows[rowIndex].Models[modelIndex].Model
			name = strings.TrimPrefix(name, "[pi] ")
			name = strings.TrimPrefix(name, "[omp] ")
			rows[rowIndex].Models[modelIndex].Model = name
		}
	}
}

var (
	errOutputTooLarge = errors.New("output limit reached")
	errTooManyRows    = errors.New("too many daily rows")
)

type cappedBuffer struct {
	buf     bytes.Buffer
	limit   int
	onLimit context.CancelFunc
	err     error
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 || len(p) > remaining {
		if remaining > 0 {
			_, _ = b.buf.Write(p[:remaining])
		}
		if b.err == nil {
			b.err = errOutputTooLarge
			if b.onLimit != nil {
				b.onLimit()
			}
		}
		return 0, errOutputTooLarge
	}
	return b.buf.Write(p)
}

// ReadFrom keeps os/exec from taking bytes.Buffer's promoted fast path, which
// would bypass the output limit.
func (b *cappedBuffer) ReadFrom(r io.Reader) (int64, error) {
	var total int64
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			written, writeErr := b.Write(chunk[:n])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func (b *cappedBuffer) Bytes() []byte { return b.buf.Bytes() }

func (b *cappedBuffer) String() string { return b.buf.String() }

type rawReport struct {
	Daily json.RawMessage `json:"daily"`
}

type rawDaily struct {
	Date                string              `json:"date"`
	InputTokens         json.RawMessage     `json:"inputTokens"`
	OutputTokens        json.RawMessage     `json:"outputTokens"`
	CacheCreationTokens json.RawMessage     `json:"cacheCreationTokens"`
	CacheReadTokens     json.RawMessage     `json:"cacheReadTokens"`
	ReasoningTokens     json.RawMessage     `json:"reasoningOutputTokens"`
	TotalTokens         json.RawMessage     `json:"totalTokens"`
	CostUSD             json.RawMessage     `json:"costUSD"`
	TotalCost           json.RawMessage     `json:"totalCost"`
	ModelBreakdowns     []rawModel          `json:"modelBreakdowns"`
	Models              map[string]rawModel `json:"models"`
}

type rawModel struct {
	ModelName           string          `json:"modelName"`
	InputTokens         json.RawMessage `json:"inputTokens"`
	OutputTokens        json.RawMessage `json:"outputTokens"`
	CacheCreationTokens json.RawMessage `json:"cacheCreationTokens"`
	CacheReadTokens     json.RawMessage `json:"cacheReadTokens"`
	ReasoningTokens     json.RawMessage `json:"reasoningOutputTokens"`
	TotalTokens         json.RawMessage `json:"totalTokens"`
	Cost                json.RawMessage `json:"cost"`
	StoredCost          json.RawMessage `json:"storedCost"`
}

// parseDaily validates the exact shapes emitted by ccusage 20.0.36 focused
// daily commands. Claude uses totalCost; Codex and OpenCode use costUSD for
// Codex and totalCost for OpenCode. All four token components and the reported
// total are required. Unknown shapes are rejected rather than guessed, so an
// empty report and an incompatible schema stay distinguishable.
func parseDaily(data []byte) ([]DailyUsage, error) {
	var report rawReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, err
	}
	if len(report.Daily) == 0 || string(report.Daily) == "null" {
		return nil, errors.New("missing daily rows")
	}
	var rawRows []rawDaily
	if err := json.Unmarshal(report.Daily, &rawRows); err != nil || rawRows == nil {
		return nil, errors.New("invalid daily rows")
	}
	if len(rawRows) > maxDailyRows {
		return nil, errTooManyRows
	}

	rows := make([]DailyUsage, 0, len(rawRows))
	for _, raw := range rawRows {
		if _, err := time.Parse("2006-01-02", raw.Date); err != nil {
			return nil, errors.New("invalid date")
		}
		row := DailyUsage{Date: raw.Date}
		var err error
		if row.InputTokens, err = requiredNumber(raw.InputTokens); err != nil {
			return nil, err
		}
		if row.OutputTokens, err = requiredNumber(raw.OutputTokens); err != nil {
			return nil, err
		}
		if row.CacheCreationTokens, err = requiredNumber(raw.CacheCreationTokens); err != nil {
			return nil, err
		}
		if row.CacheReadTokens, err = requiredNumber(raw.CacheReadTokens); err != nil {
			return nil, err
		}
		if row.ReasoningTokens, err = optionalNumber(raw.ReasoningTokens); err != nil {
			return nil, err
		}
		if row.TotalTokens, err = requiredNumber(raw.TotalTokens); err != nil {
			return nil, err
		}
		cost := raw.TotalCost
		if len(cost) == 0 {
			cost = raw.CostUSD
		}
		if row.CostUSD, err = requiredDecimal(cost); err != nil {
			return nil, err
		}
		if err := validateTokenTotal(row); err != nil {
			return nil, err
		}
		if row.Models, err = parseModels(raw); err != nil {
			return nil, err
		}
		if err := validateModelTotals(row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// parseModels accepts the two shapes emitted by the pinned collector:
// modelBreakdowns (Claude, OpenCode, pi/omp) and models (Codex). Missing model
// data is valid; a malformed present breakdown is a schema change.
func parseModels(raw rawDaily) ([]ModelUsage, error) {
	models := make([]ModelUsage, 0)
	if len(raw.ModelBreakdowns) > 0 {
		for _, item := range raw.ModelBreakdowns {
			parsed, ok, err := parseModel(item, item.ModelName)
			if err != nil {
				return nil, err
			}
			if ok {
				models = append(models, parsed)
			}
		}
	} else {
		for name, item := range raw.Models {
			parsed, ok, err := parseModel(item, name)
			if err != nil {
				return nil, err
			}
			if ok {
				models = append(models, parsed)
			}
		}
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].TotalTokens != models[j].TotalTokens {
			return models[i].TotalTokens > models[j].TotalTokens
		}
		return models[i].Model < models[j].Model
	})
	return models, nil
}

func parseModel(raw rawModel, name string) (ModelUsage, bool, error) {
	name = sanitizeModelName(name)
	if name == "" {
		return ModelUsage{}, false, errors.New("missing model name")
	}
	if !hasCounters(raw) {
		// Some adapters emit a name-only breakdown. It carries no usage, so
		// the day's total stays authoritative and the model is unattributed.
		return ModelUsage{}, false, nil
	}
	model := ModelUsage{Model: name}
	var err error
	if model.InputTokens, err = requiredNumber(raw.InputTokens); err != nil {
		return ModelUsage{}, false, err
	}
	if model.OutputTokens, err = requiredNumber(raw.OutputTokens); err != nil {
		return ModelUsage{}, false, err
	}
	if model.CacheCreationTokens, err = requiredNumber(raw.CacheCreationTokens); err != nil {
		return ModelUsage{}, false, err
	}
	if model.CacheReadTokens, err = requiredNumber(raw.CacheReadTokens); err != nil {
		return ModelUsage{}, false, err
	}
	if model.ReasoningTokens, err = optionalNumber(raw.ReasoningTokens); err != nil {
		return ModelUsage{}, false, err
	}
	known, err := sumComponents(model)
	if err != nil {
		return ModelUsage{}, false, err
	}
	if len(raw.TotalTokens) > 0 && string(raw.TotalTokens) != "null" {
		if model.TotalTokens, err = requiredNumber(raw.TotalTokens); err != nil {
			return ModelUsage{}, false, err
		}
		if model.TotalTokens < known {
			return ModelUsage{}, false, errors.New("inconsistent model total tokens")
		}
	} else {
		model.TotalTokens = known
	}
	cost := raw.Cost
	if len(cost) == 0 || string(cost) == "null" {
		cost = raw.StoredCost
	}
	if len(cost) > 0 && string(cost) != "null" {
		value, err := requiredDecimal(cost)
		if err != nil {
			return ModelUsage{}, false, err
		}
		model.CostUSD = &value
	}
	return model, true, nil
}

func hasCounters(raw rawModel) bool {
	for _, value := range []json.RawMessage{raw.InputTokens, raw.OutputTokens, raw.CacheCreationTokens, raw.CacheReadTokens} {
		if len(value) > 0 && string(value) != "null" {
			return true
		}
	}
	return false
}

func sumComponents(model ModelUsage) (uint64, error) {
	total := model.InputTokens
	for _, value := range []uint64{model.OutputTokens, model.CacheCreationTokens, model.CacheReadTokens} {
		if total > math.MaxUint64-value {
			return 0, errors.New("token counter overflow")
		}
		total += value
	}
	return total, nil
}

func validateModelTotals(row DailyUsage) error {
	var total uint64
	for _, model := range row.Models {
		if total > math.MaxUint64-model.TotalTokens {
			return errors.New("token counter overflow")
		}
		total += model.TotalTokens
	}
	// A source may leave usage unattributed (OpenCode folds reasoning into the
	// reported total), but model rows can never exceed the authoritative total.
	if total > row.TotalTokens {
		return errors.New("model totals exceed daily total")
	}
	return nil
}

func sanitizeModelName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if utf8.RuneCountInString(name) > 120 {
		name = string([]rune(name)[:120])
	}
	return name
}

func validateTokenTotal(row DailyUsage) error {
	known := row.InputTokens + row.OutputTokens
	if known < row.InputTokens {
		return errors.New("token counter overflow")
	}
	known += row.CacheCreationTokens
	if known < row.CacheCreationTokens {
		return errors.New("token counter overflow")
	}
	known += row.CacheReadTokens
	if known < row.CacheReadTokens {
		return errors.New("token counter overflow")
	}
	// The reported total always includes input, output, and cache counters.
	// Reasoning may be folded into output, into the total, or not counted at
	// all depending on the source, so it is never added here. A total below
	// the non-overlapping components means the schema changed.
	if row.TotalTokens < known {
		return errors.New("inconsistent total tokens")
	}
	return nil
}

func requiredNumber(raw json.RawMessage) (uint64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, errors.New("missing counter")
	}
	return number(raw)
}

func optionalNumber(raw json.RawMessage) (uint64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, nil
	}
	return number(raw)
}

func number(raw json.RawMessage) (uint64, error) {
	var n uint64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || f < 0 || math.Trunc(f) != f || f >= math.Exp2(64) {
		return 0, errors.New("invalid counter")
	}
	return uint64(f), nil
}

func requiredDecimal(raw json.RawMessage) (float64, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return 0, errors.New("missing cost")
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, errors.New("invalid cost")
	}
	return n, nil
}
