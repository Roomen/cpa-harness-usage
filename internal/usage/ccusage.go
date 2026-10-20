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
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultCommandTimeout bounds one source scan. Sources run in parallel.
	DefaultCommandTimeout = 45 * time.Second
	// DefaultMaxOutputBytes bounds the JSON read from one source.
	DefaultMaxOutputBytes = 16 << 20
	maxDailyRows          = 100_000
)

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

	// Timeout and MaxOutputBytes are optional overrides used by tests.
	Timeout        time.Duration
	MaxOutputBytes int
}

// Collector invokes ccusage and converts its output to a privacy-safe report.
type Collector struct {
	binary    string
	sources   []string
	timeout   time.Duration
	maxOutput int
}

// Report contains one independently successful, empty, or failed result per
// source. GeneratedAt is the UTC completion time of the scan.
type Report struct {
	SchemaVersion int            `json:"schemaVersion"`
	GeneratedAt   string         `json:"generatedAt"`
	Sources       []SourceReport `json:"sources"`
}

// SourceReport deliberately excludes source paths, model names, session IDs,
// project names, prompts, and credentials.
type SourceReport struct {
	Source string       `json:"source"`
	Status string       `json:"status"`
	Error  string       `json:"error,omitempty"`
	Daily  []DailyUsage `json:"daily"`
}

// DailyUsage contains only allowlisted aggregate counters. TotalTokens is the
// value reported by ccusage and is never recomputed from the components:
// sources differ in whether reasoning or cache tokens are already included.
type DailyUsage struct {
	Date                string  `json:"date"`
	InputTokens         uint64  `json:"inputTokens"`
	OutputTokens        uint64  `json:"outputTokens"`
	CacheCreationTokens uint64  `json:"cacheCreationTokens"`
	CacheReadTokens     uint64  `json:"cacheReadTokens"`
	ReasoningTokens     uint64  `json:"reasoningOutputTokens"`
	TotalTokens         uint64  `json:"totalTokens"`
	CostUSD             float64 `json:"costUSD"`
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
	return &Collector{
		binary:    binary,
		sources:   append([]string(nil), cfg.Sources...),
		timeout:   timeout,
		maxOutput: maxOutput,
	}
}

// Snapshot collects every configured source in parallel. A failure for one
// source never prevents reports from the remaining sources. Results keep the
// configured source order.
func (c *Collector) Snapshot(ctx context.Context) Report {
	sources := make([]SourceReport, len(c.sources))
	var wg sync.WaitGroup
	for index, source := range c.sources {
		wg.Add(1)
		go func(index int, source string) {
			defer wg.Done()
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
	switch source {
	case "claude", "codex", "opencode":
		return true
	default:
		return false
	}
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
	cmd := exec.CommandContext(commandCtx, binary, source, "daily", "--json", "--offline")
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

	rows, err := parseDaily(stdout.Bytes())
	if err != nil {
		if errors.Is(err, errTooManyRows) {
			return nil, ErrorOutputTooLarge
		}
		return nil, ErrorUnknownSchema
	}
	return rows, ""
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
	Date                string          `json:"date"`
	InputTokens         json.RawMessage `json:"inputTokens"`
	OutputTokens        json.RawMessage `json:"outputTokens"`
	CacheCreationTokens json.RawMessage `json:"cacheCreationTokens"`
	CacheReadTokens     json.RawMessage `json:"cacheReadTokens"`
	ReasoningTokens     json.RawMessage `json:"reasoningOutputTokens"`
	TotalTokens         json.RawMessage `json:"totalTokens"`
	CostUSD             json.RawMessage `json:"costUSD"`
	TotalCost           json.RawMessage `json:"totalCost"`
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
		rows = append(rows, row)
	}
	return rows, nil
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
