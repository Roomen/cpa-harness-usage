package usage

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

const testPrices = `{"_cloudprice":{"updated_at":"2026-10-19"},"sample_spec":{"input_cost_per_token":"description"},"gpt-5":{"input_cost_per_token":0.00000125,"output_cost_per_token":0.00001,"cache_read_input_token_cost":0.000000125},"free":{"input_cost_per_token":0,"output_cost_per_token":0,"cache_read_input_token_cost":0,"cache_creation_input_token_cost":0},"deepseek-v4.1-flash":{"input_cost_per_token":0.00000031,"output_cost_per_token":0.00000123},"deepseek/deepseek-v4.1-flash":{"input_cost_per_token":0.0000003,"output_cost_per_token":0.0000012}}`

func testPricingStore(t *testing.T) *PricingStore {
	t.Helper()
	s := NewPricingStore("")
	if err := s.Update([]byte(testPrices)); err != nil {
		t.Fatal(err)
	}
	return s
}
func pricingRows() []DailyUsage {
	return []DailyUsage{{TotalTokens: 430, InputTokens: 270, OutputTokens: 125, CacheReadTokens: 30, ReasoningTokens: 5, Models: []ModelUsage{{Model: "gpt-5", InputTokens: 270, OutputTokens: 125, CacheReadTokens: 30, ReasoningTokens: 5, TotalTokens: 430}}}}
}
func TestPricingPersistenceAndStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "prices.json")
	s := NewPricingStore(path)
	if status := s.Status(); status.Status != "empty" || status.ModelCount != 0 || status.Source != "CloudPrice" {
		t.Fatal(status)
	}
	if err := s.Update([]byte(testPrices)); err != nil {
		t.Fatal(err)
	}
	status := s.Status()
	if status.Status != "ready" || status.ModelCount != 4 || status.UpdatedAt == "" || status.UpstreamUpdatedAt != "2026-10-19" {
		t.Fatal(status)
	}
	restarted := NewPricingStore(path)
	if restarted.Status() != status {
		t.Fatalf("restart status=%+v want %+v", restarted.Status(), status)
	}
	if runtime.GOOS != "windows" {
		for _, check := range []struct {
			path string
			mode os.FileMode
		}{{path, 0600}, {filepath.Dir(path), 0700}} {
			info, err := os.Stat(check.path)
			if err != nil || info.Mode().Perm() != check.mode {
				t.Fatalf("cache permissions: %v %v", info, err)
			}
		}
	}
	rows := pricingRows()
	restarted.Apply(rows, "codex")
	if math.Abs(rows[0].CostUSD-0.00159125) > 1e-12 || rows[0].MissingPricing || !rows[0].CostEstimated {
		t.Fatal(rows)
	}
}
func TestPricingRejectsInvalidUpdatesAndKeepsOld(t *testing.T) {
	s := testPricingStore(t)
	status := s.Status()
	invalid := []string{`null`, `[]`, `{}`, `{"gpt-5":`, `{"x":null}`, `{"x":{"input_cost_per_token":1}}`, `{"x":{"input_cost_per_token":-1,"output_cost_per_token":1}}`, `{"x":{"input_cost_per_token":1e999,"output_cost_per_token":1}}`, `{"x":{"input_cost_per_token":null,"output_cost_per_token":1}}`, `{"x":{"input_cost_per_token":1,"output_cost_per_token":"1"}}`, `{"gpt-5":{"input_cost_per_token":1,"output_cost_per_token":2},"partial":{"cache_read_input_token_cost":-1}}`}
	for _, data := range invalid {
		if err := s.Update([]byte(data)); err == nil {
			t.Errorf("accepted %s", data)
		}
		if s.Status() != status {
			t.Fatal("failed update replaced snapshot")
		}
	}
	if err := s.Update(bytes.Repeat([]byte(" "), MaxPricingBytes+1)); err == nil {
		t.Fatal("accepted oversized update")
	}
	// A long key independently exercises the per-entry bound.
	data, _ := json.Marshal(map[string]any{strings.Repeat("x", maxPricingModelName+1): map[string]any{"input_cost_per_token": 1, "output_cost_per_token": 1}})
	if s.Update(data) == nil {
		t.Fatal("accepted oversized model name")
	}
}
func TestPricingModelCountLimit(t *testing.T) {
	var data bytes.Buffer
	data.WriteByte('{')
	for i := 0; i <= maxPricingModels; i++ {
		if i > 0 {
			data.WriteByte(',')
		}
		name, _ := json.Marshal(strings.Repeat("x", i/10000+1) + strings.ToUpper(string(rune(0x1000+i%10000))))
		data.Write(name)
		data.WriteString(`:{}`)
	}
	data.WriteByte('}')
	if NewPricingStore("").Update(data.Bytes()) == nil {
		t.Fatal("accepted too many entries")
	}
}
func TestPricingInvalidDiskAndFailedWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte(`{"models":`), 0600); err != nil {
		t.Fatal(err)
	}
	if NewPricingStore(path).Status().Status != "empty" {
		t.Fatal("invalid cache accepted")
	}
	s := NewPricingStore(path)
	if err := s.Update([]byte(testPrices)); err != nil {
		t.Fatal(err)
	}
	old := s.Status()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if s.Update([]byte(`{"new":{"input_cost_per_token":1,"output_cost_per_token":1}}`)) == nil {
		t.Fatal("expected persistence error")
	}
	if s.Status() != old {
		t.Fatal("failed save replaced snapshot")
	}
}
func TestCodexAlwaysRepricesAndUnknownWithoutCache(t *testing.T) {
	for _, store := range []*PricingStore{NewPricingStore(""), testPricingStore(t)} {
		rows := pricingRows()
		value := 99.0
		rows[0].CostUSD = value
		rows[0].Models[0].CostUSD = &value
		store.Apply(rows, "codex")
		if store.Status().Status == "empty" {
			if rows[0].CostUSD != 0 || !rows[0].MissingPricing || rows[0].Models[0].CostUSD != nil {
				t.Fatal(rows)
			}
		} else if math.Abs(rows[0].CostUSD-0.00159125) > 1e-12 {
			t.Fatal(rows)
		}
	}
}
func TestPricingFallbackAcrossSourcesAndPreservesReportedCosts(t *testing.T) {
	s := testPricingStore(t)
	for _, source := range KnownSources {
		rows := pricingRows()
		s.Apply(rows, source)
		if rows[0].MissingPricing || rows[0].Models[0].CostUSD == nil || !rows[0].CostEstimated {
			t.Fatalf("%s: %+v", source, rows)
		}
		if source == "codex" {
			continue
		}
		rows = pricingRows()
		reported := 0.42
		rows[0].CostUSD = .5
		rows[0].Models[0].CostUSD = &reported
		s.Apply(rows, source)
		if rows[0].CostUSD != .5 || *rows[0].Models[0].CostUSD != reported || rows[0].Models[0].CostEstimated {
			t.Fatalf("%s: %+v", source, rows)
		}
	}
}
func TestPricingFreeAndMissingCacheRatesAndAliases(t *testing.T) {
	s := testPricingStore(t)
	for _, tc := range []struct {
		name    string
		write   uint64
		missing bool
	}{{"free", 5, false}, {"gpt-5", 5, true}, {"unknown", 0, true}, {"provider/gpt-5", 0, true}, {"deepseek-4.1-flash", 0, false}, {"deepseek/deepseek-4.1-flash", 0, false}, {"provider/deepseek-4.1-flash", 0, true}} {
		rows := []DailyUsage{{TotalTokens: 10 + tc.write, Models: []ModelUsage{{Model: tc.name, InputTokens: 10, CacheCreationTokens: tc.write, TotalTokens: 10 + tc.write}}}}
		s.Apply(rows, "codex")
		if rows[0].MissingPricing != tc.missing || rows[0].Models[0].MissingPricing != tc.missing {
			t.Fatalf("%s: %+v", tc.name, rows)
		}
		if tc.name == "free" && (rows[0].CostUSD != 0 || rows[0].Models[0].CostUSD == nil) {
			t.Fatal(rows)
		}
	}
}
func TestPricingChangedRatesAffectSubsequentApply(t *testing.T) {
	for _, source := range KnownSources {
		t.Run(source, func(t *testing.T) {
			s := testPricingStore(t)
			rows := pricingRows()
			s.Apply(rows, source)
			before := rows[0].CostUSD
			if err := s.Update([]byte(strings.ReplaceAll(testPrices, "0.00000125", "0.0000025"))); err != nil {
				t.Fatal(err)
			}
			s.Apply(rows, source)
			if rows[0].CostUSD <= before {
				t.Fatal("retained previous estimated costs")
			}
		})
	}
}
func TestPricingConcurrentReadsUpdates(t *testing.T) {
	s := testPricingStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(update bool) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if update {
					if err := s.Update([]byte(testPrices)); err != nil {
						t.Error(err)
					}
				} else {
					rows := pricingRows()
					s.Apply(rows, "codex")
					if rows[0].MissingPricing || s.Status().Status != "ready" {
						t.Error("inconsistent snapshot")
					}
				}
			}
		}(i == 0)
	}
	wg.Wait()
}
func TestParseSourceDailyDoesNotApplyPrices(t *testing.T) {
	rows, err := parseSourceDaily([]byte(`{"daily":[{"date":"2026-10-19","inputTokens":1,"outputTokens":0,"cacheReadTokens":0,"cacheCreationTokens":0,"totalTokens":1,"costUSD":0,"models":{"gpt-5":{"inputTokens":1,"outputTokens":0,"cacheReadTokens":0,"cacheCreationTokens":0,"totalTokens":1}}}]}`), "codex")
	if err != nil || rows[0].Models[0].CostUSD != nil || rows[0].CostUSD != 0 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestStoredCostSeparatesRecordedAndNativeEstimatedCosts(t *testing.T) {
	for _, source := range []string{"claude", "opencode", "pi", "omp"} {
		t.Run(source, func(t *testing.T) {
			parse := func(stored string) []DailyUsage {
				t.Helper()
				rows, err := parseSourceDaily([]byte(`{"daily":[{"date":"2026-10-19","inputTokens":1000000,"outputTokens":0,"cacheReadTokens":0,"cacheCreationTokens":0,"totalTokens":1000000,"totalCost":1,"modelBreakdowns":[{"modelName":"gpt-5","inputTokens":1000000,"outputTokens":0,"cacheReadTokens":0,"cacheCreationTokens":0,"totalTokens":1000000,"cost":1,"storedCost":`+stored+`}]}]}`), source)
				if err != nil {
					t.Fatal(err)
				}
				return rows
			}
			s := testPricingStore(t)
			rows := parse("0")
			if !rows[0].CostEstimated || !rows[0].Models[0].CostEstimated {
				t.Fatal("native estimated provenance lost")
			}
			s.Apply(rows, source)
			if rows[0].CostUSD != 1.25 || !rows[0].CostEstimated {
				t.Fatal(rows)
			}
			if err := s.Update([]byte(strings.ReplaceAll(testPrices, "0.00000125", "0.0000025"))); err != nil {
				t.Fatal(err)
			}
			s.Apply(rows, source)
			if rows[0].CostUSD != 2.5 {
				t.Fatal(rows)
			}
			rows = parse("0.5")
			s.Apply(rows, source)
			if rows[0].CostUSD != 2.5 || !rows[0].CostEstimated || !rows[0].Models[0].CostEstimated {
				t.Fatal(rows)
			}
			rows = parse("1")
			s.Apply(rows, source)
			if rows[0].CostUSD != 1 || rows[0].Models[0].CostUSD == nil || *rows[0].Models[0].CostUSD != 1 || rows[0].CostEstimated || rows[0].Models[0].CostEstimated {
				t.Fatal(rows)
			}
			rows = parse("0")
			NewPricingStore("").Apply(rows, source)
			if rows[0].CostUSD != 0 || !rows[0].MissingPricing || rows[0].Models[0].CostUSD != nil {
				t.Fatal(rows)
			}
		})
	}
}

func TestStoredOnlyModelCostAndDailyReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name, costs string
		want        float64
		estimated   bool
	}{
		{"stored only", `"storedCost":0.5`, .5, false},
		{"stored matches native", `"cost":0.5,"storedCost":0.5`, .5, false},
		{"partial stored subtotal", `"cost":1,"storedCost":0.5`, 1.25, true},
		{"native without provenance", `"cost":1`, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseSourceDaily([]byte(`{"daily":[{"date":"2026-10-19","inputTokens":1000000,"outputTokens":0,"cacheReadTokens":0,"cacheCreationTokens":0,"totalTokens":1000000,"totalCost":1,"modelBreakdowns":[{"modelName":"gpt-5","inputTokens":1000000,"outputTokens":0,"cacheReadTokens":0,"cacheCreationTokens":0,"totalTokens":1000000,`+tc.costs+`}]}]}`), "claude")
			if err != nil {
				t.Fatal(err)
			}
			testPricingStore(t).Apply(rows, "claude")
			if rows[0].CostUSD != tc.want || rows[0].CostEstimated != tc.estimated {
				t.Fatal(rows)
			}
		})
	}
}
